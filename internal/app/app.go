package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	"github.com/vladar107/home-ai-assistant/internal/health"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/module"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/telegram"
)

const telemetryShutdownTimeout = 3 * time.Second

type App struct {
	cfg       config.Config
	logger    *slog.Logger
	registry  *module.Registry
	modules   []module.Module
	server    *http.Server
	store     *sqlite.Store
	assembly  *assembly
	telegram  *telegram.Service
	telemetry *observability.Telemetry
	ready     atomic.Bool
}

func New(cfg config.Config, registry *module.Registry, logger *slog.Logger) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if registry == nil {
		return nil, fmt.Errorf("module registry is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}

	modules, err := registry.Ordered()
	if err != nil {
		return nil, err
	}

	application := &App{
		cfg:       cfg,
		logger:    logger,
		registry:  registry,
		modules:   modules,
		telemetry: observability.Noop(),
	}
	application.server = &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           health.Handler(application, application.telemetry),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return application, nil
}

func (a *App) Run(ctx context.Context) (runErr error) {
	telemetry, err := observability.New(ctx, a.cfg.Observability, a.logger)
	if err != nil {
		return fmt.Errorf("initialize observability: %w", err)
	}
	a.telemetry = telemetry
	httpretry.SetDefaults(telemetry, a.logger)
	a.server.Handler = health.Handler(a, telemetry)
	defer func() {
		// The services have already spent the shutdown budget, so the final flush gets a short one
		// of its own: an unreachable collector must not hold the stop past the container's grace.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
		defer cancel()
		if err := telemetry.Shutdown(shutdownCtx); err != nil {
			a.logger.Warn("telemetry.shutdown.failed")
		}
	}()

	storeCtx, endStoreOpen := telemetry.StartLifecycle(ctx, "store_open")
	store, err := sqlite.Open(storeCtx, a.cfg.DatabasePath())
	endStoreOpen(err)
	if err != nil {
		return err
	}
	a.store = store
	if err := telemetry.RegisterConnectionPool("sqlite", store.Stats); err != nil {
		return errors.Join(fmt.Errorf("register store metrics: %w", err), a.closeStore())
	}

	initializeTelegramCtx, endInitializeTelegram := telemetry.StartLifecycle(ctx, "telegram_initialize")
	err = a.initializeTelegram(initializeTelegramCtx)
	endInitializeTelegram(err)
	if err != nil {
		return errors.Join(err, a.closeStore())
	}
	// Connectors register during initialization, so the module order is settled only now.
	a.modules, err = a.registry.Ordered()
	if err != nil {
		return errors.Join(err, a.closeStore())
	}

	startModulesCtx, endStartModules := telemetry.StartLifecycle(ctx, "modules_start")
	started, err := a.startModules(startModulesCtx)
	endStartModules(err)
	if err != nil {
		return errors.Join(err, a.closeStore())
	}
	startConversationCtx, endStartConversation := telemetry.StartLifecycle(ctx, "conversation_start")
	err = a.startConversation(startConversationCtx)
	endStartConversation(err)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
		shutdownErr := a.shutdownModules(shutdownCtx, started)
		cancel()
		return errors.Join(err, shutdownErr, a.closeStore())
	}
	startTelegramCtx, endStartTelegram := telemetry.StartLifecycle(ctx, "telegram_start")
	err = a.startTelegram(startTelegramCtx)
	endStartTelegram(err)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
		shutdownErr := errors.Join(
			a.shutdownTelegram(shutdownCtx),
			a.shutdownConversation(shutdownCtx),
			a.shutdownModules(shutdownCtx, started),
		)
		cancel()
		return errors.Join(err, shutdownErr, a.closeStore())
	}

	// The backlog service exists only after initialization; the listener is not open yet.
	var backlogService *backlog.Service
	if a.assembly != nil {
		backlogService = a.assembly.backlog
	}
	a.server.Handler = httpHandler(a, telemetry, backlogService)
	_, endListen := telemetry.StartLifecycle(ctx, "http_listen")
	listener, err := net.Listen("tcp", a.cfg.ListenAddress)
	endListen(err)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
		shutdownErr := errors.Join(
			a.shutdownTelegram(shutdownCtx),
			a.shutdownConversation(shutdownCtx),
			a.shutdownModules(shutdownCtx, started),
		)
		cancel()
		return errors.Join(
			fmt.Errorf("listen on %s: %w", a.cfg.ListenAddress, err),
			shutdownErr,
			a.closeStore(),
		)
	}

	a.ready.Store(true)
	a.telemetry.SetRunning(ctx, true)
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- a.server.Serve(listener)
	}()

	a.logger.Info("application started", "address", listener.Addr().String(), "modules", len(a.modules))

	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve HTTP: %w", err)
		}
	}

	a.ready.Store(false)
	a.telemetry.SetRunning(ctx, false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()

	if err := shutdownTogether(shutdownCtx,
		a.shutdownTelegram,
		a.shutdownConversation,
		labelled("shutdown HTTP server", a.server.Shutdown),
		func(ctx context.Context) error { return a.shutdownModules(ctx, started) },
	); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if err := a.closeStore(); err != nil {
		runErr = errors.Join(runErr, err)
	}

	a.logger.Info("application stopped")
	return runErr
}

func (a *App) Ready(ctx context.Context) error {
	if !a.ready.Load() {
		return fmt.Errorf("application is not ready")
	}
	if err := a.store.Ready(ctx); err != nil {
		return err
	}
	if a.cfg.Telegram.Enabled {
		if a.telegram == nil {
			return fmt.Errorf("Telegram service is not initialized")
		}
		if err := a.telegram.Ready(ctx); err != nil {
			return err
		}
	}
	for _, registeredModule := range a.modules {
		if err := registeredModule.Ready(ctx); err != nil {
			return fmt.Errorf("module %q: %w", registeredModule.Descriptor().ID, err)
		}
	}
	return nil
}

func (a *App) initializeTelegram(ctx context.Context) error {
	if !a.cfg.Telegram.Enabled {
		return nil
	}

	configured := make([]identity.ConfiguredPrincipal, 0, len(a.cfg.Telegram.Users))
	for _, user := range a.cfg.Telegram.Users {
		configured = append(configured, identity.ConfiguredPrincipal{
			ID:             user.PrincipalID,
			DisplayName:    user.DisplayName,
			TelegramUserID: user.TelegramUserID,
		})
	}

	token := a.cfg.Telegram.BotToken
	if token == "" {
		var err error
		token, err = telegram.LoadTokenFile(a.cfg.Telegram.BotTokenFile)
		if err != nil {
			return fmt.Errorf("load Telegram bot token: %w", err)
		}
	}
	client, err := telegram.NewClient(telegram.ClientConfig{
		BaseURL:     a.cfg.Telegram.APIBaseURL,
		Token:       token,
		PollTimeout: a.cfg.Telegram.PollTimeout,
		Telemetry:   a.telemetry,
	})
	if err != nil {
		return fmt.Errorf("initialize Telegram client: %w", err)
	}

	if a.cfg.Conversation.Enabled {
		assembled := &assembly{
			cfg:          a.cfg,
			store:        a.store,
			registry:     a.registry,
			logger:       a.logger,
			telemetry:    a.telemetry,
			displayNames: make(map[string]string, len(configured)),
		}
		for _, principal := range configured {
			assembled.principals = append(assembled.principals, principal.ID)
			assembled.displayNames[principal.ID] = principal.DisplayName
		}
		if err := assembled.buildCore(ctx, client); err != nil {
			return err
		}
		if err := assembled.buildConnectors(ctx); err != nil {
			return err
		}
		a.assembly = assembled
	}
	return a.buildTelegram(ctx, client, configured)
}

// buildTelegram builds the Telegram service over what the assembly built, and closes the cycles
// between them.
func (a *App) buildTelegram(ctx context.Context, client *telegram.Client, configured []identity.ConfiguredPrincipal) error {
	identities, err := identity.NewService(a.store, configured)
	if err != nil {
		return fmt.Errorf("initialize Telegram identities: %w", err)
	}
	bootstrapCtx, endBootstrap := a.telemetry.StartStore(ctx, "identity_bootstrap")
	err = identities.Bootstrap(bootstrapCtx)
	endBootstrap(err)
	if err != nil {
		return fmt.Errorf("bootstrap Telegram identities: %w", err)
	}

	deps := telegram.Dependencies{
		Client:     client,
		Identities: identities,
		Cursors:    a.store,
		Logger:     a.logger,
		Telemetry:  a.telemetry,
		Tabs:       a.store,
	}
	asm := a.assembly
	if asm != nil {
		// Interface values are set only for what was built, so none of them is a typed nil.
		deps.Conversation = asm.conversation
		deps.Attachments = asm.attachments
		deps.Approvals = asm.approvals
		deps.Schedules = asm.scheduler
		deps.Trips = asm.trips
		deps.Routes = asm.routes
		deps.Memory = asm.memory
		deps.Backlog = asm.backlog
		deps.Transcriber = asm.chatGPT
		if asm.locations != nil {
			deps.Locations = asm.locations
		}
		if asm.tasks != nil {
			deps.Tasks = asm.tasks
		}
		if asm.watches != nil {
			deps.Watches = asm.watches
		}
		deps.Sections, err = a.settingsSections(asm)
		if err != nil {
			return err
		}
	}
	service, err := telegram.NewService(deps)
	if err != nil {
		return fmt.Errorf("initialize Telegram service: %w", err)
	}
	client.ObserveTabs(service)
	if asm != nil {
		// Messages nobody asked for go to their feed tab while the bot has tabs on.
		asm.scheduler.SetFeeds(service.Feeds())
		if asm.watches != nil {
			asm.watches.SetFeeds(service.Feeds())
		}
		asm.tripNotifier.Feeds = service.Feeds()
		asm.conversation.SetTabReplies(service)
		if err := asm.tools.Register(service.Help()); err != nil {
			return err
		}
	}
	a.telegram = service
	return nil
}

// settingsSections are the /settings pages of what the assembly built.
func (a *App) settingsSections(asm *assembly) ([]telegram.Section, error) {
	sections := []telegram.Section{telegram.NewModelSection(asm.connections, asm.conversation, a.logger, a.telemetry)}
	if asm.notion != nil {
		sections = append(sections, telegram.NewNotionSection(asm.notion, a.logger, a.telemetry))
	}
	if asm.nas != nil {
		sections = append(sections, telegram.NewNASSection(asm.nas, a.logger, a.telemetry))
	}
	if asm.google != nil {
		sections = append(sections, telegram.NewGoogleSection(asm.google, a.logger, a.telemetry))
	}
	for _, page := range asm.switchPages {
		settings := telegram.SwitchSettings{Toggle: asm.switches.Toggle(page.integration), Allowed: page.allowed}
		if page.exposure != nil {
			settings.Details = telegram.HomeDetails(page.exposure)
		}
		found, ok := telegram.SwitchPageFor(page.integration.Integration)
		if !ok {
			return nil, fmt.Errorf("integration %q has no settings page", page.integration.Integration)
		}
		settings.Page = found
		sections = append(sections, telegram.NewSwitchSection(settings, a.logger, a.telemetry))
	}
	sections = append(sections, telegram.NewHeadsUpSection(asm.scheduler, a.logger, a.telemetry))
	if asm.locations != nil {
		sections = append(sections, telegram.NewLocationSection(asm.locations, a.logger, a.telemetry))
	}
	sections = append(sections, telegram.NewMemorySection(asm.memory, a.logger, a.telemetry))
	if asm.google != nil {
		sections = append(sections, telegram.NewTripsSection(asm.scheduler, asm.google, a.logger, a.telemetry))
	}
	return sections, nil
}

func (a *App) startConversation(ctx context.Context) error {
	asm := a.assembly
	if asm == nil {
		return nil
	}
	if err := asm.connections.Start(ctx); err != nil {
		return fmt.Errorf("start model connections: %w", err)
	}
	if err := asm.approvals.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("start approvals: %w", err), asm.connections.Shutdown(ctx))
	}
	if err := asm.conversation.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("start conversation service: %w", err), asm.approvals.Shutdown(ctx), asm.connections.Shutdown(ctx))
	}
	if err := asm.scheduler.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("start scheduler: %w", err), asm.conversation.Shutdown(ctx), asm.approvals.Shutdown(ctx), asm.connections.Shutdown(ctx))
	}
	if asm.tasks != nil {
		if err := asm.tasks.Start(ctx); err != nil {
			return errors.Join(fmt.Errorf("start background tasks: %w", err), asm.scheduler.Shutdown(ctx), asm.conversation.Shutdown(ctx), asm.approvals.Shutdown(ctx), asm.connections.Shutdown(ctx))
		}
	}
	return nil
}

func (a *App) shutdownConversation(ctx context.Context) error {
	asm := a.assembly
	if asm == nil {
		return nil
	}
	steps := []func(context.Context) error{
		labelled("shutdown scheduler", asm.scheduler.Shutdown),
		labelled("shutdown conversation service", asm.conversation.Shutdown),
		labelled("shutdown approvals", asm.approvals.Shutdown),
		labelled("shutdown model connections", asm.connections.Shutdown),
	}
	if asm.tasks != nil {
		steps = append(steps, labelled("shutdown background tasks", asm.tasks.Shutdown))
	}
	return shutdownTogether(ctx, steps...)
}

// shutdownTogether runs every shutdown at once and waits for all of them. Each one cancels its own
// work before it waits, so running them in turn would let one slow service spend the whole budget
// before the next had even been told to stop.
func shutdownTogether(ctx context.Context, steps ...func(context.Context) error) error {
	errs := make([]error, len(steps))
	var wg sync.WaitGroup
	for i, step := range steps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = step(ctx)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func labelled(label string, shutdown func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := shutdown(ctx); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		return nil
	}
}

func (a *App) startTelegram(ctx context.Context) error {
	if a.telegram == nil {
		return nil
	}
	if err := a.telegram.Start(ctx); err != nil {
		return fmt.Errorf("start Telegram service: %w", err)
	}
	return nil
}

func (a *App) shutdownTelegram(ctx context.Context) error {
	if a.telegram == nil {
		return nil
	}
	if err := a.telegram.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown Telegram service: %w", err)
	}
	return nil
}

func (a *App) closeStore() error {
	if a.store == nil {
		return nil
	}
	return a.store.Close()
}

func (a *App) startModules(ctx context.Context) ([]module.Module, error) {
	started := make([]module.Module, 0, len(a.modules))
	for _, registeredModule := range a.modules {
		if err := registeredModule.Start(ctx); err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
			shutdownErr := a.shutdownModules(shutdownCtx, started)
			cancel()
			return nil, errors.Join(
				fmt.Errorf("start module %q: %w", registeredModule.Descriptor().ID, err),
				shutdownErr,
			)
		}
		started = append(started, registeredModule)
	}
	return started, nil
}

func (a *App) shutdownModules(ctx context.Context, started []module.Module) error {
	steps := make([]func(context.Context) error, 0, len(started))
	for i := len(started) - 1; i >= 0; i-- {
		registeredModule := started[i]
		steps = append(steps, labelled(fmt.Sprintf("shutdown module %q", registeredModule.Descriptor().ID), registeredModule.Shutdown))
	}
	return shutdownTogether(ctx, steps...)
}

// nasRoots translates the configured NAS view into the connector's own shape, so the connector
// never depends on the configuration package.
func nasRoots(configured config.NASRoots) []nas.PrincipalRoots {
	roots := make([]nas.PrincipalRoots, 0, len(configured))
	for _, principal := range configured {
		paths := make([]nas.RootPath, 0, len(principal.Roots))
		for _, root := range principal.Roots {
			paths = append(paths, nas.RootPath{Name: root.Name, Path: root.Path, Writable: root.Writable})
		}
		roots = append(roots, nas.PrincipalRoots{
			PrincipalID:  principal.PrincipalID,
			Roots:        paths,
			DefaultWrite: principal.DefaultWrite,
		})
	}
	return roots
}

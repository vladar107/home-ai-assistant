package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/connectors/bus"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/connectors/grafana"
	"github.com/vladar107/home-ai-assistant/internal/connectors/homeassistant"
	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
	"github.com/vladar107/home-ai-assistant/internal/connectors/ns"
	"github.com/vladar107/home-ai-assistant/internal/connectors/places"
	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
	"github.com/vladar107/home-ai-assistant/internal/health"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	userlocation "github.com/vladar107/home-ai-assistant/internal/location"
	"github.com/vladar107/home-ai-assistant/internal/module"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	piruntime "github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/task"
	"github.com/vladar107/home-ai-assistant/internal/telegram"
	"github.com/vladar107/home-ai-assistant/internal/wiring"
)

const telemetryShutdownTimeout = 3 * time.Second

type App struct {
	cfg      config.Config
	logger   *slog.Logger
	registry *module.Registry
	modules  []module.Module
	server   *http.Server
	store    *sqlite.Store
	assembly *wiring.Assembly
	// tripNotifier is the assembly's trip notices, which reach the Telegram feeds once the service
	// exists.
	tripNotifier *telegram.TripNotifier
	telegram     *telegram.Service
	telemetry    *observability.Telemetry
	ready        atomic.Bool
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

	application := &App{
		cfg:       cfg,
		logger:    logger,
		registry:  registry,
		modules:   registry.Ordered(),
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
	a.modules = a.registry.Ordered()

	// Everything started stops together, however far the start got.
	var lifecycle wiring.Lifecycle
	abort := func(err error) error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
		defer cancel()
		return errors.Join(err, lifecycle.Shutdown(shutdownCtx), a.closeStore())
	}
	startModulesCtx, endStartModules := telemetry.StartLifecycle(ctx, "modules_start")
	err = lifecycle.Start(startModulesCtx, a.moduleSteps()...)
	endStartModules(err)
	if err != nil {
		return abort(err)
	}
	startConversationCtx, endStartConversation := telemetry.StartLifecycle(ctx, "conversation_start")
	err = lifecycle.Start(startConversationCtx, a.conversationSteps()...)
	endStartConversation(err)
	if err != nil {
		return abort(err)
	}
	startTelegramCtx, endStartTelegram := telemetry.StartLifecycle(ctx, "telegram_start")
	err = lifecycle.Start(startTelegramCtx, a.telegramSteps()...)
	endStartTelegram(err)
	if err != nil {
		return abort(err)
	}

	// The backlog service exists only after initialization; the listener is not open yet.
	var backlogService *backlog.Service
	if a.assembly != nil {
		backlogService = a.assembly.Backlog
	}
	a.server.Handler = httpHandler(a, telemetry, backlogService)
	_, endListen := telemetry.StartLifecycle(ctx, "http_listen")
	listener, err := net.Listen("tcp", a.cfg.ListenAddress)
	endListen(err)
	if err != nil {
		return abort(fmt.Errorf("listen on %s: %w", a.cfg.ListenAddress, err))
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

	if err := lifecycle.Shutdown(shutdownCtx, wiring.Labelled("shutdown HTTP server", a.server.Shutdown)); err != nil {
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
		assembled, err := a.buildAssembly(ctx, client, configured)
		if err != nil {
			return err
		}
		a.assembly = assembled
	}
	return a.buildTelegram(ctx, client, configured)
}

// buildAssembly translates the configuration into the wiring's inputs and builds everything but the
// Telegram service.
func (a *App) buildAssembly(ctx context.Context, client *telegram.Client, configured []identity.ConfiguredPrincipal) (*wiring.Assembly, error) {
	cfg := a.cfg
	profilesDir := filepath.Join(cfg.DataDir, "pi")
	principals := make([]string, 0, len(configured))
	for _, principal := range configured {
		principals = append(principals, principal.ID)
	}
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load household time zone: %w", err)
	}
	// The extension file is required only where web access can be turned on, so a local run
	// without the vendored checkout still starts.
	var webSearchExtension string
	if cfg.Web.Enabled {
		webSearchExtension = cfg.Conversation.PiWebSearchExtension
	}
	runtime, err := piruntime.New(ctx, piruntime.Config{
		Binary:             cfg.Conversation.PiBinary,
		NodeBinary:         cfg.Conversation.PiNodeBinary,
		Provider:           cfg.Conversation.PiProvider,
		Model:              cfg.Conversation.PiModel,
		ExpectedVersion:    cfg.Conversation.PiVersion,
		ProfilesDir:        profilesDir,
		WebSearchExtension: webSearchExtension,
		Logger:             a.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize Pi runtime: %w", err)
	}
	runtime.SetTelemetry(a.telemetry)
	a.tripNotifier = &telegram.TripNotifier{Client: client, Chats: a.store}
	in := wiring.Inputs{
		Store:        a.store,
		Registry:     a.registry,
		Logger:       a.logger,
		Telemetry:    a.telemetry,
		Runtime:      runtime,
		SessionFiles: runtime,
		Sessions:     runtime.SessionPruner(a.store),
		Chat:         client,
		Reminders:    telegram.ReminderSender{Client: client},
		Alerts:       telegram.WatchAlertSender{Client: client},
		TaskMessages: telegram.TaskMessenger{Client: client},
		TripNotices:  a.tripNotifier,
		Principals:   configured,
		DataDir:      cfg.DataDir,
		Zone:         cfg.Timezone,
		Core: wiring.CoreConfig{
			TurnTimeout: cfg.Conversation.TurnTimeout,
			TurnZone:    location,
			ChatModel:   cfg.Conversation.PiModel,
			TaskModel:   cfg.Conversation.PiTaskModel,
			Connection: connection.Config{
				ProfilesDir: profilesDir,
				Provider:    cfg.Conversation.PiProvider,
				Principals:  principals,
			},
			TypeSafe: judgment.Config{APIKey: cfg.TypeSafe.APIKey},
			Watches:  cfg.Watches.Enabled,
		},
	}
	if cfg.Tasks.Enabled {
		in.Core.Tasks = &task.Config{Timeout: cfg.Tasks.Timeout}
	}
	if cfg.Location.Enabled {
		in.Core.Location = &wiring.LocationConfig{
			Policy: userlocation.Policy{
				PinTTL:      cfg.Location.PinTTL,
				LiveSilence: cfg.Location.LiveSilence,
				LiveGrace:   cfg.Location.LiveGrace,
			},
			PruneInterval: cfg.Location.PruneInterval,
			Places:        places.Config{Contact: cfg.Location.Contact},
		}
	}
	if cfg.Notion.Enabled {
		in.Notion = &notion.Config{APIBaseURL: cfg.Notion.APIBaseURL, ChatConnect: cfg.Notion.ChatConnect}
	}
	if cfg.NAS.Enabled {
		in.NAS = &nas.Config{Roots: nasRoots(cfg.NAS.Roots)}
	}
	if cfg.Google.Enabled {
		secret := cfg.Google.ClientSecret
		if secret == "" {
			secret, err = google.LoadClientSecretFile(cfg.Google.ClientSecretFile)
			if err != nil {
				return nil, err
			}
		}
		in.Google = &wiring.GoogleConfig{Accounts: google.Config{
			ClientID:     cfg.Google.ClientID,
			ClientSecret: secret,
			RedirectURL:  cfg.Google.RedirectURL,
		}}
	}
	if cfg.Web.Enabled {
		in.Web = &web.Config{WeatherHome: cfg.Web.WeatherHome}
	}
	if cfg.NS.Enabled() {
		in.NS = &ns.Config{APIKey: cfg.NS.APIKey}
	}
	if cfg.HA.Enabled() {
		in.HA = &homeassistant.Config{Token: cfg.HA.Token, BaseURL: cfg.HA.URL}
	}
	if cfg.Grafana.Enabled() {
		in.Grafana = &grafana.Config{Token: cfg.Grafana.Token, BaseURL: cfg.Grafana.URL, Principals: cfg.Grafana.Principals}
	}
	if cfg.Bus.Enabled {
		in.Bus = &bus.Config{HomeStop: cfg.Bus.HomeStop, Contact: cfg.Bus.Contact}
	}
	return wiring.Build(ctx, in)
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
		location, err := time.LoadLocation(a.cfg.Timezone)
		if err != nil {
			return fmt.Errorf("load household time zone: %w", err)
		}
		// Interface values are set only for what was built, so none of them is a typed nil.
		deps.Conversation = asm.Conversation
		deps.Namer = asm.Turns
		deps.Attachments = asm.Attachments
		deps.Approvals = asm.Approvals
		deps.Schedules = asm.Scheduler
		deps.Trips = asm.Trips
		deps.Routes = asm.Routes
		deps.Memory = asm.Memory
		deps.Backlog = asm.Backlog
		deps.Transcriber = asm.ChatGPT
		deps.Model = &telegram.ModelCommand{
			Lanes: asm.Conversation, Catalog: asm.Models, Usage: asm.ChatGPT,
			ChatDefault: asm.ChatModel, TaskDefault: asm.TaskModel,
			Turns: asm.Conversation, Location: location,
		}
		if asm.Tasks != nil {
			deps.Model.Tasks = asm.Tasks
		}
		if asm.Locations != nil {
			deps.Locations = asm.Locations
		}
		if asm.Tasks != nil {
			deps.Tasks = asm.Tasks
		}
		if asm.Watches != nil {
			deps.Watches = asm.Watches
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
		asm.Scheduler.SetFeeds(service.Feeds())
		if asm.Watches != nil {
			asm.Watches.SetFeeds(service.Feeds())
		}
		a.tripNotifier.Feeds = service.Feeds()
		asm.Conversation.SetTabReplies(service)
		if err := asm.Tools.Register(service.Help()); err != nil {
			return err
		}
	}
	a.telegram = service
	return nil
}

// settingsSections are the /settings pages of what the assembly built.
func (a *App) settingsSections(asm *wiring.Assembly) ([]telegram.Section, error) {
	sections := []telegram.Section{telegram.NewModelSection(asm.Connections, asm.Conversation, a.logger, a.telemetry)}
	if asm.Notion != nil {
		sections = append(sections, telegram.NewNotionSection(asm.Notion, a.logger, a.telemetry))
	}
	if asm.NAS != nil {
		sections = append(sections, telegram.NewNASSection(asm.NAS, a.logger, a.telemetry))
	}
	if asm.Google != nil {
		sections = append(sections, telegram.NewGoogleSection(asm.Google, a.logger, a.telemetry))
	}
	for _, page := range asm.SwitchPages {
		settings := telegram.SwitchSettings{Toggle: asm.Switches.Toggle(page.Switch), Allowed: page.Allowed}
		if page.Exposure != nil {
			settings.Details = telegram.HomeDetails(page.Exposure)
		}
		found, ok := telegram.SwitchPageFor(page.Switch.Integration)
		if !ok {
			return nil, fmt.Errorf("integration %q has no settings page", page.Switch.Integration)
		}
		settings.Page = found
		sections = append(sections, telegram.NewSwitchSection(settings, a.logger, a.telemetry))
	}
	sections = append(sections, telegram.NewHeadsUpSection(asm.Scheduler, a.logger, a.telemetry))
	if asm.Locations != nil {
		sections = append(sections, telegram.NewLocationSection(asm.Locations, a.logger, a.telemetry))
	}
	sections = append(sections, telegram.NewMemorySection(asm.Memory, a.logger, a.telemetry))
	if asm.Google != nil {
		sections = append(sections, telegram.NewTripsSection(asm.Scheduler, asm.Google, a.logger, a.telemetry))
	}
	return sections, nil
}

func (a *App) moduleSteps() []wiring.Step {
	steps := make([]wiring.Step, 0, len(a.modules))
	for _, registeredModule := range a.modules {
		steps = append(steps, wiring.Step{
			Name:     fmt.Sprintf("module %q", registeredModule.Descriptor().ID),
			Start:    registeredModule.Start,
			Shutdown: registeredModule.Shutdown,
		})
	}
	return steps
}

func (a *App) conversationSteps() []wiring.Step {
	if a.assembly == nil {
		return nil
	}
	return a.assembly.Steps()
}

func (a *App) telegramSteps() []wiring.Step {
	if a.telegram == nil {
		return nil
	}
	return []wiring.Step{{Name: "Telegram service", Start: a.telegram.Start, Shutdown: a.telegram.Shutdown}}
}

func (a *App) closeStore() error {
	if a.store == nil {
		return nil
	}
	return a.store.Close()
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

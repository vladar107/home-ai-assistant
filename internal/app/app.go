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

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/health"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/module"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	piruntime "github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/telegram"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type App struct {
	cfg          config.Config
	logger       *slog.Logger
	registry     *module.Registry
	modules      []module.Module
	server       *http.Server
	store        *sqlite.Store
	connections  *connection.Service
	attachments  *attachment.Service
	conversation *conversation.Service
	tools        *tools.Broker
	approvals    *approval.Service
	notion       *notion.Service
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
	a.server.Handler = health.Handler(a, telemetry)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
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
			runErr = fmt.Errorf("serve health endpoint: %w", err)
		}
	}

	a.ready.Store(false)
	a.telemetry.SetRunning(ctx, false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()

	if err := a.shutdownTelegram(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if err := a.shutdownConversation(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if err := a.server.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("shutdown HTTP server: %w", err))
	}
	if err := a.shutdownModules(shutdownCtx, started); err != nil {
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
		profilesDir := filepath.Join(a.cfg.DataDir, "pi")
		principals := make([]string, 0, len(configured))
		for _, principal := range configured {
			principals = append(principals, principal.ID)
		}
		connections, err := connection.NewService(connection.Config{
			ProfilesDir: profilesDir,
			Provider:    a.cfg.Conversation.PiProvider,
			Principals:  principals,
		}, a.store, client, a.logger)
		if err != nil {
			return fmt.Errorf("initialize model connections: %w", err)
		}
		runtime, err := piruntime.New(ctx, piruntime.Config{
			Binary:          a.cfg.Conversation.PiBinary,
			NodeBinary:      a.cfg.Conversation.PiNodeBinary,
			Provider:        a.cfg.Conversation.PiProvider,
			Model:           a.cfg.Conversation.PiModel,
			ExpectedVersion: a.cfg.Conversation.PiVersion,
			ProfilesDir:     profilesDir,
		})
		if err != nil {
			return fmt.Errorf("initialize Pi runtime: %w", err)
		}
		broker, err := tools.NewBroker(a.logger)
		if err != nil {
			return fmt.Errorf("initialize tool broker: %w", err)
		}
		attachments, err := attachment.NewService(attachment.Config{
			Directory: filepath.Join(a.cfg.DataDir, "attachments"),
		}, a.store, a.logger)
		if err != nil {
			return fmt.Errorf("initialize attachments: %w", err)
		}
		if err := broker.Register(attachments); err != nil {
			return err
		}
		approvals, err := approval.NewService(approval.Config{}, a.store, client, a.logger)
		if err != nil {
			return fmt.Errorf("initialize approvals: %w", err)
		}
		conversationService, err := conversation.NewService(
			conversation.Config{
				TurnTimeout: a.cfg.Conversation.TurnTimeout,
				FollowUp:    approvals,
				Notice:      approvals,
				Attachments: attachments,
			},
			a.store,
			runtime,
			client,
			broker,
			connections,
			a.logger,
			a.telemetry,
		)
		if err != nil {
			return fmt.Errorf("initialize conversation service: %w", err)
		}
		a.attachments = attachments
		a.connections = connections
		a.conversation = conversationService
		a.tools = broker
		a.approvals = approvals
		if a.cfg.Notion.Enabled {
			notionService, err := notion.NewService(notion.Config{
				CredentialsDir: filepath.Join(a.cfg.DataDir, "integrations"),
				APIBaseURL:     a.cfg.Notion.APIBaseURL,
				ChatConnect:    a.cfg.Notion.ChatConnect,
			}, a.store, approvals, a.logger)
			if err != nil {
				return fmt.Errorf("initialize Notion connector: %w", err)
			}
			if err := a.registry.Register(notionService); err != nil {
				return err
			}
			if err := broker.Register(notionService); err != nil {
				return err
			}
			if err := approvals.RegisterExecutor(notionService); err != nil {
				return err
			}
			a.notion = notionService
		}
	}

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

	// Interface values are built here so a disabled conversation never becomes a typed nil controller.
	var conversationController telegram.ConversationController
	var attachmentStore telegram.AttachmentStore
	var connectionController telegram.ConnectionController
	var notionController telegram.NotionController
	var approvalController telegram.ApprovalController
	if a.conversation != nil {
		conversationController = a.conversation
		attachmentStore = a.attachments
		connectionController = a.connections
		approvalController = a.approvals
	}
	if a.notion != nil {
		notionController = a.notion
	}
	service, err := telegram.NewService(client, identities, a.store, conversationController, attachmentStore, connectionController, notionController, approvalController, a.logger, a.telemetry)
	if err != nil {
		return fmt.Errorf("initialize Telegram service: %w", err)
	}
	a.telegram = service
	return nil
}

func (a *App) startConversation(ctx context.Context) error {
	if a.conversation == nil {
		return nil
	}
	if err := a.connections.Start(ctx); err != nil {
		return fmt.Errorf("start model connections: %w", err)
	}
	if err := a.approvals.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("start approvals: %w", err), a.connections.Shutdown(ctx))
	}
	if err := a.conversation.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("start conversation service: %w", err), a.approvals.Shutdown(ctx), a.connections.Shutdown(ctx))
	}
	return nil
}

func (a *App) shutdownConversation(ctx context.Context) error {
	if a.conversation == nil {
		return nil
	}
	var errs []error
	if err := a.conversation.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown conversation service: %w", err))
	}
	if err := a.approvals.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown approvals: %w", err))
	}
	if err := a.connections.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown model connections: %w", err))
	}
	return errors.Join(errs...)
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
	var shutdownErr error
	for i := len(started) - 1; i >= 0; i-- {
		registeredModule := started[i]
		if err := registeredModule.Shutdown(ctx); err != nil {
			shutdownErr = errors.Join(
				shutdownErr,
				fmt.Errorf("shutdown module %q: %w", registeredModule.Descriptor().ID, err),
			)
		}
	}
	return shutdownErr
}

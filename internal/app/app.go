package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/approval/withdrawal"
	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/connectors/bus"
	"github.com/vladar107/home-ai-assistant/internal/connectors/calendar"
	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/connectors/grafana"
	"github.com/vladar107/home-ai-assistant/internal/connectors/homeassistant"
	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
	"github.com/vladar107/home-ai-assistant/internal/connectors/ns"
	"github.com/vladar107/home-ai-assistant/internal/connectors/places"
	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/conversation/history"
	"github.com/vladar107/home-ai-assistant/internal/delivery"
	"github.com/vladar107/home-ai-assistant/internal/exposure"
	"github.com/vladar107/home-ai-assistant/internal/health"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/imagegen"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	userlocation "github.com/vladar107/home-ai-assistant/internal/location"
	usermemory "github.com/vladar107/home-ai-assistant/internal/memory"
	"github.com/vladar107/home-ai-assistant/internal/module"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/reaction"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	piruntime "github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/searchcheck"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/task"
	"github.com/vladar107/home-ai-assistant/internal/telegram"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/transit"
	"github.com/vladar107/home-ai-assistant/internal/trip"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const telemetryShutdownTimeout = 3 * time.Second

type App struct {
	cfg           config.Config
	logger        *slog.Logger
	registry      *module.Registry
	modules       []module.Module
	server        *http.Server
	store         *sqlite.Store
	connections   *connection.Service
	chatGPT       *chatgpt.Client
	attachments   *attachment.Service
	conversation  *conversation.Service
	tools         *tools.Broker
	approvals     *approval.Service
	scheduler     *schedule.Service
	tasks         *task.Service
	watches       *watch.Service
	notion        *notion.Service
	nas           *nas.Service
	google        *google.Service
	web           *web.Service
	ns            *ns.Service
	homeAssistant *homeassistant.Service
	grafana       *grafana.Service
	bus           *bus.Service
	routes        *routes.Service
	locations     *userlocation.Service
	memory        *usermemory.Service
	backlog       *backlog.Service
	trips         *trip.Service
	tripNotifier  *telegram.TripNotifier
	switches      *integration.Switches
	typeSafe      bool
	telegram      *telegram.Service
	telemetry     *observability.Telemetry
	ready         atomic.Bool
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
	a.server.Handler = httpHandler(a, telemetry, a.backlog)
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
		// The extension file is required only where web access can be turned on, so a local run
		// without the vendored checkout still starts.
		var webSearchExtension string
		if a.cfg.Web.Enabled {
			webSearchExtension = a.cfg.Conversation.PiWebSearchExtension
		}
		runtime, err := piruntime.New(ctx, piruntime.Config{
			Binary:             a.cfg.Conversation.PiBinary,
			NodeBinary:         a.cfg.Conversation.PiNodeBinary,
			Provider:           a.cfg.Conversation.PiProvider,
			Model:              a.cfg.Conversation.PiModel,
			ExpectedVersion:    a.cfg.Conversation.PiVersion,
			ProfilesDir:        profilesDir,
			WebSearchExtension: webSearchExtension,
			Logger:             a.logger,
		})
		if err != nil {
			return fmt.Errorf("initialize Pi runtime: %w", err)
		}
		runtime.SetTelemetry(a.telemetry)
		a.telemetry.ZeroRareOutcomes(ctx, principals, conversation.TurnErrorKinds(), reaction.Emojis(), conversation.FeedbackClasses())
		switches, err := integration.NewSwitches(a.store, a.logger)
		if err != nil {
			return fmt.Errorf("initialize integration switches: %w", err)
		}
		a.switches = switches
		broker, err := tools.NewBroker(a.logger)
		if err != nil {
			return fmt.Errorf("initialize tool broker: %w", err)
		}
		broker.SetTelemetry(a.telemetry)
		broker.SetSwitches(switches)
		attachments, err := attachment.NewService(attachment.Config{
			Directory: filepath.Join(a.cfg.DataDir, "attachments"),
		}, a.store, a.logger)
		if err != nil {
			return fmt.Errorf("initialize attachments: %w", err)
		}
		if err := broker.Register(attachments); err != nil {
			return err
		}
		// Pictures and voice use the principal's own subscription, the same one Pi holds.
		chatGPT, err := chatgpt.New(chatgpt.Config{}, connections)
		if err != nil {
			return fmt.Errorf("initialize ChatGPT client: %w", err)
		}
		a.chatGPT = chatGPT
		// Files reach the user through their own private chat, which is the Telegram identity
		// the allowlist already binds to the principal running the turn.
		files, err := delivery.NewService(a.store, client, a.logger)
		if err != nil {
			return fmt.Errorf("initialize file delivery: %w", err)
		}
		images, err := imagegen.NewService(imagegen.Config{Model: a.cfg.Conversation.PiModel}, chatGPT, files, attachments, a.logger)
		if err != nil {
			return fmt.Errorf("initialize image generation: %w", err)
		}
		if err := broker.Register(images); err != nil {
			return fmt.Errorf("register image generation: %w", err)
		}
		cards, err := outbox.New(filepath.Join(a.cfg.DataDir, "outbox"))
		if err != nil {
			return fmt.Errorf("initialize outbox: %w", err)
		}
		approvals, err := approval.NewService(approval.Config{}, a.store, client, a.logger)
		if err != nil {
			return fmt.Errorf("initialize approvals: %w", err)
		}
		approvals.SetTelemetry(a.telemetry)
		approvals.SetSwitches(switches)
		if err := broker.Register(withdrawal.Provider{Withdrawer: approvals}); err != nil {
			return fmt.Errorf("register approval tools: %w", err)
		}
		reactions, err := reaction.NewProvider(a.store, client, a.telemetry)
		if err != nil {
			return fmt.Errorf("initialize reactions: %w", err)
		}
		if err := broker.Register(reactions); err != nil {
			return fmt.Errorf("register reaction tools: %w", err)
		}
		memoryService, err := usermemory.NewService(usermemory.Config{}, a.store, approvals, a.logger)
		if err != nil {
			return fmt.Errorf("initialize memory: %w", err)
		}
		memoryService.SetTelemetry(a.telemetry)
		exposures, err := exposure.NewService(a.store, a.logger)
		if err != nil {
			return fmt.Errorf("initialize exposure: %w", err)
		}
		broker.SetTurnExposure(exposures)
		broker.SetTurnPrivacy(exposures)
		approvals.SetExposure(exposures)
		searchChecks, err := searchcheck.NewService(exposures, approvals, a.logger, nil)
		if err != nil {
			return fmt.Errorf("initialize search checks: %w", err)
		}
		searchChecks.SetTelemetry(a.telemetry)
		if err := approvals.RegisterExecutor(searchChecks); err != nil {
			return err
		}
		// Interface values stay nil without a key, so no message or reply is kept in memory for nothing.
		var recent conversation.Exchange
		var toolHint conversation.ToolHinter
		var safetyChecks conversation.SafetyChecks
		if a.cfg.TypeSafe.Enabled() {
			registry := turnstate.New(nil)
			broker.SetExposure(registry)
			recent = registry
			judge, err := judgment.NewClient(judgment.Config{APIKey: a.cfg.TypeSafe.APIKey, Logger: a.logger, Telemetry: a.telemetry})
			if err != nil {
				return fmt.Errorf("initialize TypeSafe client: %w", err)
			}
			typeSafe := judgment.NewOptIn(switches)
			screener := judgment.NewScreener(judge, typeSafe, registry)
			broker.SetScreener(screener)
			memoryService.SetScreener(screener)
			approvals.SetIntentChecker(judgment.NewIntentChecker(judge, typeSafe, registry, registry))
			searchChecks.SetJudge(judgment.NewSearchJudge(judge, typeSafe, registry, registry))
			toolHint = judgment.NewToolHinter(judge, typeSafe)
			safetyChecks = typeSafe
			a.typeSafe = true
		}
		// Scheduled runs reach only the owner's own chat, resolved from the allowlist at delivery.
		scheduler, err := schedule.NewService(schedule.Config{Zone: a.cfg.Timezone, Cards: cards, Photos: client, Reminders: telegram.ReminderSender{Client: client}, Sessions: runtime.SessionPruner(a.store)}, a.store, a.store, client, approvals, a.logger)
		if err != nil {
			return fmt.Errorf("initialize scheduler: %w", err)
		}
		scheduler.SetTelemetry(a.telemetry)
		scheduler.SetSafetyChecks(safetyChecks)
		scheduler.SetExposure(exposures)
		tripService, err := trip.NewService(trip.Config{}, a.store, trip.NewScheduleReminders(scheduler), a.logger)
		if err != nil {
			return fmt.Errorf("initialize trips: %w", err)
		}
		scheduler.SetTravel(tripService)
		var nsService *ns.Service
		var trains transit.Trips
		if a.cfg.NS.Enabled() {
			nsService, err = ns.NewService(ns.Config{APIKey: a.cfg.NS.APIKey}, switches, a.logger)
			if err != nil {
				return fmt.Errorf("initialize NS connector: %w", err)
			}
			trains = nsService
		}
		location, err := time.LoadLocation(a.cfg.Timezone)
		if err != nil {
			return fmt.Errorf("load household time zone: %w", err)
		}
		var webSearch conversation.WebSearch
		if a.cfg.Web.Enabled {
			webService, err := web.NewService(web.Config{WeatherHome: a.cfg.Web.WeatherHome, Location: location, Cards: cards, Trains: trains}, switches, a.logger)
			if err != nil {
				return fmt.Errorf("initialize web connector: %w", err)
			}
			webService.SetTelemetry(a.telemetry)
			webService.SetExposure(exposures)
			if err := a.registry.Register(webService); err != nil {
				return err
			}
			if err := broker.Register(webService); err != nil {
				return err
			}
			a.web = webService
			webSearch = webService
		}
		var turnLocations conversation.Locations
		if a.cfg.Location.Enabled {
			placesService, err := places.NewService(places.Config{Contact: a.cfg.Location.Contact}, a.logger)
			if err != nil {
				return fmt.Errorf("initialize places connector: %w", err)
			}
			if err := a.registry.Register(placesService); err != nil {
				return err
			}
			if err := broker.Register(placesService); err != nil {
				return err
			}
			locationService, err := userlocation.NewService(userlocation.Config{
				Policy: userlocation.Policy{
					PinTTL:      a.cfg.Location.PinTTL,
					LiveSilence: a.cfg.Location.LiveSilence,
					LiveGrace:   a.cfg.Location.LiveGrace,
				},
				PruneInterval: a.cfg.Location.PruneInterval,
				Describer:     placesService,
				Zone:          location,
			}, a.store, a.logger)
			if err != nil {
				return fmt.Errorf("initialize locations: %w", err)
			}
			if err := a.registry.Register(locationService); err != nil {
				return err
			}
			a.locations = locationService
			turnLocations = locationService
		}
		conversationHistory, err := history.NewService(a.store, runtime, location, a.telemetry)
		if err != nil {
			return fmt.Errorf("initialize conversation history: %w", err)
		}
		if err := broker.Register(conversationHistory); err != nil {
			return err
		}
		notices := conversation.Notices{approvals}
		var reports conversation.Reports = scheduler
		var tasks *task.Service
		if a.cfg.Tasks.Enabled {
			tasks, err = task.NewService(task.Config{Timeout: a.cfg.Tasks.Timeout, Cards: cards, Photos: client}, a.store, a.store, telegram.TaskMessenger{Client: client}, client, a.logger)
			if err != nil {
				return fmt.Errorf("initialize background tasks: %w", err)
			}
			tasks.SetTelemetry(a.telemetry)
			tasks.SetExposure(exposures)
			tasks.SetProposer(approvals)
			if err := broker.Register(tasks); err != nil {
				return err
			}
			if err := approvals.RegisterExecutor(tasks); err != nil {
				return err
			}
			notices = append(notices, conversation.OutsideNotice{Notice: tasks})
		}
		if a.cfg.Watches.Enabled {
			// Alerts reach only the owner's own chat, resolved from the allowlist at delivery.
			watches, err := watch.NewService(watch.Config{Zone: a.cfg.Timezone}, a.store, a.store, telegram.WatchAlertSender{Client: client}, approvals, a.logger)
			if err != nil {
				return fmt.Errorf("initialize watches: %w", err)
			}
			watches.SetTelemetry(a.telemetry)
			watches.SetTravel(tripService)
			a.telemetry.ZeroWatchOutcomes(ctx, principals, []string{watch.KindDeparture, watch.KindRoute, watch.KindDisruptions})
			if err := a.registry.Register(watches); err != nil {
				return err
			}
			if err := broker.Register(watches); err != nil {
				return err
			}
			if err := approvals.RegisterExecutor(watches); err != nil {
				return err
			}
			// Alerts are written by code from transit data and Home Assistant names and states, which
			// no tool marks as outside content, so retelling one does not withhold search or make
			// home actions wait. An agent watch's alert is written by a model from web pages or mail,
			// so it reaches the next turn as outside content, like a scheduled report.
			notices = append(notices, watches, conversation.OutsideNotice{Notice: watches.AgentNotices()})
			// A reply to an alert quotes it, marked as the alert's own content is.
			reports = conversation.ReportSources{scheduler, watches}
			a.watches = watches
		}
		displayNames := make(map[string]string, len(a.cfg.Telegram.Users))
		for _, user := range a.cfg.Telegram.Users {
			displayNames[user.PrincipalID] = user.DisplayName
		}
		conversationService, err := conversation.NewService(
			conversation.Config{
				TurnTimeout:  a.cfg.Conversation.TurnTimeout,
				FollowUp:     approvals,
				Notice:       notices,
				Reports:      reports,
				Jobs:         scheduler,
				Attachments:  attachments,
				Transcripts:  runtime,
				WebSearch:    webSearch,
				Cards:        cards,
				Photos:       client,
				Recent:       recent,
				ToolHint:     toolHint,
				SafetyChecks: safetyChecks,
				Location:     location,
				Locations:    turnLocations,
				Trips:        tripService,
				Memory:       memoryService,
				Exposure:     exposures,
				SearchChecks: searchChecks,
				History:      conversationHistory,
				DisplayNames: displayNames,
				Reactions:    reactions,
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
		if err := scheduler.Register(schedule.ModelTurn(conversationService)); err != nil {
			return err
		}
		if err := scheduler.Register(schedule.Reminder(scheduler.Now)); err != nil {
			return err
		}
		if err := broker.Register(scheduler); err != nil {
			return err
		}
		if err := approvals.RegisterExecutor(scheduler); err != nil {
			return err
		}
		if err := broker.Register(memoryService); err != nil {
			return err
		}
		if err := approvals.RegisterExecutor(memoryService); err != nil {
			return err
		}
		a.memory = memoryService
		tripTools, err := trip.NewTools(trip.ToolsConfig{Zone: scheduler.DefaultZone()}, tripService, approvals, a.logger)
		if err != nil {
			return fmt.Errorf("initialize trip tools: %w", err)
		}
		if err := broker.Register(tripTools); err != nil {
			return err
		}
		if err := approvals.RegisterExecutor(tripTools); err != nil {
			return err
		}
		tripState := &tripScanState{schedules: scheduler}
		tripTools.SetScanState(tripState)
		tripNotifier := &telegram.TripNotifier{Client: client, Chats: a.store}
		a.tripNotifier = tripNotifier
		tripService.SetTabs(tripNotifier)
		if err := scheduler.Register(trip.ScanJobType(tripScanChecker{conversationService}, a.store, tripState, tripService, tripNotifier, tripNotifier, scheduler.Now, a.logger)); err != nil {
			return err
		}
		paused := []trip.Paused{scheduler}
		if a.watches != nil {
			paused = append(paused, a.watches)
		}
		tripLoop, err := trip.NewLoop(trip.LoopConfig{}, tripService, paused, tripNotifier, a.logger)
		if err != nil {
			return fmt.Errorf("initialize trip loop: %w", err)
		}
		if err := a.registry.Register(tripLoop); err != nil {
			return err
		}
		a.trips = tripService
		backlogService, err := backlog.NewService(backlog.Config{Location: location}, a.store, approvals, a.logger)
		if err != nil {
			return fmt.Errorf("initialize backlog: %w", err)
		}
		backlogService.SetExposure(exposures)
		if err := broker.Register(backlogService); err != nil {
			return err
		}
		if err := approvals.RegisterExecutor(backlogService); err != nil {
			return err
		}
		a.backlog = backlogService
		a.scheduler = scheduler
		if tasks != nil {
			tasks.SetRunner(conversationService)
			a.tasks = tasks
		}
		if a.watches != nil {
			a.watches.SetRewriter(conversationService)
			agent, err := watch.NewAgentEvaluator(conversationService, watch.AgentConfig{Finished: a.watches.Wake})
			if err != nil {
				return fmt.Errorf("initialize agent watches: %w", err)
			}
			if err := a.watches.Register(watch.KindAgent, agent); err != nil {
				return err
			}
			a.telemetry.ZeroWatchOutcomes(ctx, principals, []string{watch.KindAgent})
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
				DisplayNames:   displayNames,
				Location:       location,
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
		if a.cfg.NAS.Enabled {
			nasService, err := nas.NewService(nas.Config{
				Roots: nasRoots(a.cfg.NAS.Roots),
			}, a.store, approvals, attachments, files, a.logger)
			if err != nil {
				return fmt.Errorf("initialize NAS connector: %w", err)
			}
			if err := a.registry.Register(nasService); err != nil {
				return err
			}
			if err := broker.Register(nasService); err != nil {
				return err
			}
			if err := approvals.RegisterExecutor(nasService); err != nil {
				return err
			}
			a.nas = nasService
		}
		if a.cfg.Google.Enabled {
			secret := a.cfg.Google.ClientSecret
			if secret == "" {
				secret, err = google.LoadClientSecretFile(a.cfg.Google.ClientSecretFile)
				if err != nil {
					return err
				}
			}
			googleService, err := google.NewService(google.Config{
				CredentialsDir: filepath.Join(a.cfg.DataDir, "integrations"),
				ClientID:       a.cfg.Google.ClientID,
				ClientSecret:   secret,
				RedirectURL:    a.cfg.Google.RedirectURL,
			}, a.store, a.logger)
			if err != nil {
				return fmt.Errorf("initialize Google connector: %w", err)
			}
			if err := a.registry.Register(googleService); err != nil {
				return err
			}
			gmailService, err := gmail.NewService(gmail.Config{}, googleService, a.logger)
			if err != nil {
				return fmt.Errorf("initialize Gmail connector: %w", err)
			}
			if err := broker.Register(gmailService); err != nil {
				return err
			}
			calendarService, err := calendar.NewService(calendar.Config{Location: location}, googleService, a.logger)
			if err != nil {
				return fmt.Errorf("initialize Calendar connector: %w", err)
			}
			if err := broker.Register(calendarService); err != nil {
				return err
			}
			tripState.google = googleService
			a.google = googleService
		}
		if nsService != nil {
			if err := a.registry.Register(nsService); err != nil {
				return err
			}
			if err := broker.Register(nsService); err != nil {
				return err
			}
			a.ns = nsService
		}
		if a.cfg.HA.Enabled() {
			homeAssistantService, err := homeassistant.NewService(homeassistant.Config{Token: a.cfg.HA.Token, BaseURL: a.cfg.HA.URL}, switches, approvals, a.logger)
			if err != nil {
				return fmt.Errorf("initialize Home Assistant connector: %w", err)
			}
			homeAssistantService.SetExposure(exposures)
			if err := a.registry.Register(homeAssistantService); err != nil {
				return err
			}
			if err := broker.Register(homeAssistantService); err != nil {
				return err
			}
			if err := approvals.RegisterExecutor(homeAssistantService); err != nil {
				return err
			}
			a.homeAssistant = homeAssistantService
			if a.watches != nil {
				if err := a.watches.Register(watch.KindHome, homeAssistantService); err != nil {
					return err
				}
				a.telemetry.ZeroWatchOutcomes(ctx, principals, []string{watch.KindHome})
			}
		}
		if a.cfg.Grafana.Enabled() {
			grafanaService, err := grafana.NewService(grafana.Config{
				Token:      a.cfg.Grafana.Token,
				BaseURL:    a.cfg.Grafana.URL,
				Principals: a.cfg.Grafana.Principals,
				Location:   location,
			}, a.logger)
			if err != nil {
				return fmt.Errorf("initialize Grafana connector: %w", err)
			}
			if err := a.registry.Register(grafanaService); err != nil {
				return err
			}
			if err := broker.Register(grafanaService); err != nil {
				return err
			}
			a.grafana = grafanaService
		}
		if a.cfg.Bus.Enabled {
			busService, err := bus.NewService(bus.Config{HomeStop: a.cfg.Bus.HomeStop, Contact: a.cfg.Bus.Contact}, switches, a.logger)
			if err != nil {
				return fmt.Errorf("initialize bus connector: %w", err)
			}
			if err := a.registry.Register(busService); err != nil {
				return err
			}
			if err := broker.Register(busService); err != nil {
				return err
			}
			a.bus = busService
		}
		// Typed nils would hide a missing connector, so only the configured ones are passed.
		routeConfig := routes.Config{Location: location}
		if a.ns != nil {
			routeConfig.Trains = a.ns
		}
		if a.bus != nil {
			routeConfig.Buses = a.bus
		}
		routeService, err := routes.NewService(routeConfig, a.store, approvals, a.logger)
		if err != nil {
			return fmt.Errorf("initialize routes: %w", err)
		}
		if err := broker.Register(routeService); err != nil {
			return err
		}
		if err := approvals.RegisterExecutor(routeService); err != nil {
			return err
		}
		a.routes = routeService
		if a.watches != nil {
			if err := registerWatchKinds(a.watches, a.ns, a.bus, a.routes); err != nil {
				return err
			}
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
	deps := telegram.Dependencies{
		Client:     client,
		Identities: identities,
		Cursors:    a.store,
		Logger:     a.logger,
		Telemetry:  a.telemetry,
		Tabs:       a.store,
	}
	if a.conversation != nil {
		deps.Conversation = a.conversation
		deps.Attachments = a.attachments
		deps.Connections = a.connections
		deps.Approvals = a.approvals
		deps.Schedules = a.scheduler
		deps.Trips = a.trips
		deps.Routes = a.routes
		deps.Memory = a.memory
		deps.Backlog = a.backlog
		deps.Transcriber = a.chatGPT
	}
	if a.notion != nil {
		deps.Notion = a.notion
	}
	if a.nas != nil {
		deps.NAS = a.nas
	}
	if a.google != nil {
		deps.Google = a.google
	}
	if a.web != nil {
		deps.Web = a.switches.Toggle(web.Switch)
	}
	if a.ns != nil {
		deps.NS = a.switches.Toggle(ns.Switch)
	}
	if a.homeAssistant != nil {
		deps.HomeAssistant = homeSettings{Toggle: a.switches.Toggle(homeassistant.Switch), home: a.homeAssistant}
	}
	if a.bus != nil {
		deps.Bus = a.switches.Toggle(bus.Switch)
	}
	if a.typeSafe {
		deps.TypeSafe = a.switches.Toggle(judgment.OptInSwitch)
	}
	if a.locations != nil {
		deps.Locations = a.locations
	}
	if a.grafana != nil {
		deps.Grafana = grafanaSettings{Toggle: a.switches.Toggle(grafana.Switch), grafana: a.grafana}
	}
	if a.tasks != nil {
		deps.Tasks = a.tasks
	}
	if a.watches != nil {
		deps.Watches = a.watches
	}
	service, err := telegram.NewService(deps)
	if err != nil {
		return fmt.Errorf("initialize Telegram service: %w", err)
	}
	client.ObserveTabs(service)
	// Messages nobody asked for go to their feed tab while the bot has tabs on.
	if a.scheduler != nil {
		a.scheduler.SetFeeds(service.Feeds())
	}
	if a.watches != nil {
		a.watches.SetFeeds(service.Feeds())
	}
	if a.tripNotifier != nil {
		a.tripNotifier.Feeds = service.Feeds()
	}
	if a.conversation != nil {
		a.conversation.SetTabReplies(service)
	}
	if a.tools != nil {
		if err := a.tools.Register(service.Help()); err != nil {
			return err
		}
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
	if err := a.scheduler.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("start scheduler: %w", err), a.conversation.Shutdown(ctx), a.approvals.Shutdown(ctx), a.connections.Shutdown(ctx))
	}
	if a.tasks != nil {
		if err := a.tasks.Start(ctx); err != nil {
			return errors.Join(fmt.Errorf("start background tasks: %w", err), a.scheduler.Shutdown(ctx), a.conversation.Shutdown(ctx), a.approvals.Shutdown(ctx), a.connections.Shutdown(ctx))
		}
	}
	return nil
}

func (a *App) shutdownConversation(ctx context.Context) error {
	if a.conversation == nil {
		return nil
	}
	steps := []func(context.Context) error{
		labelled("shutdown scheduler", a.scheduler.Shutdown),
		labelled("shutdown conversation service", a.conversation.Shutdown),
		labelled("shutdown approvals", a.approvals.Shutdown),
		labelled("shutdown model connections", a.connections.Shutdown),
	}
	if a.tasks != nil {
		steps = append(steps, labelled("shutdown background tasks", a.tasks.Shutdown))
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

// registerWatchKinds offers the kinds of watch the configured connectors can check; each check
// still runs under its owner's own trains or buses switch.
func registerWatchKinds(watches *watch.Service, trains *ns.Service, buses *bus.Service, saved *routes.Service) error {
	if trains == nil && buses == nil {
		return nil
	}
	// A nil connector must reach the evaluator as a nil interface, not a typed nil.
	var trainDepartures, busDepartures transit.Departures
	if trains != nil {
		trainDepartures = trains
	}
	if buses != nil {
		busDepartures = buses
	}
	departures, err := watch.NewDepartureEvaluator(trainDepartures, busDepartures)
	if err != nil {
		return fmt.Errorf("initialize departure watches: %w", err)
	}
	if err := watches.Register(watch.KindDeparture, departures); err != nil {
		return err
	}
	if saved != nil {
		route, err := watch.NewRouteEvaluator(saved)
		if err != nil {
			return fmt.Errorf("initialize route watches: %w", err)
		}
		if err := watches.Register(watch.KindRoute, route); err != nil {
			return err
		}
	}
	if trains == nil {
		return nil
	}
	disruptions, err := watch.NewDisruptionsEvaluator(trains)
	if err != nil {
		return fmt.Errorf("initialize disruption watches: %w", err)
	}
	return watches.Register(watch.KindDisruptions, disruptions)
}

// homeSettings is what the home settings section reads: the principal's home switch and the
// connector's exposure check.
type homeSettings struct {
	integration.Toggle
	home *homeassistant.Service
}

func (h homeSettings) Exposure(ctx context.Context) homeassistant.Exposure {
	return h.home.Exposure(ctx)
}

// grafanaSettings is what the monitoring settings section reads: the principal's Grafana switch and
// the operator's allowlist.
type grafanaSettings struct {
	integration.Toggle
	grafana *grafana.Service
}

func (g grafanaSettings) Allowed(principalID string) bool {
	return g.grafana.Allowed(principalID)
}

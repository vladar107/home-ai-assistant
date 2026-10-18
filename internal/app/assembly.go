package app

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
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

// connector is what one integration adds to the application: its lifecycle, the tools it offers,
// the executor of its confirmed actions, the kinds of watch it checks, and its on/off page. Every
// field is optional, and each is set inside the integration's own configuration check, so a
// connector that is not configured adds nothing rather than a typed nil.
type connector struct {
	module   module.Module
	tools    []tools.Provider
	executor approval.Executor
	watches  map[string]watch.Evaluator
	switched *switchSettings
}

// switchSettings is what an integration's on/off page in /settings needs beyond its switch.
type switchSettings struct {
	integration integration.Switch
	// allowed limits the page to some principals; nil shows it to everyone.
	allowed func(principalID string) bool
	// exposure is the home's check of which devices are exposed; nil for the others.
	exposure func(context.Context) (checked bool, conflicts []string)
}

// assembly is everything the application builds before the Telegram service.
type assembly struct {
	cfg       config.Config
	store     *sqlite.Store
	registry  *module.Registry
	logger    *slog.Logger
	telemetry *observability.Telemetry

	principals   []string
	displayNames map[string]string
	location     *time.Location
	files        *delivery.Service
	cards        *outbox.Box
	exposures    *exposure.Service
	tripState    *tripScanState

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
	// switchPages are the on/off pages of the registered integrations, in registration order.
	switchPages []switchSettings
	typeSafe    bool
}

// register adds a connector to every part of the application it takes part in.
func (a *assembly) register(ctx context.Context, c connector) error {
	if c.module != nil {
		if err := a.registry.Register(c.module); err != nil {
			return err
		}
	}
	for _, provider := range c.tools {
		if err := a.tools.Register(provider); err != nil {
			return err
		}
	}
	if c.executor != nil {
		if err := a.approvals.RegisterExecutor(c.executor); err != nil {
			return err
		}
	}
	if a.watches != nil && len(c.watches) > 0 {
		kinds := slices.Sorted(maps.Keys(c.watches))
		for _, kind := range kinds {
			if err := a.watches.Register(kind, c.watches[kind]); err != nil {
				return err
			}
		}
		a.telemetry.ZeroWatchOutcomes(ctx, a.principals, kinds)
	}
	if c.switched != nil {
		a.switchPages = append(a.switchPages, *c.switched)
	}
	return nil
}

// buildCore builds the conversation and the services around it. Web access and shared locations
// are registered among them, where the model has always found their tools.
func (a *assembly) buildCore(ctx context.Context, client *telegram.Client) error {
	profilesDir := filepath.Join(a.cfg.DataDir, "pi")
	connections, err := connection.NewService(connection.Config{
		ProfilesDir: profilesDir,
		Provider:    a.cfg.Conversation.PiProvider,
		Principals:  a.principals,
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
	a.telemetry.ZeroRareOutcomes(ctx, a.principals, conversation.TurnErrorKinds(), reaction.Emojis(), conversation.FeedbackClasses())
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
	a.tools = broker
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
	a.files = files
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
	a.cards = cards
	approvals, err := approval.NewService(approval.Config{}, a.store, client, a.logger)
	if err != nil {
		return fmt.Errorf("initialize approvals: %w", err)
	}
	approvals.SetTelemetry(a.telemetry)
	approvals.SetSwitches(switches)
	a.approvals = approvals
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
	a.exposures = exposures
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
		if err := a.register(ctx, connector{switched: &switchSettings{integration: judgment.OptInSwitch}}); err != nil {
			return err
		}
	}
	// Scheduled runs reach only the owner's own chat, resolved from the allowlist at delivery.
	scheduler, err := schedule.NewService(schedule.Config{Zone: a.cfg.Timezone, Cards: cards, Photos: client, Reminders: telegram.ReminderSender{Client: client}, Sessions: runtime.SessionPruner(a.store)}, a.store, a.store, client, approvals, a.logger)
	if err != nil {
		return fmt.Errorf("initialize scheduler: %w", err)
	}
	scheduler.SetTelemetry(a.telemetry)
	scheduler.SetSafetyChecks(safetyChecks)
	scheduler.SetExposure(exposures)
	a.scheduler = scheduler
	tripService, err := trip.NewService(trip.Config{}, a.store, trip.NewScheduleReminders(scheduler), a.logger)
	if err != nil {
		return fmt.Errorf("initialize trips: %w", err)
	}
	scheduler.SetTravel(tripService)
	a.trips = tripService
	var trains transit.Trips
	if a.cfg.NS.Enabled() {
		a.ns, err = ns.NewService(ns.Config{APIKey: a.cfg.NS.APIKey}, switches, a.logger)
		if err != nil {
			return fmt.Errorf("initialize NS connector: %w", err)
		}
		trains = a.ns
	}
	a.location, err = time.LoadLocation(a.cfg.Timezone)
	if err != nil {
		return fmt.Errorf("load household time zone: %w", err)
	}
	var webSearch conversation.WebSearch
	if a.cfg.Web.Enabled {
		webService, err := web.NewService(web.Config{WeatherHome: a.cfg.Web.WeatherHome, Location: a.location, Cards: cards, Trains: trains}, switches, a.logger)
		if err != nil {
			return fmt.Errorf("initialize web connector: %w", err)
		}
		webService.SetTelemetry(a.telemetry)
		webService.SetExposure(exposures)
		if err := a.register(ctx, connector{module: webService, tools: []tools.Provider{webService}, switched: &switchSettings{integration: web.Switch}}); err != nil {
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
		if err := a.register(ctx, connector{module: placesService, tools: []tools.Provider{placesService}}); err != nil {
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
			Zone:          a.location,
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
	conversationHistory, err := history.NewService(a.store, runtime, a.location, a.telemetry)
	if err != nil {
		return fmt.Errorf("initialize conversation history: %w", err)
	}
	if err := broker.Register(conversationHistory); err != nil {
		return err
	}
	notices := conversation.Notices{approvals}
	var reports conversation.Reports = scheduler
	if a.cfg.Tasks.Enabled {
		tasks, err := task.NewService(task.Config{Timeout: a.cfg.Tasks.Timeout, Cards: cards, Photos: client}, a.store, a.store, telegram.TaskMessenger{Client: client}, client, a.logger)
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
		a.tasks = tasks
	}
	if a.cfg.Watches.Enabled {
		// Alerts reach only the owner's own chat, resolved from the allowlist at delivery.
		watches, err := watch.NewService(watch.Config{Zone: a.cfg.Timezone}, a.store, a.store, telegram.WatchAlertSender{Client: client}, approvals, a.logger)
		if err != nil {
			return fmt.Errorf("initialize watches: %w", err)
		}
		watches.SetTelemetry(a.telemetry)
		watches.SetTravel(tripService)
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
			Location:     a.location,
			Locations:    turnLocations,
			Trips:        tripService,
			Memory:       memoryService,
			Exposure:     exposures,
			SearchChecks: searchChecks,
			History:      conversationHistory,
			DisplayNames: a.displayNames,
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
	a.tripState = &tripScanState{schedules: scheduler}
	tripTools.SetScanState(a.tripState)
	tripNotifier := &telegram.TripNotifier{Client: client, Chats: a.store}
	a.tripNotifier = tripNotifier
	tripService.SetTabs(tripNotifier)
	if err := scheduler.Register(trip.ScanJobType(tripScanChecker{conversationService}, a.store, a.tripState, tripService, tripNotifier, tripNotifier, scheduler.Now, a.logger)); err != nil {
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
	backlogService, err := backlog.NewService(backlog.Config{Location: a.location}, a.store, approvals, a.logger)
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
	if a.tasks != nil {
		a.tasks.SetRunner(conversationService)
	}
	if a.watches != nil {
		a.watches.SetRewriter(conversationService)
		agent, err := watch.NewAgentEvaluator(conversationService, watch.AgentConfig{Finished: a.watches.Wake})
		if err != nil {
			return fmt.Errorf("initialize agent watches: %w", err)
		}
		if err := a.register(ctx, connector{watches: map[string]watch.Evaluator{watch.KindAgent: agent}}); err != nil {
			return err
		}
	}
	a.attachments = attachments
	a.connections = connections
	a.conversation = conversationService
	return nil
}

// buildConnectors builds the connectors the operator configured and the routes and watches that
// read them.
func (a *assembly) buildConnectors(ctx context.Context) error {
	if a.cfg.Notion.Enabled {
		notionService, err := notion.NewService(notion.Config{
			CredentialsDir: filepath.Join(a.cfg.DataDir, "integrations"),
			APIBaseURL:     a.cfg.Notion.APIBaseURL,
			ChatConnect:    a.cfg.Notion.ChatConnect,
			DisplayNames:   a.displayNames,
			Location:       a.location,
		}, a.store, a.approvals, a.logger)
		if err != nil {
			return fmt.Errorf("initialize Notion connector: %w", err)
		}
		if err := a.register(ctx, connector{module: notionService, tools: []tools.Provider{notionService}, executor: notionService}); err != nil {
			return err
		}
		a.notion = notionService
	}
	if a.cfg.NAS.Enabled {
		nasService, err := nas.NewService(nas.Config{
			Roots: nasRoots(a.cfg.NAS.Roots),
		}, a.store, a.approvals, a.attachments, a.files, a.logger)
		if err != nil {
			return fmt.Errorf("initialize NAS connector: %w", err)
		}
		if err := a.register(ctx, connector{module: nasService, tools: []tools.Provider{nasService}, executor: nasService}); err != nil {
			return err
		}
		a.nas = nasService
	}
	if a.cfg.Google.Enabled {
		secret := a.cfg.Google.ClientSecret
		if secret == "" {
			var err error
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
		gmailService, err := gmail.NewService(gmail.Config{}, googleService, a.logger)
		if err != nil {
			return fmt.Errorf("initialize Gmail connector: %w", err)
		}
		calendarService, err := calendar.NewService(calendar.Config{Location: a.location}, googleService, a.logger)
		if err != nil {
			return fmt.Errorf("initialize Calendar connector: %w", err)
		}
		if err := a.register(ctx, connector{module: googleService, tools: []tools.Provider{gmailService, calendarService}}); err != nil {
			return err
		}
		a.tripState.google = googleService
		a.google = googleService
	}
	if a.ns != nil {
		if err := a.register(ctx, connector{module: a.ns, tools: []tools.Provider{a.ns}, switched: &switchSettings{integration: ns.Switch}}); err != nil {
			return err
		}
	}
	if a.cfg.HA.Enabled() {
		homeAssistantService, err := homeassistant.NewService(homeassistant.Config{Token: a.cfg.HA.Token, BaseURL: a.cfg.HA.URL}, a.switches, a.approvals, a.logger)
		if err != nil {
			return fmt.Errorf("initialize Home Assistant connector: %w", err)
		}
		homeAssistantService.SetExposure(a.exposures)
		if err := a.register(ctx, connector{
			module:   homeAssistantService,
			tools:    []tools.Provider{homeAssistantService},
			executor: homeAssistantService,
			watches:  map[string]watch.Evaluator{watch.KindHome: homeAssistantService},
			switched: &switchSettings{integration: homeassistant.Switch, exposure: homeExposure(homeAssistantService)},
		}); err != nil {
			return err
		}
		a.homeAssistant = homeAssistantService
	}
	if a.cfg.Grafana.Enabled() {
		grafanaService, err := grafana.NewService(grafana.Config{
			Token:      a.cfg.Grafana.Token,
			BaseURL:    a.cfg.Grafana.URL,
			Principals: a.cfg.Grafana.Principals,
			Location:   a.location,
		}, a.logger)
		if err != nil {
			return fmt.Errorf("initialize Grafana connector: %w", err)
		}
		if err := a.register(ctx, connector{
			module:   grafanaService,
			tools:    []tools.Provider{grafanaService},
			switched: &switchSettings{integration: grafana.Switch, allowed: grafanaService.Allowed},
		}); err != nil {
			return err
		}
		a.grafana = grafanaService
	}
	if a.cfg.Bus.Enabled {
		busService, err := bus.NewService(bus.Config{HomeStop: a.cfg.Bus.HomeStop, Contact: a.cfg.Bus.Contact}, a.switches, a.logger)
		if err != nil {
			return fmt.Errorf("initialize bus connector: %w", err)
		}
		if err := a.register(ctx, connector{module: busService, tools: []tools.Provider{busService}, switched: &switchSettings{integration: bus.Switch}}); err != nil {
			return err
		}
		a.bus = busService
	}
	// Typed nils would hide a missing connector, so only the configured ones are passed.
	routeConfig := routes.Config{Location: a.location}
	if a.ns != nil {
		routeConfig.Trains = a.ns
	}
	if a.bus != nil {
		routeConfig.Buses = a.bus
	}
	routeService, err := routes.NewService(routeConfig, a.store, a.approvals, a.logger)
	if err != nil {
		return fmt.Errorf("initialize routes: %w", err)
	}
	if err := a.register(ctx, connector{tools: []tools.Provider{routeService}, executor: routeService}); err != nil {
		return err
	}
	a.routes = routeService
	transitWatches, err := a.transitWatches()
	if err != nil {
		return err
	}
	return a.register(ctx, connector{watches: transitWatches})
}

// transitWatches are the kinds of watch the configured transit connectors can check; each check
// still runs under its owner's own trains or buses switch.
func (a *assembly) transitWatches() (map[string]watch.Evaluator, error) {
	if a.ns == nil && a.bus == nil {
		return nil, nil
	}
	// A nil connector must reach the evaluator as a nil interface, not a typed nil.
	var trainDepartures, busDepartures transit.Departures
	if a.ns != nil {
		trainDepartures = a.ns
	}
	if a.bus != nil {
		busDepartures = a.bus
	}
	departures, err := watch.NewDepartureEvaluator(trainDepartures, busDepartures)
	if err != nil {
		return nil, fmt.Errorf("initialize departure watches: %w", err)
	}
	route, err := watch.NewRouteEvaluator(a.routes)
	if err != nil {
		return nil, fmt.Errorf("initialize route watches: %w", err)
	}
	evaluators := map[string]watch.Evaluator{watch.KindDeparture: departures, watch.KindRoute: route}
	if a.ns != nil {
		disruptions, err := watch.NewDisruptionsEvaluator(a.ns)
		if err != nil {
			return nil, fmt.Errorf("initialize disruption watches: %w", err)
		}
		evaluators[watch.KindDisruptions] = disruptions
	}
	return evaluators, nil
}

// homeExposure hands the Home Assistant connector's exposure check to the home settings page.
func homeExposure(home *homeassistant.Service) func(context.Context) (bool, []string) {
	return func(ctx context.Context) (bool, []string) {
		exposure := home.Exposure(ctx)
		return exposure.Checked, exposure.Conflicts
	}
}

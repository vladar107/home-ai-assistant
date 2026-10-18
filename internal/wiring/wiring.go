// Package wiring builds the application's services and connects them, everything except the
// Telegram service, which is built on top. Production and the Telegram tests both build through it,
// so the tests run the services production runs, wired the way production wires them.
package wiring

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/approval/withdrawal"
	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
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
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/imagegen"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	userlocation "github.com/vladar107/home-ai-assistant/internal/location"
	usermemory "github.com/vladar107/home-ai-assistant/internal/memory"
	"github.com/vladar107/home-ai-assistant/internal/module"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/reaction"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/searchcheck"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/task"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/transit"
	"github.com/vladar107/home-ai-assistant/internal/trip"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// Telemetry is what the assembled services record.
type Telemetry interface {
	conversation.Telemetry
	tools.Telemetry
	approval.Telemetry
	reaction.Telemetry
	usermemory.Telemetry
	searchcheck.Telemetry
	schedule.Telemetry
	task.Telemetry
	watch.Telemetry
	web.Telemetry
	history.Telemetry
	judgment.Recorder
	// ZeroRareOutcomes and ZeroWatchOutcomes create the series of outcomes that may not happen for
	// a long time, so they read zero rather than nothing.
	ZeroRareOutcomes(ctx context.Context, principals, turnErrorKinds, reactions, feedbackClasses []string)
	ZeroWatchOutcomes(ctx context.Context, principals, kinds []string)
}

// Chat is the chat the assistant talks to its users through: the Telegram client.
type Chat interface {
	conversation.Deliverer
	conversation.PhotoDeliverer
	connection.Deliverer
	approval.Deliverer
	delivery.Documents
	reaction.Reactor
}

// TripNotices sends trip cards and notices and keeps each planned trip's tab.
type TripNotices interface {
	trip.Tabs
	trip.CardSender
	trip.Sender
}

// Inputs is everything Build needs. The connector configurations are nil for a connector the
// operator did not configure; Build fills the fields they leave empty that it knows, such as the
// household time zone and the credentials directory.
type Inputs struct {
	Store *sqlite.Store
	// Registry receives the connectors' modules; nil starts a new one.
	Registry  *module.Registry
	Logger    *slog.Logger
	Telemetry Telemetry
	// Runtime runs the model. Where it implements conversation.Transcripts it also forgets a
	// finished conversation's photos.
	Runtime agentruntime.AgentRuntime
	// SessionFiles locates the runtime's session files, for the conversation history tools.
	SessionFiles history.Directories
	// Sessions removes old runtime sessions of finished runs; nil keeps them.
	Sessions schedule.SessionPruner

	Chat         Chat
	Reminders    schedule.ReminderDeliverer
	Alerts       watch.Sender
	TaskMessages task.Messenger
	TripNotices  TripNotices

	Principals []identity.ConfiguredPrincipal
	DataDir    string
	// Zone is the household's IANA time zone.
	Zone string
	Core CoreConfig

	Notion  *notion.Config
	NAS     *nas.Config
	Google  *GoogleConfig
	Web     *web.Config
	NS      *ns.Config
	HA      *homeassistant.Config
	Grafana *grafana.Config
	Bus     *bus.Config

	// ExtraProviders are tools offered ahead of every other, and WatchEvaluators replace or add the
	// evaluators of their kinds; both are for tests.
	ExtraProviders  []tools.Provider
	WatchEvaluators map[string]watch.Evaluator
}

// CoreConfig configures the services every assembly has, and the optional core features.
type CoreConfig struct {
	TurnTimeout time.Duration
	// TurnZone is the zone of the time line each turn's message starts with; nil leaves the line
	// out. The application uses the household zone.
	TurnZone *time.Location
	// TypingInterval, DraftDelay, and DraftInterval replace the turn indicator's timings; zero
	// keeps the defaults.
	TypingInterval, DraftDelay, DraftInterval time.Duration
	// Now is the clock of the conversation, schedules, watches, routes, trips, shared locations,
	// and search checks; nil means time.Now.
	Now func() time.Time
	// ScheduleTick is how often the scheduler looks for due jobs; zero keeps the default.
	ScheduleTick time.Duration
	// ImageModel generates pictures, on the principal's own ChatGPT subscription.
	ImageModel string
	ChatGPT    chatgpt.Config
	Connection connection.Config
	Approval   approval.Config
	// TypeSafe turns the safety checks on when it has a key.
	TypeSafe judgment.Config
	// Recent keeps each chat turn's exchange for the guards without TypeSafe's judge; TypeSafe
	// keeps its own.
	Recent *turnstate.Registry
	// Tasks turns background tasks on; nil leaves them off.
	Tasks *task.Config
	// Watches turns watches on.
	Watches bool
	// Location turns shared locations on; nil leaves them off.
	Location *LocationConfig
}

// LocationConfig configures shared locations and the places connector that names them.
type LocationConfig struct {
	Policy        userlocation.Policy
	PruneInterval time.Duration
	Places        places.Config
}

// GoogleConfig configures the Google accounts and the mail and calendar tools that read them.
type GoogleConfig struct {
	Accounts google.Config
	Mail     gmail.Config
	Calendar calendar.Config
}

// Assembly is what Build built. A service that was not configured is nil.
type Assembly struct {
	Registry      *module.Registry
	Switches      *integration.Switches
	Tools         *tools.Broker
	Connections   *connection.Service
	ChatGPT       *chatgpt.Client
	Attachments   *attachment.Service
	Outbox        *outbox.Box
	Conversation  *conversation.Service
	Approvals     *approval.Service
	SearchChecks  *searchcheck.Service
	Scheduler     *schedule.Service
	Tasks         *task.Service
	Watches       *watch.Service
	Notion        *notion.Service
	NAS           *nas.Service
	Google        *google.Service
	Web           *web.Service
	NS            *ns.Service
	HomeAssistant *homeassistant.Service
	Grafana       *grafana.Service
	Bus           *bus.Service
	Routes        *routes.Service
	Locations     *userlocation.Service
	Memory        *usermemory.Service
	Backlog       *backlog.Service
	Trips         *trip.Service
	// SwitchPages are the on/off pages of the registered integrations, in registration order.
	SwitchPages []SwitchSettings

	in           Inputs
	principals   []string
	displayNames map[string]string
	location     *time.Location
	files        *delivery.Service
	exposures    *exposure.Service
	tripState    *tripScanState
	watchKinds   map[string]bool
}

// Build builds every service and connector the inputs configure and connects them.
func Build(ctx context.Context, in Inputs) (*Assembly, error) {
	switch {
	case in.Store == nil:
		return nil, errors.New("wiring: store is required")
	case in.Logger == nil:
		return nil, errors.New("wiring: logger is required")
	case in.Telemetry == nil:
		return nil, errors.New("wiring: telemetry is required")
	case in.Runtime == nil:
		return nil, errors.New("wiring: model runtime is required")
	case in.SessionFiles == nil:
		return nil, errors.New("wiring: session files are required")
	case in.Chat == nil || in.Reminders == nil || in.Alerts == nil || in.TaskMessages == nil || in.TripNotices == nil:
		return nil, errors.New("wiring: the chat and its senders are required")
	case len(in.Principals) == 0:
		return nil, errors.New("wiring: principals are required")
	case in.DataDir == "":
		return nil, errors.New("wiring: data directory is required")
	}
	location, err := time.LoadLocation(in.Zone)
	if err != nil || in.Zone == "" {
		return nil, fmt.Errorf("load household time zone: %w", cmp.Or(err, errors.New("zone is required")))
	}
	a := &Assembly{
		Registry:     cmp.Or(in.Registry, module.NewRegistry()),
		in:           in,
		displayNames: make(map[string]string, len(in.Principals)),
		location:     location,
		watchKinds:   make(map[string]bool),
	}
	for _, principal := range in.Principals {
		a.principals = append(a.principals, principal.ID)
		a.displayNames[principal.ID] = principal.DisplayName
	}
	if err := a.buildCore(ctx); err != nil {
		return nil, err
	}
	if err := a.buildConnectors(ctx); err != nil {
		return nil, err
	}
	if err := a.registerTestWatches(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// Start starts the conversation and the services it runs on; the modules in Registry are started
// by the caller.
func (a *Assembly) Start(ctx context.Context) error {
	if err := a.Connections.Start(ctx); err != nil {
		return fmt.Errorf("start model connections: %w", err)
	}
	if err := a.Approvals.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("start approvals: %w", err), a.Connections.Shutdown(ctx))
	}
	if err := a.Conversation.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("start conversation service: %w", err), a.Approvals.Shutdown(ctx), a.Connections.Shutdown(ctx))
	}
	if err := a.Scheduler.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("start scheduler: %w", err), a.Conversation.Shutdown(ctx), a.Approvals.Shutdown(ctx), a.Connections.Shutdown(ctx))
	}
	if a.Tasks != nil {
		if err := a.Tasks.Start(ctx); err != nil {
			return errors.Join(fmt.Errorf("start background tasks: %w", err), a.Scheduler.Shutdown(ctx), a.Conversation.Shutdown(ctx), a.Approvals.Shutdown(ctx), a.Connections.Shutdown(ctx))
		}
	}
	return nil
}

// Shutdown stops what Start started, all at once.
func (a *Assembly) Shutdown(ctx context.Context) error {
	steps := []func(context.Context) error{
		Labelled("shutdown scheduler", a.Scheduler.Shutdown),
		Labelled("shutdown conversation service", a.Conversation.Shutdown),
		Labelled("shutdown approvals", a.Approvals.Shutdown),
		Labelled("shutdown model connections", a.Connections.Shutdown),
	}
	if a.Tasks != nil {
		steps = append(steps, Labelled("shutdown background tasks", a.Tasks.Shutdown))
	}
	return ShutdownTogether(ctx, steps...)
}

// ShutdownTogether runs every shutdown at once and waits for all of them. Each one cancels its own
// work before it waits, so running them in turn would let one slow service spend the whole budget
// before the next had even been told to stop.
func ShutdownTogether(ctx context.Context, steps ...func(context.Context) error) error {
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

// Labelled names the service a shutdown error came from.
func Labelled(label string, shutdown func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := shutdown(ctx); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		return nil
	}
}

// buildCore builds the conversation and the services around it. Web access and shared locations
// are registered among them, where the model has always found their tools.
func (a *Assembly) buildCore(ctx context.Context) error {
	in := a.in
	telemetry := in.Telemetry
	connections, err := connection.NewService(in.Core.Connection, in.Store, in.Chat, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize model connections: %w", err)
	}
	telemetry.ZeroRareOutcomes(ctx, a.principals, conversation.TurnErrorKinds(), reaction.Emojis(), conversation.FeedbackClasses())
	switches, err := integration.NewSwitches(in.Store, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize integration switches: %w", err)
	}
	a.Switches = switches
	broker, err := tools.NewBroker(in.Logger)
	if err != nil {
		return fmt.Errorf("initialize tool broker: %w", err)
	}
	broker.SetTelemetry(telemetry)
	broker.SetSwitches(switches)
	a.Tools = broker
	for _, provider := range in.ExtraProviders {
		if err := broker.Register(provider); err != nil {
			return err
		}
	}
	attachments, err := attachment.NewService(attachment.Config{
		Directory: filepath.Join(in.DataDir, "attachments"),
	}, in.Store, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize attachments: %w", err)
	}
	a.Attachments = attachments
	if err := broker.Register(attachments); err != nil {
		return err
	}
	// Pictures and voice use the principal's own subscription, the same one the runtime holds.
	chatGPT, err := chatgpt.New(in.Core.ChatGPT, connections)
	if err != nil {
		return fmt.Errorf("initialize ChatGPT client: %w", err)
	}
	a.ChatGPT = chatGPT
	// Files reach the user through their own private chat, which is the Telegram identity
	// the allowlist already binds to the principal running the turn.
	files, err := delivery.NewService(in.Store, in.Chat, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize file delivery: %w", err)
	}
	a.files = files
	images, err := imagegen.NewService(imagegen.Config{Model: in.Core.ImageModel}, chatGPT, files, attachments, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize image generation: %w", err)
	}
	if err := broker.Register(images); err != nil {
		return fmt.Errorf("register image generation: %w", err)
	}
	cards, err := outbox.New(filepath.Join(in.DataDir, "outbox"))
	if err != nil {
		return fmt.Errorf("initialize outbox: %w", err)
	}
	a.Outbox = cards
	approvals, err := approval.NewService(in.Core.Approval, in.Store, in.Chat, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize approvals: %w", err)
	}
	approvals.SetTelemetry(telemetry)
	approvals.SetSwitches(switches)
	a.Approvals = approvals
	if err := broker.Register(withdrawal.Provider{Withdrawer: approvals}); err != nil {
		return fmt.Errorf("register approval tools: %w", err)
	}
	reactions, err := reaction.NewProvider(in.Store, in.Chat, telemetry)
	if err != nil {
		return fmt.Errorf("initialize reactions: %w", err)
	}
	if err := broker.Register(reactions); err != nil {
		return fmt.Errorf("register reaction tools: %w", err)
	}
	memoryService, err := usermemory.NewService(usermemory.Config{}, in.Store, approvals, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize memory: %w", err)
	}
	memoryService.SetTelemetry(telemetry)
	exposures, err := exposure.NewService(in.Store, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize exposure: %w", err)
	}
	a.exposures = exposures
	broker.SetTurnExposure(exposures)
	broker.SetTurnPrivacy(exposures)
	approvals.SetExposure(exposures)
	searchChecks, err := searchcheck.NewService(exposures, approvals, in.Logger, in.Core.Now)
	if err != nil {
		return fmt.Errorf("initialize search checks: %w", err)
	}
	searchChecks.SetTelemetry(telemetry)
	a.SearchChecks = searchChecks
	if err := approvals.RegisterExecutor(searchChecks); err != nil {
		return err
	}
	// Interface values stay nil without a key, so no message or reply is kept in memory for nothing.
	var recent conversation.Exchange
	if in.Core.Recent != nil {
		recent = in.Core.Recent
	}
	var toolHint conversation.ToolHinter
	var safetyChecks conversation.SafetyChecks
	if in.Core.TypeSafe.APIKey != "" {
		registry := turnstate.New(nil)
		broker.SetExposure(registry)
		recent = registry
		judgeConfig := in.Core.TypeSafe
		judgeConfig.Logger, judgeConfig.Telemetry = in.Logger, telemetry
		judge, err := judgment.NewClient(judgeConfig)
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
		if err := a.register(ctx, Connector{Switch: &SwitchSettings{Switch: judgment.OptInSwitch}}); err != nil {
			return err
		}
	}
	// Scheduled runs reach only the owner's own chat, resolved from the allowlist at delivery.
	scheduler, err := schedule.NewService(schedule.Config{
		Zone: in.Zone, TickInterval: in.Core.ScheduleTick, Now: in.Core.Now,
		Cards: cards, Photos: in.Chat, Reminders: in.Reminders, Sessions: in.Sessions,
	}, in.Store, in.Store, in.Chat, approvals, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize scheduler: %w", err)
	}
	scheduler.SetTelemetry(telemetry)
	scheduler.SetSafetyChecks(safetyChecks)
	scheduler.SetExposure(exposures)
	a.Scheduler = scheduler
	tripService, err := trip.NewService(trip.Config{Now: in.Core.Now}, in.Store, trip.NewScheduleReminders(scheduler), in.Logger)
	if err != nil {
		return fmt.Errorf("initialize trips: %w", err)
	}
	scheduler.SetTravel(tripService)
	a.Trips = tripService
	var trains transit.Trips
	if in.NS != nil {
		a.NS, err = ns.NewService(*in.NS, switches, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize NS connector: %w", err)
		}
		trains = a.NS
	}
	var webSearch conversation.WebSearch
	if in.Web != nil {
		webConfig := *in.Web
		webConfig.Location = cmp.Or(webConfig.Location, a.location)
		webConfig.Cards = cards
		if trains != nil {
			webConfig.Trains = trains
		}
		webService, err := web.NewService(webConfig, switches, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize web connector: %w", err)
		}
		webService.SetTelemetry(telemetry)
		webService.SetExposure(exposures)
		if err := a.register(ctx, Connector{Module: webService, Tools: []tools.Provider{webService}, Switch: &SwitchSettings{Switch: web.Switch}}); err != nil {
			return err
		}
		a.Web = webService
		webSearch = webService
	}
	var turnLocations conversation.Locations
	if in.Core.Location != nil {
		placesService, err := places.NewService(in.Core.Location.Places, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize places connector: %w", err)
		}
		if err := a.register(ctx, Connector{Module: placesService, Tools: []tools.Provider{placesService}}); err != nil {
			return err
		}
		locationService, err := userlocation.NewService(userlocation.Config{
			Policy:        in.Core.Location.Policy,
			PruneInterval: in.Core.Location.PruneInterval,
			Describer:     placesService,
			Zone:          a.location,
			Now:           in.Core.Now,
		}, in.Store, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize locations: %w", err)
		}
		if err := a.Registry.Register(locationService); err != nil {
			return err
		}
		a.Locations = locationService
		turnLocations = locationService
	}
	conversationHistory, err := history.NewService(in.Store, in.SessionFiles, a.location, telemetry)
	if err != nil {
		return fmt.Errorf("initialize conversation history: %w", err)
	}
	if err := broker.Register(conversationHistory); err != nil {
		return err
	}
	notices := conversation.Notices{approvals}
	var reports conversation.Reports = scheduler
	if in.Core.Tasks != nil {
		taskConfig := *in.Core.Tasks
		taskConfig.Cards, taskConfig.Photos = cards, in.Chat
		tasks, err := task.NewService(taskConfig, in.Store, in.Store, in.TaskMessages, in.Chat, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize background tasks: %w", err)
		}
		tasks.SetTelemetry(telemetry)
		tasks.SetExposure(exposures)
		tasks.SetProposer(approvals)
		if err := broker.Register(tasks); err != nil {
			return err
		}
		if err := approvals.RegisterExecutor(tasks); err != nil {
			return err
		}
		notices = append(notices, conversation.OutsideNotice{Notice: tasks})
		a.Tasks = tasks
	}
	if in.Core.Watches {
		// Alerts reach only the owner's own chat, resolved from the allowlist at delivery.
		watches, err := watch.NewService(watch.Config{Zone: in.Zone, Now: in.Core.Now}, in.Store, in.Store, in.Alerts, approvals, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize watches: %w", err)
		}
		watches.SetTelemetry(telemetry)
		watches.SetTravel(tripService)
		if err := a.Registry.Register(watches); err != nil {
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
		a.Watches = watches
	}
	var transcripts conversation.Transcripts
	if runtime, ok := in.Runtime.(conversation.Transcripts); ok {
		transcripts = runtime
	}
	conversationService, err := conversation.NewService(
		conversation.Config{
			TurnTimeout:    in.Core.TurnTimeout,
			FollowUp:       approvals,
			Notice:         notices,
			Reports:        reports,
			Jobs:           scheduler,
			Attachments:    attachments,
			Transcripts:    transcripts,
			WebSearch:      webSearch,
			Cards:          cards,
			Photos:         in.Chat,
			Recent:         recent,
			ToolHint:       toolHint,
			SafetyChecks:   safetyChecks,
			Location:       in.Core.TurnZone,
			Now:            in.Core.Now,
			Locations:      turnLocations,
			Trips:          tripService,
			Memory:         memoryService,
			Exposure:       exposures,
			SearchChecks:   searchChecks,
			History:        conversationHistory,
			DisplayNames:   a.displayNames,
			Reactions:      reactions,
			TypingInterval: in.Core.TypingInterval,
			DraftDelay:     in.Core.DraftDelay,
			DraftInterval:  in.Core.DraftInterval,
		},
		in.Store,
		in.Runtime,
		in.Chat,
		broker,
		connections,
		in.Logger,
		telemetry,
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
	a.Memory = memoryService
	tripTools, err := trip.NewTools(trip.ToolsConfig{Zone: scheduler.DefaultZone()}, tripService, approvals, in.Logger)
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
	tripService.SetTabs(in.TripNotices)
	if err := scheduler.Register(trip.ScanJobType(tripScanChecker{conversationService}, in.Store, a.tripState, tripService, in.TripNotices, in.TripNotices, scheduler.Now, in.Logger)); err != nil {
		return err
	}
	paused := []trip.Paused{scheduler}
	if a.Watches != nil {
		paused = append(paused, a.Watches)
	}
	tripLoop, err := trip.NewLoop(trip.LoopConfig{Now: in.Core.Now}, tripService, paused, in.TripNotices, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize trip loop: %w", err)
	}
	if err := a.Registry.Register(tripLoop); err != nil {
		return err
	}
	backlogService, err := backlog.NewService(backlog.Config{Location: a.location}, in.Store, approvals, in.Logger)
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
	a.Backlog = backlogService
	if a.Tasks != nil {
		a.Tasks.SetRunner(conversationService)
	}
	if a.Watches != nil {
		a.Watches.SetRewriter(conversationService)
		agent, err := watch.NewAgentEvaluator(conversationService, watch.AgentConfig{Now: in.Core.Now, Finished: a.Watches.Wake})
		if err != nil {
			return fmt.Errorf("initialize agent watches: %w", err)
		}
		if err := a.register(ctx, Connector{Watches: map[string]watch.Evaluator{watch.KindAgent: agent}}); err != nil {
			return err
		}
	}
	a.Connections = connections
	a.Conversation = conversationService
	return nil
}

// buildConnectors builds the connectors the operator configured and the routes and watches that
// read them.
func (a *Assembly) buildConnectors(ctx context.Context) error {
	in := a.in
	if in.Notion != nil {
		notionConfig := *in.Notion
		notionConfig.CredentialsDir = cmp.Or(notionConfig.CredentialsDir, filepath.Join(in.DataDir, "integrations"))
		notionConfig.Location = cmp.Or(notionConfig.Location, a.location)
		if notionConfig.DisplayNames == nil {
			notionConfig.DisplayNames = a.displayNames
		}
		notionService, err := notion.NewService(notionConfig, in.Store, a.Approvals, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize Notion connector: %w", err)
		}
		if err := a.register(ctx, Connector{Module: notionService, Tools: []tools.Provider{notionService}, Executor: notionService}); err != nil {
			return err
		}
		a.Notion = notionService
	}
	if in.NAS != nil {
		nasService, err := nas.NewService(*in.NAS, in.Store, a.Approvals, a.Attachments, a.files, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize NAS connector: %w", err)
		}
		if err := a.register(ctx, Connector{Module: nasService, Tools: []tools.Provider{nasService}, Executor: nasService}); err != nil {
			return err
		}
		a.NAS = nasService
	}
	if in.Google != nil {
		accounts := in.Google.Accounts
		accounts.CredentialsDir = cmp.Or(accounts.CredentialsDir, filepath.Join(in.DataDir, "integrations"))
		googleService, err := google.NewService(accounts, in.Store, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize Google connector: %w", err)
		}
		gmailService, err := gmail.NewService(in.Google.Mail, googleService, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize Gmail connector: %w", err)
		}
		calendarConfig := in.Google.Calendar
		calendarConfig.Location = cmp.Or(calendarConfig.Location, a.location)
		calendarService, err := calendar.NewService(calendarConfig, googleService, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize Calendar connector: %w", err)
		}
		if err := a.register(ctx, Connector{Module: googleService, Tools: []tools.Provider{gmailService, calendarService}}); err != nil {
			return err
		}
		a.tripState.google = googleService
		a.Google = googleService
	}
	if a.NS != nil {
		if err := a.register(ctx, Connector{Module: a.NS, Tools: []tools.Provider{a.NS}, Switch: &SwitchSettings{Switch: ns.Switch}}); err != nil {
			return err
		}
	}
	if in.HA != nil {
		homeAssistantService, err := homeassistant.NewService(*in.HA, a.Switches, a.Approvals, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize Home Assistant connector: %w", err)
		}
		homeAssistantService.SetExposure(a.exposures)
		if err := a.register(ctx, Connector{
			Module:   homeAssistantService,
			Tools:    []tools.Provider{homeAssistantService},
			Executor: homeAssistantService,
			Watches:  map[string]watch.Evaluator{watch.KindHome: homeAssistantService},
			Switch:   &SwitchSettings{Switch: homeassistant.Switch, Exposure: homeExposure(homeAssistantService)},
		}); err != nil {
			return err
		}
		a.HomeAssistant = homeAssistantService
	}
	if in.Grafana != nil {
		grafanaConfig := *in.Grafana
		grafanaConfig.Location = cmp.Or(grafanaConfig.Location, a.location)
		grafanaService, err := grafana.NewService(grafanaConfig, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize Grafana connector: %w", err)
		}
		if err := a.register(ctx, Connector{
			Module: grafanaService,
			Tools:  []tools.Provider{grafanaService},
			Switch: &SwitchSettings{Switch: grafana.Switch, Allowed: grafanaService.Allowed},
		}); err != nil {
			return err
		}
		a.Grafana = grafanaService
	}
	if in.Bus != nil {
		busService, err := bus.NewService(*in.Bus, a.Switches, in.Logger)
		if err != nil {
			return fmt.Errorf("initialize bus connector: %w", err)
		}
		if err := a.register(ctx, Connector{Module: busService, Tools: []tools.Provider{busService}, Switch: &SwitchSettings{Switch: bus.Switch}}); err != nil {
			return err
		}
		a.Bus = busService
	}
	// Typed nils would hide a missing connector, so only the configured ones are passed.
	routeConfig := routes.Config{Location: a.location, Now: in.Core.Now}
	if a.NS != nil {
		routeConfig.Trains = a.NS
	}
	if a.Bus != nil {
		routeConfig.Buses = a.Bus
	}
	routeService, err := routes.NewService(routeConfig, in.Store, a.Approvals, in.Logger)
	if err != nil {
		return fmt.Errorf("initialize routes: %w", err)
	}
	if err := a.register(ctx, Connector{Tools: []tools.Provider{routeService}, Executor: routeService}); err != nil {
		return err
	}
	a.Routes = routeService
	transitWatches, err := a.transitWatches()
	if err != nil {
		return err
	}
	return a.register(ctx, Connector{Watches: transitWatches})
}

// transitWatches are the kinds of watch the configured transit connectors can check; each check
// still runs under its owner's own trains or buses switch.
func (a *Assembly) transitWatches() (map[string]watch.Evaluator, error) {
	if a.NS == nil && a.Bus == nil {
		return nil, nil
	}
	// A nil connector must reach the evaluator as a nil interface, not a typed nil.
	var trainDepartures, busDepartures transit.Departures
	if a.NS != nil {
		trainDepartures = a.NS
	}
	if a.Bus != nil {
		busDepartures = a.Bus
	}
	departures, err := watch.NewDepartureEvaluator(trainDepartures, busDepartures)
	if err != nil {
		return nil, fmt.Errorf("initialize departure watches: %w", err)
	}
	route, err := watch.NewRouteEvaluator(a.Routes)
	if err != nil {
		return nil, fmt.Errorf("initialize route watches: %w", err)
	}
	evaluators := map[string]watch.Evaluator{watch.KindDeparture: departures, watch.KindRoute: route}
	if a.NS != nil {
		disruptions, err := watch.NewDisruptionsEvaluator(a.NS)
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

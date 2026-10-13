package telegram

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/approval/withdrawal"
	"github.com/vladar107/home-ai-assistant/internal/attachment"
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
	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/conversation/history"
	"github.com/vladar107/home-ai-assistant/internal/delivery"
	"github.com/vladar107/home-ai-assistant/internal/exposure"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/imagegen"
	"github.com/vladar107/home-ai-assistant/internal/location"
	"github.com/vladar107/home-ai-assistant/internal/memory"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/reaction"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/searchcheck"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/task"
	"github.com/vladar107/home-ai-assistant/internal/testutil/calendarsim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/chatgptsim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/codexauth"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// integrationTimeout bounds a whole test, not one exchange, and the longest tests here run more
// than a dozen sequential round-trips. It is only an upper bound, so a generous value costs a
// passing run nothing and keeps a loaded CI runner from failing at whatever step crosses it.
const integrationTimeout = 60 * time.Second

const (
	recoverySessionID        = "11111111-1111-4111-8111-111111111111"
	recoveryRuntimeSessionID = "22222222-2222-4222-8222-222222222222"
	recoveryRequestID        = "33333333-3333-4333-8333-333333333333"
)

type runtimeCall struct {
	request agentruntime.RunRequest
	ctx     context.Context
	sink    agentruntime.EventSink
	answer  chan agentruntime.RunResult
}

type controlledRuntime struct{ calls chan runtimeCall }

func newControlledRuntime() *controlledRuntime {
	return &controlledRuntime{calls: make(chan runtimeCall, 20)}
}

func (r *controlledRuntime) Run(ctx context.Context, request agentruntime.RunRequest, sink agentruntime.EventSink) (agentruntime.RunResult, error) {
	call := runtimeCall{request: request, ctx: ctx, sink: sink, answer: make(chan agentruntime.RunResult, 1)}
	r.calls <- call
	select {
	case result := <-call.answer:
		return result, nil
	case <-ctx.Done():
		return agentruntime.RunResult{Response: "late answer after cancellation"}, nil
	}
}

type sentMessage struct {
	chatID    int64
	replyTo   int64
	text      string
	parseMode string
	keyboard  InlineKeyboard
	status    int
	messageID int64
}

type sentDocument struct {
	chatID int64
	name   string
	data   []byte
}

type editedMessage struct {
	chatID    int64
	messageID int64
	text      string
	keyboard  InlineKeyboard
}

type callbackAnswer struct {
	id   string
	text string
}

type deletedMessage struct {
	chatID    int64
	messageID int64
}

type commandRegistration struct {
	commands []BotCommand
	scope    []string
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type harnessOptions struct {
	// chatGPT adds voice transcription and the image tool against the given backend.
	chatGPT             *chatgptsim.Simulator
	profilesDir         string
	codeTimeout         time.Duration
	providers           []tools.Provider
	notion              *notionsim.Simulator
	notionChatConnect   bool
	notionIntakeTimeout time.Duration
	approvalLifetime    time.Duration
	approvalSweep       time.Duration
	nasRoots            []nas.PrincipalRoots
	// schedules adds the scheduler, its tools, and /jobs, on the given clock.
	schedules *testClock
	google    *googlesim.Simulator
	// calendar serves the Calendar API for the google simulator's tokens; nil leaves Calendar out.
	calendar *calendarsim.Simulator
	// web adds the web connector and its settings section, as HOME_AGENT_WEB_ENABLED does.
	web bool
	// ns adds the NS connector against the given gateway, as an operator key does.
	ns string
	// homeAssistant adds the Home Assistant connector against the given URL, as an operator token does.
	homeAssistant string
	// grafana adds the Grafana connector against the given URL for alice alone, as an operator token does.
	grafana string
	// bus adds the bus connector against the given base URL, as HOME_AGENT_BUS_ENABLED does, with
	// busContact as the operator's Transitous contact.
	bus        string
	busContact string
	// cards adds the outbox and the stage_card test tool that stages a card for the turn's reply.
	cards       bool
	turnTimeout time.Duration
	// location turns on the current-time line, read from clock when one is given.
	location *time.Location
	clock    *testClock
	// locations enables shared locations on the clock, naming every place locationName.
	locations    bool
	locationName string
	pinTTL       time.Duration
	// memory adds per-user memory with its tools, /memory, and its settings switch, with third-party
	// exposure recorded on the conversation.
	memory bool
	// history reads Pi's session files from these directories for /sessions and the history tools.
	history history.Directories
	// tasks adds background tasks with this configuration, as HOME_AGENT_TASKS_ENABLED does.
	tasks *task.Config
	// watches adds the watch service, on the schedules clock, checking departures with this
	// evaluator; tests run its cycles themselves.
	watches watch.Evaluator
	// routes adds saved routes with their tools and /jobs entries, checked through the ns and bus
	// connectors where those are set.
	routes bool
	// search adds the web search check, as the application does, so a query after outside content
	// is allowed, held for confirmation, or denied.
	search bool
	// recent keeps each chat turn's message and plain reply for the guards, as a TypeSafe key does.
	recent *turnstate.Registry
	// typingInterval, draftDelay, and draftInterval replace the turn indicator's timings.
	typingInterval, draftDelay, draftInterval time.Duration
	// reactions adds the react tool, as the application always does.
	reactions bool
}

type fixedDescriber string

func (d fixedDescriber) Describe(context.Context, float64, float64) (string, error) {
	return string(d), nil
}

// testFile is one attachment the Telegram simulator will hand out through getFile.
type testFile struct {
	path   string
	data   []byte
	status int
}

type conversationHarness struct {
	t              *testing.T
	ctx            context.Context
	store          *sqlite.Store
	db             *sql.DB
	client         *Client
	service        *Service
	conversation   *conversation.Service
	attachments    *attachment.Service
	attachmentsDir string
	files          map[string]testFile
	connections    *connection.Service
	notion         *notion.Service
	nas            *nas.Service
	google         *google.Service
	web            *web.Service
	ns             *ns.Service
	homeAssistant  *homeassistant.Service
	grafana        *grafana.Service
	bus            *bus.Service
	locations      *location.Service
	memory         *memory.Service
	searches       *searchcheck.Service
	approvals      *approval.Service
	scheduler      *schedule.Service
	tasks          *task.Service
	watches        *watch.Service
	routes         *routes.Service
	auth           *codexauth.Simulator
	profilesDir    string
	credentialsDir string
	sent           []string
	updates        chan []Update
	messages       chan sentMessage
	documents      chan sentDocument
	photos         chan sentPhoto
	outbox         *outbox.Box
	outboxDir      string
	photoStatuses  []int
	photoReason    string
	edits          chan editedMessage
	deletes        chan deletedMessage
	answers        chan callbackAnswer
	offsets        chan int64
	registrations  chan commandRegistration
	turns          chan turnOutcome
	logs           *lockedBuffer
	mu             sync.Mutex
	sentCount      int64
	sendStatuses   []int
	sendReason     string
	sendGate       <-chan struct{}
	commandStatus  int
	editStatus     int
	editReason     string
	deleteStatus   int
	drafts         []draftRequest
	chatActions    []time.Time
	actionStatus   int
	draftStatus    int
	// indicatorFailures counts the typing and draft failures the conversation reported, by method.
	indicatorFailures map[string]int
	reactions         []setReaction
	// reactionCounts counts the reactions the react tool reported to telemetry, by emoji.
	reactionCounts map[string]int
	// feedbackCounts counts the user reactions and reconsider outcomes the conversation reported,
	// and the reply conversions the react tool reported.
	feedbackCounts map[string]int
}

type setReaction struct {
	chatID, messageID int64
	emoji             string
}

type draftRequest struct {
	chatID, draftID int64
	text            string
	canStop         bool
}

type turnOutcome struct{ outcome, errorKind string }

type integrationTelemetry struct {
	turns chan turnOutcome
	// indicatorFailure, when set, receives each failed typing or draft call by method.
	indicatorFailure func(method string)
	// feedback, when set, receives each user reaction class and reconsider outcome as "kind:value".
	feedback func(counted string)
}

func (integrationTelemetry) ContextLogger(_ context.Context, logger *slog.Logger) *slog.Logger {
	return logger
}
func (i integrationTelemetry) StartConversationTurn(ctx context.Context, _, _ string) (context.Context, func(string, string)) {
	return ctx, func(outcome, errorKind string) {
		select {
		case i.turns <- turnOutcome{outcome: outcome, errorKind: errorKind}:
		default:
		}
	}
}

func (integrationTelemetry) RecordReply(context.Context, string, string, string, string, time.Duration) {
}
func (integrationTelemetry) RecordReplyStage(context.Context, string, string, time.Duration) {}
func (integrationTelemetry) RecordModelTurn(context.Context, string, string, int, int64, int64) {
}
func (integrationTelemetry) RecordToolCallsPerTurn(context.Context, string, string, int) {}
func (integrationTelemetry) RecordHintFollowThrough(context.Context, string, string)     {}
func (integrationTelemetry) RecordConversationResume(context.Context, string, string)    {}
func (i integrationTelemetry) RecordTurnIndicatorFailure(_ context.Context, _, method string) {
	if i.indicatorFailure != nil {
		i.indicatorFailure(method)
	}
}
func (integrationTelemetry) RecordConversationSearch(context.Context, string, string) {}
func (i integrationTelemetry) RecordReplyFeedback(_ context.Context, _, class string) {
	if i.feedback != nil {
		i.feedback("class:" + class)
	}
}
func (i integrationTelemetry) RecordReconsider(_ context.Context, _, outcome string) {
	if i.feedback != nil {
		i.feedback("reconsider:" + outcome)
	}
}

func newConversationHarness(t *testing.T, runtime agentruntime.AgentRuntime) *conversationHarness {
	t.Helper()
	return newConversationHarnessWithOptions(t, runtime, harnessOptions{})
}

func newConversationHarnessWithOptions(t *testing.T, runtime agentruntime.AgentRuntime, options harnessOptions) *conversationHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	t.Cleanup(cancel)
	h := &conversationHarness{
		t: t, ctx: ctx, updates: make(chan []Update, 20), messages: make(chan sentMessage, 100), documents: make(chan sentDocument, 20), photos: make(chan sentPhoto, 20), edits: make(chan editedMessage, 100),
		answers: make(chan callbackAnswer, 100), deletes: make(chan deletedMessage, 100), offsets: make(chan int64, 100), registrations: make(chan commandRegistration, 10),
		turns: make(chan turnOutcome, 20), logs: &lockedBuffer{}, files: make(map[string]testFile), indicatorFailures: make(map[string]int),
		reactionCounts: make(map[string]int), feedbackCounts: make(map[string]int),
	}
	server := httptest.NewServer(http.HandlerFunc(h.serveHTTP))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "state", "assistant.sqlite")
	var err error
	h.store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.store.Close(); err != nil {
			t.Error(err)
		}
	})
	h.db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.db.Close() })
	identities, err := identity.NewService(h.store, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 202},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := identities.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	h.client, err = NewClient(ClientConfig{BaseURL: server.URL, Token: "999:test-token", PollTimeout: time.Second, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(h.logs, nil))
	h.profilesDir = options.profilesDir
	if h.profilesDir == "" {
		h.profilesDir = filepath.Join(t.TempDir(), "profiles")
	}
	h.auth = codexauth.New(t)
	h.connections, err = connection.NewService(connection.Config{
		ProfilesDir: h.profilesDir, AuthBaseURL: h.auth.URL(), HTTPClient: h.auth.Client(),
		CodeTimeout: options.codeTimeout, Principals: []string{"alice", "bob"},
	}, h.store, h.client, logger)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := tools.NewBroker(logger)
	if err != nil {
		t.Fatal(err)
	}
	exposures, err := exposure.NewService(h.store, logger)
	if err != nil {
		t.Fatal(err)
	}
	broker.SetTurnExposure(exposures)
	broker.SetTurnPrivacy(exposures)
	for _, provider := range options.providers {
		if err := broker.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	h.attachmentsDir = filepath.Join(t.TempDir(), "attachments")
	h.attachments, err = attachment.NewService(attachment.Config{Directory: h.attachmentsDir}, h.store, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(h.attachments); err != nil {
		t.Fatal(err)
	}
	var transcriber Transcriber
	if sim := options.chatGPT; sim != nil {
		client, err := chatgpt.New(chatgpt.Config{BaseURL: sim.URL(), HTTPClient: sim.Client()}, h.connections)
		if err != nil {
			t.Fatal(err)
		}
		files, err := delivery.NewService(h.store, h.client, logger)
		if err != nil {
			t.Fatal(err)
		}
		images, err := imagegen.NewService(imagegen.Config{Model: "gpt-test:medium"}, client, files, h.attachments, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(images); err != nil {
			t.Fatal(err)
		}
		transcriber = client
	}
	h.approvals, err = approval.NewService(approval.Config{Lifetime: options.approvalLifetime, SweepInterval: options.approvalSweep}, h.store, h.client, logger)
	if err != nil {
		t.Fatal(err)
	}
	// Tests that assert a turn's exact tools leave the withdraw tool out by not using the scheduler.
	if options.schedules != nil {
		if err := broker.Register(withdrawal.Provider{Withdrawer: h.approvals}); err != nil {
			t.Fatal(err)
		}
	}
	var reactions conversation.Reactions
	if options.reactions {
		provider, err := reaction.NewProvider(h.store, h.client, reactionTelemetry{h})
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(provider); err != nil {
			t.Fatal(err)
		}
		reactions = provider
	}
	h.credentialsDir = filepath.Join(t.TempDir(), "integrations")
	var notionController NotionController
	if options.notion != nil {
		h.notion, err = notion.NewService(notion.Config{
			CredentialsDir: h.credentialsDir, APIBaseURL: options.notion.URL(), ChatConnect: options.notionChatConnect,
			HTTPClient: options.notion.Client(), IntakeLifetime: options.notionIntakeTimeout,
		}, h.store, h.approvals, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(h.notion); err != nil {
			t.Fatal(err)
		}
		if err := h.approvals.RegisterExecutor(h.notion); err != nil {
			t.Fatal(err)
		}
		notionController = h.notion
	}
	var nasController NASController
	if len(options.nasRoots) > 0 {
		files, err := delivery.NewService(h.store, h.client, logger)
		if err != nil {
			t.Fatal(err)
		}
		h.nas, err = nas.NewService(nas.Config{Roots: options.nasRoots}, h.store, h.approvals, h.attachments, files, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(h.nas); err != nil {
			t.Fatal(err)
		}
		if err := h.approvals.RegisterExecutor(h.nas); err != nil {
			t.Fatal(err)
		}
		nasController = h.nas
	}
	var googleController GoogleController
	if sim := options.google; sim != nil {
		h.google, err = google.NewService(google.Config{
			CredentialsDir: h.credentialsDir, ClientID: googlesim.ClientID, ClientSecret: googlesim.ClientSecret,
			RedirectURL: googlesim.RedirectURL, AuthURL: sim.AuthURL(), TokenURL: sim.TokenURL(), RevokeURL: sim.RevokeURL(),
			UserInfoURL: sim.UserInfoURL(), HTTPClient: sim.Client(),
		}, h.store, logger)
		if err != nil {
			t.Fatal(err)
		}
		mail, err := gmail.NewService(gmail.Config{APIBaseURL: sim.URL(), HTTPClient: sim.Client()}, h.google, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(mail); err != nil {
			t.Fatal(err)
		}
		if calendarSim := options.calendar; calendarSim != nil {
			calendars, err := calendar.NewService(calendar.Config{
				Location: time.UTC, APIBaseURL: calendarSim.APIBaseURL(), HTTPClient: calendarSim.Client(),
			}, h.google, logger)
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Register(calendars); err != nil {
				t.Fatal(err)
			}
		}
		googleController = h.google
	}
	var webController WebController
	var webSearch conversation.WebSearch
	if options.web {
		h.web, err = web.NewService(web.Config{}, h.store, logger)
		if err != nil {
			t.Fatal(err)
		}
		h.web.SetExposure(exposures)
		if err := broker.Register(h.web); err != nil {
			t.Fatal(err)
		}
		webController, webSearch = h.web, h.web
	}
	var nsController NSController
	if options.ns != "" {
		h.ns, err = ns.NewService(ns.Config{APIKey: "test-key", BaseURL: options.ns}, h.store, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(h.ns); err != nil {
			t.Fatal(err)
		}
		nsController = h.ns
	}
	var homeAssistantController HomeAssistantController
	if options.homeAssistant != "" {
		h.homeAssistant, err = homeassistant.NewService(homeassistant.Config{Token: "test-token", BaseURL: options.homeAssistant}, h.store, h.approvals, logger)
		if err != nil {
			t.Fatal(err)
		}
		h.homeAssistant.SetExposure(exposures)
		if err := broker.Register(h.homeAssistant); err != nil {
			t.Fatal(err)
		}
		homeAssistantController = h.homeAssistant
	}
	if options.grafana != "" {
		h.grafana, err = grafana.NewService(grafana.Config{Token: "test-token", BaseURL: options.grafana, Principals: []string{"alice"}}, h.store, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(h.grafana); err != nil {
			t.Fatal(err)
		}
	}
	var busController BusController
	if options.bus != "" {
		h.bus, err = bus.NewService(bus.Config{
			HomeStop: "50420100", Contact: options.busContact,
			OVapiURL: options.bus, GTFSURL: options.bus, TransitousURL: options.bus,
		}, h.store, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(h.bus); err != nil {
			t.Fatal(err)
		}
		busController = h.bus
	}
	var cards conversation.Cards
	var photos conversation.PhotoDeliverer
	if options.cards {
		h.outboxDir = filepath.Join(t.TempDir(), "outbox")
		h.outbox, err = outbox.New(h.outboxDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(cardProvider{box: h.outbox}); err != nil {
			t.Fatal(err)
		}
		cards, photos = h.outbox, h.client
	}
	turnTimeout := options.turnTimeout
	if turnTimeout == 0 {
		turnTimeout = 10 * time.Second
	}
	notices := conversation.Notices{h.approvals}
	var reports conversation.Reports
	var jobs conversation.Jobs
	var scheduleController ScheduleController
	if options.schedules != nil {
		h.scheduler, err = schedule.NewService(schedule.Config{Zone: "Europe/Amsterdam", TickInterval: 10 * time.Millisecond, Now: options.schedules.Now, Cards: cards, Photos: photos, Reminders: ReminderSender{Client: h.client}},
			h.store, h.store, h.client, h.approvals, logger)
		if err != nil {
			t.Fatal(err)
		}
		h.scheduler.SetExposure(exposures)
		if err := broker.Register(h.scheduler); err != nil {
			t.Fatal(err)
		}
		if err := h.approvals.RegisterExecutor(h.scheduler); err != nil {
			t.Fatal(err)
		}
		scheduleController = h.scheduler
		reports = h.scheduler
		jobs = h.scheduler
	}
	if options.watches != nil {
		h.watches, err = watch.NewService(watch.Config{Zone: "Europe/Amsterdam", Now: options.schedules.Now}, h.store, h.store, WatchAlertSender{Client: h.client}, h.approvals, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.watches.Register(watch.KindDeparture, options.watches); err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(h.watches); err != nil {
			t.Fatal(err)
		}
		if err := h.approvals.RegisterExecutor(h.watches); err != nil {
			t.Fatal(err)
		}
	}
	if options.routes {
		amsterdam, err := time.LoadLocation("Europe/Amsterdam")
		if err != nil {
			t.Fatal(err)
		}
		routeConfig := routes.Config{Location: amsterdam, Now: options.schedules.Now}
		if h.ns != nil {
			routeConfig.Trains = h.ns
		}
		if h.bus != nil {
			routeConfig.Buses = h.bus
		}
		h.routes, err = routes.NewService(routeConfig, h.store, h.approvals, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(h.routes); err != nil {
			t.Fatal(err)
		}
		if err := h.approvals.RegisterExecutor(h.routes); err != nil {
			t.Fatal(err)
		}
	}
	if options.tasks != nil {
		taskConfig := *options.tasks
		taskConfig.Cards, taskConfig.Photos = cards, photos
		h.tasks = h.newTaskService(taskConfig)
		if err := broker.Register(h.tasks); err != nil {
			t.Fatal(err)
		}
		notices = append(notices, conversation.OutsideNotice{Notice: h.tasks})
	}
	var now func() time.Time
	if options.clock != nil {
		now = options.clock.Now
	}
	var locationTurns conversation.Locations
	if options.locations {
		ttl := cmp.Or(options.pinTTL, time.Hour)
		h.locations, err = location.NewService(location.Config{
			Policy:        location.Policy{PinTTL: ttl, LiveSilence: time.Hour, LiveGrace: 15 * time.Minute},
			PruneInterval: time.Hour, Describer: fixedDescriber(cmp.Or(options.locationName, "Stationsplein 1, Leiden")),
			Zone: cmp.Or(options.location, time.UTC), Now: now,
		}, h.store, logger)
		if err != nil {
			t.Fatal(err)
		}
		locationTurns = h.locations
	}
	var memoryTurns conversation.Memory
	if options.memory {
		h.memory, err = memory.NewService(memory.Config{}, h.store, h.approvals, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(h.memory); err != nil {
			t.Fatal(err)
		}
		if err := h.approvals.RegisterExecutor(h.memory); err != nil {
			t.Fatal(err)
		}
		memoryTurns = h.memory
	}
	var searchChecks conversation.SearchChecks
	if options.search {
		h.searches, err = searchcheck.NewService(exposures, h.approvals, logger, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.approvals.RegisterExecutor(h.searches); err != nil {
			t.Fatal(err)
		}
		searchChecks = h.searches
	}
	var conversationHistory conversation.History
	if options.history != nil {
		reader, err := history.NewService(h.store, options.history, options.location, integrationTelemetry{turns: h.turns})
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(reader); err != nil {
			t.Fatal(err)
		}
		conversationHistory = reader
	}
	var recent conversation.Exchange
	if options.recent != nil {
		recent = options.recent
	}
	h.conversation, err = conversation.NewService(
		conversation.Config{Recent: recent, Memory: memoryTurns, Exposure: exposures, SearchChecks: searchChecks, Locations: locationTurns, TurnTimeout: turnTimeout, FollowUp: h.approvals, Notice: notices, Reports: reports, Jobs: jobs, Attachments: h.attachments, WebSearch: webSearch, Cards: cards, Photos: photos, Location: options.location, Now: now, History: conversationHistory, Reactions: reactions,
			TypingInterval: options.typingInterval, DraftDelay: options.draftDelay, DraftInterval: options.draftInterval},
		h.store, runtime, h.client, broker, h.connections, logger, integrationTelemetry{turns: h.turns, indicatorFailure: h.recordIndicatorFailure, feedback: h.recordFeedback},
	)
	if err != nil {
		t.Fatal(err)
	}
	if h.scheduler != nil {
		if err := h.scheduler.Register(schedule.ModelTurn(h.conversation)); err != nil {
			t.Fatal(err)
		}
		if err := h.scheduler.Register(schedule.Reminder(h.scheduler.Now)); err != nil {
			t.Fatal(err)
		}
	}
	h.service, err = NewService(h.client, identities, h.store, h.conversation, h.attachments, h.connections, notionController, nasController, googleController, webController, nsController, homeAssistantController, busController, h.approvals, scheduleController, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(h.service.Help()); err != nil {
		t.Fatal(err)
	}
	if h.locations != nil {
		h.service.SetLocations(h.locations)
	}
	if h.grafana != nil {
		h.service.SetGrafana(h.grafana)
	}
	if h.memory != nil {
		h.service.SetMemory(h.memory)
	}
	if h.tasks != nil {
		h.tasks.SetRunner(h.conversation)
		h.service.SetTasks(h.tasks)
	}
	if h.watches != nil {
		h.service.SetWatches(h.watches)
	}
	if h.routes != nil {
		h.service.SetRoutes(h.routes)
	}
	if transcriber != nil {
		h.service.SetTranscriber(transcriber)
	}
	t.Cleanup(h.stop)
	return h
}

func (h *conversationHarness) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if route, isFile := strings.CutPrefix(r.URL.Path, "/file/"); isFile {
		h.serveFileDownload(w, route)
		return
	}
	if filepath.Base(r.URL.Path) == "sendDocument" {
		h.serveDocumentUpload(w, r)
		return
	}
	if filepath.Base(r.URL.Path) == "sendPhoto" {
		h.servePhotoUpload(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.t.Error(err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var result any = struct{}{}
	switch filepath.Base(r.URL.Path) {
	case "getMe":
		result = Bot{ID: 999, Username: "assistant_test_bot"}
	case "getUpdates":
		offset, err := strconv.ParseInt(r.Form.Get("offset"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		select {
		case h.offsets <- offset:
		case <-r.Context().Done():
			return
		}
		select {
		case result = <-h.updates:
		case <-r.Context().Done():
			return
		}
	case "sendMessage":
		chatID, err := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		var replyTo int64
		if raw := r.Form.Get("reply_parameters"); raw != "" {
			var parameters struct {
				MessageID                int64 `json:"message_id"`
				AllowSendingWithoutReply bool  `json:"allow_sending_without_reply"`
			}
			if err := json.Unmarshal([]byte(raw), &parameters); err != nil || parameters.MessageID <= 0 || !parameters.AllowSendingWithoutReply {
				h.t.Errorf("reply_parameters %q must quote a message and allow sending without it: %v", raw, err)
			}
			replyTo = parameters.MessageID
		}
		h.mu.Lock()
		status := http.StatusOK
		if len(h.sendStatuses) > 0 {
			status = h.sendStatuses[0]
			h.sendStatuses = h.sendStatuses[1:]
		}
		gate, reason := h.sendGate, h.sendReason
		h.sent = append(h.sent, r.Form.Get("text"))
		h.sentCount++
		messageID := sentMessageID(h.sentCount)
		h.mu.Unlock()
		select {
		case h.messages <- sentMessage{chatID: chatID, replyTo: replyTo, text: r.Form.Get("text"), parseMode: r.Form.Get("parse_mode"), keyboard: h.parseKeyboard(r.Form.Get("reply_markup")), status: status, messageID: messageID}:
		case <-r.Context().Done():
			return
		}
		result = Message{MessageID: messageID, Chat: Chat{ID: chatID, Type: ChatTypePrivate}}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": reason})
			return
		}
	case "getFile":
		file, known := h.file(r.Form.Get("file_id"))
		if !known {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: file is too big"})
			return
		}
		result = FileReference{FileID: r.Form.Get("file_id"), FilePath: file.path, FileSize: int64(len(file.data))}
	case "sendChatAction":
		if r.Form.Get("chat_id") == "" || r.Form.Get("action") != "typing" {
			h.t.Errorf("sendChatAction needs a chat and the typing action: %q %q", r.Form.Get("chat_id"), r.Form.Get("action"))
			return
		}
		h.mu.Lock()
		h.chatActions = append(h.chatActions, time.Now())
		status := h.actionStatus
		h.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": "Too Many Requests: retry after 5"})
			return
		}
		result = true
	case "sendMessageDraft":
		chatID, chatErr := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		draftID, draftErr := strconv.ParseInt(r.Form.Get("draft_id"), 10, 64)
		if chatErr != nil || draftErr != nil || draftID == 0 {
			h.t.Errorf("sendMessageDraft needs a chat and a non-zero draft: %q %q", r.Form.Get("chat_id"), r.Form.Get("draft_id"))
			return
		}
		h.mu.Lock()
		h.drafts = append(h.drafts, draftRequest{chatID: chatID, draftID: draftID, text: r.Form.Get("text"), canStop: r.Form.Get("can_stop") == "true"})
		status := h.draftStatus
		h.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": "Too Many Requests: retry after 5"})
			return
		}
		result = true
	case "setMessageReaction":
		chatID, chatErr := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		messageID, messageErr := strconv.ParseInt(r.Form.Get("message_id"), 10, 64)
		var reactions []struct {
			Type  string `json:"type"`
			Emoji string `json:"emoji"`
		}
		if chatErr != nil || messageErr != nil || json.Unmarshal([]byte(r.Form.Get("reaction")), &reactions) != nil || len(reactions) > 1 {
			h.t.Errorf("setMessageReaction needs a chat, a message, and at most one reaction: %q %q %q", r.Form.Get("chat_id"), r.Form.Get("message_id"), r.Form.Get("reaction"))
			return
		}
		set := setReaction{chatID: chatID, messageID: messageID}
		if len(reactions) == 1 {
			if reactions[0].Type != "emoji" {
				h.t.Errorf("setMessageReaction reaction type = %q", reactions[0].Type)
			}
			set.emoji = reactions[0].Emoji
		}
		h.mu.Lock()
		h.reactions = append(h.reactions, set)
		h.mu.Unlock()
		result = true
	case "deleteMessage":
		chatID, err := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		messageID, err := strconv.ParseInt(r.Form.Get("message_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		h.mu.Lock()
		status := h.deleteStatus
		h.mu.Unlock()
		select {
		case h.deletes <- deletedMessage{chatID: chatID, messageID: messageID}:
		case <-r.Context().Done():
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": "Bad Request: message can't be deleted"})
			return
		}
		result = true
	case "answerCallbackQuery":
		if r.Form.Get("callback_query_id") == "" {
			h.t.Error("answerCallbackQuery without a callback ID")
		}
		select {
		case h.answers <- callbackAnswer{id: r.Form.Get("callback_query_id"), text: r.Form.Get("text")}:
		case <-r.Context().Done():
			return
		}
		result = true
	case "editMessageText":
		chatID, err := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		messageID, err := strconv.ParseInt(r.Form.Get("message_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		h.mu.Lock()
		status, reason := h.editStatus, h.editReason
		h.sent = append(h.sent, r.Form.Get("text"))
		h.mu.Unlock()
		select {
		case h.edits <- editedMessage{chatID: chatID, messageID: messageID, text: r.Form.Get("text"), keyboard: h.parseKeyboard(r.Form.Get("reply_markup"))}:
		case <-r.Context().Done():
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": reason})
			return
		}
		result = true
	case "setMyCommands":
		registration := commandRegistration{scope: r.Form["scope"]}
		if err := json.Unmarshal([]byte(r.Form.Get("commands")), &registration.commands); err != nil {
			h.t.Error(err)
			return
		}
		h.mu.Lock()
		status := h.commandStatus
		h.mu.Unlock()
		select {
		case h.registrations <- registration:
		case <-r.Context().Done():
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status})
			return
		}
		result = true
	default:
		h.t.Errorf("unexpected Telegram method: %s", r.URL.Path)
		w.WriteHeader(404)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result}); err != nil && r.Context().Err() == nil {
		h.t.Error(err)
	}
}

// serveDocumentUpload answers an upload, whose multipart body ParseForm does not read.
func (h *conversationHarness) serveDocumentUpload(w http.ResponseWriter, r *http.Request) {
	chatID, err := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	if err != nil {
		h.t.Error(err)
		return
	}
	file, header, err := r.FormFile("document")
	if err != nil {
		h.t.Error(err)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		h.t.Error(err)
		return
	}
	h.mu.Lock()
	h.sentCount++
	messageID := sentMessageID(h.sentCount)
	h.mu.Unlock()
	select {
	case h.documents <- sentDocument{chatID: chatID, name: header.Filename, data: data}:
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "application/json")
	result := Message{MessageID: messageID, Chat: Chat{ID: chatID, Type: ChatTypePrivate}}
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result}); err != nil && r.Context().Err() == nil {
		h.t.Error(err)
	}
}

// serveFileDownload answers the bot file route, which returns bytes rather than a JSON envelope.
func (h *conversationHarness) serveFileDownload(w http.ResponseWriter, route string) {
	_, filePath, _ := strings.Cut(route, "/")
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, file := range h.files {
		if file.path != filePath {
			continue
		}
		if file.status != 0 {
			w.WriteHeader(file.status)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if _, err := w.Write(file.data); err != nil {
			h.t.Error(err)
		}
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (h *conversationHarness) file(fileID string) (testFile, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	file, known := h.files[fileID]
	return file, known
}

// addFile registers what the simulator will serve for one file identifier.
func (h *conversationHarness) addFile(fileID, filePath string, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files[fileID] = testFile{path: filePath, data: data}
}

func (h *conversationHarness) parseKeyboard(raw string) InlineKeyboard {
	h.t.Helper()
	if raw == "" {
		return nil
	}
	var markup struct {
		InlineKeyboard InlineKeyboard `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(raw), &markup); err != nil || len(markup.InlineKeyboard) == 0 {
		h.t.Errorf("reply_markup %q must be an inline keyboard: %v", raw, err)
	}
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if button.Text == "" || button.CallbackData == "" || len(button.CallbackData) > 64 {
				h.t.Errorf("invalid inline button %+v", button)
			}
		}
	}
	return markup.InlineKeyboard
}

func (h *conversationHarness) start() {
	h.t.Helper()
	if err := h.connections.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if err := h.approvals.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if err := h.conversation.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if h.scheduler != nil {
		if err := h.scheduler.Start(h.ctx); err != nil {
			h.t.Fatal(err)
		}
	}
	if h.tasks != nil {
		if err := h.tasks.Start(h.ctx); err != nil {
			h.t.Fatal(err)
		}
	}
	if h.notion != nil {
		if err := h.notion.Start(h.ctx); err != nil {
			h.t.Fatal(err)
		}
	}
	if h.google != nil {
		if err := h.google.Start(h.ctx); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.service.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *conversationHarness) stop() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.service.Shutdown(ctx); err != nil {
		h.t.Error(err)
	}
	if h.scheduler != nil {
		if err := h.scheduler.Shutdown(ctx); err != nil {
			h.t.Error(err)
		}
	}
	if h.tasks != nil {
		if err := h.tasks.Shutdown(ctx); err != nil {
			h.t.Error(err)
		}
	}
	if err := h.conversation.Shutdown(ctx); err != nil {
		h.t.Error(err)
	}
	if err := h.approvals.Shutdown(ctx); err != nil {
		h.t.Error(err)
	}
	if err := h.connections.Shutdown(ctx); err != nil {
		h.t.Error(err)
	}
	if h.notion != nil {
		if err := h.notion.Shutdown(ctx); err != nil {
			h.t.Error(err)
		}
	}
}

// provision installs a credential file and account row directly, the way a completed ceremony does.
func (h *conversationHarness) provision(principal, accountID string) {
	h.t.Helper()
	directory := filepath.Join(h.profilesDir, principal)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		h.t.Fatal(err)
	}
	record := fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":%q,"refresh":%q,"expires":%d,"accountId":%q}}`,
		codexauth.AccessToken(accountID), codexauth.RefreshToken(accountID), time.Now().Add(time.Hour).UnixMilli(), accountID)
	if err := os.WriteFile(filepath.Join(directory, "auth.json"), []byte(record), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if bound, err := h.store.BindModelConnection(h.ctx, principal, "openai-codex", accountID); err != nil || !bound {
		h.t.Fatalf("bind %s: bound=%v err=%v", principal, bound, err)
	}
}

func (h *conversationHarness) recordIndicatorFailure(method string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.indicatorFailures[method]++
}

func (h *conversationHarness) chatActionTimes() []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.chatActions)
}

func (h *conversationHarness) draftRequests() []draftRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]draftRequest(nil), h.drafts...)
}

func (h *conversationHarness) setReactions() []setReaction {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reactions)
}

func (h *conversationHarness) reactionCount(emoji string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reactionCounts[emoji]
}

func (h *conversationHarness) recordFeedback(counted string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.feedbackCounts[counted]++
}

func (h *conversationHarness) feedbackCount(counted string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.feedbackCounts[counted]
}

// reactionTelemetry counts what the react tool reports on the harness.
type reactionTelemetry struct{ h *conversationHarness }

func (r reactionTelemetry) RecordReaction(_ context.Context, _, emoji string) {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	r.h.reactionCounts[emoji]++
}

func (r reactionTelemetry) RecordReactionConversion(_ context.Context, _, outcome string) {
	r.h.recordFeedback("converted:" + outcome)
}

func (h *conversationHarness) sentTexts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sent...)
}

// requestMessageID differs from the update ID so a reply quoting the wrong identifier is caught.
func requestMessageID(updateID int64) int64 { return 1000 + updateID }

func textUpdate(id, userID int64, text string) Update {
	return Update{UpdateID: id, Message: &Message{MessageID: requestMessageID(id), From: &User{ID: userID, FirstName: "Test"}, Chat: Chat{ID: userID, Type: ChatTypePrivate}, Text: text}}
}

func photoUpdate(id, userID int64, caption, fileID string, size int64) Update {
	update := textUpdate(id, userID, "")
	update.Message.Caption = caption
	update.Message.Photo = []PhotoSize{
		{FileID: fileID + "-small", Width: 90, Height: 60, FileSize: size / 4},
		{FileID: fileID, Width: 1280, Height: 853, FileSize: size},
	}
	return update
}

func documentUpdate(id, userID int64, caption string, document MediaFile) Update {
	update := textUpdate(id, userID, "")
	update.Message.Caption = caption
	update.Message.Document = &document
	return update
}

func voiceUpdate(id, userID int64) Update {
	update := textUpdate(id, userID, "")
	update.Message.Voice = &MediaFile{FileID: "voice-1", MimeType: "audio/ogg", FileSize: 4096}
	return update
}

// settingsMessageID stands for the bot's settings message that a button press refers to.
func settingsMessageID(updateID int64) int64 { return 5000 + updateID }

// sentMessageID is the ID the Telegram simulator assigns to the n-th message the bot sends.
func sentMessageID(n int64) int64 { return 9000 + n }

func callbackID(updateID int64) string { return fmt.Sprintf("cb-%d", updateID) }

func callbackUpdate(id, userID, messageID int64, data string) Update {
	return Update{UpdateID: id, CallbackQuery: &CallbackQuery{
		ID: callbackID(id), From: &User{ID: userID, FirstName: "Test"},
		Message: &Message{MessageID: messageID, Chat: Chat{ID: userID, Type: ChatTypePrivate}}, Data: data,
	}}
}

func (h *conversationHarness) push(updates ...Update) {
	h.t.Helper()
	select {
	case h.updates <- updates:
	case <-h.ctx.Done():
		h.t.Fatal("timed out submitting Telegram updates")
	}
}

func receive[T any](t *testing.T, ctx context.Context, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-ctx.Done():
		t.Fatal("timed out waiting for integration event")
		var zero T
		return zero
	}
}

func (h *conversationHarness) nextMessage() sentMessage { return receive(h.t, h.ctx, h.messages) }

func (h *conversationHarness) nextDocument() sentDocument { return receive(h.t, h.ctx, h.documents) }

// documentCount is the number of uploads the simulator has taken and no test has collected yet.
func (h *conversationHarness) documentCount() int { return len(h.documents) }

func (h *conversationHarness) nextEdit() editedMessage { return receive(h.t, h.ctx, h.edits) }

func (h *conversationHarness) wantAnswer(updateID int64) {
	h.t.Helper()
	if answer := receive(h.t, h.ctx, h.answers); answer.id != callbackID(updateID) {
		h.t.Fatalf("answered callback %+v, want %s", answer, callbackID(updateID))
	}
}

// wantMessage expects an unquoted send, as connection notifications are.
func (h *conversationHarness) wantMessage(chatID int64, text string) {
	h.t.Helper()
	message := h.nextMessage()
	if message.chatID != chatID || message.replyTo != 0 || message.text != text {
		h.t.Fatalf("message = %+v, want chat %d text %q without a quote", message, chatID, text)
	}
}

// wantReply expects a send that answers the update by quoting its message.
func (h *conversationHarness) wantReply(chatID, updateID int64, text string) {
	h.t.Helper()
	message := h.nextMessage()
	if message.chatID != chatID || message.replyTo != requestMessageID(updateID) || message.text != text {
		h.t.Fatalf("message = %+v, want chat %d text %q quoting message %d", message, chatID, text, requestMessageID(updateID))
	}
}

func (h *conversationHarness) waitState(updateID int64, state string) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var actual, message string
		var response sql.NullString
		err := h.db.QueryRowContext(h.ctx, `SELECT state, message, response_text FROM conversation_requests WHERE telegram_update_id = ?`, updateID).Scan(&actual, &message, &response)
		if err == nil && actual == state {
			if (state == "delivered" || state == "cancelled") && (message != "" || response.Valid) {
				h.t.Fatalf("terminal request retained content: message=%q response=%v", message, response)
			}
			return
		}
		if err != nil && err != sql.ErrNoRows {
			h.t.Fatal(err)
		}
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("request %d state = %q, want %q", updateID, actual, state)
		}
	}
}

func (h *conversationHarness) waitOffset(want int64) {
	h.t.Helper()
	for {
		if got := receive(h.t, h.ctx, h.offsets); got == want {
			return
		} else if got > want {
			h.t.Fatalf("poll offset %d passed wanted %d", got, want)
		}
	}
}

func TestConversationSessionsAndUserIsolation(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()
	h.push(textUpdate(1, 101, "remember amber"))
	alice := receive(t, h.ctx, runtime.calls)
	if alice.request.PrincipalID != "alice" || alice.request.Message != "remember amber" || !onlyHelp(alice.request.Tools) {
		t.Fatalf("unexpected runtime request: %+v", alice.request)
	}
	h.push(textUpdate(2, 202, "remember blue"))
	bob := receive(t, h.ctx, runtime.calls)
	if bob.request.PrincipalID != "bob" || bob.request.SessionID == alice.request.SessionID {
		t.Fatalf("principal sessions not isolated: %+v / %+v", alice.request, bob.request)
	}
	bob.answer <- agentruntime.RunResult{Response: "blue saved"}
	h.wantReply(202, 2, "blue saved")
	h.waitState(2, "delivered")
	alice.answer <- agentruntime.RunResult{Response: "amber saved"}
	h.wantReply(101, 1, "amber saved")
	h.waitState(1, "delivered")
	h.push(textUpdate(3, 101, "recall"))
	recall := receive(t, h.ctx, runtime.calls)
	if recall.request.SessionID != alice.request.SessionID {
		t.Fatal("conversation did not reuse Alice's session")
	}
	recall.answer <- agentruntime.RunResult{Response: "amber"}
	h.wantReply(101, 3, "amber")
	h.waitState(3, "delivered")
	h.push(textUpdate(4, 101, "/new"))
	h.wantReply(101, 4, NewSessionReply)
	h.push(textUpdate(5, 101, "recall"), textUpdate(6, 202, "recall"))
	for range 2 {
		call := receive(t, h.ctx, runtime.calls)
		if call.request.PrincipalID == "alice" && call.request.SessionID == alice.request.SessionID {
			t.Fatal("/new retained Alice's session")
		}
		if call.request.PrincipalID == "bob" && call.request.SessionID != bob.request.SessionID {
			t.Fatal("Alice's /new reset Bob's session")
		}
		call.answer <- agentruntime.RunResult{Response: "done"}
	}
	h.nextMessage()
	h.nextMessage()
	h.waitState(5, "delivered")
	h.waitState(6, "delivered")
}

func TestCommandsRemainResponsiveAndCancelSuppressesLateAnswer(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()
	h.push(textUpdate(1, 101, "long request"))
	call := receive(t, h.ctx, runtime.calls)
	h.push(textUpdate(2, 101, "second request"), textUpdate(3, 101, "/status"), textUpdate(4, 101, "/privacy"), textUpdate(5, 101, "/cancel"))
	h.wantReply(101, 2, conversation.BusyReply)
	h.wantReply(101, 3, ConversationStatusReply)
	h.wantReply(101, 4, ConversationPrivacyReply+privacyItem+AttachmentPrivacyLine)
	h.wantReply(101, 5, CancelledReply)
	receive(t, h.ctx, call.ctx.Done())
	h.waitState(1, "cancelled")
	h.push(textUpdate(6, 101, "next request"))
	next := receive(t, h.ctx, runtime.calls)
	if next.request.Message != "next request" {
		t.Fatalf("busy request reached runtime: %+v", next.request)
	}
	next.answer <- agentruntime.RunResult{Response: "next answer"}
	h.wantReply(101, 6, "next answer")
	h.waitState(6, "delivered")
	h.stop()
	select {
	case message := <-h.messages:
		t.Fatalf("unexpected late reply: %+v", message)
	default:
	}
}

func TestUnauthorizedAndNonPrivateUpdatesAreIgnoredAndCursorSurvivesRestart(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()
	group := textUpdate(2, 101, "group request")
	group.Message.Chat.Type = "group"
	group.Message.Chat.ID = -500
	unquotable := textUpdate(5, 101, "no message id")
	unquotable.Message.MessageID = 0
	h.push(textUpdate(1, 303, "unauthorized"), group, Update{UpdateID: 3}, textUpdate(4, 101, ""), unquotable, textUpdate(6, 101, "accepted"), textUpdate(6, 101, "accepted"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.Message != "accepted" {
		t.Fatalf("ignored input reached runtime: %+v", call.request)
	}
	call.answer <- agentruntime.RunResult{Response: "one answer"}
	h.wantReply(101, 6, "one answer")
	h.waitState(6, "delivered")
	h.waitOffset(7)
	if logs := h.logs.String(); !strings.Contains(logs, "reason=malformed_message") {
		t.Fatalf("message without an ID was not ignored as malformed: %s", logs)
	}
	h.stop()
	h.start()
	h.waitOffset(7)
	h.push(textUpdate(6, 101, "accepted"), textUpdate(7, 101, "/status"))
	h.wantReply(101, 7, ConversationStatusReply)
	h.waitOffset(8)
	h.stop()
	select {
	case call := <-runtime.calls:
		t.Fatalf("duplicate/ignored update ran model: %+v", call.request)
	default:
	}
	var count int
	if err := h.db.QueryRow(`SELECT count(*) FROM conversation_requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persisted %d requests, want 1", count)
	}
}

func TestConversationRestartRecovery(t *testing.T) {
	for _, state := range []string{"queued", "running", "ready", "delivering"} {
		t.Run(state, func(t *testing.T) {
			runtime := newControlledRuntime()
			h := newConversationHarness(t, runtime)
			h.provision("alice", "acct-alice")
			input := conversation.EnqueueInput{BotUserID: 999, TelegramUpdateID: 1, TelegramMessageID: requestMessageID(1), PrincipalID: "alice", ChatID: 101, Message: "original request"}
			_, err := h.store.EnqueueConversation(h.ctx, input, conversation.Session{ID: recoverySessionID, PrincipalID: "alice", RuntimeSessionID: recoveryRuntimeSessionID}, recoveryRequestID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if state != "queued" {
				if ok, err := h.store.ClaimConversationRequest(h.ctx, recoveryRequestID); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
			}
			if state == "ready" || state == "delivering" {
				if ok, err := h.store.SaveConversationResponse(h.ctx, recoveryRequestID, "saved answer", ""); err != nil || !ok {
					t.Fatalf("save: %v %v", ok, err)
				}
			}
			if state == "delivering" {
				if ok, err := h.store.ClaimConversationDelivery(h.ctx, recoveryRequestID); err != nil || !ok {
					t.Fatalf("delivery: %v %v", ok, err)
				}
			}
			h.start()
			want := "saved answer"
			if state == "queued" {
				call := receive(t, h.ctx, runtime.calls)
				if call.request.Message != input.Message {
					t.Fatal("queued request changed")
				}
				call.answer <- agentruntime.RunResult{Response: "queued answer"}
				want = "queued answer"
			}
			if state == "running" {
				want = conversation.InterruptedReply
			}
			h.wantReply(101, 1, want)
			h.waitState(1, "delivered")
			h.push(textUpdate(2, 101, "follow up"))
			call := receive(t, h.ctx, runtime.calls)
			if call.request.Message != "follow up" {
				t.Fatalf("replayed model work after restart: %+v", call.request)
			}
			if call.request.SessionID != recoveryRuntimeSessionID {
				t.Fatalf("recovery after %s continued in session %q, want %q", state, call.request.SessionID, recoveryRuntimeSessionID)
			}
			call.answer <- agentruntime.RunResult{Response: "follow up answer"}
			h.wantReply(101, 2, "follow up answer")
			h.waitState(2, "delivered")
		})
	}
}

func TestConversationDeliveryFailuresAndUnicodeChunks(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			runtime := newControlledRuntime()
			h := newConversationHarness(t, runtime)
			h.provision("alice", "acct-alice")
			h.sendStatuses = []int{status}
			h.start()
			h.push(textUpdate(1, 101, "answer"))
			call := receive(t, h.ctx, runtime.calls)
			call.answer <- agentruntime.RunResult{Response: "model answer"}
			if first := h.nextMessage(); first.status != status || first.replyTo != requestMessageID(1) || first.text != "model answer" {
				t.Fatalf("first attempt = %+v", first)
			}
			if status == http.StatusForbidden {
				h.waitState(1, "cancelled")
			} else {
				h.wantReply(101, 1, "model answer")
				h.waitState(1, "delivered")
			}
			h.push(textUpdate(2, 101, "next"))
			call = receive(t, h.ctx, runtime.calls)
			if call.request.Message != "next" {
				t.Fatal("delivery retried the model request")
			}
			answer := strings.Repeat("😀", 8001)
			call.answer <- agentruntime.RunResult{Response: answer}
			var joined strings.Builder
			for index := range 3 {
				chunk := h.nextMessage()
				if chunk.chatID != 101 || !utf8.ValidString(chunk.text) || utf8.RuneCountInString(chunk.text) > 4000 {
					t.Fatalf("invalid Telegram chunk: chat=%d runes=%d", chunk.chatID, utf8.RuneCountInString(chunk.text))
				}
				wantQuote := int64(0)
				if index == 0 {
					wantQuote = requestMessageID(2)
				}
				if chunk.replyTo != wantQuote {
					t.Fatalf("chunk %d quotes message %d, want %d", index, chunk.replyTo, wantQuote)
				}
				joined.WriteString(chunk.text)
			}
			if joined.String() != answer {
				t.Fatal("chunking changed reply content")
			}
			h.waitState(2, "delivered")
		})
	}
}

func TestOutgoingMessagesUseHTMLAndFallBackToPlainText(t *testing.T) {
	const answer = "**Done** on [the page](https://notion.so/p?a=1&b=2): `data_source_id` a & b <now>"
	const formatted = `<b>Done</b> on <a href="https://notion.so/p?a=1&amp;b=2">the page</a>: <code>data_source_id</code> a &amp; b &lt;now&gt;`
	t.Run("rendered", func(t *testing.T) {
		runtime := newControlledRuntime()
		h := newConversationHarness(t, runtime)
		h.provision("alice", "acct-alice")
		h.start()
		h.push(textUpdate(1, 101, "answer"))
		call := receive(t, h.ctx, runtime.calls)
		call.answer <- agentruntime.RunResult{Response: answer}
		message := h.nextMessage()
		if message.parseMode != "HTML" || message.text != formatted {
			t.Fatalf("message = %+v, want %q with parse mode HTML", message, formatted)
		}
		h.waitState(1, "delivered")
	})
	t.Run("refused entities", func(t *testing.T) {
		runtime := newControlledRuntime()
		h := newConversationHarness(t, runtime)
		h.provision("alice", "acct-alice")
		h.sendStatuses = []int{http.StatusBadRequest}
		h.sendReason = "Bad Request: can't parse entities: Unsupported start tag"
		h.start()
		h.push(textUpdate(1, 101, "answer"))
		call := receive(t, h.ctx, runtime.calls)
		call.answer <- agentruntime.RunResult{Response: answer}
		if refused := h.nextMessage(); refused.parseMode != "HTML" || refused.text != formatted {
			t.Fatalf("first attempt = %+v, want the rendered message", refused)
		}
		retry := h.nextMessage()
		if retry.parseMode != "" || retry.text != answer || retry.replyTo != requestMessageID(1) {
			t.Fatalf("retry = %+v, want the plain text quoting the request", retry)
		}
		h.waitState(1, "delivered")
		if sent := h.sentTexts(); len(sent) != 2 {
			t.Fatalf("sent %q, want exactly one plain-text retry", sent)
		}
	})
}

func TestBusyWhileResponseIsBeingDelivered(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	gate := make(chan struct{})
	h.sendGate = gate
	h.provision("alice", "acct-alice")
	h.start()
	h.push(textUpdate(1, 101, "first"))
	call := receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "first answer"}
	h.wantReply(101, 1, "first answer")
	h.waitState(1, "delivering")
	h.push(textUpdate(2, 101, "second"))
	message := h.nextMessage()
	if message.text != conversation.BusyReply || message.replyTo != requestMessageID(2) {
		t.Fatalf("during delivery got %+v, want busy reply quoting the second request", message)
	}
	close(gate)
	h.waitState(1, "delivered")
	h.waitOffset(3)
	h.stop()
	select {
	case call := <-runtime.calls:
		t.Fatalf("message during delivery was enqueued: %s", fmt.Sprint(call.request))
	default:
	}
}

func TestBotCommandsRegisteredOnceAtStartup(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	h.start()
	want := []string{"status", "privacy", "cancel", "new", "sessions", "settings"}
	for _, wantScope := range [][]string{nil, {`{"type":"all_private_chats"}`}} {
		registration := receive(t, h.ctx, h.registrations)
		if !slices.Equal(registration.scope, wantScope) {
			t.Fatalf("registration scope = %v, want %v", registration.scope, wantScope)
		}
		if len(registration.commands) != len(want) {
			t.Fatalf("registered %+v, want %v", registration.commands, want)
		}
		for index, command := range registration.commands {
			if command.Command != want[index] || command.Description == "" {
				t.Fatalf("command %d = %+v, want %q with a description", index, command, want[index])
			}
		}
	}
	h.push(textUpdate(1, 101, "/status"), textUpdate(2, 101, "/privacy"))
	h.wantReply(101, 1, ConversationStatusReply)
	h.wantReply(101, 2, ConversationPrivacyReply+privacyItem+AttachmentPrivacyLine)
	h.waitOffset(3)
	select {
	case extra := <-h.registrations:
		t.Fatalf("registered again while polling: %+v", extra)
	default:
	}
}

func TestBotCommandRegistrationFailureKeepsPolling(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	h.commandStatus = http.StatusInternalServerError
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	h.push(textUpdate(1, 101, "/status"))
	h.wantReply(101, 1, ConversationStatusReply)
	logs := h.logs.String()
	if !strings.Contains(logs, "telegram.commands.register_failed") {
		t.Fatalf("registration failure was not logged: %s", logs)
	}
	if strings.Contains(logs, "test-token") {
		t.Fatal("logs contain the Telegram token")
	}
}

package eval

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/connectors/calendar"
	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
	"github.com/vladar107/home-ai-assistant/internal/conversation/history"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	userlocation "github.com/vladar107/home-ai-assistant/internal/location"
	"github.com/vladar107/home-ai-assistant/internal/looseend"
	"github.com/vladar107/home-ai-assistant/internal/memory"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil/calendarsim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
	"github.com/vladar107/home-ai-assistant/internal/weekday"
	"github.com/vladar107/home-ai-assistant/internal/wiring"
)

const (
	// evalTelegramUserID is the synthetic Telegram identity of that principal, and so its private
	// chat's ID. It is configuration of the world, not a household value.
	evalTelegramUserID int64 = 900001
	botUserID          int64 = 900000

	notionToken = "ntn_evalworldtoken"
)

// FixedClock is the one clock of a world: it stays where it is until the harness advances it.
type FixedClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFixedClock starts a clock at the instant.
func NewFixedClock(at time.Time) *FixedClock { return &FixedClock{now: at} }

func (c *FixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *FixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// WorldOptions is what a world needs from its caller.
type WorldOptions struct {
	// Runtime runs the model: the Pi runtime in a real run, the Pi fixture in tests.
	Runtime agentruntime.AgentRuntime
	// SessionFiles locates the runtime's session files; nil asks the runtime.
	SessionFiles history.Directories
	// ProfilesDir is the root of the model profiles; the eval principal's is <root>/eval.
	ProfilesDir string
	// AccountID is the model account the eval principal's connection is bound to.
	AccountID string
	// Provider is the model provider of that binding; empty means connection.DefaultProvider.
	Provider string
	// AuthBaseURL and ChatGPT replace the model provider's endpoints; empty keeps production's.
	AuthBaseURL string
	ChatGPT     chatgpt.Config
	// TypeSafe reaches the safety checks and the tool hint; without an APIKey they stay off.
	TypeSafe judgment.Config
	// ChatModel, TaskModel and HeadsUpModel are the models turns run on; the case's user model
	// and effort replace ChatModel's. A zero TaskModel and HeadsUpModel follow ChatModel.
	ChatModel, TaskModel, HeadsUpModel agentruntime.ModelEffort
	// TurnTimeout and ScheduledTurnTimeout bound a chat turn and a scheduled turn; zero means
	// production's defaults, two and five minutes.
	TurnTimeout, ScheduledTurnTimeout time.Duration
	// StepTimeout bounds one step; zero means five minutes.
	StepTimeout time.Duration
	// Dir holds the temporary store and files; empty makes a temporary directory the world removes.
	Dir    string
	Logger *slog.Logger
	// Variant is the prompt variant the world's turns run under; nil runs production's prompt.
	Variant *Variant
	// Configure may change the wiring inputs before the build, to plug in the fake of a connector
	// the world does not serve itself.
	Configure func(*wiring.Inputs) error
}

// World is the production wiring for the eval principal over a temporary store, one clock and the
// fakes the case seeds. Steps are played one at a time.
type World struct {
	Case      Case
	Assembly  *wiring.Assembly
	Store     *sqlite.Store
	Chat      *Chat
	Recorder  *Recorder
	Clock     *FixedClock
	Location  *time.Location
	Dir       string
	Google    *googlesim.Simulator
	Calendar  *calendarsim.Simulator
	Notion    *notionsim.Simulator
	chatID    int64
	db        *sql.DB
	lifecycle wiring.Lifecycle
	options   WorldOptions
	ownsDir   bool
	reporter  *reporter
	jobIDs    map[string]string
	closers   []func()
	// defaultSwitches are integrations the world's own fakes switch on for the principal, below
	// the case's user.switches.
	defaultSwitches []string
	// assistantMail serves the assistant's own mailbox, and assistantMailSeed holds the mail the next
	// triage_mail step delivers.
	assistantMail     *googlesim.Simulator
	assistantMailSeed assistantMailSection

	mu         sync.Mutex
	counter    int64
	pending    []Offer
	lastReply  int64
	scheduling bool
}

// NewWorld builds the world for the case and starts the model connection, the conversation
// service and approvals. The scheduler's steps and every other background service stay stopped.
func NewWorld(ctx context.Context, c Case, options WorldOptions) (world *World, err error) {
	switch {
	case options.Runtime == nil:
		return nil, errors.New("eval world: a model runtime is required")
	case options.ProfilesDir == "":
		return nil, errors.New("eval world: a profiles directory is required")
	case options.AccountID == "":
		return nil, errors.New("eval world: a model account is required")
	}
	start, err := c.Clock.Time()
	if err != nil {
		return nil, err
	}
	w := &World{
		Case: c, options: options, Clock: NewFixedClock(start), Location: start.Location(), Chat: newChat(),
		chatID: evalTelegramUserID, reporter: &reporter{}, jobIDs: map[string]string{},
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, w.Close())
		}
	}()
	if options.Dir == "" {
		if w.Dir, err = os.MkdirTemp("", "eval-world-*"); err != nil {
			return nil, err
		}
		w.ownsDir = true
	} else {
		w.Dir = options.Dir
	}
	if w.Store, err = sqlite.Open(ctx, filepath.Join(w.Dir, "state", "assistant.sqlite")); err != nil {
		return nil, fmt.Errorf("open the world's store: %w", err)
	}
	if w.db, err = sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(w.Dir, "state", "assistant.sqlite"))+"?_busy_timeout=5000"); err != nil {
		return nil, err
	}
	principals := []identity.ConfiguredPrincipal{{ID: PrincipalID, DisplayName: c.User.Name, TelegramUserID: evalTelegramUserID}}
	// The identity store wants a name; a case without one leaves the turn's user line out.
	named := []identity.ConfiguredPrincipal{{ID: PrincipalID, DisplayName: cmp.Or(c.User.Name, "Eval"), TelegramUserID: evalTelegramUserID}}
	identities, err := identity.NewService(w.Store, named)
	if err != nil {
		return nil, err
	}
	if err := identities.Bootstrap(ctx); err != nil {
		return nil, err
	}
	provider := cmp.Or(options.Provider, connection.DefaultProvider)
	if bound, err := w.Store.BindModelConnection(ctx, PrincipalID, provider, options.AccountID); err != nil || !bound {
		return nil, fmt.Errorf("bind the eval principal's model account: bound=%v: %w", bound, err)
	}
	// The recorder wraps the placeholder layer, so a case's checks compare real values.
	w.Recorder = NewRecorder(wiring.PlaceholderRuntime(w.Store, options.Runtime, true, options.Logger))
	w.Recorder.sections = options.Variant.promptSections()
	inputs, err := w.inputs(principals, provider)
	if err != nil {
		return nil, err
	}
	if options.Configure != nil {
		if err := options.Configure(&inputs); err != nil {
			return nil, err
		}
	}
	if err := w.guard(func() error { return w.seedFakes(&inputs) }); err != nil {
		return nil, err
	}
	if w.Assembly, err = wiring.Build(ctx, inputs); err != nil {
		return nil, fmt.Errorf("build the world's wiring: %w", err)
	}
	if overrides := options.Variant.guidance(); len(overrides) > 0 {
		w.Assembly.Tools.SetGuidanceOverrides(overrides)
	}
	if err := w.guard(func() error { return w.seedStore(ctx) }); err != nil {
		return nil, err
	}
	if err := w.lifecycle.Start(ctx, w.chatSteps()...); err != nil {
		return nil, err
	}
	return w, nil
}

// chatSteps are the services a chat turn needs: the rest of the assembly's steps, the scheduler
// among them, stay stopped.
func (w *World) chatSteps() []wiring.Step {
	wanted := []string{"model connections", "approvals", "conversation service", "reply provenance", "turn traces"}
	var steps []wiring.Step
	for _, step := range w.Assembly.Steps() {
		if slices.Contains(wanted, step.Name) {
			steps = append(steps, step)
		}
	}
	// The MCP connector lists its servers' tools only once it runs.
	if w.Assembly.MCP != nil {
		steps = append(steps, wiring.Step{Name: "mcp connector", Start: w.Assembly.MCP.Start, Shutdown: w.Assembly.MCP.Shutdown})
	}
	return steps
}

func (w *World) inputs(principals []identity.ConfiguredPrincipal, provider string) (wiring.Inputs, error) {
	o := w.options
	logger := cmp.Or(o.Logger, slog.New(slog.NewTextHandler(io.Discard, nil)))
	chatModel := o.ChatModel
	if w.Case.User.Model != "" {
		chatModel.Model = w.Case.User.Model
	}
	if w.Case.User.Effort != "" {
		chatModel.Effort = w.Case.User.Effort
	}
	chatModel.Effort = cmp.Or(chatModel.Effort, agentruntime.DefaultEffort)
	taskModel := cmp.Or(o.TaskModel, chatModel)
	if taskModel.Effort == "" {
		taskModel.Effort = agentruntime.DefaultEffort
	}
	headsUp := cmp.Or(o.HeadsUpModel, taskModel)
	var sessionFiles history.Directories = o.SessionFiles
	if sessionFiles == nil {
		sessionFiles, _ = o.Runtime.(history.Directories)
	}
	return wiring.Inputs{
		Store: w.Store, Logger: logger, Telemetry: observability.Noop(),
		Runtime: w.Recorder, RuntimeHasPlaceholders: true, SessionFiles: sessionFiles,
		Chat: w.Chat, Reminders: w.Chat, Alerts: w.Chat, TaskMessages: w.Chat, Tabs: w.Chat.tabs,
		LooseEnds:  func(*looseend.Service) looseend.Nudger { return looseEndNudger{chat: w.Chat, location: w.Location} },
		Principals: principals, DataDir: w.Dir, Zone: w.Case.Clock.Zone,
		Core: wiring.CoreConfig{
			TurnTimeout: cmp.Or(o.TurnTimeout, 2*time.Minute), ScheduledTurnTimeout: cmp.Or(o.ScheduledTurnTimeout, 5*time.Minute),
			TurnZone: w.Location, Now: w.Clock.Now, Watches: true,
			ChatModel: chatModel, TaskModel: taskModel, HeadsUpModel: headsUp,
			ChatGPT: o.ChatGPT,
			Connection: connection.Config{
				Provider: provider, ProfilesDir: o.ProfilesDir, AuthBaseURL: o.AuthBaseURL, Principals: []string{PrincipalID},
			},
			TypeSafe: o.TypeSafe,
		},
	}, nil
}

// reporter is the part of a test the fakes use, for a harness that is not a test: a fake's failure
// to serve is kept and reported when the world closes.
type reporter struct {
	mu       sync.Mutex
	errs     []error
	cleanups []func()
}

type fatal struct{ error }

func (r *reporter) Helper() {}

func (r *reporter) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

func (r *reporter) Fatalf(format string, args ...any) { panic(fatal{fmt.Errorf(format, args...)}) }

func (r *reporter) Cleanup(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, fn)
}

// guard runs a seeding step that may stop on a fake's Fatalf.
func (w *World) guard(step func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			failure, ok := recovered.(fatal)
			if !ok {
				panic(recovered)
			}
			err = failure.error
		}
	}()
	return step()
}

type googleAccount struct{ mail, calendar bool }

func (w *World) section(name string, into any) (bool, error) {
	raw, ok := w.Case.World[name]
	if !ok {
		return false, nil
	}
	return true, decodeStrict(raw, into)
}

func (w *World) optionalLocal(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return w.parseLocal(value)
}

func (w *World) parseLocal(value string) (time.Time, error) {
	return time.ParseInLocation(ClockLayout, value, w.Location)
}

// seedFakes starts the loopback fakes the case's sections need and points the inputs at them.
func (w *World) seedFakes(in *wiring.Inputs) error {
	var mail gmailSection
	hasMail, err := w.section(SectionGmail, &mail)
	if err != nil {
		return err
	}
	var cal calendarSection
	hasCalendar, err := w.section(SectionCalendar, &cal)
	if err != nil {
		return err
	}
	if hasMail || hasCalendar {
		google := googlesim.New(w.reporter)
		google.SetClock(w.Clock.Now)
		w.Google = google
		in.Google = &wiring.GoogleConfig{
			Accounts: googleConfig(google),
			Mail:     gmailConfig(google),
			Calendar: calendarConfig(w.Location),
		}
		if hasMail {
			messages := make([]googlesim.Message, 0, len(mail.Messages))
			for _, m := range mail.Messages {
				received, err := w.parseLocal(m.Received)
				if err != nil {
					return err
				}
				labels := m.Labels
				if len(labels) == 0 {
					labels = []string{"INBOX"}
				}
				messages = append(messages, googlesim.Message{
					ID: m.ID, ThreadID: cmp.Or(m.ThreadID, m.ID), From: m.From, To: cmp.Or(m.To, cmp.Or(mail.Account, DefaultAccount)),
					Subject: m.Subject, Snippet: m.Snippet, Labels: labels, Received: received,
					PlainText: m.Body, HTML: m.HTML, Attachments: m.Attachments,
				})
			}
			google.AddMailbox(cmp.Or(mail.Account, DefaultAccount), messages...)
		}
		if hasCalendar {
			account := cmp.Or(cal.Account, DefaultAccount)
			sim := calendarsim.New(w.reporter, google.AccountFor)
			w.Calendar = sim
			in.Google.Calendar.APIBaseURL, in.Google.Calendar.HTTPClient = sim.APIBaseURL(), sim.Client()
			sim.AddCalendar(account, calendarsim.Calendar{ID: account, Summary: "Calendar", Primary: true, AccessRole: "owner"})
			events := make([]calendarsim.Event, 0, len(cal.Events))
			for _, e := range cal.Events {
				event, err := w.calendarEvent(e)
				if err != nil {
					return err
				}
				events = append(events, event)
			}
			sim.AddEvents(account, account, events...)
		}
	}
	var pages notionSection
	hasNotion, err := w.section(SectionNotion, &pages)
	if err != nil {
		return err
	}
	if hasNotion {
		sim := notionsim.New(w.reporter)
		w.Notion = sim
		sim.AddToken(notionToken, notionsim.Token{Kind: "internal", BotName: "Eval", WorkspaceID: "ws-eval", WorkspaceName: "Eval workspace"})
		edited := w.Clock.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		for _, p := range pages.Pages {
			blocks := make([]map[string]any, 0, len(p.Lines))
			for _, line := range p.Lines {
				if text, ok := cutPrefix(line, "- "); ok {
					blocks = append(blocks, notionsim.Bullet(text))
				} else {
					blocks = append(blocks, notionsim.Paragraph(line))
				}
			}
			sim.SharePage(notionToken, notionsim.Page{
				ID: p.ID, Title: p.Title, URL: cmp.Or(p.URL, "https://notion.example.test/"+p.ID), LastEdited: edited, Blocks: blocks,
			})
		}
		seedNotionTables(sim, pages.DataSources, edited)
		in.Notion = &notion.Config{APIBaseURL: sim.URL(), HTTPClient: sim.Client(), Now: w.Clock.Now}
	}
	if err := w.seedPlaces(in); err != nil {
		return err
	}
	if err := w.seedAssistantMail(in); err != nil {
		return err
	}
	return w.seedConnectorFakes(in)
}

func (w *World) calendarEvent(e calendarEvent) (calendarsim.Event, error) {
	start, err := w.parseLocal(e.Start)
	if err != nil {
		return calendarsim.Event{}, err
	}
	end, err := w.parseLocal(e.End)
	if err != nil {
		return calendarsim.Event{}, err
	}
	event := calendarsim.Event{
		ID: e.ID, Summary: e.Summary, Description: e.Description, Location: e.Location,
		Start: start, End: end, AllDay: e.AllDay, Organizer: e.Organizer,
	}
	for _, a := range e.Attendees {
		event.Attendees = append(event.Attendees, calendarsim.Attendee{Email: a.Email, DisplayName: a.Name, Response: cmp.Or(a.Response, "accepted")})
	}
	return event, nil
}

// seedStore applies the user and the case's sections that live in the store or in a connector's
// own account, through the services where there is one.
func (w *World) seedStore(ctx context.Context) error {
	now := w.Clock.Now()
	if w.Case.User.Language != "" {
		language, ok := locale.Parse(w.Case.User.Language)
		if !ok {
			return fmt.Errorf("user.language %q is not a language the assistant speaks", w.Case.User.Language)
		}
		if err := w.Store.SaveLanguage(ctx, PrincipalID, language); err != nil {
			return err
		}
	}
	switches := map[string]bool{}
	for _, name := range w.defaultSwitches {
		switches[name] = true
	}
	if w.options.TypeSafe.APIKey != "" {
		switches[judgment.OptInSwitch.Integration] = true
	}
	for name, enabled := range w.Case.User.Switches {
		switches[name] = enabled
	}
	for name, enabled := range switches {
		if err := w.Assembly.Switches.SetEnabled(ctx, PrincipalID, integration.Switch{Integration: name, Name: name}, enabled); err != nil {
			return fmt.Errorf("user.switches.%s: %w", name, err)
		}
	}
	for i, line := range w.Case.User.Memory {
		if err := validateMemoryText(memory.KindFact, line); err != nil {
			return fmt.Errorf("user.memory[%d]: %w", i, err)
		}
		if err := w.addMemory(ctx, memory.KindFact, line, now, "user", fmt.Sprint(i)); err != nil {
			return err
		}
	}
	var entries []memorySeed
	if _, err := w.section(SectionMemory, &entries); err != nil {
		return err
	}
	for i, entry := range entries {
		if err := w.addMemory(ctx, cmp.Or(entry.Kind, memory.KindFact), entry.Text, now, "world", fmt.Sprint(i)); err != nil {
			return err
		}
	}
	var items []backlogSeed
	if _, err := w.section(SectionBacklog, &items); err != nil {
		return err
	}
	for i, item := range items {
		err := w.Store.AddBacklogItem(ctx, backlog.Item{
			ID: seedID("backlog", fmt.Sprint(i), item.Title), PrincipalID: PrincipalID, Title: item.Title, Details: item.Details,
			Status: backlog.StatusOpen, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("backlog[%d]: %w", i, err)
		}
	}
	var specs []routes.Spec
	if _, err := w.section(SectionRoutes, &specs); err != nil {
		return err
	}
	for i, spec := range specs {
		spec.Days, _ = weekday.Normalize(spec.Days)
		created, err := w.Store.CreateRoute(ctx, routes.Route{
			ID: longID("route", spec.Name), PrincipalID: PrincipalID, Spec: spec, CreatedAt: now, UpdatedAt: now,
		}, routes.MaximumRoutesPerPrincipal)
		if err != nil || !created {
			return fmt.Errorf("routes[%d]: created=%v: %w", i, created, err)
		}
	}
	var places []placeSeed
	if _, err := w.section(SectionPlaces, &places); err != nil {
		return err
	}
	for i, place := range places {
		created, err := w.Store.CreatePlace(ctx, userlocation.Place{
			ID: longID("place", place.Name), PrincipalID: PrincipalID, Name: place.Name, Latitude: place.Lat, Longitude: place.Lon,
			RadiusM: cmp.Or(place.RadiusM, 100), CreatedAt: now, UpdatedAt: now,
		}, userlocation.MaximumPlacesPerPrincipal)
		if err != nil || !created {
			return fmt.Errorf("places[%d]: created=%v: %w", i, created, err)
		}
	}
	var parcels []parcelSeed
	if _, err := w.section(SectionParcels, &parcels); err != nil {
		return err
	}
	for i, seed := range parcels {
		held := seed.parcel()
		held.PrincipalID, held.CreatedAt, held.UpdatedAt = PrincipalID, now, now
		var err error
		if held.WindowFrom, err = w.optionalLocal(seed.WindowFrom); err != nil {
			return fmt.Errorf("parcels[%d]: %w", i, err)
		}
		if held.WindowTo, err = w.optionalLocal(seed.WindowTo); err != nil {
			return fmt.Errorf("parcels[%d]: %w", i, err)
		}
		if carrier, ok := parcel.DefaultCarriers().ByID(held.Carrier); ok {
			held.Link = carrier.Link(held.Number)
		}
		if _, _, err := w.Store.UpsertParcel(ctx, held); err != nil {
			return fmt.Errorf("parcels[%d]: %w", i, err)
		}
	}
	if err := w.connectAccounts(ctx); err != nil {
		return err
	}
	looseEndsJob, err := w.seedJobs(ctx)
	if err != nil {
		return err
	}
	return w.seedLooseEnds(ctx, looseEndsJob)
}

func (w *World) addMemory(ctx context.Context, kind, text string, now time.Time, origin, index string) error {
	return w.Store.AddMemory(ctx, memory.Entry{
		ID: seedID("memory", origin, index), PrincipalID: PrincipalID, Kind: kind, Text: text, Source: memory.SourceConfirmed,
		CreatedAt: now, UpdatedAt: now,
	})
}

// connectAccounts signs the eval principal in to the fakes through the connectors' own flows.
func (w *World) connectAccounts(ctx context.Context) error {
	var mail gmailSection
	hasMail, _ := w.section(SectionGmail, &mail)
	var cal calendarSection
	hasCalendar, _ := w.section(SectionCalendar, &cal)
	accounts := map[string]*googleAccount{}
	if hasMail {
		accounts[cmp.Or(mail.Account, DefaultAccount)] = &googleAccount{mail: true}
	}
	if hasCalendar {
		account := cmp.Or(cal.Account, DefaultAccount)
		if accounts[account] == nil {
			accounts[account] = &googleAccount{}
		}
		accounts[account].calendar = true
	}
	for _, account := range sortedAccounts(accounts) {
		link, err := w.Assembly.Google.BeginSignIn(PrincipalID)
		if err != nil {
			return err
		}
		redirect := w.Google.Consent(link, account, googlesim.Grant{Mail: accounts[account].mail, Calendar: accounts[account].calendar})
		if _, err := w.Assembly.Google.CompleteSignIn(ctx, PrincipalID, redirect); err != nil {
			return fmt.Errorf("connect %s to the Google fake: %w", account, err)
		}
	}
	if w.Notion != nil {
		if _, err := w.Assembly.Notion.Connect(ctx, PrincipalID, notionToken); err != nil {
			return fmt.Errorf("connect the Notion fake: %w", err)
		}
		var section notionSection
		if _, err := w.section(SectionNotion, &section); err != nil {
			return err
		}
		if section.Writes {
			if _, err := w.Assembly.Notion.SetWrites(ctx, PrincipalID, true); err != nil {
				return fmt.Errorf("grant the Notion fake's writes: %w", err)
			}
		}
	}
	return nil
}

func sortedAccounts(accounts map[string]*googleAccount) []string {
	names := make([]string, 0, len(accounts))
	for name := range accounts {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// seedJobs stores the case's scheduled jobs through the schedule service, so each has the next run
// the case's clock gives it, and reports whether one is the Loose ends job.
func (w *World) seedJobs(ctx context.Context) (looseEnds bool, err error) {
	var jobs []jobSeed
	if _, err := w.section(SectionJobs, &jobs); err != nil {
		return false, err
	}
	scheduler := w.Assembly.Scheduler
	for i, job := range jobs {
		id, name, jobType := longID("job", job.ID), cmp.Or(job.Name, job.ID), schedule.TypeModelTurn
		var config json.RawMessage
		timing := schedule.Timing{Cron: job.Cron, At: job.At, Zone: job.Zone}
		switch {
		case job.HeadsUp:
			id, name = schedule.HeadsUpJobID(PrincipalID), schedule.HeadsUpName
			if timing.Cron == "" && timing.At == "" {
				timing.Cron = schedule.HeadsUpCron
			}
			config, err = schedule.ModelTurnConfigWithModel(cmp.Or(job.Prompt, schedule.HeadsUpNotes), job.Model)
		case job.LooseEnds:
			id, name, jobType, config, looseEnds = schedule.LooseEndsJobID(PrincipalID), schedule.LooseEndsName, schedule.LooseEndScanType, looseend.JobConfigJSON(), true
			if timing.Cron == "" && timing.At == "" {
				timing.Cron = schedule.LooseEndsCron
			}
		case job.Reminder != "":
			jobType = schedule.TypeReminder
			config, err = schedule.ReminderConfig(job.Reminder)
		default:
			config, err = schedule.ModelTurnConfigWithModel(job.Prompt, job.Model)
		}
		if err != nil {
			return false, fmt.Errorf("jobs[%d]: %w", i, err)
		}
		prepared, err := scheduler.Prepare(PrincipalID, name, jobType, timing, config)
		if err != nil {
			return false, fmt.Errorf("jobs[%d]: %w", i, err)
		}
		prepared.Job.Enabled = !job.Disabled
		created, err := scheduler.Create(ctx, id, prepared)
		if err != nil || !created {
			return false, fmt.Errorf("jobs[%d]: created=%v: %w", i, created, err)
		}
		w.jobIDs[job.ID] = id
	}
	return looseEnds, nil
}

// Advance moves the world's clock forward. A job that becomes due runs when a run_job step starts
// the scheduler.
func (w *World) Advance(d time.Duration) { w.Clock.Advance(d) }

// DB is a second handle on the world's store for checks to read.
func (w *World) DB() *sql.DB { return w.db }

// Close stops the services, closes the store and the fakes, and removes a directory the world made.
// It reports what the fakes could not serve.
func (w *World) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var errs []error
	if w.Assembly != nil && w.scheduling {
		errs = append(errs, w.Assembly.Scheduler.Shutdown(ctx))
	}
	errs = append(errs, w.lifecycle.Shutdown(ctx))
	if w.db != nil {
		errs = append(errs, w.db.Close())
	}
	if w.Store != nil {
		errs = append(errs, w.Store.Close())
	}
	for _, closer := range slices.Backward(w.closers) {
		closer()
	}
	w.closers = nil
	w.reporter.mu.Lock()
	cleanups, served := w.reporter.cleanups, w.reporter.errs
	w.reporter.cleanups = nil
	w.reporter.mu.Unlock()
	for _, cleanup := range slices.Backward(cleanups) {
		cleanup()
	}
	errs = append(errs, served...)
	if w.ownsDir && w.Dir != "" {
		errs = append(errs, os.RemoveAll(w.Dir))
	}
	return errors.Join(errs...)
}

func googleConfig(sim *googlesim.Simulator) google.Config {
	return google.Config{
		ClientID: googlesim.ClientID, ClientSecret: googlesim.ClientSecret, RedirectURL: googlesim.RedirectURL,
		AuthURL: sim.AuthURL(), TokenURL: sim.TokenURL(), RevokeURL: sim.RevokeURL(), UserInfoURL: sim.UserInfoURL(), HTTPClient: sim.Client(),
	}
}

func gmailConfig(sim *googlesim.Simulator) gmail.Config {
	return gmail.Config{APIBaseURL: sim.URL(), HTTPClient: sim.Client()}
}

// calendarConfig leaves the calendar API unreachable until a calendar section serves it.
func calendarConfig(location *time.Location) calendar.Config {
	return calendar.Config{Location: location, APIBaseURL: "http://127.0.0.1:1"}
}

func cutPrefix(s, prefix string) (string, bool) { return strings.CutPrefix(s, prefix) }

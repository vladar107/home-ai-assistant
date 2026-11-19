package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/testutil/codexauth"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pifixture"
)

const (
	testAccount  = "account-eval"
	callPrefix   = "fixture:call "
	quietMarker  = "script:quiet"
	clockLocal   = "2026-03-10T08:30"
	clockZone    = "Europe/Berlin"
	testTypeSafe = "test-key"
)

var (
	outsideMu       sync.Mutex
	outsideLoopback []string
)

// refuseOutsideLoopback makes the default transport, and every client cloned from it, fail to dial
// anything but a loopback address, so a test cannot reach a real service by accident.
func refuseOutsideLoopback(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		outsideMu.Lock()
		outsideLoopback = append(outsideLoopback, address)
		outsideMu.Unlock()
		return nil, fmt.Errorf("eval test: %s is not loopback", address)
	}
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func TestMain(m *testing.M) {
	pifixture.RunIfRequested()
	http.DefaultTransport.(*http.Transport).DialContext = refuseOutsideLoopback
	code := m.Run()
	if len(outsideLoopback) > 0 {
		fmt.Fprintln(os.Stderr, "requests outside loopback:", outsideLoopback)
		code = 1
	}
	os.Exit(code)
}

// scripted plays a model that makes the tool calls a message asks for with "fixture:call <tool>
// <json>" lines, and leaves everything else to the Pi fixture.
type scripted struct{ *pi.Runtime }

func (s scripted) Run(ctx context.Context, request agentruntime.RunRequest, sink agentruntime.EventSink) (agentruntime.RunResult, error) {
	if strings.Contains(request.Message, quietMarker) {
		return agentruntime.RunResult{Response: prompt.NothingToAdd, Final: prompt.NothingToAdd, Usage: agentruntime.Usage{ModelCalls: 1}}, nil
	}
	var calls []string
	for _, line := range strings.Split(request.Message, "\n") {
		if call, ok := strings.CutPrefix(line, callPrefix); ok {
			calls = append(calls, call)
		}
	}
	if len(calls) == 0 {
		return s.Runtime.Run(ctx, request, sink)
	}
	var texts []string
	for i, call := range calls {
		name, arguments, _ := strings.Cut(call, " ")
		result, err := request.Tools.Executor.Execute(ctx, agentruntime.ToolCall{ID: fmt.Sprint("call-", i), Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil {
			return agentruntime.RunResult{}, err
		}
		texts = append(texts, result.Text)
	}
	text := strings.Join(texts, "\n")
	return agentruntime.RunResult{Response: text, Final: text, Usage: agentruntime.Usage{ModelCalls: len(calls), PromptTokens: 10, OutputTokens: 5}}, nil
}

type testWorld struct {
	*World
	t *testing.T
}

type worldConfig struct {
	user     User
	sections map[string]any
	typeSafe string
}

func newTestWorld(t *testing.T, config worldConfig, steps ...Step) testWorld {
	t.Helper()
	world := map[string]json.RawMessage{}
	for name, section := range config.sections {
		raw, err := json.Marshal(section)
		if err != nil {
			t.Fatal(err)
		}
		if err := DefaultRegistry.worlds[name](raw); err != nil {
			t.Fatalf("world.%s: %v", name, err)
		}
		world[name] = raw
	}
	c := Case{
		ID: "world-test", Title: "World test", Kind: KindHappy, Clock: Clock{Local: clockLocal, Zone: clockZone},
		User: config.user, World: world, Steps: steps, Trials: 1,
	}
	return newCaseWorld(t, c, config.typeSafe)
}

// newCaseWorld builds the world of a case over the scripted Pi fixture runtime.
func newCaseWorld(t *testing.T, c Case, typeSafe string) testWorld {
	t.Helper()
	return newVariantWorld(t, c, typeSafe, nil)
}

// newVariantWorld is newCaseWorld with a prompt variant on the world's turns.
func newVariantWorld(t *testing.T, c Case, typeSafe string, variant *Variant) testWorld {
	t.Helper()
	profiles := filepath.Join(t.TempDir(), "profiles")
	directory := filepath.Join(profiles, PrincipalID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":%q,"refresh":%q,"expires":%d,"accountId":%q}}`,
		codexauth.AccessToken(testAccount), codexauth.RefreshToken(testAccount), time.Now().Add(time.Hour).UnixMilli(), testAccount)
	if err := os.WriteFile(filepath.Join(directory, "auth.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := pi.New(t.Context(), pi.Config{
		Binary: binary, Provider: "fixture-provider", Model: agentruntime.ModelEffort{Model: "fixture-model", Effort: "low"},
		ExpectedVersion: pifixture.Version, ProfilesDir: profiles, StopGrace: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	options := WorldOptions{
		Runtime: scripted{runtime}, ProfilesDir: profiles, AccountID: testAccount, StepTimeout: 30 * time.Second,
		ChatModel: agentruntime.ModelEffort{Model: "gpt-test", Effort: agentruntime.DefaultEffort}, Variant: variant,
	}
	if typeSafe != "" {
		options.TypeSafe = judgment.Config{APIKey: testTypeSafe, Endpoint: typeSafe}
	}
	w, err := NewWorld(t.Context(), c, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	})
	return testWorld{World: w, t: t}
}

func (w testWorld) play(step Step) TurnRecord {
	w.t.Helper()
	record, err := w.Play(w.t.Context(), step)
	if err != nil {
		w.t.Fatalf("play %+v: %v", step, err)
	}
	return record
}

func (w testWorld) start() time.Time {
	w.t.Helper()
	start, err := w.Case.Clock.Time()
	if err != nil {
		w.t.Fatal(err)
	}
	return start
}

func call(tool, arguments string) string { return callPrefix + tool + " " + arguments }

func (w testWorld) toolResult(record TurnRecord, tool string) ToolCall {
	w.t.Helper()
	for _, c := range record.ToolCalls {
		if c.Name == tool {
			return c
		}
	}
	w.t.Fatalf("no call of %s in %+v", tool, record.ToolCalls)
	return ToolCall{}
}

func (w testWorld) scheduleRuns() int {
	w.t.Helper()
	var count int
	if err := w.DB().QueryRowContext(w.t.Context(), `SELECT COUNT(*) FROM schedule_runs`).Scan(&count); err != nil {
		w.t.Fatal(err)
	}
	return count
}

func TestSayReplyReachesTheStubAndNothingElse(t *testing.T) {
	w := newTestWorld(t, worldConfig{user: User{Name: "Sam"}})
	start := w.start()

	record := w.play(Step{Say: "hello there"})

	location := start.Location()
	if !strings.HasPrefix(record.Message, prompt.Time(start, location)) {
		t.Fatalf("message does not start with the case's time line %q:\n%s", prompt.Time(start, location), record.Message)
	}
	for _, want := range []string{prompt.User("Sam"), "hello there"} {
		if !strings.Contains(record.Message, want) {
			t.Fatalf("message lacks %q:\n%s", want, record.Message)
		}
	}
	if record.Reply != "reply: "+record.Message {
		t.Fatalf("reply = %q, want the fixture's echo of the message", record.Reply)
	}
	if len(record.Captured.Replies) != 1 || len(record.Captured.Notices) != 0 || len(record.Captured.Offers) != 0 {
		t.Fatalf("captured = %+v, want one reply and nothing else", record.Captured)
	}
	if runs := w.scheduleRuns(); runs != 0 {
		t.Fatalf("%d scheduled runs without a run_job step", runs)
	}
	if record.Usage.ModelCalls != 0 && len(record.Runs) != 1 {
		t.Fatalf("runs = %d, want one", len(record.Runs))
	}
}

func TestSeededConnectorsAnswerTheToolsThatReadThem(t *testing.T) {
	w := newTestWorld(t, worldConfig{sections: map[string]any{
		SectionGmail: gmailSection{Messages: []gmailMessage{{
			ID: "m1", From: "Billing <billing@example.test>", Subject: "Invoice 4711", Received: "2026-03-09T18:00",
			Body: "Your invoice 4711 is due on 2026-03-20.",
		}}},
		SectionCalendar: calendarSection{Events: []calendarEvent{{
			ID: "e1", Summary: "Planning review", Start: "2026-03-10T14:00", End: "2026-03-10T15:00",
		}}},
		SectionNotion: notionSection{Pages: []notionPage{{ID: "p1", Title: "Garden checklist", Lines: []string{"Water the ferns"}}}},
	}})

	record := w.play(Step{Say: strings.Join([]string{
		call("gmail_search", `{"query":"invoice"}`),
		call("calendar_events", `{"start":"2026-03-10","end":"2026-03-10"}`),
		call("notion_search", `{"query":"Garden"}`),
	}, "\n")})

	for tool, want := range map[string]string{
		"gmail_search":    "Invoice 4711",
		"calendar_events": "Planning review",
		"notion_search":   "Garden checklist",
	} {
		result := w.toolResult(record, tool)
		if result.IsError || !strings.Contains(result.Result, want) {
			t.Fatalf("%s = %+v, want a result with %q", tool, result, want)
		}
	}
	if w.Google == nil || w.Calendar == nil || w.Notion == nil {
		t.Fatal("a section's fake is not running")
	}
}

func TestInternalToolWritesTheTemporaryStoreOnTheCaseClock(t *testing.T) {
	w := newTestWorld(t, worldConfig{user: User{Memory: []string{"Prefers tea."}}})
	start := w.start()

	record := w.play(Step{Say: call("memory_add", `{"kind":"fact","text":"Lives upstairs."}`)})

	if result := w.toolResult(record, "memory_add"); result.IsError {
		t.Fatalf("memory_add = %+v", result)
	}
	entries, err := w.Assembly.Memory.Entries(t.Context(), PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, 0, len(entries))
	for _, entry := range entries {
		texts = append(texts, entry.Text)
		if !entry.CreatedAt.Equal(start) {
			t.Fatalf("entry %q created at %s, want the case clock %s", entry.Text, entry.CreatedAt, start)
		}
	}
	slices.Sort(texts)
	if want := []string{"Lives upstairs.", "Prefers tea."}; !slices.Equal(texts, want) {
		t.Fatalf("memory = %v, want %v", texts, want)
	}
}

func TestSeededInternalSections(t *testing.T) {
	w := newTestWorld(t, worldConfig{sections: map[string]any{
		SectionMemory:  []memorySeed{{Kind: "instruction", Text: "Answer briefly."}},
		SectionBacklog: []backlogSeed{{Title: "Fix the porch light"}},
		SectionRoutes: []map[string]any{{
			"name": "work", "days": []string{"mon", "tue"}, "legs": []map[string]any{{"mode": "walk", "minutes": 5}, {"mode": "train", "from": "Alpha", "to": "Beta"}},
		}},
		SectionPlaces: []placeSeed{{Name: "Allotment", Lat: 52.1, Lon: 4.3}},
		SectionJobs: []jobSeed{
			{ID: "morning-news", Prompt: "Summarise the news.", Cron: "0 9 * * *"},
			{ID: "call-dentist", Reminder: "Call the dentist", At: "2026-03-10 10:00"},
		},
	}})
	ctx := t.Context()

	if items, err := w.Assembly.Backlog.Items(ctx, PrincipalID); err != nil || len(items) != 1 || items[0].Title != "Fix the porch light" {
		t.Fatalf("backlog = %+v, %v", items, err)
	}
	if found, err := w.Assembly.Routes.Routes(ctx, PrincipalID); err != nil || len(found) != 1 || found[0].Name != "work" || len(found[0].Legs) != 2 {
		t.Fatalf("routes = %+v, %v", found, err)
	}
	place, ok, err := w.Assembly.Locations.PlaceNamed(ctx, PrincipalID, "Allotment")
	if err != nil || !ok || place.Latitude != 52.1 {
		t.Fatalf("place = %+v, %v, %v", place, ok, err)
	}
	jobs, err := w.Assembly.Scheduler.Jobs(ctx, PrincipalID)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	start := w.start()
	want := map[string]time.Time{
		w.jobIDs["morning-news"]: time.Date(2026, 3, 10, 9, 0, 0, 0, start.Location()),
		w.jobIDs["call-dentist"]: time.Date(2026, 3, 10, 10, 0, 0, 0, start.Location()),
	}
	for _, job := range jobs {
		if !job.NextRunAt.Equal(want[job.ID]) || !job.CreatedAt.Equal(start) {
			t.Fatalf("job %s next run %s created %s, want %s created %s", job.ID, job.NextRunAt, job.CreatedAt, want[job.ID], start)
		}
	}
	record := w.play(Step{Say: call("schedule_list", `{}`)})
	if result := w.toolResult(record, "schedule_list"); !strings.Contains(result.Result, "Summarise the news") && !strings.Contains(result.Result, "morning-news") {
		t.Fatalf("schedule_list = %q, want the seeded job", result.Result)
	}
}

func TestProposalsConfirmRejectAndExpireOnTheCaseClock(t *testing.T) {
	w := newTestWorld(t, worldConfig{sections: map[string]any{
		SectionGmail: gmailSection{Messages: []gmailMessage{{ID: "m1", From: "a@example.test", Subject: "Hello", Received: "2026-03-09T18:00", Body: "Hi."}}},
	}})
	ctx := t.Context()
	start := w.start()
	propose := func(text string) TurnRecord {
		return w.play(Step{Say: strings.Join([]string{
			call("gmail_search", `{"query":"hello"}`),
			call("memory_add", fmt.Sprintf(`{"kind":"instruction","text":%q}`, text)),
		}, "\n")})
	}

	first := propose("Answer in short sentences.")
	if len(first.Proposals) != 1 || first.Proposals[0].Tool != "memory_add" || first.Proposals[0].Status != approval.StatusPending {
		t.Fatalf("proposals = %+v, want one pending memory_add", first.Proposals)
	}
	record, found, err := w.Store.LoadApproval(ctx, first.Proposals[0].ID)
	if err != nil || !found {
		t.Fatalf("load approval: %v %v", found, err)
	}
	if !record.CreatedAt.Equal(start) || !record.ExpiresAt.Equal(start.Add(approval.DefaultLifetime)) {
		t.Fatalf("proposal created %s expires %s, want %s and %s", record.CreatedAt, record.ExpiresAt, start, start.Add(approval.DefaultLifetime))
	}

	confirmed := w.play(Step{Confirm: true})
	if len(confirmed.Resolved) != 1 || confirmed.Resolved[0].Status != approval.StatusExecuted {
		t.Fatalf("resolved = %+v, want executed", confirmed.Resolved)
	}
	if confirmed.Reply == "" || !strings.Contains(confirmed.Message, prompt.Continue) {
		t.Fatalf("confirm produced no continuation turn: %+v", confirmed)
	}
	entries, _ := w.Assembly.Memory.Entries(ctx, PrincipalID)
	if len(entries) != 1 || !entries[0].CreatedAt.Equal(start) {
		t.Fatalf("memory after confirm = %+v, want one entry on the case clock", entries)
	}

	second := propose("Never use emoji.")
	rejected := w.play(Step{Reject: true})
	if len(rejected.Resolved) != 1 || rejected.Resolved[0].Status != approval.StatusRejected || rejected.Resolved[0].ID != second.Proposals[0].ID {
		t.Fatalf("rejected = %+v", rejected.Resolved)
	}

	third := propose("Use metric units.")
	w.Advance(approval.DefaultLifetime + time.Minute)
	expired := w.play(Step{Confirm: true})
	if len(expired.Resolved) != 1 || expired.Resolved[0].ID != third.Proposals[0].ID || expired.Resolved[0].Status != approval.StatusExpired {
		t.Fatalf("after the lifetime = %+v, want expired", expired.Resolved)
	}
	if entries, _ := w.Assembly.Memory.Entries(ctx, PrincipalID); len(entries) != 1 {
		t.Fatalf("memory = %+v, want only the confirmed entry", entries)
	}
}

func TestRunJobRunsThePromptAndNothingRunsOnItsOwn(t *testing.T) {
	w := newTestWorld(t, worldConfig{user: User{Language: "ru"}, sections: map[string]any{
		SectionJobs: []jobSeed{
			{ID: "digest", Name: "Digest", Prompt: "Summarise the open items.", Cron: "0 9 * * *"},
			{ID: "heads-up", HeadsUp: true, Prompt: "Keep it short."},
			{ID: "quiet", Name: "Quiet check", Prompt: quietMarker, Cron: "0 9 * * *"},
		},
	}})
	start := w.start()

	w.play(Step{Say: "hello"})
	if runs := w.scheduleRuns(); runs != 0 {
		t.Fatalf("%d runs before any run_job step", runs)
	}

	digest := w.play(Step{RunJob: "digest"})
	if !strings.Contains(digest.Message, "Summarise the open items.") || !strings.Contains(digest.Reply, "Digest") || digest.Quiet || !strings.Contains(digest.Message, "Interface language: Russian") {
		t.Fatalf("digest = %+v", digest)
	}
	var created string
	if err := w.DB().QueryRowContext(t.Context(), `SELECT created_at FROM schedule_runs`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if want := start.UTC().Format("2006-01-02T15:04:05.000Z"); created != want {
		t.Fatalf("run recorded at %s, want the case clock %s", created, want)
	}

	headsUp := w.play(Step{RunJob: "heads-up"})
	if !strings.Contains(headsUp.Message, schedule.HeadsUpTask(schedule.HeadsUpJobID(PrincipalID), "")) || !strings.Contains(headsUp.Message, "Keep it short.") {
		t.Fatalf("heads-up message lacks the built-in task:\n%s", headsUp.Message)
	}

	quiet := w.play(Step{RunJob: "quiet"})
	if !quiet.Quiet || quiet.Reply != "" {
		t.Fatalf("quiet run = %+v, want quiet with no reply", quiet)
	}
	if running := w.scheduling; running {
		t.Fatal("the scheduler is still running after the step")
	}
}

// A triage_mail step delivers the section's mail once and lets the mailbox triage it with the mail
// tools alone; the fixture's echo is no triage answer, so the notice lists the mail as it arrived.
func TestTriageMailDeliversTheMailOnceAndTheNoticeIsTheReply(t *testing.T) {
	w := newTestWorld(t, worldConfig{user: User{Name: "Sam"}, sections: map[string]any{
		SectionAssistantMail: assistantMailSection{Messages: []assistantMailMessage{
			{From: "Venue Desk <desk@venue.example>", Subject: "Choose a room", Body: "Which room do you want?"},
		}},
	}})

	record := w.play(Step{TriageMail: true})
	if record.Quiet || !strings.HasPrefix(record.Reply, "📬 1 new email at your assistant address:\n• Venue Desk (desk@venue.example) · Choose a room · ") {
		t.Fatalf("triage = %+v", record)
	}
	if len(record.Runs) != 1 || !strings.Contains(record.Message, "This is a check of new mail in one thread") {
		t.Fatalf("runs = %d, message %q", len(record.Runs), record.Message)
	}
	var offered []string
	for _, definition := range record.Runs[0].Request.Tools.Definitions {
		offered = append(offered, definition.Name)
	}
	if !slices.Equal(offered, []string{"assistant_mail_list", "assistant_mail_read"}) {
		t.Fatalf("triage tools = %v", offered)
	}

	again := w.play(Step{TriageMail: true})
	if !again.Quiet || again.Reply != "" || len(again.Runs) != 0 {
		t.Fatalf("second triage = %+v", again)
	}
}

// A sent message starts the principal's thread as the assistant's own, so a reply in it is triaged
// with the routine-reply tool while the user has routine replies on.
func TestTriageMailStartsAThreadWithASentMessage(t *testing.T) {
	w := newTestWorld(t, worldConfig{user: User{Name: "Sam", Switches: map[string]bool{"assistant-mail-replies": true}}, sections: map[string]any{
		SectionAssistantMail: assistantMailSection{Messages: []assistantMailMessage{
			{Sent: true, ThreadID: "t1", To: "Venue Desk <desk@venue.example>", Subject: "Room booking", Body: "Please book the small room."},
			{ThreadID: "t1", From: "Venue Desk <desk@venue.example>", Subject: "Re: Room booking", Body: "Booked, thanks.", Received: "2026-03-10T08:35"},
		}},
	}})

	record := w.play(Step{TriageMail: true})
	if len(record.Runs) != 1 {
		t.Fatalf("runs = %d", len(record.Runs))
	}
	var offered []string
	for _, definition := range record.Runs[0].Request.Tools.Definitions {
		offered = append(offered, definition.Name)
	}
	if !slices.Equal(offered, []string{"assistant_mail_list", "assistant_mail_read", "assistant_mail_reply_routine"}) {
		t.Fatalf("triage tools = %v", offered)
	}
	first, found, err := w.Store.FirstThreadRoute(t.Context(), "t1")
	if err != nil || !found || first.Direction != "out" || first.PrincipalID != PrincipalID {
		t.Fatalf("first route = %+v %v %v", first, found, err)
	}
}

func TestThumbsDownStartsAReconsiderTurn(t *testing.T) {
	w := newTestWorld(t, worldConfig{})
	w.play(Step{Say: "what is two plus two"})

	record := w.play(Step{ThumbsDown: true})

	if record.Reconsider != "started" || !strings.Contains(record.Message, prompt.Reconsider) || record.Reply == "" {
		t.Fatalf("reconsider = %+v", record)
	}
}

// typeSafeStandIn answers every question the safety checks and the tool hint ask, and keeps what
// it was asked.
type typeSafeStandIn struct {
	*httptest.Server
	mu        sync.Mutex
	questions []map[string]judgment.Question
}

func newTypeSafeStandIn(t *testing.T) *typeSafeStandIn {
	t.Helper()
	stand := &typeSafeStandIn{}
	stand.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]judgment.Question `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		stand.mu.Lock()
		stand.questions = append(stand.questions, body.Questions)
		stand.mu.Unlock()
		answers := map[string]any{}
		for id, question := range body.Questions {
			switch question.Type {
			case "choice":
				answers[id] = map[string]any{"type": "choice", "choice": "schedule_list", "confidence": 0.99}
				if options, ok := question.Criteria.(map[string]any); ok {
					if _, has := options["schedule_list"]; !has {
						for option := range options {
							answers[id] = map[string]any{"type": "choice", "choice": option, "confidence": 0.99}
							break
						}
					}
				}
			default:
				answers[id] = map[string]any{"type": "noul", "noul": 0.05}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "stand-in", "answers": answers})
	}))
	t.Cleanup(stand.Close)
	return stand
}

func (s *typeSafeStandIn) asked(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, questions := range s.questions {
		if _, ok := questions[id]; ok {
			count++
		}
	}
	return count
}

func (s *typeSafeStandIn) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.questions)
}

func TestSafetyChecksAndToolHintReachTypeSafeOnlyWhenSwitchedOn(t *testing.T) {
	sections := map[string]any{
		SectionGmail: gmailSection{Messages: []gmailMessage{{ID: "m1", From: "a@example.test", Subject: "Hello", Received: "2026-03-09T18:00", Body: "Hi."}}},
	}
	steps := strings.Join([]string{
		call("gmail_search", `{"query":"hello"}`),
		call("memory_add", `{"kind":"instruction","text":"Answer in short sentences."}`),
	}, "\n")

	t.Run("on", func(t *testing.T) {
		stand := newTypeSafeStandIn(t)
		w := newTestWorld(t, worldConfig{sections: sections, typeSafe: stand.URL})

		record := w.play(Step{Say: steps})

		if stand.asked("tool") == 0 || !strings.Contains(record.Message, "Likely relevant tool: schedule_list") {
			t.Fatalf("tool hint asked %d times, message:\n%s", stand.asked("tool"), record.Message)
		}
		if stand.total() <= stand.asked("tool") {
			t.Fatalf("no safety check reached TypeSafe: %d requests, %d hints", stand.total(), stand.asked("tool"))
		}
	})

	t.Run("off", func(t *testing.T) {
		stand := newTypeSafeStandIn(t)
		w := newTestWorld(t, worldConfig{user: User{Switches: map[string]bool{judgment.OptInSwitch.Integration: false}}, sections: sections, typeSafe: stand.URL})

		record := w.play(Step{Say: steps})

		if stand.total() != 0 || strings.Contains(record.Message, "Likely relevant tool") {
			t.Fatalf("TypeSafe was asked %d times with the switch off", stand.total())
		}
	})
}

func TestWorldRefusesAnIncompleteSetup(t *testing.T) {
	_, err := NewWorld(t.Context(), Case{}, WorldOptions{})
	if err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("err = %v, want the missing runtime named", err)
	}
}

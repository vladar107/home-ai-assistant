package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/calendarsim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	aliceCalendar = "alice@example.test"
	bobCalendar   = "bob@example.test"
	primary       = "primary"
)

// ownGrants hands each principal only their own accounts and records every token asked for.
type ownGrants struct {
	mu     sync.Mutex
	owned  map[string][]google.Account
	asked  []string
	tokens map[string]string
}

func newOwnGrants() *ownGrants {
	return &ownGrants{
		owned: map[string][]google.Account{
			"alice": {{ID: "id-" + aliceCalendar, Address: aliceCalendar}},
			"bob":   {{ID: "id-" + bobCalendar, Address: bobCalendar}},
		},
		tokens: map[string]string{"token-alice": aliceCalendar, "token-bob": bobCalendar},
	}
}

func (g *ownGrants) Accounts(_ context.Context, principalID string, _ google.Product) ([]google.Account, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]google.Account(nil), g.owned[principalID]...), nil
}

func (g *ownGrants) AccessToken(_ context.Context, principalID, accountID string, _ google.Product, _ bool) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, principalID+"|"+accountID)
	for _, account := range g.owned[principalID] {
		if account.ID == accountID {
			return "token-" + principalID, nil
		}
	}
	return "", google.ErrNotConnected
}

func (g *ownGrants) accountFor(token string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	account, ok := g.tokens[token]
	return account, ok
}

// recordingDeliverer keeps every message the scheduler sends.
type recordingDeliverer struct {
	mu   sync.Mutex
	sent []string
}

func (d *recordingDeliverer) SendReply(context.Context, int64, int64, string) error { return nil }
func (d *recordingDeliverer) SendTrackedReply(_ context.Context, _, _ int64, text string) (int64, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, text)
	return int64(len(d.sent)), 0, nil
}
func (d *recordingDeliverer) SendTyping(context.Context, int64) error { return nil }
func (d *recordingDeliverer) SendMessageDraft(context.Context, int64, int64, string, bool) error {
	return nil
}

// SendReminder records a reminder sent with its snooze buttons.
func (d *recordingDeliverer) SendReminder(_ context.Context, _ int64, _, text string) (int64, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, "[snooze] "+text)
	return int64(len(d.sent)), 0, nil
}

func (d *recordingDeliverer) messages() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.sent...)
}

// gate holds the events requests of one account until it is released, as a slow calendar does.
type gate struct {
	base    http.RoundTripper
	mu      sync.Mutex
	held    string
	blocked chan struct{}
	release chan struct{}
}

func (g *gate) RoundTrip(request *http.Request) (*http.Response, error) {
	g.mu.Lock()
	held := g.held != "" && request.Header.Get("Authorization") == "Bearer "+g.held && strings.Contains(request.URL.Path, "/events")
	g.mu.Unlock()
	if held {
		select {
		case g.blocked <- struct{}{}:
		default:
		}
		select {
		case <-g.release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	}
	return g.base.RoundTrip(request)
}

type anchorFixture struct {
	t         *testing.T
	ctx       context.Context
	sim       *calendarsim.Simulator
	grants    *ownGrants
	gate      *gate
	scheduler *schedule.Service
	store     *sqlite.Store
	db        *sql.DB
	sent      *recordingDeliverer

	mu  sync.Mutex
	now time.Time
}

func newAnchorFixture(t *testing.T) *anchorFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "assistant.sqlite")
	store := testutil.OpenStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	f := &anchorFixture{t: t, ctx: t.Context(), grants: newOwnGrants(), store: store, db: testutil.OpenDB(t, path), sent: &recordingDeliverer{},
		now: time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)}
	f.sim = calendarsim.New(t, f.grants.accountFor)
	for _, account := range []string{aliceCalendar, bobCalendar} {
		f.sim.AddCalendar(account, calendarsim.Calendar{ID: primary, Summary: "Personal", Primary: true})
	}
	f.gate = &gate{base: f.sim.Client().Transport, blocked: make(chan struct{}, 1), release: make(chan struct{})}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	calendarService, err := NewService(Config{Location: time.UTC, APIBaseURL: f.sim.APIBaseURL(), HTTPClient: &http.Client{Transport: f.gate}}, f.grants, logger)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := schedule.NewService(schedule.Config{Zone: "UTC", TickInterval: time.Hour, Now: f.clock, Reminders: f.sent}, store, store, f.sent, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Register(schedule.Reminder(f.clock)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{schedule.AnchorCalendarEvent, schedule.AnchorCalendarMatch} {
		if err := scheduler.RegisterAnchor(kind, calendarService); err != nil {
			t.Fatal(err)
		}
	}
	f.scheduler = scheduler
	return f
}

func (f *anchorFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *anchorFixture) set(at time.Time) {
	f.mu.Lock()
	f.now = at
	f.mu.Unlock()
}

func (f *anchorFixture) start() {
	f.t.Helper()
	if err := f.scheduler.Start(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = f.scheduler.Shutdown(context.Background()) })
}

func (f *anchorFixture) remind(principalID, arguments string) tools.Result {
	f.t.Helper()
	result, err := f.scheduler.Execute(f.ctx, principalID, tools.Call{Name: "reminder_set", Arguments: json.RawMessage(arguments)})
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *anchorFixture) onlyJob(principalID string) schedule.Job {
	f.t.Helper()
	var id string
	if err := f.db.QueryRowContext(f.ctx, `SELECT id FROM scheduled_jobs WHERE principal_id = ?`, principalID).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	job, found, err := f.store.LoadScheduledJob(f.ctx, principalID, id)
	if err != nil || !found {
		f.t.Fatalf("load %s: %v %v", id, found, err)
	}
	return job
}

func (f *anchorFixture) runs(jobID string) []time.Time {
	f.t.Helper()
	rows, err := f.db.QueryContext(f.ctx, `SELECT scheduled_for FROM schedule_runs WHERE job_id = ? ORDER BY scheduled_for`, jobID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var times []time.Time
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			f.t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			f.t.Fatal(err)
		}
		times = append(times, at)
	}
	return times
}

func (f *anchorFixture) waitFor(what string, done func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *anchorFixture) delivered(text string) int {
	count := 0
	for _, message := range f.sent.messages() {
		if strings.Contains(message, text) {
			count++
		}
	}
	return count
}

func at(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC)
}

// A reminder 30 minutes before an event fires once at that time, follows the event when it moves,
// and no run exists before it is due.
func TestAReminderBeforeAnEventFollowsTheEventAndFiresOnce(t *testing.T) {
	f := newAnchorFixture(t)
	f.sim.AddEvents(aliceCalendar, primary, calendarsim.Event{ID: "dentist", Summary: "Dentist", Start: at(5, 10, 0), End: at(5, 11, 0)})
	result := f.remind("alice", `{"text":"Leave for the dentist","anchor":{"kind":"calendar_event","account":"`+aliceCalendar+`","calendar_id":"primary","event_id":"dentist","offset_minutes":-30}}`)
	if result.IsError || !strings.Contains(result.Text, "Mon 5 Oct 09:30 UTC, 30 min before Dentist") {
		t.Fatalf("reminder_set = %+v", result)
	}
	job := f.onlyJob("alice")
	if !job.RunAt.Equal(at(5, 9, 30)) || !job.NextRunAt.Equal(at(5, 9, 30)) || job.OccurrenceKey != "primary/dentist" {
		t.Fatalf("anchored reminder = %+v", job)
	}
	f.start()

	f.set(at(5, 9, 0))
	f.scheduler.Wake()
	f.scheduler.WakeAnchors()
	time.Sleep(50 * time.Millisecond)
	if runs := f.runs(job.ID); len(runs) != 0 {
		t.Fatalf("runs before the reminder is due = %v", runs)
	}

	f.sim.ReplaceEvent(aliceCalendar, primary, calendarsim.Event{ID: "dentist", Summary: "Dentist", Start: at(5, 11, 0), End: at(5, 12, 0)})
	f.set(at(5, 9, 20))
	f.scheduler.WakeAnchors()
	f.waitFor("the reminder to follow the moved event", func() bool { return f.onlyJob("alice").RunAt.Equal(at(5, 10, 30)) })
	f.scheduler.Wake()
	time.Sleep(50 * time.Millisecond)
	if runs := f.runs(job.ID); len(runs) != 0 {
		t.Fatalf("runs at the event's old time = %v", runs)
	}

	f.set(at(5, 10, 31))
	f.scheduler.Wake()
	f.waitFor("the reminder to be delivered", func() bool { return f.delivered("Leave for the dentist") == 1 })
	f.scheduler.Wake()
	f.scheduler.WakeAnchors()
	time.Sleep(50 * time.Millisecond)
	if runs := f.runs(job.ID); len(runs) != 1 || !runs[0].Equal(at(5, 10, 30)) || f.delivered("[snooze] ⏰ Leave for the dentist") != 1 {
		t.Fatalf("runs = %v, messages %q", runs, f.sent.messages())
	}

	// A snoozed reminder keeps the time it was snoozed to, however the event moves after.
	snoozed, ok, err := f.scheduler.Snooze(f.ctx, "alice", job.ID, schedule.SnoozeHour)
	if err != nil || !ok || !snoozed.RunAt.Equal(at(5, 11, 31)) {
		t.Fatalf("snooze = %+v %v %v", snoozed, ok, err)
	}
	f.sim.ReplaceEvent(aliceCalendar, primary, calendarsim.Event{ID: "dentist", Summary: "Dentist", Start: at(5, 14, 0), End: at(5, 15, 0)})
	f.set(at(5, 11, 10))
	requests := len(f.sim.Requests())
	f.scheduler.WakeAnchors()
	time.Sleep(50 * time.Millisecond)
	if got := f.onlyJob("alice"); !got.RunAt.Equal(at(5, 11, 31)) || !got.NextRunAt.Equal(at(5, 11, 31)) || len(f.sim.Requests()) != requests {
		t.Fatalf("snoozed reminder after its event moved = %+v", got)
	}
}

// A cancelled event finishes its reminder and tells the user once.
func TestACancelledEventFinishesTheReminderWithOneLine(t *testing.T) {
	f := newAnchorFixture(t)
	f.sim.AddEvents(aliceCalendar, primary, calendarsim.Event{ID: "checkup", Summary: "Checkup", Start: at(5, 12, 0), End: at(5, 13, 0)})
	if result := f.remind("alice", `{"text":"Bring the forms","anchor":{"kind":"calendar_event","account":"`+aliceCalendar+`","calendar_id":"primary","event_id":"checkup","offset_minutes":-60}}`); result.IsError {
		t.Fatalf("reminder_set = %+v", result)
	}
	job := f.onlyJob("alice")
	f.start()

	f.sim.ReplaceEvent(aliceCalendar, primary, calendarsim.Event{ID: "checkup", Summary: "Checkup", Start: at(5, 12, 0), End: at(5, 13, 0), Cancelled: true})
	f.set(at(5, 8, 20))
	f.scheduler.WakeAnchors()
	f.waitFor("the cancellation notice", func() bool { return f.delivered("Event cancelled, so the reminder is gone.") == 1 })
	if finished := f.onlyJob("alice"); finished.FinishedAt.IsZero() {
		t.Fatalf("the reminder of a cancelled event is still pending: %+v", finished)
	}
	f.set(at(5, 11, 1))
	f.scheduler.Wake()
	f.scheduler.WakeAnchors()
	time.Sleep(50 * time.Millisecond)
	if messages := f.sent.messages(); len(messages) != 1 || messages[0] != "**Bring the forms**: Event cancelled, so the reminder is gone." {
		t.Fatalf("messages after the cancellation = %q", messages)
	}
	var kind string
	if err := f.db.QueryRowContext(f.ctx, `SELECT error_kind FROM schedule_runs WHERE job_id = ?`, job.ID).Scan(&kind); err != nil || kind != "anchor_gone" {
		t.Fatalf("notice run = %q %v", kind, err)
	}
}

// A calendar_match reminder runs for each matching event in turn, never for others, stays
// enabled between them, and resumes at the next match after a pause.
func TestAReminderBeforeEachMatchingEventRunsForEachInTurn(t *testing.T) {
	f := newAnchorFixture(t)
	f.sim.AddEvents(aliceCalendar, primary,
		calendarsim.Event{ID: "p1", Summary: "Planning", Start: at(5, 10, 0), End: at(5, 11, 0)},
		calendarsim.Event{ID: "lunch", Summary: "Lunch", Start: at(5, 12, 0), End: at(5, 13, 0)},
		calendarsim.Event{ID: "p2", Summary: "Sprint planning", Start: at(6, 10, 0), End: at(6, 11, 0)},
		calendarsim.Event{ID: "p3", Summary: "Planning", Start: at(8, 10, 0), End: at(8, 11, 0)},
	)
	result := f.remind("alice", `{"text":"Agenda time","anchor":{"kind":"calendar_match","title":"planning","offset_minutes":-60}}`)
	if result.IsError || !strings.Contains(result.Text, `1 h before each event titled "planning"; next: Mon 5 Oct 09:00 UTC`) {
		t.Fatalf("reminder_set = %+v", result)
	}
	job := f.onlyJob("alice")
	if !job.AnchorRepeats || job.OneTime() || !job.NextRunAt.Equal(at(5, 9, 0)) {
		t.Fatalf("repeating reminder = %+v", job)
	}
	f.start()

	f.set(at(5, 9, 1))
	f.scheduler.Wake()
	f.waitFor("the next match after the first run", func() bool { return f.onlyJob("alice").NextRunAt.Equal(at(6, 9, 0)) })
	if got := f.onlyJob("alice"); !got.Enabled || !got.FinishedAt.IsZero() {
		t.Fatalf("job after its first run = %+v", got)
	}
	f.set(at(5, 11, 30))
	f.scheduler.Wake()
	f.set(at(6, 9, 1))
	f.scheduler.Wake()
	f.waitFor("the second run", func() bool { return len(f.runs(job.ID)) == 2 })
	f.waitFor("the third match", func() bool { return f.onlyJob("alice").NextRunAt.Equal(at(8, 9, 0)) })

	if changed, err := f.scheduler.SetEnabled(f.ctx, "alice", job.ID, false); err != nil || !changed {
		t.Fatalf("pause = %v %v", changed, err)
	}
	f.sim.AddEvents(aliceCalendar, primary, calendarsim.Event{ID: "p4", Summary: "Planning", Start: at(9, 10, 0), End: at(9, 11, 0)})
	f.set(at(8, 12, 0))
	f.scheduler.Wake()
	if changed, err := f.scheduler.SetEnabled(f.ctx, "alice", job.ID, true); err != nil || !changed {
		t.Fatalf("resume = %v %v", changed, err)
	}
	if got := f.onlyJob("alice"); !got.NextRunAt.Equal(at(9, 9, 0)) || got.OccurrenceKey != "primary/p4" {
		t.Fatalf("resumed job = %+v", got)
	}
	if runs := f.runs(job.ID); len(runs) != 2 || !runs[0].Equal(at(5, 9, 0)) || !runs[1].Equal(at(6, 9, 0)) {
		t.Fatalf("runs = %v", runs)
	}
	f.waitFor("both reminders delivered", func() bool { return f.delivered("Agenda time") == 2 })
	if f.delivered("[snooze]") != 0 {
		t.Fatalf("a repeating reminder was sent with snooze buttons: %q", f.sent.messages())
	}
}

// An event that moves after its reminder ran never runs the reminder a second time; the next match
// is the next event.
func TestAMatchedEventMovedAfterItsRunIsNotRunAgain(t *testing.T) {
	f := newAnchorFixture(t)
	f.sim.AddEvents(aliceCalendar, primary,
		calendarsim.Event{ID: "p1", Summary: "Planning", Start: at(5, 10, 0), End: at(5, 11, 0)},
		calendarsim.Event{ID: "p2", Summary: "Planning", Start: at(7, 10, 0), End: at(7, 11, 0)},
	)
	if result := f.remind("alice", `{"text":"Agenda time","anchor":{"kind":"calendar_match","title":"Planning","offset_minutes":-60}}`); result.IsError {
		t.Fatalf("reminder_set = %+v", result)
	}
	job := f.onlyJob("alice")
	f.start()
	f.set(at(5, 9, 1))
	f.scheduler.Wake()
	f.waitFor("the second match after the first run", func() bool { return f.onlyJob("alice").NextRunAt.Equal(at(7, 9, 0)) })

	f.sim.ReplaceEvent(aliceCalendar, primary, calendarsim.Event{ID: "p1", Summary: "Planning", Start: at(6, 10, 0), End: at(6, 11, 0)})
	f.set(at(5, 9, 20))
	f.scheduler.WakeAnchors()
	time.Sleep(50 * time.Millisecond)
	f.set(at(5, 9, 40))
	f.scheduler.WakeAnchors()
	time.Sleep(50 * time.Millisecond)
	if got := f.onlyJob("alice"); !got.NextRunAt.Equal(at(7, 9, 0)) || got.OccurrenceKey != "primary/p2" || got.LastOccurrenceKey != "primary/p1" {
		t.Fatalf("job after the run event moved = %+v", got)
	}
	f.set(at(6, 9, 1))
	f.scheduler.Wake()
	time.Sleep(50 * time.Millisecond)
	if runs := f.runs(job.ID); len(runs) != 1 {
		t.Fatalf("runs after the event moved = %v", runs)
	}
}

// A calendar that hangs holds up only its owner's anchors: another principal's confirmation within
// 30 minutes of its run still moves that job.
func TestAHangingCalendarDoesNotHoldUpAnotherPrincipalsAnchors(t *testing.T) {
	f := newAnchorFixture(t)
	f.sim.AddEvents(aliceCalendar, primary, calendarsim.Event{ID: "p1", Summary: "Planning", Start: at(6, 10, 0), End: at(6, 11, 0)})
	f.sim.AddEvents(bobCalendar, primary, calendarsim.Event{ID: "visit", Summary: "Visit", Start: at(5, 10, 0), End: at(5, 11, 0)})
	if result := f.remind("alice", `{"text":"Agenda","anchor":{"kind":"calendar_match","title":"Planning","offset_minutes":-60}}`); result.IsError {
		t.Fatalf("alice's reminder = %+v", result)
	}
	if result := f.remind("bob", `{"text":"Bring flowers","anchor":{"kind":"calendar_event","account":"`+bobCalendar+`","calendar_id":"primary","event_id":"visit","offset_minutes":-30}}`); result.IsError {
		t.Fatalf("bob's reminder = %+v", result)
	}
	f.gate.mu.Lock()
	f.gate.held = "token-alice"
	f.gate.mu.Unlock()
	f.start()
	select {
	case <-f.gate.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("alice's calendar was never read")
	}

	f.sim.ReplaceEvent(bobCalendar, primary, calendarsim.Event{ID: "visit", Summary: "Visit", Start: at(5, 9, 45), End: at(5, 11, 0)})
	f.set(at(5, 9, 5))
	f.scheduler.WakeAnchors()
	f.waitFor("bob's confirmation while alice's calendar hangs", func() bool { return f.onlyJob("bob").RunAt.Equal(at(5, 9, 15)) })
	close(f.gate.release)
}

// A calendar that answers slowly holds up only its own owner's anchor, never another principal's
// due reminder.
func TestASlowCalendarDoesNotDelayAnotherPrincipalsDueJob(t *testing.T) {
	f := newAnchorFixture(t)
	f.sim.AddEvents(aliceCalendar, primary, calendarsim.Event{ID: "p1", Summary: "Planning", Start: at(6, 10, 0), End: at(6, 11, 0)})
	if result := f.remind("alice", `{"text":"Agenda","anchor":{"kind":"calendar_match","title":"Planning","offset_minutes":-60}}`); result.IsError {
		t.Fatalf("alice's reminder = %+v", result)
	}
	if result := f.remind("bob", `{"text":"Call the plumber","at":"2026-10-05 08:30"}`); result.IsError {
		t.Fatalf("bob's reminder = %+v", result)
	}
	f.gate.mu.Lock()
	f.gate.held = "token-alice"
	f.gate.mu.Unlock()
	f.start()

	f.set(at(5, 8, 31))
	f.scheduler.WakeAnchors()
	select {
	case <-f.gate.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("alice's calendar was never read")
	}
	f.scheduler.Wake()
	f.waitFor("bob's reminder while alice's calendar is slow", func() bool { return f.delivered("Call the plumber") == 1 })
	close(f.gate.release)
}

// One principal's anchor is never resolved with another's calendars.
func TestAnotherPrincipalsCalendarIsNeverRead(t *testing.T) {
	f := newAnchorFixture(t)
	f.sim.AddEvents(aliceCalendar, primary, calendarsim.Event{ID: "secret", Summary: "Private", Start: at(5, 15, 0), End: at(5, 16, 0)})
	result := f.remind("bob", `{"text":"Snoop","anchor":{"kind":"calendar_event","account":"`+aliceCalendar+`","calendar_id":"primary","event_id":"secret","offset_minutes":-10}}`)
	if !result.IsError {
		t.Fatalf("bob anchored a reminder to alice's event: %+v", result)
	}
	if result := f.remind("bob", `{"text":"Snoop","anchor":{"kind":"calendar_match","title":"Private","offset_minutes":-10}}`); result.IsError {
		t.Fatalf("bob's match = %+v", result)
	} else if job := f.onlyJob("bob"); job.OccurrenceKey != "" {
		t.Fatalf("bob's match found alice's event: %+v", job)
	}
	f.grants.mu.Lock()
	defer f.grants.mu.Unlock()
	for _, asked := range f.grants.asked {
		if strings.HasPrefix(asked, "bob|") && asked != "bob|id-"+bobCalendar {
			t.Fatalf("bob's anchor asked for %s", asked)
		}
	}
	for _, request := range f.sim.Requests() {
		if request.Account == aliceCalendar {
			t.Fatalf("alice's calendar was read for bob: %+v", request)
		}
	}
}

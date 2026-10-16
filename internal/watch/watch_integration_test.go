package watch_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set moves the clock to a local time in Amsterdam, such as "2026-09-28 06:15".
func (c *testClock) Set(t *testing.T, local string) {
	t.Helper()
	location, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.ParseInLocation("2006-01-02 15:04", local, location)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

type answer struct {
	observation watch.Observation
	err         error
}

// fakeEvaluator answers every check with the answer currently set, and asks the cycle's cache
// with the watch's target, so watches on the same target share one upstream query.
type fakeEvaluator struct {
	mu      sync.Mutex
	current answer
	calls   int
	fetches int
}

func (f *fakeEvaluator) set(observation watch.Observation, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = answer{observation: observation, err: err}
}

func (f *fakeEvaluator) Evaluate(ctx context.Context, _ string, w watch.Watch, occurrence watch.Occurrence) (watch.Observation, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	key := fmt.Sprintf("%s|%s|%s|%s", w.Target.From, w.Target.To, occurrence.Date, w.End)
	result, err := watch.Cached(ctx, occurrence.Cache, key, func(context.Context) (answer, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.fetches++
		return f.current, nil
	})
	if err != nil {
		return watch.Observation{}, err
	}
	return result.observation, result.err
}

func (f *fakeEvaluator) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.fetches
}

type sentAlert struct {
	chatID  int64
	watchID string
	text    string
}

type transientError struct{}

func (transientError) Error() string { return "telegram is unavailable" }

type fakeSender struct {
	mu       sync.Mutex
	sent     []sentAlert
	attempts int
	failure  error
}

func (f *fakeSender) SendAlert(_ context.Context, chatID int64, watchID, text string) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.failure != nil {
		return 0, 0, f.failure
	}
	f.sent = append(f.sent, sentAlert{chatID: chatID, watchID: watchID, text: text})
	return int64(f.attempts), 0, nil
}

func (f *fakeSender) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure = err
}

// take returns and forgets what was sent since the last call.
func (f *fakeSender) take() []sentAlert {
	f.mu.Lock()
	defer f.mu.Unlock()
	sent := f.sent
	f.sent = nil
	return sent
}

type fakeTelemetry struct {
	mu     sync.Mutex
	events []string
}

func (f *fakeTelemetry) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *fakeTelemetry) RecordWatchCheck(_ context.Context, principal, kind, outcome string) {
	f.record("check " + principal + " " + kind + " " + outcome)
}

func (f *fakeTelemetry) RecordWatchAlert(_ context.Context, principal, kind, alertType, outcome string) {
	f.record("alert " + principal + " " + kind + " " + alertType + " " + outcome)
}

func (f *fakeTelemetry) RecordWatchUpstreamError(_ context.Context, kind, service string) {
	f.record("upstream " + kind + " " + service)
}

func (f *fakeTelemetry) RecordWatchRewrite(_ context.Context, principal, outcome string) {
	f.record("rewrite " + principal + " " + outcome)
}

func (f *fakeTelemetry) count(event string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, recorded := range f.events {
		if recorded == event {
			count++
		}
	}
	return count
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

type engine struct {
	t         *testing.T
	ctx       context.Context
	path      string
	store     *sqlite.Store
	clock     *testClock
	evaluator *fakeEvaluator
	sender    *fakeSender
	telemetry *fakeTelemetry
	logs      *lockedBuffer
	service   *watch.Service
}

// Monday 28 September 2026, before any window opens.
const mondayEarly = "2026-09-28 05:00"

func newEngine(t *testing.T) *engine {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state", "assistant.sqlite")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 202},
	}); err != nil {
		t.Fatal(err)
	}
	e := &engine{
		t: t, ctx: ctx, path: path, store: store, clock: &testClock{}, evaluator: &fakeEvaluator{},
		sender: &fakeSender{}, telemetry: &fakeTelemetry{}, logs: &lockedBuffer{},
	}
	e.clock.Set(t, mondayEarly)
	e.evaluator.set(onTime, nil)
	e.service = e.newService()
	return e
}

// newService builds a service on the same store, as a restarted process does.
func (e *engine) newService() *watch.Service {
	e.t.Helper()
	service, err := watch.NewService(watch.Config{Zone: "Europe/Amsterdam", Now: e.clock.Now}, e.store, e.store, e.sender, nil, slog.New(slog.NewTextHandler(e.logs, nil)))
	if err != nil {
		e.t.Fatal(err)
	}
	if err := service.Register(watch.KindDeparture, e.evaluator); err != nil {
		e.t.Fatal(err)
	}
	service.SetTelemetry(e.telemetry)
	return service
}

// at moves the clock and runs one cycle.
func (e *engine) at(local string) {
	e.t.Helper()
	e.clock.Set(e.t, local)
	e.service.Cycle(e.ctx)
}

func (e *engine) wantSent(count int, contains ...string) []sentAlert {
	e.t.Helper()
	sent := e.sender.take()
	if len(sent) != count {
		e.t.Fatalf("sent %d alerts, want %d: %+v", len(sent), count, sent)
	}
	for _, alert := range sent {
		if alert.chatID != 101 {
			e.t.Fatalf("alert went to chat %d", alert.chatID)
		}
	}
	for index, want := range contains {
		if !strings.Contains(sent[index].text, want) {
			e.t.Fatalf("alert %d = %q, want it to contain %q", index, sent[index].text, want)
		}
	}
	return sent
}

func (e *engine) alertStatus(watchID, occurrence string, sequence int) (string, string) {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	var status string
	var reason sql.NullString
	if err := db.QueryRowContext(e.ctx, `SELECT status, reason FROM watch_alerts WHERE watch_id = ? AND occurrence = ? AND sequence = ?`,
		watchID, occurrence, sequence).Scan(&status, &reason); err != nil {
		e.t.Fatal(err)
	}
	return status, reason.String
}

var (
	onTime    = watch.Observation{State: "on_time", Headline: "It runs on time."}
	late      = watch.Observation{State: "delay>=5", Triggered: true, Headline: "It is 8 minutes late.", Details: []string{"Expected 07:50."}}
	cancelled = watch.Observation{State: "cancelled", Triggered: true, Headline: "It is cancelled."}
)

func departureSpec() watch.Spec {
	return watch.Spec{
		Kind:         watch.KindDeparture,
		Target:       watch.Target{Mode: watch.ModeTrain, From: "Den Haag Centraal", To: "Zwolle"},
		Triggers:     []string{watch.TriggerCancelled, watch.TriggerPartlyCancelled, watch.TriggerDelay, watch.TriggerPlatform},
		DelayMinutes: 5,
		Days:         []string{"mon", "tue", "wed", "thu", "fri"},
		Start:        "06:12",
		End:          "07:42",
		Zone:         "Europe/Amsterdam",
	}
}

func (e *engine) create(principalID, id string, spec watch.Spec) {
	e.t.Helper()
	now := e.clock.Now()
	created, err := e.store.CreateWatch(e.ctx, watch.Watch{ID: id, PrincipalID: principalID, Spec: spec, Enabled: true, CreatedAt: now, UpdatedAt: now}, watch.MaximumWatchesPerPrincipal)
	if err != nil || !created {
		e.t.Fatalf("create watch: %v %v", created, err)
	}
}

const watchID = "0000000000000001"

// Nothing is checked outside the window, an on-time departure stays silent, a delay over the
// threshold alerts once however often it is seen, and its recovery sends "back to normal" once.
func TestWatchAlertsOnceOnChangeAndOnceWhenBackToNormal(t *testing.T) {
	e := newEngine(t)
	e.create("alice", watchID, departureSpec())

	e.at("2026-09-28 06:00")
	if calls, _ := e.evaluator.counts(); calls != 0 {
		t.Fatalf("checked %d times before the window", calls)
	}
	e.at("2026-09-28 06:15")
	e.at("2026-09-28 06:17")
	e.at("2026-09-28 06:20")
	e.wantSent(0)
	if calls, _ := e.evaluator.counts(); calls != 2 {
		t.Fatalf("checks = %d, want 2: the 06:17 cycle was within five minutes of the last check", calls)
	}

	e.evaluator.set(late, nil)
	e.at("2026-09-28 06:25")
	sent := e.wantSent(1, "07:42 train Den Haag Centraal → Zwolle")
	if sent[0].watchID != watchID || !strings.Contains(sent[0].text, "It is 8 minutes late.\nExpected 07:50.") {
		t.Fatalf("alert = %+v", sent[0])
	}
	e.at("2026-09-28 06:30")
	e.at("2026-09-28 06:35")
	e.wantSent(0)

	e.evaluator.set(onTime, nil)
	e.at("2026-09-28 06:40")
	e.wantSent(1, "Back to normal. It runs on time.")
	e.at("2026-09-28 06:45")
	e.wantSent(0)

	if e.telemetry.count("check alice departure triggered") != 3 || e.telemetry.count("check alice departure normal") != 4 {
		t.Fatalf("check metrics = %v", e.telemetry.events)
	}
	if e.telemetry.count("alert alice departure alert delivered") != 1 || e.telemetry.count("alert alice departure recovered delivered") != 1 {
		t.Fatalf("alert metrics = %v", e.telemetry.events)
	}
	if notice := e.service.PendingNotice("alice"); !strings.Contains(notice, "8 minutes late") || !strings.Contains(notice, "Back to normal") {
		t.Fatalf("notice = %q", notice)
	}
	if notice := e.service.PendingNotice("bob"); notice != "" {
		t.Fatalf("bob's notice = %q", notice)
	}
}

// A cancellation seen on the first check alerts at once, and checks in the last half hour run
// every two minutes.
func TestWatchAlertsOnCancellationAndChecksMoreOftenNearDeparture(t *testing.T) {
	e := newEngine(t)
	e.create("alice", watchID, departureSpec())
	e.evaluator.set(cancelled, nil)

	e.at("2026-09-28 07:15")
	e.wantSent(1, "It is cancelled.")
	e.at("2026-09-28 07:17")
	e.at("2026-09-28 07:19")
	e.wantSent(0)
	if calls, _ := e.evaluator.counts(); calls != 3 {
		t.Fatalf("checks = %d, want 3 at two-minute intervals", calls)
	}
}

// Three failed checks in a row send one notice, a fourth sends nothing, and checks that work
// again send nothing unless a real alert is due.
func TestWatchTellsOnceWhenChecksKeepFailing(t *testing.T) {
	e := newEngine(t)
	e.create("alice", watchID, departureSpec())
	failure := &watch.UpstreamError{Service: "ns", Err: errors.New("503 Service Unavailable")}
	e.evaluator.set(watch.Observation{}, failure)

	e.at("2026-09-28 06:15")
	e.at("2026-09-28 06:20")
	e.wantSent(0)
	e.at("2026-09-28 06:25")
	e.wantSent(1, "I can't check this right now")
	e.at("2026-09-28 06:30")
	e.wantSent(0)

	e.evaluator.set(onTime, nil)
	e.at("2026-09-28 06:35")
	e.wantSent(0)
	e.evaluator.set(late, nil)
	e.at("2026-09-28 06:40")
	e.wantSent(1, "8 minutes late")

	if e.telemetry.count("upstream departure ns") != 4 || e.telemetry.count("check alice departure failed") != 4 {
		t.Fatalf("failure metrics = %v", e.telemetry.events)
	}
	if e.telemetry.count("alert alice departure failure delivered") != 1 {
		t.Fatalf("notice metrics = %v", e.telemetry.events)
	}
}

// A restart in the middle of a window keeps the state: the same delay does not alert again, and
// its recovery still sends "back to normal". A ready alert left by the previous process is sent.
func TestWatchStateSurvivesRestart(t *testing.T) {
	e := newEngine(t)
	e.create("alice", watchID, departureSpec())
	e.evaluator.set(late, nil)
	e.sender.fail(transientError{})
	e.at("2026-09-28 06:15")
	e.wantSent(0)

	e.sender.fail(nil)
	e.service = e.newService()
	e.at("2026-09-28 06:16")
	e.wantSent(1, "8 minutes late")
	e.at("2026-09-28 06:20")
	e.wantSent(0)

	e.service = e.newService()
	e.at("2026-09-28 06:25")
	e.wantSent(0)
	e.evaluator.set(onTime, nil)
	e.at("2026-09-28 06:30")
	e.wantSent(1, "Back to normal")
}

// An alert that cannot be sent is retried while its window lasts and dropped after it, never sent
// late; one that Telegram refuses for good is not retried.
func TestWatchDropsAlertsPastTheirWindow(t *testing.T) {
	e := newEngine(t)
	e.create("alice", watchID, departureSpec())
	e.evaluator.set(late, nil)
	e.sender.fail(transientError{})

	e.at("2026-09-28 07:35")
	e.at("2026-09-28 07:41")
	if status, _ := e.alertStatus(watchID, "2026-09-28", 1); status != "ready" {
		t.Fatalf("status inside the window = %s", status)
	}
	e.sender.fail(nil)
	e.at("2026-09-28 07:43")
	e.wantSent(0)
	if status, reason := e.alertStatus(watchID, "2026-09-28", 1); status != "dropped" || reason != "late" {
		t.Fatalf("late alert = %s %s", status, reason)
	}
	if e.sender.attempts != 2 || e.telemetry.count("alert alice departure alert dropped") != 1 {
		t.Fatalf("attempts = %d, metrics = %v", e.sender.attempts, e.telemetry.events)
	}

	e.evaluator.set(onTime, nil)
	e.at("2026-09-29 07:00")
	e.sender.fail(permanentError{})
	e.evaluator.set(cancelled, nil)
	e.at("2026-09-29 07:05")
	e.sender.fail(nil)
	e.at("2026-09-29 07:06")
	e.wantSent(0)
	if status, reason := e.alertStatus(watchID, "2026-09-29", 1); status != "failed" || reason != "undeliverable" {
		t.Fatalf("refused alert = %s %s", status, reason)
	}
}

type permanentError struct{}

func (permanentError) Error() string   { return "Forbidden: bot was blocked by the user" }
func (permanentError) Permanent() bool { return true }

// Mute silences the watch for the rest of the day only, pause stops it until resumed, and an
// alert queued before either is dropped rather than sent. Another user can do neither.
func TestWatchMuteAndPause(t *testing.T) {
	e := newEngine(t)
	e.create("alice", watchID, departureSpec())
	e.evaluator.set(late, nil)
	e.at("2026-09-28 06:15")
	e.wantSent(1)

	for _, attempt := range []func() (bool, error){
		func() (bool, error) { return e.service.Mute(e.ctx, "bob", watchID) },
		func() (bool, error) { return e.service.SetEnabled(e.ctx, "bob", watchID, false) },
		func() (bool, error) { return e.service.Delete(e.ctx, "bob", watchID) },
	} {
		if changed, err := attempt(); err != nil || changed {
			t.Fatalf("bob changed alice's watch: %v %v", changed, err)
		}
	}
	if watches, err := e.service.Watches(e.ctx, "bob"); err != nil || len(watches) != 0 {
		t.Fatalf("bob's watches = %+v %v", watches, err)
	}

	if muted, err := e.service.Mute(e.ctx, "alice", watchID); err != nil || !muted {
		t.Fatalf("mute = %v %v", muted, err)
	}
	if muted, err := e.service.Mute(e.ctx, "alice", watchID); err != nil || !muted {
		t.Fatalf("muting twice = %v %v", muted, err)
	}
	e.evaluator.set(cancelled, nil)
	e.at("2026-09-28 06:25")
	e.wantSent(0)
	if calls, _ := e.evaluator.counts(); calls != 1 {
		t.Fatalf("a muted watch was checked: %d", calls)
	}

	e.at("2026-09-29 06:15")
	e.wantSent(1, "It is cancelled.")

	e.sender.fail(transientError{})
	e.evaluator.set(late, nil)
	e.at("2026-09-29 06:20")
	if paused, err := e.service.SetEnabled(e.ctx, "alice", watchID, false); err != nil || !paused {
		t.Fatalf("pause = %v %v", paused, err)
	}
	e.sender.fail(nil)
	e.at("2026-09-29 06:21")
	e.wantSent(0)
	if status, reason := e.alertStatus(watchID, "2026-09-29", 2); status != "dropped" || reason != "paused" {
		t.Fatalf("alert of a paused watch = %s %s", status, reason)
	}
	calls, _ := e.evaluator.counts()
	e.at("2026-09-29 06:30")
	if after, _ := e.evaluator.counts(); after != calls {
		t.Fatal("a paused watch was checked")
	}
}

// One window sends at most five messages however often the state flips.
func TestWatchSendsAtMostFiveMessagesPerWindow(t *testing.T) {
	e := newEngine(t)
	e.create("alice", watchID, departureSpec())
	for minute := 15; minute <= 55; minute += 5 {
		if minute%10 == 5 {
			e.evaluator.set(late, nil)
		} else {
			e.evaluator.set(onTime, nil)
		}
		e.at(fmt.Sprintf("2026-09-28 06:%02d", minute))
	}
	e.wantSent(5)
}

// Watches of a kind with no evaluator are left alone, logged once; a one-off watch expires after
// its window; identical queries within one cycle reach the upstream service once.
func TestWatchSkipsUnservedKindsExpiresOneOffsAndSharesQueries(t *testing.T) {
	e := newEngine(t)
	e.create("alice", watchID, departureSpec())
	e.create("bob", "0000000000000002", departureSpec())
	oneOff := departureSpec()
	oneOff.Days, oneOff.Date = nil, "2026-09-28"
	e.create("alice", "0000000000000003", oneOff)
	route := departureSpec()
	route.Kind, route.Target, route.Triggers = watch.KindRoute, watch.Target{Route: "work"}, []string{watch.TriggerLeg, watch.TriggerArrival}
	e.create("alice", "0000000000000004", route)

	e.at("2026-09-28 06:15")
	e.at("2026-09-28 06:20")
	if calls, fetches := e.evaluator.counts(); calls != 6 || fetches != 2 {
		t.Fatalf("calls = %d, fetches = %d; want 6 checks sharing one fetch per cycle", calls, fetches)
	}
	if count := strings.Count(e.logs.String(), "watch.kind.unavailable"); count != 1 {
		t.Fatalf("unavailable kind logged %d times", count)
	}

	e.at("2026-09-28 07:43")
	watches, err := e.service.Watches(e.ctx, "alice")
	if err != nil || len(watches) != 2 || watches[0].ID != watchID || watches[1].ID != "0000000000000004" {
		t.Fatalf("alice's watches after the one-off ended = %+v %v", watches, err)
	}
}

// fakeRewriter stands in for the model call that rewrites a message into the watch's language.
type fakeRewriter struct {
	mu    sync.Mutex
	calls []string
	reply func(text string) (string, error)
}

func (f *fakeRewriter) RewriteAlert(_ context.Context, principalID, language, text string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, principalID+" "+language)
	reply := f.reply
	f.mu.Unlock()
	return reply(text)
}

func (f *fakeRewriter) answer(reply func(text string) (string, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = reply
}

// byWatch keys alerts sent in one cycle by their watch, since their order follows the alert ids.
func byWatch(sent []sentAlert) map[string]string {
	texts := make(map[string]string, len(sent))
	for _, alert := range sent {
		texts[alert.watchID] = alert.text
	}
	return texts
}

// A watch with a language sends the rewrite when it keeps every number, and its English text when
// the rewrite changes a number or fails; a watch without one never reaches the model.
func TestWatchSendsMessagesInItsLanguageAndFallsBackToEnglish(t *testing.T) {
	e := newEngine(t)
	rewriter := &fakeRewriter{}
	e.service.SetRewriter(rewriter)
	russian := departureSpec()
	russian.Language = "Russian"
	e.create("alice", watchID, russian)
	const englishID = "0000000000000002"
	e.create("alice", englishID, departureSpec())

	rewriter.answer(func(string) (string, error) {
		return "**⚠️ 07:42 поезд Den Haag Centraal → Zwolle**\n\nОпаздывает на 8 минут.\nОжидается в 07:50.", nil
	})
	e.evaluator.set(late, nil)
	e.at("2026-09-28 06:25")
	sent := byWatch(e.wantSent(2))
	if sent[watchID] != "**⚠️ 07:42 поезд Den Haag Centraal → Zwolle**\n\nОпаздывает на 8 минут.\nОжидается в 07:50." {
		t.Fatalf("russian alert = %q", sent[watchID])
	}
	if !strings.Contains(sent[englishID], "It is 8 minutes late.") {
		t.Fatalf("english alert = %q", sent[englishID])
	}
	if notice := e.service.PendingNotice("alice"); !strings.Contains(notice, "Опаздывает на 8 минут.") {
		t.Fatalf("notice = %q, want the text that was sent", notice)
	}

	rewriter.answer(func(string) (string, error) {
		return "**✅ 07:43 поезд Den Haag Centraal → Zwolle**\n\nВсё в порядке.", nil
	})
	e.evaluator.set(onTime, nil)
	e.at("2026-09-28 06:40")
	sent = byWatch(e.wantSent(2))
	if !strings.Contains(sent[watchID], "07:42 train Den Haag Centraal → Zwolle") || !strings.Contains(sent[watchID], "Back to normal.") {
		t.Fatalf("a rewrite that changed the time was sent: %q", sent[watchID])
	}

	rewriter.answer(func(string) (string, error) { return "", errors.New("model unavailable") })
	e.evaluator.set(cancelled, nil)
	e.at("2026-09-28 06:45")
	sent = byWatch(e.wantSent(2))
	if !strings.Contains(sent[watchID], "It is cancelled.") {
		t.Fatalf("a failed rewrite did not fall back to English: %q", sent[watchID])
	}

	rewriter.mu.Lock()
	calls := strings.Join(rewriter.calls, ", ")
	rewriter.mu.Unlock()
	if calls != "alice Russian, alice Russian, alice Russian" {
		t.Fatalf("rewrites = %q, want three for the Russian watch only", calls)
	}
	for _, outcome := range []string{"rewritten", "rejected", "failed"} {
		if e.telemetry.count("rewrite alice "+outcome) != 1 {
			t.Fatalf("rewrite metrics = %v", e.telemetry.events)
		}
	}
}

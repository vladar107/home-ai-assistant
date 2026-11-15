package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// newFeedHarness runs Alice's chat with the scheduler, watches checked by the evaluator when one is
// given, and tabs on or off.
func newFeedHarness(t *testing.T, runtime agentruntime.AgentRuntime, evaluator watch.Evaluator, topics bool) (*conversationHarness, *testClock) {
	t.Helper()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, watches: evaluator, topics: topics})
	if evaluator != nil {
		if err := h.watches.Register(watch.KindHome, evaluator); err != nil {
			t.Fatal(err)
		}
	}
	h.provision("alice", "account-alice")
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	return h, clock
}

// frontDoorWatch is a Home Assistant watch whose window contains mondayMorning.
func frontDoorWatch() watch.Spec {
	return watch.Spec{
		Kind:     watch.KindHome,
		Target:   watch.Target{Entity: "binary_sensor.front_door", Name: "Front door"},
		Triggers: []string{watch.TriggerEnter},
		Days:     []string{"mon", "tue", "wed", "thu", "fri"},
		Start:    "07:00",
		End:      "08:30",
		Zone:     "Europe/Amsterdam",
	}
}

func (h *conversationHarness) notifier() TripNotifier {
	return TripNotifier{Client: h.client, Chats: h.store, Feeds: h.tabSet}
}

// wantFeedTab takes the next tab the bot made and checks it is the named feed, recorded as fixed.
func (h *conversationHarness) wantFeedTab(name, feed string) int64 {
	h.t.Helper()
	created := receive(h.t, h.ctx, h.tabs.created)
	if created.name != name {
		h.t.Fatalf("made tab %q, want %q", created.name, name)
	}
	// The tab is recorded once Telegram has answered its creation.
	for {
		threadID, found, err := h.store.SubjectTab(h.ctx, "alice", thread.FeedSubject(feed))
		if err != nil || (found && threadID != created.threadID) {
			h.t.Fatalf("registry has %s at %d %v %v, want %d", feed, threadID, found, err, created.threadID)
		}
		if found {
			break
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-h.ctx.Done():
			h.t.Fatalf("feed %s was never recorded", feed)
		}
	}
	if tab, _ := h.registryTab(created.threadID); tab.NamedBy != thread.NamedByFixed || tab.Name != name {
		h.t.Fatalf("feed tab in the registry = %+v", tab)
	}
	return created.threadID
}

func (h *conversationHarness) landedThread(query string, args ...any) int64 {
	h.t.Helper()
	var threadID int64
	if err := h.db.QueryRowContext(h.ctx, query, args...).Scan(&threadID); err != nil {
		h.t.Fatal(err)
	}
	return threadID
}

func TestEachKindOfMessageLandsInItsFeedTab(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	evaluator := &stubEvaluator{}
	evaluator.set(watch.Observation{State: "cancelled", Triggered: true, Headline: "It is cancelled."})
	h, clock := newFeedHarness(t, runtime, evaluator, true)

	h.createWatch("alice", "00000000000000a1", trainWatch())
	h.watches.Cycle(h.ctx)
	watches := h.wantFeedTab("Watches", thread.FeedWatches)
	if alert := h.nextMessage(); alert.threadID != watches || !strings.Contains(alert.text, "Utrecht Centraal") {
		t.Fatalf("departure alert = %+v, want it in tab %d", alert, watches)
	}
	h.createWatch("alice", "00000000000000b2", frontDoorWatch())
	h.watches.Cycle(h.ctx)
	home := h.wantFeedTab("Home", thread.FeedHome)
	if alert := h.nextMessage(); alert.threadID != home || !strings.Contains(alert.text, "Front door") {
		t.Fatalf("home alert = %+v, want it in tab %d", alert, home)
	}
	if landed := h.landedThread(`SELECT thread_id FROM watch_alert_messages WHERE watch_id = '00000000000000b2'`); landed != home {
		t.Fatalf("home alert recorded in tab %d", landed)
	}

	h.createReminder("alice", "00000000000000c3", "Take out the bins", "2026-09-28 08:05")
	clock.Set(t, "2026-09-28T08:05:10+02:00")
	h.scheduler.Wake()
	reminders := h.wantFeedTab("Reminders", thread.FeedReminders)
	if reminder := h.nextMessage(); reminder.threadID != reminders || !strings.Contains(reminder.text, "Take out the bins") {
		t.Fatalf("reminder = %+v, want it in tab %d", reminder, reminders)
	}
	h.waitRun("00000000000000c3", schedule.StatusDelivered)

	job := h.createJob("alice", "00000000000000d4", "Commute", "0 9 * * *", "Check my trains.")
	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	// The run has a route to its tab, found only when it sends, and stays out of that tab's
	// conversation.
	if delivery, set := thread.Resolved(run.ctx); !set || delivery != 0 || thread.From(run.ctx) != 0 || len(h.tabs.created) != 0 {
		t.Fatalf("run context before a send goes to %d (%v) as lane %d with %d tabs made, want an unfound route as the main lane", delivery, set, thread.From(run.ctx), len(h.tabs.created))
	}
	run.answer <- agentruntime.RunResult{Response: "All trains run.", Final: "All trains run."}
	reports := h.wantFeedTab("Reports", thread.FeedReports)
	if report := h.nextMessage(); report.threadID != reports {
		t.Fatalf("report = %+v, want it in tab %d", report, reports)
	}
	h.waitRun(job.ID, schedule.StatusDelivered)
	if landed := h.landedThread(`SELECT message.thread_id FROM job_report_messages AS message JOIN job_reports AS report ON report.id = message.report_id WHERE report.job_id = ?`, job.ID); landed != reports {
		t.Fatalf("report recorded in tab %d", landed)
	}

	if err := h.notifier().Send(h.ctx, "alice", "", "Your trip starts tomorrow."); err != nil {
		t.Fatal(err)
	}
	if notice := h.nextMessage(); notice.threadID != reminders {
		t.Fatalf("trip notice = %+v, want it in tab %d", notice, reminders)
	}

	// Deliveries of one feed at once make one tab.
	var wg sync.WaitGroup
	threads := make(chan int64, 8)
	for range 8 {
		wg.Go(func() {
			threadID, err := h.tabSet.Thread(h.ctx, "bob", 202, thread.FeedReports)
			if err != nil {
				t.Error(err)
			}
			threads <- threadID
		})
	}
	wg.Wait()
	close(threads)
	created := receive(t, h.ctx, h.tabs.created)
	for threadID := range threads {
		if threadID != created.threadID {
			t.Fatalf("a concurrent delivery got tab %d, want %d", threadID, created.threadID)
		}
	}
	if extra := len(h.tabs.created); created.chatID != 202 || extra != 0 {
		t.Fatalf("concurrent deliveries made %+v and %d more tabs", created, extra)
	}
	if strings.Contains(h.logs.String(), "Front door") || strings.Contains(h.logs.String(), "Take out the bins") {
		t.Fatalf("a message reached the logs:\n%s", h.logs.String())
	}
}

// A file a scheduled run sends lands in the run's feed tab, while the run stays out of that tab's
// conversation.
func TestAScheduledRunSendsItsFilesToItsFeedTab(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	roots := nasRootsFor(t, false)
	if err := os.WriteFile(filepath.Join(roots[0].Roots[0].Path, "Timetable.pdf"), []byte("%PDF-1.7 timetable"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: newTestClock(t, mondayMorning), nasRoots: roots, topics: true})
	h.provision("alice", "account-alice")
	h.start()
	job := h.createJob("alice", "00000000000000d4", "Commute", "0 9 * * *", "Send me the timetable.")
	if queued, err := h.scheduler.RunNow(h.ctx, "alice", job.ID, ""); err != nil || !queued {
		t.Fatalf("run now = %v %v", queued, err)
	}
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if thread.From(run.ctx) != 0 {
		t.Fatalf("the run is in lane %d, want the main lane", thread.From(run.ctx))
	}
	result, err := run.request.Tools.Executor.Execute(run.ctx, agentruntime.ToolCall{Name: "nas_send_file", Arguments: json.RawMessage(`{"path":"home/Timetable.pdf"}`)})
	if err != nil || result.IsError {
		t.Fatalf("send = %+v %v", result, err)
	}
	reports := h.wantFeedTab("Reports", thread.FeedReports)
	if document := h.nextDocument(); document.threadID != reports || document.name != "Timetable.pdf" {
		t.Fatalf("document = %+v, want it in tab %d", document, reports)
	}
	run.answer <- agentruntime.RunResult{Response: "Sent the timetable.", Final: "Sent the timetable."}
	if report := h.nextMessage(); report.threadID != reports {
		t.Fatalf("report = %+v, want it in tab %d", report, reports)
	}
	h.waitRun(job.ID, schedule.StatusDelivered)
}

func TestFeedTabsShowTheirIconWhenTelegramOffersIt(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	if err := h.notifier().Send(h.ctx, "alice", "", "first"); err != nil {
		t.Fatal(err)
	}
	reminders := h.wantFeedTab("Reminders", thread.FeedReminders)
	// The clock is offered with a variation selector; the feed asks for it without one.
	if edit := receive(t, h.ctx, h.tabs.topicEdits); edit != (editedTopic{threadID: reminders, iconID: "5000000000000000002", iconed: true}) {
		t.Fatalf("feed tab edit = %+v, want only its icon", edit)
	}
	if message := h.nextMessage(); message.threadID != reminders {
		t.Fatalf("message = %+v, want it in tab %d", message, reminders)
	}
	if tab, _ := h.registryTab(reminders); tab.IconCustomEmojiID != "5000000000000000002" || tab.Name != "Reminders" {
		t.Fatalf("feed tab in the registry = %+v", tab)
	}

	if err := h.notifier().Send(h.ctx, "alice", "", "second"); err != nil {
		t.Fatal(err)
	}
	h.nextMessage()
	// The Reports icon is not in the set Telegram offers here, so that tab is made without one.
	if _, err := h.tabSet.Thread(h.ctx, "alice", 101, thread.FeedReports); err != nil {
		t.Fatal(err)
	}
	h.wantFeedTab("Reports", thread.FeedReports)
	select {
	case edit := <-h.tabs.topicEdits:
		t.Fatalf("unexpected tab edit %+v", edit)
	default:
	}
}

// A message refused by a feed tab taken for gone makes the feed's new tab at once and lands there,
// where the next message goes too.
func TestAGoneFeedTabIsReplacedByTheMessageThatFoundItGone(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	if err := h.notifier().Send(h.ctx, "alice", "", "first"); err != nil {
		t.Fatal(err)
	}
	first := h.wantFeedTab("Reminders", thread.FeedReminders)
	h.nextMessage()

	h.goneTab(first)
	h.ageTab(first)
	if err := h.notifier().Send(h.ctx, "alice", "", "second"); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendMessage", first)
	again := h.wantFeedTab("Reminders", thread.FeedReminders)
	if message := h.nextMessage(); again == first || message.threadID != again || message.text != "second" {
		t.Fatalf("message to a gone feed tab = %+v, want it in a new tab, not %d", message, first)
	}
	if tab, _ := h.registryTab(again); !tab.Confirmed {
		t.Fatalf("the new tab took its message but is not confirmed: %+v", tab)
	}
	replaced := fmt.Sprintf(`msg=telegram.feed.replaced principal_id=alice gone_thread_id=%d thread_id=%d feed=reminders object=false moved_approvals=0 method=sendMessage error_code=400 description="Bad Request: message thread not found"`, first, again)
	if !strings.Contains(h.logs.String(), replaced) {
		t.Fatalf("replacement not logged as %s:\n%s", replaced, h.logs.String())
	}

	if err := h.notifier().Send(h.ctx, "alice", "", "third"); err != nil {
		t.Fatal(err)
	}
	if message := h.nextMessage(); message.threadID != again || message.text != "third" {
		t.Fatalf("message after the replacement = %+v, want it in tab %d", message, again)
	}
	if made := len(h.tabs.created); made != 0 {
		t.Fatalf("%d more tabs were made", made)
	}
}

// When a gone feed tab's replacement cannot be made, the message reaches the main view, and the next
// delivery makes the tab.
func TestAGoneFeedTabWhoseReplacementCannotBeMadeFallsBackToTheMainView(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	if err := h.notifier().Send(h.ctx, "alice", "", "first"); err != nil {
		t.Fatal(err)
	}
	first := h.wantFeedTab("Reminders", thread.FeedReminders)
	h.nextMessage()

	h.goneTab(first)
	h.ageTab(first)
	h.mu.Lock()
	h.tabs.createStatus = 429
	h.mu.Unlock()
	if err := h.notifier().Send(h.ctx, "alice", "", "second"); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendMessage", first)
	if message := h.nextMessage(); message.threadID != 0 || message.text != "second" {
		t.Fatalf("message without a replacement = %+v, want it in the main view", message)
	}

	h.mu.Lock()
	h.tabs.createStatus = 0
	h.mu.Unlock()
	if err := h.notifier().Send(h.ctx, "alice", "", "third"); err != nil {
		t.Fatal(err)
	}
	again := h.wantFeedTab("Reminders", thread.FeedReminders)
	if message := h.nextMessage(); message.threadID != again || message.text != "third" {
		t.Fatalf("next message = %+v, want it in the new tab %d", message, again)
	}
}

// A document whose body cannot be read again is not sent anywhere else when its tab is gone, but
// its route moves to the replacement, so the delivery's next message lands there.
func TestAGoneTabMovesTheRouteOfASendThatCannotBeMadeAgain(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	if err := h.notifier().Send(h.ctx, "alice", "", "first"); err != nil {
		t.Fatal(err)
	}
	first := h.wantFeedTab("Reminders", thread.FeedReminders)
	h.nextMessage()

	h.goneTab(first)
	h.ageTab(first)
	ctx := thread.WithRoute(h.ctx, h.tabSet.Route(h.ctx, "alice", 101, "", thread.FeedReminders))
	body := io.MultiReader(strings.NewReader("notes"))
	err := h.client.SendDocument(ctx, 101, "notes.txt", body, 5)
	if verdict, refused := tabRefusal(err); !refused || verdict != TabGone {
		t.Fatalf("document to a gone tab = %v, want its refusal as gone", err)
	}
	h.wantRefused("sendDocument", first)
	again := h.wantFeedTab("Reminders", thread.FeedReminders)
	if err := h.client.SendMessage(ctx, 101, "after the document"); err != nil {
		t.Fatal(err)
	}
	if message := h.nextMessage(); message.threadID != again || message.text != "after the document" {
		t.Fatalf("next message of the delivery = %+v, want it in the new tab %d", message, again)
	}
}

// Two deliveries that found the same feed tab before it was gone make one replacement, both
// landing there: both are refused before the first replacement is recorded.
func TestTwoDeliveriesToAGoneFeedTabMakeOneReplacement(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	if err := h.notifier().Send(h.ctx, "alice", "", "first"); err != nil {
		t.Fatal(err)
	}
	first := h.wantFeedTab("Reminders", thread.FeedReminders)
	h.nextMessage()

	contexts := make([]context.Context, 2)
	for index := range contexts {
		contexts[index] = thread.WithRoute(h.ctx, h.tabSet.Route(h.ctx, "alice", 101, "", thread.FeedReminders))
		if found, _ := thread.Delivery(contexts[index]); found != first {
			t.Fatalf("route %d found tab %d, want %d", index, found, first)
		}
	}
	h.goneTab(first)
	h.ageTab(first)
	hold := make(chan struct{})
	h.mu.Lock()
	h.tabs.createHold = hold
	h.mu.Unlock()
	sent := make(chan error, len(contexts))
	for index, ctx := range contexts {
		go func() { sent <- h.client.SendMessage(ctx, 101, fmt.Sprintf("delivery %d", index)) }()
	}
	h.wantRefused("sendMessage", first)
	h.wantRefused("sendMessage", first)
	created := receive(t, h.ctx, h.tabs.created)
	h.mu.Lock()
	h.tabs.createHold = nil
	h.mu.Unlock()
	close(hold)
	for range contexts {
		if err := receive(t, h.ctx, sent); err != nil {
			t.Fatal(err)
		}
	}
	for range contexts {
		if message := h.nextMessage(); message.threadID != created.threadID {
			t.Fatalf("message = %+v, want it in the one new tab %d", message, created.threadID)
		}
	}
	if made := len(h.tabs.created); made != 0 {
		t.Fatalf("two deliveries made %d more tabs", made)
	}
	if threadID, found, err := h.store.SubjectTab(h.ctx, "alice", thread.FeedSubject(thread.FeedReminders)); err != nil || !found || threadID != created.threadID {
		t.Fatalf("registry has the feed at %d %v %v, want %d", threadID, found, err, created.threadID)
	}
}

// A route finds its tab under the context of the send that needs it; a lookup that send's context
// ended is not kept, so the route's next send looks again.
func TestARouteLooksAgainAfterASendThatEndedDuringItsLookup(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	route := h.tabSet.Route(h.ctx, "alice", 101, "", thread.FeedReminders)
	hold := make(chan struct{})
	h.mu.Lock()
	h.tabs.createHold = hold
	h.mu.Unlock()
	sendCtx, cancel := context.WithCancel(thread.WithRoute(h.ctx, route))
	sent := make(chan error, 1)
	go func() { sent <- h.client.SendMessage(sendCtx, 101, "first") }()
	receive(t, h.ctx, h.tabs.created)
	cancel()
	if err := receive(t, h.ctx, sent); err == nil {
		t.Fatal("a send whose context ended was reported sent")
	}
	h.mu.Lock()
	h.tabs.createHold = nil
	h.mu.Unlock()
	close(hold)

	if err := h.client.SendMessage(thread.WithRoute(h.ctx, route), 101, "second"); err != nil {
		t.Fatal(err)
	}
	tab := h.wantFeedTab("Reminders", thread.FeedReminders)
	if message := h.nextMessage(); message.threadID != tab || message.text != "second" {
		t.Fatalf("message after a cancelled lookup = %+v, want it in the new tab %d", message, tab)
	}
}

// Asking which tab a trip's messages go to, as its language sample does, makes no tab.
func TestTheTripTabIsReadWithoutMakingOne(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	if threadID, err := h.notifier().TripTab(h.ctx, "alice", ""); err != nil || threadID != 0 {
		t.Fatalf("trip tab without a Reminders tab = %d %v, want the main view", threadID, err)
	}
	if made := len(h.tabs.created); made != 0 {
		t.Fatalf("reading the trip tab made %d tabs", made)
	}
	if err := h.notifier().Send(h.ctx, "alice", "", "first"); err != nil {
		t.Fatal(err)
	}
	reminders := h.wantFeedTab("Reminders", thread.FeedReminders)
	h.nextMessage()
	if threadID, err := h.notifier().TripTab(h.ctx, "alice", ""); err != nil || threadID != reminders {
		t.Fatalf("trip tab = %d %v, want the Reminders tab %d", threadID, err, reminders)
	}
}

// Telegram's own description of a refused send to a tab is logged, whatever the refusal, and the
// message never is.
func TestARefusedTabSendLogsTelegramsDescription(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	if err := h.notifier().Send(h.ctx, "alice", "", "first"); err != nil {
		t.Fatal(err)
	}
	tab := h.wantFeedTab("Reminders", thread.FeedReminders)
	h.nextMessage()

	h.refuseTab(tab, "Bad Request: TOPIC_CLOSED")
	if err := h.notifier().Send(h.ctx, "alice", "", "a private closed note"); err == nil {
		t.Fatal("a refused send reported success")
	}
	h.wantRefused("sendMessage", tab)
	refused := fmt.Sprintf(`msg=telegram.send.refused principal_id=alice thread_id=%d method=sendMessage error_code=400 description="Bad Request: TOPIC_CLOSED"`, tab)
	if !strings.Contains(h.logs.String(), refused) {
		t.Fatalf("refusal not logged as %s:\n%s", refused, h.logs.String())
	}

	h.restoreTab(tab)
	h.goneTab(tab)
	h.ageTab(tab)
	if err := h.notifier().Send(h.ctx, "alice", "", "a private gone note"); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendMessage", tab)
	h.nextMessage()
	gone := fmt.Sprintf(`msg=telegram.send.thread_gone principal_id=alice thread_id=%d method=sendMessage error_code=400 description="Bad Request: message thread not found"`, tab)
	if !strings.Contains(h.logs.String(), gone) {
		t.Fatalf("gone tab not logged as %s:\n%s", gone, h.logs.String())
	}
	if strings.Contains(h.logs.String(), "private") {
		t.Fatalf("a message reached the logs:\n%s", h.logs.String())
	}
}

func TestANewFeedTabThatRefusesSendsIsNotReplacedForADay(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	broken, err := h.tabSet.Thread(h.ctx, "alice", 101, thread.FeedReminders)
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.ctx, h.tabs.created)
	h.goneTab(broken)
	for _, text := range []string{"first", "second"} {
		if err := h.notifier().Send(h.ctx, "alice", "", text); err != nil {
			t.Fatal(err)
		}
		h.wantRefused("sendMessage", broken)
		if message := h.nextMessage(); message.threadID != 0 || message.text != text {
			t.Fatalf("message to a refusing new tab = %+v, want it in the main view", message)
		}
	}
	if _, found := h.registryTab(broken); !found || len(h.tabs.created) != 0 {
		t.Fatalf("a refusing new feed tab was replaced: kept=%v, %d new tabs", found, len(h.tabs.created))
	}
	if !strings.Contains(h.logs.String(), "msg=telegram.send.thread_gone_suspect principal_id=alice thread_id=") {
		t.Fatalf("suspect feed tab not logged:\n%s", h.logs.String())
	}
}

func TestAFeedTabRecordedByItsCreationNoticeFirstTakesItsMessage(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	made := h.makeTabAfterItsNotice(1, func() error { return h.notifier().Send(h.ctx, "alice", "", "first") })
	if message := h.nextMessage(); message.threadID != made || message.text != "first" {
		t.Fatalf("message = %+v, want it in the new tab %d", message, made)
	}
	if tab := h.wantFeedTab("Reminders", thread.FeedReminders); tab != made {
		t.Fatalf("reminders tab = %d, want %d", tab, made)
	}
	if strings.Contains(h.logs.String(), "msg=telegram.feed.failed") {
		t.Fatalf("making the feed tab failed:\n%s", h.logs.String())
	}
}

func TestAFeedTabThatCannotBeMadeLeavesTheMessageInTheMainView(t *testing.T) {
	t.Parallel()
	h, _ := newFeedHarness(t, newControlledRuntime(), nil, true)
	h.mu.Lock()
	h.tabs.createStatus = 429
	h.mu.Unlock()
	if err := h.notifier().Send(h.ctx, "alice", "", "first"); err != nil {
		t.Fatal(err)
	}
	if message := h.nextMessage(); message.threadID != 0 || message.text != "first" {
		t.Fatalf("message without a feed tab = %+v, want it in the main view", message)
	}
	if !strings.Contains(h.logs.String(), "msg=telegram.feed.failed principal_id=alice feed=reminders stage=create") {
		t.Fatalf("failed creation not logged:\n%s", h.logs.String())
	}

	h.mu.Lock()
	h.tabs.createStatus = 0
	h.mu.Unlock()
	if err := h.notifier().Send(h.ctx, "alice", "", "second"); err != nil {
		t.Fatal(err)
	}
	if tab := h.wantFeedTab("Reminders", thread.FeedReminders); h.nextMessage().threadID != tab {
		t.Fatal("the next message did not make the feed tab")
	}
}

func TestWithTopicsOffFeedsSendNoThread(t *testing.T) {
	t.Parallel()
	h, clock := newFeedHarness(t, newControlledRuntime(), nil, false)
	if err := h.notifier().Send(h.ctx, "alice", "", "a trip notice"); err != nil {
		t.Fatal(err)
	}
	if message := h.nextMessage(); message.threadID != 0 {
		t.Fatalf("trip notice with topics off = %+v", message)
	}
	h.createReminder("alice", "00000000000000c3", "Take out the bins", "2026-09-28 08:05")
	clock.Set(t, "2026-09-28T08:05:10+02:00")
	h.scheduler.Wake()
	if reminder := h.nextMessage(); reminder.threadID != 0 {
		t.Fatalf("reminder with topics off = %+v", reminder)
	}
}

// A reply in a feed tab to a report whose conversation another tab holds runs nothing and names
// that tab.
func TestAReplyToAConversationOpenElsewhereNamesItsTab(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h, _ := newFeedHarness(t, runtime, nil, true)
	reports, err := h.tabSet.Thread(h.ctx, "alice", 101, thread.FeedReports)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveJobReport(h.ctx, schedule.Report{
		ID: "00000000000000e5", PrincipalID: "alice", JobID: "00000000000000d4", JobName: "Commute", JobType: schedule.TypeModelTurn,
		Text: "All trains run.", DeliveredAt: time.Now(), Messages: []schedule.Sent{{MessageID: 9100, ThreadID: reports}},
	}, 200); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AddTab(h.ctx, "alice", 60, "Garden", thread.NamedByUser); err != nil {
		t.Fatal(err)
	}
	h.write(`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, thread_id, subject)
		 VALUES ('66666666-6666-4666-8666-666666666666', 'alice', '66666666-0000-4000-8000-000000000000', 60, ?)`, conversation.JobSubject("00000000000000d4"))

	update := tabMessage(1, reports, "and the second train?")
	update.Message.ReplyToMessage = &Message{MessageID: 9100, MessageThreadID: reports}
	h.push(update)
	if answer := h.nextMessage(); answer.threadID != reports || answer.text != SessionOpenIn(60, "Garden") || !strings.Contains(answer.text, "“Garden”") {
		t.Fatalf("answer = %+v, want %q in the feed tab", answer, SessionOpenIn(60, "Garden"))
	}

	// One the main view holds is named as the main view.
	h.write(`UPDATE conversation_sessions SET thread_id = 0 WHERE id = '66666666-6666-4666-8666-666666666666'`)
	update = tabMessage(2, reports, "and the third?")
	update.Message.ReplyToMessage = &Message{MessageID: 9100, MessageThreadID: reports}
	h.push(update)
	if answer := h.nextMessage(); answer.threadID != reports || answer.text != "That conversation is open in the main view. Write there to continue it." {
		t.Fatalf("answer = %+v, want it to name the main view", answer)
	}
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)
}

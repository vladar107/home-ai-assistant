package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/looseend"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

// Monday 30 November 2026, before the Loose ends run, in Amsterdam.
const looseEndsMorning = "2026-11-30T07:00:00+01:00"

// fakeLooseEndScan stands for the daily scan: it runs what the test sets and says whose scan ran.
type fakeLooseEndScan struct {
	mu   sync.Mutex
	next func(ctx context.Context, principalID string, now time.Time) error
	ran  chan string
}

func newFakeLooseEndScan() *fakeLooseEndScan {
	return &fakeLooseEndScan{ran: make(chan string, 16)}
}

func (f *fakeLooseEndScan) Scan(ctx context.Context, principalID string, now time.Time) error {
	f.mu.Lock()
	next := f.next
	f.mu.Unlock()
	var err error
	if next != nil {
		err = next(ctx, principalID, now)
	}
	f.ran <- principalID
	return err
}

func (f *fakeLooseEndScan) set(next func(ctx context.Context, principalID string, now time.Time) error) {
	f.mu.Lock()
	f.next = next
	f.mu.Unlock()
}

func newLooseEndsHarness(t *testing.T, scan *fakeLooseEndScan, clock *testClock) (*conversationHarness, *controlledRuntime) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, looseEndScanner: scan})
	h.provision("alice", "account-alice")
	h.start()
	return h, runtime
}

// enableLooseEnds turns Loose ends on for the principal as the settings page does, without the run
// it starts.
func (h *conversationHarness) enableLooseEnds(principalID string) string {
	h.t.Helper()
	if err := h.assembly.Switches.Toggle(looseend.Switch).SetEnabled(h.ctx, principalID, true); err != nil {
		h.t.Fatal(err)
	}
	prepared, err := h.scheduler.Prepare(principalID, schedule.LooseEndsName, schedule.LooseEndScanType, schedule.Timing{Cron: schedule.LooseEndsCron}, looseend.JobConfigJSON())
	if err != nil {
		h.t.Fatal(err)
	}
	id := schedule.LooseEndsJobID(principalID)
	if created, err := h.scheduler.Create(h.ctx, id, prepared); err != nil || !created {
		h.t.Fatalf("create the Loose ends job: created=%v err=%v", created, err)
	}
	return id
}

func (h *conversationHarness) trackLooseEnd(principalID string, draft looseend.Draft) looseend.Item {
	h.t.Helper()
	tracked, err := h.assembly.LooseEnds.Track(h.ctx, principalID, draft)
	if err != nil || tracked.Outcome != looseend.OutcomeCreated {
		h.t.Fatalf("track %+v: %+v %v", draft, tracked, err)
	}
	return tracked.Item
}

func (h *conversationHarness) looseEnd(principalID, id string) looseend.Item {
	h.t.Helper()
	item, found, err := h.assembly.LooseEnds.Get(h.ctx, principalID, id)
	if err != nil || !found {
		h.t.Fatalf("load loose end %s: found=%v err=%v", id, found, err)
	}
	return item
}

// runLooseEnds moves the clock to an instant and lets the scheduler run what is due.
func (h *conversationHarness) runLooseEndsAt(clock *testClock, at string) {
	h.t.Helper()
	clock.Set(h.t, at)
	h.scheduler.Wake()
}

// pressNudge presses a nudge button on the message as the user sees it now.
func (h *conversationHarness) pressNudge(updateID, userID, messageID int64, text string, keyboard InlineKeyboard, data string) editedMessage {
	h.t.Helper()
	update := callbackUpdate(updateID, userID, messageID, data)
	update.CallbackQuery.Message.Text = text
	update.CallbackQuery.Message.ReplyMarkup = &ReplyMarkup{InlineKeyboard: keyboard}
	h.push(update)
	h.wantAnswer(updateID)
	return h.nextEdit()
}

func mailLooseEnd(message string, kind looseend.Kind, title, due string) looseend.Draft {
	return looseend.Draft{
		Kind: kind, Title: title, Counterpart: "hotel.example", Due: due,
		Source: looseend.SourceMail, Account: "alice@example.com", SourceRef: message, ThreadRef: "thread-" + message, Outside: true,
	}
}

func chatLooseEnd(kind looseend.Kind, title, due string) looseend.Draft {
	return looseend.Draft{Kind: kind, Title: title, Due: due, Source: looseend.SourceChat}
}

func looseEndData(action, line, id string) string {
	return "loose:" + action + ":" + line + ":" + id
}

// The day's nudges go out before the scan and whatever the scan does: one message lists each item
// due with its buttons, is the job's report for later turns and Heads-up, and is outside content
// when an item in it came from someone else's text, so replying to it exposes the turn.
func TestLooseEndNudgesComeBeforeTheScanAndWhenItFails(t *testing.T) {
	t.Parallel()
	scan := newFakeLooseEndScan()
	clock := newTestClock(t, looseEndsMorning)
	h, runtime := newLooseEndsHarness(t, scan, clock)
	jobID := h.enableLooseEnds("alice")
	booking := h.trackLooseEnd("alice", mailLooseEnd("m1", looseend.KindCancelWindow, "Hotel booking", "2026-12-02"))
	h.trackLooseEnd("alice", chatLooseEnd(looseend.KindPromise, "Send the forms", "2026-12-04"))
	nudgedAtScan := -1
	scan.set(func(ctx context.Context, principalID string, _ time.Time) error {
		nudgedAtScan = h.countRows(`SELECT count(*) FROM loose_ends WHERE principal_id = ? AND nudged_on = '2026-11-30'`, principalID)
		return errors.New("the mail could not be read")
	})

	h.runLooseEndsAt(clock, "2026-11-30T07:20:10+01:00")
	nudge := h.nextMessage()
	wantText := "Loose ends\n• Free cancellation: Hotel booking (hotel.example), by Wed 2 Dec"
	wantButtons := []string{looseEndData("done", "1", booking.ID), looseEndData("tomorrow", "1", booking.ID), looseEndData("drop", "1", booking.ID)}
	if nudge.chatID != 101 || nudge.text != wantText || nudge.parseMode != "" || !slices.Equal(buttonData(nudge.keyboard), wantButtons) || nudge.silent {
		t.Fatalf("nudge = %+v", nudge)
	}
	if receive(t, h.ctx, scan.ran) != "alice" || nudgedAtScan != 1 {
		t.Fatalf("the scan saw %d nudged items, want the nudge recorded before it", nudgedAtScan)
	}
	failure := h.nextMessage()
	if !strings.Contains(failure.text, "Loose ends") || !strings.Contains(failure.text, schedule.FailedText) {
		t.Fatalf("failed scan report = %+v", failure)
	}
	h.waitRuns(jobID, schedule.StatusDelivered, 1)
	if got := h.countRows(`SELECT count(*) FROM job_reports r JOIN job_report_messages m ON m.report_id = r.id
		WHERE r.principal_id = 'alice' AND r.job_id = ? AND r.text = ? AND r.external = 1 AND m.telegram_message_id = ?`, jobID, wantText, nudge.messageID); got != 1 {
		t.Fatalf("nudge reports = %d, want the message kept as an outside report", got)
	}

	listed, err := h.scheduler.Execute(h.ctx, "alice", tools.Call{Name: "job_reports", Arguments: json.RawMessage(`{"job":"Loose ends"}`)})
	if err != nil || listed.IsError || !strings.Contains(listed.Text, `job "Loose ends"`) || !strings.Contains(listed.Text, "Free cancellation: Hotel booking") {
		t.Fatalf("job_reports = %+v %v", listed, err)
	}

	h.push(replyTo(textUpdate(1, 101, "what was this about?"), nudge.messageID))
	reply := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(reply.request.Message, `The user is replying to this report (report id `) || !strings.Contains(reply.request.Message, "Hotel booking (hotel.example)") {
		t.Fatalf("reply turn message = %q", reply.request.Message)
	}
	reply.answer <- agentruntime.RunResult{Response: "A booking you can cancel for free."}
	h.wantReply(101, 1, "A booking you can cancel for free.")
	h.waitState(1, "delivered")
	if count := h.countRows(`SELECT count(*) FROM conversation_sessions WHERE principal_id = 'alice' AND external_at IS NOT NULL`); count != 1 {
		t.Fatalf("external sessions after replying to the nudge = %d, want 1", count)
	}
}

// A nudge of items the user stated alone is not outside content.
func TestLooseEndNudgeOfTheUsersOwnItemsIsNotOutside(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, looseEndsMorning)
	h, _ := newLooseEndsHarness(t, newFakeLooseEndScan(), clock)
	jobID := h.enableLooseEnds("alice")
	h.trackLooseEnd("alice", chatLooseEnd(looseend.KindPromise, "Send the forms", "2026-11-30T17:00"))

	h.runLooseEndsAt(clock, "2026-11-30T07:20:10+01:00")
	nudge := h.nextMessage()
	if nudge.text != "Loose ends\n• Promise: Send the forms, by Mon 30 Nov 17:00" || len(buttonData(nudge.keyboard)) != 2 {
		t.Fatalf("nudge = %+v", nudge)
	}
	h.waitRuns(jobID, schedule.StatusSkipped, 1)
	if got := h.countRows(`SELECT count(*) FROM job_reports WHERE job_id = ? AND external = 0`, jobID); got != 1 {
		t.Fatalf("clean nudge reports = %d, want 1", got)
	}
}

// A nudge and its outcomes are written in the user's interface language; titles stay as found.
func TestLooseEndNudgeFollowsTheInterfaceLanguage(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, looseEndsMorning)
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{schedules: clock, looseEndScanner: newFakeLooseEndScan(), languages: true})
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.enableLooseEnds("alice")
	promise := h.trackLooseEnd("alice", chatLooseEnd(looseend.KindPromise, "Send the forms", "2026-11-30T17:00"))

	h.runLooseEndsAt(clock, "2026-11-30T07:20:10+01:00")
	nudge := h.nextMessage()
	if nudge.text != "Незакрытые дела\n• Обещание: Send the forms, до пн 30 ноя 17:00" || nudge.keyboard[0][0].Text != "Сделано" || nudge.keyboard[0][1].Text != "Не нужно" {
		t.Fatalf("nudge = %+v", nudge)
	}
	done := h.pressNudge(1, 101, nudge.messageID, nudge.text, nudge.keyboard, looseEndData("done", "1", promise.ID))
	if !strings.HasSuffix(done.text, "до пн 30 ноя 17:00 — сделано") {
		t.Fatalf("after Done = %+v", done)
	}
}

// Done, Tomorrow, and Drop change the principal's own item and edit its line; Tomorrow is not
// offered on the due day; a press on an item no longer open, or another user's, says so. A snoozed
// item comes back the next morning, and a morning with nothing due sends nothing.
func TestLooseEndButtonsChangeTheItemAndEditItsLine(t *testing.T) {
	t.Parallel()
	scan := newFakeLooseEndScan()
	clock := newTestClock(t, looseEndsMorning)
	h, _ := newLooseEndsHarness(t, scan, clock)
	jobID := h.enableLooseEnds("alice")
	booking := h.trackLooseEnd("alice", mailLooseEnd("m1", looseend.KindCancelWindow, "Hotel booking", "2026-12-02"))
	promise := h.trackLooseEnd("alice", chatLooseEnd(looseend.KindPromise, "Call the landlord", "2026-11-30T18:00"))
	order := h.trackLooseEnd("alice", chatLooseEnd(looseend.KindReturnWindow, "Return the jacket", "2026-12-03"))

	h.runLooseEndsAt(clock, "2026-11-30T07:20:10+01:00")
	nudge := h.nextMessage()
	wantText := "Loose ends\n" +
		"1. Promise: Call the landlord, by Mon 30 Nov 18:00\n" +
		"2. Free cancellation: Hotel booking (hotel.example), by Wed 2 Dec\n" +
		"3. Return window: Return the jacket, by Thu 3 Dec"
	if nudge.text != wantText {
		t.Fatalf("nudge text = %q", nudge.text)
	}
	wantButtons := []string{
		looseEndData("done", "1", promise.ID), looseEndData("drop", "1", promise.ID),
		looseEndData("done", "2", booking.ID), looseEndData("tomorrow", "2", booking.ID), looseEndData("drop", "2", booking.ID),
		looseEndData("done", "3", order.ID), looseEndData("tomorrow", "3", order.ID), looseEndData("drop", "3", order.ID),
	}
	if !slices.Equal(buttonData(nudge.keyboard), wantButtons) || len(nudge.keyboard) != 3 || nudge.keyboard[0][0].Text != "1. Done" {
		t.Fatalf("nudge buttons = %v", nudge.keyboard)
	}
	receive(t, h.ctx, scan.ran)
	h.waitRuns(jobID, schedule.StatusSkipped, 1)
	h.expectNoMessage(50 * time.Millisecond)

	done := h.pressNudge(1, 101, nudge.messageID, nudge.text, nudge.keyboard, looseEndData("done", "1", promise.ID))
	if !strings.Contains(done.text, "1. Promise: Call the landlord, by Mon 30 Nov 18:00 — done\n2. Free cancellation") || len(done.keyboard) != 2 {
		t.Fatalf("after Done = %+v", done)
	}
	if item := h.looseEnd("alice", promise.ID); item.Status != looseend.StatusDone {
		t.Fatalf("promise after Done = %+v", item)
	}
	later := h.pressNudge(2, 101, nudge.messageID, done.text, done.keyboard, looseEndData("tomorrow", "2", booking.ID))
	if !strings.Contains(later.text, "Hotel booking (hotel.example), by Wed 2 Dec — again tomorrow\n") || len(later.keyboard) != 1 {
		t.Fatalf("after Tomorrow = %+v", later)
	}
	if item := h.looseEnd("alice", booking.ID); item.Status != looseend.StatusOpen || item.NudgeOn != "2026-12-01" {
		t.Fatalf("booking after Tomorrow = %+v", item)
	}
	dropped := h.pressNudge(3, 101, nudge.messageID, later.text, later.keyboard, looseEndData("drop", "3", order.ID))
	if !strings.HasSuffix(dropped.text, "Return the jacket, by Thu 3 Dec — dropped") || len(dropped.keyboard) != 0 {
		t.Fatalf("after Drop = %+v", dropped)
	}
	if item := h.looseEnd("alice", order.ID); item.Status != looseend.StatusDropped {
		t.Fatalf("order after Drop = %+v", item)
	}
	stale := h.pressNudge(4, 101, nudge.messageID, nudge.text, nudge.keyboard, looseEndData("done", "3", order.ID))
	if !strings.HasSuffix(stale.text, "Return the jacket, by Thu 3 Dec — "+localetest.English(LooseEndGoneOutcome)) {
		t.Fatalf("press on a dropped item = %+v", stale)
	}
	if item := h.looseEnd("alice", order.ID); item.Status != looseend.StatusDropped {
		t.Fatalf("a press on a dropped item changed it: %+v", item)
	}
	foreign := h.pressNudge(5, 202, nudge.messageID, nudge.text, nudge.keyboard, looseEndData("done", "2", booking.ID))
	if !strings.Contains(foreign.text, "by Wed 2 Dec — "+localetest.English(LooseEndGoneOutcome)) {
		t.Fatalf("bob's press = %+v", foreign)
	}
	if item := h.looseEnd("alice", booking.ID); item.Status != looseend.StatusOpen {
		t.Fatalf("bob closed alice's item: %+v", item)
	}

	h.runLooseEndsAt(clock, "2026-12-01T07:20:10+01:00")
	again := h.nextMessage()
	if again.text != "Loose ends\n• Free cancellation: Hotel booking (hotel.example), by Wed 2 Dec" || len(buttonData(again.keyboard)) != 3 {
		t.Fatalf("snoozed item's nudge = %+v", again)
	}
	receive(t, h.ctx, scan.ran)

	h.runLooseEndsAt(clock, "2026-12-02T07:20:10+01:00")
	receive(t, h.ctx, scan.ran)
	h.waitRuns(jobID, schedule.StatusSkipped, 3)
	h.expectNoMessage(50 * time.Millisecond)
	// Tomorrow pressed on an earlier nudge on the due day keeps Done and Drop.
	today := h.pressNudge(6, 101, again.messageID, again.text, again.keyboard, looseEndData("tomorrow", "1", booking.ID))
	if !strings.HasSuffix(today.text, "— due today, so it cannot wait until tomorrow") ||
		!slices.Equal(buttonData(today.keyboard), []string{looseEndData("done", "1", booking.ID), looseEndData("drop", "1", booking.ID)}) {
		t.Fatalf("Tomorrow on the due day = %+v", today)
	}
}

// A missed morning is caught up the next; an expiry is nudged a month and a week ahead; an item
// past its due time expires without a word, a date without a time at the end of its local day.
func TestLooseEndsCatchUpNudgeExpiriesTwiceAndExpireSilently(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, "2026-11-30T12:00:00+01:00")
	h, _ := newLooseEndsHarness(t, newFakeLooseEndScan(), clock)
	jobID := h.enableLooseEnds("alice")
	passport := h.trackLooseEnd("alice", chatLooseEnd(looseend.KindExpiry, "Passport", "2026-12-31"))
	booking := h.trackLooseEnd("alice", mailLooseEnd("m1", looseend.KindCancelWindow, "Hotel booking", "2026-12-04"))
	promise := h.trackLooseEnd("alice", chatLooseEnd(looseend.KindPromise, "Send the forms", "2026-12-24"))

	h.runLooseEndsAt(clock, "2026-12-01T07:20:10+01:00")
	if first := h.nextMessage(); first.text != "Loose ends\n• Expires: Passport, by Thu 31 Dec" {
		t.Fatalf("first expiry nudge = %+v", first)
	}
	h.waitRuns(jobID, schedule.StatusSkipped, 1)

	// The booking's morning, 2 December, is missed.
	h.runLooseEndsAt(clock, "2026-12-03T07:20:10+01:00")
	if caught := h.nextMessage(); caught.text != "Loose ends\n• Free cancellation: Hotel booking (hotel.example), by Fri 4 Dec" {
		t.Fatalf("caught-up nudge = %+v", caught)
	}
	h.waitRuns(jobID, schedule.StatusSkipped, 2)

	h.runLooseEndsAt(clock, "2026-12-24T07:20:10+01:00")
	if second := h.nextMessage(); second.text != "Loose ends\n1. Promise: Send the forms, by Thu 24 Dec\n2. Expires: Passport, by Thu 31 Dec" {
		t.Fatalf("second expiry nudge = %+v", second)
	}
	h.waitRuns(jobID, schedule.StatusSkipped, 3)
	if item := h.looseEnd("alice", booking.ID); item.Status != looseend.StatusExpired {
		t.Fatalf("booking past its deadline = %+v", item)
	}

	clock.Set(t, "2026-12-24T23:59:00+01:00")
	if queued, err := h.scheduler.RunNow(h.ctx, "alice", jobID, ""); err != nil || !queued {
		t.Fatalf("run now = %v %v", queued, err)
	}
	h.scheduler.Wake()
	if late := h.nextMessage(); !strings.Contains(late.text, schedule.QuietManualText) {
		t.Fatalf("late manual run = %+v", late)
	}
	if item := h.looseEnd("alice", promise.ID); item.Status != looseend.StatusOpen {
		t.Fatalf("promise before the end of its day = %+v", item)
	}
	h.runLooseEndsAt(clock, "2026-12-25T07:20:10+01:00")
	h.waitRuns(jobID, schedule.StatusSkipped, 4)
	h.expectNoMessage(50 * time.Millisecond)
	if item := h.looseEnd("alice", promise.ID); item.Status != looseend.StatusExpired {
		t.Fatalf("promise after its day = %+v", item)
	}
	if item := h.looseEnd("alice", passport.ID); item.Status != looseend.StatusOpen || item.NudgedOn != "2026-12-24" {
		t.Fatalf("passport after two nudges = %+v", item)
	}
}

// Nudge days are local days across a daylight-saving change.
func TestLooseEndNudgeDaysAcrossDaylightSavingChange(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, "2026-10-23T12:00:00+02:00")
	h, _ := newLooseEndsHarness(t, newFakeLooseEndScan(), clock)
	jobID := h.enableLooseEnds("alice")
	// Clocks go back on Sunday 25 October; both items are due on the Monday after.
	h.trackLooseEnd("alice", mailLooseEnd("m1", looseend.KindCancelWindow, "Hotel booking", "2026-10-26"))
	h.trackLooseEnd("alice", chatLooseEnd(looseend.KindPromise, "Send the forms", "2026-10-26T08:00"))

	h.runLooseEndsAt(clock, "2026-10-24T07:20:10+02:00")
	if saturday := h.nextMessage(); saturday.text != "Loose ends\n• Free cancellation: Hotel booking (hotel.example), by Mon 26 Oct" {
		t.Fatalf("Saturday's nudge = %+v", saturday)
	}
	h.runLooseEndsAt(clock, "2026-10-25T07:20:10+01:00")
	h.waitRuns(jobID, schedule.StatusSkipped, 2)
	h.expectNoMessage(50 * time.Millisecond)
	h.runLooseEndsAt(clock, "2026-10-26T07:20:10+01:00")
	if monday := h.nextMessage(); monday.text != "Loose ends\n• Promise: Send the forms, by Mon 26 Oct 08:00" {
		t.Fatalf("Monday's nudge = %+v", monday)
	}
}

// What the scan finds inside its lead time is nudged in a second, short message the same morning;
// what it finds further ahead waits. Heads-up reads both nudges through job_reports with its
// read-only tools, which leave the items as they are.
func TestLooseEndScanFindingsGetASecondNudgeThatHeadsUpSees(t *testing.T) {
	t.Parallel()
	scan := newFakeLooseEndScan()
	clock := newTestClock(t, looseEndsMorning)
	h, runtime := newLooseEndsHarness(t, scan, clock)
	jobID := h.enableLooseEnds("alice")
	h.trackLooseEnd("alice", chatLooseEnd(looseend.KindPromise, "Send the forms", "2026-11-30T17:00"))
	var found []string
	scan.set(func(ctx context.Context, principalID string, _ time.Time) error {
		batch, err := h.assembly.LooseEnds.TrackBatch(ctx, principalID, []looseend.Draft{
			mailLooseEnd("m2", looseend.KindCancelWindow, "Table for two", "2026-12-01"),
			mailLooseEnd("m3", looseend.KindRenewal, "Streaming plan", "2026-12-10"),
		})
		for _, tracked := range batch.Tracked {
			found = append(found, tracked.ID)
		}
		return err
	})

	h.runLooseEndsAt(clock, "2026-11-30T07:20:10+01:00")
	first := h.nextMessage()
	second := h.nextMessage()
	if first.text != "Loose ends\n• Promise: Send the forms, by Mon 30 Nov 17:00" {
		t.Fatalf("first nudge = %+v", first)
	}
	receive(t, h.ctx, scan.ran)
	if second.text != "Loose ends\n• Free cancellation: Table for two (hotel.example), by Tue 1 Dec" || len(found) != 2 ||
		!slices.Equal(buttonData(second.keyboard), []string{looseEndData("done", "1", found[0]), looseEndData("tomorrow", "1", found[0]), looseEndData("drop", "1", found[0])}) {
		t.Fatalf("second nudge = %+v", second)
	}
	h.waitRuns(jobID, schedule.StatusSkipped, 1)
	if got := h.countRows(`SELECT count(*) FROM job_reports WHERE job_id = ?`, jobID); got != 2 {
		t.Fatalf("nudge reports = %d, want 2", got)
	}

	headsUp := h.createJob("alice", schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, schedule.HeadsUpCron, schedule.HeadsUpNotes)
	h.runLooseEndsAt(clock, "2026-11-30T07:30:20+01:00")
	run := receive(t, h.ctx, runtime.calls)
	names := toolNames(run.request.Tools)
	if !slices.Contains(names, "job_reports") || slices.Contains(names, "reminder_set") || slices.Contains(names, "schedule_propose") {
		t.Fatalf("Heads-up tools = %v", names)
	}
	listed := h.callTool(run, "job_reports", `{}`)
	if listed.IsError || !strings.Contains(listed.Text, `job "Loose ends"`) || !strings.Contains(listed.Text, "Promise: Send the forms") || !strings.Contains(listed.Text, "Table for two") {
		t.Fatalf("Heads-up job_reports = %+v", listed)
	}
	run.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd, Final: prompt.NothingToAdd}
	h.waitRuns(headsUp.ID, schedule.StatusSkipped, 1)
	if got := h.countRows(`SELECT count(*) FROM loose_ends WHERE principal_id = 'alice' AND status = 'open'`); got != 3 {
		t.Fatalf("open items after Heads-up = %d, want 3", got)
	}

	scan.set(nil)
	h.runLooseEndsAt(clock, "2026-12-01T07:20:10+01:00")
	receive(t, h.ctx, scan.ran)
	h.waitRuns(jobID, schedule.StatusSkipped, 2)
	h.expectNoMessage(50 * time.Millisecond)
}

// The switch is off by default. Turning it on creates the principal's own job, starts the scan
// state, and checks at once; Check now runs it again; turning it off disables the job and forgets
// the scan state, keeps the items, and leaves no run.
func TestLooseEndsSwitchManagesItsJob(t *testing.T) {
	t.Parallel()
	scan := newFakeLooseEndScan()
	clock := newTestClock(t, looseEndsMorning)
	h, _ := newLooseEndsHarness(t, scan, clock)
	jobID := schedule.LooseEndsJobID("alice")

	h.requestSettings(1, 101)
	page := h.openSettingsPage(2, 101, settingsMessageID(1), "tools")
	if !strings.Contains(page.text, localetest.English(LooseEndsOffLine)) || !slices.Contains(buttonData(page.keyboard), "looseends:on") {
		t.Fatalf("page while off = %+v", page)
	}
	if h.countRows(`SELECT count(*) FROM scheduled_jobs`) != 0 {
		t.Fatal("a job exists before the switch is on")
	}
	h.trackLooseEnd("alice", chatLooseEnd(looseend.KindPromise, "Send the forms", "2026-11-30T17:00"))

	page = h.press(3, 101, settingsMessageID(1), "looseends:on")
	if !strings.Contains(page.text, localetest.English(LooseEndsOnLine)) || !strings.Contains(page.text, localetest.English(LooseEndsCheckingNote)) ||
		!slices.Contains(buttonData(page.keyboard), "looseends:check") || !slices.Contains(buttonData(page.keyboard), "looseends:off") {
		t.Fatalf("page after on = %+v", page)
	}
	if nudge := h.nextMessage(); nudge.text != "Loose ends\n• Promise: Send the forms, by Mon 30 Nov 17:00" {
		t.Fatalf("nudge of the first check = %+v", nudge)
	}
	receive(t, h.ctx, scan.ran)
	if checked := h.nextMessage(); !strings.Contains(checked.text, "Checked your loose ends") {
		t.Fatalf("first check report = %+v", checked)
	}
	jobs, err := h.scheduler.Jobs(h.ctx, "alice")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	if job := jobs[0]; job.ID != jobID || job.Type != schedule.LooseEndScanType || job.Cron != schedule.LooseEndsCron || job.Name != schedule.LooseEndsName || !job.Enabled || job.Zone != "Europe/Amsterdam" {
		t.Fatalf("job = %+v", job)
	}
	if h.countRows(`SELECT count(*) FROM loose_end_scans WHERE principal_id = 'alice' AND backfill_until IS NOT NULL`) != 1 {
		t.Fatal("turning Loose ends on started no scan state")
	}
	if bobs, err := h.scheduler.Jobs(h.ctx, "bob"); err != nil || len(bobs) != 0 {
		t.Fatalf("bob's jobs = %+v %v", bobs, err)
	}

	// A run is one per job and instant, so the second check comes a minute later.
	clock.Set(t, "2026-11-30T07:01:00+01:00")
	if page = h.press(4, 101, settingsMessageID(1), "looseends:check"); !strings.Contains(page.text, localetest.English(LooseEndsCheckingNote)) {
		t.Fatalf("page after Check now = %+v", page)
	}
	receive(t, h.ctx, scan.ran)
	if quiet := h.nextMessage(); !strings.Contains(quiet.text, schedule.QuietManualText) {
		t.Fatalf("second check report = %+v", quiet)
	}

	if page = h.press(5, 101, settingsMessageID(1), "looseends:off"); !strings.Contains(page.text, localetest.English(LooseEndsOffLine)) {
		t.Fatalf("page after off = %+v", page)
	}
	if h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE id = ? AND enabled = 0`, jobID) != 1 || h.countRows(`SELECT count(*) FROM loose_end_scans`) != 0 ||
		h.countRows(`SELECT count(*) FROM loose_ends WHERE principal_id = 'alice'`) != 1 {
		t.Fatal("turning Loose ends off left the job enabled or the scan state, or dropped the items")
	}
	h.press(6, 101, settingsMessageID(1), "looseends:check")
	h.runLooseEndsAt(clock, "2026-12-01T07:20:10+01:00")
	h.expectNoMessage(100 * time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM schedule_runs WHERE job_id = ?`, jobID); count != 2 {
		t.Fatalf("runs = %d, want only the two checks", count)
	}

	// A job resumed in /jobs while the switch is off runs and sends nothing.
	if _, err := h.scheduler.SetEnabled(h.ctx, "alice", jobID, true); err != nil {
		t.Fatal(err)
	}
	h.trackLooseEnd("alice", chatLooseEnd(looseend.KindPromise, "Pay the plumber", "2026-12-02T17:00"))
	h.runLooseEndsAt(clock, "2026-12-02T07:20:10+01:00")
	h.waitRuns(jobID, schedule.StatusSkipped, 1)
	h.expectNoMessage(50 * time.Millisecond)

	if page = h.press(7, 101, settingsMessageID(1), "looseends:on"); !strings.Contains(page.text, localetest.English(LooseEndsOnLine)) {
		t.Fatalf("page after on again = %+v", page)
	}
	if nudge := h.nextMessage(); nudge.text != "Loose ends\n• Promise: Pay the plumber, by Wed 2 Dec 17:00" {
		t.Fatalf("nudge after on again = %+v", nudge)
	}
	receive(t, h.ctx, scan.ran)
	h.nextMessage()
	if h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE id = ? AND enabled = 1`, jobID) != 1 || h.countRows(`SELECT count(*) FROM loose_end_scans`) != 1 {
		t.Fatal("turning Loose ends on again left the job paused or no scan state")
	}
}

// An item whose nudge day is its due day is sent without Tomorrow.
func TestLooseEndTomorrowIsAbsentWhenTheNudgeDayIsTheDueDay(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, looseEndsMorning)
	h, _ := newLooseEndsHarness(t, newFakeLooseEndScan(), clock)
	h.enableLooseEnds("alice")
	// Both were found inside their lead time, so they are nudged on the day they are due.
	jacket := h.trackLooseEnd("alice", chatLooseEnd(looseend.KindReturnWindow, "Return the jacket", "2026-11-30"))
	booking := h.trackLooseEnd("alice", mailLooseEnd("m1", looseend.KindCancelWindow, "Hotel booking", "2026-11-30T20:00"))

	h.runLooseEndsAt(clock, "2026-11-30T07:20:10+01:00")
	nudge := h.nextMessage()
	want := []string{
		looseEndData("done", "1", booking.ID), looseEndData("drop", "1", booking.ID),
		looseEndData("done", "2", jacket.ID), looseEndData("drop", "2", jacket.ID),
	}
	if nudge.text != "Loose ends\n1. Free cancellation: Hotel booking (hotel.example), by Mon 30 Nov 20:00\n2. Return window: Return the jacket, by Mon 30 Nov" ||
		!slices.Equal(buttonData(nudge.keyboard), want) {
		t.Fatalf("nudge = %+v", nudge)
	}
}

// A failed scan says what failed: the turn's own failure, or that new mail it could not read twice
// in a row was skipped, with when.
func TestLooseEndScanFailureSaysWhatHappened(t *testing.T) {
	t.Parallel()
	scan := newFakeLooseEndScan()
	clock := newTestClock(t, looseEndsMorning)
	h, _ := newLooseEndsHarness(t, scan, clock)
	jobID := h.enableLooseEnds("alice")
	timeout := turn.TimeoutReply.In(locale.English)
	scan.set(func(context.Context, string, time.Time) error {
		return &looseend.FailedError{Kind: "timeout", Text: timeout}
	})

	h.runLooseEndsAt(clock, "2026-11-30T07:20:10+01:00")
	if failed := h.nextMessage(); !strings.Contains(failed.text, timeout) {
		t.Fatalf("turn failure report = %+v", failed)
	}
	h.waitRuns(jobID, schedule.StatusDelivered, 1)
	if got := h.countRows(`SELECT count(*) FROM schedule_runs WHERE job_id = ? AND error_kind = 'timeout'`, jobID); got != 1 {
		t.Fatalf("runs that failed on a timeout = %d, want 1", got)
	}

	from := time.Date(2026, 11, 29, 6, 20, 0, 0, time.UTC)
	until := time.Date(2026, 12, 1, 6, 20, 10, 0, time.UTC)
	scan.set(func(context.Context, string, time.Time) error {
		return &looseend.FailedError{Kind: "unreadable", SkippedFrom: from, SkippedUntil: until}
	})
	h.runLooseEndsAt(clock, "2026-12-01T07:20:10+01:00")
	if skipped := h.nextMessage(); !strings.Contains(skipped.text, "Two Loose ends checks in a row could not read your new mail from Sun 29 Nov 07:20 to Tue 1 Dec 07:20") {
		t.Fatalf("skipped mail report = %+v", skipped)
	}
}

// newSentPromiseHarness runs the Loose ends job with alice's mailbox readable, and tracks a promise
// she made in a sent message of the thread, due on the morning of the run.
func newSentPromiseHarness(t *testing.T, sim *googlesim.Simulator, thread string) (*conversationHarness, *testClock, string, looseend.Item) {
	t.Helper()
	clock := newTestClock(t, looseEndsMorning)
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{schedules: clock, looseEndScanner: newFakeLooseEndScan(), google: sim})
	h.provision("alice", "account-alice")
	h.start()
	h.connectGoogle(sim, 1, 101, aliceMailbox, googlesim.Grant{Mail: true})
	jobID := h.enableLooseEnds("alice")
	promise := h.trackLooseEnd("alice", looseend.Draft{
		Kind: looseend.KindPromise, Title: "Send the signed form to Alex", Counterpart: "partner.example", Due: "2026-11-30",
		Source: looseend.SourceSent, Account: aliceMailbox, SourceRef: "p1", ThreadRef: thread,
	})
	return h, clock, jobID, promise
}

func sentPromiseThread(thread string) []googlesim.Message {
	at := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC) }
	return []googlesim.Message{
		{ID: "p1", ThreadID: thread, From: aliceMailbox, To: "Alex <alex@partner.example>", Subject: "Re: The form", Labels: []string{"SENT"},
			Received: at(27, 9), PlainText: "I'll send you the signed form on Monday."},
		{ID: "r1", ThreadID: thread, From: "Alex <alex@partner.example>", To: aliceMailbox, Subject: "Re: The form", Labels: []string{"INBOX"},
			Received: at(27, 10), PlainText: "Thanks, Monday works."},
		{ID: "p2", ThreadID: thread, From: aliceMailbox, To: "Alex <alex@partner.example>", Subject: "Re: The form", Labels: []string{"SENT"},
			Received: at(28, 8), PlainText: "You're welcome!"},
	}
}

// A later message the user sent in a sent promise's thread, such as a thanks, is said on its line,
// and the promise is still nudged as outside content, since a thanks keeps no promise.
func TestLooseEndSentPromiseSaysTheUserWroteAgain(t *testing.T) {
	t.Parallel()
	sim := googlesim.New(t)
	sim.AddMailbox(aliceMailbox, sentPromiseThread("t1")...)
	h, clock, jobID, promise := newSentPromiseHarness(t, sim, "t1")

	h.runLooseEndsAt(clock, "2026-11-30T07:20:10+01:00")
	nudge := h.nextMessage()
	if nudge.text != "Loose ends\n• Promise: Send the signed form to Alex (partner.example), by Mon 30 Nov; you wrote again on Sat 28 Nov" {
		t.Fatalf("nudge = %+v", nudge)
	}
	h.waitRuns(jobID, schedule.StatusSkipped, 1)
	if got := h.looseEnd("alice", promise.ID); got.Status != looseend.StatusOpen || got.NudgedOn != "2026-11-30" {
		t.Fatalf("promise after the nudge = %+v", got)
	}
	if got := h.countRows(`SELECT count(*) FROM job_reports WHERE principal_id = 'alice' AND external = 1 AND text = ?`, nudge.text); got != 1 {
		t.Fatalf("outside nudge reports = %d, want 1", got)
	}
}

// A sent promise whose thread cannot be read is still nudged, without the note.
func TestLooseEndSentPromiseWithAnUnreadableThreadIsStillNudged(t *testing.T) {
	t.Parallel()
	sim := googlesim.New(t)
	sim.AddMailbox(aliceMailbox, sentPromiseThread("t1")...)
	sim.FailThread("t1", http.StatusForbidden)
	h, clock, jobID, promise := newSentPromiseHarness(t, sim, "t1")

	h.runLooseEndsAt(clock, "2026-11-30T07:20:10+01:00")
	nudge := h.nextMessage()
	if nudge.text != "Loose ends\n• Promise: Send the signed form to Alex (partner.example), by Mon 30 Nov" {
		t.Fatalf("nudge = %+v", nudge)
	}
	h.waitRuns(jobID, schedule.StatusSkipped, 1)
	if got := h.looseEnd("alice", promise.ID); got.NudgedOn != "2026-11-30" {
		t.Fatalf("promise after the nudge = %+v", got)
	}
}

// Check now with the switch on but neither the job nor the scan state there, as a restore can
// leave them, creates both and runs the check.
func TestLooseEndsCheckNowRepairsAMissingJobAndScanState(t *testing.T) {
	t.Parallel()
	scan := newFakeLooseEndScan()
	clock := newTestClock(t, looseEndsMorning)
	h, _ := newLooseEndsHarness(t, scan, clock)
	jobID := schedule.LooseEndsJobID("alice")
	if err := h.assembly.Switches.Toggle(looseend.Switch).SetEnabled(h.ctx, "alice", true); err != nil {
		t.Fatal(err)
	}

	h.requestSettings(1, 101)
	h.openSettingsPage(2, 101, settingsMessageID(1), "tools")
	if page := h.press(3, 101, settingsMessageID(1), "looseends:check"); !strings.Contains(page.text, localetest.English(LooseEndsCheckingNote)) {
		t.Fatalf("page after Check now = %+v", page)
	}
	receive(t, h.ctx, scan.ran)
	if quiet := h.nextMessage(); !strings.Contains(quiet.text, schedule.QuietManualText) {
		t.Fatalf("check report = %+v", quiet)
	}
	if h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE id = ? AND enabled = 1`, jobID) != 1 ||
		h.countRows(`SELECT count(*) FROM loose_end_scans WHERE principal_id = 'alice' AND backfill_until IS NOT NULL`) != 1 {
		t.Fatal("Check now left the job or the scan state missing")
	}
}

// A promise due before the next morning's run is nudged the morning before, without Tomorrow.
func TestLooseEndDueBeforeTheMorningRunIsNudgedTheDayBefore(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, looseEndsMorning)
	h, _ := newLooseEndsHarness(t, newFakeLooseEndScan(), clock)
	h.enableLooseEnds("alice")
	keys := h.trackLooseEnd("alice", chatLooseEnd(looseend.KindPromise, "Drop off the keys", "2026-12-01T06:00"))

	h.runLooseEndsAt(clock, "2026-11-30T07:20:10+01:00")
	nudge := h.nextMessage()
	if nudge.text != "Loose ends\n• Promise: Drop off the keys, by Tue 1 Dec 06:00" ||
		!slices.Equal(buttonData(nudge.keyboard), []string{looseEndData("done", "1", keys.ID), looseEndData("drop", "1", keys.ID)}) {
		t.Fatalf("nudge = %+v", nudge)
	}
}

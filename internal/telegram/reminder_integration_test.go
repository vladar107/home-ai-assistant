package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// createReminder stores a reminder for the principal directly, as reminder_set does.
func (h *conversationHarness) createReminder(principalID, id, text, at string) {
	h.t.Helper()
	config, err := schedule.ReminderConfig(text)
	if err != nil {
		h.t.Fatal(err)
	}
	prepared, err := h.scheduler.Prepare(principalID, schedule.ReminderName(text), schedule.TypeReminder, schedule.Timing{At: at}, config)
	if err != nil {
		h.t.Fatal(err)
	}
	if created, err := h.scheduler.Create(h.ctx, id, prepared); err != nil || !created {
		h.t.Fatalf("create reminder: created=%v err=%v", created, err)
	}
}

func (h *conversationHarness) expectNoMessage(wait time.Duration) {
	h.t.Helper()
	select {
	case message := <-h.messages:
		h.t.Fatalf("unexpected message %+v", message)
	case <-time.After(wait):
	}
}

func (h *conversationHarness) nextRunTime(jobID string) string {
	h.t.Helper()
	var next string
	if err := h.db.QueryRowContext(h.ctx, `SELECT next_run_at FROM scheduled_jobs WHERE id = ?`, jobID).Scan(&next); err != nil {
		h.t.Fatal(err)
	}
	return next
}

// A chat turn sets a reminder through the tool without any confirmation, and the tool's result
// carries the time as Go rendered it.
func TestReminderSetInConversationNeedsNoConfirmation(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "remind me at 5pm to call mom"))
	chat := receive(t, h.ctx, runtime.calls)
	names := toolNames(chat.request.Tools)
	if !slices.Contains(names, "reminder_set") || !slices.Contains(names, "reminder_cancel") {
		t.Fatalf("chat tools = %v", names)
	}
	result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{
		Name: "reminder_set", Arguments: json.RawMessage(`{"text":"Call mom","at":"2026-09-28 17:00"}`),
	})
	if err != nil || result.IsError || !strings.Contains(result.Text, "Mon 28 Sep 17:00 CEST") || !strings.Contains(result.Text, "Call mom") {
		t.Fatalf("reminder_set = %+v %v", result, err)
	}
	chat.answer <- agentruntime.RunResult{Response: "Reminder set."}
	h.wantReply(101, 1, "Reminder set.")

	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE principal_id = 'alice' AND job_type = ? AND run_at = '2026-09-28T15:00:00.000Z'`, schedule.TypeReminder); count != 1 {
		t.Fatalf("reminders = %d, want 1", count)
	}
	if count := h.countRows(`SELECT count(*) FROM approvals`); count != 0 {
		t.Fatalf("a reminder made %d approvals", count)
	}
	h.expectNoMessage(50 * time.Millisecond)

	list := h.requestJobs(2)
	if !strings.Contains(list.text, "1. ⏰ Call mom\nonce, Mon 28 Sep 17:00 CEST") {
		t.Fatalf("jobs = %q", list.text)
	}
	if data := buttonData(list.keyboard); len(data) != 2 || strings.HasPrefix(data[0], "jobs:pause:") || strings.HasPrefix(data[0], "jobs:resume:") {
		t.Fatalf("a one-time job has a pause button: %v", data)
	}

	h.push(textUpdate(3, 101, "never mind, cancel it"))
	second := receive(t, h.ctx, runtime.calls)
	cancel, err := second.request.Tools.Executor.Execute(second.ctx, agentruntime.ToolCall{
		Name: "reminder_cancel", Arguments: json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, h.jobID("alice"))),
	})
	if err != nil || cancel.IsError || !strings.Contains(cancel.Text, "Call mom") {
		t.Fatalf("reminder_cancel = %+v %v", cancel, err)
	}
	second.answer <- agentruntime.RunResult{Response: "Cancelled."}
	h.wantReply(101, 3, "Cancelled.")
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("jobs after cancelling = %d", count)
	}
}

// A reminder reaches its owner once, with snooze buttons, without a model turn and even when the
// owner's model subscription is not connected.
func TestReminderIsDeliveredOnceWithoutModelOrSubscription(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.start()
	h.createReminder("alice", "0000000000000001", "Call mom", "2026-09-28 17:00")

	h.scheduler.Wake()
	h.expectNoMessage(50 * time.Millisecond)

	clock.Set(t, "2026-09-28T17:00:10+02:00")
	h.scheduler.Wake()
	message := h.nextMessage()
	if message.chatID != 101 || message.replyTo != 0 || !strings.Contains(message.text, "⏰ Call mom") || strings.Contains(message.text, "This was due") {
		t.Fatalf("reminder = %+v", message)
	}
	want := []string{"remind:10m:0000000000000001", "remind:1h:0000000000000001", "remind:tomorrow:0000000000000001"}
	if data := buttonData(message.keyboard); !slices.Equal(data, want) {
		t.Fatalf("snooze buttons = %v", data)
	}
	h.waitRun("0000000000000001", schedule.StatusDelivered)

	h.scheduler.Wake()
	h.expectNoMessage(100 * time.Millisecond)
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM schedule_runs`); count != 1 {
		t.Fatalf("runs = %d, want 1", count)
	}
}

// Snooze edits the delivered message and brings the reminder back later; a second press while it
// is pending again moves nothing.
func TestReminderSnoozeEditsMessageAndRedelivers(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.start()
	h.createReminder("alice", "0000000000000001", "Call mom", "2026-09-28 17:00")

	clock.Set(t, "2026-09-28T17:00:10+02:00")
	h.scheduler.Wake()
	first := h.nextMessage()
	h.waitRun("0000000000000001", schedule.StatusDelivered)

	edit := h.pressJob(1, first.messageID, "remind:10m:0000000000000001")
	if edit.messageID != first.messageID || !strings.Contains(edit.text, "Snoozed until Mon 28 Sep 17:10 CEST.") || len(buttonData(edit.keyboard)) != 0 {
		t.Fatalf("snoozed message = %+v", edit)
	}
	if next := h.nextRunTime("0000000000000001"); next != "2026-09-28T15:10:10.000Z" {
		t.Fatalf("next run after snooze = %s", next)
	}
	if jobs := h.requestJobs(2); !strings.Contains(jobs.text, "once, Mon 28 Sep 17:10 CEST") {
		t.Fatalf("jobs after snooze = %q", jobs.text)
	}

	gone := h.pressJob(3, first.messageID, "remind:1h:0000000000000001")
	if !strings.Contains(gone.text, ReminderGoneReply) || len(buttonData(gone.keyboard)) != 0 {
		t.Fatalf("second press = %+v", gone)
	}
	if next := h.nextRunTime("0000000000000001"); next != "2026-09-28T15:10:10.000Z" {
		t.Fatalf("a second press moved the reminder to %s", next)
	}

	h.scheduler.Wake()
	h.expectNoMessage(50 * time.Millisecond)
	clock.Set(t, "2026-09-28T17:10:20+02:00")
	h.scheduler.Wake()
	again := h.nextMessage()
	if !strings.Contains(again.text, "⏰ Call mom") || strings.Contains(again.text, "This was due") || len(buttonData(again.keyboard)) != 3 {
		t.Fatalf("redelivered reminder = %+v", again)
	}
	if count := h.countRows(`SELECT count(*) FROM schedule_runs WHERE job_id = '0000000000000001'`); count != 2 {
		t.Fatalf("runs after snooze = %d, want 2", count)
	}
}

// Another user's snooze press or a press for a job that is not a delivered reminder changes
// nothing.
func TestReminderSnoozeIsOwnerOnly(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.start()
	h.createReminder("alice", "0000000000000001", "Call mom", "2026-09-28 17:00")
	clock.Set(t, "2026-09-28T17:00:10+02:00")
	h.scheduler.Wake()
	message := h.nextMessage()
	h.waitRun("0000000000000001", schedule.StatusDelivered)

	h.push(callbackUpdate(1, 202, message.messageID, "remind:10m:0000000000000001"))
	h.wantAnswer(1)
	if edit := h.nextEdit(); !strings.Contains(edit.text, ReminderGoneReply) {
		t.Fatalf("bob's press = %+v", edit)
	}
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE id = '0000000000000001' AND finished_at IS NOT NULL`); count != 1 {
		t.Fatal("bob snoozed alice's reminder")
	}
}

// A reminder is delivered however late, and says when it was due once it is more than a few
// minutes late.
func TestReminderDeliveredLateSaysWhenItWasDue(t *testing.T) {
	for _, test := range []struct {
		name string
		now  string
	}{
		{"six hours", "2026-09-28T23:00:00+02:00"},
		{"twenty-five hours", "2026-09-29T18:00:00+02:00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := newControlledRuntime()
			clock := newTestClock(t, mondayMorning)
			h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
			h.start()
			h.createReminder("alice", "0000000000000001", "Call mom", "2026-09-28 17:00")

			clock.Set(t, test.now)
			h.scheduler.Wake()
			message := h.nextMessage()
			if !strings.Contains(message.text, "⏰ Call mom") || !strings.Contains(message.text, "(This was due Mon 28 Sep 17:00 CEST.)") {
				t.Fatalf("late reminder = %q", message.text)
			}
			if len(buttonData(message.keyboard)) != 3 {
				t.Fatalf("late reminder buttons = %v", buttonData(message.keyboard))
			}
			expectNoRuntimeCall(t, runtime, 20*time.Millisecond)
		})
	}
}

// A one-time model turn is proposed with a time, confirmed once, runs once as a read-only turn,
// and then leaves /jobs.
func TestOneTimeJobIsProposedConfirmedRunOnceAndLeavesJobs(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "at 9 check whether my train is delayed"))
	chat := receive(t, h.ctx, runtime.calls)
	result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{
		Name:      "schedule_propose",
		Arguments: json.RawMessage(`{"name":"Train check","at":"2026-09-28 09:00","prompt":"Is my 9:30 train delayed?"}`),
	})
	if err != nil || result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
		t.Fatalf("proposal = %+v %v", result, err)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	for _, want := range []string{"Runs once: Mon 28 Sep 09:00 CEST", "Is my 9:30 train delayed?"} {
		if !strings.Contains(proposal.text, want) {
			t.Fatalf("proposal lacks %q: %s", want, proposal.text)
		}
	}
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("a proposal created %d jobs before confirmation", count)
	}

	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Scheduled for Mon 28 Sep 09:00 CEST.") {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 2)
	if list := h.requestJobs(3); !strings.Contains(list.text, "Train check\nonce, Mon 28 Sep 09:00 CEST") {
		t.Fatalf("jobs before the run = %q", list.text)
	}

	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if !strings.HasSuffix(run.request.Message, "Is my 9:30 train delayed?") {
		t.Fatalf("scheduled message = %q", run.request.Message)
	}
	run.answer <- agentruntime.RunResult{Response: "No delay."}
	h.wantMessage(101, "<b>Scheduled: Train check</b>\n\nNo delay.")
	h.waitRun(h.jobID("alice"), schedule.StatusDelivered)

	h.scheduler.Wake()
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM schedule_runs`); count != 1 {
		t.Fatalf("runs = %d, want 1", count)
	}
	if list := h.requestJobs(4); list.text != NoJobsReply {
		t.Fatalf("jobs after the run = %q", list.text)
	}
}

// A one-time model turn that was due more than its catch-up window ago is not run; its owner is
// told it was missed.
func TestOneTimeJobMissedForTooLongIsReportedNotRun(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()
	config, err := schedule.ModelTurnConfig("Is my train delayed?")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := h.scheduler.Prepare("alice", "Train check", schedule.TypeModelTurn, schedule.Timing{At: "2026-09-28 09:00"}, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.scheduler.Create(h.ctx, "0000000000000001", prepared); err != nil {
		t.Fatal(err)
	}

	clock.Set(t, "2026-09-28T15:30:00+02:00")
	h.scheduler.Wake()
	message := h.nextMessage()
	if message.chatID != 101 || !strings.Contains(message.text, "Scheduled: Train check") || !strings.Contains(message.text, schedule.MissedText) {
		t.Fatalf("missed notice = %+v", message)
	}
	h.waitRun("0000000000000001", schedule.StatusDelivered)
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)
	if list := h.requestJobs(1); list.text != NoJobsReply {
		t.Fatalf("jobs after the missed run = %q", list.text)
	}
}

// An unattended scheduled turn can read the schedule but is not offered the reminder tools, while
// chat turns are.
func TestScheduledTurnIsNotOfferedReminderTools(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Daily digest", "30 7 * * *", "Summarise my day.")

	h.push(callbackUpdate(1, 101, settingsMessageID(1), "jobs:run:"+job.ID))
	h.wantAnswer(1)
	h.nextEdit()
	h.wantMessage(101, "Running it now; the report will arrive here.")
	run := receive(t, h.ctx, runtime.calls)
	names := toolNames(run.request.Tools)
	if !slices.Contains(names, "schedule_list") {
		t.Fatalf("scheduled run tools = %v", names)
	}
	for _, forbidden := range []string{"reminder_set", "reminder_cancel", "schedule_propose", "schedule_remove"} {
		if slices.Contains(names, forbidden) {
			t.Fatalf("scheduled run was offered %s: %v", forbidden, names)
		}
	}
	written, err := run.request.Tools.Executor.Execute(run.ctx, agentruntime.ToolCall{
		Name: "reminder_set", Arguments: json.RawMessage(`{"text":"x","at":"2026-09-28 17:00"}`),
	})
	if err != nil || !written.IsError {
		t.Fatalf("a scheduled run set a reminder: %+v %v", written, err)
	}
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE job_type = ?`, schedule.TypeReminder); count != 0 {
		t.Fatalf("reminders created by a scheduled run = %d", count)
	}
	run.answer <- agentruntime.RunResult{Response: "Quiet day."}
	h.wantMessage(101, "<b>Scheduled: Daily digest</b>\n\nQuiet day.")
}

// Twenty jobs span three pages that stay within Telegram's limits, navigation shows the requested
// page, and an action on page two refreshes page two.
func TestJobsPagingStaysWithinTelegramLimitsAndKeepsPage(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.start()
	longPrompt := strings.Repeat("Summarise everything. ", 40)
	ids := make([]string, 0, 19)
	for n := 1; n <= 19; n++ {
		id := fmt.Sprintf("%016d", n)
		ids = append(ids, id)
		h.createJob("alice", id, fmt.Sprintf("Job number %02d %s", n, strings.Repeat("x", 40)), "30 7 * * *", longPrompt)
	}
	h.createReminder("alice", "0000000000000099", strings.Repeat("Call mom ", 50), "2026-09-28 17:00")

	list := h.requestJobs(1)
	if !strings.Contains(list.text, "Scheduled jobs (page 1 of 3)") || !strings.Contains(list.text, "1. Job number 01") || !strings.Contains(list.text, "8. Job number 08") || strings.Contains(list.text, "9. Job number 09") {
		t.Fatalf("page 1 = %q", list.text)
	}
	assertWithinLimits := func(page string, text string, keyboard InlineKeyboard) {
		t.Helper()
		buttons := 0
		for _, row := range keyboard {
			buttons += len(row)
		}
		if buttons > 100 || utf8.RuneCountInString(text) > 4096 {
			t.Fatalf("%s has %d buttons and %d characters", page, buttons, utf8.RuneCountInString(text))
		}
	}
	assertWithinLimits("page 1", list.text, list.keyboard)
	if data := buttonData(list.keyboard); !slices.Contains(data, "jobs:page:2") || slices.Contains(data, "jobs:page:1") {
		t.Fatalf("page 1 navigation = %v", data)
	}

	second := h.pressJob(2, list.messageID, "jobs:page:2")
	if !strings.Contains(second.text, "(page 2 of 3)") || !strings.Contains(second.text, "9. Job number 09") || !strings.Contains(second.text, "16. Job number 16") || strings.Contains(second.text, "8. Job number 08") {
		t.Fatalf("page 2 = %q", second.text)
	}
	assertWithinLimits("page 2", second.text, second.keyboard)
	if data := buttonData(second.keyboard); !slices.Contains(data, "jobs:page:1") || !slices.Contains(data, "jobs:page:3") {
		t.Fatalf("page 2 navigation = %v", data)
	}

	paused := h.pressJob(3, list.messageID, "jobs:pause:"+ids[8]+":2")
	if !strings.Contains(paused.text, "(page 2 of 3)") || !strings.Contains(paused.text, "9. Job number 09") || !strings.Contains(paused.text, ", paused") {
		t.Fatalf("page 2 after pausing = %q", paused.text)
	}
	if !slices.Contains(buttonData(paused.keyboard), "jobs:resume:"+ids[8]+":2") {
		t.Fatalf("page 2 buttons after pausing = %v", buttonData(paused.keyboard))
	}

	third := h.pressJob(4, list.messageID, "jobs:page:3")
	if !strings.Contains(third.text, "(page 3 of 3)") || !strings.Contains(third.text, "20. ⏰ Call mom") {
		t.Fatalf("page 3 = %q", third.text)
	}
	assertWithinLimits("page 3", third.text, third.keyboard)
	reminderRow := third.keyboard[len(third.keyboard)-2]
	if data := buttonData(InlineKeyboard{reminderRow}); len(data) != 2 || data[0] != "jobs:run:0000000000000099:3" || data[1] != "jobs:delete:0000000000000099:3" {
		t.Fatalf("reminder row = %v", data)
	}

	deleted := h.pressJob(5, list.messageID, "jobs:delete:0000000000000099:3")
	if !strings.Contains(deleted.text, "(page 3 of 3)") || strings.Contains(deleted.text, "Call mom") {
		t.Fatalf("page 3 after deleting the reminder = %q", deleted.text)
	}
}

// homeProvider stands in for a tool that reads the household's own devices.
type homeProvider struct{}

func (homeProvider) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{Definitions: []tools.Definition{{
		Name: "read_home", Description: "Read the home's devices.", Risk: tools.RiskRead, ThirdParty: true, Household: true,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}}, nil
}

func (homeProvider) Execute(context.Context, string, tools.Call) (tools.Result, error) {
	return tools.Result{Text: "lamp off"}, nil
}

// Reading the home's own devices leaves a reminder immediate; once the conversation reads outside
// content, a reminder needs Confirm in that turn and in the conversation's later turns.
func TestReminderAfterOutsideContentNeedsConfirmation(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, providers: []tools.Provider{homeProvider{}, outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()

	turn := func(updateID int64, read string) agentruntime.ToolResult {
		t.Helper()
		h.push(textUpdate(updateID, 101, "remind me at 5pm to call mom"))
		chat := receive(t, h.ctx, runtime.calls)
		if read != "" {
			if result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{Name: read, Arguments: json.RawMessage(`{}`)}); err != nil || result.IsError {
				t.Fatalf("%s = %+v %v", read, result, err)
			}
		}
		result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{
			Name: "reminder_set", Arguments: json.RawMessage(`{"text":"Call mom","at":"2026-09-28 17:00"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		chat.answer <- agentruntime.RunResult{Response: "ok"}
		h.wantReply(101, updateID, "ok")
		h.waitState(updateID, "delivered")
		return result
	}
	jobs := func() int {
		return h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE principal_id = 'alice' AND job_type = ?`, schedule.TypeReminder)
	}

	if result := turn(1, "read_home"); result.IsError || !strings.Contains(result.Text, "Reminder set") || jobs() != 1 {
		t.Fatalf("reminder after household content = %+v, jobs %d", result, jobs())
	}
	if result := turn(2, "read_outside"); result.IsError || strings.Contains(result.Text, "Reminder set") || jobs() != 1 {
		t.Fatalf("reminder after outside content = %+v, jobs %d", result, jobs())
	}
	if proposal := h.nextMessage(); !strings.Contains(proposal.text, "Call mom") || !strings.Contains(proposal.text, "outside content") {
		t.Fatalf("proposal = %+v", proposal)
	}
	if result := turn(3, ""); result.IsError || strings.Contains(result.Text, "Reminder set") || jobs() != 1 {
		t.Fatalf("reminder in a later turn of the same conversation = %+v, jobs %d", result, jobs())
	}
	if count := h.countRows(`SELECT count(*) FROM approvals WHERE principal_id = 'alice' AND tool = 'reminder_set'`); count != 2 {
		t.Fatalf("reminder proposals = %d, want 2", count)
	}
}

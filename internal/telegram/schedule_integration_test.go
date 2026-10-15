package telegram

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

// testClock is the scheduler's clock in these tests, so an occurrence is reached by moving time
// rather than by waiting for it.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(t *testing.T, value string) *testClock {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return &testClock{now: at}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(t *testing.T, value string) {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// Monday 28 September 2026, 08:00 in Amsterdam.
const mondayMorning = "2026-09-28T08:00:00+02:00"

func (h *conversationHarness) createJob(principalID, id, name, cron, prompt string) schedule.Job {
	h.t.Helper()
	config, err := schedule.ModelTurnConfig(prompt)
	if err != nil {
		h.t.Fatal(err)
	}
	prepared, err := h.scheduler.Prepare(principalID, name, schedule.TypeModelTurn, schedule.Timing{Cron: cron}, config)
	if err != nil {
		h.t.Fatal(err)
	}
	if created, err := h.scheduler.Create(h.ctx, id, prepared); err != nil || !created {
		h.t.Fatalf("create job: created=%v err=%v", created, err)
	}
	job := prepared.Job
	job.ID = id
	return job
}

func (h *conversationHarness) countRows(query string, arguments ...any) int {
	h.t.Helper()
	var count int
	if err := h.db.QueryRowContext(h.ctx, query, arguments...).Scan(&count); err != nil {
		h.t.Fatal(err)
	}
	return count
}

func (h *conversationHarness) waitRun(jobID, status string) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for h.countRows(`SELECT count(*) FROM schedule_runs WHERE job_id = ? AND status = ?`, jobID, status) == 0 {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("no run of %s reached %s", jobID, status)
		}
	}
}

// requestJobs sends /jobs and returns the reply.
func (h *conversationHarness) requestJobs(updateID int64) sentMessage {
	h.t.Helper()
	h.push(textUpdate(updateID, 101, "/jobs"))
	message := h.nextMessage()
	if message.chatID != 101 || message.replyTo != requestMessageID(updateID) {
		h.t.Fatalf("jobs reply = %+v", message)
	}
	return message
}

func (h *conversationHarness) pressJob(updateID, messageID int64, data string) editedMessage {
	h.t.Helper()
	h.push(callbackUpdate(updateID, 101, messageID, data))
	h.wantAnswer(updateID)
	return h.nextEdit()
}

func expectNoRuntimeCall(t *testing.T, runtime *controlledRuntime, wait time.Duration) {
	t.Helper()
	select {
	case call := <-runtime.calls:
		t.Fatalf("unexpected model turn: %q", call.request.Message)
	case <-time.After(wait):
	}
}

// The whole path: a job is proposed in conversation, confirmed once, runs at its time as an
// unattended read-only turn, reaches the owner's chat, and is known to the next chat turn.
func TestScheduledJobIsProposedConfirmedRunAndDelivered(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "every monday at 9 tell me what is overdue"))
	chat := receive(t, h.ctx, runtime.calls)
	readOutside(t, chat)
	names := toolNames(chat.request.Tools)
	if !slices.Contains(names, "schedule_propose") || !slices.Contains(names, "schedule_list") {
		t.Fatalf("chat tools = %v", names)
	}
	result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{
		Name:      "schedule_propose",
		Arguments: json.RawMessage(`{"name":"Weekly tasks","cron":"0 9 * * 1","prompt":"List my overdue Notion tasks."}`),
	})
	if err != nil || result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
		t.Fatalf("proposal = %+v %v", result, err)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	for _, want := range []string{
		"Runs: 0 9 * * 1 (Europe/Amsterdam)",
		"Next: Mon 28 Sep 09:00 CEST; Mon 5 Oct 09:00 CEST; Mon 12 Oct 09:00 CEST",
		"List my overdue Notion tasks.",
	} {
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
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Scheduled. First run: Mon 28 Sep 09:00 CEST.") {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 2)
	h.push(callbackUpdate(3, 101, proposal.messageID, confirm))
	h.wantAnswer(3)
	h.nextEdit()
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE principal_id = 'alice'`); count != 1 {
		t.Fatalf("jobs after two presses = %d, want 1", count)
	}
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)

	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(run.request.Message, `scheduled job "Weekly tasks"`) || !strings.HasSuffix(run.request.Message, "List my overdue Notion tasks.") {
		t.Fatalf("scheduled message = %q", run.request.Message)
	}
	if run.request.SessionID == chat.request.SessionID {
		t.Fatal("the scheduled run joined the chat session")
	}
	runNames := toolNames(run.request.Tools)
	if !slices.Contains(runNames, "schedule_list") || slices.Contains(runNames, "schedule_propose") || slices.Contains(runNames, "schedule_remove") {
		t.Fatalf("scheduled run tools = %v", runNames)
	}
	if strings.Contains(run.request.Tools.Guidance, "schedule_propose only proposes") {
		t.Fatalf("scheduled run was told about write tools: %q", run.request.Tools.Guidance)
	}
	written, err := run.request.Tools.Executor.Execute(run.ctx, agentruntime.ToolCall{
		Name: "schedule_propose", Arguments: json.RawMessage(`{"name":"Again","cron":"0 10 * * *","prompt":"x"}`),
	})
	if err != nil || !written.IsError {
		t.Fatalf("a scheduled run could call a write tool: %+v %v", written, err)
	}
	run.answer <- agentruntime.RunResult{Response: "Two tasks are overdue.", Final: "Two tasks are overdue."}
	h.wantMessage(101, "<b>Scheduled: Weekly tasks</b>\n\nTwo tasks are overdue.")
	h.waitRun(h.jobID("alice"), schedule.StatusDelivered)
	if retained := h.countRows(`SELECT count(*) FROM schedule_runs WHERE reply IS NOT NULL`); retained != 0 {
		t.Fatalf("%d delivered runs kept their reply", retained)
	}

	// The same occurrence never runs twice, however often the scheduler looks.
	h.scheduler.Wake()
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM schedule_runs`); count != 1 {
		t.Fatalf("runs = %d, want 1", count)
	}

	h.push(textUpdate(4, 101, "tell me more about the first one"))
	followUp := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(followUp.request.Message, "job \"Weekly tasks\" (job id "+h.jobID("alice")+"), Mon 28 Sep 09:00 CEST, report id ") ||
		!strings.Contains(followUp.request.Message, ": Two tasks are overdue.\n") {
		t.Fatalf("follow-up turn message = %q", followUp.request.Message)
	}
	followUp.answer <- agentruntime.RunResult{Response: "details"}
	h.wantReply(101, 4, "details")
}

// A report that may retell Notion page content makes the conversation it reaches private, so the
// guards never keep that conversation's replies; a report that read none leaves it recordable.
func TestScheduledReportOfPrivateContentMakesTheNextConversationPrivate(t *testing.T) {
	for _, private := range []bool{true, false} {
		t.Run(fmt.Sprintf("private=%v", private), func(t *testing.T) {
			runtime := newControlledRuntime()
			clock := newTestClock(t, mondayMorning)
			registry := turnstate.New(time.Now)
			h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, notion: writableNotionSimulator(t), recent: registry})
			h.provision("alice", "acct-alice-1111")
			h.installNotionToken("alice", aliceNotionToken)
			h.start()
			job := h.createJob("alice", "a1b2c3d4e5f60718", "Groceries", "0 9 * * *", "Summarise my grocery page.")

			clock.Set(t, "2026-09-28T09:00:20+02:00")
			h.scheduler.Wake()
			run := receive(t, h.ctx, runtime.calls)
			if private {
				result, err := run.request.Tools.Executor.Execute(run.ctx, agentruntime.ToolCall{Name: "notion_get_page", Arguments: json.RawMessage(`{"page_id":"page-groceries"}`)})
				if err != nil || result.IsError {
					t.Fatalf("notion_get_page = %+v %v", result, err)
				}
			}
			run.answer <- agentruntime.RunResult{Response: "Groceries is open.", Final: "Groceries is open."}
			h.wantMessage(101, "<b>Scheduled: Groceries</b>\n\nGroceries is open.")
			h.waitRun(job.ID, schedule.StatusDelivered)

			h.push(textUpdate(1, 101, "what is still open?"))
			followUp := receive(t, h.ctx, runtime.calls)
			if !strings.Contains(followUp.request.Message, "Groceries is open.") {
				t.Fatalf("follow-up turn message = %q", followUp.request.Message)
			}
			followUp.answer <- agentruntime.RunResult{Response: "Milk and eggs."}
			h.wantReply(101, 1, "Milk and eggs.")
			h.waitState(1, "delivered")

			// The worker finishes delivering a reply before it starts the next turn, and that turn's
			// own reply is not recorded until it is delivered.
			h.push(textUpdate(2, 101, "thanks"))
			next := receive(t, h.ctx, runtime.calls)
			kept := slices.ContainsFunc(registry.RecentExchange("alice"), func(line turnstate.Line) bool {
				return line.Role == turnstate.RoleAssistant && line.Text == "Milk and eggs."
			})
			next.answer <- agentruntime.RunResult{Response: "You're welcome."}
			h.wantReply(101, 2, "You're welcome.")
			if kept == private {
				t.Fatalf("reply kept = %v after a report with private=%v", kept, private)
			}
		})
	}
}

// The model reads a job's whole instructions before changing them, the user confirms the job as it
// will be, and the change keeps the job's id and pause state; a job changed before the press is
// left alone.
func TestScheduledJobIsReadInFullAndChanged(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()
	prompt := strings.Repeat("Summarise my day. ", 30) + "End with the weather."
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Daily digest", "30 7 * * *", prompt)
	if changed, err := h.scheduler.SetEnabled(h.ctx, "alice", job.ID, false); err != nil || !changed {
		t.Fatalf("pause = %v %v", changed, err)
	}

	h.push(textUpdate(1, 101, "move my digest to 8 and drop the weather"))
	chat := receive(t, h.ctx, runtime.calls)
	readOutside(t, chat)
	call := func(name, arguments string) agentruntime.ToolResult {
		t.Helper()
		result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if listed := call("schedule_list", `{}`); strings.Contains(listed.Text, "End with the weather.") {
		t.Fatalf("schedule_list showed the whole long prompt: %s", listed.Text)
	}
	if full := call("schedule_get", `{"job_id":"`+job.ID+`"}`); full.IsError || !strings.HasSuffix(full.Text, "Instructions: "+prompt) {
		t.Fatalf("schedule_get = %+v", full)
	}
	if switched := call("schedule_update", `{"job_id":"`+job.ID+`","at":"2026-09-30 08:00"}`); !switched.IsError {
		t.Fatalf("a recurring job became one-time: %+v", switched)
	}
	edited := strings.TrimSuffix(prompt, " End with the weather.")
	if proposed := call("schedule_update", `{"job_id":"`+job.ID+`","cron":"0 8 * * *","prompt":"`+edited+`"}`); proposed.IsError {
		t.Fatalf("schedule_update = %+v", proposed)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	for _, want := range []string{`Change "Daily digest"`, "Runs: 0 8 * * * (Europe/Amsterdam)", edited, "It stays paused"} {
		if !strings.Contains(proposal.text, want) {
			t.Fatalf("proposal lacks %q: %s", want, proposal.text)
		}
	}
	if strings.Contains(proposal.text, "weather") {
		t.Fatalf("proposal kept the dropped sentence: %s", proposal.text)
	}

	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Changed. It stays paused until you resume it in /jobs.") {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 2)
	stored, err := h.scheduler.Jobs(h.ctx, "alice")
	if err != nil || len(stored) != 1 {
		t.Fatalf("jobs = %+v %v", stored, err)
	}
	if got := stored[0]; got.ID != job.ID || got.Cron != "0 8 * * *" || schedule.ModelTurnPrompt(got.Config) != edited || got.Enabled {
		t.Fatalf("changed job = %+v", got)
	}

	h.push(textUpdate(3, 101, "rename it"))
	chat = receive(t, h.ctx, runtime.calls)
	if proposed := call("schedule_update", `{"job_id":"`+job.ID+`","name":"Morning digest"}`); proposed.IsError {
		t.Fatalf("schedule_update = %+v", proposed)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 3, "proposed")
	h.waitState(3, "delivered")
	proposal = h.nextMessage()
	if !strings.Contains(proposal.text, `Change "Daily digest", renamed to "Morning digest"`) {
		t.Fatalf("rename proposal = %s", proposal.text)
	}
	clock.Set(t, "2026-09-28T08:00:01+02:00")
	if changed, err := h.scheduler.SetEnabled(h.ctx, "alice", job.ID, true); err != nil || !changed {
		t.Fatalf("resume = %v %v", changed, err)
	}
	confirm, _ = approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(4, 101, proposal.messageID, confirm))
	h.wantAnswer(4)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "That job changed after this was proposed; nothing was changed.") {
		t.Fatalf("stale confirm = %+v", edit)
	}
	h.continueTurn(runtime, 4)
	if stored, err := h.scheduler.Jobs(h.ctx, "alice"); err != nil || stored[0].Name != "Daily digest" {
		t.Fatalf("a stale change was applied: %+v %v", stored, err)
	}
}

func (h *conversationHarness) jobID(principalID string) string {
	h.t.Helper()
	var id string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM scheduled_jobs WHERE principal_id = ?`, principalID).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// /jobs runs, pauses, resumes, and deletes the owner's jobs, and a chat message sent during a
// scheduled run waits for it instead of starting a second model process.
func TestJobsCommandControlsAndSharesTheTurnSlot(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Daily digest", "30 7 * * *", "Summarise my day.")

	list := h.requestJobs(1)
	if !strings.Contains(list.text, "1. Daily digest\n30 7 * * * (Europe/Amsterdam), next Tue 29 Sep 07:30 CEST") {
		t.Fatalf("jobs = %q", list.text)
	}
	if data := buttonData(list.keyboard); !slices.Equal(data, []string{"jobs:pause:" + job.ID + ":1", "jobs:tpause:" + job.ID + ":1", "jobs:run:" + job.ID + ":1", "jobs:delete:" + job.ID + ":1"}) {
		t.Fatalf("jobs buttons = %v", data)
	}

	h.pressJob(2, list.messageID, "jobs:run:"+job.ID)
	h.wantMessage(101, "Running it now; the report will arrive here.")
	run := receive(t, h.ctx, runtime.calls)

	h.push(textUpdate(3, 101, "hello"))
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)
	run.answer <- agentruntime.RunResult{Response: "Quiet day.", Final: "Quiet day."}
	h.wantMessage(101, "<b>Scheduled: Daily digest</b>\n\nQuiet day.")
	chat := receive(t, h.ctx, runtime.calls)
	chat.answer <- agentruntime.RunResult{Response: "hi"}
	h.wantReply(101, 3, "hi")

	// The poller retries a press whose reply failed; the retry finds the run already queued.
	if queued, err := h.scheduler.RunNow(h.ctx, "alice", job.ID, pressTrigger(Bot{ID: 999}, 2)); err != nil || !queued {
		t.Fatalf("retried Run now = %v %v", queued, err)
	}
	h.scheduler.Wake()
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM schedule_runs WHERE manual = 1`); count != 1 {
		t.Fatalf("manual runs after a retried press = %d, want 1", count)
	}

	var next string
	if err := h.db.QueryRowContext(h.ctx, `SELECT next_run_at FROM scheduled_jobs WHERE id = ?`, job.ID).Scan(&next); err != nil || next != "2026-09-29T05:30:00.000Z" {
		t.Fatalf("next run after Run now = %q %v", next, err)
	}

	paused := h.pressJob(4, list.messageID, "jobs:pause:"+job.ID)
	if !strings.Contains(paused.text, "Daily digest\n30 7 * * * (Europe/Amsterdam), paused") || !slices.Contains(buttonData(paused.keyboard), "jobs:resume:"+job.ID+":1") {
		t.Fatalf("jobs after pause = %+v", paused)
	}
	clock.Set(t, "2026-09-29T07:31:00+02:00")
	h.scheduler.Wake()
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)

	resumed := h.pressJob(5, list.messageID, "jobs:resume:"+job.ID)
	if !strings.Contains(resumed.text, "next Wed 30 Sep 07:30 CEST") {
		t.Fatalf("jobs after resume = %q", resumed.text)
	}
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)

	// Bob cannot act on Alice's job, even with her button data.
	h.push(callbackUpdate(6, 202, list.messageID, "jobs:delete:"+job.ID))
	h.wantAnswer(6)
	h.nextEdit()
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE id = ?`, job.ID); count != 1 {
		t.Fatal("another user deleted the job")
	}
	h.wantMessage(202, JobGoneReply)

	deleted := h.pressJob(7, list.messageID, "jobs:delete:"+job.ID)
	if deleted.text != NoJobsReply {
		t.Fatalf("jobs after delete = %q", deleted.text)
	}
}

// /jobs toggles a recurring job's pause while traveling, and offers it neither for the built-in
// Heads-up job nor for a one-time job.
func TestJobsCommandTogglesThePauseWhileTraveling(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Daily digest", "30 7 * * *", "Summarise my day.")
	headsUp := h.createJob("alice", schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, schedule.HeadsUpCron, schedule.HeadsUpPrompt)

	list := h.requestJobs(1)
	if slices.Contains(buttonData(list.keyboard), "jobs:tpause:"+headsUp.ID+":1") {
		t.Fatalf("Heads-up offers the travel pause: %v", buttonData(list.keyboard))
	}
	flagged := h.pressJob(2, list.messageID, "jobs:tpause:"+job.ID)
	if !strings.Contains(flagged.text, "Daily digest\n30 7 * * * (Europe/Amsterdam), next Tue 29 Sep 07:30 CEST, pauses while traveling") ||
		!slices.Contains(buttonData(flagged.keyboard), "jobs:tresume:"+job.ID+":1") {
		t.Fatalf("jobs after the travel pause = %+v", flagged)
	}
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE travel_pause = 1`); count != 1 {
		t.Fatalf("flagged jobs = %d", count)
	}
	cleared := h.pressJob(3, list.messageID, "jobs:tresume:"+job.ID)
	if strings.Contains(cleared.text, "traveling") || !slices.Contains(buttonData(cleared.keyboard), "jobs:tpause:"+job.ID+":1") {
		t.Fatalf("jobs after clearing the travel pause = %+v", cleared)
	}
}

// A run whose owner has no model connection says so instead of disappearing, and a run whose
// owner lost access is dropped without reaching any chat.
func TestScheduledRunsForUnavailableOwners(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.start()
	alice := h.createJob("alice", "0000000000000001", "Morning", "0 9 * * *", "Anything new?")
	bob := h.createJob("bob", "0000000000000002", "Morning", "0 9 * * *", "Anything new?")
	if _, err := h.db.ExecContext(h.ctx, `UPDATE telegram_allowlist SET allowed_at = '2026-09-01T00:00:00.000Z', revoked_at = '2026-09-28T06:30:00.000Z' WHERE principal_id = 'bob'`); err != nil {
		t.Fatal(err)
	}

	clock.Set(t, "2026-09-28T09:00:05+02:00")
	h.scheduler.Wake()
	h.wantMessage(101, "<b>Scheduled: Morning</b>\n\n"+conversation.NotConnectedReply)
	h.waitRun(alice.ID, schedule.StatusDelivered)
	h.waitRun(bob.ID, schedule.StatusSkipped)
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)
	select {
	case message := <-h.messages:
		t.Fatalf("a revoked owner was sent %+v", message)
	default:
	}
}

// An occurrence missed for longer than the catch-up window is skipped instead of run late, while a
// recent one runs once.
func TestScheduledRunCatchUpAfterDowntime(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()
	late := h.createJob("alice", "00000000000000aa", "Early", "0 1 * * *", "Night report.")
	recent := h.createJob("alice", "00000000000000bb", "Recent", "0 6 * * *", "Morning report.")
	// Both were last due during downtime: 01:00 is seven hours ago, 06:00 two.
	if _, err := h.db.ExecContext(h.ctx, `UPDATE scheduled_jobs SET next_run_at = '2026-09-27T23:00:00.000Z' WHERE id = ?`, late.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `UPDATE scheduled_jobs SET next_run_at = '2026-09-28T04:00:00.000Z' WHERE id = ?`, recent.ID); err != nil {
		t.Fatal(err)
	}

	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if !strings.HasSuffix(run.request.Message, "Morning report.") {
		t.Fatalf("caught-up run = %q", run.request.Message)
	}
	run.answer <- agentruntime.RunResult{Response: "done", Final: "done"}
	h.wantMessage(101, "<b>Scheduled: Recent</b>\n\ndone")
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM schedule_runs WHERE job_id = ?`, late.ID); count != 0 {
		t.Fatal("a seven-hour-old occurrence was queued")
	}
	var next string
	if err := h.db.QueryRowContext(h.ctx, `SELECT next_run_at FROM scheduled_jobs WHERE id = ?`, late.ID).Scan(&next); err != nil || next != "2026-09-28T23:00:00.000Z" {
		t.Fatalf("missed job next run = %q %v", next, err)
	}
}

// A run left running by a previous process is reported as interrupted, never run again.
func TestScheduledRunInterruptedByRestart(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	job := h.createJob("alice", "00000000000000cc", "Digest", "0 9 * * *", "Digest.")
	run := schedule.Run{ID: "00000000000000dd", JobID: job.ID, PrincipalID: "alice", ScheduledFor: clock.Now()}
	if err := h.store.QueueScheduledRun(h.ctx, run); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.store.TransitionScheduledRun(h.ctx, run.ID, schedule.StatusQueued, schedule.StatusRunning, "", "", clock.Now()); err != nil || !ok {
		t.Fatalf("start run: %v %v", ok, err)
	}

	h.start()
	h.wantMessage(101, "<b>Scheduled: Digest</b>\n\n"+schedule.InterruptedText)
	h.waitRun(job.ID, schedule.StatusDelivered)
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)
}

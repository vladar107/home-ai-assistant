package telegram

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

func TestQuietScheduledRunSendsNothing(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	job := h.createJob("alice", "a1b2c3d4e5f60718", "Mail", "0 9 * * *", "Tell me only when new mail needs a reply.")
	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(run.request.Message, prompt.NothingToAdd) {
		t.Fatalf("scheduled message lacks the quiet instruction: %q", run.request.Message)
	}
	run.answer <- agentruntime.RunResult{Response: " " + prompt.NothingToAdd + "\n", Final: prompt.NothingToAdd}
	h.waitRun(job.ID, schedule.StatusSkipped)

	if got := h.countRows(`SELECT count(*) FROM schedule_runs WHERE job_id = ? AND status = 'skipped' AND error_kind = 'quiet'`, job.ID); got != 1 {
		t.Fatalf("quiet skipped runs = %d, want 1", got)
	}
	if got := h.countRows(`SELECT count(*) FROM job_reports`); got != 0 {
		t.Fatalf("a quiet run stored %d reports", got)
	}
	time.Sleep(50 * time.Millisecond)
	if len(h.messages) != 0 {
		t.Fatalf("a quiet run sent a message: %+v", <-h.messages)
	}
}

func TestQuietManualRunSaysSoAndMarkerIsStrippedFromText(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	job := h.createJob("alice", "a1b2c3d4e5f60718", "Mail", "0 9 * * *", "Tell me only when new mail needs a reply.")
	if queued, err := h.scheduler.RunNow(h.ctx, "alice", job.ID, ""); err != nil || !queued {
		t.Fatalf("run now = %v %v", queued, err)
	}
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	run.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd, Final: prompt.NothingToAdd}
	if message := h.nextMessage(); !strings.Contains(message.text, schedule.QuietManualText) {
		t.Fatalf("manual quiet run sent %q", message.text)
	}
	h.waitRun(job.ID, schedule.StatusDelivered)

	clock.Set(t, "2026-09-28T08:05:00+02:00")
	if queued, err := h.scheduler.RunNow(h.ctx, "alice", job.ID, ""); err != nil || !queued {
		t.Fatalf("run now = %v %v", queued, err)
	}
	h.scheduler.Wake()
	run = receive(t, h.ctx, runtime.calls)
	text := "Rent is due tomorrow. " + prompt.NothingToAdd
	run.answer <- agentruntime.RunResult{Response: text, Final: text}
	message := h.nextMessage()
	if !strings.Contains(message.text, "Rent is due tomorrow.") || strings.Contains(message.text, prompt.NothingToAdd) {
		t.Fatalf("report = %q", message.text)
	}
}

func TestHeadsUpNotesFitTheJobLimit(t *testing.T) {
	t.Parallel()
	if n := utf8.RuneCountInString(schedule.HeadsUpNotes); n > 1000 {
		t.Fatalf("notes have %d runes", n)
	}
}

// A Heads-up run follows the built-in method with the user's notes after it, and reads its own
// reports of the last days in full; another job's run gets only its own instructions.
func TestHeadsUpRunsTheMethodWithTheUsersNotes(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	notes := "An all-day Office event means I go to the office."
	headsUp := h.createJob("alice", schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, schedule.HeadsUpCron, notes)
	other := h.createJob("alice", "a1b2c3d4e5f60718", "Mail", "0 9 * * *", "Check my mail.")
	run := func(jobID, reply string) string {
		t.Helper()
		if queued, err := h.scheduler.RunNow(h.ctx, "alice", jobID, ""); err != nil || !queued {
			t.Fatalf("run now = %v %v", queued, err)
		}
		h.scheduler.Wake()
		call := receive(t, h.ctx, runtime.calls)
		call.answer <- agentruntime.RunResult{Response: reply, Final: reply}
		h.nextMessage()
		for h.countRows(`SELECT count(*) FROM job_reports WHERE text = ?`, reply) == 0 {
			select {
			case <-time.After(5 * time.Millisecond):
			case <-h.ctx.Done():
				t.Fatalf("the report %q was not kept", reply)
			}
		}
		return call.request.Message
	}

	message := run(headsUp.ID, "The 08:17 is cancelled; take the 08:23.")
	method, ownNotes := strings.Index(message, "This is the user's Heads-up"), strings.Index(message, notes)
	if method < 0 || ownNotes < method || !strings.HasSuffix(strings.TrimSpace(message), notes) || !strings.Contains(message, `job "`+headsUp.ID+`" and days 3`) {
		t.Fatalf("heads-up message = %q", message)
	}
	if message := run(other.ID, "No new mail."); strings.Contains(message, "This is the user's Heads-up") || !strings.Contains(message, "Check my mail.") {
		t.Fatalf("another job's message = %q", message)
	}

	clock.Set(t, "2026-09-28T18:30:00+02:00")
	run(headsUp.ID, "The brush needs replacing.")
	read := func(arguments string) string {
		t.Helper()
		result, err := h.scheduler.Execute(h.ctx, "alice", tools.Call{Name: "job_reports", Arguments: json.RawMessage(arguments)})
		if err != nil {
			t.Fatal(err)
		}
		return result.Text
	}
	full := read(`{"job":"Heads-up","days":3}`)
	newest, oldest := strings.Index(full, "The brush needs replacing."), strings.Index(full, "The 08:17 is cancelled; take the 08:23.")
	if newest < 0 || oldest < newest || strings.Contains(full, "No new mail.") {
		t.Fatalf("full reports = %q", full)
	}
	for _, arguments := range []string{`{"days":3}`, `{"job":"Heads-up","days":8}`} {
		if text := read(arguments); !strings.Contains(text, "days must be between 1 and 7") {
			t.Fatalf("%s = %q", arguments, text)
		}
	}
	clock.Set(t, "2026-10-02T09:00:00+02:00")
	if text := read(`{"job":"Heads-up","days":3}`); !strings.Contains(text, "no such reports from the last 3 days") {
		t.Fatalf("old reports were returned: %q", text)
	}
}

func TestHeadsUpSwitchCreatesResumesAndDeletesItsJob(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	settings := h.requestSettings(1, 101)
	messageID := settingsMessageID(1)
	if !strings.Contains(settings.text, "Heads-up off") {
		t.Fatalf("overview = %q", settings.text)
	}
	page := h.openSettingsPage(2, 101, messageID, "tools")
	if !strings.Contains(page.text, HeadsUpOffLine) || !slices.Contains(buttonData(page.keyboard), "headsup:on") {
		t.Fatalf("page by default = %+v", page)
	}
	if got := h.countRows(`SELECT count(*) FROM scheduled_jobs`); got != 0 {
		t.Fatalf("jobs before turning on = %d", got)
	}

	page = h.press(3, 101, messageID, "headsup:on")
	if !strings.Contains(page.text, HeadsUpOnLine) || !slices.Contains(buttonData(page.keyboard), "headsup:off") {
		t.Fatalf("page after on = %+v", page)
	}
	page = h.press(4, 101, messageID, "headsup:on")
	if !strings.Contains(page.text, HeadsUpOnLine) {
		t.Fatalf("page after a second on = %+v", page)
	}
	jobs, err := h.scheduler.Jobs(h.ctx, "alice")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	job := jobs[0]
	if job.ID != schedule.HeadsUpJobID("alice") || job.Name != schedule.HeadsUpName || job.Cron != schedule.HeadsUpCron ||
		job.Zone != h.scheduler.DefaultZone() || job.Type != schedule.TypeModelTurn || !job.Enabled ||
		schedule.ModelTurnPrompt(job.Config) != schedule.HeadsUpNotes {
		t.Fatalf("job = %+v", job)
	}
	if schedule.HeadsUpJobID("alice") == schedule.HeadsUpJobID("bob") || len(job.ID) != 16 {
		t.Fatalf("job id %q is not per principal", job.ID)
	}

	if changed, err := h.scheduler.SetEnabled(h.ctx, "alice", job.ID, false); err != nil || !changed {
		t.Fatalf("pause = %v %v", changed, err)
	}
	page = h.press(5, 101, messageID, settingsNavPrefix+":tools")
	if !strings.Contains(page.text, HeadsUpOffLine) || !slices.Contains(buttonData(page.keyboard), "headsup:on") {
		t.Fatalf("page for a paused job = %+v", page)
	}
	page = h.press(6, 101, messageID, "headsup:on")
	if !strings.Contains(page.text, HeadsUpOnLine) {
		t.Fatalf("page after resuming = %+v", page)
	}
	if got := h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE enabled = 1`); got != 1 {
		t.Fatalf("enabled jobs after resuming = %d", got)
	}

	page = h.press(7, 101, messageID, "headsup:off")
	if !strings.Contains(page.text, HeadsUpOffLine) {
		t.Fatalf("page after off = %+v", page)
	}
	if got := h.countRows(`SELECT count(*) FROM scheduled_jobs`); got != 0 {
		t.Fatalf("jobs after turning off = %d", got)
	}
}

// The Heads-up prompt asks for about a dozen lookups, so its runs get more tool calls than another
// job's turn.
func TestHeadsUpRunsGetALargerToolBudget(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	calls := func(jobID string) int {
		if queued, err := h.scheduler.RunNow(h.ctx, "alice", jobID, ""); err != nil || !queued {
			t.Fatalf("run now = %v %v", queued, err)
		}
		h.scheduler.Wake()
		run := receive(t, h.ctx, runtime.calls)
		made := 0
		for range 12 {
			result, err := run.request.Tools.Executor.Execute(run.ctx, agentruntime.ToolCall{Name: "app_help", Arguments: json.RawMessage(`{"topic":"commands"}`)})
			if err != nil || strings.Contains(result.Text, "budget for this turn is exhausted") {
				break
			}
			made++
		}
		run.answer <- agentruntime.RunResult{Response: "ok", Final: "ok"}
		h.waitRun(jobID, schedule.StatusDelivered)
		h.nextMessage()
		return made
	}

	other := h.createJob("alice", "a1b2c3d4e5f60718", "Mail", "0 9 * * *", "Check my mail.")
	if made := calls(other.ID); made != 8 {
		t.Fatalf("another job made %d calls, want 8", made)
	}
	clock.Set(t, "2026-09-28T08:05:00+02:00")
	headsUp := h.createJob("alice", schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, schedule.HeadsUpCron, schedule.HeadsUpNotes)
	if made := calls(headsUp.ID); made != 12 {
		t.Fatalf("the Heads-up job made %d calls, want 12", made)
	}
}

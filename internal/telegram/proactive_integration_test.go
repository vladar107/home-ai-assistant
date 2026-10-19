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

func TestHeadsUpPromptFitsTheJobLimit(t *testing.T) {
	t.Parallel()
	if n := utf8.RuneCountInString(schedule.HeadsUpPrompt); n > 1000 {
		t.Fatalf("prompt has %d runes", n)
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
		schedule.ModelTurnPrompt(job.Config) != schedule.HeadsUpPrompt {
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
	headsUp := h.createJob("alice", schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, schedule.HeadsUpCron, schedule.HeadsUpPrompt)
	if made := calls(headsUp.ID); made != 12 {
		t.Fatalf("the Heads-up job made %d calls, want 12", made)
	}
}

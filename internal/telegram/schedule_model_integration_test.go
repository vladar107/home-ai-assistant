package telegram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// jobModelCatalog offers the harness's chat, task, and Heads-up models, and a model whose minimal
// level runs as low.
const jobModelCatalog = `{"openai-codex":{"models":[
 {"id":"gpt-test","name":"Test","reasoning":true,"contextWindow":200000,"thinkingLevelMap":{"off":null}},
 {"id":"gpt-6.1-sol","name":"Sol","reasoning":true,"contextWindow":1000000,"thinkingLevelMap":{"off":null,"minimal":null,"xhigh":"xhigh"}},
 {"id":"gpt-7-terra","name":"Terra","reasoning":true,"contextWindow":400000,"thinkingLevelMap":{"off":null,"minimal":"low"}}
]}}`

func newJobModelHarness(t *testing.T, providers ...tools.Provider) (*conversationHarness, *controlledRuntime) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: newTestClock(t, mondayMorning), providers: providers})
	h.provision("alice", "account-alice")
	if err := os.WriteFile(filepath.Join(h.profilesDir, "alice", "models-store.json"), []byte(jobModelCatalog), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start()
	return h, runtime
}

func (h *conversationHarness) createJobWithModel(id, name, prompt, model string) schedule.Job {
	h.t.Helper()
	config, err := schedule.ModelTurnConfigWithModel(prompt, model)
	if err != nil {
		h.t.Fatal(err)
	}
	prepared, err := h.scheduler.Prepare("alice", name, schedule.TypeModelTurn, schedule.Timing{Cron: "0 9 * * *"}, config)
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

// runJobModel runs the job once by hand and returns the model and effort its turn ran on.
func (h *conversationHarness) runJobModel(runtime *controlledRuntime, jobID string) agentruntime.ModelEffort {
	h.t.Helper()
	if queued, err := h.scheduler.RunNow(h.ctx, "alice", jobID, ""); err != nil || !queued {
		h.t.Fatalf("run now = %v %v", queued, err)
	}
	run := receive(h.t, h.ctx, runtime.calls)
	run.answer <- agentruntime.RunResult{Response: "ok", Final: "ok"}
	h.waitRun(jobID, schedule.StatusDelivered)
	h.nextMessage()
	return agentruntime.ModelEffort{Model: run.request.Model, Effort: run.request.Effort}
}

// A job runs on its own model, the Heads-up job without one on the Heads-up default, and every
// other job on the chat default; a model the catalog no longer offers falls back to the chat
// default.
func TestScheduledRunsUseTheJobModel(t *testing.T) {
	t.Parallel()
	h, runtime := newJobModelHarness(t)

	plain := h.createJob("alice", "a1b2c3d4e5f60718", "Mail", "0 9 * * *", "Check my mail.")
	if got := h.runJobModel(runtime, plain.ID); got != (agentruntime.ModelEffort{}) {
		t.Fatalf("a job without a model ran on %+v, want the runtime's default", got)
	}
	headsUp := h.createJob("alice", schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, schedule.HeadsUpCron, schedule.HeadsUpNotes)
	if got := h.runJobModel(runtime, headsUp.ID); got != (agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "high"}) {
		t.Fatalf("the Heads-up job ran on %+v, want the Heads-up default", got)
	}
	own := h.createJobWithModel("b1b2c3d4e5f60718", "Plans", "Check my plans.", "gpt-7-terra:low")
	if got := h.runJobModel(runtime, own.ID); got != (agentruntime.ModelEffort{Model: "gpt-7-terra", Effort: "low"}) {
		t.Fatalf("a job with its own model ran on %+v", got)
	}
	retired := h.createJobWithModel("c1b2c3d4e5f60718", "Old", "Check the news.", "gpt-5-luna:high")
	if got := h.runJobModel(runtime, retired.ID); got != (agentruntime.ModelEffort{Model: "gpt-test", Effort: "medium"}) {
		t.Fatalf("a job whose model is gone ran on %+v, want the chat default", got)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "schedule.model.fallback") || !strings.Contains(logs, "job_id="+retired.ID) || !strings.Contains(logs, "reason=catalog") {
		t.Fatalf("the fallback was not logged: %s", logs)
	}
}

// The Heads-up job falls back to the chat default when the catalog does not offer its default.
func TestHeadsUpFallsBackWhenItsDefaultIsNotOffered(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: newTestClock(t, mondayMorning)})
	h.provision("alice", "account-alice")
	catalog := `{"openai-codex":{"models":[{"id":"gpt-test","name":"Test","reasoning":true,"contextWindow":200000,"thinkingLevelMap":{"off":null}}]}}`
	if err := os.WriteFile(filepath.Join(h.profilesDir, "alice", "models-store.json"), []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start()

	headsUp := h.createJob("alice", schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, schedule.HeadsUpCron, schedule.HeadsUpNotes)
	if got := h.runJobModel(runtime, headsUp.ID); got != (agentruntime.ModelEffort{Model: "gpt-test", Effort: "medium"}) {
		t.Fatalf("the Heads-up job ran on %+v, want the chat default", got)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "schedule.model.fallback") || !strings.Contains(logs, "job_id="+headsUp.ID) {
		t.Fatalf("the fallback was not logged: %s", logs)
	}
}

// A proposed job with its own model shows the model on the proposal, previews on it, and is stored
// with the level the model runs once confirmed, and its runs use it.
func TestProposedJobModelGoesThroughConfirm(t *testing.T) {
	t.Parallel()
	h, runtime := newJobModelHarness(t, outsideProvider{})

	h.push(textUpdate(1, 101, "every morning check my plans on terra"))
	chat := receive(t, h.ctx, runtime.calls)
	readOutside(t, chat)
	if proposed := toolCaller(t, chat)("schedule_propose", `{"name":"Plans","cron":"0 9 * * *","prompt":"Check my plans.","model":"gpt-7-terra:minimal"}`); proposed.IsError {
		t.Fatalf("schedule_propose = %+v", proposed)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	h.waitMessageRecorded(proposal.messageID)
	if !strings.Contains(proposal.text, "Model: gpt-7-terra:low") {
		t.Fatalf("proposal lacks the model: %s", proposal.text)
	}
	data := buttonData(proposal.keyboard)
	if len(data) != 3 || !strings.HasPrefix(data[2], "approval:preview:") {
		t.Fatalf("proposal buttons = %v", data)
	}

	h.push(callbackUpdate(2, 101, proposal.messageID, data[2]))
	h.nextAnswer(2)
	preview := receive(t, h.ctx, runtime.calls)
	if got := (agentruntime.ModelEffort{Model: preview.request.Model, Effort: preview.request.Effort}); got != (agentruntime.ModelEffort{Model: "gpt-7-terra", Effort: "low"}) {
		t.Fatalf("the preview ran on %+v", got)
	}
	preview.answer <- agentruntime.RunResult{Response: "Two plans.", Final: "Two plans."}
	h.nextMessage()

	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(3, 101, proposal.messageID, confirm))
	h.wantAnswer(3)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Scheduled. First run:") {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 3)
	jobs, err := h.scheduler.Jobs(h.ctx, "alice")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	if got := schedule.ModelTurnModel(jobs[0].Config); got != "gpt-7-terra:low" {
		t.Fatalf("stored model = %q", got)
	}
	if got := h.runJobModel(runtime, jobs[0].ID); got != (agentruntime.ModelEffort{Model: "gpt-7-terra", Effort: "low"}) {
		t.Fatalf("the confirmed job ran on %+v", got)
	}
}

// Chat sets a job's model only to one the catalog offers, stored on the level the model runs, and
// schedule_get names the model each job runs on and where it comes from. Undo restores the default.
func TestScheduleUpdateChecksTheJobModel(t *testing.T) {
	t.Parallel()
	h, runtime := newJobModelHarness(t)
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Mail", "0 9 * * *", "Check my mail.")
	h.createJob("alice", schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, schedule.HeadsUpCron, schedule.HeadsUpNotes)

	refused := h.ownDataTurn(runtime, 1, false, "schedule_update", `{"job_id":"`+job.ID+`","model":"gpt-5-luna:high"}`)
	if !refused.IsError || !strings.Contains(refused.Text, "gpt-5-luna:high") || !strings.Contains(refused.Text, "gpt-7-terra (low, medium, high)") {
		t.Fatalf("schedule_update with a model not offered = %+v", refused)
	}
	if stored, _ := h.storedJob(job.ID); schedule.ModelTurnModel(stored.Config) != "" {
		t.Fatalf("a refused model was stored: %s", stored.Config)
	}

	updated := h.wantDirect(h.ownDataTurn(runtime, 2, false, "schedule_update", `{"job_id":"`+job.ID+`","model":"gpt-7-terra:minimal"}`), "schedule_update")
	stored, _ := h.storedJob(job.ID)
	if got := schedule.ModelTurnModel(stored.Config); got != "gpt-7-terra:low" {
		t.Fatalf("stored model = %q, want the level the model runs", got)
	}
	if schedule.ModelTurnPrompt(stored.Config) != "Check my mail." {
		t.Fatalf("the model change touched the prompt: %s", stored.Config)
	}

	h.push(textUpdate(3, 101, "which models do my jobs use?"))
	chat := receive(t, h.ctx, runtime.calls)
	call := toolCaller(t, chat)
	for id, want := range map[string]string{
		job.ID:                         "Model: gpt-7-terra:low (its own choice)",
		schedule.HeadsUpJobID("alice"): "Model: gpt-6.1-sol:high (Heads-up default)",
	} {
		if got := call("schedule_get", `{"job_id":"`+id+`"}`); got.IsError || !strings.Contains(got.Text, want) {
			t.Fatalf("schedule_get %s = %+v, want %q", id, got, want)
		}
	}
	h.finishTurn(chat, 3, "ok")

	if answer := h.undoDirect(updated); answer != "Undone." {
		t.Fatalf("undo of a model change = %q", answer)
	}
	if restored, _ := h.storedJob(job.ID); schedule.ModelTurnModel(restored.Config) != "" {
		t.Fatalf("undo left the model: %s", restored.Config)
	}

	h.push(textUpdate(4, 101, "and now?"))
	chat = receive(t, h.ctx, runtime.calls)
	if got := toolCaller(t, chat)("schedule_get", `{"job_id":"`+job.ID+`"}`); !strings.Contains(got.Text, "Model: gpt-test:medium (chat default)") {
		t.Fatalf("schedule_get after undo = %+v", got)
	}
	h.finishTurn(chat, 4, "ok")
}

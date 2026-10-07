package telegram

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

// toolCaller calls the chat turn's tools as the model would.
func toolCaller(t *testing.T, chat runtimeCall) func(name, arguments string) agentruntime.ToolResult {
	return func(name, arguments string) agentruntime.ToolResult {
		t.Helper()
		result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
}

// A change to a job's instructions is a patch: each edit must match once, the result is held to the
// prompt's limits, and the user confirms only the changed passages.
func TestScheduledJobInstructionsArePatched(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()
	prompt := "Check the trains from Utrecht Centraal to Den Haag Centraal. Report delays over 5 minutes. Report cancellations. End with the weather."
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Morning trains", "30 7 * * 1-5", prompt)

	h.push(textUpdate(1, 101, "only tell me about delays over 10 minutes and skip the weather"))
	chat := receive(t, h.ctx, runtime.calls)
	call := toolCaller(t, chat)
	for _, refused := range []struct{ arguments, want string }{
		{`{"job_id":"` + job.ID + `","prompt_edits":[{"find":"over 15 minutes","replace":"over 10 minutes"}]}`, "Edit 1: its find text \"over 15 minutes\" does not occur"},
		{`{"job_id":"` + job.ID + `","prompt_edits":[{"find":"over 5 minutes","replace":"over 10 minutes"},{"find":"Report","replace":"Tell me about"}]}`, "Edit 2: its find text \"Report\" occurs 2 times"},
		{`{"job_id":"` + job.ID + `","prompt":"Check the trains.","prompt_edits":[{"find":"over 5 minutes","replace":"over 10 minutes"}]}`, "not both"},
		{`{"job_id":"` + job.ID + `","prompt_edits":[{"find":"End with the weather.","replace":"` + strings.Repeat("x", 1000) + `"}]}`, "the prompt must be between 1 and 1000 characters"},
		{`{"job_id":"` + job.ID + `","prompt_edits":[{"find":"over 5 minutes","replace":"over 5 minutes"}]}`, "That changes nothing"},
	} {
		if result := call("schedule_update", refused.arguments); !result.IsError || !strings.Contains(result.Text, refused.want) {
			t.Fatalf("schedule_update %s = %+v, want an error with %q", refused.arguments, result, refused.want)
		}
	}
	patched := call("schedule_update", `{"job_id":"`+job.ID+`","prompt_edits":[{"find":"over 5 minutes","replace":"over 10 minutes"},{"find":" End with the weather.","replace":""}]}`)
	if patched.IsError {
		t.Fatalf("schedule_update = %+v", patched)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	for _, want := range []string{
		`Change "Morning trains"`,
		"Runs: 30 7 * * 1-5 (Europe/Amsterdam)",
		"Its instructions change only here:\n1.\nWas: over 5 minutes\nNow: over 10 minutes\n2.\nWas:  End with the weather.\nNow: (removed)",
	} {
		if !strings.Contains(proposal.text, want) {
			t.Fatalf("proposal lacks %q: %s", want, proposal.text)
		}
	}
	if strings.Contains(proposal.text, "Utrecht Centraal") {
		t.Fatalf("a patch showed the unchanged instructions: %s", proposal.text)
	}

	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Changed. Next run:") {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 2)
	stored, err := h.scheduler.Jobs(h.ctx, "alice")
	if err != nil || len(stored) != 1 {
		t.Fatalf("jobs = %+v %v", stored, err)
	}
	want := "Check the trains from Utrecht Centraal to Den Haag Centraal. Report delays over 10 minutes. Report cancellations."
	if got := schedule.ModelTurnPrompt(stored[0].Config); got != want {
		t.Fatalf("patched prompt = %q, want %q", got, want)
	}
}

func (h *conversationHarness) nextAnswer(updateID int64) string {
	h.t.Helper()
	answer := receive(h.t, h.ctx, h.answers)
	if answer.id != callbackID(updateID) {
		h.t.Fatalf("answered callback %+v, want %s", answer, callbackID(updateID))
	}
	return answer.text
}

// A proposed job can be previewed before it is confirmed: the preview runs its prompt once as a
// scheduled run would, posts the report under the proposal, and saves nothing. One preview runs per
// bundle at a time, a retried press starts no second one, and another user's press does nothing.
func TestProposedJobIsPreviewedWithoutSavingAnything(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	h.push(textUpdate(1, 101, "every monday at 9 tell me what is overdue"))
	chat := receive(t, h.ctx, runtime.calls)
	if proposed := toolCaller(t, chat)("schedule_propose", `{"name":"Weekly tasks","cron":"0 9 * * 1","prompt":"List my overdue Notion tasks."}`); proposed.IsError {
		t.Fatalf("schedule_propose = %+v", proposed)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	h.waitMessageRecorded(proposal.messageID)
	data := buttonData(proposal.keyboard)
	if len(data) != 3 || !strings.HasPrefix(data[2], "approval:preview:") {
		t.Fatalf("proposal buttons = %v", data)
	}
	preview := data[2]

	h.push(callbackUpdate(2, 101, proposal.messageID, preview))
	if answer := h.nextAnswer(2); answer != "Running a preview; its report will follow here. Nothing is saved." {
		t.Fatalf("preview press answer = %q", answer)
	}
	run := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(run.request.Message, `scheduled job "Weekly tasks"`) || !strings.HasSuffix(run.request.Message, "List my overdue Notion tasks.") {
		t.Fatalf("preview message = %q", run.request.Message)
	}
	if names := toolNames(run.request.Tools); slices.Contains(names, "schedule_propose") || !slices.Contains(names, "schedule_list") {
		t.Fatalf("preview tools = %v", names)
	}

	h.push(callbackUpdate(3, 101, proposal.messageID, preview))
	if answer := h.nextAnswer(3); answer != "A preview of this batch is already running; its report will follow here." {
		t.Fatalf("second preview press answer = %q", answer)
	}
	if again, err := h.approvals.Preview(h.ctx, "alice", approvalID(preview), pressTrigger(Bot{ID: 999}, 2), 101, proposal.messageID); err != nil || again.Text != "Running a preview; its report will follow here. Nothing is saved." {
		t.Fatalf("retried preview press = %+v %v", again, err)
	}
	h.push(callbackUpdate(4, 202, proposal.messageID, preview))
	if answer := h.nextAnswer(4); answer != "" {
		t.Fatalf("another user's preview press answer = %q", answer)
	}
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)

	run.answer <- agentruntime.RunResult{Response: "Two tasks are overdue.", Final: "Two tasks are overdue."}
	report := h.nextMessage()
	if report.chatID != 101 || report.replyTo != proposal.messageID ||
		report.text != "<b>Preview: Weekly tasks</b>\n\nTwo tasks are overdue.\n\nPreview only: nothing is scheduled or changed until you confirm." {
		t.Fatalf("preview report = %+v", report)
	}
	h.expectNoEdit(50 * time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("a preview created %d jobs", count)
	}
	if count := h.countRows(`SELECT count(*) FROM schedule_runs`); count != 0 {
		t.Fatalf("a preview recorded %d runs", count)
	}
	if status, _ := h.approvalStatus(approvalID(preview)); status != "pending" {
		t.Fatalf("proposal after preview = %s", status)
	}

	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(5, 101, proposal.messageID, confirm))
	h.wantAnswer(5)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Scheduled. First run:") || len(edit.keyboard) != 0 {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 5)
	h.push(callbackUpdate(6, 101, proposal.messageID, preview))
	if answer := h.nextAnswer(6); answer != "This batch is no longer waiting for confirmation, so there is nothing to preview." {
		t.Fatalf("preview press after confirm = %q", answer)
	}
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM schedule_runs`); count != 0 {
		t.Fatalf("runs after confirm = %d, want 0", count)
	}

	// A change that leaves the instructions alone has nothing to preview.
	h.push(textUpdate(7, 101, "rename it"))
	chat = receive(t, h.ctx, runtime.calls)
	if proposed := toolCaller(t, chat)("schedule_update", `{"job_id":"`+approvalID(confirm)+`","name":"Overdue tasks"}`); proposed.IsError {
		t.Fatalf("schedule_update = %+v", proposed)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 7, "proposed")
	h.waitState(7, "delivered")
	if rename := h.nextMessage(); len(buttonData(rename.keyboard)) != 2 {
		t.Fatalf("rename buttons = %v", buttonData(rename.keyboard))
	}
}

// The model withdraws part of a waiting batch: the message shows the item as withdrawn and keeps
// its buttons for the rest, Confirm runs only what is left, and a batch withdrawn before it was
// sent shows only what remains. A batch with nothing left loses its buttons.
func TestModelWithdrawsPartOfABatch(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "set up three weekly reports"))
	chat := receive(t, h.ctx, runtime.calls)
	call := toolCaller(t, chat)
	for _, name := range []string{"Report A", "Report B", "Report C"} {
		if proposed := call("schedule_propose", `{"name":"`+name+`","cron":"0 9 * * 1","prompt":"Summarise `+name+`."}`); proposed.IsError {
			t.Fatalf("schedule_propose = %+v", proposed)
		}
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	h.waitMessageRecorded(proposal.messageID)

	h.push(textUpdate(2, 101, "drop the second one"))
	chat = receive(t, h.ctx, runtime.calls)
	call = toolCaller(t, chat)
	withdrawn := call("proposal_withdraw", `{"items":[2]}`)
	if withdrawn.IsError || !strings.Contains(withdrawn.Text, `Withdrew item 2 (schedule_propose: Schedule "Report B")`) ||
		!strings.Contains(withdrawn.Text, `1. schedule_propose: Schedule "Report A"; 3. schedule_propose: Schedule "Report C"`) {
		t.Fatalf("proposal_withdraw = %+v", withdrawn)
	}
	edit := h.nextEdit()
	if edit.messageID != proposal.messageID || !strings.Contains(edit.text, "Report B.\nWithdrawn by the assistant; nothing was written.") {
		t.Fatalf("message after withdrawal = %+v", edit)
	}
	confirm, _ := approvalButtons(t, edit.keyboard)
	if again := call("proposal_withdraw", `{"items":[2]}`); !again.IsError || !strings.Contains(again.Text, "Item 2 is no longer waiting") {
		t.Fatalf("second withdrawal = %+v", again)
	}
	if invalid := call("proposal_withdraw", `{"items":[4]}`); !invalid.IsError || !strings.Contains(invalid.Text, "There is no item 4; the batch has 3.") {
		t.Fatalf("withdrawal of a missing item = %+v", invalid)
	}
	chat.answer <- agentruntime.RunResult{Response: "withdrawn"}
	h.wantReply(101, 2, "withdrawn")
	h.waitState(2, "delivered")

	h.push(callbackUpdate(3, 101, proposal.messageID, confirm))
	h.wantAnswer(3)
	if edit := h.nextEdit(); len(edit.keyboard) != 0 || strings.Count(edit.text, "Done. Scheduled.") != 2 || !strings.Contains(edit.text, "Withdrawn by the assistant") {
		t.Fatalf("message after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 3)
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 2 {
		t.Fatalf("jobs after confirm = %d, want 2", count)
	}
	if count := h.countRows(`SELECT count(*) FROM approvals WHERE status = 'rejected' AND reason = 'withdrawn'`); count != 1 {
		t.Fatalf("withdrawn approvals = %d, want 1", count)
	}

	// Withdrawn before the batch is sent, an item never reaches the user.
	h.push(textUpdate(4, 101, "two more"))
	chat = receive(t, h.ctx, runtime.calls)
	call = toolCaller(t, chat)
	for _, name := range []string{"Report D", "Report E"} {
		if proposed := call("schedule_propose", `{"name":"`+name+`","cron":"0 10 * * 1","prompt":"Summarise `+name+`."}`); proposed.IsError {
			t.Fatalf("schedule_propose = %+v", proposed)
		}
	}
	if withdrawn := call("proposal_withdraw", `{"items":[1]}`); withdrawn.IsError || !strings.Contains(withdrawn.Text, `numbered as the user sees them: 1. schedule_propose: Schedule "Report E"`) {
		t.Fatalf("withdrawal before sending = %+v", withdrawn)
	}
	if proposed := call("schedule_propose", `{"name":"Report F","cron":"0 11 * * 1","prompt":"Summarise Report F."}`); proposed.IsError {
		t.Fatalf("schedule_propose after a withdrawal = %+v", proposed)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 4, "proposed")
	h.waitState(4, "delivered")
	second := h.nextMessage()
	if strings.Contains(second.text, "Report D") || !strings.HasPrefix(second.text, "2 changes") || !strings.Contains(second.text, "Report E") || !strings.Contains(second.text, "Report F") {
		t.Fatalf("batch after a withdrawal before sending = %s", second.text)
	}
	h.waitMessageRecorded(second.messageID)

	h.push(textUpdate(5, 101, "forget all of it"))
	chat = receive(t, h.ctx, runtime.calls)
	if withdrawn := toolCaller(t, chat)("proposal_withdraw", `{"all":true}`); withdrawn.IsError || !strings.Contains(withdrawn.Text, "Nothing is waiting for the user's confirmation any more.") {
		t.Fatalf("withdraw all = %+v", withdrawn)
	}
	if edit := h.nextEdit(); edit.messageID != second.messageID || len(edit.keyboard) != 0 || !strings.Contains(edit.text, "Withdrawn by the assistant; nothing was written.") {
		t.Fatalf("message after withdrawing all = %+v", edit)
	}
	chat.answer <- agentruntime.RunResult{Response: "done"}
	h.wantReply(101, 5, "done")
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 2 {
		t.Fatalf("jobs after withdrawing all = %d, want 2", count)
	}
}

package telegram

import (
	"encoding/json"
	"strings"
	"testing"

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

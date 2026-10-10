package telegram

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

// react calls the turn's react tool and fails the test unless the reaction was set.
func react(t *testing.T, call runtimeCall, emoji string) {
	t.Helper()
	result, err := call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: "react", Arguments: json.RawMessage(`{"emoji":"` + emoji + `"}`)})
	if err != nil || result.IsError || !strings.Contains(result.Text, "The reaction is set") {
		t.Fatalf("react %s = %+v %v", emoji, result, err)
	}
}

// A turn that only reacts sends no text, yet is delivered and still sends the proposals it made.
func TestReactOnlyTurnSendsNothingAndStillRunsTheFollowUp(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, reactions: true})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "got it, and check my train at 9"))
	chat := receive(t, h.ctx, runtime.calls)
	if !slices.Contains(toolNames(chat.request.Tools), "react") || !strings.Contains(chat.request.Tools.Guidance, "react and then reply with exactly "+conversation.NothingToAdd) {
		t.Fatalf("chat turn tools = %v, guidance %q", toolNames(chat.request.Tools), chat.request.Tools.Guidance)
	}
	react(t, chat, "👍")
	result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{
		Name:      "schedule_propose",
		Arguments: json.RawMessage(`{"name":"Train check","at":"2026-09-28 09:00","prompt":"Is my 9:30 train delayed?"}`),
	})
	if err != nil || result.IsError {
		t.Fatalf("proposal = %+v %v", result, err)
	}
	chat.answer <- agentruntime.RunResult{Response: conversation.NothingToAdd}
	h.waitState(1, "delivered")
	if proposal := h.nextMessage(); proposal.replyTo != 0 || !strings.Contains(proposal.text, "Runs once: Mon 28 Sep 09:00 CEST") {
		t.Fatalf("follow-up message = %+v", proposal)
	}
	if sent := h.sentTexts(); len(sent) != 1 {
		t.Fatalf("a react-only turn sent %d messages: %q", len(sent), sent)
	}
	if got := h.setReactions(); !slices.Equal(got, []setReaction{{chatID: 101, messageID: requestMessageID(1), emoji: "👍"}}) {
		t.Fatalf("reactions = %+v", got)
	}
	if count := h.reactionCount("👍"); count != 1 {
		t.Fatalf("counted %d 👍 reactions, want 1", count)
	}
}

// A reaction can go with a reply; a later reaction replaces it, and only the curated emoji are set.
func TestReactionWithReplySendsBoth(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{reactions: true})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "I passed my exam!"))
	chat := receive(t, h.ctx, runtime.calls)
	result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{Name: "react", Arguments: json.RawMessage(`{"emoji":"👎"}`)})
	if err != nil || !result.IsError {
		t.Fatalf("react with an emoji outside the set = %+v %v", result, err)
	}
	react(t, chat, "🎉")
	react(t, chat, "❤")
	chat.answer <- agentruntime.RunResult{Response: "Congratulations!"}
	h.wantReply(101, 1, "Congratulations!")
	h.waitState(1, "delivered")
	want := []setReaction{{chatID: 101, messageID: requestMessageID(1), emoji: "🎉"}, {chatID: 101, messageID: requestMessageID(1), emoji: "❤"}}
	if got := h.setReactions(); !slices.Equal(got, want) {
		t.Fatalf("reactions = %+v, want %+v", got, want)
	}
}

// A react-only turn that returned to an earlier conversation sends nothing, not even the line that
// marks the return.
func TestReactOnlyTurnInAResumedConversationSendsNothing(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{reactions: true})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "which plants need water on Sundays"))
	first := receive(t, h.ctx, runtime.calls)
	first.answer <- agentruntime.RunResult{Response: "The ferns."}
	earlierAnswer := h.nextMessage()
	h.waitState(1, "delivered")
	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)
	h.push(textUpdate(3, 101, "book a table for Friday"))
	later := receive(t, h.ctx, runtime.calls)
	later.answer <- agentruntime.RunResult{Response: "Booked."}
	h.wantReply(101, 3, "Booked.")
	h.waitState(3, "delivered")

	thanks := textUpdate(4, 101, "понял")
	thanks.Message.ReplyToMessage = &Message{MessageID: earlierAnswer.messageID}
	h.push(thanks)
	resumed := receive(t, h.ctx, runtime.calls)
	if resumed.request.SessionID != first.request.SessionID {
		t.Fatalf("reply continued in %s, want the earlier %s", resumed.request.SessionID, first.request.SessionID)
	}
	react(t, resumed, "👌")
	resumed.answer <- agentruntime.RunResult{Response: conversation.NothingToAdd}
	h.waitState(4, "delivered")
	if pending := len(h.messages); pending != 0 {
		t.Fatalf("a react-only resumed turn sent %d messages: %+v", pending, h.nextMessage())
	}
	if got := h.setReactions(); !slices.Equal(got, []setReaction{{chatID: 101, messageID: requestMessageID(4), emoji: "👌"}}) {
		t.Fatalf("reactions = %+v", got)
	}
}

// The turn after Confirm and a scheduled run answer no message the user wrote, so neither can react.
func TestContinuationAndScheduledRunsAreNotOfferedReact(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, reactions: true})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "at 9 check whether my train is delayed"))
	chat := receive(t, h.ctx, runtime.calls)
	result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{
		Name:      "schedule_propose",
		Arguments: json.RawMessage(`{"name":"Train check","at":"2026-09-28 09:00","prompt":"Is my 9:30 train delayed?"}`),
	})
	if err != nil || result.IsError {
		t.Fatalf("proposal = %+v %v", result, err)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	h.nextEdit()

	continuation := receive(t, h.ctx, runtime.calls)
	if !strings.HasSuffix(continuation.request.Message, conversation.ContinueMessage) {
		t.Fatalf("continuation message = %q", continuation.request.Message)
	}
	if names := toolNames(continuation.request.Tools); slices.Contains(names, "react") || strings.Contains(continuation.request.Tools.Guidance, "react sets") {
		t.Fatalf("continuation tools = %v", names)
	}
	if result, err := continuation.request.Tools.Executor.Execute(continuation.ctx, agentruntime.ToolCall{Name: "react", Arguments: json.RawMessage(`{"emoji":"👍"}`)}); err != nil || !result.IsError {
		t.Fatalf("react in a continuation = %+v %v", result, err)
	}
	continuation.answer <- agentruntime.RunResult{Response: conversation.NothingToAdd}
	h.waitState(2, "delivered")

	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if names := toolNames(run.request.Tools); slices.Contains(names, "react") {
		t.Fatalf("scheduled run tools = %v", names)
	}
	run.answer <- agentruntime.RunResult{Response: "No delay.", Final: "No delay."}
	h.wantMessage(101, "<b>Scheduled: Train check</b>\n\nNo delay.")
	h.waitRun(h.jobID("alice"), schedule.StatusDelivered)
	if got := h.setReactions(); len(got) != 0 {
		t.Fatalf("reactions = %+v", got)
	}
}

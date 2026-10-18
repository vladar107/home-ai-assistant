package telegram

import (
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/memory"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const learnedPrefix = "Learned from your 👎 on "

// thumbsDown answers a chat turn, marks the reply 👎 with the given update, and returns the
// reconsider turn it starts.
func (h *conversationHarness) thumbsDown(runtime *controlledRuntime, askID, reactID int64) runtimeCall {
	h.t.Helper()
	h.push(textUpdate(askID, 101, "how far is the walk to the station"))
	chat := receive(h.t, h.ctx, runtime.calls)
	chat.answer <- agentruntime.RunResult{Response: "About 2 miles."}
	reply := h.nextMessage()
	h.waitState(askID, "delivered")
	h.push(reactionUpdate(reactID, 101, reply.messageID, "👎"))
	reconsider := receive(h.t, h.ctx, runtime.calls)
	if !strings.HasPrefix(reconsider.request.Message, prompt.Reconsider) {
		h.t.Fatalf("reconsider message = %q", reconsider.request.Message)
	}
	return reconsider
}

func callTool(t *testing.T, call runtimeCall, toolCall agentruntime.ToolCall) agentruntime.ToolResult {
	t.Helper()
	result, err := call.request.Tools.Executor.Execute(call.ctx, toolCall)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// A 👎 starts a turn that may propose one instruction; the card names the 👎, Confirm keeps it as
// learned, and /memory marks it.
func TestThumbsDownProposesOneLearnedInstruction(t *testing.T) {
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)

	reconsider := h.thumbsDown(runtime, 1, 2)
	if !strings.Contains(reconsider.request.Message, "propose one memory instruction") {
		t.Fatalf("reconsider message lacks the learning step: %q", reconsider.request.Message)
	}
	if result := callTool(t, reconsider, addCall("instruction", "Use metric units for distances.")); result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
		t.Fatalf("lesson = %+v", result)
	}
	if result := callTool(t, reconsider, addCall("instruction", "Answer briefly.")); !result.IsError || !strings.Contains(result.Text, "only one instruction") {
		t.Fatalf("second lesson = %+v", result)
	}
	reconsider.answer <- agentruntime.RunResult{Response: "Sorry, about 3 km."}
	if answer := h.nextMessage(); answer.text != "Sorry, about 3 km." {
		t.Fatalf("reconsider answer = %+v", answer)
	}
	h.waitState(2, "delivered")
	proposal := h.nextMessage()
	if !strings.HasPrefix(proposal.text, learnedPrefix) || !strings.Contains(proposal.text, "Use metric units for distances.") || strings.Contains(proposal.text, "Answer briefly.") {
		t.Fatalf("lesson card = %+v", proposal)
	}
	if pending := len(h.messages); pending != 0 {
		t.Fatalf("the refused lesson sent %d more messages", pending)
	}

	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(3, 101, proposal.messageID, confirm))
	h.wantAnswer(3)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Remembered.") {
		t.Fatalf("lesson after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 3)
	entries := h.memoryEntries("alice")
	if len(entries) != 1 || entries[0].Kind != memory.KindInstruction || entries[0].Source != memory.SourceLearned {
		t.Fatalf("entries after confirm = %+v", entries)
	}

	h.push(textUpdate(4, 101, "/memory"))
	if list := h.nextMessage(); !strings.Contains(list.text, "1. Use metric units for distances. (learned)") {
		t.Fatalf("/memory = %s", list.text)
	}
}

// With learning from 👎 off, the reconsider turn is not asked for a lesson and cannot propose one.
func TestLearningOffLeavesTheReconsiderTurnWithoutALesson(t *testing.T) {
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)

	h.requestSettings(1, 101)
	page := h.openSettingsPage(2, 101, settingsMessageID(1), "memory")
	if !strings.Contains(page.text, MemoryLearnOnLine) {
		t.Fatalf("memory page = %+v", page)
	}
	if off := h.press(3, 101, settingsMessageID(1), "memory:learn_off"); !strings.Contains(off.text, MemoryLearnOffLine) || !strings.Contains(off.text, MemoryOnLine) {
		t.Fatalf("after turning learning off = %+v", off)
	}

	reconsider := h.thumbsDown(runtime, 4, 5)
	if strings.Contains(reconsider.request.Message, "memory") {
		t.Fatalf("reconsider message with learning off = %q", reconsider.request.Message)
	}
	if result := callTool(t, reconsider, addCall("instruction", "Use metric units for distances.")); !result.IsError || !strings.Contains(result.Text, "turned off learning") {
		t.Fatalf("lesson with learning off = %+v", result)
	}
	reconsider.answer <- agentruntime.RunResult{Response: "Sorry, about 3 km."}
	h.nextMessage()
	h.waitState(5, "delivered")
	if pending := len(h.messages); pending != 0 {
		t.Fatalf("a refused lesson sent %d messages", pending)
	}
	if entries := h.memoryEntries("alice"); len(entries) != 0 {
		t.Fatalf("entries = %+v", entries)
	}
}

// An instruction kept in an ordinary chat turn, even one after a reconsider, is not a lesson: it is
// written at once, as the model's own.
func TestInstructionInAChatTurnIsNotALesson(t *testing.T) {
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)

	reconsider := h.thumbsDown(runtime, 1, 2)
	reconsider.answer <- agentruntime.RunResult{Response: "Sorry, about 3 km."}
	h.nextMessage()
	h.waitState(2, "delivered")

	h.memoryTurn(runtime, 3, 101, "ok", addCall("instruction", "Always use 24-hour times."))
	done := h.nextMessage()
	if strings.Contains(done.text, "Learned from") || !strings.HasPrefix(done.text, "Done:\n\nRemember this instruction") {
		t.Fatalf("chat done message = %+v", done)
	}
	if entries := h.memoryEntries("alice"); len(entries) != 1 || entries[0].Source != memory.SourceModel {
		t.Fatalf("entries = %+v", entries)
	}
}

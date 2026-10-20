package telegram

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/memory"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

const (
	tableTitle = "Book a table at a restaurant"
	tableSteps = "1. places_search with the cuisine and the area.\n2. Check the opening hours before suggesting a time."
)

func saveCall(title, steps, id string) agentruntime.ToolCall {
	if id == "" {
		return memoryCall("procedure_save", fmt.Sprintf(`{"title":%q,"steps":%q}`, title, steps))
	}
	return memoryCall("procedure_save", fmt.Sprintf(`{"title":%q,"steps":%q,"id":%q}`, title, steps, id))
}

func readCall(id string) agentruntime.ToolCall {
	return memoryCall("procedure_read", fmt.Sprintf(`{"id":%q}`, id))
}

func (h *conversationHarness) procedures(principalID string) []memory.Procedure {
	h.t.Helper()
	procedures, err := h.memory.Procedures(h.ctx, principalID)
	if err != nil {
		h.t.Fatal(err)
	}
	return procedures
}

func (h *conversationHarness) seedProcedure(principalID, id, title, steps string) {
	h.t.Helper()
	now := time.Now()
	if err := h.store.AddProcedure(h.ctx, memory.Procedure{ID: id, PrincipalID: principalID, Title: title, Steps: steps, Source: memory.SourceConfirmed, CreatedAt: now, UpdatedAt: now}); err != nil {
		h.t.Fatal(err)
	}
}

// confirmProposal presses Confirm on the proposal card and answers the turn it queues.
func (h *conversationHarness) confirmProposal(runtime *controlledRuntime, updateID int64, proposal sentMessage, wantOutcome string) {
	h.t.Helper()
	confirm, _ := approvalButtons(h.t, proposal.keyboard)
	h.push(callbackUpdate(updateID, 101, proposal.messageID, confirm))
	h.wantAnswer(updateID)
	if edit := h.nextEdit(); !strings.Contains(edit.text, wantOutcome) {
		h.t.Fatalf("proposal after confirm = %+v, want %q", edit, wantOutcome)
	}
	h.continueTurn(runtime, updateID)
}

// A procedure is kept only once the user confirms the card that shows its steps, and later turns
// list its title alone.
func TestProcedureIsKeptAfterConfirmAndListedByTitle(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)

	request, results := h.memoryTurn(runtime, 1, 101, "ok", saveCall("  Book a table   at a restaurant ", "1. places_search with the cuisine and the area.\n\n  2. Check the opening   hours before suggesting a time.  ", ""))
	if !slices.Contains(toolNames(request.Tools), "procedure_save") || slices.Contains(toolNames(request.Tools), "procedure_read") || !strings.Contains(request.Tools.Guidance, "procedure_save only when") {
		t.Fatalf("tools without procedures = %v", toolNames(request.Tools))
	}
	if len(results) != 1 || results[0].IsError || !strings.Contains(results[0].Text, "awaiting the user's confirmation") {
		t.Fatalf("save result = %+v", results)
	}
	proposal := h.nextMessage()
	if !strings.Contains(proposal.text, "Keep this procedure: "+tableTitle+"\n"+tableSteps) {
		t.Fatalf("proposal = %s", proposal.text)
	}
	if procedures := h.procedures("alice"); len(procedures) != 0 {
		t.Fatalf("procedures before confirm = %+v", procedures)
	}

	h.confirmProposal(runtime, 2, proposal, "Kept the procedure.")
	procedures := h.procedures("alice")
	if len(procedures) != 1 || procedures[0].Title != tableTitle || procedures[0].Steps != tableSteps || procedures[0].Source != memory.SourceConfirmed {
		t.Fatalf("procedures after confirm = %+v", procedures)
	}

	request, _ = h.memoryTurn(runtime, 3, 101, "ok")
	if !strings.Contains(request.Memory, "Procedures you have kept (read one with procedure_read before following it):\n- ["+procedures[0].ID+"] "+tableTitle) {
		t.Fatalf("memory = %q", request.Memory)
	}
	if strings.Contains(request.Memory, "places_search") {
		t.Fatalf("memory carries the steps: %q", request.Memory)
	}
	if !slices.Contains(toolNames(request.Tools), "procedure_read") {
		t.Fatalf("tools with a procedure = %v", toolNames(request.Tools))
	}

	request, _ = h.memoryTurn(runtime, 4, 202, "ok")
	if request.Memory != "" || slices.Contains(toolNames(request.Tools), "procedure_read") {
		t.Fatalf("another user's turn: memory %q tools %v", request.Memory, toolNames(request.Tools))
	}
}

// procedure_read returns the steps in a chat turn and in a background task, which cannot propose
// one, and stamps when the procedure was read.
func TestProcedureReadWorksInChatAndInATask(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)
	h.seedProcedure("alice", "ccccccc1", tableTitle, tableSteps)
	h.seedProcedure("alice", "ccccccc2", "Plan a train trip", "1. Look up the departures first.")
	h.seedProcedure("bob", "ddddddd1", "Bob's procedure", "bob's steps")

	_, results := h.memoryTurn(runtime, 1, 101, "ok", readCall("ccccccc1"), readCall("ddddddd1"))
	// The steps are the user's own confirmed text, so they are not framed as untrusted data.
	if results[0].IsError || results[0].Text != prompt.ToolResultConfirmed+"Procedure: "+tableTitle+"\n"+tableSteps {
		t.Fatalf("read result = %+v", results[0])
	}
	if !results[1].IsError || !strings.Contains(results[1].Text, "No procedure has that id") {
		t.Fatalf("another user's procedure = %+v", results[1])
	}
	if procedures := h.procedures("alice"); procedures[0].ReadAt.IsZero() || !procedures[1].ReadAt.IsZero() {
		t.Fatalf("read stamps = %v and %v, want only the read one stamped", procedures[0].ReadAt, procedures[1].ReadAt)
	}
	if read := h.procedures("bob")[0].ReadAt; !read.IsZero() {
		t.Fatalf("bob's procedure was stamped at %v", read)
	}

	replies := make(chan turn.TaskReply, 1)
	go func() {
		reply, err := h.engine.RunTask(h.ctx, turn.TaskRun{
			PrincipalID: "alice", Turn: outbox.TaskTurn("eeee"), SessionID: "55555555-5555-4555-8555-555555555555",
			Message: "find a restaurant", CallBudget: 4,
		})
		if err != nil {
			t.Error(err)
		}
		replies <- reply
	}()
	call := receive(t, h.ctx, runtime.calls)
	names := toolNames(call.request.Tools)
	if !slices.Contains(names, "procedure_read") || slices.Contains(names, "procedure_save") {
		t.Fatalf("task tools = %v", names)
	}
	if guidance := call.request.Tools.Guidance; !strings.Contains(guidance, "read it with procedure_read") || strings.Contains(guidance, "procedure_save") || strings.Contains(guidance, "memory_add") {
		t.Fatalf("task guidance = %q", guidance)
	}
	if !strings.Contains(call.request.Memory, "[ccccccc2] Plan a train trip") {
		t.Fatalf("task memory = %q", call.request.Memory)
	}
	result, err := call.request.Tools.Executor.Execute(call.ctx, readCall("ccccccc2"))
	if err != nil || result.IsError || result.Text != prompt.ToolResultConfirmed+"Procedure: Plan a train trip\n1. Look up the departures first." {
		t.Fatalf("task read = %+v %v", result, err)
	}
	call.answer <- agentruntime.RunResult{Response: "booked", Final: "booked"}
	if reply := receive(t, h.ctx, replies); reply.Text != "booked" {
		t.Fatalf("task reply = %+v", reply)
	}
	if read := h.procedures("alice")[1].ReadAt; read.IsZero() {
		t.Fatal("the task's read was not stamped")
	}
}

// Saving with an id replaces that procedure, and a principal keeping the most there may be is
// refused a new one.
func TestProcedureSaveReplacesByIDAndCapsTheCount(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)
	for n := 1; n <= memory.MaximumProcedures; n++ {
		h.seedProcedure("alice", fmt.Sprintf("ccccc%03d", n), fmt.Sprintf("Procedure %d", n), "steps")
	}

	_, results := h.memoryTurn(runtime, 1, 101, "ok",
		saveCall("Procedure 21", "steps", ""),
		saveCall("Procedure 2", "other steps", "ccccc001"),
		saveCall("Anything", "steps", "missing1"),
		saveCall(strings.Repeat("t", 81), "steps", ""),
		saveCall("Long steps", strings.Repeat("s", 1001), "ccccc001"),
		saveCall("Procedure 1, better", "1. Search first.\n2. Then ask.", "ccccc001"),
	)
	if !results[0].IsError || !strings.Contains(results[0].Text, "keeps 20 procedures") {
		t.Fatalf("21st = %+v", results[0])
	}
	if !results[1].IsError || !strings.Contains(results[1].Text, "id ccccc002") {
		t.Fatalf("duplicate title = %+v", results[1])
	}
	if !results[2].IsError || !strings.Contains(results[2].Text, "No procedure has that id") {
		t.Fatalf("unknown id = %+v", results[2])
	}
	if !results[3].IsError || !strings.Contains(results[3].Text, "at most 80") || !results[4].IsError || !strings.Contains(results[4].Text, "at most 1000") {
		t.Fatalf("oversized = %+v %+v", results[3], results[4])
	}
	if results[5].IsError {
		t.Fatalf("replacement = %+v", results[5])
	}
	proposal := h.nextMessage()
	if !strings.Contains(proposal.text, `Replace the procedure "Procedure 1" with this one: Procedure 1, better`+"\n1. Search first.\n2. Then ask.") {
		t.Fatalf("proposal = %s", proposal.text)
	}

	h.confirmProposal(runtime, 2, proposal, "Updated the procedure.")
	procedures := h.procedures("alice")
	if len(procedures) != memory.MaximumProcedures || procedures[0].ID != "ccccc001" || procedures[0].Title != "Procedure 1, better" || procedures[0].Steps != "1. Search first.\n2. Then ask." {
		t.Fatalf("procedures after replacing = %d %+v", len(procedures), procedures[0])
	}
}

// A procedure proposed after the conversation read outside content always carries a warning. Its
// steps are never screened: a procedure addresses the assistant by design, so the screener would
// flag nearly every one.
func TestProcedureAfterOutsideContentCarriesAWarning(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)
	screener := &assistantScreener{}
	h.memory.SetScreener(screener)

	_, results := h.memoryTurn(runtime, 1, 101, "ok",
		saveCall(tableTitle, tableSteps, ""),
		memoryCall("read_outside", `{}`),
		saveCall("Plan a train trip", "1. Look up the departures first.", ""),
	)
	for _, result := range []agentruntime.ToolResult{results[0], results[2]} {
		if result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
			t.Fatalf("save result = %+v", result)
		}
	}
	if screened := screener.texts(); len(screened) != 0 {
		t.Fatalf("screened texts = %q, want none", screened)
	}
	proposal := h.nextMessage()
	const warning = "⚠️ I wrote these steps after this conversation read outside content. Check that they describe how you want this done before confirming.\nKeep this procedure: Plan a train trip"
	if strings.Count(proposal.text, "read outside content") != 1 || !strings.Contains(proposal.text, warning) {
		t.Fatalf("proposal = %s", proposal.text)
	}
	if clean := strings.Index(proposal.text, "Keep this procedure: "+tableTitle); clean < 0 || clean > strings.Index(proposal.text, warning) {
		t.Fatalf("the clean procedure is missing or carries the warning: %s", proposal.text)
	}
}

func TestMemoryCommandListsAndDeletesProcedures(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)
	h.seedMemory("alice", "aaaaaaa1", memory.KindFact, "likes tea")
	h.seedProcedure("alice", "ccccccc1", tableTitle, tableSteps)
	h.seedProcedure("alice", "ccccccc2", "Plan a train trip", "steps")
	h.seedProcedure("bob", "ddddddd1", "Bob's procedure", "steps")

	h.push(textUpdate(1, 101, "/memory"))
	list := h.nextMessage()
	if !strings.Contains(list.text, "Facts\n1. likes tea\n\nProcedures\n2. "+tableTitle+"\n3. Plan a train trip") || strings.Contains(list.text, "places_search") || strings.Contains(list.text, "Bob's") {
		t.Fatalf("/memory = %s", list.text)
	}
	data := buttonData(list.keyboard)
	for _, want := range []string{"mem:del:aaaaaaa1:1", "mem:pdel:ccccccc1:1", "mem:pdel:ccccccc2:1"} {
		if !slices.Contains(data, want) {
			t.Fatalf("buttons %v lack %s", data, want)
		}
	}

	h.push(callbackUpdate(2, 101, settingsMessageID(1), "mem:pdel:ccccccc1:1"))
	h.wantAnswer(2)
	if edit := h.nextEdit(); strings.Contains(edit.text, tableTitle) || !strings.Contains(edit.text, "Procedures\n2. Plan a train trip") {
		t.Fatalf("list after delete = %+v", edit)
	}
	if procedures := h.procedures("alice"); len(procedures) != 1 || procedures[0].ID != "ccccccc2" {
		t.Fatalf("procedures after delete = %+v", procedures)
	}

	h.push(callbackUpdate(3, 101, settingsMessageID(1), "mem:pdel:ddddddd1:1"))
	h.wantAnswer(3)
	h.nextEdit()
	h.wantMessage(101, MemoryGoneReply)
	if procedures := h.procedures("bob"); len(procedures) != 1 {
		t.Fatalf("bob's procedures after alice's forged press = %+v", procedures)
	}

	h.push(callbackUpdate(4, 101, settingsMessageID(1), "mem:ask:1"))
	h.wantAnswer(4)
	if ask := h.nextEdit(); !strings.Contains(ask.text, "Forget all 2 items?") {
		t.Fatalf("confirmation = %+v", ask)
	}
	h.push(callbackUpdate(5, 101, settingsMessageID(1), "mem:clear:1"))
	h.wantAnswer(5)
	if cleared := h.nextEdit(); cleared.text != NoMemoryReply {
		t.Fatalf("after clearing = %+v", cleared)
	}
	h.wantMessage(101, MemoryClearedNote)
	if procedures := h.procedures("alice"); len(procedures) != 0 {
		t.Fatalf("alice's procedures = %+v", procedures)
	}
	if procedures := h.procedures("bob"); len(procedures) != 1 {
		t.Fatalf("bob's procedures = %+v", procedures)
	}
}

// With "Learn procedures" off the kept ones are still listed and read but none is proposed; with
// memory off, neither tool is offered.
func TestLearnProceduresSwitchAndMemorySwitch(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newMemoryHarness(t, runtime)
	h.seedProcedure("alice", "ccccccc1", tableTitle, tableSteps)

	h.requestSettings(1, 101)
	page := h.openSettingsPage(2, 101, settingsMessageID(1), "memory")
	if !strings.Contains(page.text, MemoryProceduresOnLine) || !slices.Contains(buttonData(page.keyboard), "memory:procedures_off") {
		t.Fatalf("memory page = %+v", page)
	}
	off := h.press(3, 101, settingsMessageID(1), "memory:procedures_off")
	if !strings.Contains(off.text, MemoryProceduresOffLine) || !strings.Contains(off.text, MemoryLearnOnLine) || !slices.Contains(buttonData(off.keyboard), "memory:procedures_on") {
		t.Fatalf("after turning procedures off = %+v", off)
	}

	request, results := h.memoryTurn(runtime, 4, 101, "ok", readCall("ccccccc1"))
	names := toolNames(request.Tools)
	if slices.Contains(names, "procedure_save") || strings.Contains(request.Tools.Guidance, "procedure_save") || !slices.Contains(names, "procedure_read") || !slices.Contains(names, "memory_add") {
		t.Fatalf("tools with procedures off = %v", names)
	}
	if results[0].IsError || !strings.Contains(results[0].Text, tableSteps) {
		t.Fatalf("read with procedures off = %+v", results[0])
	}
	if !strings.Contains(request.Memory, tableTitle) {
		t.Fatalf("memory with procedures off = %q", request.Memory)
	}

	if on := h.press(5, 101, settingsMessageID(1), "memory:procedures_on"); !strings.Contains(on.text, MemoryProceduresOnLine) {
		t.Fatalf("after turning procedures on = %+v", on)
	}
	h.press(6, 101, settingsMessageID(1), "memory:off")
	request, _ = h.memoryTurn(runtime, 7, 101, "ok")
	if request.Memory != "" || slices.ContainsFunc(toolNames(request.Tools), func(name string) bool { return strings.HasPrefix(name, "procedure_") }) {
		t.Fatalf("memory off: %q %v", request.Memory, toolNames(request.Tools))
	}
}

package telegram

import (
	"testing"
)

func newOutsideTextHarness(t *testing.T, options harnessOptions) (*conversationHarness, *controlledRuntime) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, options)
	h.provision("alice", "acct-alice-1111")
	if err := h.approvals.RegisterExecutor(&noteBook{notes: make(map[string]string)}); err != nil {
		t.Fatal(err)
	}
	h.start()
	return h, runtime
}

func (h *conversationHarness) externalSessions() int {
	h.t.Helper()
	return h.countRows(`SELECT count(*) FROM conversation_sessions WHERE principal_id = 'alice' AND external_at IS NOT NULL`)
}

// A Notion page and a NAS file can hold text written by others, so reading either makes a later
// action in the conversation wait for Confirm, as a web page does.
func TestNotionAndNASReadsAreOutsideContent(t *testing.T) {
	h, runtime := newOutsideTextHarness(t, harnessOptions{notion: writableNotionSimulator(t), nasRoots: nasRootsFor(t, false)})
	h.installNotionToken("alice", aliceNotionToken)

	for index, read := range []struct{ tool, arguments string }{
		{"notion_get_page", `{"page_id":"page-groceries"}`},
		{"nas_read_file", `{"path":"home/notes.md"}`},
	} {
		updateID := int64(2*index + 1)
		if index > 0 {
			h.push(textUpdate(updateID-1, 101, "/new"))
			h.wantReply(101, updateID-1, NewSessionReply)
		}
		h.push(textUpdate(updateID, 101, "read it"))
		call := receive(t, h.ctx, runtime.calls)
		text, err := writeNote(call.ctx, h, read.tool+"-before", false)
		wantDone(t, text, err)
		if result := executeTool(t, call.ctx, call.request.Tools, read.tool, read.arguments); result.IsError {
			t.Fatalf("%s = %+v", read.tool, result)
		}
		proposed, err := writeNote(call.ctx, h, read.tool+"-after", false)
		wantProposed(t, proposed, err)
		h.finishTurn(call, updateID, "read")
		if count := h.externalSessions(); count != index+1 {
			t.Fatalf("external sessions after %s = %d", read.tool, count)
		}
		h.nextMessage()
		h.nextMessage()
	}
}

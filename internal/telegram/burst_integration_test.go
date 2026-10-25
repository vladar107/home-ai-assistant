package telegram

import (
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// Texts that reach the bot in one batch for one tab become one turn, while a command and a secret
// stay on their own and end the run.
func TestBurstOfTextsInATabIsOneTurn(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{topics: true, notion: notionsim.New(t)})
	h.provision("alice", "acct-alice")
	if _, err := h.store.AddTab(h.ctx, "alice", 71, "Reports", thread.NamedByFixed); err != nil {
		t.Fatal(err)
	}
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)

	h.push(
		tabMessage(10, 71, "first"), tabMessage(11, 71, "second"), tabMessage(12, 71, "third"),
		tabMessage(13, 71, aliceNotionToken), tabMessage(14, 71, "/status"),
	)
	call := receive(t, h.ctx, runtime.calls)
	if call.request.Message != "first\n\nsecond\n\nthird" {
		t.Fatalf("runtime received %q, want the three texts joined", call.request.Message)
	}
	if deleted := h.nextDelete(); deleted.messageID != requestMessageID(13) {
		t.Fatalf("token deletion = %+v", deleted)
	}
	h.wantMessage(101, NotionTokenGuardReply)
	h.wantStatusReply(101, 14)
	h.waitOffset(15)

	call.answer <- agentruntime.RunResult{Response: "all three"}
	h.wantReply(101, 12, "all three")
	h.waitState(12, "delivered")
	if requests := h.count(`SELECT count(*) FROM conversation_requests`); requests != 1 {
		t.Fatalf("persisted %d requests, want 1", requests)
	}
	h.stop()
	select {
	case extra := <-runtime.calls:
		t.Fatalf("a message that stood alone reached the runtime: %+v", extra.request)
	default:
	}
}

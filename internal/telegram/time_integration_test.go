package telegram

import (
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func TestChatTurnsCarryTheCurrentLocalTime(t *testing.T) {
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	runtime := newControlledRuntime()
	clock := newTestClock(t, "2026-09-30T12:05:00Z")
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{location: amsterdam, clock: clock})
	h.provision("alice", "acct-alice")
	h.start()

	h.push(textUpdate(1, 101, "what day is it?"))
	summer := receive(t, h.ctx, runtime.calls)
	if want := "[system] Current local time: Wednesday 30 September 2026, 14:05 CEST (Europe/Amsterdam).\n\nwhat day is it?"; summer.request.Message != want {
		t.Fatalf("turn message = %q, want %q", summer.request.Message, want)
	}
	summer.answer <- agentruntime.RunResult{Response: "Wednesday"}
	h.wantReply(101, 1, "Wednesday")
	h.waitState(1, "delivered")

	// Each turn reads the clock afresh, and the zone's own rules decide the offset.
	clock.Set(t, "2026-11-02T08:30:00Z")
	h.push(textUpdate(2, 101, "and now?"))
	winter := receive(t, h.ctx, runtime.calls)
	if winter.request.SessionID != summer.request.SessionID {
		t.Fatal("the second turn did not continue the session")
	}
	if want := "[system] Current local time: Monday 2 November 2026, 09:30 CET (Europe/Amsterdam).\n\nand now?"; winter.request.Message != want {
		t.Fatalf("turn message = %q, want %q", winter.request.Message, want)
	}
	winter.answer <- agentruntime.RunResult{Response: "Monday"}
	h.wantReply(101, 2, "Monday")
	h.waitState(2, "delivered")
}

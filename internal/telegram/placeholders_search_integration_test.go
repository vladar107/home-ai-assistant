package telegram

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A web search whose query holds a placeholder is denied before the search check, so the user is
// offered no card showing a token and nothing is searched, and the turn's trace labels the search
// with the real value.
func TestWebSearchWithAPlaceholderIsDenied(t *testing.T) {
	t.Parallel()
	profiles := filepath.Join(t.TempDir(), "profiles")
	runtime, _ := newWebFixtureRuntime(t, profiles)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{profilesDir: profiles, web: true, placeholders: true})
	h.provision("alice", "acct-alice")
	h.start()

	h.push(textUpdate(1, 101, "fixture:search offers for "+landlordAddress))
	reply := h.nextMessage()
	if reply.text != "Searching.\n\nsaw: The search was not run: its query holds a placeholder, and hidden values cannot be searched. Search without them." {
		t.Fatalf("reply = %q", reply.text)
	}
	if report := latestReport(t, profiles, "alice"); !strings.Contains(report.Message, "offers for ⟦email-") {
		t.Fatalf("the fixture searched for %q, want the address as a token", report.Message)
	}
	h.waitState(1, "delivered")
	h.expectNoMessage(100 * time.Millisecond)
	if got := h.countRows(`SELECT count(*) FROM approvals`); got != 0 {
		t.Fatalf("the denied search left %d proposals", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		trace, found, err := h.store.TurnTraceByMessage(h.ctx, "alice", 101, reply.messageID)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			if len(trace.Calls) != 1 || trace.Calls[0].Tool != "web_search" || trace.Calls[0].Label != "offers for "+landlordAddress {
				t.Fatalf("trace calls = %+v, want the search labelled with the real value", trace.Calls)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn's trace was not saved")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

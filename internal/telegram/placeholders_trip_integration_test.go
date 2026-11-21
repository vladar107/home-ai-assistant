package telegram

import (
	"regexp"
	"strings"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// An event that moves to a street address no listing showed reaches the model's rewrite of the
// trip update as a token, and the user gets the update with the address.
func TestATripUpdatesNewEventAddressReachesTheRewriteAsAToken(t *testing.T) {
	t.Parallel()
	const address = "Lindenlaan 12, Westfield"
	h := newTripUpdateHarnessWith(t, harnessOptions{placeholders: true})
	h.add(h.timed("museum", "Museum visit", "2026-09-30T14:00", "2026-09-30T15:00", ""))
	h.check(mondayMorning)
	h.write(1, 0, spanishSample)

	h.replace(h.timed("museum", "Museum visit", "2026-09-30T14:00", "2026-09-30T15:00", address))
	done := h.begin("2026-09-28T17:00:00+02:00")
	call := receive(t, h.ctx, h.runtime.calls)
	message := call.request.Message
	token := regexp.MustCompile(`⟦address-[a-z2-7]{6}⟧`).FindString(message)
	if !strings.HasPrefix(message, rewriteOpening) || token == "" || !strings.Contains(message, "Museum visit: now at "+token+"\n---") || strings.Contains(message, "Lindenlaan") {
		t.Fatalf("rewrite run = %q, want the new place as a token", message)
	}
	call.answer <- agentruntime.RunResult{Final: "Conference: cambios en el calendario" + updateBullet + "Museum visit: ahora en " + token}
	h.wantUpdate("Conference: cambios en el calendario" + updateBullet + "Museum visit: ahora en Lindenlaan 12")
	receive(t, h.ctx, done)
}

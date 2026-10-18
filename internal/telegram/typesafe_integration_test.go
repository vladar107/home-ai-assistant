package telegram

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/judgment"
)

func TestTypeSafeSettingsToggleThroughTheSettingsFlow(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	optIn := judgment.NewOptIn(h.switches)
	h.service.SetTypeSafe(h.switches.Toggle(judgment.OptInSwitch))
	h.provision("alice", "acct-alice-1111")
	h.start()

	enabled := func(principal string) bool {
		t.Helper()
		on, err := optIn.JudgmentsEnabled(context.Background(), principal)
		if err != nil {
			t.Fatal(err)
		}
		return on
	}
	if enabled("alice") {
		t.Fatal("safety checks are on by default")
	}
	overview := h.requestSettings(1, 101)
	if !strings.Contains(overview.text, "Safety &amp; privacy: safety checks off") || !slices.Equal(buttonData(overview.keyboard), []string{"nav:model", "nav:safety"}) {
		t.Fatalf("settings overview = %+v", overview)
	}
	settings := h.openSettingsPage(2, 101, settingsMessageID(1), "safety")
	if !strings.Contains(settings.text, "Safety checks (TypeSafe): off.") || !strings.Contains(settings.text, "never sent") ||
		!slices.Contains(buttonData(settings.keyboard), "typesafe:on") {
		t.Fatalf("default settings = %+v", settings)
	}

	on := h.press(3, 101, settingsMessageID(1), "typesafe:on")
	if !enabled("alice") || enabled("bob") || !strings.Contains(on.text, "Safety checks (TypeSafe): on.") || !slices.Contains(buttonData(on.keyboard), "typesafe:off") || !slices.Contains(buttonData(on.keyboard), "nav:root") {
		t.Fatalf("after turning on: %+v", on)
	}
	h.push(textUpdate(4, 101, "/privacy"))
	if privacy := h.nextMessage(); !strings.Contains(privacy.text, TypeSafePrivacyLine) {
		t.Fatalf("privacy reply = %q", privacy.text)
	}

	off := h.press(5, 101, settingsMessageID(1), "typesafe:off")
	if enabled("alice") || !strings.Contains(off.text, "Safety checks (TypeSafe): off.") {
		t.Fatalf("after turning off: %+v", off)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "integration.switch.changed principal_id=alice integration=typesafe enabled=true") ||
		!strings.Contains(logs, "integration.switch.changed principal_id=alice integration=typesafe enabled=false") {
		t.Fatalf("logs lack the opt-in events: %s", logs)
	}
}

func TestTypeSafeSectionAbsentWithoutTypeSafe(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	h.provision("alice", "acct-alice-1111")
	h.start()
	settings := h.requestSettings(1, 101)
	if strings.Contains(settings.text, "TypeSafe") || slices.Contains(buttonData(settings.keyboard), "typesafe:on") {
		t.Fatalf("settings show safety checks without TypeSafe configured: %+v", settings)
	}
}

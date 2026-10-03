package telegram

import (
	"slices"
	"strings"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func TestBusesHiddenUnlessEnabled(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()

	settings := h.requestSettings(1, 101)
	if strings.Contains(settings.text, "Buses") || slices.ContainsFunc(buttonData(settings.keyboard), func(data string) bool {
		return strings.HasPrefix(data, busSectionPrefix+":")
	}) {
		t.Fatalf("settings showed buses while disabled: %+v", settings)
	}
	h.push(textUpdate(2, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if names := toolNames(call.request.Tools); slices.Contains(names, "bus_trip") {
		t.Fatalf("tools while disabled = %v", names)
	}
	call.answer <- agentruntime.RunResult{Response: "hi"}
	h.wantReply(101, 2, "hi")
}

func TestBusesSwitchControlsToolsPerUser(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{bus: nsGateway(t), busContact: "ops@example.org"})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	offered := func(update int64, chat int64) []string {
		t.Helper()
		h.push(textUpdate(update, chat, "hello"))
		call := receive(t, h.ctx, runtime.calls)
		names := toolNames(call.request.Tools)
		call.answer <- agentruntime.RunResult{Response: "ok"}
		h.wantReply(chat, update, "ok")
		h.waitState(update, "delivered")
		return names
	}
	wantBuses := func(names []string, want bool) {
		t.Helper()
		for _, name := range []string{"bus_trip", "bus_stop_status"} {
			if slices.Contains(names, name) != want {
				t.Fatalf("tools = %v, %s offered = %v, want %v", names, name, !want, want)
			}
		}
	}

	wantBuses(offered(1, 101), true)
	h.push(textUpdate(2, 101, "/privacy"))
	h.wantReply(101, 2, ConversationPrivacyReply+" "+AttachmentPrivacyLine+" "+BusPrivacyLine+" "+BusPlanningPrivacyLine)

	overview := h.requestSettings(3, 101)
	if !strings.Contains(overview.text, "Everyday tools: Buses on") || !slices.Contains(buttonData(overview.keyboard), "nav:tools") {
		t.Fatalf("settings overview = %+v", overview)
	}
	settings := h.openSettingsPage(4, 101, settingsMessageID(3), "tools")
	if !strings.Contains(settings.text, BusOnLine) || !strings.Contains(settings.text, BusPlanningLine) ||
		!slices.Contains(buttonData(settings.keyboard), "bus:off") || slices.Contains(buttonData(settings.keyboard), "bus:on") {
		t.Fatalf("settings by default = %+v", settings)
	}

	edit := h.press(5, 101, settingsMessageID(3), "bus:off")
	if !strings.Contains(edit.text, BusOffLine) || !slices.Contains(buttonData(edit.keyboard), "bus:on") || !slices.Contains(buttonData(edit.keyboard), "nav:root") || slices.Contains(buttonData(edit.keyboard), "bus:off") {
		t.Fatalf("settings after turning buses off = %+v", edit)
	}
	wantBuses(offered(6, 101), false)
	h.push(textUpdate(7, 101, "/privacy"))
	h.wantReply(101, 7, ConversationPrivacyReply+" "+AttachmentPrivacyLine)

	wantBuses(offered(8, 202), true)

	h.requestSettings(9, 101)
	if edit := h.press(10, 101, settingsMessageID(9), "bus:on"); !strings.Contains(edit.text, BusOnLine) || !slices.Contains(buttonData(edit.keyboard), "bus:off") {
		t.Fatalf("settings after turning buses back on = %+v", edit)
	}
	wantBuses(offered(11, 101), true)
}

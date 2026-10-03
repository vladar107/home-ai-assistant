package telegram

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func nsGateway(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	return server.URL
}

func TestTrainsHiddenWithoutOperatorKey(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()

	settings := h.requestSettings(1, 101)
	if strings.Contains(settings.text, "Trains (NS)") || slices.ContainsFunc(buttonData(settings.keyboard), func(data string) bool {
		return strings.HasPrefix(data, nsSectionPrefix+":")
	}) {
		t.Fatalf("settings showed trains without a key: %+v", settings)
	}
	h.push(textUpdate(2, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if names := toolNames(call.request.Tools); slices.Contains(names, "ns_trips") {
		t.Fatalf("tools without a key = %v", names)
	}
	call.answer <- agentruntime.RunResult{Response: "hi"}
	h.wantReply(101, 2, "hi")
}

func TestTrainsSwitchControlsToolsPerUser(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{ns: nsGateway(t)})
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
	wantTrains := func(names []string, want bool) {
		t.Helper()
		for _, name := range []string{"ns_trips", "ns_departures", "ns_disruptions"} {
			if slices.Contains(names, name) != want {
				t.Fatalf("tools = %v, %s offered = %v, want %v", names, name, !want, want)
			}
		}
	}

	wantTrains(offered(1, 101), true)
	h.push(textUpdate(2, 101, "/privacy"))
	h.wantReply(101, 2, ConversationPrivacyReply+" "+AttachmentPrivacyLine+" "+NSPrivacyLine)

	overview := h.requestSettings(3, 101)
	if !strings.Contains(overview.text, "Everyday tools: Trains on") || !slices.Contains(buttonData(overview.keyboard), "nav:tools") {
		t.Fatalf("settings overview = %+v", overview)
	}
	settings := h.openSettingsPage(4, 101, settingsMessageID(3), "tools")
	if !strings.Contains(settings.text, NSOnLine) || !slices.Contains(buttonData(settings.keyboard), "ns:off") || slices.Contains(buttonData(settings.keyboard), "ns:on") {
		t.Fatalf("settings by default = %+v", settings)
	}

	edit := h.press(5, 101, settingsMessageID(3), "ns:off")
	if !strings.Contains(edit.text, NSOffLine) || !slices.Contains(buttonData(edit.keyboard), "ns:on") || !slices.Contains(buttonData(edit.keyboard), "nav:root") || slices.Contains(buttonData(edit.keyboard), "ns:off") {
		t.Fatalf("settings after turning trains off = %+v", edit)
	}
	wantTrains(offered(6, 101), false)
	h.push(textUpdate(7, 101, "/privacy"))
	h.wantReply(101, 7, ConversationPrivacyReply+" "+AttachmentPrivacyLine)

	wantTrains(offered(8, 202), true)
	h.push(textUpdate(9, 202, "/privacy"))
	h.wantReply(202, 9, ConversationPrivacyReply+" "+AttachmentPrivacyLine+" "+NSPrivacyLine)

	h.requestSettings(10, 101)
	if edit := h.press(11, 101, settingsMessageID(10), "ns:on"); !strings.Contains(edit.text, NSOnLine) || !slices.Contains(buttonData(edit.keyboard), "ns:off") {
		t.Fatalf("settings after turning trains back on = %+v", edit)
	}
	wantTrains(offered(12, 101), true)
}

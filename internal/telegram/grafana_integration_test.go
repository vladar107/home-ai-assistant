package telegram

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func grafanaGateway(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	return server.URL
}

func hasGrafanaButton(data []string) bool {
	return slices.ContainsFunc(data, func(d string) bool { return strings.HasPrefix(d, grafanaSectionPrefix+":") })
}

func TestMonitoringHiddenWithoutOperatorToken(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()

	settings := h.requestSettings(1, 101)
	if strings.Contains(settings.text, "Monitoring") || hasGrafanaButton(buttonData(settings.keyboard)) || slices.Contains(buttonData(settings.keyboard), "nav:monitoring") {
		t.Fatalf("settings showed monitoring without a token: %+v", settings)
	}
}

func TestMonitoringSwitchControlsToolsAndPrivacyForTheAllowedUser(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{grafana: grafanaGateway(t)})
	h.provision("alice", "account-alice")
	h.start()

	offered := func(update int64) []string {
		t.Helper()
		h.push(textUpdate(update, 101, "hello"))
		call := receive(t, h.ctx, runtime.calls)
		names := toolNames(call.request.Tools)
		call.answer <- agentruntime.RunResult{Response: "ok"}
		h.wantReply(101, update, "ok")
		h.waitState(update, "delivered")
		return names
	}
	wantMonitoring := func(names []string, want bool) {
		t.Helper()
		for _, name := range []string{"grafana_dashboards", "grafana_metrics", "grafana_logs"} {
			if slices.Contains(names, name) != want {
				t.Fatalf("tools = %v, %s offered = %v, want %v", names, name, !want, want)
			}
		}
	}

	wantMonitoring(offered(1), true)
	h.push(textUpdate(2, 101, "/privacy"))
	h.wantReply(101, 2, privacyLines(AttachmentPrivacyLine, SchedulePrivacyLine, VoicePrivacyLine, GrafanaPrivacyLine, MemoryPrivacyLine))

	overview := h.requestSettings(3, 101)
	if !strings.Contains(overview.text, "Monitoring: on") || !slices.Contains(buttonData(overview.keyboard), "nav:monitoring") {
		t.Fatalf("settings overview = %+v", overview)
	}
	settings := h.openSettingsPage(4, 101, settingsMessageID(3), "monitoring")
	if !strings.Contains(settings.text, GrafanaOnLine) || !slices.Contains(buttonData(settings.keyboard), "grafana:off") || slices.Contains(buttonData(settings.keyboard), "grafana:on") {
		t.Fatalf("settings by default = %+v", settings)
	}

	edit := h.press(5, 101, settingsMessageID(3), "grafana:off")
	if !strings.Contains(edit.text, GrafanaOffLine) || !slices.Contains(buttonData(edit.keyboard), "grafana:on") || slices.Contains(buttonData(edit.keyboard), "grafana:off") {
		t.Fatalf("settings after turning monitoring off = %+v", edit)
	}
	wantMonitoring(offered(6), false)
	h.push(textUpdate(7, 101, "/privacy"))
	h.wantReply(101, 7, privacyLines(AttachmentPrivacyLine, SchedulePrivacyLine, VoicePrivacyLine, MemoryPrivacyLine))

	h.requestSettings(8, 101)
	if edit := h.press(9, 101, settingsMessageID(8), "grafana:on"); !strings.Contains(edit.text, GrafanaOnLine) || !slices.Contains(buttonData(edit.keyboard), "grafana:off") {
		t.Fatalf("settings after turning monitoring back on = %+v", edit)
	}
	wantMonitoring(offered(10), true)
}

func TestMonitoringIsInvisibleToUsersTheOperatorDidNotName(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{grafana: grafanaGateway(t)})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	overview := h.requestSettings(1, 202)
	if strings.Contains(overview.text, "Monitoring") || slices.Contains(buttonData(overview.keyboard), "nav:monitoring") {
		t.Fatalf("settings for bob showed monitoring: %+v", overview)
	}

	h.push(textUpdate(2, 202, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if names := toolNames(call.request.Tools); slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, "grafana_") }) {
		t.Fatalf("tools for bob = %v", names)
	}
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(202, 2, "ok")
	h.waitState(2, "delivered")

	h.push(textUpdate(3, 202, "/privacy"))
	h.wantReply(202, 3, privacyLines(AttachmentPrivacyLine, SchedulePrivacyLine, VoicePrivacyLine, MemoryPrivacyLine))

	forged := h.press(4, 202, settingsMessageID(1), "grafana:off")
	if strings.Contains(forged.text, "Monitoring") || hasGrafanaButton(buttonData(forged.keyboard)) {
		t.Fatalf("forged press for bob = %+v", forged)
	}

	alice := h.requestSettings(5, 101)
	if !strings.Contains(alice.text, "Monitoring: on") {
		t.Fatalf("settings for alice after bob's forged press = %+v", alice)
	}
}

package telegram

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func homeAssistantGateway(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	return server.URL
}

func TestHomeHiddenWithoutOperatorToken(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	h.provision("alice", "account-alice")
	h.start()

	settings := h.requestSettings(1, 101)
	if strings.Contains(settings.text, "Home (Home Assistant)") || slices.ContainsFunc(buttonData(settings.keyboard), func(data string) bool {
		return strings.HasPrefix(data, homeAssistantSectionPrefix+":")
	}) {
		t.Fatalf("settings showed home without a token: %+v", settings)
	}
}

func TestHomeSwitchIsPerUser(t *testing.T) {
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{homeAssistant: homeAssistantGateway(t)})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	overview := h.requestSettings(1, 101)
	if !strings.Contains(overview.text, "Home &amp; files: Home on") || !slices.Contains(buttonData(overview.keyboard), "nav:home") {
		t.Fatalf("settings overview = %+v", overview)
	}
	settings := h.openSettingsPage(2, 101, settingsMessageID(1), "home")
	if !strings.Contains(settings.text, HomeAssistantOnLine) || !slices.Contains(buttonData(settings.keyboard), "ha:off") || slices.Contains(buttonData(settings.keyboard), "ha:on") {
		t.Fatalf("settings by default = %+v", settings)
	}

	edit := h.press(3, 101, settingsMessageID(1), "ha:off")
	if !strings.Contains(edit.text, HomeAssistantOffLine) || buttonData(edit.keyboard)[len(buttonData(edit.keyboard))-1] != "nav:root" || !slices.Contains(buttonData(edit.keyboard), "ha:on") || slices.Contains(buttonData(edit.keyboard), "ha:off") {
		t.Fatalf("settings after turning home off = %+v", edit)
	}

	h.requestSettings(4, 202)
	if other := h.openSettingsPage(5, 202, settingsMessageID(4), "home"); !strings.Contains(other.text, HomeAssistantOnLine) || !slices.Contains(buttonData(other.keyboard), "ha:off") {
		t.Fatalf("settings for another user = %+v", other)
	}

	h.requestSettings(6, 101)
	if edit := h.press(7, 101, settingsMessageID(6), "ha:on"); !strings.Contains(edit.text, HomeAssistantOnLine) || !slices.Contains(buttonData(edit.keyboard), "ha:off") {
		t.Fatalf("settings after turning home back on = %+v", edit)
	}
}

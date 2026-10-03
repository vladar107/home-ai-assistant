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

	settings := h.requestSettings(1, 101)
	if !strings.Contains(settings.text, HomeAssistantOnLine) || !slices.Contains(buttonData(settings.keyboard), "ha:off") || slices.Contains(buttonData(settings.keyboard), "ha:on") {
		t.Fatalf("settings by default = %+v", settings)
	}

	edit := h.press(2, 101, settingsMessageID(1), "ha:off")
	if !strings.Contains(edit.text, HomeAssistantOffLine) || !slices.Contains(buttonData(edit.keyboard), "ha:on") || slices.Contains(buttonData(edit.keyboard), "ha:off") {
		t.Fatalf("settings after turning home off = %+v", edit)
	}

	if other := h.requestSettings(3, 202); !strings.Contains(other.text, HomeAssistantOnLine) || !slices.Contains(buttonData(other.keyboard), "ha:off") {
		t.Fatalf("settings for another user = %+v", other)
	}

	h.requestSettings(4, 101)
	if edit := h.press(5, 101, settingsMessageID(4), "ha:on"); !strings.Contains(edit.text, HomeAssistantOnLine) || !slices.Contains(buttonData(edit.keyboard), "ha:off") {
		t.Fatalf("settings after turning home back on = %+v", edit)
	}
}

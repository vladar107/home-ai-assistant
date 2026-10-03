package telegram

import (
	"context"
	"log/slog"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/connectors/homeassistant"
)

const (
	homeAssistantSectionPrefix = "ha"
	homeAssistantOnAction      = "on"
	homeAssistantOffAction     = "off"

	HomeAssistantOnLine        = "Home (Home Assistant): on (the default). I can read and control the devices the household exposed to Assist, and propose confirmed actions for devices labelled for confirmation."
	HomeAssistantUncheckedLine = "Home actions are paused: I could not reach Home Assistant to check which devices are exposed."
	HomeAssistantConflictLine  = "Home actions are paused while these devices that need your confirmation are exposed to Assist; unexpose them in Home Assistant: "
	HomeAssistantPrivacyLine   = "Home is on: the names, areas, and states of the devices the household exposed in Home Assistant reach your model subscription when you ask about the home. It is on by default; you can turn it off in /settings."
	HomeAssistantOffLine       = "Home (Home Assistant): off. Turn it back on to let me read and control the devices the household exposed to Assist, and propose confirmed actions for devices labelled for confirmation."
)

// HomeAssistantController is implemented by the Home Assistant connector; nil disables the home settings section.
type HomeAssistantController interface {
	Status(context.Context, string) (homeassistant.Status, error)
	SetEnabled(context.Context, string, bool) (homeassistant.Status, error)
	Exposure(context.Context) homeassistant.Exposure
}

type homeAssistantSection struct {
	homeAssistant HomeAssistantController
	logger        *slog.Logger
	telemetry     Telemetry
}

func (h homeAssistantSection) prefix() string { return homeAssistantSectionPrefix }

func (h homeAssistantSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := h.homeAssistant.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if status.Enabled {
		lines := []string{HomeAssistantOnLine}
		exposure := h.homeAssistant.Exposure(ctx)
		switch {
		case !exposure.Checked:
			lines = append(lines, HomeAssistantUncheckedLine)
		case len(exposure.Conflicts) > 0:
			lines = append(lines, HomeAssistantConflictLine+strings.Join(exposure.Conflicts, "; ")+".")
		}
		return settingsView{
			lines:   lines,
			buttons: []InlineButton{{Text: "Turn off home", CallbackData: homeAssistantSectionPrefix + ":" + homeAssistantOffAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{HomeAssistantOffLine},
		buttons: []InlineButton{{Text: "Turn on home", CallbackData: homeAssistantSectionPrefix + ":" + homeAssistantOnAction}},
	}, nil
}

func (h homeAssistantSection) act(ctx context.Context, principalID string, _ int64, action string) (string, error) {
	switch action {
	case homeAssistantOnAction, homeAssistantOffAction:
		enabled := action == homeAssistantOnAction
		if _, err := h.homeAssistant.SetEnabled(ctx, principalID, enabled); err != nil {
			return "", err
		}
		h.telemetry.ContextLogger(ctx, h.logger).InfoContext(ctx, "homeassistant.home.changed", "principal_id", principalID, "enabled", enabled)
		return "", nil
	default:
		return "", errUnknownSettingsAction
	}
}

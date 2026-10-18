package telegram

import (
	"context"
	"log/slog"
)

const (
	webSectionPrefix = "web"
	webOnAction      = "on"
	webOffAction     = "off"

	WebOnLine  = "Web access: on (the default). I can search the web through your own model subscription, read links, and check the weather."
	WebOffLine = "Web access: off. Turn it back on to let me search the web through your own model subscription, read links, and check the weather."
)

type webSection struct {
	web       SwitchController
	logger    *slog.Logger
	telemetry Telemetry
}

func (w webSection) prefix() string { return webSectionPrefix }

func (w webSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	enabled, err := w.web.Enabled(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	return settingsSummary{name: "Web", state: onOff(enabled)}, nil
}

func (w webSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := w.web.Enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		return settingsView{
			lines:   []string{WebOnLine},
			buttons: []InlineButton{{Text: "Turn off web access", CallbackData: webSectionPrefix + ":" + webOffAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{WebOffLine},
		buttons: []InlineButton{{Text: "Turn on web access", CallbackData: webSectionPrefix + ":" + webOnAction}},
	}, nil
}

func (w webSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	switch action {
	case webOnAction, webOffAction:
		enabled := action == webOnAction
		if err := w.web.SetEnabled(ctx, principalID, enabled); err != nil {
			return settingsOutcome{}, err
		}
		w.telemetry.ContextLogger(ctx, w.logger).InfoContext(ctx, "web.access.changed", "principal_id", principalID, "enabled", enabled)
		return settingsOutcome{}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

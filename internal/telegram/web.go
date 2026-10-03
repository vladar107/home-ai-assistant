package telegram

import (
	"context"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
)

const (
	webSectionPrefix = "web"
	webOnAction      = "on"
	webOffAction     = "off"

	WebOnLine  = "Web access: on (the default). I can search the web through your own model subscription, read links, and check the weather."
	WebOffLine = "Web access: off. Turn it back on to let me search the web through your own model subscription, read links, and check the weather."
)

// WebController is implemented by the web connector; nil disables the web access settings section.
type WebController interface {
	Status(context.Context, string) (web.Status, error)
	SetEnabled(context.Context, string, bool) (web.Status, error)
}

type webSection struct {
	web       WebController
	logger    *slog.Logger
	telemetry Telemetry
}

func (w webSection) prefix() string { return webSectionPrefix }

func (w webSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	status, err := w.web.Status(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	return settingsSummary{name: "Web", state: onOff(status.Enabled)}, nil
}

func (w webSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := w.web.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if status.Enabled {
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
		if _, err := w.web.SetEnabled(ctx, principalID, enabled); err != nil {
			return settingsOutcome{}, err
		}
		w.telemetry.ContextLogger(ctx, w.logger).InfoContext(ctx, "web.access.changed", "principal_id", principalID, "enabled", enabled)
		return settingsOutcome{}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

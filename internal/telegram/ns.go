package telegram

import (
	"context"
	"log/slog"
)

const (
	nsSectionPrefix = "ns"
	nsOnAction      = "on"
	nsOffAction     = "off"

	NSOnLine  = "Trains (NS): on (the default). I can plan trips, list departures, and report disruptions from live NS data."
	NSOffLine = "Trains (NS): off. Turn it back on to let me plan trips, list departures, and report disruptions from live NS data."
)

type nsSection struct {
	ns        SwitchController
	logger    *slog.Logger
	telemetry Telemetry
}

func (n nsSection) prefix() string { return nsSectionPrefix }

func (n nsSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	enabled, err := n.ns.Enabled(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	return settingsSummary{name: "Trains", state: onOff(enabled)}, nil
}

func (n nsSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := n.ns.Enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		return settingsView{
			lines:   []string{NSOnLine},
			buttons: []InlineButton{{Text: "Turn off trains", CallbackData: nsSectionPrefix + ":" + nsOffAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{NSOffLine},
		buttons: []InlineButton{{Text: "Turn on trains", CallbackData: nsSectionPrefix + ":" + nsOnAction}},
	}, nil
}

func (n nsSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	switch action {
	case nsOnAction, nsOffAction:
		enabled := action == nsOnAction
		if err := n.ns.SetEnabled(ctx, principalID, enabled); err != nil {
			return settingsOutcome{}, err
		}
		n.telemetry.ContextLogger(ctx, n.logger).InfoContext(ctx, "ns.trains.changed", "principal_id", principalID, "enabled", enabled)
		return settingsOutcome{}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

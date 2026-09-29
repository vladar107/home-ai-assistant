package telegram

import (
	"context"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/connectors/ns"
)

const (
	nsSectionPrefix = "ns"
	nsOnAction      = "on"
	nsOffAction     = "off"

	NSOnLine  = "Trains (NS): on (the default). I can plan trips, list departures, and report disruptions from live NS data."
	NSOffLine = "Trains (NS): off. Turn it back on to let me plan trips, list departures, and report disruptions from live NS data."
)

// NSController is implemented by the NS connector; nil disables the trains settings section.
type NSController interface {
	Status(context.Context, string) (ns.Status, error)
	SetEnabled(context.Context, string, bool) (ns.Status, error)
}

type nsSection struct {
	ns        NSController
	logger    *slog.Logger
	telemetry Telemetry
}

func (n nsSection) prefix() string { return nsSectionPrefix }

func (n nsSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := n.ns.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if status.Enabled {
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

func (n nsSection) act(ctx context.Context, principalID string, _ int64, action string) (string, error) {
	switch action {
	case nsOnAction, nsOffAction:
		enabled := action == nsOnAction
		if _, err := n.ns.SetEnabled(ctx, principalID, enabled); err != nil {
			return "", err
		}
		n.telemetry.ContextLogger(ctx, n.logger).InfoContext(ctx, "ns.trains.changed", "principal_id", principalID, "enabled", enabled)
		return "", nil
	default:
		return "", errUnknownSettingsAction
	}
}

package telegram

import (
	"context"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/connectors/bus"
)

const (
	busSectionPrefix = "bus"
	busOnAction      = "on"
	busOffAction     = "off"

	BusOnLine  = "Buses: on (the default). I can show live departures, direct buses, and disruption notices from OVapi."
	BusOffLine = "Buses: off. Turn it back on to let me show live departures, direct buses, and disruption notices from OVapi."
	// BusPlanningLine credits the planner, as Transitous's terms ask.
	BusPlanningLine = "Trips at a later time or with changes are planned by Transitous (transitous.org/sources), using open transit data and © OpenStreetMap contributors."
)

// BusController is implemented by the bus connector; nil disables the buses settings section.
type BusController interface {
	Status(context.Context, string) (bus.Status, error)
	SetEnabled(context.Context, string, bool) (bus.Status, error)
}

type busSection struct {
	bus       BusController
	logger    *slog.Logger
	telemetry Telemetry
}

func (b busSection) prefix() string { return busSectionPrefix }

func (b busSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := b.bus.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if status.Enabled {
		return settingsView{
			lines:   []string{BusOnLine, BusPlanningLine},
			buttons: []InlineButton{{Text: "Turn off buses", CallbackData: busSectionPrefix + ":" + busOffAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{BusOffLine},
		buttons: []InlineButton{{Text: "Turn on buses", CallbackData: busSectionPrefix + ":" + busOnAction}},
	}, nil
}

func (b busSection) act(ctx context.Context, principalID string, _ int64, action string) (string, error) {
	switch action {
	case busOnAction, busOffAction:
		enabled := action == busOnAction
		if _, err := b.bus.SetEnabled(ctx, principalID, enabled); err != nil {
			return "", err
		}
		b.telemetry.ContextLogger(ctx, b.logger).InfoContext(ctx, "bus.changed", "principal_id", principalID, "enabled", enabled)
		return "", nil
	default:
		return "", errUnknownSettingsAction
	}
}

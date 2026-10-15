package telegram

import (
	"context"
	"errors"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

const (
	headsUpSectionPrefix = "headsup"
	headsUpOnAction      = "on"
	headsUpOffAction     = "off"

	HeadsUpOnLine  = "Heads-up: on. A message at 07:30 and 18:30 when something needs your attention; quiet otherwise. Uses your model subscription."
	HeadsUpOffLine = "Heads-up: off. Turn it on for a message at 07:30 and 18:30 when something needs your attention; quiet otherwise. Needs a connected model subscription, which it uses."
	// HeadsUpTooManyNote explains why the switch could not create its job.
	HeadsUpTooManyNote = "You already have the maximum number of scheduled jobs. Remove one in /jobs, then turn Heads-up on."
)

type headsUpSection struct {
	schedules ScheduleController
	logger    *slog.Logger
	telemetry Telemetry
}

func (h headsUpSection) prefix() string { return headsUpSectionPrefix }

// enabled is true while the principal's Heads-up job exists and is not paused.
func (h headsUpSection) enabled(ctx context.Context, principalID string) (bool, error) {
	jobs, err := h.schedules.Jobs(ctx, principalID)
	if err != nil {
		return false, err
	}
	id := schedule.HeadsUpJobID(principalID)
	for _, job := range jobs {
		if job.ID == id {
			return job.Enabled, nil
		}
	}
	return false, nil
}

func (h headsUpSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	enabled, err := h.enabled(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	return settingsSummary{name: "Heads-up", state: onOff(enabled)}, nil
}

func (h headsUpSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := h.enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		return settingsView{
			lines:   []string{HeadsUpOnLine},
			buttons: []InlineButton{{Text: "Turn off Heads-up", CallbackData: headsUpSectionPrefix + ":" + headsUpOffAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{HeadsUpOffLine},
		buttons: []InlineButton{{Text: "Turn on Heads-up", CallbackData: headsUpSectionPrefix + ":" + headsUpOnAction}},
	}, nil
}

func (h headsUpSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	id := schedule.HeadsUpJobID(principalID)
	switch action {
	case headsUpOnAction:
		jobs, err := h.schedules.Jobs(ctx, principalID)
		if err != nil {
			return settingsOutcome{}, err
		}
		for _, job := range jobs {
			if job.ID != id {
				continue
			}
			if !job.Enabled {
				if _, err := h.schedules.SetEnabled(ctx, principalID, id, true); err != nil {
					return settingsOutcome{}, err
				}
				h.telemetry.ContextLogger(ctx, h.logger).InfoContext(ctx, "headsup.changed", "principal_id", principalID, "enabled", true)
			}
			return settingsOutcome{}, nil
		}
		config, err := schedule.ModelTurnConfig(schedule.HeadsUpPrompt)
		if err != nil {
			return settingsOutcome{}, err
		}
		prepared, err := h.schedules.Prepare(principalID, schedule.HeadsUpName, schedule.TypeModelTurn,
			schedule.Timing{Cron: schedule.HeadsUpCron, Zone: h.schedules.DefaultZone()}, config)
		if err != nil {
			return settingsOutcome{}, err
		}
		created, err := h.schedules.Create(ctx, id, prepared)
		if err != nil {
			if errors.Is(err, schedule.ErrTooManyJobs) {
				return settingsOutcome{note: HeadsUpTooManyNote}, nil
			}
			return settingsOutcome{}, err
		}
		if created {
			h.telemetry.ContextLogger(ctx, h.logger).InfoContext(ctx, "headsup.changed", "principal_id", principalID, "enabled", true)
		}
		return settingsOutcome{}, nil
	case headsUpOffAction:
		deleted, err := h.schedules.Delete(ctx, principalID, id)
		if err != nil {
			return settingsOutcome{}, err
		}
		if deleted {
			h.telemetry.ContextLogger(ctx, h.logger).InfoContext(ctx, "headsup.changed", "principal_id", principalID, "enabled", false)
		}
		return settingsOutcome{}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

package telegram

import (
	"context"
	"errors"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

const (
	headsUpSectionPrefix = "headsup"
	headsUpOnAction      = "on"
	headsUpOffAction     = "off"
)

var (
	// HeadsUpTooManyNote explains why the switch could not create its job.
	HeadsUpTooManyNote = locale.T("You already have the maximum number of scheduled jobs. Remove one in /jobs, then turn Heads-up on.", "У тебя уже максимальное число заданий по расписанию. Удали одно в /jobs, а затем включи оповещения.")

	headsUpName      = locale.T("Heads-up", "Оповещения")
	headsUpOffButton = locale.T("Turn off Heads-up", "Выключить оповещения")
	headsUpOnButton  = locale.T("Turn on Heads-up", "Включить оповещения")
	HeadsUpOnLine    = locale.T("Heads-up: on. A message at 07:30 and 18:30 when something needs your attention; quiet otherwise. Uses your model subscription.", "Оповещения: включены. Сообщение в 07:30 и 18:30, когда что-то требует твоего внимания; в остальное время тишина. Использует твою подписку на модель.")
	HeadsUpOffLine   = locale.T("Heads-up: off. Turn it on for a message at 07:30 and 18:30 when something needs your attention; quiet otherwise. Needs a connected model subscription, which it uses.", "Оповещения: выключены. Включи, и в 07:30 и 18:30 будет приходить сообщение, когда что-то требует твоего внимания; в остальное время тишина. Нужна подключённая подписка на модель, которую эта функция использует.")
)

// NewHeadsUpSection is the page that turns the principal's twice-daily heads-up job on and off.
func NewHeadsUpSection(schedules ScheduleController, logger *slog.Logger, telemetry Telemetry) Section {
	return headsUpSection{schedules: schedules, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type headsUpSection struct {
	schedules ScheduleController
	logger    *slog.Logger
	telemetry Telemetry
}

func (h headsUpSection) prefix() string { return headsUpSectionPrefix }

func (h headsUpSection) helpTopic() string { return "heads_up" }

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
	return settingsSummary{name: headsUpName.Format(ctx), state: stateOf(ctx, enabled)}, nil
}

func (h headsUpSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := h.enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		return settingsView{
			lines:   []string{HeadsUpOnLine.Format(ctx)},
			buttons: []InlineButton{{Text: headsUpOffButton.Format(ctx), CallbackData: headsUpSectionPrefix + ":" + headsUpOffAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{HeadsUpOffLine.Format(ctx)},
		buttons: []InlineButton{{Text: headsUpOnButton.Format(ctx), CallbackData: headsUpSectionPrefix + ":" + headsUpOnAction}},
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
		config, err := schedule.ModelTurnConfig(schedule.HeadsUpNotes)
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
				return settingsOutcome{note: HeadsUpTooManyNote.Format(ctx)}, nil
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

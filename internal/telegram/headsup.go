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
	phraseOnAction       = "phraseon"
	phraseOffAction      = "phraseoff"
)

var (
	// HeadsUpTooManyNote explains why the switch could not create its job.
	HeadsUpTooManyNote = locale.T("You already have the maximum number of scheduled jobs. Remove one in /jobs, then turn Heads-up on.", "У тебя уже максимальное число заданий по расписанию. Удали одно в /jobs, а затем включи оповещения.")

	headsUpName      = locale.T("Heads-up", "Оповещения")
	headsUpOffButton = locale.T("Turn off Heads-up", "Выключить оповещения")
	headsUpOnButton  = locale.T("Turn on Heads-up", "Включить оповещения")
	HeadsUpOnLine    = locale.T("Heads-up: on. A message every morning at 07:30 when something needs your attention; quiet otherwise. Uses your model subscription.", "Оповещения: включены. Сообщение каждое утро в 07:30, когда что-то требует твоего внимания; в остальное время тишина. Использует твою подписку на модель.")
	HeadsUpOffLine   = locale.T("Heads-up: off. Turn it on for a message every morning at 07:30 when something needs your attention; quiet otherwise. Needs a connected model subscription, which it uses.", "Оповещения: выключены. Включи, и каждое утро в 07:30 будет приходить сообщение, когда что-то требует твоего внимания; в остальное время тишина. Нужна подключённая подписка на модель, которую эта функция использует.")

	phraseOnButton    = locale.T("Turn on Dutch phrase", "Включить фразу на нидерландском")
	phraseOffButton   = locale.T("Turn off Dutch phrase", "Выключить фразу на нидерландском")
	PhraseOnLine      = locale.T("Dutch phrase: on. The first Heads-up each day ends with an everyday Dutch phrase, its translation hidden behind a spoiler; on a quiet morning the phrase comes alone. Uses your model subscription.", "Фраза на нидерландском: включена. Первое оповещение каждого дня заканчивается повседневной фразой на нидерландском, перевод скрыт под спойлером; в тихое утро фраза приходит одна. Использует твою подписку на модель.")
	PhraseOffLine     = locale.T("Dutch phrase: off. Turn it on to end the first Heads-up each day with an everyday Dutch phrase, its translation hidden behind a spoiler.", "Фраза на нидерландском: выключена. Включи, и первое оповещение каждого дня будет заканчиваться повседневной фразой на нидерландском с переводом под спойлером.")
	PhrasePrivacyLine = locale.T("The Dutch phrases of the day you were sent are kept, also after you turn the phrase off, so none is sent twice; /export includes them.", "Присланные фразы дня на нидерландском хранятся, и после выключения фразы тоже, чтобы ни одна не повторилась; /export их включает.")
)

// PhraseHistory reports whether a principal was ever sent a Dutch phrase, which is kept after the
// phrase is turned off.
type PhraseHistory interface {
	HasDutchPhrases(ctx context.Context, principalID string) (bool, error)
}

// NewHeadsUpSection is the page that turns the principal's daily heads-up job on and off, and,
// while it is on, the Dutch phrase that ends it.
func NewHeadsUpSection(schedules ScheduleController, phrase SwitchController, history PhraseHistory, logger *slog.Logger, telemetry Telemetry) Section {
	return headsUpSection{schedules: schedules, phrase: phrase, history: history, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type headsUpSection struct {
	schedules ScheduleController
	phrase    SwitchController
	history   PhraseHistory
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
		phrase, err := h.phrase.Enabled(ctx, principalID)
		if err != nil {
			return settingsView{}, err
		}
		phraseLine, phraseButton := PhraseOffLine, InlineButton{Text: phraseOnButton.Format(ctx), CallbackData: headsUpSectionPrefix + ":" + phraseOnAction}
		if phrase {
			phraseLine, phraseButton = PhraseOnLine, InlineButton{Text: phraseOffButton.Format(ctx), CallbackData: headsUpSectionPrefix + ":" + phraseOffAction}
		}
		return settingsView{
			lines:   []string{HeadsUpOnLine.Format(ctx), phraseLine.Format(ctx)},
			buttons: []InlineButton{{Text: headsUpOffButton.Format(ctx), CallbackData: headsUpSectionPrefix + ":" + headsUpOffAction}, phraseButton},
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
	case phraseOnAction, phraseOffAction:
		on := action == phraseOnAction
		if on {
			// A button left on an older page cannot turn the phrase on once Heads-up is off.
			enabled, err := h.enabled(ctx, principalID)
			if err != nil || !enabled {
				return settingsOutcome{}, err
			}
		}
		if err := h.phrase.SetEnabled(ctx, principalID, on); err != nil {
			return settingsOutcome{}, err
		}
		h.telemetry.ContextLogger(ctx, h.logger).InfoContext(ctx, "headsup.phrase.changed", "principal_id", principalID, "enabled", on)
		return settingsOutcome{}, nil
	case headsUpOffAction:
		// The phrase switch is kept; it has no effect without the job and shows again once
		// Heads-up is back on.
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

// privacy adds the phrase's line while the phrase is on or the principal holds phrases sent before.
func (h headsUpSection) privacy(ctx context.Context, principalID string) ([]string, error) {
	on, err := h.phrase.Enabled(ctx, principalID)
	if err != nil {
		return nil, err
	}
	if !on {
		held, err := h.history.HasDutchPhrases(ctx, principalID)
		if err != nil || !held {
			return nil, err
		}
	}
	return []string{PhrasePrivacyLine.Format(ctx)}, nil
}

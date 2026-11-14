package telegram

import (
	"context"
	"errors"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/intake"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

const (
	inboxSectionPrefix = "inbox"
	inboxOnAction      = "on"
	inboxOffAction     = "off"
	inboxCheckAction   = "check"
)

var (
	InboxTooManyNote = locale.T("You already have the maximum number of scheduled jobs. Remove one in /jobs, then turn Inbox intake on.", "У вас уже максимальное число заданий по расписанию. Удалите одно в /jobs, а затем включите разбор почты.")
	InboxBusyNote    = locale.T("A check is already running; anything that needs you arrives in the Inbox tab, or here while tabs are off.", "Проверка уже идёт; всё, что требует вашего внимания, придёт во вкладку «Входящие» или сюда, пока вкладки выключены.")

	inboxName      = locale.T("Inbox intake", "Разбор почты")
	inboxCheckBtn  = locale.T("Check now", "Проверить сейчас")
	inboxOffButton = locale.T("Turn off Inbox intake", "Выключить разбор почты")
	inboxOnButton  = locale.T("Turn on Inbox intake", "Включить разбор почты")
	InboxOnLine    = locale.T("Inbox intake: on. Every three hours between 08:00 and 22:00 I check your new mail for letters, bills, and notices that need something from you, and send what they say with the follow-ups I propose, such as a reminder before a deadline, on one card to confirm; mail that needs nothing sends nothing. Press Check now to look at once. Uses your model subscription.", "Разбор почты: включён. Каждые три часа с 08:00 до 22:00 я проверяю вашу новую почту на письма, счета и уведомления, от которых что-то требуется, и присылаю их содержание вместе с предлагаемыми действиями, например напоминанием перед сроком, одной карточкой для подтверждения; если от письма ничего не требуется, ничего не приходит. Нажмите «Проверить сейчас», чтобы посмотреть сразу. Использует вашу подписку на модель.")
	InboxOffLine   = locale.T("Inbox intake: off. Turn it on and I check your mail from the last day at once and then every three hours between 08:00 and 22:00 for letters, bills, and notices that need something from you, and propose follow-ups on one card; nothing is set without your press. Needs a connected model subscription, which it uses.", "Разбор почты: выключен. Включите, и я сразу проверю вашу почту за последние сутки, а затем буду проверять её каждые три часа с 08:00 до 22:00 на письма, счета и уведомления, от которых что-то требуется, и предлагать действия одной карточкой; без вашего нажатия ничего не назначается. Нужна подключённая подписка на модель, которую эта функция использует.")
	// InboxNeedsMailLine explains why the switch is not offered.
	InboxNeedsMailLine = locale.T("Inbox intake: off. It reads your mail, so turn on Mail for a connected Google account in Accounts › Google first.", "Разбор почты: выключен. Он читает вашу почту, поэтому сначала включите «Почту» для подключённого аккаунта Google в разделе «Аккаунты › Google».")
	InboxCheckingNote  = locale.T("Checking your new mail now; anything that needs you arrives in the Inbox tab, or here while tabs are off.", "Проверяю новую почту; всё, что требует вашего внимания, придёт во вкладку «Входящие» или сюда, пока вкладки выключены.")
)

// InboxController is the intake service as the Inbox intake switch sees it.
type InboxController interface {
	// ResetInbox forgets where the check read to and the follow-ups waiting for a card.
	ResetInbox(ctx context.Context, principalID string) error
}

// NewInboxSection is the page that turns the principal's inbox check of Google mail on and off.
func NewInboxSection(schedules ScheduleController, google GoogleController, inbox InboxController, logger *slog.Logger, telemetry Telemetry) Section {
	return inboxSection{schedules: schedules, google: google, inbox: inbox, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type inboxSection struct {
	schedules ScheduleController
	google    GoogleController
	inbox     InboxController
	logger    *slog.Logger
	telemetry Telemetry
}

func (i inboxSection) prefix() string { return inboxSectionPrefix }

func (i inboxSection) helpTopic() string { return "inbox" }

func (i inboxSection) enabled(ctx context.Context, principalID string) (bool, error) {
	jobs, err := i.schedules.Jobs(ctx, principalID)
	if err != nil {
		return false, err
	}
	id := schedule.InboxJobID(principalID)
	for _, job := range jobs {
		if job.ID == id {
			return job.Enabled, nil
		}
	}
	return false, nil
}

// readable is true while a connected Google account has Mail granted and turned on.
func (i inboxSection) readable(ctx context.Context, principalID string) (bool, error) {
	statuses, err := i.google.Status(ctx, principalID)
	if err != nil {
		return false, err
	}
	for _, status := range statuses {
		if !status.NeedsReconnect && status.MailGranted && status.MailEnabled {
			return true, nil
		}
	}
	return false, nil
}

func (i inboxSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	enabled, err := i.enabled(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	return settingsSummary{name: inboxName.Format(ctx), state: stateOf(ctx, enabled)}, nil
}

func (i inboxSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := i.enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		return settingsView{
			lines: []string{InboxOnLine.Format(ctx)},
			buttons: []InlineButton{
				{Text: inboxCheckBtn.Format(ctx), CallbackData: inboxSectionPrefix + ":" + inboxCheckAction},
				{Text: inboxOffButton.Format(ctx), CallbackData: inboxSectionPrefix + ":" + inboxOffAction},
			},
		}, nil
	}
	readable, err := i.readable(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if !readable {
		return settingsView{lines: []string{InboxNeedsMailLine.Format(ctx)}}, nil
	}
	return settingsView{
		lines:   []string{InboxOffLine.Format(ctx)},
		buttons: []InlineButton{{Text: inboxOnButton.Format(ctx), CallbackData: inboxSectionPrefix + ":" + inboxOnAction}},
	}, nil
}

func (i inboxSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	id := schedule.InboxJobID(principalID)
	logger := i.telemetry.ContextLogger(ctx, i.logger)
	switch action {
	case inboxOnAction:
		readable, err := i.readable(ctx, principalID)
		if err != nil {
			return settingsOutcome{}, err
		}
		if !readable {
			return settingsOutcome{note: InboxNeedsMailLine.Format(ctx)}, nil
		}
		jobs, err := i.schedules.Jobs(ctx, principalID)
		if err != nil {
			return settingsOutcome{}, err
		}
		for _, job := range jobs {
			if job.ID != id {
				continue
			}
			if job.Enabled {
				return settingsOutcome{}, nil
			}
			if _, err := i.schedules.SetEnabled(ctx, principalID, id, true); err != nil {
				return settingsOutcome{}, err
			}
			logger.InfoContext(ctx, "inbox.changed", "principal_id", principalID, "enabled", true)
			return i.check(ctx, principalID)
		}
		prepared, err := i.schedules.Prepare(principalID, schedule.InboxName, schedule.InboxScanType,
			schedule.Timing{Cron: schedule.InboxCron, Zone: i.schedules.DefaultZone()}, intake.ScanConfigJSON())
		if err != nil {
			return settingsOutcome{}, err
		}
		created, err := i.schedules.Create(ctx, id, prepared)
		if err != nil {
			if errors.Is(err, schedule.ErrTooManyJobs) {
				return settingsOutcome{note: InboxTooManyNote.Format(ctx)}, nil
			}
			return settingsOutcome{}, err
		}
		if !created {
			return settingsOutcome{}, nil
		}
		logger.InfoContext(ctx, "inbox.changed", "principal_id", principalID, "enabled", true)
		return i.check(ctx, principalID)
	case inboxCheckAction:
		enabled, err := i.enabled(ctx, principalID)
		if err != nil || !enabled {
			return settingsOutcome{}, err
		}
		return i.check(ctx, principalID)
	case inboxOffAction:
		deleted, err := i.schedules.Delete(ctx, principalID, id)
		if err != nil {
			return settingsOutcome{}, err
		}
		// Turning the check on again starts from the last day, with nothing old left to offer.
		if err := i.inbox.ResetInbox(ctx, principalID); err != nil {
			return settingsOutcome{}, err
		}
		if deleted {
			logger.InfoContext(ctx, "inbox.changed", "principal_id", principalID, "enabled", false)
		}
		return settingsOutcome{}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

// check queues one run of the principal's inbox check, named by the press so a retried update
// queues no second one. A check already queued or running is left to finish, so two never share
// the user's subscription.
func (i inboxSection) check(ctx context.Context, principalID string) (settingsOutcome, error) {
	id := schedule.InboxJobID(principalID)
	busy, err := i.schedules.Busy(ctx, principalID, id)
	if err != nil {
		return settingsOutcome{}, err
	}
	if busy {
		return settingsOutcome{note: InboxBusyNote.Format(ctx)}, nil
	}
	queued, err := i.schedules.RunNow(ctx, principalID, id, pressTriggerFrom(ctx))
	if err != nil || !queued {
		return settingsOutcome{}, err
	}
	i.telemetry.ContextLogger(ctx, i.logger).InfoContext(ctx, "inbox.check.queued", "principal_id", principalID)
	return settingsOutcome{note: InboxCheckingNote.Format(ctx)}, nil
}

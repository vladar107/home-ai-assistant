package telegram

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/looseend"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

const (
	looseEndPrefix         = "loose"
	looseEndDoneAction     = "done"
	looseEndTomorrowAction = "tomorrow"
	looseEndDropAction     = "drop"

	looseEndsSectionPrefix = "looseends"
	looseEndsOnAction      = "on"
	looseEndsOffAction     = "off"
	looseEndsCheckAction   = "check"
)

var (
	looseEndsHeading     = locale.T("Loose ends", "Незакрытые дела")
	looseEndDue          = locale.T("%[1]s: %[2]s, by %[3]s", "%[1]s: %[2]s, до %[3]s")
	looseEndDueWith      = locale.T("%[1]s: %[2]s (%[3]s), by %[4]s", "%[1]s: %[2]s (%[3]s), до %[4]s")
	looseEndWroteAgain   = locale.T("%[1]s; you wrote again on %[2]s", "%[1]s; позже в переписке есть твоё письмо от %[2]s")
	looseEndDoneButton   = locale.T("Done", "Сделано")
	looseEndTomorrowBtn  = locale.T("Tomorrow", "Завтра")
	looseEndDropButton   = locale.T("Drop", "Не нужно")
	looseEndDoneOutcome  = locale.T("done", "сделано")
	looseEndDropOutcome  = locale.T("dropped", "снято")
	looseEndAgainOutcome = locale.T("again tomorrow", "напомню завтра")
	looseEndDueToday     = locale.T("due today, so it cannot wait until tomorrow", "срок сегодня, отложить на завтра нельзя")
	LooseEndGoneOutcome  = locale.T("no longer open", "уже не открыто")

	looseEndKinds = map[looseend.Kind]locale.Text{
		looseend.KindCancelWindow: locale.T("Free cancellation", "Бесплатная отмена"),
		looseend.KindReturnWindow: locale.T("Return window", "Срок возврата"),
		looseend.KindRenewal:      locale.T("Renewal", "Продление"),
		looseend.KindTrialEnd:     locale.T("Trial ends", "Конец пробного периода"),
		looseend.KindExpiry:       locale.T("Expires", "Истекает срок"),
		looseend.KindPromise:      locale.T("Promise", "Обещание"),
	}
)

// looseEndIDPattern is the shape of the IDs the loose ends service gives its items; a press with
// any other is not one of a nudge's buttons.
var looseEndIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// LooseEndController is the loose ends service as the nudge buttons and the Loose ends page see it.
type LooseEndController interface {
	Close(ctx context.Context, principalID, id string, status looseend.Status) (looseend.Item, bool, error)
	Snooze(ctx context.Context, principalID, id string) (looseend.Item, bool, error)
	CanSnooze(item looseend.Item) bool
	// Location is the zone due dates are shown in.
	Location() *time.Location
	StartScan(ctx context.Context, principalID string) (looseend.ScanState, error)
	ResetScan(ctx context.Context, principalID string) error
}

// LooseEndNudger sends a principal's due loose ends as one plain-text message, a line and a row of
// buttons per item, in the caller's context: the scheduler puts a run's sends in its feed tab and
// its owner's language.
type LooseEndNudger struct {
	Client    *Client
	Chats     TripChats
	LooseEnds LooseEndController
}

var _ looseend.Nudger = LooseEndNudger{}

func (n LooseEndNudger) Nudge(ctx context.Context, principalID string, items []looseend.DueItem) (string, schedule.Sent, error) {
	chatID, found, err := n.Chats.ResolveActiveChat(ctx, principalID)
	if err != nil {
		return "", schedule.Sent{}, err
	}
	if !found {
		return "", schedule.Sent{}, errors.New("the principal has no active chat")
	}
	text, keyboard := n.render(ctx, items)
	message, err := n.Client.sendPlainIn(ctx, chatID, text, keyboard, true)
	if err != nil {
		return "", schedule.Sent{}, err
	}
	return text, schedule.Sent{MessageID: message.MessageID, ThreadID: message.MessageThreadID}, nil
}

// render writes the heading and a line per item, numbered with its buttons when there are several,
// and the item's buttons: Tomorrow only while the item is not due today. A sent promise's line says
// when the user wrote again in its thread. A pressed button's callback names its line, so the press
// edits that line alone.
func (n LooseEndNudger) render(ctx context.Context, items []looseend.DueItem) (string, InlineKeyboard) {
	lines := []string{looseEndsHeading.Format(ctx)}
	keyboard := make(InlineKeyboard, 0, len(items))
	numbered := len(items) > 1
	location := n.LooseEnds.Location()
	for index, item := range items {
		line, number := looseEndLine(ctx, item.Item, location), ""
		if !item.WroteAgain.IsZero() {
			line = looseEndWroteAgain.Format(ctx, line, locale.Date(locale.From(ctx), item.WroteAgain.In(location), locale.Day))
		}
		if numbered {
			number = strconv.Itoa(index+1) + ". "
			line = number + line
		} else {
			line = "• " + line
		}
		lines = append(lines, line)
		keyboard = append(keyboard, looseEndButtons(ctx, number, len(lines)-1, item.ID, n.LooseEnds.CanSnooze(item.Item)))
	}
	return strings.Join(lines, "\n"), keyboard
}

func looseEndLine(ctx context.Context, item looseend.Item, location *time.Location) string {
	kind := string(item.Kind)
	if label, known := looseEndKinds[item.Kind]; known {
		kind = label.Format(ctx)
	}
	style := locale.DayTime
	if item.DueAllDay {
		style = locale.Day
	}
	due := locale.Date(locale.From(ctx), item.DueAt.In(location), style)
	if item.Counterpart == "" {
		return looseEndDue.Format(ctx, kind, item.Title, due)
	}
	return looseEndDueWith.Format(ctx, kind, item.Title, item.Counterpart, due)
}

func looseEndButtons(ctx context.Context, number string, line int, id string, tomorrow bool) []InlineButton {
	data := func(action string) string {
		return looseEndPrefix + ":" + action + ":" + strconv.Itoa(line) + ":" + id
	}
	row := []InlineButton{{Text: number + looseEndDoneButton.Format(ctx), CallbackData: data(looseEndDoneAction)}}
	if tomorrow {
		row = append(row, InlineButton{Text: number + looseEndTomorrowBtn.Format(ctx), CallbackData: data(looseEndTomorrowAction)})
	}
	return append(row, InlineButton{Text: number + looseEndDropButton.Format(ctx), CallbackData: data(looseEndDropAction)})
}

type looseEndPress struct {
	action string
	line   int
	id     string
}

// parseLooseEndCallback accepts only the fixed vocabulary of nudge buttons.
func parseLooseEndCallback(data string) (looseEndPress, bool) {
	if len(data) > maximumCallbackDataBytes {
		return looseEndPress{}, false
	}
	parts := strings.Split(data, ":")
	if len(parts) != 4 || parts[0] != looseEndPrefix || !looseEndIDPattern.MatchString(parts[3]) {
		return looseEndPress{}, false
	}
	switch parts[1] {
	case looseEndDoneAction, looseEndTomorrowAction, looseEndDropAction:
	default:
		return looseEndPress{}, false
	}
	line, err := strconv.Atoi(parts[2])
	if err != nil || line < 1 || strconv.Itoa(line) != parts[2] {
		return looseEndPress{}, false
	}
	return looseEndPress{action: parts[1], line: line, id: parts[3]}, true
}

// processLooseEndPress changes the principal's own item and edits its line to the outcome. The
// item's buttons go, except that Tomorrow pressed on the due day leaves Done and Drop; an item that
// is no longer open, or is not the principal's, is said to be so.
func (s *Service) processLooseEndPress(ctx context.Context, callback *CallbackQuery, principalID string, press looseEndPress) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "looseend." + press.action}
	var item looseend.Item
	var changed bool
	var err error
	switch press.action {
	case looseEndDoneAction:
		item, changed, err = s.looseEnds.Close(ctx, principalID, press.id, looseend.StatusDone)
	case looseEndDropAction:
		item, changed, err = s.looseEnds.Close(ctx, principalID, press.id, looseend.StatusDropped)
	default:
		item, changed, err = s.looseEnds.Snooze(ctx, principalID, press.id)
	}
	if err != nil {
		return processingResult{}, "loose_end", err
	}
	var outcome string
	var row []InlineButton
	switch {
	case changed && press.action == looseEndDoneAction:
		outcome = looseEndDoneOutcome.Format(ctx)
	case changed && press.action == looseEndDropAction:
		outcome = looseEndDropOutcome.Format(ctx)
	case changed:
		outcome = looseEndAgainOutcome.Format(ctx)
	case item.Status == looseend.StatusOpen:
		outcome = looseEndDueToday.Format(ctx)
		row = looseEndButtons(ctx, looseEndNumber(callback.Message, press.line), press.line, press.id, false)
	default:
		outcome = LooseEndGoneOutcome.Format(ctx)
	}
	lines := strings.Split(callback.Message.Text, "\n")
	if press.line >= len(lines) {
		// Telegram sent the press without the message, so there is no line to edit.
		return result, "", nil
	}
	lines[press.line] += " — " + outcome
	keyboard := looseEndKeyboard(callback.Message, press.id, row)
	if err := s.client.editPlainCard(ctx, callback.Message.Chat.ID, callback.Message.MessageID, strings.Join(lines, "\n"), keyboard); err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

// looseEndNumber is the number a numbered nudge shows its line with, "" for a nudge of one item.
func looseEndNumber(message *Message, line int) string {
	lines := strings.Split(message.Text, "\n")
	if line >= len(lines) {
		return ""
	}
	if number, _, found := strings.Cut(lines[line], ". "); found {
		if _, err := strconv.Atoi(number); err == nil {
			return number + ". "
		}
	}
	return ""
}

// looseEndKeyboard is the message's buttons with the item's row replaced by row, or left out when
// row is empty.
func looseEndKeyboard(message *Message, id string, row []InlineButton) InlineKeyboard {
	if message.ReplyMarkup == nil {
		return nil
	}
	var keyboard InlineKeyboard
	for _, current := range message.ReplyMarkup.InlineKeyboard {
		if len(current) == 0 {
			continue
		}
		if press, ok := parseLooseEndCallback(current[0].CallbackData); ok && press.id == id {
			if len(row) > 0 {
				keyboard = append(keyboard, row)
			}
			continue
		}
		keyboard = append(keyboard, current)
	}
	return keyboard
}

var (
	LooseEndsTooManyNote  = locale.T("You already have the maximum number of scheduled jobs. Remove one in /jobs, then turn Loose ends on.", "У тебя уже максимальное число заданий по расписанию. Удали одно в /jobs, а затем включи незакрытые дела.")
	LooseEndsBusyNote     = locale.T("A check is already running; anything due arrives in the Reminders tab, or here while tabs are off.", "Проверка уже идёт; всё, что пора закрыть, придёт во вкладку «Напоминания» или сюда, пока вкладки выключены.")
	LooseEndsCheckingNote = locale.T("Checking your loose ends now; anything due arrives in the Reminders tab, or here while tabs are off.", "Проверяю незакрытые дела; всё, что пора закрыть, придёт во вкладку «Напоминания» или сюда, пока вкладки выключены.")

	looseEndsCheckBtn  = locale.T("Check now", "Проверить сейчас")
	looseEndsOffButton = locale.T("Turn off Loose ends", "Выключить незакрытые дела")
	looseEndsOnButton  = locale.T("Turn on Loose ends", "Включить незакрытые дела")
	LooseEndsOnLine    = locale.T("Loose ends: on. Every morning at 07:20 I look through your connected Google mail and calendar, read or unread, for deadlines nobody asked you about: a free-cancellation window, a return window, a renewal, a trial that ends, or something that expires. I also keep the dated promises you make in chat. I stay quiet until one is near, then send one message with Done, Tomorrow, and Drop for each. Ask me any time what is still open. Press Check now to look at once. Uses your model subscription.", "Незакрытые дела: включено. Каждое утро в 07:20 я просматриваю твою подключённую почту и календарь Google, прочитанное и нет, в поисках сроков, о которых никто не спрашивал: бесплатная отмена, срок возврата, продление, конец пробного периода или что-то, что истекает. Ещё я слежу за обещаниями со сроком, которые ты даёшь в чате. Я молчу, пока срок не близко, а потом присылаю одно сообщение с кнопками «Сделано», «Завтра» и «Не нужно» для каждого дела. Спроси меня в любой момент, что ещё не закрыто. Нажми «Проверить сейчас», чтобы посмотреть сразу. Использует твою подписку на модель.")
	LooseEndsOffLine   = locale.T("Loose ends: off. Turn it on and every morning I look for deadlines nobody asked you about in your connected Google mail and calendar, such as a free-cancellation window or a trial that ends, keep the dated promises you make in chat, and tell you only when one is near. What I already keep stays while it is off. Needs a connected model subscription, which it uses.", "Незакрытые дела: выключено. Включи, и каждое утро я буду искать в подключённой почте и календаре Google сроки, о которых никто не спрашивал, например бесплатную отмену или конец пробного периода, следить за обещаниями со сроком, которые ты даёшь в чате, и напоминать, только когда срок близко. То, за чем я уже слежу, сохраняется, пока функция выключена. Нужна подключённая подписка на модель, которую эта функция использует.")
)

// NewLooseEndsSection is the page that turns the principal's Loose ends on and off: the switch the
// tools read, and the daily job that nudges and scans.
func NewLooseEndsSection(schedules ScheduleController, toggle SwitchController, looseEnds LooseEndController, logger *slog.Logger, telemetry Telemetry) Section {
	return looseEndsSection{schedules: schedules, toggle: toggle, looseEnds: looseEnds, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type looseEndsSection struct {
	schedules ScheduleController
	toggle    SwitchController
	looseEnds LooseEndController
	logger    *slog.Logger
	telemetry Telemetry
}

func (l looseEndsSection) prefix() string { return looseEndsSectionPrefix }

func (l looseEndsSection) helpTopic() string { return "loose_ends" }

func (l looseEndsSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	enabled, err := l.toggle.Enabled(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	return settingsSummary{name: looseEndsHeading.Format(ctx), state: stateOf(ctx, enabled)}, nil
}

func (l looseEndsSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := l.toggle.Enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		return settingsView{
			lines: []string{LooseEndsOnLine.Format(ctx)},
			buttons: []InlineButton{
				{Text: looseEndsCheckBtn.Format(ctx), CallbackData: looseEndsSectionPrefix + ":" + looseEndsCheckAction},
				{Text: looseEndsOffButton.Format(ctx), CallbackData: looseEndsSectionPrefix + ":" + looseEndsOffAction},
			},
		}, nil
	}
	return settingsView{
		lines:   []string{LooseEndsOffLine.Format(ctx)},
		buttons: []InlineButton{{Text: looseEndsOnButton.Format(ctx), CallbackData: looseEndsSectionPrefix + ":" + looseEndsOnAction}},
	}, nil
}

func (l looseEndsSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	switch action {
	case looseEndsOnAction, looseEndsCheckAction:
		enabled, err := l.toggle.Enabled(ctx, principalID)
		if err != nil || (!enabled && action == looseEndsCheckAction) {
			return settingsOutcome{}, err
		}
		// With the switch already on, as after a restore, this repairs a missing job or scan state.
		if note, err := l.ensureJob(ctx, principalID); err != nil || note != "" {
			return settingsOutcome{note: note}, err
		}
		if !enabled {
			if err := l.looseEnds.ResetScan(ctx, principalID); err != nil {
				return settingsOutcome{}, err
			}
		}
		if _, err := l.looseEnds.StartScan(ctx, principalID); err != nil {
			return settingsOutcome{}, err
		}
		if !enabled {
			if err := l.toggle.SetEnabled(ctx, principalID, true); err != nil {
				return settingsOutcome{}, err
			}
			l.telemetry.ContextLogger(ctx, l.logger).InfoContext(ctx, "looseends.changed", "principal_id", principalID, "enabled", true)
		}
		return l.check(ctx, principalID)
	case looseEndsOffAction:
		if err := l.toggle.SetEnabled(ctx, principalID, false); err != nil {
			return settingsOutcome{}, err
		}
		if _, err := l.schedules.SetEnabled(ctx, principalID, schedule.LooseEndsJobID(principalID), false); err != nil {
			return settingsOutcome{}, err
		}
		// Turning it on again reads from then and backfills afresh; the tracked items stay.
		if err := l.looseEnds.ResetScan(ctx, principalID); err != nil {
			return settingsOutcome{}, err
		}
		l.telemetry.ContextLogger(ctx, l.logger).InfoContext(ctx, "looseends.changed", "principal_id", principalID, "enabled", false)
		return settingsOutcome{}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

// ensureJob creates the principal's Loose ends job, or enables it when it is paused, and returns a
// note when it cannot be created.
func (l looseEndsSection) ensureJob(ctx context.Context, principalID string) (string, error) {
	id := schedule.LooseEndsJobID(principalID)
	jobs, err := l.schedules.Jobs(ctx, principalID)
	if err != nil {
		return "", err
	}
	for _, job := range jobs {
		if job.ID != id {
			continue
		}
		if !job.Enabled {
			_, err = l.schedules.SetEnabled(ctx, principalID, id, true)
		}
		return "", err
	}
	prepared, err := l.schedules.Prepare(principalID, schedule.LooseEndsName, schedule.LooseEndScanType,
		schedule.Timing{Cron: schedule.LooseEndsCron, Zone: l.schedules.DefaultZone()}, looseend.JobConfigJSON())
	if err != nil {
		return "", err
	}
	if _, err := l.schedules.Create(ctx, id, prepared); err != nil {
		if errors.Is(err, schedule.ErrTooManyJobs) {
			return LooseEndsTooManyNote.Format(ctx), nil
		}
		return "", err
	}
	return "", nil
}

// check queues one run of the principal's Loose ends job, named by the press so a retried update
// queues no second one. A run already queued or running is left to finish.
func (l looseEndsSection) check(ctx context.Context, principalID string) (settingsOutcome, error) {
	id := schedule.LooseEndsJobID(principalID)
	busy, err := l.schedules.Busy(ctx, principalID, id)
	if err != nil {
		return settingsOutcome{}, err
	}
	if busy {
		return settingsOutcome{note: LooseEndsBusyNote.Format(ctx)}, nil
	}
	queued, err := l.schedules.RunNow(ctx, principalID, id, pressTriggerFrom(ctx))
	if err != nil || !queued {
		return settingsOutcome{}, err
	}
	l.telemetry.ContextLogger(ctx, l.logger).InfoContext(ctx, "looseends.check.queued", "principal_id", principalID)
	return settingsOutcome{note: LooseEndsCheckingNote.Format(ctx)}, nil
}

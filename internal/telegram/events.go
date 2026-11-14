package telegram

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/events"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const (
	eventsSectionPrefix = "events"
	eventsAddAction     = "add"
	eventsModeAction    = "mode"
	eventsPauseAction   = "pause"
	eventsResumeAction  = "resume"
	eventsTokenAction   = "token"
	eventsDeleteAction  = "delete"

	// eventNamingLifetime is how long the message after Add event source is taken as its name.
	eventNamingLifetime = 10 * time.Minute
	// messageLimit is the longest message Telegram sends, in UTF-16 code units.
	messageLimit = 4096
)

var (
	EventsLine = locale.T("Event sources let your own systems, such as a backup script or a home automation, message you: they post to /api/events with a source's token. "+
		"Forward sends an event as it is; assess lets me decide from your memory whether it is worth telling you, and always sends a critical one.",
		"Источники событий позволяют твоим собственным системам, например скрипту резервного копирования или умному дому, писать тебе: они отправляют POST на /api/events с токеном источника. "+
			"Режим «пересылка» отправляет событие как есть; режим «оценка» позволяет мне решить по твоей памяти, стоит ли тебе о нём сообщать, а критическое событие я отправляю всегда.")
	EventsNoSourcesLine  = locale.T("No event sources yet.", "Источников событий пока нет.")
	EventsFullLine       = locale.T("You have the most event sources there can be; delete one to add another.", "У тебя максимально возможное число источников событий; удали один, чтобы добавить другой.")
	EventsBadNameReply   = locale.T("That is not a usable name: it must be one line of at most 40 characters. Press Add event source again to retry.", "Такое название не подходит: оно должно быть одной строкой не длиннее 40 символов. Нажми «Добавить источник событий» ещё раз, чтобы повторить.")
	EventsFullReply      = locale.T("You already have the most event sources there can be, so nothing was added. Delete one in /settings first.", "У тебя уже максимально возможное число источников событий, поэтому ничего не добавлено. Сначала удали один в /settings.")
	EventsFailedReply    = locale.T("I could not set up that event source. Please try again later.", "Не удалось настроить этот источник событий. Повтори попытку позже.")
	EventsGoneNote       = locale.T("That event source no longer exists.", "Этого источника событий больше нет.")
	EventsRotateFailNote = locale.T("The old token no longer works, but I could not show the new one. Press New token again.", "Старый токен больше не работает, но показать новый не удалось. Нажми «Новый токен» ещё раз.")

	eventsCreatedReply = locale.T("Event source %q is set up, forwarding events as they are. Its token is in the message above: copy it now, because I delete that message in 10 minutes and never show it again.", "Источник событий %q настроен и пересылает события как есть. Его токен в сообщении выше: скопируй его сейчас, потому что через 10 минут я удалю это сообщение и больше никогда не покажу токен.")
	eventsTokenMessage = locale.T("Token for event source <b>%[1]s</b>:\n<code>%[2]s</code>\n\n"+
		"Send it as <code>Authorization: Bearer</code> on POST /api/events. I delete this message in 10 minutes and never show the token again.",
		"Токен источника событий <b>%[1]s</b>:\n<code>%[2]s</code>\n\n"+
			"Отправляй его как <code>Authorization: Bearer</code> в POST /api/events. Через 10 минут я удалю это сообщение и больше никогда не покажу токен.")
	eventsTokenPlain = locale.T("Token for event source %[1]s:\n%[2]s\n\n"+
		"Send it as Authorization: Bearer on POST /api/events. I delete this message in 10 minutes and never show the token again.",
		"Токен источника событий %[1]s:\n%[2]s\n\n"+
			"Отправляй его как Authorization: Bearer в POST /api/events. Через 10 минут я удалю это сообщение и больше никогда не покажу токен.")

	eventsName         = locale.T("Events", "События")
	eventsNoSources    = locale.T("no sources", "нет источников")
	eventsSourceCount  = locale.N("%[1]d source", "%[1]d sources", "%[1]d источник", "%[1]d источника", "%[1]d источников")
	eventsOnState      = locale.T("on", "включён")
	eventsPausedState  = locale.T("paused", "на паузе")
	eventsForwardMode  = locale.T("forward", "пересылка")
	eventsAssessMode   = locale.T("assess", "оценка")
	eventsSourceLine   = locale.T("• %[1]s: %[2]s, %[3]s.", "• %[1]s: %[2]s, %[3]s.")
	eventsUsedLine     = locale.T("• %[1]s: %[2]s, %[3]s, last event %[4]s.", "• %[1]s: %[2]s, %[3]s, последнее событие %[4]s.")
	eventsAssessButton = locale.T("Assess", "Оценивать")
	eventsForwardBtn   = locale.T("Forward", "Пересылать")
	eventsPauseButton  = locale.T("Pause", "Пауза")
	eventsResumeButton = locale.T("Resume", "Продолжить")
	eventsTokenButton  = locale.T("New token", "Новый токен")
	eventsDeleteButton = locale.T("Delete", "Удалить")
	eventsAddButton    = locale.T("Add event source", "Добавить источник событий")

	eventsForwardsNote = locale.T("%q now forwards its events as they are.", "%q теперь пересылает свои события как есть.")
	eventsAssessNote   = locale.T("%q now has me assess each event and tell you only what is worth it; critical events always reach you.", "%q теперь поручает мне оценивать каждое событие и сообщать тебе только то, что того стоит; критические события доходят до тебя всегда.")
	eventsPausedNote   = locale.T("%q is paused: its events are refused until you resume it.", "%q на паузе: его события отклоняются, пока ты не продолжишь.")
	eventsResumedNote  = locale.T("%q takes events again.", "%q снова принимает события.")
	eventsTokenNote    = locale.T("%q has a new token, in the message below; the old one no longer works.", "У %q новый токен, он в сообщении ниже; старый больше не работает.")
	eventsDeletedNote  = locale.T("%q is deleted; its token no longer works.", "%q удалён; его токен больше не работает.")
	EventsNameReply    = locale.T("Send the new event source's name as your next message within 10 minutes, such as Backups or Home automations; at most 40 characters.", "Отправь название нового источника событий следующим сообщением в течение 10 минут, например «Резервные копии» или «Умный дом»; не больше 40 символов.")
	EventsTokenGuard   = locale.T("That looked like an event source token, so I deleted it and did not send it to the model. To get a new one, press New token in /settings.", "Это было похоже на токен источника событий, поэтому сообщение удалено и модели не отправлено. Чтобы получить новый токен, нажми «Новый токен» в /settings.")
)

// EventsCreatedReply is the English confirmation of a new source, as a test reads it; its token
// went out just before it.
func EventsCreatedReply(name string) string {
	return eventsCreatedReply.FormatIn(locale.English, name)
}

// severityMarkers lead an event's message, so its weight shows before it is read.
var severityMarkers = map[string]string{
	events.SeverityInfo:     "ℹ️",
	events.SeverityWarning:  "⚠️",
	events.SeverityCritical: "🚨",
}

// EventSender delivers events and token messages. Everything an event's system wrote is escaped
// for Telegram's HTML, so it can never add a link or formatting of its own.
type EventSender struct {
	Client *Client
}

var _ events.Sender = EventSender{}

func (e EventSender) SendEvent(ctx context.Context, chatID int64, message events.Message) (int64, error) {
	marker := severityMarkers[message.Severity]
	header := strings.TrimSpace(marker + " " + message.Source)
	formatted := strings.TrimSpace(marker+" <b>"+textEscaper.Replace(message.Source)+"</b>") + "\n<b>" + textEscaper.Replace(message.Title) + "</b>"
	plain := header + "\n" + message.Title
	if text := clipUTF16(message.Text, messageLimit-utf16Length(plain)-2); text != "" {
		formatted += "\n\n" + textEscaper.Replace(text)
		plain += "\n\n" + text
	}
	sent, err := e.Client.sendHTML(ctx, chatID, formatted, plain)
	return sent.MessageID, err
}

func (e EventSender) SendToken(ctx context.Context, chatID int64, source, token string) (int64, error) {
	formatted := eventsTokenMessage.Format(ctx, textEscaper.Replace(source), token)
	plain := eventsTokenPlain.Format(ctx, source, token)
	sent, err := e.Client.sendHTML(ctx, chatID, formatted, plain)
	if err == nil && sent.MessageID <= 0 {
		err = errors.New("Telegram sendMessage returned no message ID")
	}
	return sent.MessageID, err
}

func (e EventSender) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	return e.Client.DeleteMessage(ctx, chatID, messageID)
}

// sendHTML sends HTML the caller built and escaped itself, in the context's tab, and the plain text
// instead when Telegram refuses to parse it.
func (c *Client) sendHTML(ctx context.Context, chatID int64, formatted, plain string) (Message, error) {
	params := url.Values{"chat_id": {strconv.FormatInt(chatID, 10)}}
	var message Message
	err := c.inTab(ctx, chatID, true, func(ctx context.Context) error {
		params.Del("message_thread_id")
		setThread(ctx, params)
		params.Set("text", formatted)
		params.Set("parse_mode", "HTML")
		var err error
		message, err = execute[Message](c, ctx, "sendMessage", params)
		if hasUnparsableEntities(err) {
			setPlainText(params, plain)
			message, err = execute[Message](c, ctx, "sendMessage", params)
		}
		return err
	})
	return message, err
}

// clipUTF16 shortens text to at most limit UTF-16 code units, marking a cut with an ellipsis.
func clipUTF16(text string, limit int) string {
	if utf16Length(text) <= limit {
		return text
	}
	if limit <= 1 {
		return ""
	}
	length := 0
	for index, r := range text {
		if length+utf16Length(string(r)) > limit-1 {
			return text[:index] + "…"
		}
		length += utf16Length(string(r))
	}
	return text
}

// EventController is the event service as the settings page sees it.
type EventController interface {
	Sources(ctx context.Context, principalID string) ([]events.Source, error)
	Create(ctx context.Context, principalID, name string) (events.Source, error)
	Rotate(ctx context.Context, principalID, id string) (bool, error)
	SetEnabled(ctx context.Context, principalID, id string, enabled bool) (bool, error)
	SetMode(ctx context.Context, principalID, id, mode string) (bool, error)
	Delete(ctx context.Context, principalID, id string) (bool, error)
}

// NewEventsSection is the page that adds, pauses, rotates, and deletes the principal's event
// sources. It takes the message after Add event source as the new source's name, and keeps any
// token-looking message from the model.
func NewEventsSection(controller EventController, location *time.Location, logger *slog.Logger, telemetry Telemetry) Section {
	if location == nil {
		location = time.UTC
	}
	return &eventsSection{events: controller, location: location, logger: logger, telemetry: telemetryOrNoop(telemetry), naming: make(map[string]time.Time)}
}

type eventsSection struct {
	events    EventController
	location  *time.Location
	logger    *slog.Logger
	telemetry Telemetry

	mu     sync.Mutex
	naming map[string]time.Time
}

func (e *eventsSection) prefix() string { return eventsSectionPrefix }

func (e *eventsSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	sources, err := e.events.Sources(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	switch len(sources) {
	case 0:
		return settingsSummary{name: eventsName.Format(ctx), state: eventsNoSources.Format(ctx)}, nil
	default:
		return settingsSummary{name: eventsName.Format(ctx), state: eventsSourceCount.Format(ctx, len(sources))}, nil
	}
}

func (e *eventsSection) render(ctx context.Context, principalID string) (settingsView, error) {
	sources, err := e.events.Sources(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	view := settingsView{lines: []string{EventsLine.Format(ctx)}}
	if len(sources) == 0 {
		view.lines = append(view.lines, EventsNoSourcesLine.Format(ctx))
	}
	for _, source := range sources {
		state := eventsOnState.Format(ctx)
		if !source.Enabled {
			state = eventsPausedState.Format(ctx)
		}
		mode := source.Mode
		switch mode {
		case events.ModeForward:
			mode = eventsForwardMode.Format(ctx)
		case events.ModeAssess:
			mode = eventsAssessMode.Format(ctx)
		}
		line := eventsSourceLine.Format(ctx, source.Name, mode, state)
		if !source.LastUsedAt.IsZero() {
			line = eventsUsedLine.Format(ctx, source.Name, mode, state, locale.Date(locale.From(ctx), source.LastUsedAt.In(e.location), locale.DateTime))
		}
		view.lines = append(view.lines, line)
		modeButton := InlineButton{Text: eventsAssessButton.Format(ctx), CallbackData: eventsCallback(eventsModeAction, source.ID)}
		if source.Mode == events.ModeAssess {
			modeButton.Text = eventsForwardBtn.Format(ctx)
		}
		pause := InlineButton{Text: eventsPauseButton.Format(ctx), CallbackData: eventsCallback(eventsPauseAction, source.ID)}
		if !source.Enabled {
			pause = InlineButton{Text: eventsResumeButton.Format(ctx), CallbackData: eventsCallback(eventsResumeAction, source.ID)}
		}
		view.rows = append(view.rows,
			[]InlineButton{{Text: source.Name, CallbackData: labelCallbackData}},
			[]InlineButton{modeButton, pause,
				{Text: eventsTokenButton.Format(ctx), CallbackData: eventsCallback(eventsTokenAction, source.ID)},
				{Text: eventsDeleteButton.Format(ctx), CallbackData: eventsCallback(eventsDeleteAction, source.ID)}},
		)
	}
	if len(sources) < events.MaximumSources {
		view.buttons = []InlineButton{{Text: eventsAddButton.Format(ctx), CallbackData: eventsSectionPrefix + ":" + eventsAddAction}}
	} else {
		view.lines = append(view.lines, EventsFullLine.Format(ctx))
	}
	return view, nil
}

func eventsCallback(action, sourceID string) string {
	return eventsSectionPrefix + ":" + action + ":" + sourceID
}

func (e *eventsSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	if action == eventsAddAction {
		sources, err := e.events.Sources(ctx, principalID)
		if err != nil {
			return settingsOutcome{}, err
		}
		if len(sources) >= events.MaximumSources {
			return settingsOutcome{note: EventsFullLine.Format(ctx)}, nil
		}
		e.mu.Lock()
		e.naming[principalID] = time.Now().Add(eventNamingLifetime)
		e.mu.Unlock()
		return settingsOutcome{message: EventsNameReply.Format(ctx)}, nil
	}
	verb, sourceID, found := strings.Cut(action, ":")
	if !found || sourceID == "" {
		return settingsOutcome{}, errUnknownSettingsAction
	}
	sources, err := e.events.Sources(ctx, principalID)
	if err != nil {
		return settingsOutcome{}, err
	}
	var source events.Source
	for _, candidate := range sources {
		if candidate.ID == sourceID {
			source = candidate
		}
	}
	if source.ID == "" {
		return settingsOutcome{note: EventsGoneNote.Format(ctx)}, nil
	}
	var changed bool
	var note string
	switch verb {
	case eventsModeAction:
		mode := events.ModeAssess
		if source.Mode == events.ModeAssess {
			mode = events.ModeForward
		}
		changed, err = e.events.SetMode(ctx, principalID, sourceID, mode)
		note = eventsForwardsNote.Format(ctx, source.Name)
		if mode == events.ModeAssess {
			note = eventsAssessNote.Format(ctx, source.Name)
		}
	case eventsPauseAction, eventsResumeAction:
		enabled := verb == eventsResumeAction
		changed, err = e.events.SetEnabled(ctx, principalID, sourceID, enabled)
		note = eventsPausedNote.Format(ctx, source.Name)
		if enabled {
			note = eventsResumedNote.Format(ctx, source.Name)
		}
	case eventsTokenAction:
		changed, err = e.events.Rotate(ctx, principalID, sourceID)
		if changed && err != nil {
			e.telemetry.ContextLogger(ctx, e.logger).WarnContext(ctx, "events.token.show_failed", "principal_id", principalID, "source_id", sourceID, "error", err)
			return settingsOutcome{note: EventsRotateFailNote.Format(ctx)}, nil
		}
		note = eventsTokenNote.Format(ctx, source.Name)
	case eventsDeleteAction:
		changed, err = e.events.Delete(ctx, principalID, sourceID)
		note = eventsDeletedNote.Format(ctx, source.Name)
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
	if err != nil {
		return settingsOutcome{}, err
	}
	if !changed {
		return settingsOutcome{note: EventsGoneNote.Format(ctx)}, nil
	}
	return settingsOutcome{note: note}, nil
}

// intercept takes the message after Add event source as the new source's name, and keeps any
// token-looking message out of the model whether or not a name is awaited.
func (e *eventsSection) intercept(ctx context.Context, principalID string, text string) (interception, error) {
	if guarded := e.guard(ctx, principalID, text); guarded.handled {
		return guarded, nil
	}
	if !e.consumeNaming(principalID) {
		return interception{}, nil
	}
	name, err := events.NormalizeName(text)
	if err != nil {
		return interception{handled: true, reply: EventsBadNameReply.Format(ctx), reason: "events_name_invalid"}, nil
	}
	source, err := e.events.Create(ctx, principalID, name)
	switch {
	case errors.Is(err, events.ErrFull):
		return interception{handled: true, reply: EventsFullReply.Format(ctx), reason: "events_full"}, nil
	case err != nil:
		e.telemetry.ContextLogger(ctx, e.logger).WarnContext(ctx, "events.source.create_failed", "principal_id", principalID, "error", err)
		return interception{handled: true, reply: EventsFailedReply.Format(ctx), reason: "events_create_failed"}, nil
	}
	return interception{handled: true, reply: eventsCreatedReply.Format(ctx, source.Name), reason: "events_source_created"}, nil
}

func (e *eventsSection) guard(ctx context.Context, _ string, text string) interception {
	if !events.LooksLikeToken(text) {
		return interception{}
	}
	return interception{handled: true, remove: true, reply: EventsTokenGuard.Format(ctx), reason: "events_token_guard"}
}

func (e *eventsSection) consumeNaming(principalID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	expires, pending := e.naming[principalID]
	delete(e.naming, principalID)
	return pending && time.Now().Before(expires)
}

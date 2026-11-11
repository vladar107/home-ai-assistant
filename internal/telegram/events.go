package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/events"
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

	EventsLine = "Event sources let your own systems, such as a backup script or a home automation, message you: they post to /api/events with a source's token. " +
		"Forward sends an event as it is; assess lets me decide from your memory whether it is worth telling you, and always sends a critical one."
	EventsNoSourcesLine  = "No event sources yet."
	EventsFullLine       = "You have the most event sources there can be; delete one to add another."
	EventsNameReply      = "Send the new event source's name as your next message within 10 minutes, such as Backups or Home automations; at most 40 characters."
	EventsBadNameReply   = "That is not a usable name: it must be one line of at most 40 characters. Press Add event source again to retry."
	EventsFullReply      = "You already have the most event sources there can be, so nothing was added. Delete one in /settings first."
	EventsFailedReply    = "I could not set up that event source. Please try again later."
	EventsGoneNote       = "That event source no longer exists."
	EventsTokenGuard     = "That looked like an event source token, so I deleted it and did not send it to the model. To get a new one, press New token in /settings."
	EventsRotateFailNote = "The old token no longer works, but I could not show the new one. Press New token again."
)

// EventsCreatedReply confirms a new source; its token went out just before it.
func EventsCreatedReply(name string) string {
	return fmt.Sprintf("Event source %q is set up, forwarding events as they are. Its token is in the message above: copy it now, because I delete that message in 10 minutes and never show it again.", name)
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
	name := textEscaper.Replace(source)
	formatted := "Token for event source <b>" + name + "</b>:\n<code>" + token + "</code>\n\n" +
		"Send it as <code>Authorization: Bearer</code> on POST /api/events. I delete this message in 10 minutes and never show the token again."
	plain := "Token for event source " + source + ":\n" + token + "\n\n" +
		"Send it as Authorization: Bearer on POST /api/events. I delete this message in 10 minutes and never show the token again."
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
		return settingsSummary{name: "Events", state: "no sources"}, nil
	case 1:
		return settingsSummary{name: "Events", state: "1 source"}, nil
	default:
		return settingsSummary{name: "Events", state: fmt.Sprintf("%d sources", len(sources))}, nil
	}
}

func (e *eventsSection) render(ctx context.Context, principalID string) (settingsView, error) {
	sources, err := e.events.Sources(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	view := settingsView{lines: []string{EventsLine}}
	if len(sources) == 0 {
		view.lines = append(view.lines, EventsNoSourcesLine)
	}
	for _, source := range sources {
		state := "on"
		if !source.Enabled {
			state = "paused"
		}
		line := fmt.Sprintf("• %s: %s, %s", source.Name, source.Mode, state)
		if !source.LastUsedAt.IsZero() {
			line += ", last event " + source.LastUsedAt.In(e.location).Format("2 Jan 15:04")
		}
		view.lines = append(view.lines, line+".")
		mode := InlineButton{Text: "Assess", CallbackData: eventsCallback(eventsModeAction, source.ID)}
		if source.Mode == events.ModeAssess {
			mode.Text = "Forward"
		}
		pause := InlineButton{Text: "Pause", CallbackData: eventsCallback(eventsPauseAction, source.ID)}
		if !source.Enabled {
			pause = InlineButton{Text: "Resume", CallbackData: eventsCallback(eventsResumeAction, source.ID)}
		}
		view.rows = append(view.rows,
			[]InlineButton{{Text: source.Name, CallbackData: labelCallbackData}},
			[]InlineButton{mode, pause,
				{Text: "New token", CallbackData: eventsCallback(eventsTokenAction, source.ID)},
				{Text: "Delete", CallbackData: eventsCallback(eventsDeleteAction, source.ID)}},
		)
	}
	if len(sources) < events.MaximumSources {
		view.buttons = []InlineButton{{Text: "Add event source", CallbackData: eventsSectionPrefix + ":" + eventsAddAction}}
	} else {
		view.lines = append(view.lines, EventsFullLine)
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
			return settingsOutcome{note: EventsFullLine}, nil
		}
		e.mu.Lock()
		e.naming[principalID] = time.Now().Add(eventNamingLifetime)
		e.mu.Unlock()
		return settingsOutcome{message: EventsNameReply}, nil
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
		return settingsOutcome{note: EventsGoneNote}, nil
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
		note = fmt.Sprintf("%q now forwards its events as they are.", source.Name)
		if mode == events.ModeAssess {
			note = fmt.Sprintf("%q now has me assess each event and tell you only what is worth it; critical events always reach you.", source.Name)
		}
	case eventsPauseAction, eventsResumeAction:
		enabled := verb == eventsResumeAction
		changed, err = e.events.SetEnabled(ctx, principalID, sourceID, enabled)
		note = fmt.Sprintf("%q is paused: its events are refused until you resume it.", source.Name)
		if enabled {
			note = fmt.Sprintf("%q takes events again.", source.Name)
		}
	case eventsTokenAction:
		changed, err = e.events.Rotate(ctx, principalID, sourceID)
		if changed && err != nil {
			e.telemetry.ContextLogger(ctx, e.logger).WarnContext(ctx, "events.token.show_failed", "principal_id", principalID, "source_id", sourceID, "error", err)
			return settingsOutcome{note: EventsRotateFailNote}, nil
		}
		note = fmt.Sprintf("%q has a new token, in the message below; the old one no longer works.", source.Name)
	case eventsDeleteAction:
		changed, err = e.events.Delete(ctx, principalID, sourceID)
		note = fmt.Sprintf("%q is deleted; its token no longer works.", source.Name)
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
	if err != nil {
		return settingsOutcome{}, err
	}
	if !changed {
		return settingsOutcome{note: EventsGoneNote}, nil
	}
	return settingsOutcome{note: note}, nil
}

// intercept takes the message after Add event source as the new source's name, and keeps any
// token-looking message out of the model whether or not a name is awaited.
func (e *eventsSection) intercept(ctx context.Context, principalID string, text string) (interception, error) {
	if guarded := e.guard(principalID, text); guarded.handled {
		return guarded, nil
	}
	if !e.consumeNaming(principalID) {
		return interception{}, nil
	}
	name, err := events.NormalizeName(text)
	if err != nil {
		return interception{handled: true, reply: EventsBadNameReply, reason: "events_name_invalid"}, nil
	}
	source, err := e.events.Create(ctx, principalID, name)
	switch {
	case errors.Is(err, events.ErrFull):
		return interception{handled: true, reply: EventsFullReply, reason: "events_full"}, nil
	case err != nil:
		e.telemetry.ContextLogger(ctx, e.logger).WarnContext(ctx, "events.source.create_failed", "principal_id", principalID, "error", err)
		return interception{handled: true, reply: EventsFailedReply, reason: "events_create_failed"}, nil
	}
	return interception{handled: true, reply: EventsCreatedReply(source.Name), reason: "events_source_created"}, nil
}

func (e *eventsSection) guard(_ string, text string) interception {
	if !events.LooksLikeToken(text) {
		return interception{}
	}
	return interception{handled: true, remove: true, reply: EventsTokenGuard, reason: "events_token_guard"}
}

func (e *eventsSection) consumeNaming(principalID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	expires, pending := e.naming[principalID]
	delete(e.naming, principalID)
	return pending && time.Now().Before(expires)
}

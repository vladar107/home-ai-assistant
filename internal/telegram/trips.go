package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

const (
	tripPrefix       = "trip"
	tripTrackAction  = "track"
	tripIgnoreAction = "ignore"

	tripsSectionPrefix = "trips"
	tripsOnAction      = "on"
	tripsOffAction     = "off"
	tripsScanAction    = "scan"

	// tripCardBookings and tripCardRunes keep a card well inside Telegram's 4096-character limit.
	tripCardBookings = 8
	tripCardRunes    = 3500

	TripGoneReply      = "This trip no longer exists."
	TripTrackedReply   = "Already tracking this trip."
	TripIgnoredReply   = "Ignored; I won't offer it again."
	TripCancelledReply = "This trip was cancelled."

	TripsOnLine  = "Trips: on. Each morning I look in your mail and calendar for travel bookings and offer each trip with Track and Ignore buttons; nothing is planned without your press. Press Scan now to look at once. Uses your model subscription."
	TripsOffLine = "Trips: off. Turn it on and I look in your mail and calendar for travel bookings at once and then each morning, and offer each trip with Track and Ignore buttons; nothing is planned without your press. Needs a connected model subscription, which it uses."
	// TripsNeedsGoogleLine explains why the switch is not offered.
	TripsNeedsGoogleLine = "Trips: off. It reads your mail and calendar, so turn on Mail or Calendar for a connected Google account in Accounts › Google first."
	TripsTooManyNote     = "You already have the maximum number of scheduled jobs. Remove one in /jobs, then turn Trips on."
	TripsScanningNote    = "Scanning your mail and calendar now; any trips I find arrive here as cards."
	TripsBusyNote        = "A scan is already running; any trips it finds arrive here as cards."
)

// TripController is implemented by the trip service; nil leaves the buttons of trip cards and
// trip tabs out. The Trips switch is NewTripsSection's.
type TripController interface {
	Track(ctx context.Context, principalID, id string) (trip.Result, error)
	Dismiss(ctx context.Context, principalID, id string) (trip.Result, error)
	Trip(ctx context.Context, principalID, id string) (trip.Trip, bool, error)
	// SetSummaryMessage records the message that sums the trip up in its tab.
	SetSummaryMessage(ctx context.Context, principalID, id string, messageID int64) error
	// Now is the trips' clock, which says whether a trip has ended.
	Now() time.Time
}

// TripChats resolves the principal's active chat.
type TripChats interface {
	ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
}

// TripFeeds resolves the tabs trip messages go to and keeps each trip's tab in step with the trip;
// the service's Feeds implement it.
type TripFeeds interface {
	thread.Feeds
	// Summarize gives a planned trip its tab when it has none, and keeps its pinned summary and
	// the tab's name current.
	Summarize(ctx context.Context, principalID string, chatID int64, tripID string)
}

// TripNotifier delivers suggested trips as cards with Track and Ignore buttons, and plain trip
// notices, to the principal's active chat. Its messages carry no markup and no link previews.
type TripNotifier struct {
	Client *Client
	Chats  TripChats
	// Feeds puts a trip's notices in its own tab, and cards and notices of a trip without one in
	// the reminders tab, while the bot has tabs on; nil sends them under the caller's context.
	Feeds TripFeeds
}

var _ trip.Tabs = TripNotifier{}

func (n TripNotifier) SendTripCard(ctx context.Context, found trip.Trip) error {
	chatID, err := n.chat(ctx, found.PrincipalID)
	if err != nil {
		return err
	}
	ctx = n.inFeed(ctx, found.PrincipalID, chatID, "")
	keyboard := InlineKeyboard{{
		{Text: "Track", CallbackData: tripPrefix + ":" + tripTrackAction + ":" + found.ID},
		{Text: "Ignore", CallbackData: tripPrefix + ":" + tripIgnoreAction + ":" + found.ID},
	}}
	return n.Client.sendPlain(ctx, chatID, tripCardText(found), keyboard)
}

func (n TripNotifier) Send(ctx context.Context, principalID, tripID, text string) error {
	chatID, err := n.chat(ctx, principalID)
	if err != nil {
		return err
	}
	return n.Client.sendPlain(n.inFeed(ctx, principalID, chatID, tripID), chatID, text, nil)
}

// TripChanged keeps the trip's tab and its summary in step with a write to the trip. A failure is
// logged by the resolver and leaves the next write to try again.
func (n TripNotifier) TripChanged(ctx context.Context, changed trip.Trip) {
	if n.Feeds == nil {
		return
	}
	chatID, err := n.chat(ctx, changed.PrincipalID)
	if err != nil {
		return
	}
	n.Feeds.Summarize(ctx, changed.PrincipalID, chatID, changed.ID)
}

// inFeed returns a context whose sends go to the trip's tab, or to the reminders tab for a trip
// without one or for no trip. A tab that cannot be had leaves the message in the main view; the
// resolver logs why.
func (n TripNotifier) inFeed(ctx context.Context, principalID string, chatID int64, tripID string) context.Context {
	if n.Feeds == nil {
		return ctx
	}
	var threadID int64
	var err error
	if tripID != "" {
		threadID, err = n.Feeds.Destination(ctx, principalID, chatID, thread.TripDestination(tripID), thread.FeedReminders)
	} else {
		threadID, err = n.Feeds.Thread(ctx, principalID, chatID, thread.FeedReminders)
	}
	if err != nil {
		threadID = 0
	}
	return thread.WithDelivery(ctx, threadID)
}

func (n TripNotifier) chat(ctx context.Context, principalID string) (int64, error) {
	chatID, found, err := n.Chats.ResolveActiveChat(ctx, principalID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, errors.New("the principal has no active chat")
	}
	return chatID, nil
}

func (c *Client) sendPlain(ctx context.Context, chatID int64, text string, keyboard InlineKeyboard) error {
	_, err := c.sendPlainIn(ctx, chatID, text, keyboard, true)
	return err
}

// SendPlainInTab posts plain text without link previews in the context's tab and nowhere else, and
// returns its message ID: a tab that is not found is reported to the observer, and the error
// carries its verdict.
func (c *Client) SendPlainInTab(ctx context.Context, chatID int64, text string) (int64, error) {
	message, err := c.sendPlainIn(ctx, chatID, text, nil, false)
	if err != nil {
		return 0, err
	}
	if message.MessageID <= 0 {
		return 0, fmt.Errorf("Telegram sendMessage returned no message ID")
	}
	return message.MessageID, nil
}

func (c *Client) sendPlainIn(ctx context.Context, chatID int64, text string, keyboard InlineKeyboard, fallback bool) (Message, error) {
	params := url.Values{"chat_id": {strconv.FormatInt(chatID, 10)}}
	if err := setKeyboard(params, keyboard); err != nil {
		return Message{}, err
	}
	setPlainText(params, text)
	setNoLinkPreview(params)
	var message Message
	err := c.inTab(ctx, chatID, fallback, func(ctx context.Context) error {
		params.Del("message_thread_id")
		setThread(ctx, params)
		var err error
		message, err = execute[Message](c, ctx, "sendMessage", params)
		return err
	})
	return message, err
}

// PinChatMessage pins a message without a notification, which Telegram does in private chats
// anyway.
func (c *Client) PinChatMessage(ctx context.Context, chatID, messageID int64) error {
	params := url.Values{
		"chat_id":              {strconv.FormatInt(chatID, 10)},
		"message_id":           {strconv.FormatInt(messageID, 10)},
		"disable_notification": {"true"},
	}
	_, err := execute[bool](c, ctx, "pinChatMessage", params)
	return err
}

// EditPlain replaces a bot message's text without markup or link previews and removes its buttons.
func (c *Client) EditPlain(ctx context.Context, chatID, messageID int64, text string) error {
	params := url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
	}
	setPlainText(params, text)
	setNoLinkPreview(params)
	_, err := execute[json.RawMessage](c, ctx, "editMessageText", params)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.NotModified() {
		return nil
	}
	return err
}

func setNoLinkPreview(params url.Values) {
	params.Set("link_preview_options", `{"is_disabled":true}`)
}

type plainEditor interface {
	EditPlain(ctx context.Context, chatID, messageID int64, text string) error
}

func tripCheckInSource(source string) string {
	switch source {
	case trip.CheckInStated:
		return "stated in the booking"
	case trip.CheckInLookedUp:
		return "looked up on the provider's site"
	default:
		return "assumed, 24 h before the start"
	}
}

func tripSpan(found trip.Trip, location *time.Location) string {
	const layout = "Mon 2 Jan 2006"
	return found.StartsAt.In(location).Format(layout) + " to " + found.EndLabel(layout, layout)
}

func tripZone(found trip.Trip) *time.Location {
	location, err := schedule.LoadZone(found.Zone)
	if err != nil {
		return time.UTC
	}
	return location
}

// tripCardText is built from the validated fields of the trip only.
func tripCardText(found trip.Trip) string {
	location := tripZone(found)
	source := "mail"
	if found.Source == trip.SourceCalendar {
		source = "your calendar"
	}
	lines := []string{
		"Trip found in " + source + ":",
		found.Title + " (" + found.Destination + ")",
		tripSpan(found, location) + ", times in " + found.Zone,
		"",
	}
	for index, booking := range found.Bookings {
		if index == tripCardBookings {
			lines = append(lines, fmt.Sprintf("...and %d more", len(found.Bookings)-tripCardBookings))
			break
		}
		line := "- " + booking.Title + ", " + booking.StartsAt.In(location).Format("Mon 2 Jan 15:04")
		if booking.Reference != "" {
			line += " (" + booking.Reference + ")"
		}
		if booking.CheckInAt.IsZero() {
			line += ": no check-in time"
		} else {
			line += ": check-in opens " + booking.CheckInAt.In(location).Format("Mon 2 Jan 15:04") + " (" + tripCheckInSource(booking.CheckInSource) + ")"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", "Track it for check-in reminders, or ignore it and I won't offer it again.")
	text := strings.Join(lines, "\n")
	if runes := []rune(text); len(runes) > tripCardRunes {
		text = string(runes[:tripCardRunes-1]) + "…"
	}
	return text
}

var tripUnsetReasons = map[string]string{
	trip.UnsetPast:     "check-in is already open",
	trip.UnsetCap:      "too many scheduled jobs",
	trip.UnsetFar:      "more than a year ahead",
	trip.UnsetAfterEnd: "check-in opens after the trip ends",
	trip.UnsetInvalid:  "the time was refused",
}

func tripTrackedText(result trip.Result) string {
	found := result.Trip
	location := tripZone(found)
	unset := make(map[string]string, len(result.Unset))
	for _, entry := range result.Unset {
		unset[entry.BookingID] = tripUnsetReasons[entry.Reason]
	}
	lines := []string{"Tracking: " + found.Title + " (" + found.Destination + "), " + tripSpan(found, location) + "."}
	for _, booking := range found.Bookings {
		if booking.CheckInAt.IsZero() {
			continue
		}
		if reason, skipped := unset[booking.ID]; skipped {
			lines = append(lines, "No reminder for "+booking.Title+": "+reason+".")
			continue
		}
		lines = append(lines, "Reminder for "+booking.Title+" at "+booking.CheckInAt.In(location).Format("Mon 2 Jan 15:04")+".")
	}
	lines = append(lines, "Tip: ask me to pause your home-bound reminders while you're away.")
	return strings.Join(lines, "\n")
}

// parseTripCallback accepts only the fixed button vocabulary.
func parseTripCallback(data string) (string, string, bool) {
	if len(data) > maximumCallbackDataBytes {
		return "", "", false
	}
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != tripPrefix || parts[2] == "" {
		return "", "", false
	}
	if parts[1] != tripTrackAction && parts[1] != tripIgnoreAction {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// processTripPress tracks or ignores the principal's own suggested trip and replaces the card with
// the outcome. The service looks trips up by principal, so another user's press finds nothing.
func (s *Service) processTripPress(ctx context.Context, callback *CallbackQuery, principalID, action, tripID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "trip." + action}
	var outcome trip.Result
	var err error
	if action == tripTrackAction {
		outcome, err = s.trips.Track(ctx, principalID, tripID)
	} else {
		outcome, err = s.trips.Dismiss(ctx, principalID, tripID)
	}
	if err != nil {
		return processingResult{}, "trip_action", err
	}
	text := strings.TrimSpace(callback.Message.Text)
	switch {
	case outcome.Rejection == "" && action == tripTrackAction:
		text = tripTrackedText(outcome)
	case outcome.Rejection == "":
		text += "\n\n" + TripIgnoredReply
	default:
		known, found, err := s.trips.Trip(ctx, principalID, tripID)
		if err != nil {
			return processingResult{}, "trip_action", err
		}
		text += "\n\n" + staleTripReply(known, found)
	}
	if err := s.editPlain(ctx, callback.Message.Chat.ID, callback.Message.MessageID, text); err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

func staleTripReply(known trip.Trip, found bool) string {
	switch {
	case !found:
		return TripGoneReply
	case known.Status == trip.StatusPlanned:
		return TripTrackedReply
	case known.Status == trip.StatusDismissed:
		return TripIgnoredReply
	case known.Status == trip.StatusCancelled:
		return TripCancelledReply
	}
	return TripGoneReply
}

func (s *Service) editPlain(ctx context.Context, chatID, messageID int64, text string) error {
	if editor, ok := s.client.(plainEditor); ok {
		return editor.EditPlain(ctx, chatID, messageID, text)
	}
	return s.client.EditKeyboard(ctx, chatID, messageID, text, nil)
}

// NewTripsSection is the page that turns the principal's Trips scan of Google mail and calendar on
// and off.
func NewTripsSection(schedules ScheduleController, google GoogleController, logger *slog.Logger, telemetry Telemetry) Section {
	return tripsSection{schedules: schedules, google: google, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type tripsSection struct {
	schedules ScheduleController
	google    GoogleController
	logger    *slog.Logger
	telemetry Telemetry
}

func (t tripsSection) prefix() string { return tripsSectionPrefix }

func (t tripsSection) helpTopic() string { return "trips" }

func (t tripsSection) enabled(ctx context.Context, principalID string) (bool, error) {
	jobs, err := t.schedules.Jobs(ctx, principalID)
	if err != nil {
		return false, err
	}
	id := schedule.TripsJobID(principalID)
	for _, job := range jobs {
		if job.ID == id {
			return job.Enabled, nil
		}
	}
	return false, nil
}

// readable is true while a connected Google account has Mail or Calendar granted and turned on.
func (t tripsSection) readable(ctx context.Context, principalID string) (bool, error) {
	statuses, err := t.google.Status(ctx, principalID)
	if err != nil {
		return false, err
	}
	for _, status := range statuses {
		if !status.NeedsReconnect && googleReadable(status) {
			return true, nil
		}
	}
	return false, nil
}

func googleReadable(status google.AccountStatus) bool {
	return status.MailGranted && status.MailEnabled || status.CalendarGranted && status.CalendarEnabled
}

func (t tripsSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	enabled, err := t.enabled(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	return settingsSummary{name: "Trips", state: onOff(enabled)}, nil
}

func (t tripsSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := t.enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		return settingsView{
			lines: []string{TripsOnLine},
			buttons: []InlineButton{
				{Text: "Scan now", CallbackData: tripsSectionPrefix + ":" + tripsScanAction},
				{Text: "Turn off Trips", CallbackData: tripsSectionPrefix + ":" + tripsOffAction},
			},
		}, nil
	}
	readable, err := t.readable(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if !readable {
		return settingsView{lines: []string{TripsNeedsGoogleLine}}, nil
	}
	return settingsView{
		lines:   []string{TripsOffLine},
		buttons: []InlineButton{{Text: "Turn on Trips", CallbackData: tripsSectionPrefix + ":" + tripsOnAction}},
	}, nil
}

func (t tripsSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	id := schedule.TripsJobID(principalID)
	switch action {
	case tripsOnAction:
		readable, err := t.readable(ctx, principalID)
		if err != nil {
			return settingsOutcome{}, err
		}
		if !readable {
			return settingsOutcome{note: TripsNeedsGoogleLine}, nil
		}
		jobs, err := t.schedules.Jobs(ctx, principalID)
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
			if _, err := t.schedules.SetEnabled(ctx, principalID, id, true); err != nil {
				return settingsOutcome{}, err
			}
			t.telemetry.ContextLogger(ctx, t.logger).InfoContext(ctx, "trips.changed", "principal_id", principalID, "enabled", true)
			return t.scan(ctx, principalID)
		}
		prepared, err := t.schedules.Prepare(principalID, schedule.TripsName, schedule.TripScanType,
			schedule.Timing{Cron: schedule.TripsCron, Zone: t.schedules.DefaultZone()}, trip.ScanConfig())
		if err != nil {
			return settingsOutcome{}, err
		}
		created, err := t.schedules.Create(ctx, id, prepared)
		if err != nil {
			if errors.Is(err, schedule.ErrTooManyJobs) {
				return settingsOutcome{note: TripsTooManyNote}, nil
			}
			return settingsOutcome{}, err
		}
		if !created {
			return settingsOutcome{}, nil
		}
		t.telemetry.ContextLogger(ctx, t.logger).InfoContext(ctx, "trips.changed", "principal_id", principalID, "enabled", true)
		return t.scan(ctx, principalID)
	case tripsScanAction:
		enabled, err := t.enabled(ctx, principalID)
		if err != nil || !enabled {
			return settingsOutcome{}, err
		}
		return t.scan(ctx, principalID)
	case tripsOffAction:
		deleted, err := t.schedules.Delete(ctx, principalID, id)
		if err != nil {
			return settingsOutcome{}, err
		}
		if deleted {
			t.telemetry.ContextLogger(ctx, t.logger).InfoContext(ctx, "trips.changed", "principal_id", principalID, "enabled", false)
		}
		return settingsOutcome{}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

// scan queues one run of the principal's Trips scan, named by the press so a retried update
// queues no second one. A scan already queued or running is left to finish, so two far scans
// never share the user's subscription.
func (t tripsSection) scan(ctx context.Context, principalID string) (settingsOutcome, error) {
	id := schedule.TripsJobID(principalID)
	busy, err := t.schedules.Busy(ctx, principalID, id)
	if err != nil {
		return settingsOutcome{}, err
	}
	if busy {
		return settingsOutcome{note: TripsBusyNote}, nil
	}
	queued, err := t.schedules.RunNow(ctx, principalID, id, pressTriggerFrom(ctx))
	if err != nil || !queued {
		return settingsOutcome{}, err
	}
	t.telemetry.ContextLogger(ctx, t.logger).InfoContext(ctx, "trips.scan.queued", "principal_id", principalID)
	return settingsOutcome{note: TripsScanningNote}, nil
}

package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/locale"
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
	tripsTabAction     = "tab"

	// tripTabButtons is how many trips the Trips page offers to move to its tab.
	tripTabButtons = 5

	// tripCardBookings and tripCardRunes keep a card well inside Telegram's 4096-character limit.
	tripCardBookings = 8
	tripCardRunes    = 3500

	// TripsTabHelp is added to the app_help topic of the page.
	TripsTabHelp = "With tabs on, each planned trip that has not ended, up to five, has a button \"Use this tab for\" with its title on this page when the page is opened in a tab the trip can use: a tab of conversations the user has, or the tab of a trip that is over. Pressing it moves the trip there with its pinned summary, reminders, notices, and news; the tab keeps its name and icon. The main view, the Reminders and other feed tabs, and the tab of another trip, a project, or a watch cannot be used. The user sends /settings in a tab that already has a message, since /settings as the first message of a new tab is answered in the main view. Saying in a tab that it is the trip's tab also moves the trip."
)

var (
	TripCancelledReply = locale.T("This trip was cancelled.", "Эта поездка отменена.")
	TripsTooManyNote   = locale.T("You already have the maximum number of scheduled jobs. Remove one in /jobs, then turn Trips on.", "У вас уже максимальное число заданий по расписанию. Удалите одно в /jobs, а затем включите «Поездки».")

	tripTrackButton      = locale.T("Track", "Следить")
	tripIgnoreButton     = locale.T("Ignore", "Игнорировать")
	tripScanButton       = locale.T("Scan now", "Проверить сейчас")
	tripTurnOffButton    = locale.T("Turn off Trips", "Выключить поездки")
	tripTurnOnButton     = locale.T("Turn on Trips", "Включить поездки")
	tripUseTabButton     = locale.T("Use this tab for %s", "Использовать эту вкладку для: %s")
	tripsPageName        = locale.T("Trips", "Поездки")
	tripUsesTabLine      = locale.T("%q uses this tab.", "Эта вкладка используется для %q.")
	tripAlreadyUsesTab   = locale.T("%q already uses this tab.", "Эта вкладка уже используется для %q.")
	tripMovedNote        = locale.T("Moved %q to this tab: its summary is pinned here, and its reminders, notices, and news arrive here.", "Поездка %q перенесена в эту вкладку: её сводка закреплена здесь, а напоминания, уведомления и новости приходят сюда.")
	tripMovedLeftNote    = locale.T("Moved %q to this tab: its summary is pinned here, and its reminders, notices, and news arrive here. The tab it had stays with its messages; you can delete it.", "Поездка %q перенесена в эту вкладку: её сводка закреплена здесь, а напоминания, уведомления и новости приходят сюда. Прежняя вкладка остаётся со своими сообщениями; её можно удалить.")
	tripCardFromMail     = locale.T("Trip found in mail:", "Найдена поездка в почте:")
	tripCardFromCalendar = locale.T("Trip found in your calendar:", "Найдена поездка в вашем календаре:")
	tripCardTitleLine    = locale.T("%s (%s)", "%s (%s)")
	tripCardSpanLine     = locale.T("%s, times in %s", "%s, время указано в часовом поясе %s")
	tripCardMore         = locale.T("...and %d more", "...и ещё %d")
	tripCardBookingLine  = locale.T("- %s, %s", "- %s, %s")
	tripCardReference    = locale.T("%s (%s)", "%s (%s)")
	tripCardNoCheckIn    = locale.T("%s: no check-in time", "%s: время регистрации не указано")
	tripCardCheckIn      = locale.T("%s: check-in opens %s (%s)", "%s: регистрация открывается %s (%s)")
	tripCardPrompt       = locale.T("Track it for check-in reminders, or ignore it and I won't offer it again.", "Отслеживайте поездку, чтобы получать напоминания о регистрации, или проигнорируйте её, и я больше не буду её предлагать.")
	tripSpanText         = locale.T("%s to %s", "с %s по %s")
	tripTrackedHead      = locale.T("Tracking: %s (%s), %s.", "Отслеживается: %s (%s), %s.")
	tripNoReminder       = locale.T("No reminder for %s: %s.", "Для %s напоминания нет: %s.")
	tripReminder         = locale.T("Reminder for %s at %s.", "Напоминание для %s: %s.")
	tripTrackedTip       = locale.T("Tip: ask me to pause your home-bound reminders while you're away.", "Совет: попросите меня приостановить напоминания, связанные с домом, пока вы в отъезде.")
	tripCheckInStated    = locale.T("stated in the booking", "указано в бронировании")
	tripCheckInLookedUp  = locale.T("looked up on the provider's site", "найдено на сайте поставщика")
	tripCheckInAssumed   = locale.T("assumed, 24 h before the start", "предположительно, за 24 ч до начала")
	tripUnsetReasons     = map[string]locale.Text{
		trip.UnsetPast:     locale.T("check-in is already open", "регистрация уже открыта"),
		trip.UnsetCap:      locale.T("too many scheduled jobs", "слишком много заданий по расписанию"),
		trip.UnsetFar:      locale.T("more than a year ahead", "до начала больше года"),
		trip.UnsetAfterEnd: locale.T("check-in opens after the trip ends", "регистрация открывается после окончания поездки"),
		trip.UnsetInvalid:  locale.T("the time was refused", "время отклонено"),
	}

	TripGoneReply    = locale.T("This trip no longer exists.", "Этой поездки больше нет.")
	TripTrackedReply = locale.T("Already tracking this trip.", "Эта поездка уже отслеживается.")
	TripIgnoredReply = locale.T("Ignored; I won't offer it again.", "Пропущено; больше не предложу.")
	TripsOnLine      = locale.T("Trips: on. Each morning I look in your mail and calendar for travel bookings and offer each trip with Track and Ignore buttons; nothing is planned without your press. From three days before a tracked trip until it ends, I also check your mail, the web, and your calendar for important news about it about twice a day, between 08:00 and 21:00 where you are, and post what is new in the trip's tab. Press Scan now to look at once; it checks for trip news too. Uses your model subscription.", "Поездки: включены. Каждое утро я ищу в вашей почте и календаре бронирования поездок и предлагаю каждую поездку с кнопками «Следить» и «Игнорировать»; без вашего нажатия ничего не планируется. За три дня до отслеживаемой поездки и до её конца я также примерно дважды в день, с 08:00 до 21:00 по месту, где вы находитесь, проверяю почту, интернет и календарь на важные новости о ней и публикую новое во вкладке поездки. Нажмите «Проверить сейчас», чтобы посмотреть сразу; при этом проверяются и новости о поездках. Использует вашу подписку на модель.")
	TripsOffLine     = locale.T("Trips: off. Turn it on and I look in your mail and calendar for travel bookings at once and then each morning, and offer each trip with Track and Ignore buttons; nothing is planned without your press. While it is on, I also check tracked trips for important news from three days before they start until they end. Needs a connected model subscription, which it uses.", "Поездки: выключены. Включите, и я сразу поищу в вашей почте и календаре бронирования поездок, а затем буду делать это каждое утро и предлагать каждую поездку с кнопками «Следить» и «Игнорировать»; без вашего нажатия ничего не планируется. Пока функция включена, я также проверяю отслеживаемые поездки на важные новости, начиная с трёх дней до их начала и до конца. Нужна подключённая подписка на модель, которую эта функция использует.")
	// TripsNeedsGoogleLine explains why the switch is not offered.
	TripsNeedsGoogleLine = locale.T("Trips: off. It reads your mail and calendar, so turn on Mail or Calendar for a connected Google account in Accounts › Google first.", "Поездки: выключены. Они читают вашу почту и календарь, поэтому сначала включите «Почту» или «Календарь» для подключённого аккаунта Google в разделе «Аккаунты › Google».")
	TripsScanningNote    = locale.T("Scanning your mail and calendar now; any trips I find arrive here as cards, and news about trips near or under way arrives in their tabs.", "Просматриваю вашу почту и календарь; найденные поездки приходят сюда карточками, а новости о ближайших и идущих поездках приходят в их вкладки.")
	TripsBusyNote        = locale.T("A scan is already running; any trips it finds arrive here as cards.", "Просмотр уже идёт; найденные поездки придут сюда карточками.")
	// TripsTabLine stands in for the buttons that move a trip where the page's tab cannot be a
	// trip's.
	TripsTabLine     = locale.T("To move a planned trip to a tab of yours, send /settings in that tab and open this page there; a trip cannot use this one. Use a tab that already has a message, because /settings as the first message of a new tab is answered in the main view.", "Чтобы перенести запланированную поездку во вкладку, отправьте /settings в этой вкладке и откройте там эту страницу; эта вкладка для поездки не подойдёт. Используйте вкладку, в которой уже есть сообщение, потому что /settings как первое сообщение новой вкладки обрабатывается на главном экране.")
	TripsTabGoneNote = locale.T("That trip is over or no longer planned, so it has no tab to move.", "Эта поездка закончилась или больше не запланирована, поэтому её нельзя перенести во вкладку.")
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
// the service's Tabs implement it.
type TripFeeds interface {
	thread.Feeds
	// Summarize gives a planned trip its tab when it has none, and keeps its pinned summary and
	// the tab's name current.
	Summarize(ctx context.Context, principalID string, chatID int64, tripID string)
	// CheckTripTab says whether the trip, or with no ID a trip about to be added, could use the
	// tab; with named, only a tab the user named will do.
	CheckTripTab(ctx context.Context, principalID, tripID string, threadID int64, named bool) (trip.TabMove, error)
	// TakeTripTab makes the tab the trip's in place of the tab it had, or of the one it would get.
	TakeTripTab(ctx context.Context, principalID string, chatID int64, tripID string, threadID int64, named bool) (trip.TabMove, error)
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

var (
	_ trip.Tabs         = TripNotifier{}
	_ trip.Destinations = TripNotifier{}
)

func (n TripNotifier) SendTripCard(ctx context.Context, found trip.Trip) error {
	chatID, err := n.chat(ctx, found.PrincipalID)
	if err != nil {
		return err
	}
	ctx = n.inFeed(ctx, found.PrincipalID, chatID, "")
	keyboard := InlineKeyboard{{
		{Text: tripTrackButton.Format(ctx), CallbackData: tripPrefix + ":" + tripTrackAction + ":" + found.ID},
		{Text: tripIgnoreButton.Format(ctx), CallbackData: tripPrefix + ":" + tripIgnoreAction + ":" + found.ID},
	}}
	return n.Client.sendPlain(ctx, chatID, renderTripCard(ctx, found), keyboard)
}

func (n TripNotifier) Send(ctx context.Context, principalID, tripID, text string) error {
	chatID, err := n.chat(ctx, principalID)
	if err != nil {
		return err
	}
	return n.Client.sendPlain(n.inFeed(ctx, principalID, chatID, tripID), chatID, text, nil)
}

// TripTab is the tab Send puts the trip's messages in, zero for the main view.
func (n TripNotifier) TripTab(ctx context.Context, principalID, tripID string) (int64, error) {
	chatID, err := n.chat(ctx, principalID)
	if err != nil {
		return 0, err
	}
	return sendThread(n.inFeed(ctx, principalID, chatID, tripID)), nil
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

// CheckTab says whether the trip, or a trip about to be added, could use the tab; with tabs off or
// no tabs it cannot.
func (n TripNotifier) CheckTab(ctx context.Context, principalID, tripID string, threadID int64, named bool) (trip.TabMove, error) {
	if n.Feeds == nil {
		return trip.TabMove{Refusal: trip.TabsOffRefusal}, nil
	}
	return n.Feeds.CheckTripTab(ctx, principalID, tripID, threadID, named)
}

// TakeTab makes the tab the trip's, in the principal's active chat.
func (n TripNotifier) TakeTab(ctx context.Context, changed trip.Trip, threadID int64, named bool) (trip.TabMove, error) {
	if n.Feeds == nil {
		return trip.TabMove{Refusal: trip.TabsOffRefusal}, nil
	}
	chatID, err := n.chat(ctx, changed.PrincipalID)
	if err != nil {
		return trip.TabMove{}, err
	}
	return n.Feeds.TakeTripTab(ctx, changed.PrincipalID, chatID, changed.ID, threadID, named)
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

func tripCheckInSource(ctx context.Context, source string) string {
	switch source {
	case trip.CheckInStated:
		return tripCheckInStated.Format(ctx)
	case trip.CheckInLookedUp:
		return tripCheckInLookedUp.Format(ctx)
	default:
		return tripCheckInAssumed.Format(ctx)
	}
}

// tripSpan is the trip's first and last day. An end at local midnight is the start of the next
// day, so it reads as the previous day.
func tripSpan(ctx context.Context, found trip.Trip, location *time.Location) string {
	language := locale.From(ctx)
	end := found.EndsAt.In(location)
	if end.Hour() == 0 && end.Minute() == 0 && end.Second() == 0 && found.EndsAt.After(found.StartsAt) {
		end = end.AddDate(0, 0, -1)
	}
	return tripSpanText.Format(ctx, locale.Date(language, found.StartsAt.In(location), locale.DayYear), locale.Date(language, end, locale.DayYear))
}

func tripZone(found trip.Trip) *time.Location {
	location, err := schedule.LoadZone(found.Zone)
	if err != nil {
		return time.UTC
	}
	return location
}

// tripCardText is the card in English, for code that has no context.
func tripCardText(found trip.Trip) string { return renderTripCard(context.Background(), found) }

// renderTripCard is built from the validated fields of the trip only.
func renderTripCard(ctx context.Context, found trip.Trip) string {
	language := locale.From(ctx)
	location := tripZone(found)
	head := tripCardFromMail
	if found.Source == trip.SourceCalendar {
		head = tripCardFromCalendar
	}
	lines := []string{
		head.Format(ctx),
		tripCardTitleLine.Format(ctx, found.Title, found.Destination),
		tripCardSpanLine.Format(ctx, tripSpan(ctx, found, location), found.Zone),
		"",
	}
	for index, booking := range found.Bookings {
		if index == tripCardBookings {
			lines = append(lines, tripCardMore.Format(ctx, len(found.Bookings)-tripCardBookings))
			break
		}
		line := tripCardBookingLine.Format(ctx, booking.Title, locale.Date(language, booking.StartsAt.In(location), locale.DayTime))
		if booking.Reference != "" {
			line = tripCardReference.Format(ctx, line, booking.Reference)
		}
		if booking.CheckInAt.IsZero() {
			line = tripCardNoCheckIn.Format(ctx, line)
		} else {
			line = tripCardCheckIn.Format(ctx, line, locale.Date(language, booking.CheckInAt.In(location), locale.DayTime), tripCheckInSource(ctx, booking.CheckInSource))
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", tripCardPrompt.Format(ctx))
	text := strings.Join(lines, "\n")
	if runes := []rune(text); len(runes) > tripCardRunes {
		text = string(runes[:tripCardRunes-1]) + "…"
	}
	return text
}

func tripTrackedText(ctx context.Context, result trip.Result) string {
	language := locale.From(ctx)
	found := result.Trip
	location := tripZone(found)
	unset := make(map[string]string, len(result.Unset))
	for _, entry := range result.Unset {
		unset[entry.BookingID] = tripUnsetReasons[entry.Reason].Format(ctx)
	}
	lines := []string{tripTrackedHead.Format(ctx, found.Title, found.Destination, tripSpan(ctx, found, location))}
	for _, booking := range found.Bookings {
		if booking.CheckInAt.IsZero() {
			continue
		}
		if reason, skipped := unset[booking.ID]; skipped {
			lines = append(lines, tripNoReminder.Format(ctx, booking.Title, reason))
			continue
		}
		lines = append(lines, tripReminder.Format(ctx, booking.Title, locale.Date(language, booking.CheckInAt.In(location), locale.DayTime)))
	}
	lines = append(lines, tripTrackedTip.Format(ctx))
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
		text = tripTrackedText(ctx, outcome)
	case outcome.Rejection == "":
		text += "\n\n" + TripIgnoredReply.Format(ctx)
	default:
		known, found, err := s.trips.Trip(ctx, principalID, tripID)
		if err != nil {
			return processingResult{}, "trip_action", err
		}
		text += "\n\n" + staleTripReply(ctx, known, found)
	}
	if err := editPlain(ctx, s.client, callback.Message.Chat.ID, callback.Message.MessageID, text); err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

func staleTripReply(ctx context.Context, known trip.Trip, found bool) string {
	switch {
	case !found:
		return TripGoneReply.Format(ctx)
	case known.Status == trip.StatusPlanned:
		return TripTrackedReply.Format(ctx)
	case known.Status == trip.StatusDismissed:
		return TripIgnoredReply.Format(ctx)
	case known.Status == trip.StatusCancelled:
		return TripCancelledReply.Format(ctx)
	}
	return TripGoneReply.Format(ctx)
}

func editPlain(ctx context.Context, client botAPI, chatID, messageID int64, text string) error {
	if editor, ok := client.(plainEditor); ok {
		return editor.EditPlain(ctx, chatID, messageID, text)
	}
	return client.EditKeyboard(ctx, chatID, messageID, text, nil)
}

// tripIDPattern is the shape of the IDs the trip service gives its trips; a press with any other
// is not one of the page's buttons.
var tripIDPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

// TripTabController is what the Trips page needs to move a trip to the tab the page is opened in;
// the trip service implements it.
type TripTabController interface {
	// Trips returns the principal's suggested and planned trips that have not ended, earliest first.
	Trips(ctx context.Context, principalID string) ([]trip.Trip, error)
	CheckTab(ctx context.Context, principalID, tripID string, threadID int64, named bool) (trip.TabMove, error)
	UseTab(ctx context.Context, principalID, id string, threadID int64) (trip.Result, error)
}

// NewTripsSection is the page that turns the principal's Trips scan of Google mail and calendar on
// and off, and moves a planned trip to the tab the page is opened in; nil trips leaves the moves
// out.
func NewTripsSection(schedules ScheduleController, google GoogleController, trips TripTabController, logger *slog.Logger, telemetry Telemetry) Section {
	return tripsSection{schedules: schedules, google: google, trips: trips, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type tripsSection struct {
	schedules ScheduleController
	google    GoogleController
	trips     TripTabController
	logger    *slog.Logger
	telemetry Telemetry
}

func (t tripsSection) prefix() string { return tripsSectionPrefix }

func (t tripsSection) helpTopic() string { return "trips" }

func (t tripsSection) verb(action string) string {
	if strings.HasPrefix(action, tripsTabAction+":") {
		return tripsTabAction
	}
	return action
}

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
	return settingsSummary{name: tripsPageName.Format(ctx), state: stateOf(ctx, enabled)}, nil
}

func (t tripsSection) render(ctx context.Context, principalID string) (settingsView, error) {
	view, err := t.scanView(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	lines, rows, err := t.tabView(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	view.lines, view.rows = append(view.lines, lines...), rows
	return view, nil
}

func (t tripsSection) scanView(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := t.enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		return settingsView{
			lines: []string{TripsOnLine.Format(ctx)},
			buttons: []InlineButton{
				{Text: tripScanButton.Format(ctx), CallbackData: tripsSectionPrefix + ":" + tripsScanAction},
				{Text: tripTurnOffButton.Format(ctx), CallbackData: tripsSectionPrefix + ":" + tripsOffAction},
			},
		}, nil
	}
	readable, err := t.readable(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if !readable {
		return settingsView{lines: []string{TripsNeedsGoogleLine.Format(ctx)}}, nil
	}
	return settingsView{
		lines:   []string{TripsOffLine.Format(ctx)},
		buttons: []InlineButton{{Text: tripTurnOnButton.Format(ctx), CallbackData: tripsSectionPrefix + ":" + tripsOnAction}},
	}, nil
}

// plannedTrips is the principal's planned trips that have not ended, nearest first.
func (t tripsSection) plannedTrips(ctx context.Context, principalID string) ([]trip.Trip, error) {
	trips, err := t.trips.Trips(ctx, principalID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(trips, func(found trip.Trip) bool { return found.Status != trip.StatusPlanned }), nil
}

// tabView offers the tab the page is in to each of the nearest planned trips that could use it, by
// the check the trip tools make, and names the trip that already does. Where none can, one line
// says where to open the page instead; with tabs off there is nothing to say.
func (t tripsSection) tabView(ctx context.Context, principalID string) ([]string, [][]InlineButton, error) {
	if t.trips == nil {
		return nil, nil, nil
	}
	planned, err := t.plannedTrips(ctx, principalID)
	if err != nil {
		return nil, nil, err
	}
	threadID := thread.From(ctx)
	var lines []string
	var rows [][]InlineButton
	for _, found := range planned[:min(len(planned), tripTabButtons)] {
		move, err := t.trips.CheckTab(ctx, principalID, found.ID, threadID, false)
		switch {
		case err != nil:
			return nil, nil, err
		case move.Refusal == trip.TabsOffRefusal:
			return nil, nil, nil
		case move.Already:
			lines = append(lines, tripUsesTabLine.Format(ctx, found.Title))
		case move.Refusal == "":
			rows = append(rows, []InlineButton{{Text: tripUseTabButton.Format(ctx, found.Title), CallbackData: tripsSectionPrefix + ":" + tripsTabAction + ":" + found.ID}})
		}
	}
	if len(planned) > 0 && len(lines) == 0 && len(rows) == 0 {
		lines = []string{TripsTabLine.Format(ctx)}
	}
	return lines, rows, nil
}

// useTab moves the principal's planned trip to the tab the press came from, under the checks the
// trip tools make, and says what happened or why not.
func (t tripsSection) useTab(ctx context.Context, principalID, tripID string) (settingsOutcome, error) {
	planned, err := t.plannedTrips(ctx, principalID)
	if err != nil {
		return settingsOutcome{}, err
	}
	isPlanned := func(planned []trip.Trip) bool {
		return slices.ContainsFunc(planned, func(found trip.Trip) bool { return found.ID == tripID })
	}
	if !isPlanned(planned) {
		return settingsOutcome{note: TripsTabGoneNote.Format(ctx)}, nil
	}
	threadID := thread.From(ctx)
	if threadID <= 0 {
		return settingsOutcome{note: TripsTabLine.Format(ctx)}, nil
	}
	result, err := t.trips.UseTab(ctx, principalID, tripID, threadID)
	switch {
	case err != nil:
		return settingsOutcome{}, err
	case result.Rejection != "":
		// A trip that stopped being planned since the check is refused in words meant for the
		// model; only a refusal of the tab is the user's to read.
		if planned, err = t.plannedTrips(ctx, principalID); err != nil {
			return settingsOutcome{}, err
		}
		if !isPlanned(planned) {
			return settingsOutcome{note: TripsTabGoneNote.Format(ctx)}, nil
		}
		return settingsOutcome{note: result.Rejection}, nil
	case result.Move != nil && result.Move.Already:
		return settingsOutcome{note: tripAlreadyUsesTab.Format(ctx, result.Trip.Title)}, nil
	}
	t.telemetry.ContextLogger(ctx, t.logger).InfoContext(ctx, "trips.tab.moved", "principal_id", principalID, "trip_id", tripID, "thread_id", threadID)
	note := tripMovedNote.Format(ctx, result.Trip.Title)
	if result.Move != nil && result.Move.Left {
		note = tripMovedLeftNote.Format(ctx, result.Trip.Title)
	}
	return settingsOutcome{note: note}, nil
}

func (t tripsSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	if tripID, isTab := strings.CutPrefix(action, tripsTabAction+":"); isTab {
		if t.trips == nil || !tripIDPattern.MatchString(tripID) {
			return settingsOutcome{}, errUnknownSettingsAction
		}
		return t.useTab(ctx, principalID, tripID)
	}
	id := schedule.TripsJobID(principalID)
	switch action {
	case tripsOnAction:
		readable, err := t.readable(ctx, principalID)
		if err != nil {
			return settingsOutcome{}, err
		}
		if !readable {
			return settingsOutcome{note: TripsNeedsGoogleLine.Format(ctx)}, nil
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
				return settingsOutcome{note: TripsTooManyNote.Format(ctx)}, nil
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
		return settingsOutcome{note: TripsBusyNote.Format(ctx)}, nil
	}
	queued, err := t.schedules.RunNow(ctx, principalID, id, pressTriggerFrom(ctx))
	if err != nil || !queued {
		return settingsOutcome{}, err
	}
	t.telemetry.ContextLogger(ctx, t.logger).InfoContext(ctx, "trips.scan.queued", "principal_id", principalID)
	return settingsOutcome{note: TripsScanningNote.Format(ctx)}, nil
}

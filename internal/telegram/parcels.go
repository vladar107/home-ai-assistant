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
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	parcelPrefix     = "parcel"
	parcelStopAction = "stop"

	// parcelDataVersion is the version of the data a parcel widget carries. A view reads what it
	// knows and ignores the rest, and a field it does not find is left out, so cards sent by an older
	// version keep working and older views keep drawing newer data.
	parcelDataVersion = 1

	maximumParcelTitleRunes  = 60
	maximumParcelSenderRunes = 40
	maximumParcelPickupRunes = 60
)

var (
	parcelStopButton = locale.T("Stop tracking", "Прекратить отслеживание")
	parcelOpenButton = locale.T("Open at %s", "Открыть на сайте: %s")
	parcelFromLabel  = locale.T("From:", "От:")
	parcelWhenLabel  = locale.T("Expected:", "Ожидается:")
	parcelPickup     = locale.T("Pickup point:", "Пункт выдачи:")

	parcelStatusRegistered     = locale.T("Registered", "Зарегистрирована")
	parcelStatusInTransit      = locale.T("On its way", "В пути")
	parcelStatusOutForDelivery = locale.T("Out for delivery", "Доставляется")
	parcelStatusAtPickupPoint  = locale.T("Ready for pickup", "Ожидает в пункте выдачи")
	parcelStatusDelivered      = locale.T("Delivered", "Доставлена")
	parcelStatusProblem        = locale.T("Problem with delivery", "Проблема с доставкой")
	parcelStatusUnknown        = locale.T("Status unknown", "Статус неизвестен")
	parcelStatusStopped        = locale.T("Stopped", "Отслеживание остановлено")
	parcelStatusTrackingOff    = locale.T("Tracking off", "Отслеживание выключено")
	parcelStatusIdle           = locale.T("No longer followed: no news for 30 days", "Больше не отслеживается: 30 дней без новостей")

	parcelNoticeOutForDelivery = locale.T("📦 Out for delivery: %s", "📦 Доставляется: %s")
	parcelNoticeAtPickup       = locale.T("📦 Ready for pickup: %s", "📦 Ожидает в пункте выдачи: %s")
	parcelNoticeAtPickupPoint  = locale.T("📦 Ready for pickup at %[2]s: %[1]s", "📦 Ожидает в пункте выдачи %[2]s: %[1]s")
	parcelNoticeDelivered      = locale.T("📦 Delivered: %s", "📦 Доставлена: %s")
	parcelNoticeProblem        = locale.T("📦 Problem with delivery: %s", "📦 Проблема с доставкой: %s")
	parcelParcelOf             = locale.T("%s parcel", "Посылка %s")
	parcelTrackingCount        = locale.N("Tracking %[1]d parcel", "Tracking %[1]d parcels", "Слежу за %[1]d посылкой", "Слежу за %[1]d посылками", "Слежу за %[1]d посылками")
)

// parcelStatusWords are the words for a parcel's status.
var parcelStatusWords = map[parcel.Status]locale.Text{
	parcel.StatusRegistered:     parcelStatusRegistered,
	parcel.StatusInTransit:      parcelStatusInTransit,
	parcel.StatusOutForDelivery: parcelStatusOutForDelivery,
	parcel.StatusAtPickupPoint:  parcelStatusAtPickupPoint,
	parcel.StatusDelivered:      parcelStatusDelivered,
	parcel.StatusProblem:        parcelStatusProblem,
	parcel.StatusUnknown:        parcelStatusUnknown,
}

// parcelLabels are the words the parcel view draws, in a language.
func parcelLabels(language locale.Language) map[string]any {
	statuses := make(map[string]string, len(parcelStatusWords))
	for status, word := range parcelStatusWords {
		statuses[string(status)] = word.In(language)
	}
	return map[string]any{
		"statuses": statuses,
		"from":     parcelFromLabel.In(language),
		"expected": parcelWhenLabel.In(language),
		"pickup":   parcelPickup.In(language),
		"stop":     parcelStopButton.In(language),
		"stopped":  parcelStatusStopped.In(language),
		"off":      parcelStatusTrackingOff.In(language),
		"idle":     parcelStatusIdle.In(language),
	}
}

// parcelData is what a parcel widget shows. Its keys are one letter each to keep the data within
// Telegram's kilobyte, and its texts are in the user's language where they are worded, since the
// view's own labels travel in the template. Parcel widgets already in chats read this shape, so it
// only grows.
//
//	v  the version of the shape
//	i  the parcel's ID, which the Stop tracking tap names
//	c  the carrier's name
//	t  the title, at most 60 characters
//	f  the sender, at most 40 characters
//	s  the status, as parcel.Status
//	w  the expected day and window, worded
//	p  the pickup point, at most 60 characters
//	m  x once tracking was stopped, o once Parcels was turned off, i once no source had news of it
//	   for 30 days
//	g  the language the widget draws its labels in, none for English
type parcelData struct {
	Version int    `json:"v"`
	ID      string `json:"i"`
	Carrier string `json:"c,omitempty"`
	Title   string `json:"t,omitempty"`
	Sender  string `json:"f,omitempty"`
	Status  string `json:"s,omitempty"`
	Window  string `json:"w,omitempty"`
	Pickup  string `json:"p,omitempty"`
	Mode    string `json:"m,omitempty"`
	Lang    string `json:"g,omitempty"`
}

const (
	parcelModeStopped     = "x"
	parcelModeTrackingOff = "o"
	parcelModeIdle        = "i"
)

// ParcelChats resolves the principal's active chat.
type ParcelChats = TripChats

// ParcelNotifier shows parcels as cards, one message each, in the principal's Parcels tab, and
// follows their statuses with short notices. A card is a widget while the principal has widgets
// on, and text otherwise. Everything it shows from a carrier, sender, or shop is plain text.
type ParcelNotifier struct {
	Client *Client
	Chats  ParcelChats
	// Feeds puts the messages in the Parcels tab while the bot has tabs on; nil sends them under
	// the caller's context.
	Feeds thread.Feeds
	// Widgets is the principal's Widgets switch; nil sends text cards.
	Widgets interface {
		Enabled(context.Context, string) (bool, error)
	}
	// Languages gives each principal's language; nil writes English.
	Languages locale.Preferences
	Carriers  parcel.Carriers
	// Location is the zone the expected day is worded in.
	Location *time.Location
	Logger   *slog.Logger
}

var _ parcel.Cards = ParcelNotifier{}

// SendCard sends the parcel's card to the Parcels tab and returns its message and kind.
func (n ParcelNotifier) SendCard(ctx context.Context, found parcel.Parcel, silent bool) (int64, parcel.CardKind, error) {
	ctx, chatID, err := n.prepare(ctx, found.PrincipalID, true)
	if err != nil {
		return 0, "", err
	}
	if n.widgetsOn(ctx, found.PrincipalID) {
		message, err := n.Client.sendWidgetWith(ctx, chatID, parcelWidget, n.widgetData(ctx, found, parcel.CardCurrent), n.caption(ctx, found, parcel.CardCurrent), silent)
		switch {
		case err == nil && message.MessageID > 0:
			return message.MessageID, parcel.CardWidget, nil
		case err == nil:
			return 0, "", errors.New("Telegram sendWidget returned no message ID")
		case !isPermanentAPIError(err):
			return 0, "", err
		}
	}
	message, err := n.Client.sendPlainCard(ctx, chatID, 0, n.cardText(ctx, found, parcel.CardCurrent), n.keyboard(ctx, found, parcel.CardCurrent), silent)
	if err != nil {
		return 0, "", err
	}
	if message.MessageID <= 0 {
		return 0, "", errors.New("Telegram sendMessage returned no message ID")
	}
	return message.MessageID, parcel.CardText, nil
}

// EditCard shows the parcel on its stored card, in place. It returns parcel.ErrCardGone when the
// parcel has no card or Telegram no longer has its message or tab.
func (n ParcelNotifier) EditCard(ctx context.Context, found parcel.Parcel, mode parcel.CardMode) error {
	if found.CardMessage <= 0 {
		return parcel.ErrCardGone
	}
	ctx, chatID, err := n.prepare(ctx, found.PrincipalID, false)
	if err != nil {
		return err
	}
	switch found.CardKind {
	case parcel.CardWidget:
		_, err = n.Client.editWidget(ctx, chatID, found.CardMessage, n.caption(ctx, found, mode), n.widgetData(ctx, found, mode))
	default:
		err = n.Client.editPlainCard(ctx, chatID, found.CardMessage, n.cardText(ctx, found, mode), n.keyboard(ctx, found, mode))
	}
	if isMessageGone(err) {
		return parcel.ErrCardGone
	}
	return err
}

// SendNotice replies to the parcel's card with the status's notice. A status without one sends
// nothing.
func (n ParcelNotifier) SendNotice(ctx context.Context, found parcel.Parcel, silent bool) error {
	ctx, chatID, err := n.prepare(ctx, found.PrincipalID, true)
	if err != nil {
		return err
	}
	text, ok := n.noticeText(ctx, found)
	if !ok {
		return nil
	}
	_, err = n.Client.sendPlainCard(ctx, chatID, found.CardMessage, text, nil, silent)
	return err
}

// SendSummary posts the line that follows a silent baseline, without a notification.
func (n ParcelNotifier) SendSummary(ctx context.Context, principalID string, count int) error {
	ctx, chatID, err := n.prepare(ctx, principalID, true)
	if err != nil {
		return err
	}
	_, err = n.Client.sendPlainCard(ctx, chatID, 0, parcelTrackingCount.Format(ctx, count), nil, true)
	return err
}

// prepare finds the principal's chat and puts the context in their language and, when the message
// is new, in their Parcels tab.
func (n ParcelNotifier) prepare(ctx context.Context, principalID string, routed bool) (context.Context, int64, error) {
	chatID, found, err := n.Chats.ResolveActiveChat(ctx, principalID)
	if err != nil {
		return ctx, 0, err
	}
	if !found {
		return ctx, 0, errors.New("the principal has no active chat")
	}
	if withLanguage, err := locale.ForPrincipal(ctx, n.Languages, principalID); err == nil {
		ctx = withLanguage
	} else if n.Logger != nil {
		n.Logger.WarnContext(ctx, "telegram.language.load_failed", "principal_id", principalID, "error", err)
	}
	if routed && n.Feeds != nil {
		ctx = thread.WithRoute(ctx, n.Feeds.Route(ctx, principalID, chatID, "", thread.FeedParcels))
	}
	return ctx, chatID, nil
}

func (n ParcelNotifier) widgetsOn(ctx context.Context, principalID string) bool {
	if n.Widgets == nil {
		return false
	}
	on, err := n.Widgets.Enabled(ctx, principalID)
	return err == nil && on
}

func (n ParcelNotifier) location() *time.Location {
	if n.Location == nil {
		return time.UTC
	}
	return n.Location
}

// carrierName is the carrier's name from the list, or the word the parcel's carrier is known by.
func (n ParcelNotifier) carrierName(found parcel.Parcel) string {
	if carrier, known := n.Carriers.ByID(found.Carrier); known && carrier.Name != "" {
		return carrier.Name
	}
	word := found.Carrier
	if word == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(word)
	return string(unicode.ToUpper(first)) + word[size:]
}

// parcelText is a carrier's, sender's, or shop's text as one short plain line.
func parcelText(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(text, ""))
	return clip.Runes(strings.Join(strings.Fields(text), " "), limit)
}

// statusWord is what the card says about the parcel in a mode.
func statusWord(ctx context.Context, found parcel.Parcel, mode parcel.CardMode) string {
	switch {
	case mode == parcel.CardStopped || (found.Stopped() && mode == parcel.CardCurrent):
		return parcelStatusStopped.Format(ctx)
	case mode == parcel.CardTrackingOff:
		return parcelStatusTrackingOff.Format(ctx)
	case mode == parcel.CardIdle:
		return parcelStatusIdle.Format(ctx)
	}
	if word, known := parcelStatusWords[found.Status]; known {
		return word.Format(ctx)
	}
	return parcelStatusUnknown.Format(ctx)
}

// closed reports whether the card shows a final state, with nothing left to stop.
func closed(found parcel.Parcel, mode parcel.CardMode) bool {
	return mode != parcel.CardCurrent || found.Stopped() || found.Status == parcel.StatusDelivered
}

// expected words the day and window the parcel is due, or "" when there is none.
func (n ParcelNotifier) expected(language locale.Language, found parcel.Parcel) string {
	if found.WindowFrom.IsZero() {
		return ""
	}
	zone := n.location()
	from := found.WindowFrom.In(zone)
	day := locale.Date(language, from, locale.Day)
	to := found.WindowTo.In(zone)
	hasTimes := from.Hour() != 0 || from.Minute() != 0
	switch {
	case found.WindowTo.IsZero() || !to.After(from):
		if hasTimes {
			return day + ", " + from.Format("15:04")
		}
	case from.YearDay() == to.YearDay() && from.Year() == to.Year():
		if hasTimes {
			return day + ", " + from.Format("15:04") + "–" + to.Format("15:04")
		}
	default:
		return day + " – " + locale.Date(language, to, locale.Day)
	}
	return day
}

func (n ParcelNotifier) widgetData(ctx context.Context, found parcel.Parcel, mode parcel.CardMode) string {
	language := locale.From(ctx)
	data := parcelData{
		Version: parcelDataVersion, ID: found.ID, Carrier: parcelText(n.carrierName(found), 32),
		Title: parcelText(found.Title, maximumParcelTitleRunes), Sender: parcelText(found.Sender, maximumParcelSenderRunes),
		Status: string(found.Status), Lang: widgetLanguage(language),
	}
	switch mode {
	case parcel.CardStopped:
		data.Mode = parcelModeStopped
	case parcel.CardTrackingOff:
		data.Mode = parcelModeTrackingOff
	case parcel.CardIdle:
		data.Mode = parcelModeIdle
	default:
		if found.Stopped() {
			data.Mode = parcelModeStopped
		}
	}
	if data.Mode == "" {
		data.Window = n.expected(language, found)
		data.Pickup = parcelText(found.PickupPoint, maximumParcelPickupRunes)
	}
	return marshalWidgetData(data)
}

// caption is the line under a widget: the carrier, linking to its page when the parcel has a link,
// and the state. It carries nothing from outside, so it stays short and keeps its link.
func (n ParcelNotifier) caption(ctx context.Context, found parcel.Parcel, mode parcel.CardMode) string {
	name := parcelText(n.carrierName(found), 32)
	if link := captionLink(found.Link); link != "" {
		name = "[" + strings.NewReplacer("[", "", "]", "").Replace(name) + "](" + link + ")"
	}
	return "📦 " + name + " · " + statusWord(ctx, found, mode)
}

// captionLink is the link as a caption's markup can carry it, or "" when it cannot.
func captionLink(link string) string {
	parsed, err := url.Parse(link)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || strings.IndexFunc(link, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return ""
	}
	return strings.NewReplacer("(", "%28", ")", "%29").Replace(link)
}

// cardText is the text card: plain lines, with nothing from outside read as markup.
func (n ParcelNotifier) cardText(ctx context.Context, found parcel.Parcel, mode parcel.CardMode) string {
	lines := []string{"📦 " + n.carrierName(found)}
	if title := parcelText(found.Title, maximumParcelTitleRunes); title != "" {
		lines = append(lines, title)
	}
	if sender := parcelText(found.Sender, maximumParcelSenderRunes); sender != "" {
		lines = append(lines, parcelFromLabel.Format(ctx)+" "+sender)
	}
	lines = append(lines, "", statusWord(ctx, found, mode))
	if mode == parcel.CardCurrent && !found.Stopped() {
		if !closed(found, mode) {
			if when := n.expected(locale.From(ctx), found); when != "" {
				lines = append(lines, parcelWhenLabel.Format(ctx)+" "+when)
			}
		}
		if point := parcelText(found.PickupPoint, maximumParcelPickupRunes); point != "" {
			lines = append(lines, parcelPickup.Format(ctx)+" "+point)
		}
	}
	return strings.Join(lines, "\n")
}

// keyboard is the text card's buttons: the carrier's page, and Stop tracking while there is
// something to stop.
func (n ParcelNotifier) keyboard(ctx context.Context, found parcel.Parcel, mode parcel.CardMode) InlineKeyboard {
	var row []InlineButton
	if link := found.Link; link != "" && mode == parcel.CardCurrent && !found.Stopped() {
		if parsed, err := url.Parse(link); err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" {
			row = append(row, InlineButton{Text: parcelOpenButton.Format(ctx, parcelText(n.carrierName(found), 32)), URL: link})
		}
	}
	if !closed(found, mode) {
		row = append(row, InlineButton{Text: parcelStopButton.Format(ctx), CallbackData: parcelPrefix + ":" + parcelStopAction + ":" + found.ID})
	}
	if len(row) == 0 {
		return nil
	}
	return InlineKeyboard{row}
}

// noticeText is the notice for the parcel's status, and false for a status that has none.
func (n ParcelNotifier) noticeText(ctx context.Context, found parcel.Parcel) (string, bool) {
	subject := parcelText(found.Title, maximumParcelTitleRunes)
	if subject == "" {
		subject = parcelParcelOf.Format(ctx, n.carrierName(found))
	}
	switch found.Status {
	case parcel.StatusOutForDelivery:
		return parcelNoticeOutForDelivery.Format(ctx, subject), true
	case parcel.StatusAtPickupPoint:
		if point := parcelText(found.PickupPoint, maximumParcelPickupRunes); point != "" {
			return parcelNoticeAtPickupPoint.Format(ctx, subject, point), true
		}
		return parcelNoticeAtPickup.Format(ctx, subject), true
	case parcel.StatusDelivered:
		return parcelNoticeDelivered.Format(ctx, subject), true
	case parcel.StatusProblem:
		return parcelNoticeProblem.Format(ctx, subject), true
	}
	return "", false
}

func isMessageGone(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.MessageGone()
}

// sendPlainCard posts plain text, without link previews, in the context's tab, as a reply to
// replyTo when it is set, without a notification when silent.
func (c *Client) sendPlainCard(ctx context.Context, chatID, replyTo int64, text string, keyboard InlineKeyboard, silent bool) (Message, error) {
	params := url.Values{"chat_id": {strconv.FormatInt(chatID, 10)}}
	if replyTo > 0 {
		params.Set("reply_parameters", fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, replyTo))
	}
	if silent {
		params.Set("disable_notification", "true")
	}
	if err := setKeyboard(params, keyboard); err != nil {
		return Message{}, err
	}
	setPlainText(params, text)
	var message Message
	err := c.inTab(ctx, chatID, true, func(ctx context.Context) error {
		params.Del("message_thread_id")
		setThread(ctx, params)
		var err error
		message, err = execute[Message](c, ctx, "sendMessage", params)
		return err
	})
	return message, err
}

// editPlainCard replaces a bot message's text and buttons, with no markup read from the text. An
// edit that changes nothing is a success.
func (c *Client) editPlainCard(ctx context.Context, chatID, messageID int64, text string, keyboard InlineKeyboard) error {
	params := url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
	}
	if err := setKeyboard(params, keyboard); err != nil {
		return err
	}
	setPlainText(params, text)
	_, err := execute[json.RawMessage](c, ctx, "editMessageText", params)
	if isNotModified(err) {
		return nil
	}
	return err
}

// ParcelTaps is what Stop tracking taps need, from the widget and from the text card's button. The
// tracker does the stopping and the edit of the card; a tap only authorizes it.
type ParcelTaps struct {
	Parcels parcel.Repository
	Tracker parcel.Tracker
}

// parseParcelCallback accepts only the fixed vocabulary of parcel taps.
func parseParcelCallback(data string) (action, parcelID string, ok bool) {
	if len(data) > maximumCallbackDataBytes {
		return "", "", false
	}
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != parcelPrefix || parts[1] != parcelStopAction || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// processParcelPress stops tracking from the text card's button.
func (s *Service) processParcelPress(ctx context.Context, callback *CallbackQuery, principalID, parcelID string) (processingResult, string, error) {
	tap := press{chatID: callback.Message.Chat.ID, messageID: callback.Message.MessageID, failure: "answer_callback"}
	return s.stopParcelFromTap(ctx, tap, principalID, parcelID, func(context.Context) error { return nil })
}

// processParcelWidgetTap stops tracking from the widget's control. The tap is answered before the
// parcel is stopped, so the answer never waits on Home Assistant.
func (s *Service) processParcelWidgetTap(ctx context.Context, _ Bot, _ int64, tap press, principalID, data string) (bool, processingResult, string, error) {
	_, parcelID, ok := parseParcelCallback(data)
	if !ok || s.parcels == nil {
		return false, processingResult{}, "", nil
	}
	result, failure, err := s.stopParcelFromTap(ctx, tap, principalID, parcelID, func(ctx context.Context) error {
		return tap.answer(ctx, WidgetWorkingAnswer.Format(ctx))
	})
	return true, result, failure, err
}

// stopParcelFromTap stops the principal's parcel when the tap comes from the message that shows its
// card, and edits the card to Stopped. answer answers the tap once the parcel is known to be the
// principal's; a tap for another parcel is answered as doing nothing.
func (s *Service) stopParcelFromTap(ctx context.Context, tap press, principalID, parcelID string, answer func(context.Context) error) (processingResult, string, error) {
	found, exists, err := s.parcels.Parcels.LoadParcel(ctx, principalID, parcelID)
	if err != nil {
		return processingResult{}, "parcel_load", err
	}
	if !exists || found.CardMessage != tap.messageID {
		if tap.widget {
			return s.ignoreWidgetTap(ctx, tap, principalID, "foreign_parcel")
		}
		return processingResult{event: "telegram.update.ignored", reason: "foreign_parcel", principalID: principalID}, "", nil
	}
	if err := answer(ctx); err != nil && !isPermanentAPIError(err) {
		return processingResult{}, tap.failure, err
	}
	// A failed stop is not retried through the update, which would hold every later one back; the
	// tracker repairs the card on its next pass.
	if _, _, err := s.parcels.Tracker.Stop(ctx, principalID, parcelID); err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.parcel.stop_failed", "principal_id", principalID, "parcel_id", parcelID, "error", err)
	}
	return processingResult{event: "telegram.callback.handled", principalID: principalID, command: "parcel." + parcelStopAction}, "", nil
}

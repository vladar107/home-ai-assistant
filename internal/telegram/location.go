package telegram

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/location"
)

const (
	locationSectionPrefix = "location"
	locationForgetAction  = "forget"

	// indefiniteLivePeriod is Telegram's live_period for a share that runs until the user stops it.
	indefiniteLivePeriod = 0x7FFFFFFF

	maximumVenueTitleRunes   = 200
	maximumVenueAddressRunes = 300
)

var (
	LocationDisabledReply = locale.T("Location sharing is not enabled on this assistant.", "Передача местоположения на этом ассистенте не включена.")
	LocationRefusedReply  = locale.T("I could not read that location, so I did not keep it.", "Не удалось прочитать это местоположение, поэтому оно не сохранено.")
	LocationSharedLine    = locale.T("Your latest shared location is used in your conversation until it expires; only the latest one is kept, and nothing is kept once it expires or you forget it. Places you save by name are kept until you delete them, and a day more so you can undo it, and are only ever saved when you ask.", "Твоё последнее переданное местоположение используется в твоём разговоре, пока не истечёт срок; хранится только последнее, и ничего не хранится после истечения срока или после того, как ты его забудешь. Места, которые ты сохраняешь по названию, хранятся, пока ты их не удалишь, и ещё сутки, чтобы это можно было отменить, и сохраняются только по твоей просьбе.")
	LocationNotSharedLine = locale.T("Share a location or a live location in this chat to ask about places, buses, and weather near you.", "Передай местоположение или трансляцию местоположения в этом чате, чтобы спрашивать о местах, автобусах и погоде рядом с тобой.")

	locationName          = locale.T("Location", "Местоположение")
	locationSharedState   = locale.T("shared", "передано")
	locationNotShared     = locale.T("not shared", "не передано")
	locationNotSharedHdr  = locale.T("Location: not shared.", "Местоположение: не передано.")
	locationSummaryLine   = locale.T("Location: %s.", "Местоположение: %s.")
	locationForgetButton  = locale.T("Forget my location", "Забыть моё местоположение")
	LocationForgottenNote = locale.T("Location forgotten.", "Местоположение забыто.")
	// LocationPrivacyLine is added to /privacy while location sharing is enabled.
	LocationPrivacyLine = locale.T("Location: only the latest is kept, until it expires; it goes to your model, Transitous, and OpenStreetMap. Places you save by name are kept until a day after you delete them, never saved from a location without your asking.", "Местоположение: хранится только последнее, пока не истечёт срок; оно передаётся твоей модели, Transitous и OpenStreetMap. Места, которые ты сохраняешь по названию, хранятся до дня после их удаления и никогда не сохраняются по местоположению без твоей просьбы.")
)

// LocationController is implemented by the location service; nil turns shared locations away.
type LocationController interface {
	Share(context.Context, location.Fix) (string, error)
	Move(context.Context, location.Fix) (bool, error)
	Summary(context.Context, string) (string, bool, error)
	Forget(context.Context, string) (bool, error)
}

// processLocation stores a newly shared pin, venue, or live location and acknowledges it without
// a model call; the question about it comes in a later message.
func (s *Service) processLocation(ctx context.Context, message *Message, principalID string) (processingResult, string, error) {
	if s.locations == nil {
		return s.sendLocationReply(ctx, message, principalID, "disabled", LocationDisabledReply.Format(ctx))
	}
	fix := locationFix(principalID, message, false)
	if err := fix.Validate(); err != nil {
		return s.sendLocationReply(ctx, message, principalID, "invalid", LocationRefusedReply.Format(ctx))
	}
	ack, err := s.locations.Share(ctx, fix)
	if err != nil {
		return processingResult{}, "share_location", err
	}
	return s.sendLocationReply(ctx, message, principalID, string(fix.Source), ack)
}

// processEdit moves a live location silently; every other edit is ignored.
func (s *Service) processEdit(ctx context.Context, message *Message) (processingResult, string, error) {
	if message.Location == nil || message.Venue != nil || s.locations == nil {
		return ignoredResult("edited_message"), "", nil
	}
	if reason := validatePrivateMessage(message.From, message.Chat, message.MessageID); reason != "" {
		return ignoredResult(reason), "", nil
	}
	authorization, reason, failure, err := s.resolvePrincipal(ctx, message.From)
	if err != nil {
		return processingResult{}, failure, err
	}
	if reason != "" {
		return ignoredResult(reason), "", nil
	}
	principalID := authorization.Principal.ID
	fix := locationFix(principalID, message, true)
	if fix.Validate() != nil {
		return processingResult{event: "telegram.update.ignored", reason: "invalid_location", principalID: principalID}, "", nil
	}
	moved, err := s.locations.Move(ctx, fix)
	if err != nil {
		return processingResult{}, "move_location", err
	}
	if !moved {
		return processingResult{event: "telegram.update.ignored", reason: "stale_location", principalID: principalID}, "", nil
	}
	return processingResult{event: "telegram.location.moved", principalID: principalID}, "", nil
}

func (s *Service) sendLocationReply(ctx context.Context, message *Message, principalID, reason, reply string) (processingResult, string, error) {
	result := processingResult{event: "telegram.location.shared", reason: reason, principalID: principalID}
	if reason == "disabled" || reason == "invalid" {
		result.event = "telegram.location.refused"
	}
	if err := s.client.SendReply(ctx, message.Chat.ID, message.MessageID, reply); err != nil {
		if isPermanentAPIError(err) {
			result.event, result.err = "telegram.command.undeliverable", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

// locationFix reads a message's position. An edit of a live location without live_period is the
// one Telegram sends when the user stops sharing.
func locationFix(principalID string, message *Message, edited bool) location.Fix {
	sent := time.Unix(message.Date, 0).UTC()
	at := sent
	if edited && message.EditDate > 0 {
		at = time.Unix(message.EditDate, 0).UTC()
	}
	fix := location.Fix{PrincipalID: principalID, UpdatedAt: at}
	if venue := message.Venue; venue != nil {
		fix.Source = location.SourceVenue
		fix.Latitude, fix.Longitude = venue.Location.Latitude, venue.Location.Longitude
		fix.Accuracy = venue.Location.HorizontalAccuracy
		fix.VenueTitle = clipText(venue.Title, maximumVenueTitleRunes)
		fix.VenueAddress = clipText(venue.Address, maximumVenueAddressRunes)
		return fix
	}
	point := message.Location
	fix.Latitude, fix.Longitude, fix.Accuracy = point.Latitude, point.Longitude, point.HorizontalAccuracy
	switch {
	case point.LivePeriod > 0:
		fix.Source = location.SourceLive
		fix.LiveMessageID = message.MessageID
		if point.LivePeriod != indefiniteLivePeriod {
			fix.LiveUntil = sent.Add(time.Duration(point.LivePeriod) * time.Second)
		}
	case edited:
		fix.Source = location.SourceLive
		fix.LiveMessageID = message.MessageID
		fix.EndedAt = at
	default:
		fix.Source = location.SourcePin
	}
	return fix
}

func clipText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "")
	}
	return clip.Runes(text, limit)
}

// NewLocationSection is the page that shows and forgets the principal's shared location.
func NewLocationSection(locations LocationController, logger *slog.Logger, telemetry Telemetry) Section {
	return locationSection{locations: locations, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type locationSection struct {
	locations LocationController
	logger    *slog.Logger
	telemetry Telemetry
}

func (l locationSection) prefix() string { return locationSectionPrefix }

func (l locationSection) helpTopic() string { return "location" }

func (l locationSection) privacy(ctx context.Context, _ string) ([]string, error) {
	return []string{LocationPrivacyLine.Format(ctx)}, nil
}

func (l locationSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	_, shared, err := l.locations.Summary(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	state := locationNotShared.Format(ctx)
	if shared {
		state = locationSharedState.Format(ctx)
	}
	return settingsSummary{name: locationName.Format(ctx), state: state}, nil
}

func (l locationSection) render(ctx context.Context, principalID string) (settingsView, error) {
	text, shared, err := l.locations.Summary(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if !shared {
		return settingsView{lines: []string{locationNotSharedHdr.Format(ctx), LocationNotSharedLine.Format(ctx)}}, nil
	}
	return settingsView{
		lines:   []string{locationSummaryLine.Format(ctx, text), LocationSharedLine.Format(ctx)},
		buttons: []InlineButton{{Text: locationForgetButton.Format(ctx), CallbackData: locationSectionPrefix + ":" + locationForgetAction}},
	}, nil
}

func (l locationSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	if action != locationForgetAction {
		return settingsOutcome{}, errUnknownSettingsAction
	}
	forgotten, err := l.locations.Forget(ctx, principalID)
	if err != nil {
		return settingsOutcome{}, err
	}
	l.telemetry.ContextLogger(ctx, l.logger).InfoContext(ctx, "location.forgotten", "principal_id", principalID, "existed", forgotten)
	return settingsOutcome{note: LocationForgottenNote.Format(ctx)}, nil
}

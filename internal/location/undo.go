package location

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

// placeUndo reverses a direct place action: UpdatedAt is when a save or an update left the place,
// Name and RadiusM what an update replaced, and DeletedAt when a deletion hid the place. It never
// holds a point, since approvals keep it for good.
type placeUndo struct {
	PlaceID   string    `json:"place_id"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Name      string    `json:"name,omitempty"`
	RadiusM   int       `json:"radius_m,omitempty"`
	DeletedAt time.Time `json:"deleted_at,omitzero"`
}

// placeUndoPayload is the payload that reverses a save, or an update of before, read back as stored.
func (s *Service) placeUndoPayload(ctx context.Context, principalID, placeID string, before *Place) (json.RawMessage, error) {
	place, found, err := s.repository.LoadPlace(ctx, principalID, placeID)
	if err != nil || !found {
		return nil, err
	}
	undo := placeUndo{PlaceID: placeID, UpdatedAt: place.UpdatedAt}
	if before != nil {
		undo.Name, undo.RadiusM = before.Name, before.RadiusM
	}
	return json.Marshal(undo)
}

// Undo reverses a direct place action: it deletes a saved place, puts back the name and radius an
// update replaced, or recreates a deleted place under its own ID. A place changed since, gone, or
// in use by a watch or an anchored job since it was saved is refused.
func (s *Service) Undo(ctx context.Context, record approval.Record, raw json.RawMessage) (approval.Outcome, error) {
	var undo placeUndo
	if err := json.Unmarshal(raw, &undo); err != nil || undo.PlaceID == "" {
		return approval.Outcome{}, undoRefusal(ctx, unreadableRefusal)
	}
	switch {
	case record.Tool == toolPlaceSave && !undo.UpdatedAt.IsZero():
		return s.undoSaved(ctx, record.PrincipalID, undo)
	case record.Tool == toolPlaceUpdate && !undo.UpdatedAt.IsZero() && undo.Name != "":
		return s.undoUpdated(ctx, record.PrincipalID, undo)
	case record.Tool == toolPlaceDelete && !undo.DeletedAt.IsZero():
		return s.undoDeleted(ctx, record.PrincipalID, undo)
	default:
		return approval.Outcome{}, undoRefusal(ctx, notUndoableRefusal)
	}
}

var (
	unreadableRefusal      = locale.T("what to undo could not be read.", "не удалось прочитать, что отменять.")
	notUndoableRefusal     = locale.T("this action cannot be undone.", "это действие нельзя отменить.")
	placeChangedRefusal    = locale.T("the place was changed since; change it with a new request instead.", "это место уже изменили; измени его новым запросом.")
	placeGoneRefusal       = locale.T("the place no longer exists.", "такого места больше нет.")
	placeInvalidRefusal    = locale.T("the place as it was is no longer valid (%s).", "место в прежнем виде больше не подходит (%s).")
	nameTakenRefusal       = locale.T("another place is now called %s.", "теперь другое место называется %s.")
	nameTakenLaterRefusal  = locale.T("another place now has its name.", "теперь другое место носит это название.")
	placeBackRefusal       = locale.T("the place is already back or can no longer be restored.", "место уже восстановлено или его больше нельзя вернуть.")
	tooManyPlacesRefusal   = locale.T("You already have %d places, which is the most allowed; delete one first.", "У тебя уже %d мест, а это предел; сначала удали одно.")
	placeDeletedOutcome    = locale.T("Deleted the place %s.", "Место %s удалено.")
	placeRestoredAsOutcome = locale.T("Restored the place %s, radius %d m.", "Место %s возвращено как было, радиус %d м.")
	placeRestoredOutcome   = locale.T("Restored the place %s.", "Место %s восстановлено.")
)

// undoRefusal keeps the English reason for the record and the user's language for the user.
func undoRefusal(ctx context.Context, message locale.Text, arguments ...any) *approval.UndoRefusal {
	return &approval.UndoRefusal{Reason: message.FormatIn(locale.English, arguments...), UserReason: message.Format(ctx, arguments...)}
}

func undoOutcome(ctx context.Context, message locale.Text, arguments ...any) approval.Outcome {
	return approval.Outcome{Text: message.FormatIn(locale.English, arguments...), UserText: message.Format(ctx, arguments...)}
}

func (s *Service) unchangedPlace(ctx context.Context, principalID string, undo placeUndo) (Place, error) {
	place, found, err := s.repository.LoadPlace(ctx, principalID, undo.PlaceID)
	if err != nil {
		return Place{}, err
	}
	if !found {
		return Place{}, undoRefusal(ctx, placeGoneRefusal)
	}
	if !place.UpdatedAt.Equal(undo.UpdatedAt) {
		return Place{}, undoRefusal(ctx, placeChangedRefusal)
	}
	return place, nil
}

func (s *Service) undoSaved(ctx context.Context, principalID string, undo placeUndo) (approval.Outcome, error) {
	place, err := s.unchangedPlace(ctx, principalID, undo)
	if err != nil {
		return approval.Outcome{}, err
	}
	if _, _, err := s.deleteUnused(ctx, principalID, place.ID, ""); err != nil {
		var refusal *approval.ActionError
		if errors.As(err, &refusal) {
			return approval.Outcome{}, &approval.UndoRefusal{Reason: refusal.Text, UserReason: refusal.UserText}
		}
		return approval.Outcome{}, err
	}
	return undoOutcome(ctx, placeDeletedOutcome, place.Name), nil
}

func (s *Service) undoUpdated(ctx context.Context, principalID string, undo placeUndo) (approval.Outcome, error) {
	place, err := s.unchangedPlace(ctx, principalID, undo)
	if err != nil {
		return approval.Outcome{}, err
	}
	next := place
	next.Name, next.RadiusM, next.UpdatedAt = undo.Name, undo.RadiusM, s.now()
	if err := checkRadius(next.RadiusM); err != nil {
		return approval.Outcome{}, undoRefusal(ctx, placeInvalidRefusal, err.Error())
	}
	updated, err := s.updatePlace(ctx, next, place.UpdatedAt)
	switch {
	case errors.Is(err, ErrPlaceNameTaken):
		return approval.Outcome{}, undoRefusal(ctx, nameTakenRefusal, next.Name)
	case err != nil:
		return approval.Outcome{}, err
	case !updated:
		return approval.Outcome{}, undoRefusal(ctx, placeChangedRefusal)
	}
	s.logger.InfoContext(ctx, "place.updated", "principal_id", principalID, "place_id", place.ID)
	return undoOutcome(ctx, placeRestoredAsOutcome, next.Name, next.RadiusM), nil
}

// undoDeleted brings back a place deleted at undo.DeletedAt, which is kept unseen until the undo
// window ends and the prune removes it.
func (s *Service) undoDeleted(ctx context.Context, principalID string, undo placeUndo) (approval.Outcome, error) {
	restored, err := s.repository.RestorePlace(ctx, principalID, undo.PlaceID, undo.DeletedAt, MaximumPlacesPerPrincipal)
	switch {
	case errors.Is(err, ErrTooManyPlaces):
		return approval.Outcome{}, undoRefusal(ctx, tooManyPlacesRefusal, MaximumPlacesPerPrincipal)
	case errors.Is(err, ErrPlaceNameTaken):
		return approval.Outcome{}, undoRefusal(ctx, nameTakenLaterRefusal)
	case err != nil:
		return approval.Outcome{}, err
	case !restored:
		return approval.Outcome{}, undoRefusal(ctx, placeBackRefusal)
	}
	place, _, err := s.repository.LoadPlace(ctx, principalID, undo.PlaceID)
	if err != nil {
		return approval.Outcome{}, err
	}
	s.logger.InfoContext(ctx, "place.restored", "principal_id", principalID, "place_id", undo.PlaceID)
	return undoOutcome(ctx, placeRestoredOutcome, place.Name), nil
}

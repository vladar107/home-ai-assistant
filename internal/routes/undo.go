package routes

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

// routeUndo reverses a direct route action: UpdatedAt is when a save or an update left the route,
// Before the route an update replaced, and Removed the whole route a removal deleted.
type routeUndo struct {
	RouteID   string    `json:"route_id"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Before    *Route    `json:"before,omitempty"`
	Removed   *Route    `json:"removed,omitempty"`
}

// undoPayload is the payload that reverses a save, or an update of before, read back as stored.
func (s *Service) undoPayload(ctx context.Context, principalID, routeID string, before *Route) (json.RawMessage, error) {
	route, found, err := s.repository.LoadRoute(ctx, principalID, routeID)
	if err != nil || !found {
		return nil, err
	}
	return json.Marshal(routeUndo{RouteID: routeID, UpdatedAt: route.UpdatedAt, Before: before})
}

// Undo reverses a direct route action: it deletes a saved route, puts back the route an update
// replaced, or recreates a removed route under its own ID. A route changed since, or one that is
// gone, is refused.
func (s *Service) Undo(ctx context.Context, record approval.Record, raw json.RawMessage) (approval.Outcome, error) {
	var undo routeUndo
	if err := json.Unmarshal(raw, &undo); err != nil || undo.RouteID == "" {
		return approval.Outcome{}, undoRefusal(ctx, unreadableRefusal)
	}
	switch {
	case record.Tool == toolSave && !undo.UpdatedAt.IsZero():
		return s.undoSaved(ctx, record.PrincipalID, undo)
	case record.Tool == toolUpdate && !undo.UpdatedAt.IsZero() && undo.Before != nil:
		return s.undoUpdated(ctx, record.PrincipalID, undo)
	case record.Tool == toolRemove && undo.Removed != nil:
		return s.undoRemoved(ctx, record.PrincipalID, *undo.Removed)
	default:
		return approval.Outcome{}, undoRefusal(ctx, notUndoableRefusal)
	}
}

var (
	unreadableRefusal      = locale.T("what to undo could not be read.", "не удалось прочитать, что отменять.")
	notUndoableRefusal     = locale.T("this action cannot be undone.", "это действие нельзя отменить.")
	changedSinceRefusal    = locale.T("the route was changed since; change it with a new request instead.", "этот маршрут уже изменили; измени его новым запросом.")
	routeGoneRefusal       = locale.T("the route no longer exists.", "такого маршрута больше нет.")
	routeInvalidRefusal    = locale.T("the route as it was is no longer valid (%s).", "маршрут в прежнем виде больше не подходит (%s).")
	nameTakenRefusal       = locale.T("another route is now called %s.", "теперь другой маршрут называется %s.")
	routeForeignRefusal    = locale.T("the route belongs to someone else.", "маршрут принадлежит другому человеку.")
	removedInvalidRefusal  = locale.T("the route is no longer valid (%s).", "маршрут больше не подходит (%s).")
	tooManyRefusal         = locale.T("You already have %d routes, which is the most allowed; delete one in /jobs first.", "У тебя уже %d маршрутов, а это предел; сначала удали один в /jobs.")
	routeBackRefusal       = locale.T("the route is already back.", "маршрут уже восстановлен.")
	routeDeletedOutcome    = locale.T("Deleted the route %s.", "Маршрут %s удалён.")
	routeRestoredAsOutcome = locale.T("Restored the route %s as it was.", "Маршрут %s возвращён как был.")
	routeRestoredOutcome   = locale.T("Restored the route %s.", "Маршрут %s восстановлен.")
)

// undoRefusal keeps the English reason for the record and the user's language for the user.
func undoRefusal(ctx context.Context, message locale.Text, arguments ...any) *approval.UndoRefusal {
	return &approval.UndoRefusal{Reason: message.FormatIn(locale.English, arguments...), UserReason: message.Format(ctx, arguments...)}
}

func undoOutcome(ctx context.Context, message locale.Text, arguments ...any) approval.Outcome {
	return approval.Outcome{Text: message.FormatIn(locale.English, arguments...), UserText: message.Format(ctx, arguments...)}
}

// unchanged loads the route an Undo works on, refusing one that is gone or was changed since.
func (s *Service) unchanged(ctx context.Context, principalID string, undo routeUndo) (Route, error) {
	route, found, err := s.repository.LoadRoute(ctx, principalID, undo.RouteID)
	if err != nil {
		return Route{}, err
	}
	if !found {
		return Route{}, undoRefusal(ctx, routeGoneRefusal)
	}
	if !route.UpdatedAt.Equal(undo.UpdatedAt) {
		return Route{}, undoRefusal(ctx, changedSinceRefusal)
	}
	return route, nil
}

func (s *Service) undoSaved(ctx context.Context, principalID string, undo routeUndo) (approval.Outcome, error) {
	route, err := s.unchanged(ctx, principalID, undo)
	if err != nil {
		return approval.Outcome{}, err
	}
	deleted, err := s.repository.DeleteUnchangedRoute(ctx, principalID, route.ID, route.UpdatedAt)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !deleted {
		return approval.Outcome{}, undoRefusal(ctx, changedSinceRefusal)
	}
	s.logger.InfoContext(ctx, "route.deleted", "principal_id", principalID, "route_id", route.ID)
	return undoOutcome(ctx, routeDeletedOutcome, route.Name), nil
}

func (s *Service) undoUpdated(ctx context.Context, principalID string, undo routeUndo) (approval.Outcome, error) {
	route, err := s.unchanged(ctx, principalID, undo)
	if err != nil {
		return approval.Outcome{}, err
	}
	spec, err := normalize(undo.Before.Spec)
	if err != nil {
		return approval.Outcome{}, undoRefusal(ctx, routeInvalidRefusal, err.Error())
	}
	next := route
	next.Spec, next.UpdatedAt = spec, s.now()
	updated, err := s.repository.UpdateRoute(ctx, next, route.UpdatedAt)
	if errors.Is(err, ErrNameTaken) {
		return approval.Outcome{}, undoRefusal(ctx, nameTakenRefusal, spec.Name)
	}
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, undoRefusal(ctx, changedSinceRefusal)
	}
	s.logger.InfoContext(ctx, "route.updated", "principal_id", principalID, "route_id", route.ID)
	return undoOutcome(ctx, routeRestoredAsOutcome, spec.Name), nil
}

func (s *Service) undoRemoved(ctx context.Context, principalID string, removed Route) (approval.Outcome, error) {
	if removed.PrincipalID != principalID {
		return approval.Outcome{}, undoRefusal(ctx, routeForeignRefusal)
	}
	spec, err := normalize(removed.Spec)
	if err != nil {
		return approval.Outcome{}, undoRefusal(ctx, removedInvalidRefusal, err.Error())
	}
	route := Route{ID: removed.ID, PrincipalID: principalID, Spec: spec, CreatedAt: removed.CreatedAt, UpdatedAt: s.now()}
	created, err := s.repository.CreateRoute(ctx, route, MaximumRoutesPerPrincipal)
	switch {
	case errors.Is(err, ErrTooManyRoutes):
		return approval.Outcome{}, undoRefusal(ctx, tooManyRefusal, MaximumRoutesPerPrincipal)
	case errors.Is(err, ErrNameTaken):
		return approval.Outcome{}, undoRefusal(ctx, nameTakenRefusal, spec.Name)
	case err != nil:
		return approval.Outcome{}, err
	case !created:
		return approval.Outcome{}, undoRefusal(ctx, routeBackRefusal)
	}
	s.logger.InfoContext(ctx, "route.created", "principal_id", principalID, "route_id", route.ID, "legs", len(spec.Legs))
	return undoOutcome(ctx, routeRestoredOutcome, spec.Name), nil
}

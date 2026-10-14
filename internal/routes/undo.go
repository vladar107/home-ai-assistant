package routes

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
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
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "what to undo could not be read."}
	}
	switch {
	case record.Tool == toolSave && !undo.UpdatedAt.IsZero():
		return s.undoSaved(ctx, record.PrincipalID, undo)
	case record.Tool == toolUpdate && !undo.UpdatedAt.IsZero() && undo.Before != nil:
		return s.undoUpdated(ctx, record.PrincipalID, undo)
	case record.Tool == toolRemove && undo.Removed != nil:
		return s.undoRemoved(ctx, record.PrincipalID, *undo.Removed)
	default:
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "this action cannot be undone."}
	}
}

var changedSince = &approval.UndoRefusal{Reason: "the route was changed since; change it with a new request instead."}

// unchanged loads the route an Undo works on, refusing one that is gone or was changed since.
func (s *Service) unchanged(ctx context.Context, principalID string, undo routeUndo) (Route, error) {
	route, found, err := s.repository.LoadRoute(ctx, principalID, undo.RouteID)
	if err != nil {
		return Route{}, err
	}
	if !found {
		return Route{}, &approval.UndoRefusal{Reason: "the route no longer exists."}
	}
	if !route.UpdatedAt.Equal(undo.UpdatedAt) {
		return Route{}, changedSince
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
		return approval.Outcome{}, changedSince
	}
	s.logger.InfoContext(ctx, "route.deleted", "principal_id", principalID, "route_id", route.ID)
	return approval.Outcome{Text: "Deleted the route " + route.Name + "."}, nil
}

func (s *Service) undoUpdated(ctx context.Context, principalID string, undo routeUndo) (approval.Outcome, error) {
	route, err := s.unchanged(ctx, principalID, undo)
	if err != nil {
		return approval.Outcome{}, err
	}
	spec, err := normalize(undo.Before.Spec)
	if err != nil {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the route as it was is no longer valid (" + err.Error() + ")."}
	}
	next := route
	next.Spec, next.UpdatedAt = spec, s.now()
	updated, err := s.repository.UpdateRoute(ctx, next, route.UpdatedAt)
	if errors.Is(err, ErrNameTaken) {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "another route is now called " + spec.Name + "."}
	}
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, changedSince
	}
	s.logger.InfoContext(ctx, "route.updated", "principal_id", principalID, "route_id", route.ID)
	return approval.Outcome{Text: "Restored the route " + spec.Name + " as it was."}, nil
}

func (s *Service) undoRemoved(ctx context.Context, principalID string, removed Route) (approval.Outcome, error) {
	if removed.PrincipalID != principalID {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the route belongs to someone else."}
	}
	spec, err := normalize(removed.Spec)
	if err != nil {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the route is no longer valid (" + err.Error() + ")."}
	}
	route := Route{ID: removed.ID, PrincipalID: principalID, Spec: spec, CreatedAt: removed.CreatedAt, UpdatedAt: s.now()}
	created, err := s.repository.CreateRoute(ctx, route, MaximumRoutesPerPrincipal)
	switch {
	case errors.Is(err, ErrTooManyRoutes):
		return approval.Outcome{}, &approval.UndoRefusal{Reason: tooManyText()}
	case errors.Is(err, ErrNameTaken):
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "another route is now called " + spec.Name + "."}
	case err != nil:
		return approval.Outcome{}, err
	case !created:
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the route is already back."}
	}
	s.logger.InfoContext(ctx, "route.created", "principal_id", principalID, "route_id", route.ID, "legs", len(spec.Legs))
	return approval.Outcome{Text: "Restored the route " + spec.Name + "."}, nil
}

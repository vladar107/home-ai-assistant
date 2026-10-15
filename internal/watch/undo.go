package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// watchUndo reverses a direct watch action: UpdatedAt is when a create or an update left the
// watch, Before the watch an update replaced, and Removed the whole watch a removal deleted.
type watchUndo struct {
	WatchID   string    `json:"watch_id"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Before    *Watch    `json:"before,omitempty"`
	Removed   *Watch    `json:"removed,omitempty"`
	// TravelChanged is true when the update changed the travel flag, which is then the only case an
	// Undo restores it; an older payload lacks it and leaves the flag as it is.
	TravelChanged bool `json:"travel_changed,omitempty"`
}

// undoPayload is the payload that reverses a create, or an update of before, read back as stored.
func (s *Service) undoPayload(ctx context.Context, principalID, watchID string, before *Watch, travelChanged bool) (json.RawMessage, error) {
	watch, found, err := s.repository.LoadWatch(ctx, principalID, watchID)
	if err != nil || !found {
		return nil, err
	}
	return json.Marshal(watchUndo{WatchID: watchID, UpdatedAt: watch.UpdatedAt, Before: before, TravelChanged: travelChanged})
}

// Undo reverses a direct watch action: it deletes a created watch, puts back the definition an
// update replaced, or recreates a removed watch under its own ID, whose window states and alerts
// start fresh. A watch changed since, one that is gone, and a one-off whose date has passed are
// refused.
func (s *Service) Undo(ctx context.Context, record approval.Record, raw json.RawMessage) (approval.Outcome, error) {
	var undo watchUndo
	if err := json.Unmarshal(raw, &undo); err != nil || undo.WatchID == "" {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "what to undo could not be read."}
	}
	switch {
	case record.Tool == toolPropose && !undo.UpdatedAt.IsZero():
		return s.undoCreated(ctx, record.PrincipalID, undo)
	case record.Tool == toolUpdate && !undo.UpdatedAt.IsZero() && undo.Before != nil:
		return s.undoUpdated(ctx, record.PrincipalID, undo)
	case record.Tool == toolRemove && undo.Removed != nil:
		return s.undoRemoved(ctx, record.PrincipalID, *undo.Removed)
	default:
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "this action cannot be undone."}
	}
}

// unchanged loads the watch an Undo works on, refusing one that is gone or was changed since.
func (s *Service) unchanged(ctx context.Context, principalID string, undo watchUndo) (Watch, error) {
	watch, found, err := s.repository.LoadWatch(ctx, principalID, undo.WatchID)
	if err != nil {
		return Watch{}, err
	}
	if !found {
		return Watch{}, &approval.UndoRefusal{Reason: "the watch no longer exists."}
	}
	if !watch.UpdatedAt.Equal(undo.UpdatedAt) {
		return Watch{}, &approval.UndoRefusal{Reason: "the watch was changed since; change it in /jobs instead."}
	}
	return watch, nil
}

func (s *Service) undoCreated(ctx context.Context, principalID string, undo watchUndo) (approval.Outcome, error) {
	watch, err := s.unchanged(ctx, principalID, undo)
	if err != nil {
		return approval.Outcome{}, err
	}
	deleted, err := s.repository.DeleteUnchangedWatch(ctx, principalID, watch.ID, watch.UpdatedAt)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !deleted {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the watch was changed since; change it in /jobs instead."}
	}
	s.logger.InfoContext(ctx, "watch.deleted", "principal_id", principalID, "watch_id", watch.ID)
	return approval.Outcome{Text: "Stopped watching " + watch.Label() + "."}, nil
}

func (s *Service) undoUpdated(ctx context.Context, principalID string, undo watchUndo) (approval.Outcome, error) {
	watch, err := s.unchanged(ctx, principalID, undo)
	if err != nil {
		return approval.Outcome{}, err
	}
	if watch.Kind != undo.Before.Kind {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the watch was changed since; change it in /jobs instead."}
	}
	spec, err := normalize(undo.Before.Spec, s.now())
	if err != nil {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the watch as it was is no longer valid (" + err.Error() + ")."}
	}
	if sameSpec(spec, watch.Spec) {
		if undo.TravelChanged {
			if _, err := s.repository.SetWatchTravelPause(ctx, principalID, watch.ID, undo.Before.TravelPause); err != nil {
				return approval.Outcome{}, err
			}
		}
		return approval.Outcome{Text: "Restored the watch " + spec.Label() + " as it was."}, nil
	}
	next := watch
	next.Spec, next.UpdatedAt = spec, s.now()
	if undo.TravelChanged {
		next.TravelPause = undo.Before.TravelPause
	}
	updated, err := s.repository.UpdateWatch(ctx, next, watch.UpdatedAt)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the watch was changed since; change it in /jobs instead."}
	}
	s.logger.InfoContext(ctx, "watch.updated", "principal_id", principalID, "watch_id", watch.ID, "kind", spec.Kind)
	return approval.Outcome{Text: "Restored the watch " + spec.Label() + " as it was."}, nil
}

func (s *Service) undoRemoved(ctx context.Context, principalID string, removed Watch) (approval.Outcome, error) {
	if removed.PrincipalID != principalID {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the watch belongs to someone else."}
	}
	now := s.now()
	spec, err := normalize(removed.Spec, now)
	if err == nil && removed.Expired(now) {
		err = fmt.Errorf("its last day has passed")
	}
	if err != nil {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the watch can no longer be set up (" + err.Error() + ")."}
	}
	if spec.Kind == KindAgent {
		existing, err := s.repository.ListWatches(ctx, principalID)
		if err != nil {
			return approval.Outcome{}, err
		}
		if agentWatches(existing) >= MaximumAgentWatchesPerPrincipal && !slices.ContainsFunc(existing, func(w Watch) bool { return w.ID == removed.ID }) {
			return approval.Outcome{}, &approval.UndoRefusal{Reason: tooManyAgentText()}
		}
	}
	watch := Watch{ID: removed.ID, PrincipalID: principalID, Spec: spec, Enabled: removed.Enabled, MutedOn: removed.MutedOn, TravelPause: removed.TravelPause, CreatedAt: removed.CreatedAt, UpdatedAt: now}
	created, err := s.repository.CreateWatch(ctx, watch, MaximumWatchesPerPrincipal)
	if errors.Is(err, ErrTooManyWatches) {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: tooManyText()}
	}
	if err != nil {
		return approval.Outcome{}, err
	}
	if !created {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the watch is already back."}
	}
	s.logger.InfoContext(ctx, "watch.created", "principal_id", principalID, "watch_id", watch.ID, "kind", spec.Kind)
	return approval.Outcome{Text: "Watching " + spec.Label() + " again; its earlier alerts are not kept."}, nil
}

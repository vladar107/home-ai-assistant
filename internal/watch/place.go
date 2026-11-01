package watch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// SavedPlace is one of the user's saved places as a place watch names it.
type SavedPlace struct {
	ID   string
	Name string
}

// SavedPlaces are the user's saved places and whether they share the live location that arriving
// and leaving are noticed on. GuardPlace runs store while the principal's place cannot be deleted,
// and returns ErrPlaceGone without running it once the place is gone.
type SavedPlaces interface {
	FindSavedPlace(ctx context.Context, principalID, name string) (SavedPlace, bool, error)
	SharingLive(ctx context.Context, principalID string) (bool, error)
	GuardPlace(ctx context.Context, principalID, placeID string, store func() error) error
}

// ErrPlaceGone reports that the saved place a place watch names was deleted.
var ErrPlaceGone = errors.New("the saved place no longer exists")

// guardPlace stores a place watch with store while its place cannot be deleted, so no watch is left
// on a place deleted between its proposal and its confirmation; any other watch is stored at once.
func (s *Service) guardPlace(ctx context.Context, watch Watch, store func() error) error {
	if watch.Kind != KindPlace {
		return store()
	}
	evaluator, ok := s.evaluators[KindPlace].(*PlaceEvaluator)
	if !ok {
		return fmt.Errorf("place watches are not available")
	}
	return evaluator.places.GuardPlace(ctx, watch.PrincipalID, watch.Target.Place, store)
}

func placeGoneText(spec Spec) string {
	return "The saved place " + spec.Target.Name + " no longer exists."
}

// PlaceEvaluator resolves place watches to the user's saved places. A place watch is never checked
// on the loop: the location service reports each arrival and leaving to Service.PlaceEvent.
type PlaceEvaluator struct {
	places SavedPlaces
}

var (
	_ Evaluator = (*PlaceEvaluator)(nil)
	_ Resolver  = (*PlaceEvaluator)(nil)
)

func NewPlaceEvaluator(places SavedPlaces) (*PlaceEvaluator, error) {
	if places == nil {
		return nil, fmt.Errorf("place watches need saved places")
	}
	return &PlaceEvaluator{places: places}, nil
}

// Resolve finds the saved place the watch names, so the watch keeps its ID and its name as saved.
func (e *PlaceEvaluator) Resolve(ctx context.Context, principalID string, spec Spec) (Spec, error) {
	place, found, err := e.places.FindSavedPlace(ctx, principalID, spec.Target.Name)
	if err != nil {
		return Spec{}, err
	}
	if !found {
		return Spec{}, fmt.Errorf("no saved place is called %q; save it with place_save first, or use place_list to see the user's places", spec.Target.Name)
	}
	spec.Target.Name, spec.Target.Place = place.Name, place.ID
	return spec, nil
}

// Evaluate is never called by the loop, which leaves place watches out.
func (e *PlaceEvaluator) Evaluate(context.Context, string, Watch, Occurrence) (Observation, error) {
	return Observation{}, fmt.Errorf("place watches are told of arrivals and leavings: %w", ErrSkip)
}

// notSharingNote is what a place watch's owner is told when they set one up without sharing their
// live location.
const notSharingNote = "It works only while you share your live location in this chat, and you are not sharing it now."

// placeNote is notSharingNote when the principal shares no live location now, and empty otherwise
// or when that cannot be told.
func (s *Service) placeNote(ctx context.Context, principalID string) string {
	evaluator, ok := s.evaluators[KindPlace].(*PlaceEvaluator)
	if !ok {
		return ""
	}
	sharing, err := evaluator.places.SharingLive(ctx, principalID)
	if err != nil || sharing {
		return ""
	}
	return notSharingNote
}

// PlaceEvent alerts the principal's enabled place watches on the place that ask for this edge, when
// it falls in their window on a day they run and they are not muted or paused for a trip. Each
// alert counts against its window's messages, as any watch's does, and the delivery that follows is
// the loop's.
func (s *Service) PlaceEvent(ctx context.Context, principalID, placeID, placeName, edge string, at time.Time) {
	watches, err := s.repository.ListWatches(ctx, principalID)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "watch.place.failed", "principal_id", principalID, "stage", "load_watches", "error", err)
		}
		return
	}
	s.placeMu.Lock()
	defer s.placeMu.Unlock()
	queued := false
	cache := NewCache()
	for _, watch := range watches {
		if watch.Kind != KindPlace || watch.Target.Place != placeID || !watch.Enabled || !slices.Contains(watch.Triggers, edge) {
			continue
		}
		occurrence, active := watch.OccurrenceAt(at)
		if !active || at.Before(occurrence.Start) || at.After(occurrence.End) || watch.MutedOn == occurrence.Date {
			continue
		}
		if watch.TravelPause && s.travelingAt(ctx, cache, watch.PrincipalID, watch.ID, at) {
			continue
		}
		state, found, err := s.repository.LoadWatchState(ctx, watch.ID, occurrence.Date)
		if err != nil {
			s.logWatchError(ctx, watch, "load_state", err)
			continue
		}
		if !found {
			state = State{WatchID: watch.ID, PrincipalID: watch.PrincipalID, Occurrence: occurrence.Date}
			if state.Messages, err = s.repository.LastWatchAlertSequence(ctx, watch.ID, occurrence.Date); err != nil {
				s.logWatchError(ctx, watch, "load_state", err)
				continue
			}
		}
		now := s.now()
		alert := queue(&state, watch, occurrence, now, alertTypeAlert, placeText(watch, placeName, edge))
		state.Key, state.CheckedAt = edge, now
		if err := s.repository.SaveWatchCheck(ctx, state, alert, now); err != nil {
			s.logWatchError(ctx, watch, "store_check", err)
			continue
		}
		if s.telemetry != nil {
			s.telemetry.RecordWatchCheck(ctx, watch.PrincipalID, watch.Kind, checkTriggered)
		}
		if alert != nil {
			queued = true
			s.logger.InfoContext(ctx, "watch.alert.queued", "principal_id", watch.PrincipalID, "watch_id", watch.ID, "type", alert.Type, "sequence", alert.Sequence)
		}
	}
	if queued {
		s.Wake()
	}
}

func placeText(watch Watch, placeName, edge string) string {
	name := placeName
	if name == "" {
		name = watch.Target.Name
	}
	headline := "You arrived at " + name + "."
	if edge == TriggerLeave {
		headline = "You left " + name + "."
	}
	return "**📍 " + watch.Label() + "**\n\n" + headline + " Noticed on your live location."
}

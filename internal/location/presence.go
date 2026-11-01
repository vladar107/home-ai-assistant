package location

import (
	"context"
	"time"
)

const (
	// settleInterval is how often a change of presence first seen on a fix with none after it is
	// confirmed once it has lasted confirmAfter.
	settleInterval = 30 * time.Second
	// staleEvent is how old an arrival or leaving may be and still be told: an older one comes from
	// edits Telegram queued while the bot was down, and running jobs or alerting on it would be late.
	staleEvent = 5 * time.Minute
)

// SetPlaceListener sets who hears about arrivals and leavings, at wiring time before the service
// starts.
func (s *Service) SetPlaceListener(listener PlaceListener) {
	s.listener = listener
}

// track steps the presence of each of the principal's places with a live fix and tells the listener
// about every arrival and leaving it confirms. A failure is logged, without the position, and never
// fails the share.
func (s *Service) track(ctx context.Context, fix Fix) {
	events, err := s.stepPresence(ctx, fix)
	if err != nil {
		s.logger.WarnContext(ctx, "location.presence.failed", "principal_id", fix.PrincipalID, "stage", "step", "error", err)
	}
	s.notify(ctx, events)
}

func (s *Service) stepPresence(ctx context.Context, fix Fix) ([]PlaceEvent, error) {
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	places, err := s.repository.ListPlaces(ctx, fix.PrincipalID)
	if err != nil || len(places) == 0 {
		return nil, err
	}
	presences, err := s.presenceByPlace(ctx, fix.PrincipalID)
	if err != nil {
		return nil, err
	}
	var events []PlaceEvent
	for _, place := range places {
		current, known := presences[place.ID]
		next, save, edge := step(place, current, known, fix)
		if !save {
			continue
		}
		if err := s.repository.SavePresence(ctx, next); err != nil {
			return events, err
		}
		if edge != "" {
			events = append(events, PlaceEvent{PrincipalID: fix.PrincipalID, PlaceID: place.ID, PlaceName: place.Name, Edge: edge, At: fix.UpdatedAt})
		}
	}
	return events, nil
}

// SettlePresence confirms the changes of presence that have lasted long enough without a fix since,
// while their live share still runs. The loop runs it every settleInterval; it is safe to call
// directly.
func (s *Service) SettlePresence(ctx context.Context) error {
	fixes, err := s.repository.ListLocations(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	for _, fix := range fixes {
		if !fix.Live(now) {
			continue
		}
		events, err := s.settlePrincipal(ctx, fix, now)
		s.notify(ctx, events)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) settlePrincipal(ctx context.Context, fix Fix, now time.Time) ([]PlaceEvent, error) {
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	presences, err := s.repository.ListPresence(ctx, fix.PrincipalID)
	if err != nil {
		return nil, err
	}
	var events []PlaceEvent
	for _, presence := range presences {
		if presence.Candidate == "" || presence.LiveMessageID != fix.LiveMessageID {
			continue
		}
		next, edge := settle(presence, now)
		if edge == "" {
			continue
		}
		place, found, err := s.repository.LoadPlace(ctx, fix.PrincipalID, presence.PlaceID)
		if err != nil {
			return events, err
		}
		if !found {
			continue
		}
		if err := s.repository.SavePresence(ctx, next); err != nil {
			return events, err
		}
		events = append(events, PlaceEvent{PrincipalID: fix.PrincipalID, PlaceID: place.ID, PlaceName: place.Name, Edge: edge, At: now})
	}
	return events, nil
}

func (s *Service) notify(ctx context.Context, events []PlaceEvent) {
	for _, event := range events {
		if s.now().Sub(event.At) > staleEvent {
			s.logger.InfoContext(ctx, "location.place.stale", "principal_id", event.PrincipalID, "place_id", event.PlaceID, "edge", event.Edge)
			continue
		}
		s.logger.InfoContext(ctx, "location.place.crossed", "principal_id", event.PrincipalID, "place_id", event.PlaceID, "edge", event.Edge)
		if s.listener != nil {
			s.listener.PlaceEvent(ctx, event)
		}
	}
}

// resetPresence makes the principal's places unknown again where the live message, or any for zero,
// set them.
func (s *Service) resetPresence(ctx context.Context, principalID string, liveMessageID int64) error {
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	return s.repository.ResetPresence(ctx, principalID, liveMessageID)
}

func (s *Service) presenceByPlace(ctx context.Context, principalID string) (map[string]Presence, error) {
	presences, err := s.repository.ListPresence(ctx, principalID)
	if err != nil {
		return nil, err
	}
	byPlace := make(map[string]Presence, len(presences))
	for _, presence := range presences {
		byPlace[presence.PlaceID] = presence
	}
	return byPlace, nil
}

// insidePlace names the saved place the live share shows the principal inside, if any; of places
// that overlap, the smallest, and of those the nearest. A failed read leaves it out rather than
// failing the turn.
func (s *Service) insidePlace(ctx context.Context, fix Fix) string {
	presences, err := s.repository.ListPresence(ctx, fix.PrincipalID)
	if err != nil {
		s.logger.WarnContext(ctx, "location.presence.failed", "principal_id", fix.PrincipalID, "stage", "turn_line", "error", err)
		return ""
	}
	var best Place
	bestAway := 0.0
	for _, presence := range presences {
		if presence.State != PresenceInside || presence.LiveMessageID != fix.LiveMessageID {
			continue
		}
		place, found, err := s.repository.LoadPlace(ctx, fix.PrincipalID, presence.PlaceID)
		if err != nil || !found {
			continue
		}
		away := distance(place.Latitude, place.Longitude, fix.Latitude, fix.Longitude)
		if best.ID == "" || place.RadiusM < best.RadiusM || (place.RadiusM == best.RadiusM && away < bestAway) {
			best, bestAway = place, away
		}
	}
	return best.Name
}

// GuardPlace stores what names the principal's place, such as a watch or an anchored job, with
// store, while the place cannot be deleted; a place deleted or not the principal's is refused with
// ErrPlaceGone.
func (s *Service) GuardPlace(ctx context.Context, principalID, placeID string, store func() error) error {
	s.placeUse.Lock()
	defer s.placeUse.Unlock()
	if _, found, err := s.repository.LoadPlace(ctx, principalID, placeID); err != nil {
		return err
	} else if !found {
		return ErrPlaceGone
	}
	return store()
}

// SharingLive reports whether the principal shares a live location now, which arrivals and leavings
// need.
func (s *Service) SharingLive(ctx context.Context, principalID string) (bool, error) {
	fix, found, err := s.Current(ctx, principalID)
	if err != nil || !found {
		return false, err
	}
	return fix.Live(s.now()), nil
}

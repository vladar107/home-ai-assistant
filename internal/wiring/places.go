package wiring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
	userlocation "github.com/vladar107/home-ai-assistant/internal/location"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// placeAnchors resolves a job's place anchor to the owner's own saved place. A place has no
// occurrence until an arrival or a leaving, which placeEvents hands to the scheduler.
type placeAnchors struct {
	locations *userlocation.Service
}

func (p placeAnchors) ResolveAnchor(ctx context.Context, principalID string, anchor schedule.Anchor, _ time.Time, _ string) (schedule.AnchorOccurrence, error) {
	place, found, err := p.locations.PlaceByID(ctx, principalID, anchor.PlaceID)
	if err != nil {
		return schedule.AnchorOccurrence{}, err
	}
	if !found {
		return schedule.AnchorOccurrence{}, fmt.Errorf("that saved place no longer exists or is not the user's; use place_list to see the user's places")
	}
	occurrence := schedule.AnchorOccurrence{Label: place.Name}
	if sharing, err := p.locations.SharingLive(ctx, principalID); err == nil && !sharing {
		occurrence.Note = userlocation.NotSharingNote
	}
	return occurrence, nil
}

// GuardAnchor stores a job anchored to a place while the place cannot be deleted.
func (p placeAnchors) GuardAnchor(ctx context.Context, principalID string, anchor schedule.Anchor, store func() error) error {
	err := p.locations.GuardPlace(ctx, principalID, anchor.PlaceID, store)
	if errors.Is(err, userlocation.ErrPlaceGone) {
		return schedule.ErrAnchorGone
	}
	return err
}

// watchPlaces hands the user's saved places to place watches.
type watchPlaces struct {
	locations *userlocation.Service
}

func (p watchPlaces) FindSavedPlace(ctx context.Context, principalID, name string) (watch.SavedPlace, bool, error) {
	place, found, err := p.locations.PlaceNamed(ctx, principalID, name)
	if err != nil || !found {
		return watch.SavedPlace{}, false, err
	}
	return watch.SavedPlace{ID: place.ID, Name: place.Name}, true, nil
}

func (p watchPlaces) SharingLive(ctx context.Context, principalID string) (bool, error) {
	return p.locations.SharingLive(ctx, principalID)
}

func (p watchPlaces) GuardPlace(ctx context.Context, principalID, placeID string, store func() error) error {
	err := p.locations.GuardPlace(ctx, principalID, placeID, store)
	if errors.Is(err, userlocation.ErrPlaceGone) {
		return watch.ErrPlaceGone
	}
	return err
}

// webPlaces hands the user's saved places to the weather forecast and weather watches, which look
// them up before geocoding a name.
type webPlaces struct {
	locations *userlocation.Service
}

func (p webPlaces) FindSavedPlace(ctx context.Context, principalID, name string) (web.Place, bool, error) {
	place, found, err := p.locations.PlaceNamed(ctx, principalID, name)
	if err != nil || !found {
		return web.Place{}, false, err
	}
	return web.Place{Name: place.Name, Latitude: place.Latitude, Longitude: place.Longitude}, true, nil
}

// placeEvents hands each arrival and leaving to the jobs anchored to the place and to the place
// watches on it; nil leaves either out.
type placeEvents struct {
	scheduler *schedule.Service
	watches   *watch.Service
	logger    *slog.Logger
}

func (p placeEvents) PlaceEvent(ctx context.Context, event userlocation.PlaceEvent) {
	if p.scheduler != nil {
		if err := p.scheduler.PlaceEvent(ctx, event.PrincipalID, event.PlaceID, event.Edge, event.At); err != nil {
			p.logger.WarnContext(ctx, "schedule.anchor.failed", "principal_id", event.PrincipalID, "kind", schedule.AnchorPlace, "stage", "place_event", "error", err)
		}
	}
	if p.watches != nil {
		p.watches.PlaceEvent(ctx, event.PrincipalID, event.PlaceID, event.PlaceName, event.Edge, event.At)
	}
}

// placeDependents names the jobs anchored to a place and the place watches on it, so a place in use
// is not deleted.
type placeDependents struct {
	scheduler *schedule.Service
	watches   *watch.Service
}

func (p placeDependents) PlaceDependents(ctx context.Context, principalID, placeID string) ([]string, error) {
	var names []string
	jobs, err := p.scheduler.Jobs(ctx, principalID)
	if err != nil {
		return nil, err
	}
	for _, job := range jobs {
		if anchor := schedule.JobAnchor(job); anchor != nil && anchor.Kind == schedule.AnchorPlace && anchor.PlaceID == placeID {
			kind := "the job"
			if job.Type == schedule.TypeReminder {
				kind = "the reminder"
			}
			names = append(names, fmt.Sprintf("%s %q (id %s)", kind, job.Name, job.ID))
		}
	}
	if p.watches == nil {
		return names, nil
	}
	watches, err := p.watches.Watches(ctx, principalID)
	if err != nil {
		return nil, err
	}
	for _, item := range watches {
		if item.Kind == watch.KindPlace && item.Target.Place == placeID {
			names = append(names, fmt.Sprintf("the watch %q (id %s)", item.Label(), item.ID))
		}
	}
	return names, nil
}

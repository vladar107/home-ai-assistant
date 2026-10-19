package trip

import (
	"context"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/localtime"
)

// Travel answers whether a principal is traveling from the repository alone, so the scheduler and
// the watches can ask without the trip service.
type Travel struct {
	repository Repository
}

// NewTravel returns the trip lookup over the repository.
func NewTravel(repository Repository) Travel { return Travel{repository: repository} }

// Traveling is true when a planned trip of the principal spans the instant, from its start up
// to but not including its end.
func (t Travel) Traveling(ctx context.Context, principalID string, at time.Time) (bool, error) {
	active, err := t.repository.ListActiveTrips(ctx, principalID, at)
	return len(active) > 0, err
}

// Reader is the read side of the trips for the turn line and the trip tabs: it needs the
// repository and the clock, not the trip service with its reminders and tabs.
type Reader struct {
	repository Repository
	now        func() time.Time
}

// NewReader returns the reader over the repository; a nil clock means time.Now.
func NewReader(repository Repository, now func() time.Time) Reader {
	if now == nil {
		now = time.Now
	}
	return Reader{repository: repository, now: now}
}

func (r Reader) Now() time.Time { return r.now() }

// Trip finds one of the principal's trips of any status.
func (r Reader) Trip(ctx context.Context, principalID, id string) (Trip, bool, error) {
	return r.repository.LoadTrip(ctx, principalID, id)
}

// SetSummaryMessage records the message that sums the trip up in its tab; zero forgets it.
func (r Reader) SetSummaryMessage(ctx context.Context, principalID, id string, messageID int64) error {
	return r.repository.SetTripSummary(ctx, principalID, id, messageID)
}

// TurnLine tells a turn about the active trip or the next one starting within 14 days; empty
// when there is none.
func (r Reader) TurnLine(ctx context.Context, principalID string) (string, error) {
	planned, err := r.repository.ListTrips(ctx, principalID, StatusPlanned)
	if err != nil {
		return "", err
	}
	now := r.now()
	var next *Trip
	for index := range planned {
		trip := planned[index]
		if trip.Active(now) {
			location := localtime.SavedZone(trip.Zone)
			return fmt.Sprintf("User is traveling: %q, destination %q, %s to %s (%s); local time there is %s.",
				trip.Title, trip.Destination, trip.StartsAt.In(location).Format("Mon 2006-01-02"),
				trip.EndLabel(turnDateLayout, turnDateLayout), location, now.In(location).Format("Mon 2006-01-02 15:04")), nil
		}
		if trip.StartsAt.After(now) && trip.StartsAt.Sub(now) <= turnLineHorizon && (next == nil || trip.StartsAt.Before(next.StartsAt)) {
			next = &planned[index]
		}
	}
	if next == nil {
		return "", nil
	}
	location := localtime.SavedZone(next.Zone)
	return fmt.Sprintf("User's next trip: %q, destination %q, %s to %s (%s).",
		next.Title, next.Destination, next.StartsAt.In(location).Format("Mon 2006-01-02"),
		next.EndLabel(turnDateLayout, turnDateLayout), location), nil
}

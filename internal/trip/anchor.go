package trip

import (
	"context"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

// Anchors resolves jobs anchored to the start or end of one of their owner's trips.
type Anchors struct {
	repository Repository
}

var _ schedule.AnchorResolver = Anchors{}

// NewAnchors returns the trip anchor resolver over the repository.
func NewAnchors(repository Repository) Anchors { return Anchors{repository: repository} }

// ResolveAnchor finds the trip's start or end wherever it now is. A trip that is gone, cancelled, or
// dismissed is schedule.ErrAnchorGone.
func (a Anchors) ResolveAnchor(ctx context.Context, principalID string, anchor schedule.Anchor, _ time.Time, _ string) (schedule.AnchorOccurrence, error) {
	if anchor.Kind != schedule.AnchorTrip {
		return schedule.AnchorOccurrence{}, fmt.Errorf("trips cannot resolve a %s anchor", anchor.Kind)
	}
	trip, found, err := a.repository.LoadTrip(ctx, principalID, anchor.TripID)
	if err != nil {
		return schedule.AnchorOccurrence{}, err
	}
	if !found || trip.Status == StatusCancelled || trip.Status == StatusDismissed {
		return schedule.AnchorOccurrence{}, schedule.ErrAnchorGone
	}
	at := trip.StartsAt
	if anchor.Edge == schedule.EdgeEnd {
		at = trip.EndsAt
	}
	return schedule.AnchorOccurrence{Key: trip.ID + ":" + anchor.Edge, At: at, Label: trip.Title}, nil
}

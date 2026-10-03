// Package location holds a principal's latest shared position: what Telegram delivered, how long
// it stays usable, and the line that tells a turn where its user is. Only the latest position is
// kept, and nothing about it is ever logged.
package location

import (
	"context"
	"fmt"
	"math"
	"time"
)

type Source string

const (
	SourcePin   Source = "pin"
	SourceVenue Source = "venue"
	SourceLive  Source = "live"

	// MaximumAccuracy is Telegram's own bound on horizontal_accuracy, in metres.
	MaximumAccuracy = 1500
)

// Fix is a principal's latest position. A live fix has the message whose edits move it.
type Fix struct {
	PrincipalID  string
	Latitude     float64
	Longitude    float64
	Accuracy     float64
	Source       Source
	VenueTitle   string
	VenueAddress string
	// LiveMessageID is the live location message; zero for a pin or venue.
	LiveMessageID int64
	// LiveUntil is when Telegram stops a live share; zero for one shared until the user stops it.
	LiveUntil time.Time
	// EndedAt is when the user stopped a live share early; zero while it runs.
	EndedAt   time.Time
	UpdatedAt time.Time
}

// Validate checks a position that arrived from Telegram before anything stores it.
func (f Fix) Validate() error {
	if math.IsNaN(f.Latitude) || math.IsNaN(f.Longitude) || math.Abs(f.Latitude) > 90 || math.Abs(f.Longitude) > 180 {
		return fmt.Errorf("coordinates are out of range")
	}
	if math.IsNaN(f.Accuracy) || f.Accuracy < 0 || f.Accuracy > MaximumAccuracy {
		return fmt.Errorf("accuracy is out of range")
	}
	switch f.Source {
	case SourcePin, SourceVenue:
		if f.LiveMessageID != 0 {
			return fmt.Errorf("a %s has no live message", f.Source)
		}
	case SourceLive:
		if f.LiveMessageID <= 0 {
			return fmt.Errorf("a live location needs its message")
		}
	default:
		return fmt.Errorf("unknown source %q", f.Source)
	}
	if f.UpdatedAt.IsZero() {
		return fmt.Errorf("a position needs its time")
	}
	return nil
}

// Live reports whether the fix still follows a running live share at now.
func (f Fix) Live(now time.Time) bool {
	return f.Source == SourceLive && f.EndedAt.IsZero() && (f.LiveUntil.IsZero() || now.Before(f.LiveUntil))
}

type Repository interface {
	// PutLocation stores a newly shared position, replacing the principal's previous one.
	PutLocation(context.Context, Fix) error
	// MoveLiveLocation applies an edit of a live location message. It stores nothing when the
	// principal forgot that message, when another share has replaced it, or when the stored
	// position is newer, and reports whether it stored the fix.
	MoveLiveLocation(context.Context, Fix) (bool, error)
	LoadLocation(ctx context.Context, principalID string) (Fix, bool, error)
	ListLocations(context.Context) ([]Fix, error)
	// DeleteLocation removes the principal's position only if it is still the one updated at
	// updatedAt, so a prune cannot delete a position that moved meanwhile.
	DeleteLocation(ctx context.Context, principalID string, updatedAt time.Time) error
	// ForgetLocation removes the principal's position; while it follows a running live share, later
	// edits of that message are ignored for good, even once another position came and went. It
	// reports whether a position existed.
	ForgetLocation(ctx context.Context, principalID string, now time.Time) (bool, error)
}

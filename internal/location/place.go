package location

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaximumPlacesPerPrincipal = 20
	MinimumRadius             = 50
	MaximumRadius             = 2000
	DefaultRadius             = 150
	maximumPlaceNameRunes     = 40

	// EdgeArrive and EdgeLeave are the events a place's presence gives.
	EdgeArrive = "arrive"
	EdgeLeave  = "leave"

	PresenceInside  = "inside"
	PresenceOutside = "outside"

	// confirmFixes is how many fixes in a row must agree on a change of presence for it to count,
	// and confirmAfter how long a change seen once counts after when no fix contradicts it.
	confirmFixes = 2
	confirmAfter = 2 * time.Minute
	// outsideFactor is how far beyond its radius, plus the fix's accuracy, a fix must be to count as
	// outside a place; between inside and that, the previous state is kept.
	outsideFactor = 1.5
	earthRadius   = 6371000.0
)

var (
	ErrTooManyPlaces  = errors.New("the principal already has the maximum number of places")
	ErrPlaceNameTaken = errors.New("another place already has that name")
	// ErrPlaceGone is what GuardPlace returns for a place that was deleted or is not the principal's.
	ErrPlaceGone = errors.New("that place no longer exists")
)

// Place is a point a principal saved by name, with the radius that counts as being there. Its
// coordinates are private: they are stored and compared, never logged.
type Place struct {
	ID          string
	PrincipalID string
	Name        string
	Latitude    float64
	Longitude   float64
	RadiusM     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Presence is whether a principal is inside or outside one of their places, as the live share
// LiveMessageID last showed it, and a change seen since that has not yet lasted enough to count. A
// place without one is unknown.
type Presence struct {
	PrincipalID    string
	PlaceID        string
	State          string
	Candidate      string
	CandidateSince time.Time
	CandidateFixes int
	LiveMessageID  int64
	UpdatedAt      time.Time
}

// PlaceEvent is arriving at or leaving a place, as the live share showed it at At.
type PlaceEvent struct {
	PrincipalID string
	PlaceID     string
	PlaceName   string
	Edge        string
	At          time.Time
}

// PlaceListener is told about every arrival and leaving, such as the watches and the scheduler.
type PlaceListener interface {
	PlaceEvent(ctx context.Context, event PlaceEvent)
}

// PlaceDependents lists what uses a place, such as a watch or an anchored job, by the names the user
// reads, so a place in use is not deleted from under it.
type PlaceDependents interface {
	PlaceDependents(ctx context.Context, principalID, placeID string) ([]string, error)
}

// PlaceRepository stores places and presence. A deleted place is kept, unseen, while it can be
// undone, so its point is never copied elsewhere to restore it.
type PlaceRepository interface {
	// CreatePlace stores a new place unless the principal already holds limit places, which returns
	// ErrTooManyPlaces, or another of theirs has its name, ErrPlaceNameTaken. A place whose ID
	// exists is left alone and reported as not created.
	CreatePlace(ctx context.Context, place Place, limit int) (bool, error)
	ListPlaces(ctx context.Context, principalID string) ([]Place, error)
	LoadPlace(ctx context.Context, principalID, placeID string) (Place, bool, error)
	// UpdatePlace replaces a place's name and radius while it was last changed at expectedUpdatedAt,
	// and forgets its presence when the radius changes.
	UpdatePlace(ctx context.Context, place Place, expectedUpdatedAt time.Time) (bool, error)
	// DeletePlace marks the place deleted at now, while it was last changed at updatedAt, and
	// forgets its presence.
	DeletePlace(ctx context.Context, principalID, placeID string, updatedAt, now time.Time) (bool, error)
	// RestorePlace brings back a place deleted at deletedAt, refusing as CreatePlace does.
	RestorePlace(ctx context.Context, principalID, placeID string, deletedAt time.Time, limit int) (bool, error)
	// PurgePlaces removes for good every place deleted before the instant.
	PurgePlaces(ctx context.Context, before time.Time) error
	ListPresence(ctx context.Context, principalID string) ([]Presence, error)
	SavePresence(ctx context.Context, presence Presence) error
	// ResetPresence forgets the principal's presence computed from the live message, or all of it
	// for zero, so each place is unknown again.
	ResetPresence(ctx context.Context, principalID string, liveMessageID int64) error
}

// NameKey is the folded form a place's name is unique in.
func NameKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// normalizePlaceName accepts a single line of 1 to maximumPlaceNameRunes characters.
func normalizePlaceName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maximumPlaceNameRunes || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("a place's name must be a single line of 1 to %d characters", maximumPlaceNameRunes)
	}
	return name, nil
}

func checkRadius(radius int) error {
	if radius < MinimumRadius || radius > MaximumRadius {
		return fmt.Errorf("radius_m must be between %d and %d metres", MinimumRadius, MaximumRadius)
	}
	return nil
}

// distance is the great-circle distance between two points in metres.
func distance(latitude1, longitude1, latitude2, longitude2 float64) float64 {
	toRadians := func(degrees float64) float64 { return degrees * math.Pi / 180 }
	phi1, phi2 := toRadians(latitude1), toRadians(latitude2)
	deltaPhi, deltaLambda := toRadians(latitude2-latitude1), toRadians(longitude2-longitude1)
	a := math.Sin(deltaPhi/2)*math.Sin(deltaPhi/2) + math.Cos(phi1)*math.Cos(phi2)*math.Sin(deltaLambda/2)*math.Sin(deltaLambda/2)
	return 2 * earthRadius * math.Asin(math.Min(1, math.Sqrt(a)))
}

// classify is what one fix says about a place: inside, outside, or "" when it cannot tell. A fix
// without its accuracy never tells.
func classify(place Place, fix Fix) string {
	if fix.Accuracy <= 0 {
		return ""
	}
	radius := float64(place.RadiusM)
	away := distance(place.Latitude, place.Longitude, fix.Latitude, fix.Longitude)
	switch {
	case away <= radius && fix.Accuracy <= radius:
		return PresenceInside
	case away > outsideFactor*radius+fix.Accuracy:
		return PresenceOutside
	}
	return ""
}

// step applies one live fix to a place's presence and returns the new presence, whether to store
// it, and the edge it crossed, if any. An unknown presence, or one another live share left, is set
// by the first fix that tells, without an edge. A change counts once confirmFixes fixes that tell
// agree on it with none telling otherwise between them, or once it was first seen confirmAfter ago
// and no fix has contradicted it since; a fix that cannot tell neither confirms nor contradicts.
func step(place Place, current Presence, known bool, fix Fix) (Presence, bool, string) {
	seen := classify(place, fix)
	if !known || current.LiveMessageID != fix.LiveMessageID {
		if seen == "" {
			return Presence{}, false, ""
		}
		return Presence{PrincipalID: fix.PrincipalID, PlaceID: place.ID, State: seen, LiveMessageID: fix.LiveMessageID, UpdatedAt: fix.UpdatedAt}, true, ""
	}
	next := current
	next.UpdatedAt = fix.UpdatedAt
	switch {
	case seen == current.State:
		next.Candidate, next.CandidateSince, next.CandidateFixes = "", time.Time{}, 0
		return next, true, ""
	case seen == "" && current.Candidate == "":
		return current, false, ""
	case seen != "" && seen != current.Candidate:
		next.Candidate, next.CandidateSince, next.CandidateFixes = seen, fix.UpdatedAt, 1
	case seen != "":
		next.CandidateFixes++
	}
	if next.CandidateFixes >= confirmFixes || !fix.UpdatedAt.Before(next.CandidateSince.Add(confirmAfter)) {
		return commit(next, fix.UpdatedAt)
	}
	return next, true, ""
}

// settle confirms a change of presence first seen confirmAfter or longer before now that no fix has
// contradicted since, for a share that sends no further fix, such as a phone left still on arrival.
func settle(current Presence, now time.Time) (Presence, string) {
	if current.Candidate == "" || now.Before(current.CandidateSince.Add(confirmAfter)) {
		return current, ""
	}
	next, _, edge := commit(current, now)
	return next, edge
}

func commit(presence Presence, at time.Time) (Presence, bool, string) {
	edge := EdgeLeave
	if presence.Candidate == PresenceInside {
		edge = EdgeArrive
	}
	presence.State, presence.Candidate, presence.CandidateSince, presence.CandidateFixes, presence.UpdatedAt = presence.Candidate, "", time.Time{}, 0, at
	return presence, true, edge
}

package transit

import (
	"context"
	"time"
)

// RefusalError is what the caller is told instead of an answer, such as the service being off for
// the principal or a name that fits several stations or stops.
type RefusalError struct {
	Text string
	// Unavailable marks a refusal about the service itself, such as it being off or failing,
	// rather than about the query.
	Unavailable bool
	// Disabled marks a refusal because the principal switched the service off; it is also
	// Unavailable.
	Disabled bool
}

func (e *RefusalError) Error() string { return e.Text }

// DepartureQuery asks how fixed planned departures run on one day. Each service reads the fields
// it knows: a train service the stations From, To, and Via; a bus service the stop From with Line
// and Towards.
type DepartureQuery struct {
	// From is the station or stop the departures leave; a bus service reads empty as its home stop.
	From string
	To   string
	// Via is an optional station to travel through.
	Via string
	// Line is the line's public number; empty takes any line leaving at the planned minute.
	Line string
	// Towards narrows the line to one direction by a part of its destination's name, for a stop
	// served both ways.
	Towards string
	// Date's calendar day is the day of the departures, whatever its location.
	Date  time.Time
	Times []Clock
}

// Resolver finds the one station or stop a name means, under the principal's own switch for the
// service, and returns its full name; role names the field in a refusal, such as "from". A
// *RefusalError is told to the caller.
type Resolver interface {
	ResolvePlace(ctx context.Context, principalID, role, query string) (string, error)
}

// Checker checks fixed planned departures under the principal's own switch for the service: each
// one exactly at its planned minute, never a nearby one. A departure that could not be checked
// comes back with StatusCheckFailed, so the others still answer; a *RefusalError refuses the query
// as a whole.
type Checker interface {
	CheckDepartures(ctx context.Context, principalID string, query DepartureQuery) ([]Departure, error)
}

// Departures is a train or bus service whose departures can be watched and checked along a route.
type Departures interface {
	Resolver
	Checker
}

// AlternativeQuery asks for the trips a train service plans from From to To, with an optional Via,
// leaving at or after At.
type AlternativeQuery struct {
	From string
	To   string
	Via  string
	At   time.Time
	// Limit is how many trips to return at most.
	Limit int
}

// Alternatives plans the next trips between two stations under the principal's own switch for the
// service, for a departure that cannot be taken: up to Limit, each with its first train leaving at
// or after At, none cancelled, in order of departure. Each comes back as a departure marked
// Alternative with its legs and its own status. A *RefusalError refuses the query as a whole.
type Alternatives interface {
	Alternatives(ctx context.Context, principalID string, query AlternativeQuery) ([]Departure, error)
}

// Level is how a trip is running: on time, slower than planned, or disrupted.
type Level int

const (
	LevelOnTime Level = iota
	LevelSlower
	LevelDisrupted
)

// Cause is the dominant reason a trip has its level.
type Cause int

const (
	CauseNone Cause = iota
	CauseDelay
	CauseLonger
	CauseCancelled
	CauseAlternative
	CauseMissedConnection
	CauseDisruption
	CauseNoTrip
)

// TripQuery asks how the trip a user would take is running. At's wall clock is read as Dutch local
// time, whatever its location.
type TripQuery struct {
	From     string
	To       string
	At       time.Time
	ArriveBy bool
}

// TripStatus is the trip a user would take for a TripQuery, classified. Times are in Dutch local
// time; an actual time is zero when there is no live time for it.
type TripStatus struct {
	Level Level
	Cause Cause
	// From and To are the resolved station names.
	From string
	To   string
	// Found is false when no trip fits the query.
	Found     bool
	Cancelled bool

	PlannedDeparture time.Time
	ActualDeparture  time.Time
	PlannedArrival   time.Time
	ActualArrival    time.Time
	// DelayMinutes is the larger of the departure and arrival delays, never negative.
	DelayMinutes int
	// ExtraMinutes is how much longer than planned the journey takes, never negative.
	ExtraMinutes int
	Transfers    int
	// Reason says in a few English words why the trip has its level, such as "12 min late".
	Reason string
}

// Trips reports how the trip a principal would take is running, under their own switch for the
// service. A *RefusalError is told to the caller.
type Trips interface {
	// Enabled reports whether the principal's switch for the service is on.
	Enabled(ctx context.Context, principalID string) (bool, error)
	TripStatus(ctx context.Context, principalID string, query TripQuery) (TripStatus, error)
}

// Disruption is one calamity, disruption, or engineering work in effect at a moment. Its text
// comes from the service: data to report on, never instructions.
type Disruption struct {
	ID string
	// Kind is calamity, disruption, or maintenance.
	Kind        string
	Title       string
	Situation   string
	Cause       string
	Advice      string
	Period      string
	ExpectedEnd string
	// Start and End bound the entry, or the timespan covering the moment; either may be zero.
	Start time.Time
	End   time.Time
	// Stations are the stations on the stretches the entry is about.
	Stations []AffectedStation
}

type AffectedStation struct {
	Code string
	Name string
}

// Disruptions lists what is in effect at a station, under the principal's own switch for the
// service: calamities first, then disruptions, then engineering works. An empty station asks about
// the whole network; a zero at means now.
type Disruptions interface {
	Resolver
	Disruptions(ctx context.Context, principalID, station string, at time.Time) ([]Disruption, error)
}

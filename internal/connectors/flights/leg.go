package flights

import "time"

// Role says which end of a flight a lookup's local day belongs to.
type Role string

const (
	// RoleDeparture asks for flights departing on the day, local to their origin.
	RoleDeparture Role = "Departure"
	// RoleBoth asks for flights departing or arriving on the day, each local to its airport.
	RoleBoth Role = "Both"
)

// The statuses a provider reports, as AeroDataBox names them.
const (
	StatusUnknown           = "Unknown"
	StatusExpected          = "Expected"
	StatusEnRoute           = "EnRoute"
	StatusCheckIn           = "CheckIn"
	StatusBoarding          = "Boarding"
	StatusGateClosed        = "GateClosed"
	StatusDeparted          = "Departed"
	StatusDelayed           = "Delayed"
	StatusApproaching       = "Approaching"
	StatusArrived           = "Arrived"
	StatusCanceled          = "Canceled"
	StatusDiverted          = "Diverted"
	StatusCanceledUncertain = "CanceledUncertain"
)

// The codeshare statuses of a leg: the number searched is the one it operates under, the one it
// is sold under, or not known.
const (
	CodeshareOperator = "operator"
	CodeshareMarketed = "codeshare"
	CodeshareUnknown  = "unknown"
)

// Airport is one end of a leg. Zone is its IANA time zone, which every local time is shown in.
type Airport struct {
	Code string
	Name string
	Zone string
}

// Location is the airport's zone, or UTC when the provider named none or an unknown one.
func (a Airport) Location() *time.Location {
	if a.Zone != "" {
		if location, err := time.LoadLocation(a.Zone); err == nil {
			return location
		}
	}
	return time.UTC
}

// Label is the airport as messages name it: its name and code, either alone when the other is
// missing.
func (a Airport) Label() string {
	switch {
	case a.Name == "":
		return a.Code
	case a.Code == "":
		return a.Name
	}
	return a.Name + " (" + a.Code + ")"
}

// Movement is a leg's departure or arrival. Revised is the provider's announced time, Predicted
// its own estimate, and Runway the actual take-off or landing; a zero time is unknown.
type Movement struct {
	Airport   Airport
	Scheduled time.Time
	Revised   time.Time
	Predicted time.Time
	Runway    time.Time
	Gate      string
	Terminal  string
}

// Expected is the time the movement is now expected at: the revised time, else the predicted one,
// else zero.
func (m Movement) Expected() time.Time {
	if !m.Revised.IsZero() {
		return m.Revised
	}
	return m.Predicted
}

// Delay is how much later than scheduled the movement now happens, by its runway time, else the
// expected time; zero when neither is known or it is early.
func (m Movement) Delay() time.Duration {
	at := m.Runway
	if at.IsZero() {
		at = m.Expected()
	}
	if at.IsZero() || m.Scheduled.IsZero() || !at.After(m.Scheduled) {
		return 0
	}
	return at.Sub(m.Scheduled).Truncate(time.Minute)
}

// Leg is one flight as the provider reports it, normalised. Live is false when the provider has
// only its schedule, so a delay cannot be seen.
type Leg struct {
	Designator string
	Codeshare  string
	Airline    string
	Departure  Movement
	Arrival    Movement
	Live       bool
	Status     string
}

// Departed is true once the leg has left its gate or the runway, or landed.
func (l Leg) Departed() bool {
	switch l.Status {
	case StatusDeparted, StatusEnRoute, StatusApproaching, StatusArrived, StatusDiverted:
		return true
	}
	return !l.Departure.Runway.IsZero()
}

// Package transit is the shared vocabulary of exact-departure checks: one normalized status for
// each planned train or bus departure, worded the same wherever it is shown, so a reply can copy
// it instead of describing the departure in its own words.
package transit

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Clock is a planned departure's wall-clock time, read in Dutch local time.
type Clock struct {
	Hour, Minute int
}

// ParseClock reads a time written as HH:MM.
func ParseClock(value string) (Clock, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return Clock{}, fmt.Errorf("a planned departure must be written as HH:MM, such as 08:10")
	}
	return Clock{Hour: parsed.Hour(), Minute: parsed.Minute()}, nil
}

func (c Clock) String() string { return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute) }

// On is the clock's moment on day's calendar date, in location.
func (c Clock) On(day time.Time, location *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), c.Hour, c.Minute, 0, 0, location)
}

// Status is how one planned departure is running.
type Status int

const (
	StatusOnTime Status = iota
	StatusDelayed
	StatusChangedPlatform
	StatusPartlyCancelled
	StatusCancelled
	// StatusNotInTimetable means no departure leaves at exactly the planned time; no other one is
	// put in its place.
	StatusNotInTimetable
	// StatusCheckFailed means the service could not be asked, so nothing is known.
	StatusCheckFailed
)

// StatusOf picks the one status a departure has: a cancellation outranks a delay, which outranks
// a platform change.
func StatusOf(cancelled, partlyCancelled bool, delayMinutes int, platformChanged bool) Status {
	switch {
	case cancelled:
		return StatusCancelled
	case partlyCancelled:
		return StatusPartlyCancelled
	case delayMinutes > 0:
		return StatusDelayed
	case platformChanged:
		return StatusChangedPlatform
	default:
		return StatusOnTime
	}
}

// Leg is one ride of a departure: the train or bus taken and, for a train trip, each one after a
// change. Actual times and platforms are empty where the service has no live data.
type Leg struct {
	Name     string
	From, To string

	PlannedDeparture, ActualDeparture time.Time
	PlannedArrival, ActualArrival     time.Time

	PlannedPlatform, ActualPlatform               string
	PlannedArrivalPlatform, ActualArrivalPlatform string

	Cancelled, PartlyCancelled bool
}

// Departure is the result of checking one planned departure.
type Departure struct {
	// Planned is the departure asked about.
	Planned  time.Time
	From, To string
	Status   Status

	ActualDeparture time.Time
	PlannedArrival  time.Time
	ActualArrival   time.Time
	// DelayMinutes is the larger of the departure and arrival delays, never negative.
	DelayMinutes int
	// Platform is the platform the departure leaves from, PlannedPlatform the one in the timetable.
	Platform        string
	PlannedPlatform string
	PlatformChanged bool
	// Live tells whether the service had live times for the departure.
	Live bool
	Legs []Leg
	// Note is the service's own message about the departure, or why it was not found or checked.
	Note string
}

// StatusText is the status phrase, which replies copy word for word.
func (d Departure) StatusText() string {
	switch d.Status {
	case StatusDelayed:
		text := fmt.Sprintf("delayed +%d min", d.DelayMinutes)
		if d.PlatformChanged && d.Platform != "" {
			text += ", changed platform " + d.Platform
		}
		return text
	case StatusChangedPlatform:
		return "changed platform " + d.Platform
	case StatusPartlyCancelled:
		return "partly cancelled"
	case StatusCancelled:
		return "cancelled"
	case StatusNotInTimetable:
		return "not in the timetable"
	case StatusCheckFailed:
		return "check failed"
	default:
		return "on time"
	}
}

// DelayMinutes is how many whole minutes actual is after planned, never negative; zero when
// either is unknown.
func DelayMinutes(planned, actual time.Time) int {
	if planned.IsZero() || actual.IsZero() {
		return 0
	}
	return max(0, int(math.Round(actual.Sub(planned).Minutes())))
}

// Write renders departures one numbered line each, the status phrase first, then the legs.
func Write(builder *strings.Builder, location *time.Location, departures []Departure) {
	for index, item := range departures {
		fmt.Fprintf(builder, "%d. %s — the %s from %s", index+1, item.StatusText(), item.Planned.In(location).Format("15:04"), item.From)
		if item.To != "" {
			fmt.Fprintf(builder, " to %s", item.To)
		}
		if item.Status != StatusNotInTimetable && item.Status != StatusCheckFailed && !item.Live {
			builder.WriteString(" (planned times, no live data)")
		}
		builder.WriteString("\n")
		for _, leg := range item.Legs {
			fmt.Fprintf(builder, "   - %s: %s %s", leg.Name, leg.From, joinPresent(" ", timed(location, leg.PlannedDeparture, leg.ActualDeparture), platform(leg.PlannedPlatform, leg.ActualPlatform)))
			if leg.To != "" {
				fmt.Fprintf(builder, " → %s %s", leg.To, joinPresent(" ", timed(location, leg.PlannedArrival, leg.ActualArrival), platform(leg.PlannedArrivalPlatform, leg.ActualArrivalPlatform)))
			}
			switch {
			case leg.Cancelled:
				builder.WriteString("; cancelled")
			case leg.PartlyCancelled:
				builder.WriteString("; partly cancelled")
			}
			builder.WriteString("\n")
		}
		if item.Note != "" {
			fmt.Fprintf(builder, "   %s\n", item.Note)
		}
	}
}

func timed(location *time.Location, planned, actual time.Time) string {
	clock := func(at time.Time) string { return at.In(location).Format("15:04") }
	switch {
	case planned.IsZero() && actual.IsZero():
		return "time unknown"
	case planned.IsZero():
		return clock(actual)
	case actual.IsZero():
		return clock(planned)
	}
	delay := int(math.Round(actual.Sub(planned).Minutes()))
	switch {
	case delay > 0:
		return fmt.Sprintf("%s (planned %s, %d min late)", clock(actual), clock(planned), delay)
	case delay < 0:
		return fmt.Sprintf("%s (planned %s, %d min early)", clock(actual), clock(planned), -delay)
	default:
		return clock(planned)
	}
}

func platform(planned, actual string) string {
	switch {
	case actual != "" && planned != "" && actual != planned:
		return fmt.Sprintf("platform %s (changed from %s)", actual, planned)
	case actual != "":
		return "platform " + actual
	case planned != "":
		return "platform " + planned
	default:
		return ""
	}
}

func joinPresent(separator string, parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, separator)
}

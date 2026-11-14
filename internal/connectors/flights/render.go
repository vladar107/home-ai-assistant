package flights

import (
	"fmt"
	"strings"
	"time"
)

const (
	dayLayout   = "Mon 2 Jan"
	clockLayout = "15:04"

	// delayedFrom is how many minutes late a departure must be for the tool to call it delayed.
	delayedFrom = 5 * time.Minute
)

// StatusPhrase is how a leg's status opens its block: cancelled, may be cancelled, diverted,
// landed, departed, schedule only, delayed, or on time, in that order of precedence.
func StatusPhrase(leg Leg) string {
	switch {
	case leg.Status == StatusCanceled:
		return "cancelled"
	case leg.Status == StatusCanceledUncertain:
		return "may be cancelled"
	case leg.Status == StatusDiverted:
		return "diverted"
	case leg.Status == StatusArrived || !leg.Arrival.Runway.IsZero():
		return "landed"
	case leg.Departed():
		return "departed"
	case !leg.Live:
		return "schedule only, no live data"
	case leg.Departure.Delay() >= delayedFrom:
		return fmt.Sprintf("delayed +%d min", int(leg.Departure.Delay().Minutes()))
	}
	return "on time"
}

// render writes each leg as one block, in each airport's local time, and ends with the source.
func render(asked string, legs []Leg) string {
	blocks := make([]string, 0, len(legs)+1)
	for _, leg := range legs {
		blocks = append(blocks, renderLeg(asked, leg))
	}
	blocks = append(blocks, "Times are local to each airport.\n"+Attribution)
	return strings.Join(blocks, "\n\n")
}

func renderLeg(asked string, leg Leg) string {
	name := asked
	if leg.Codeshare == CodeshareMarketed && leg.Airline != "" {
		name += " (operated by " + leg.Airline + ")"
	}
	lines := []string{
		StatusPhrase(leg),
		name + " " + leg.Departure.Airport.Label() + " → " + leg.Arrival.Airport.Label(),
		"Departure: " + movementLine(leg.Departure),
		"Arrival: " + movementLine(leg.Arrival),
	}
	return strings.Join(lines, "\n")
}

// movementLine is a departure or arrival: scheduled, then expected or actual, gate, and terminal
// when known, each time in the airport's zone with the day when it is not the scheduled one.
func movementLine(movement Movement) string {
	location := movement.Airport.Location()
	scheduled := movement.Scheduled.In(location)
	parts := []string{"scheduled " + scheduled.Format(dayLayout+" "+clockLayout)}
	at := func(instant time.Time) string {
		local := instant.In(location)
		if local.Format(time.DateOnly) == scheduled.Format(time.DateOnly) {
			return local.Format(clockLayout)
		}
		return local.Format(dayLayout + " " + clockLayout)
	}
	switch {
	case !movement.Runway.IsZero():
		parts = append(parts, "actual "+at(movement.Runway))
	case !movement.Expected().IsZero():
		parts = append(parts, "expected "+at(movement.Expected()))
	}
	if movement.Gate != "" {
		parts = append(parts, "gate "+movement.Gate)
	}
	if movement.Terminal != "" {
		parts = append(parts, "terminal "+movement.Terminal)
	}
	return strings.Join(parts, ", ")
}

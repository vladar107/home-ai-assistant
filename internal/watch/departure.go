package watch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	serviceNS  = "ns"
	serviceBus = "bus"

	// delayStep is how many more minutes past the threshold a delay must grow before it counts as
	// a new state, so a delay creeping up a minute at a time alerts once per step, not per minute.
	delayStep = 5
	// dutchZone is where NS and the bus services read planned times.
	dutchZone = "Europe/Amsterdam"
)

// DepartureEvaluator checks departure watches: one planned train or bus through the train or bus
// service, each under the owner's own trains or buses switch.
type DepartureEvaluator struct {
	trains transit.Departures
	buses  transit.Departures
	dutch  *time.Location
}

var (
	_ Evaluator = (*DepartureEvaluator)(nil)
	_ Resolver  = (*DepartureEvaluator)(nil)
)

// NewDepartureEvaluator checks trains with trains and buses with buses; either may be nil, and a
// watch of that mode then fails its checks and cannot be proposed.
func NewDepartureEvaluator(trains, buses transit.Departures) (*DepartureEvaluator, error) {
	if trains == nil && buses == nil {
		return nil, fmt.Errorf("departure watches need a train or a bus service")
	}
	dutch, err := time.LoadLocation(dutchZone)
	if err != nil {
		return nil, fmt.Errorf("load %s time zone: %w", dutchZone, err)
	}
	return &DepartureEvaluator{trains: trains, buses: buses, dutch: dutch}, nil
}

// Resolve refuses a mode the owner has switched off or this service cannot check, and a station or
// stop name that does not mean exactly one; train stations come back with their full names.
func (e *DepartureEvaluator) Resolve(ctx context.Context, principalID string, spec Spec) (Spec, error) {
	target := &spec.Target
	switch target.Mode {
	case ModeTrain:
		if e.trains == nil {
			return Spec{}, fmt.Errorf("train departures cannot be watched here")
		}
		for _, field := range []struct {
			role  string
			value *string
		}{{"from", &target.From}, {"to", &target.To}, {"via", &target.Via}} {
			if *field.value == "" {
				continue
			}
			name, err := e.trains.ResolvePlace(ctx, principalID, field.role, *field.value)
			if err != nil {
				return Spec{}, err
			}
			*field.value = name
		}
		if target.From == target.To {
			return Spec{}, fmt.Errorf("from and to are both %s", target.From)
		}
	case ModeBus:
		if e.buses == nil {
			return Spec{}, fmt.Errorf("bus departures cannot be watched here")
		}
		if _, err := e.buses.ResolvePlace(ctx, principalID, "stop", target.Stop); err != nil {
			return Spec{}, err
		}
	}
	return spec, nil
}

// Evaluate checks the watch's one planned departure. A departure that could not be checked is an
// upstream failure; one that is no longer in the timetable is a state that always alerts, since
// the train or bus the user counts on is gone, or the watch names a time it never leaves at.
func (e *DepartureEvaluator) Evaluate(ctx context.Context, principalID string, watch Watch, occurrence Occurrence) (Observation, error) {
	departure, err := e.check(ctx, principalID, watch.Target, occurrence)
	if err != nil {
		return Observation{}, err
	}
	return departureObservation(watch.Spec, departure, occurrence.End.Location()), nil
}

func (e *DepartureEvaluator) check(ctx context.Context, principalID string, target Target, occurrence Occurrence) (transit.Departure, error) {
	planned := occurrence.End.In(e.dutch)
	clock := transit.Clock{Hour: planned.Hour(), Minute: planned.Minute()}
	var service string
	var parts []string
	var checker transit.Checker
	var query transit.DepartureQuery
	switch target.Mode {
	case ModeTrain:
		if e.trains == nil {
			return transit.Departure{}, fmt.Errorf("train departures cannot be checked here")
		}
		service, parts, checker = serviceNS, []string{target.From, target.To, target.Via}, e.trains
		query = transit.DepartureQuery{From: target.From, To: target.To, Via: target.Via}
	case ModeBus:
		if e.buses == nil {
			return transit.Departure{}, fmt.Errorf("bus departures cannot be checked here")
		}
		service, parts, checker = serviceBus, []string{target.Stop, target.Line}, e.buses
		query = transit.DepartureQuery{From: target.Stop, Line: target.Line}
	default:
		return transit.Departure{}, fmt.Errorf("unknown departure mode %q", target.Mode)
	}
	query.Date, query.Times = planned, []transit.Clock{clock}
	fetch := func(ctx context.Context) ([]transit.Departure, error) {
		return checker.CheckDepartures(ctx, principalID, query)
	}
	key := strings.Join(append([]string{KindDeparture, target.Mode, principalID, planned.Format(time.RFC3339)}, parts...), "\x00")
	departures, err := Cached(ctx, occurrence.Cache, key, fetch)
	if err != nil {
		return transit.Departure{}, connectorError(service, err)
	}
	if len(departures) != 1 {
		return transit.Departure{}, fmt.Errorf("%s answered %d departures for one", service, len(departures))
	}
	departure := departures[0]
	if departure.Status == transit.StatusCheckFailed {
		return transit.Departure{}, &UpstreamError{Service: service, Err: errors.New(departure.Note)}
	}
	return departure, nil
}

// connectorError marks a service's refusal about itself as an upstream failure; a switch the
// owner turned off, or a name that no longer resolves, fails the check as it is.
func connectorError(service string, err error) error {
	var refusal *transit.RefusalError
	if errors.As(err, &refusal) && refusal.Unavailable && !refusal.Disabled {
		return &UpstreamError{Service: service, Err: err}
	}
	return err
}

// departureObservation normalizes a checked departure to the watch's triggers. A delay under the
// threshold is on time, and over it the state moves only in steps of delayStep minutes; what the
// watch does not alert on is left out of the state, except a cancellation, which is stored
// silently so it is never reported as "back to normal".
func departureObservation(spec Spec, departure transit.Departure, location *time.Location) Observation {
	watched := func(trigger string) bool { return slices.Contains(spec.Triggers, trigger) }
	observation := Observation{Headline: departureHeadline(departure, location), Details: departureDetails(departure, location)}
	var parts []string
	switch {
	case departure.Status == transit.StatusNotInTimetable:
		parts = []string{"not_in_timetable"}
	case departure.Status == transit.StatusCancelled:
		parts = []string{"cancelled"}
		observation.Silent = !watched(TriggerCancelled)
	default:
		if departure.Status == transit.StatusPartlyCancelled && watched(TriggerPartlyCancelled) {
			parts = append(parts, "partly_cancelled")
		}
		if watched(TriggerDelay) && departure.DelayMinutes >= spec.DelayMinutes {
			parts = append(parts, fmt.Sprintf("delay>=%d", delayBucket(spec.DelayMinutes, departure.DelayMinutes)))
		}
		if watched(TriggerPlatform) && departure.PlatformChanged {
			parts = append(parts, "platform="+departure.Platform)
		}
	}
	if len(parts) == 0 {
		observation.State = "on_time"
		return observation
	}
	observation.State = strings.Join(parts, "|")
	observation.Triggered = true
	return observation
}

// delayBucket is the delay rounded down to a step of delayStep minutes past the threshold.
func delayBucket(threshold, delay int) int {
	return threshold + (delay-threshold)/delayStep*delayStep
}

// departureHeadline is the departure with its status phrase word for word, such as
// "07:42 Den Haag Centraal → Zwolle: cancelled".
func departureHeadline(departure transit.Departure, location *time.Location) string {
	route := departure.From
	if departure.To != "" {
		route += " → " + departure.To
	}
	headline := fmt.Sprintf("%s %s: %s", departure.Planned.In(location).Format("15:04"), route, departure.StatusText())
	if departure.Status != transit.StatusNotInTimetable && !departure.Live {
		headline += " (planned times, no live data)"
	}
	return headline
}

// departureDetails are the legs and the service's note, as the departure checks write them.
func departureDetails(departure transit.Departure, location *time.Location) []string {
	var builder strings.Builder
	transit.Write(&builder, location, []transit.Departure{departure})
	lines := strings.Split(strings.TrimRight(builder.String(), "\n"), "\n")
	details := make([]string, 0, len(lines))
	for _, line := range lines[1:] {
		details = append(details, strings.TrimSpace(line))
	}
	return details
}

package watch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/connectors/bus"
	"github.com/vladar107/home-ai-assistant/internal/connectors/ns"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const onTrack = "on_time"

// RouteEvaluator checks route watches: the owner's saved route checked leg by leg for the
// occurrence's day, under the owner's own trains and buses switches.
type RouteEvaluator struct {
	routes *routes.Service
}

var (
	_ Evaluator = (*RouteEvaluator)(nil)
	_ Resolver  = (*RouteEvaluator)(nil)
)

func NewRouteEvaluator(service *routes.Service) (*RouteEvaluator, error) {
	if service == nil {
		return nil, fmt.Errorf("route watches need the routes service")
	}
	return &RouteEvaluator{routes: service}, nil
}

// Resolve refuses a route the owner has not saved, listing the ones they have, and stores the
// route under its saved name.
func (e *RouteEvaluator) Resolve(ctx context.Context, principalID string, spec Spec) (Spec, error) {
	route, err := e.routes.Find(ctx, principalID, spec.Target.Route)
	if err != nil {
		return Spec{}, err
	}
	spec.Target.Route = route.Name
	return spec, nil
}

// Evaluate checks the route for the occurrence's day, the calendar day of its window in the
// watch's zone, whatever the routes service's zone. A route that is gone fails the check as it
// is, while every train and bus leg failing to check is an upstream failure; a partial failure is
// told in the details and leaves the state of an alert already sent as it was.
func (e *RouteEvaluator) Evaluate(ctx context.Context, principalID string, watch Watch, occurrence Occurrence) (Observation, error) {
	key := strings.Join([]string{KindRoute, principalID, strings.ToLower(watch.Target.Route), occurrence.Date}, "\x00")
	result, err := Cached(ctx, occurrence.Cache, key, func(ctx context.Context) (routes.Result, error) {
		return e.routes.Check(ctx, principalID, watch.Target.Route, occurrence.Start)
	})
	if err != nil {
		return Observation{}, err
	}
	if err := allLegsFailed(result); err != nil {
		return Observation{}, err
	}
	return e.observation(watch.Spec, result, occurrence.Previous), nil
}

// allLegsFailed is an upstream failure when no departure of any train or bus leg could be
// checked, unless that is only because the owner switched trains and buses off.
func allLegsFailed(result routes.Result) error {
	var notes []string
	for _, leg := range result.Legs {
		for _, departure := range leg.Departures {
			if departure.Status != transit.StatusCheckFailed {
				return nil
			}
			notes = append(notes, departure.Note)
		}
	}
	if len(notes) == 0 {
		return nil
	}
	if !slices.ContainsFunc(notes, func(note string) bool { return note != ns.DisabledText && note != bus.DisabledText }) {
		return errors.New(notes[0])
	}
	return &UpstreamError{Service: "routes", Err: fmt.Errorf("no leg of the route %q could be checked", result.Route.Name)}
}

// observation normalizes a checked route to the watch's triggers. A leg counts by the departure
// the route takes: a delay in steps as a departure watch has them, a platform change, or no
// departure left to take. The arrival counts as missed when it is late or when a leg leaves no way
// to get there. Legs that failed to check are not part of the state, and while some are failing an
// alert already sent is not called off.
func (e *RouteEvaluator) observation(spec Spec, result routes.Result, previous string) Observation {
	watched := func(trigger string) bool { return slices.Contains(spec.Triggers, trigger) }
	var parts, phrases []string
	if watched(TriggerLeg) {
		for index, leg := range result.Legs {
			if leg.Leg.Mode != routes.ModeTrain && leg.Leg.Mode != routes.ModeBus {
				continue
			}
			number := index + 1
			switch {
			case leg.Unusable():
				phrase := leg.Status
				if allCancelled(leg.Departures) {
					phrase = "cancelled"
				}
				parts = append(parts, fmt.Sprintf("leg%d:unusable", number))
				phrases = append(phrases, fmt.Sprintf("leg %d %s", number, phrase))
			case leg.Taken >= 0:
				taken := leg.Departures[leg.Taken]
				var legParts []string
				if taken.DelayMinutes >= spec.DelayMinutes {
					legParts = append(legParts, fmt.Sprintf("delay>=%d", delayBucket(spec.DelayMinutes, taken.DelayMinutes)))
				}
				if taken.PlatformChanged {
					legParts = append(legParts, "platform="+taken.Platform)
				}
				if len(legParts) > 0 {
					parts = append(parts, fmt.Sprintf("leg%d:%s", number, strings.Join(legParts, ",")))
					phrases = append(phrases, fmt.Sprintf("leg %d %s", number, taken.StatusText()))
				}
			}
		}
	}
	if watched(TriggerArrival) && arrivalMissed(result) {
		parts = append(parts, "arrival:missed")
		phrases = append(phrases, "arrival target missed")
	}

	observation := Observation{Details: e.details(result)}
	if len(parts) == 0 {
		observation.State = onTrack
		if len(result.FailedLegs) > 0 && previous != "" && !strings.HasPrefix(previous, onTrack) {
			observation.State, observation.Triggered = previous, true
		}
		observation.Headline = fmt.Sprintf("Route %q: on track", result.Route.Name)
		return observation
	}
	observation.State = strings.Join(parts, "|")
	observation.Triggered = true
	observation.Headline = fmt.Sprintf("Route %q: %s", result.Route.Name, strings.Join(phrases, "; "))
	return observation
}

func arrivalMissed(result routes.Result) bool {
	switch result.Verdict {
	case routes.VerdictMissed:
		return true
	case routes.VerdictUnknown:
		return slices.ContainsFunc(result.Legs, routes.LegResult.Unusable)
	}
	return false
}

func allCancelled(departures []transit.Departure) bool {
	return len(departures) > 0 && !slices.ContainsFunc(departures, func(departure transit.Departure) bool {
		return departure.Status != transit.StatusCancelled
	})
}

// details are the lines of the route check as the routes package writes them, without its
// heading.
func (e *RouteEvaluator) details(result routes.Result) []string {
	lines := strings.Split(e.routes.Render(result), "\n")
	details := make([]string, 0, len(lines))
	for _, line := range lines[1:] {
		if line = strings.TrimSpace(line); line != "" {
			details = append(details, line)
		}
	}
	return details
}

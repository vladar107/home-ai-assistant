package bus

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	ovapiTimeLayout = "2006-01-02T15:04:05"

	statusPassed    = "PASSED"
	statusCancelled = "CANCEL"
	stopTypeFirst   = "FIRST"
	stopTypeLast    = "LAST"
)

// pass is one vehicle's call at one platform, as OVapi reports it.
type pass struct {
	OperatorCode          string `json:"OperatorCode"`
	OperationDate         string `json:"OperationDate"`
	LinePlanningNumber    string `json:"LinePlanningNumber"`
	LinePublicNumber      string `json:"LinePublicNumber"`
	JourneyNumber         int    `json:"JourneyNumber"`
	FortifyOrderNumber    int    `json:"FortifyOrderNumber"`
	UserStopOrderNumber   int    `json:"UserStopOrderNumber"`
	DestinationName50     string `json:"DestinationName50"`
	TargetArrivalTime     string `json:"TargetArrivalTime"`
	ExpectedArrivalTime   string `json:"ExpectedArrivalTime"`
	TargetDepartureTime   string `json:"TargetDepartureTime"`
	ExpectedDepartureTime string `json:"ExpectedDepartureTime"`
	TripStopStatus        string `json:"TripStopStatus"`
	TransportType         string `json:"TransportType"`
	TimingPointCode       string `json:"TimingPointCode"`
	JourneyStopType       string `json:"JourneyStopType"`
}

type timingPoint struct {
	Passes map[string]pass `json:"Passes"`
}

// call is a pass read in local time, with the journey key that ties its calls at other stops.
type call struct {
	key               string
	pass              pass
	plannedDeparture  time.Time
	expectedDeparture time.Time
	plannedArrival    time.Time
	expectedArrival   time.Time
	platform          string
}

func (c call) cancelled() bool { return strings.EqualFold(c.pass.TripStopStatus, statusCancelled) }

func (c call) departureDelay() int { return delayMinutes(c.plannedDeparture, c.expectedDeparture) }

func (c call) arrivalDelay() int { return delayMinutes(c.plannedArrival, c.expectedArrival) }

// leaving is when the vehicle is expected to leave, falling back to the plan.
func (c call) leaving() time.Time {
	if !c.expectedDeparture.IsZero() {
		return c.expectedDeparture
	}
	return c.plannedDeparture
}

func (c call) reaching() time.Time {
	if !c.expectedArrival.IsZero() {
		return c.expectedArrival
	}
	return c.plannedArrival
}

// passes asks OVapi for every call at the given platforms, cached briefly by the code set.
func (s *Service) passes(ctx context.Context, codes []string) (map[string]timingPoint, error) {
	sorted := append([]string(nil), codes...)
	sort.Strings(sorted)
	joined := strings.Join(sorted, ",")
	key := "tpc|" + joined
	if cached, found := s.cached(key); found {
		return cached.(map[string]timingPoint), nil
	}
	var response map[string]timingPoint
	if err := s.ovapi.getJSON(ctx, "tpc/"+joined, nil, &response); err != nil {
		return nil, err
	}
	s.store(key, response, tpcLifetime)
	return response, nil
}

// calls reads the passes at one stop's platforms, keyed by journey.
func (s *Service) calls(points map[string]timingPoint, group *stopGroup) map[string]call {
	found := make(map[string]call)
	for _, code := range group.codes {
		for key, item := range points[code].Passes {
			entry := call{
				key:               key,
				pass:              item,
				plannedDeparture:  s.parseLocal(item.TargetDepartureTime),
				expectedDeparture: s.parseLocal(item.ExpectedDepartureTime),
				plannedArrival:    s.parseLocal(item.TargetArrivalTime),
				expectedArrival:   s.parseLocal(item.ExpectedArrivalTime),
				platform:          group.platforms[code],
			}
			// A journey that loops calls at a stop twice; the earlier call is the one to catch.
			if existing, seen := found[key]; seen && existing.pass.UserStopOrderNumber < item.UserStopOrderNumber {
				continue
			}
			found[key] = entry
		}
	}
	return found
}

// departuresFrom lists the calls at a stop that are still to leave at or after at, soonest first.
func departuresFrom(calls map[string]call, at time.Time) []call {
	var list []call
	for _, entry := range calls {
		if strings.EqualFold(entry.pass.TripStopStatus, statusPassed) || strings.EqualFold(entry.pass.JourneyStopType, stopTypeLast) {
			continue
		}
		if entry.leaving().Before(at) {
			continue
		}
		list = append(list, entry)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].leaving().Equal(list[j].leaving()) {
			return list[i].leaving().Before(list[j].leaving())
		}
		return list[i].key < list[j].key
	})
	return list
}

// directTrip is one journey that calls at the origin and, later, at the destination.
type directTrip struct {
	from call
	to   call
}

func directTrips(origin, destination map[string]call, at time.Time) []directTrip {
	var trips []directTrip
	for _, from := range departuresFrom(origin, at) {
		to, found := destination[from.key]
		if !found || to.pass.UserStopOrderNumber <= from.pass.UserStopOrderNumber || strings.EqualFold(to.pass.JourneyStopType, stopTypeFirst) {
			continue
		}
		trips = append(trips, directTrip{from: from, to: to})
	}
	return trips
}

func (s *Service) parseLocal(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.ParseInLocation(ovapiTimeLayout, value, s.location)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func delayMinutes(planned, expected time.Time) int {
	if planned.IsZero() || expected.IsZero() {
		return 0
	}
	return int(math.Round(expected.Sub(planned).Minutes()))
}

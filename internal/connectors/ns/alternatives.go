package ns

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	// MaximumAlternatives is how many trips one alternatives query returns at most.
	MaximumAlternatives = 6
	// shortTransferMinutes is the change between trains short enough that NS is also asked for the
	// trips it plans with extra transfer time.
	shortTransferMinutes = 2
)

// Alternatives plans the next trips under the principal's own trains switch: those whose first
// train leaves at or after At, none cancelled. When the first one leaves 2 minutes or less to change
// between trains, NS is also asked with the first extra transfer time its departure checks use:
// a trip from that plan whose first train is new is included, noted as planned with it, and one
// whose first train a plain trip already takes is told on that trip as the connection if the change
// is missed.
func (s *Service) Alternatives(ctx context.Context, principalID string, query transit.AlternativeQuery) ([]transit.Departure, error) {
	if query.Limit < 1 || query.Limit > MaximumAlternatives {
		return nil, &transit.RefusalError{Text: fmt.Sprintf("ask for 1 to %d alternatives.", MaximumAlternatives)}
	}
	from, to, via, hasVia, err := s.journeyStations(ctx, principalID, query.From, query.To, query.Via)
	if err != nil {
		return nil, err
	}
	at := query.At.In(s.location).Truncate(time.Minute)
	response, err := s.planTrips(ctx, from, to, via, hasVia, false, at, 0)
	if err != nil {
		s.logger.InfoContext(ctx, "ns.request.failed", "principal_id", principalID, "tool", "alternatives", "error_class", errorClass(err))
		return nil, &transit.RefusalError{Text: modelText(err), Unavailable: true}
	}
	found, short := s.upcomingTrips(response, from, to, at, 0)
	if short {
		changeTime := changeTimeSteps[1]
		if response, err := s.planTrips(ctx, from, to, via, hasVia, false, at, changeTime); err != nil {
			s.logger.InfoContext(ctx, "ns.request.failed", "principal_id", principalID, "tool", "alternatives", "error_class", errorClass(err))
		} else {
			longer, _ := s.upcomingTrips(response, from, to, at, changeTime)
			found = s.mergeLonger(found, longer)
		}
	}
	slices.SortStableFunc(found, func(a, b transit.Departure) int {
		return cmp.Or(a.Planned.Compare(b.Planned), a.PlannedArrival.Compare(b.PlannedArrival))
	})
	return found[:min(len(found), query.Limit)], nil
}

// upcomingTrips are the plan's trips whose first train leaves at or after at, as alternatives,
// leaving out cancelled ones; short reports that the first of them has a change between trains of
// shortTransferMinutes or less.
func (s *Service) upcomingTrips(response tripsResponse, from, to station, at time.Time, changeTime int) ([]transit.Departure, bool) {
	var found []transit.Departure
	short := false
	for _, item := range response.Trips {
		first, ok := firstRide(item)
		if !ok {
			continue
		}
		planned, ok := parseTime(first.Origin.PlannedDateTime)
		if !ok || planned.Before(at) {
			continue
		}
		departure := departureFrom(transit.Departure{Planned: planned.In(s.location), From: from.name, To: to.name, Alternative: true}, item, changeTime, s.location)
		if departure.Status == transit.StatusCancelled || departure.Status == transit.StatusPartlyCancelled {
			continue
		}
		if len(found) == 0 {
			short = shortTransfer(item)
		}
		found = append(found, departure)
	}
	return found, short
}

// shortTransfer reports whether the trip changes between two trains with shortTransferMinutes or
// less between arriving and leaving, by live times where NS has them.
func shortTransfer(item trip) bool {
	moment := func(at stop) (time.Time, bool) {
		if actual, ok := parseTime(at.ActualDateTime); ok {
			return actual, true
		}
		return parseTime(at.PlannedDateTime)
	}
	for index := 1; index < len(item.Legs); index++ {
		previous, next := item.Legs[index-1], item.Legs[index]
		if previous.TravelType == travelTypeWalk || next.TravelType == travelTypeWalk {
			continue
		}
		arrives, arrived := moment(previous.Destination)
		leaves, left := moment(next.Origin)
		if arrived && left && leaves.Sub(arrives) <= shortTransferMinutes*time.Minute {
			return true
		}
	}
	return false
}

// sameTrip reports whether two planned trips take the same trains at the same times.
func sameTrip(a, b transit.Departure) bool {
	if !a.Planned.Equal(b.Planned) || !a.PlannedArrival.Equal(b.PlannedArrival) || len(a.Legs) != len(b.Legs) {
		return false
	}
	for index := range a.Legs {
		if a.Legs[index].Name != b.Legs[index].Name || !a.Legs[index].PlannedDeparture.Equal(b.Legs[index].PlannedDeparture) {
			return false
		}
	}
	return true
}

// mergeLonger adds the trips planned with extra transfer time to the plain ones. A trip whose first
// train a plain trip takes too is not its own alternative, so the limit counts distinct first
// trains; its later connection is noted on the plain trip instead.
func (s *Service) mergeLonger(found, longer []transit.Departure) []transit.Departure {
	plain := len(found)
	noted := make(map[int]bool)
	for _, candidate := range longer {
		if slices.ContainsFunc(found, func(known transit.Departure) bool { return sameTrip(known, candidate) }) {
			continue
		}
		sibling := slices.IndexFunc(found[:plain], func(known transit.Departure) bool { return sameFirstTrain(known, candidate) })
		if sibling < 0 {
			found = append(found, candidate)
			continue
		}
		if fallback := s.missedChange(found[sibling], candidate); fallback != "" && !noted[sibling] {
			found[sibling].Note = joinPresent(" ", found[sibling].Note, fallback)
			noted[sibling] = true
		}
	}
	return found
}

// missedChange tells the connection a longer trip takes where it parts from the plain one, such as
// "If the 2-minute change is missed: IC 1724 at 08:40 from Amersfoort Centraal, arriving 08:53."
func (s *Service) missedChange(plain, longer transit.Departure) string {
	clock := func(at time.Time) string { return at.In(s.location).Format("15:04") }
	moment := func(planned, actual time.Time) time.Time {
		if !actual.IsZero() {
			return actual
		}
		return planned
	}
	for index := 1; index < min(len(plain.Legs), len(longer.Legs)); index++ {
		previous, next, taken := plain.Legs[index-1], plain.Legs[index], longer.Legs[index]
		if next.Name == taken.Name && next.PlannedDeparture.Equal(taken.PlannedDeparture) {
			continue
		}
		change := "change"
		if arrives, leaves := moment(previous.PlannedArrival, previous.ActualArrival), moment(next.PlannedDeparture, next.ActualDeparture); !arrives.IsZero() && !leaves.IsZero() {
			change = fmt.Sprintf("%d-minute change", int(leaves.Sub(arrives).Minutes()))
		}
		return fmt.Sprintf("If the %s is missed: %s at %s from %s, arriving %s.", change, taken.Name, clock(taken.PlannedDeparture), taken.From, clock(longer.PlannedArrival))
	}
	return ""
}

func sameFirstTrain(a, b transit.Departure) bool {
	first := func(departure transit.Departure) string {
		for _, leg := range departure.Legs {
			if leg.Name != "Walk" {
				return leg.Name
			}
		}
		return ""
	}
	return a.Planned.Equal(b.Planned) && first(a) == first(b)
}

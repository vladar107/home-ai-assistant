package ns

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	// MaximumCheckedDepartures is how many planned departures one check takes.
	MaximumCheckedDepartures = 6

	travelTypeWalk = "WALK"
	// boardJourneys is how many departures the board fallback reads from the planned minute on.
	boardJourneys = 10
)

// changeTimeSteps are the extra transfer minutes NS is asked for in turn until its plan has a trip
// leaving at the planned minute: NS drops a train whose connection a later departure also makes, or
// that a later one with a transfer beats, and a longer change time lets it win again.
var changeTimeSteps = []int{0, 10, 20, 30}

// CheckDepartures checks each planned departure under the principal's own trains switch: the trip
// whose first train leaves From at exactly that time, never a nearby one. A departure NS could not
// be asked about comes back as a failed check, so the others still answer.
func (s *Service) CheckDepartures(ctx context.Context, principalID string, query transit.DepartureQuery) ([]transit.Departure, error) {
	if len(query.Times) == 0 || len(query.Times) > MaximumCheckedDepartures {
		return nil, &transit.RefusalError{Text: fmt.Sprintf("give 1 to %d planned departures.", MaximumCheckedDepartures)}
	}
	status, err := s.Status(ctx, principalID)
	if err != nil {
		return nil, err
	}
	if !status.Enabled {
		return nil, &transit.RefusalError{Text: DisabledText, Unavailable: true, Disabled: true}
	}
	list, failure := s.stationList(ctx, principalID)
	if failure != nil {
		return nil, &transit.RefusalError{Text: failure.Text, Unavailable: true}
	}
	from, failure := pickStation(list, "from", query.From)
	if failure != nil {
		return nil, &transit.RefusalError{Text: failure.Text}
	}
	to, failure := pickStation(list, "to", query.To)
	if failure != nil {
		return nil, &transit.RefusalError{Text: failure.Text}
	}
	if from.code == to.code {
		return nil, &transit.RefusalError{Text: fmt.Sprintf("from and to are both %s.", from.name)}
	}
	var via station
	hasVia := strings.TrimSpace(query.Via) != ""
	if hasVia {
		if via, failure = pickStation(list, "via", query.Via); failure != nil {
			return nil, &transit.RefusalError{Text: failure.Text}
		}
	}
	return s.checkDepartures(ctx, principalID, from, to, via, hasVia, query.Date, query.Times), nil
}

// ResolvePlace finds the one station a name means, under the principal's own trains switch, and
// returns its full name; role names the field in a refusal, such as "from".
func (s *Service) ResolvePlace(ctx context.Context, principalID, role, query string) (string, error) {
	status, err := s.Status(ctx, principalID)
	if err != nil {
		return "", err
	}
	if !status.Enabled {
		return "", &transit.RefusalError{Text: DisabledText, Unavailable: true, Disabled: true}
	}
	list, failure := s.stationList(ctx, principalID)
	if failure != nil {
		return "", &transit.RefusalError{Text: failure.Text, Unavailable: true}
	}
	found, failure := pickStation(list, role, query)
	if failure != nil {
		return "", &transit.RefusalError{Text: failure.Text}
	}
	return found.name, nil
}

func (s *Service) checkDepartures(ctx context.Context, principalID string, from, to, via station, hasVia bool, day time.Time, times []transit.Clock) []transit.Departure {
	// plans holds the responses asked so far by change time, so each departure tries the plain plan
	// first and is noted only when it needed the extra time.
	plans := make(map[int][]tripsResponse, len(changeTimeSteps))
	results := make([]transit.Departure, 0, len(times))
	for _, clock := range times {
		planned := clock.On(day, s.location)
		base := transit.Departure{Planned: planned, From: from.name, To: to.name}
		var chosen trip
		var changeTime int
		var found bool
		var err error
		for _, step := range changeTimeSteps {
			if chosen, found = matchDeparture(plans[step], from, planned); found {
				changeTime = step
				break
			}
			var response tripsResponse
			if response, err = s.planTrips(ctx, from, to, via, hasVia, false, planned, step); err != nil {
				break
			}
			plans[step] = append(plans[step], response)
			if chosen, found = matchDeparture([]tripsResponse{response}, from, planned); found {
				changeTime = step
				break
			}
		}
		switch {
		case found:
			results = append(results, departureFrom(base, chosen, changeTime, s.location))
		case err != nil:
			s.logger.InfoContext(ctx, "ns.request.failed", "principal_id", principalID, "tool", "departure_check", "error_class", errorClass(err))
			base.Status, base.Note = transit.StatusCheckFailed, modelText(err)
			results = append(results, base)
		default:
			results = append(results, s.boardFallback(ctx, principalID, base, from, to, via, hasVia))
		}
	}
	return results
}

// matchDeparture finds a trip whose first train leaves the station at exactly planned; among
// several, the one arriving first.
func matchDeparture(responses []tripsResponse, from station, planned time.Time) (trip, bool) {
	var chosen trip
	var arrival time.Time
	found := false
	for _, response := range responses {
		for _, item := range response.Trips {
			first, ok := firstRide(item)
			if !ok || !atStation(first.Origin, from) {
				continue
			}
			departure, ok := parseTime(first.Origin.PlannedDateTime)
			if !ok || !departure.Equal(planned) {
				continue
			}
			arrives, _ := parseTime(item.Legs[len(item.Legs)-1].Destination.PlannedDateTime)
			if !found || (!arrives.IsZero() && arrives.Before(arrival)) {
				chosen, arrival, found = item, arrives, true
			}
		}
	}
	return chosen, found
}

func firstRide(item trip) (leg, bool) {
	for _, part := range item.Legs {
		if part.TravelType != travelTypeWalk {
			return part, true
		}
	}
	return leg{}, false
}

func atStation(at stop, want station) bool {
	if at.StationCode != "" {
		return strings.EqualFold(at.StationCode, want.code)
	}
	return slices.Contains(want.forms, transit.Normalize(at.Name))
}

func departureFrom(base transit.Departure, item trip, changeTime int, location *time.Location) transit.Departure {
	local := func(value string) time.Time {
		parsed, ok := parseTime(value)
		if !ok {
			return time.Time{}
		}
		return parsed.In(location)
	}
	first, _ := firstRide(item)
	last := item.Legs[len(item.Legs)-1]
	result := base
	result.ActualDeparture = local(first.Origin.ActualDateTime)
	result.PlannedArrival, result.ActualArrival = local(last.Destination.PlannedDateTime), local(last.Destination.ActualDateTime)
	result.DelayMinutes = max(transit.DelayMinutes(result.Planned, result.ActualDeparture), transit.DelayMinutes(result.PlannedArrival, result.ActualArrival))
	result.PlannedPlatform = clip.Bytes(first.Origin.PlannedTrack, 10)
	result.Platform = clip.Bytes(first.Origin.ActualTrack, 10)
	if result.Platform == "" {
		result.Platform = result.PlannedPlatform
	}
	result.PlatformChanged = result.PlannedPlatform != "" && result.Platform != result.PlannedPlatform
	result.Live = (item.Realtime == nil || *item.Realtime) && !result.ActualDeparture.IsZero()

	cancelled := first.Cancelled
	partly := strings.EqualFold(item.Status, statusCancelled) && !cancelled
	var notes []string
	unreachable := false
	for _, part := range item.Legs {
		if part.TravelType == travelTypeWalk {
			result.Legs = append(result.Legs, transit.Leg{Name: "Walk", From: text(part.Origin.Name), To: text(part.Destination.Name),
				PlannedDeparture: local(part.Origin.PlannedDateTime), PlannedArrival: local(part.Destination.PlannedDateTime)})
			continue
		}
		if part.PartCancelled || part.AlternativeTransport || (part.Cancelled && !sameLeg(part, first)) {
			partly = true
		}
		if part.AlternativeTransport {
			notes = append(notes, joinPresent(" ", text(part.Name), "replaced by alternative transport"))
		}
		if part.Reachable != nil && !*part.Reachable && !unreachable {
			notes = append(notes, "a connection on this trip cannot be made")
			unreachable = true
		}
		result.Legs = append(result.Legs, transit.Leg{
			Name:                   text(part.Name),
			From:                   text(part.Origin.Name),
			To:                     text(part.Destination.Name),
			PlannedDeparture:       local(part.Origin.PlannedDateTime),
			ActualDeparture:        local(part.Origin.ActualDateTime),
			PlannedArrival:         local(part.Destination.PlannedDateTime),
			ActualArrival:          local(part.Destination.ActualDateTime),
			PlannedPlatform:        clip.Bytes(part.Origin.PlannedTrack, 10),
			ActualPlatform:         clip.Bytes(part.Origin.ActualTrack, 10),
			PlannedArrivalPlatform: clip.Bytes(part.Destination.PlannedTrack, 10),
			ActualArrivalPlatform:  clip.Bytes(part.Destination.ActualTrack, 10),
			Cancelled:              part.Cancelled,
			PartlyCancelled:        part.PartCancelled,
		})
	}
	if message := troubleMessage(item); message != "" {
		notes = append(notes, message)
	}
	if len(notes) > 0 {
		result.Note = clip.Bytes("NS: "+strings.Join(notes, "; "), maximumReasonBytes)
	}
	if changeTime > 0 {
		result.Note = joinPresent(" ", result.Note, fmt.Sprintf("NS plans this train only with %d extra minutes for each transfer, so a connection shown may be later than the one the user makes.", changeTime))
	}
	result.Status = transit.StatusOf(cancelled, partly, result.DelayMinutes, result.PlatformChanged)
	return result
}

func sameLeg(a, b leg) bool {
	return a.Name == b.Name && a.Origin.PlannedDateTime == b.Origin.PlannedDateTime
}

// boardFallback looks for the departure on the station's board when no planned trip leaves at
// that minute, since NS may leave a cancelled train out of its trip plans. Only a cancelled train
// that serves the destination counts; anything else stays not in the timetable.
func (s *Service) boardFallback(ctx context.Context, principalID string, base transit.Departure, from, to, via station, hasVia bool) transit.Departure {
	missing := base
	missing.Status = transit.StatusNotInTimetable
	missing.Note = fmt.Sprintf("NS plans no trip whose first train leaves %s at exactly %s; no other departure is put in its place.", from.name, base.Planned.Format("15:04"))

	key := strings.Join([]string{toolDepartures, "board", from.code, base.Planned.Format(dateTimeLayout)}, "|")
	var response departuresResponse
	if cached, found := s.cached(key); found {
		response = cached.(departuresResponse)
	} else {
		query := url.Values{
			"station":     {from.code},
			"dateTime":    {formatQueryTime(base.Planned)},
			"maxJourneys": {strconv.Itoa(boardJourneys)},
			"lang":        {"en"},
		}
		if err := s.client.get(ctx, "v2/departures", query, &response); err != nil {
			s.logger.InfoContext(ctx, "ns.request.failed", "principal_id", principalID, "tool", "departure_check_board", "error_class", errorClass(err))
			return missing
		}
		s.store(key, response)
	}
	for _, item := range response.Payload.Departures {
		planned, ok := parseTime(item.PlannedDateTime)
		if !ok || !planned.Equal(base.Planned) || !item.Cancelled || !serves(item, to, via, hasVia) {
			continue
		}
		name := text(item.Name)
		if item.Product.CategoryCode != "" && item.Product.Number != "" {
			name = text(item.Product.CategoryCode + " " + item.Product.Number)
		}
		result := base
		result.Status = transit.StatusCancelled
		result.PlannedPlatform = clip.Bytes(item.PlannedTrack, 10)
		result.Platform = result.PlannedPlatform
		result.Live = true
		result.Legs = []transit.Leg{{Name: joinPresent(" towards ", name, text(item.Direction)), From: from.name, PlannedDeparture: base.Planned, PlannedPlatform: result.PlannedPlatform, Cancelled: true}}
		result.Note = "NS: cancelled on the departure board; the trip planner offers no trip with it."
		return result
	}
	return missing
}

func serves(item departure, to, via station, hasVia bool) bool {
	names := []string{transit.Normalize(item.Direction)}
	for _, stop := range item.RouteStations {
		names = append(names, transit.Normalize(stop.MediumName))
	}
	for _, name := range names {
		if slices.Contains(to.forms, name) || (hasVia && slices.Contains(via.forms, name)) {
			return true
		}
	}
	return false
}

func renderDepartureChecks(v view, from, to, via station, hasVia bool, day time.Time, departures []transit.Departure) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "NS check of planned departures from %s to %s", from.name, to.name)
	if hasVia {
		fmt.Fprintf(&builder, " via %s", via.name)
	}
	fmt.Fprintf(&builder, " on %s (Dutch local time). Each line starts with the departure's status; copy it word for word:\n", day.In(v.location).Format("Mon 2 Jan"))
	transit.Write(&builder, v.location, departures)
	return strings.TrimRight(builder.String(), "\n")
}

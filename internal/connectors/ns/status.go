package ns

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	// slowerDelay and disruptedDelay are the minutes late, at departure or arrival, from which a
	// trip turns yellow and red.
	slowerDelay    = 3
	disruptedDelay = 15
	// slowerExtra is how many minutes longer than planned a journey may take before it turns yellow.
	slowerExtra = 5

	statusNormal    = "NORMAL"
	statusCancelled = "CANCELLED"

	maximumReasonBytes = 200
)

// TripStatus plans the query's trip under the principal's own trains switch and classifies the
// trip they would take: the first one departing at or after At, or with ArriveBy the last one
// arriving by At.
func (s *Service) TripStatus(ctx context.Context, principalID string, query transit.TripQuery) (transit.TripStatus, error) {
	enabled, err := s.Enabled(ctx, principalID)
	if err != nil {
		return transit.TripStatus{}, err
	}
	if !enabled {
		return transit.TripStatus{}, &transit.RefusalError{Text: Switch.OffText(), Unavailable: true, Disabled: true}
	}
	list, failure := s.stationList(ctx, principalID)
	if failure != nil {
		return transit.TripStatus{}, &transit.RefusalError{Text: failure.Text, Unavailable: true}
	}
	from, failure := pickStation(list, "from", query.From)
	if failure != nil {
		return transit.TripStatus{}, &transit.RefusalError{Text: failure.Text}
	}
	to, failure := pickStation(list, "to", query.To)
	if failure != nil {
		return transit.TripStatus{}, &transit.RefusalError{Text: failure.Text}
	}
	if from.code == to.code {
		return transit.TripStatus{}, &transit.RefusalError{Text: fmt.Sprintf("from and to are both %s.", from.name)}
	}
	at := time.Date(query.At.Year(), query.At.Month(), query.At.Day(), query.At.Hour(), query.At.Minute(), 0, 0, s.location)
	response, err := s.planTrips(ctx, from, to, station{}, false, query.ArriveBy, at, 0)
	if err != nil {
		s.logger.InfoContext(ctx, "ns.request.failed", "principal_id", principalID, "tool", "trip_status", "error_class", errorClass(err))
		return transit.TripStatus{}, &transit.RefusalError{Text: modelText(err), Unavailable: true}
	}
	result := transit.TripStatus{From: from.name, To: to.name}
	chosen, found := pickTrip(response.Trips, at, query.ArriveBy)
	if !found {
		result.Level, result.Cause, result.Reason = transit.LevelDisrupted, transit.CauseNoTrip, "no trip found"
		return result, nil
	}
	classify(&result, chosen, s.location)
	return result, nil
}

func pickTrip(trips []trip, at time.Time, arriveBy bool) (trip, bool) {
	var chosen trip
	found := false
	for _, item := range trips {
		if len(item.Legs) == 0 {
			continue
		}
		if arriveBy {
			arrival, ok := parseTime(item.Legs[len(item.Legs)-1].Destination.PlannedDateTime)
			if ok && !arrival.After(at) {
				chosen, found = item, true
			}
			continue
		}
		departure, ok := parseTime(item.Legs[0].Origin.PlannedDateTime)
		if ok && !departure.Before(at) {
			return item, true
		}
	}
	return chosen, found
}

func classify(result *transit.TripStatus, item trip, location *time.Location) {
	first, last := item.Legs[0], item.Legs[len(item.Legs)-1]
	local := func(value string) time.Time {
		parsed, ok := parseTime(value)
		if !ok {
			return time.Time{}
		}
		return parsed.In(location)
	}
	result.Found = true
	result.Transfers = item.Transfers
	result.PlannedDeparture, result.ActualDeparture = local(first.Origin.PlannedDateTime), local(first.Origin.ActualDateTime)
	result.PlannedArrival, result.ActualArrival = local(last.Destination.PlannedDateTime), local(last.Destination.ActualDateTime)
	result.DelayMinutes = max(delayMinutes(result.PlannedDeparture, result.ActualDeparture), delayMinutes(result.PlannedArrival, result.ActualArrival))
	if item.PlannedDurationInMinutes != nil && item.ActualDurationInMinutes != nil {
		result.ExtraMinutes = max(0, *item.ActualDurationInMinutes-*item.PlannedDurationInMinutes)
	}

	var reasons []string
	result.Cancelled = strings.EqualFold(item.Status, statusCancelled)
	alternative, unreachable := false, false
	for _, part := range item.Legs {
		switch {
		case part.Cancelled:
			reasons = append(reasons, joinPresent(" ", text(part.Name), "cancelled"))
			result.Cancelled = true
		case part.PartCancelled:
			reasons = append(reasons, joinPresent(" ", text(part.Name), "partly cancelled"))
			result.Cancelled = true
		}
		if part.AlternativeTransport && !alternative {
			reasons = append(reasons, "alternative transport")
			alternative = true
		}
		if part.Reachable != nil && !*part.Reachable && !unreachable {
			reasons = append(reasons, "a connection cannot be made")
			unreachable = true
		}
	}
	if result.Cancelled && len(reasons) == 0 {
		reasons = append(reasons, "cancelled")
	}
	if result.DelayMinutes > 0 {
		reasons = append(reasons, fmt.Sprintf("%d min late", result.DelayMinutes))
	}
	if result.ExtraMinutes > 0 {
		reasons = append(reasons, fmt.Sprintf("%d min longer than planned", result.ExtraMinutes))
	}
	abnormal := item.Status != "" && !strings.EqualFold(item.Status, statusNormal)

	switch {
	case result.Cancelled:
		result.Level, result.Cause = transit.LevelDisrupted, transit.CauseCancelled
	case alternative:
		result.Level, result.Cause = transit.LevelDisrupted, transit.CauseAlternative
	case unreachable:
		result.Level, result.Cause = transit.LevelDisrupted, transit.CauseMissedConnection
	case result.DelayMinutes >= disruptedDelay:
		result.Level, result.Cause = transit.LevelDisrupted, transit.CauseDelay
	case result.DelayMinutes >= slowerDelay:
		result.Level, result.Cause = transit.LevelSlower, transit.CauseDelay
	case result.ExtraMinutes >= slowerExtra:
		result.Level, result.Cause = transit.LevelSlower, transit.CauseLonger
	case abnormal:
		result.Level, result.Cause = transit.LevelSlower, transit.CauseDisruption
	default:
		result.Level = transit.LevelOnTime
		result.Reason = "on time"
		return
	}
	if message := troubleMessage(item); message != "" {
		reasons = append(reasons, message)
	} else if abnormal {
		reasons = append(reasons, "status "+strings.ToLower(text(item.Status)))
	}
	result.Reason = clip.Bytes(strings.Join(reasons, "; "), maximumReasonBytes)
}

// troubleMessage is the first thing NS says about the trip's trouble: its own messages, or else the
// first leg note.
func troubleMessage(item trip) string {
	for _, label := range item.LabelListItems {
		if value := text(label.Label); value != "" {
			return value
		}
	}
	if primary := item.PrimaryMessage; primary != nil {
		if value := text(primary.Title); value != "" {
			return value
		}
	}
	for _, message := range item.Messages {
		if value := joinPresent(": ", text(message.Head), text(message.Text)); value != "" {
			return value
		}
	}
	for _, part := range item.Legs {
		for _, note := range part.Notes {
			if value := text(note.Value); value != "" {
				return value
			}
		}
	}
	return ""
}

func delayMinutes(planned, actual time.Time) int {
	if planned.IsZero() || actual.IsZero() {
		return 0
	}
	return max(0, int(math.Round(actual.Sub(planned).Minutes())))
}

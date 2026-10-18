package bus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	// MaximumCheckedDepartures is how many planned departures one check takes.
	MaximumCheckedDepartures = 6

	statusPlanned = "PLANNED"
	statusUnknown = "UNKNOWN"
	// checkStopTimes is how many timetable departures the check reads from the planned minute on.
	checkStopTimes = 20
)

// CheckDepartures checks each planned departure under the principal's own buses switch: the call
// at the stop planned for exactly that minute, live from OVapi within the next hour and otherwise
// from the Transitous timetable, never a nearby one.
func (s *Service) CheckDepartures(ctx context.Context, principalID string, query transit.DepartureQuery) ([]transit.Departure, error) {
	if len(query.Times) == 0 || len(query.Times) > MaximumCheckedDepartures {
		return nil, &transit.RefusalError{Text: fmt.Sprintf("give 1 to %d planned departures.", MaximumCheckedDepartures)}
	}
	enabled, err := s.enabled(ctx, principalID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, &transit.RefusalError{Text: Switch.OffText(), Unavailable: true, Disabled: true}
	}
	index, failure := s.index(ctx, principalID)
	if failure != nil {
		return nil, &transit.RefusalError{Text: failure.Text, Unavailable: true}
	}
	group, failure := s.findStop(index, "stop", query.From, true)
	if failure != nil {
		return nil, &transit.RefusalError{Text: failure.Text}
	}
	return s.checkDepartures(ctx, principalID, group, query.Line, query.Towards, query.Date, query.Times), nil
}

// ResolvePlace checks that a name means one stop, under the principal's own buses switch, and
// returns the stop's name; role names the field in a refusal, such as "stop".
func (s *Service) ResolvePlace(ctx context.Context, principalID, role, query string) (string, error) {
	enabled, err := s.enabled(ctx, principalID)
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", &transit.RefusalError{Text: Switch.OffText(), Unavailable: true, Disabled: true}
	}
	index, failure := s.index(ctx, principalID)
	if failure != nil {
		return "", &transit.RefusalError{Text: failure.Text, Unavailable: true}
	}
	group, failure := s.findStop(index, role, query, false)
	if failure != nil {
		return "", &transit.RefusalError{Text: failure.Text}
	}
	return group.name, nil
}

func (s *Service) checkDepartures(ctx context.Context, principalID string, group *stopGroup, line, towards string, day time.Time, times []transit.Clock) []transit.Departure {
	now := s.now().In(s.location)
	var points map[string]timingPoint
	var pointsErr error
	asked := false
	results := make([]transit.Departure, 0, len(times))
	for _, clock := range times {
		planned := clock.On(day, s.location)
		base := transit.Departure{Planned: planned, From: group.name}
		if len(group.codes) > 0 && !planned.Before(now.Add(-pastSlack)) && planned.Before(now.Add(liveWindow)) {
			if !asked {
				points, pointsErr = s.passes(ctx, group.codes)
				asked = true
				if pointsErr != nil {
					s.logFailure(ctx, principalID, "departure_check", pointsErr)
				}
			}
			if pointsErr == nil && known(points, group) {
				var matches []call
				for _, entry := range s.calls(points, group) {
					if !strings.EqualFold(entry.pass.JourneyStopType, stopTypeLast) && entry.plannedDeparture.Truncate(time.Minute).Equal(planned) &&
						lineMatches(line, entry.pass.LinePublicNumber) && towardsMatches(towards, entry.pass.DestinationName50) {
						matches = append(matches, entry)
					}
				}
				if len(matches) > 0 {
					results = append(results, liveDeparture(base, matches))
					continue
				}
			}
		}
		results = append(results, s.timetableDeparture(ctx, principalID, group, base, line, towards))
	}
	return results
}

func liveDeparture(base transit.Departure, matches []call) transit.Departure {
	if len(matches) > 1 {
		slices.SortFunc(matches, func(a, b call) int { return strings.Compare(a.key, b.key) })
		var options []string
		for _, entry := range matches {
			options = append(options, joinPresent(" ", lineLabel(entry.pass), prefixed("towards ", text(entry.pass.DestinationName50)), parenthesised(platform(entry.platform))))
		}
		return ambiguous(base, options)
	}
	entry := matches[0]
	result := base
	result.To = text(entry.pass.DestinationName50)
	result.Live = !strings.EqualFold(entry.pass.TripStopStatus, statusPlanned) && !strings.EqualFold(entry.pass.TripStopStatus, statusUnknown)
	if result.Live {
		result.ActualDeparture = entry.expectedDeparture
	}
	result.DelayMinutes = transit.DelayMinutes(entry.plannedDeparture, result.ActualDeparture)
	result.Platform, result.PlannedPlatform = entry.platform, entry.platform
	result.Legs = []transit.Leg{{
		Name:             joinPresent(" ", lineLabel(entry.pass), prefixed("towards ", text(entry.pass.DestinationName50)), parenthesised(text(entry.pass.OperatorCode))),
		From:             base.From,
		PlannedDeparture: entry.plannedDeparture,
		ActualDeparture:  result.ActualDeparture,
		PlannedPlatform:  entry.platform,
		Cancelled:        entry.cancelled(),
	}}
	result.Status = transit.StatusOf(entry.cancelled(), false, result.DelayMinutes, false)
	return result
}

// timetableDeparture looks the departure up in the Transitous timetable, live where marked.
func (s *Service) timetableDeparture(ctx context.Context, principalID string, group *stopGroup, base transit.Departure, line, towards string) transit.Departure {
	response, err := s.stopTimes(ctx, group, base.Planned.Add(-time.Minute), checkStopTimes)
	if err != nil {
		s.logFailure(ctx, principalID, "departure_check", err)
		failed := base
		failed.Status, failed.Note = transit.StatusCheckFailed, modelText(err)
		return failed
	}
	var matches []stopTime
	for _, item := range response.StopTimes {
		scheduled := parseUTC(item.Place.ScheduledDeparture)
		if !scheduled.IsZero() && scheduled.Truncate(time.Minute).Equal(base.Planned) &&
			lineMatches(line, firstPresent(item.RouteShortName, item.DisplayName)) && towardsMatches(towards, item.Headsign) {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 0:
		missing := base
		missing.Status = transit.StatusNotInTimetable
		what := "No departure"
		if strings.TrimSpace(line) != "" {
			what = "No departure of line " + text(line)
		}
		missing.Note = fmt.Sprintf("%s leaves %s at exactly %s; no other departure is put in its place.", what, group.name, base.Planned.Format("15:04"))
		return missing
	case 1:
	default:
		var options []string
		for _, item := range matches {
			options = append(options, joinPresent(" ", modeName(item.Mode), text(firstPresent(item.RouteShortName, item.DisplayName)), prefixed("towards ", text(item.Headsign))))
		}
		return ambiguous(base, options)
	}
	item := matches[0]
	result := base
	scheduled := parseUTC(item.Place.ScheduledDeparture).In(base.Planned.Location())
	cancelled := item.Cancelled || item.TripCancelled || item.Place.Cancelled
	result.To = text(item.Headsign)
	result.Live = item.RealTime
	if item.RealTime {
		result.ActualDeparture = parseUTC(item.Place.Departure).In(base.Planned.Location())
	}
	result.DelayMinutes = transit.DelayMinutes(scheduled, result.ActualDeparture)
	result.PlannedPlatform = clip.Bytes(text(item.Place.ScheduledTrack), 10)
	result.Platform = clip.Bytes(text(item.Place.Track), 10)
	if result.Platform == "" {
		result.Platform = result.PlannedPlatform
	}
	result.PlatformChanged = result.PlannedPlatform != "" && result.Platform != result.PlannedPlatform
	result.Legs = []transit.Leg{{
		Name:             joinPresent(" ", modeName(item.Mode), text(firstPresent(item.RouteShortName, item.DisplayName)), prefixed("towards ", text(item.Headsign)), parenthesised(text(item.AgencyName))),
		From:             base.From,
		PlannedDeparture: scheduled,
		ActualDeparture:  result.ActualDeparture,
		PlannedPlatform:  result.PlannedPlatform,
		ActualPlatform:   clip.Bytes(text(item.Place.Track), 10),
		Cancelled:        cancelled,
	}}
	result.Status = transit.StatusOf(cancelled, false, result.DelayMinutes, result.PlatformChanged)
	return result
}

func ambiguous(base transit.Departure, options []string) transit.Departure {
	result := base
	result.Status = transit.StatusCheckFailed
	result.Note = fmt.Sprintf("Several departures leave at %s: %s; name the line and its direction.", base.Planned.Format("15:04"), strings.Join(options, "; "))
	return result
}

// lineMatches compares a line as the user names it, such as "3" or "Bus 3", with a public number.
func lineMatches(want, number string) bool {
	fields := strings.Fields(want)
	if len(fields) == 0 {
		return true
	}
	return strings.EqualFold(fields[len(fields)-1], strings.TrimSpace(number))
}

func towardsMatches(want, destination string) bool {
	want = transit.Normalize(want)
	return want == "" || strings.Contains(transit.Normalize(destination), want)
}

func renderDepartureCheck(v view, group *stopGroup, departures []transit.Departure, notices noticeList) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Check of planned departures from %s on %s (Dutch local time). Each line starts with the departure's status; copy it word for word:\n",
		group.name, v.day.Format("Mon 2 Jan"))
	transit.Write(&builder, v.location, departures)
	renderNotices(&builder, v, notices)
	return strings.TrimRight(builder.String(), "\n")
}

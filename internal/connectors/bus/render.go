package bus

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maximumTextBytes        = 300
	maximumDescriptionBytes = 500

	transitousAttribution = "Planned with Transitous (https://transitous.org/sources/), using open transit data and © OpenStreetMap contributors."
)

// view prints times in Dutch local time, naming the day whenever it is not the day asked about.
type view struct {
	location *time.Location
	day      time.Time
}

func (v view) clock(at time.Time) string {
	local := at.In(v.location)
	if !sameDay(local, v.day) {
		return local.Format("Mon 2 Jan 15:04")
	}
	return local.Format("15:04")
}

// timed renders an expected time against its plan: "08:04 (planned 08:01, 3 min late)".
func (v view) timed(planned, expected time.Time) string {
	switch {
	case planned.IsZero() && expected.IsZero():
		return "time unknown"
	case planned.IsZero():
		return v.clock(expected)
	case expected.IsZero():
		return v.clock(planned)
	}
	delay := delayMinutes(planned, expected)
	switch {
	case delay > 0:
		return fmt.Sprintf("%s (planned %s, %d min late)", v.clock(expected), v.clock(planned), delay)
	case delay < 0:
		return fmt.Sprintf("%s (planned %s, %d min early)", v.clock(expected), v.clock(planned), -delay)
	default:
		return v.clock(planned)
	}
}

func lineLabel(item pass) string {
	return joinPresent(" ", transportName(item.TransportType), text(item.LinePublicNumber))
}

func transportName(kind string) string {
	switch strings.ToUpper(kind) {
	case "BUS":
		return "Bus"
	case "TRAM":
		return "Tram"
	case "METRO":
		return "Metro"
	case "BOAT":
		return "Ferry"
	default:
		return "Line"
	}
}

func platform(value string) string {
	if value == "" {
		return ""
	}
	return "platform " + value
}

func renderLiveTrips(v view, from, to *stopGroup, trips []directTrip, notices noticeList) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Live direct buses from %s to %s (OVapi, live times):\n", from.name, to.name)
	for number, trip := range trips {
		fmt.Fprintf(&builder, "%d. %s towards %s", number+1, lineLabel(trip.from.pass), text(trip.from.pass.DestinationName50))
		if operator := text(trip.from.pass.OperatorCode); operator != "" {
			fmt.Fprintf(&builder, " (%s)", operator)
		}
		builder.WriteString("\n")
		if trip.from.cancelled() {
			builder.WriteString("   CANCELLED.\n")
		}
		fmt.Fprintf(&builder, "   departs %s%s\n", v.timed(trip.from.plannedDeparture, trip.from.expectedDeparture), prefixed(", ", platform(trip.from.platform)))
		fmt.Fprintf(&builder, "   arrives %s%s, %d min ride\n", v.timed(trip.to.plannedArrival, trip.to.expectedArrival), prefixed(", ", platform(trip.to.platform)),
			max(0, int(math.Round(trip.to.reaching().Sub(trip.from.leaving()).Minutes()))))
	}
	renderNotices(&builder, v, notices)
	return strings.TrimRight(builder.String(), "\n")
}

func renderStopStatus(v view, group *stopGroup, departures []call, limit int, notices noticeList) string {
	var builder strings.Builder
	delayed, cancelled := 0, 0
	for _, entry := range departures {
		switch {
		case entry.cancelled():
			cancelled++
		case entry.departureDelay() >= 3:
			delayed++
		}
	}
	fmt.Fprintf(&builder, "Live departures from %s at %s (OVapi): %d in the next 90 minutes or so", group.name, v.clock(v.day), len(departures))
	fmt.Fprintf(&builder, ", %d delayed by 3 min or more, %d cancelled.\n", delayed, cancelled)
	if len(departures) == 0 {
		builder.WriteString("Nothing is scheduled to leave in that window.\n")
	}
	if len(departures) > limit {
		departures = departures[:limit]
	}
	for _, entry := range departures {
		fmt.Fprintf(&builder, "- %s %s towards %s%s", v.timed(entry.plannedDeparture, entry.expectedDeparture), lineLabel(entry.pass), text(entry.pass.DestinationName50), prefixed(", ", platform(entry.platform)))
		if entry.cancelled() {
			builder.WriteString(", CANCELLED")
		} else if entry.expectedDeparture.IsZero() {
			builder.WriteString(", no live time")
		}
		builder.WriteString("\n")
	}
	renderNotices(&builder, v, notices)
	return strings.TrimRight(builder.String(), "\n")
}

func renderStopTimes(v view, group *stopGroup, response stopTimesResponse, notices noticeList) string {
	var builder strings.Builder
	delayed, cancelled := 0, 0
	for _, item := range response.StopTimes {
		switch {
		case item.Cancelled || item.TripCancelled || item.Place.Cancelled:
			cancelled++
		case delayMinutes(parseUTC(item.Place.ScheduledDeparture), parseUTC(item.Place.Departure)) >= 3:
			delayed++
		}
	}
	fmt.Fprintf(&builder, "Next departures from %s at %s (Transitous; times are live where marked): %d listed, %d delayed by 3 min or more, %d cancelled.\n",
		group.name, v.clock(v.day), len(response.StopTimes), delayed, cancelled)
	if len(response.StopTimes) == 0 {
		builder.WriteString("Nothing is scheduled to leave soon.\n")
	}
	for _, item := range response.StopTimes {
		name := joinPresent(" ", modeName(item.Mode), text(firstPresent(item.RouteShortName, item.DisplayName)))
		fmt.Fprintf(&builder, "- %s %s%s%s%s", v.timed(parseUTC(item.Place.ScheduledDeparture), parseUTC(item.Place.Departure)), name,
			prefixed(" towards ", text(item.Headsign)), prefixed(" ", parenthesised(text(item.AgencyName))), prefixed(", ", trackChange(item.Place)))
		switch {
		case item.Cancelled || item.TripCancelled || item.Place.Cancelled:
			builder.WriteString(", CANCELLED")
		case item.RealTime:
			builder.WriteString(" [live]")
		default:
			builder.WriteString(" [planned]")
		}
		builder.WriteString("\n")
	}
	renderNotices(&builder, v, notices)
	builder.WriteString(transitousAttribution)
	return builder.String()
}

// renderPlan states the trip's moment against the day it was asked on, and its times against the
// trip's own day, so a date is written once.
func renderPlan(asked, v view, from, to place, at time.Time, arriveBy bool, response planResponse, notices noticeList) string {
	var builder strings.Builder
	moment := "departing from " + asked.clock(at)
	if arriveBy {
		moment = "arriving by " + asked.clock(at)
	}
	fmt.Fprintf(&builder, "Planned trips from %s to %s, %s (Transitous; times are live where marked):\n", from.name, to.name, moment)
	if len(response.Itineraries) == 0 {
		builder.WriteString("No trip found.\n")
	}
	for number, item := range response.Itineraries {
		start, end := parseUTC(item.StartTime), parseUTC(item.EndTime)
		fmt.Fprintf(&builder, "%d. %s → %s, %d min, %s\n", number+1, v.clock(start), v.clock(end), (item.Duration+30)/60, transfers(item.Transfers))
		for _, leg := range item.Legs {
			if line := renderLeg(v, leg, from, to); line != "" {
				builder.WriteString("   " + line + "\n")
			}
		}
	}
	renderNotices(&builder, v, notices)
	builder.WriteString(transitousAttribution)
	return builder.String()
}

func transfers(count int) string {
	switch count {
	case 0:
		return "direct"
	case 1:
		return "1 change"
	default:
		return fmt.Sprintf("%d changes", count)
	}
}

func renderLeg(v view, leg planLeg, from, to place) string {
	minutes := (leg.Duration + 30) / 60
	if leg.Mode == modeWalk {
		if minutes < 2 {
			return ""
		}
		return fmt.Sprintf("walk %d min to %s", minutes, legEnd(leg.To.Name, to))
	}
	name := joinPresent(" ", modeName(leg.Mode), text(firstPresent(leg.RouteShortName, leg.DisplayName)))
	description := joinPresent(" ", name, prefixed("towards ", text(leg.Headsign)), parenthesised(text(leg.AgencyName)))
	departure := v.timed(parseUTC(leg.ScheduledStartTime), parseUTC(leg.StartTime))
	arrival := v.timed(parseUTC(leg.ScheduledEndTime), parseUTC(leg.EndTime))
	line := fmt.Sprintf("%s: %s %s%s → %s %s%s", description,
		legEnd(leg.From.Name, from), departure, prefixed(", ", trackChange(leg.From)),
		legEnd(leg.To.Name, to), arrival, prefixed(", ", trackChange(leg.To)))
	switch {
	case leg.Cancelled || leg.From.Cancelled || leg.To.Cancelled:
		line += " — CANCELLED"
	case leg.RealTime:
		line += " [live]"
	default:
		line += " [planned]"
	}
	return line
}

// legEnd names a leg's end, swapping Transitous's START and END for the places asked about.
func legEnd(name string, endpoint place) string {
	switch name {
	case "START", "END", "":
		return endpoint.name
	default:
		return text(name)
	}
}

func trackChange(stop planStop) string {
	planned, actual := clip(text(stop.ScheduledTrack), 10), clip(text(stop.Track), 10)
	switch {
	case actual != "" && planned != "" && actual != planned:
		return fmt.Sprintf("track %s (changed from %s)", actual, planned)
	case actual != "":
		return "track " + actual
	case planned != "":
		return "track " + planned
	default:
		return ""
	}
}

func modeName(mode string) string {
	switch mode {
	case "BUS", "COACH":
		return "Bus"
	case "TRAM":
		return "Tram"
	case "SUBWAY", "METRO":
		return "Metro"
	case "FERRY":
		return "Ferry"
	case "RAIL", "REGIONAL_RAIL", "REGIONAL_FAST_RAIL", "HIGHSPEED_RAIL", "LONG_DISTANCE", "NIGHT_RAIL", "SUBURBAN":
		return "Train"
	default:
		return strings.ToLower(text(mode))
	}
}

func sortNotices(list []notice) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].ranked != list[j].ranked {
			return list[i].ranked < list[j].ranked
		}
		return list[i].alert.header < list[j].alert.header
	})
}

func renderNotices(builder *strings.Builder, v view, notices noticeList) {
	switch {
	case notices.unavailable:
		builder.WriteString("Disruption notices are unavailable right now.\n")
		return
	case notices.total == 0:
		builder.WriteString("No disruption notices for these stops and lines.\n")
		return
	}
	fmt.Fprintf(builder, "Disruption notices (%d", notices.total)
	if notices.total > len(notices.items) {
		fmt.Fprintf(builder, ", showing %d", len(notices.items))
	}
	builder.WriteString("):\n")
	for _, item := range notices.items {
		about := joinPresent(", ", prefixed("at ", item.atStop), prefixed("line ", item.line))
		header := text(item.alert.header)
		description := clip(strings.Join(strings.Fields(item.alert.description), " "), maximumDescriptionBytes)
		if strings.HasPrefix(description, header) {
			header = ""
		}
		fmt.Fprintf(builder, "- %s", joinPresent(": ", joinPresent(" ", bracketed(item.alert.effect), bracketed(about)), joinPresent(" ", header, description)))
		if !item.end.IsZero() {
			fmt.Fprintf(builder, " (until %s)", item.end.In(v.location).Format("Mon 2 Jan 15:04"))
		}
		builder.WriteString("\n")
	}
}

func bracketed(value string) string {
	if value == "" {
		return ""
	}
	return "[" + value + "]"
}

func prefixed(prefix, value string) string {
	if value == "" {
		return ""
	}
	return prefix + value
}

func firstPresent(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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

// text collapses the whitespace in one field of outside text and bounds it.
func text(value string) string {
	return clip(strings.Join(strings.Fields(value), " "), maximumTextBytes)
}

func clip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func sameDay(a, b time.Time) bool {
	yearA, monthA, dayA := a.Date()
	yearB, monthB, dayB := b.Date()
	return yearA == yearB && monthA == monthB && dayA == dayB
}

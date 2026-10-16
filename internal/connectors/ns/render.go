package ns

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	maximumTextBytes          = 300
	maximumDisruptions        = 15
	maximumDisruptionBytes    = 800
	maximumRouteStations      = 4
	maximumDepartureMessages  = 3
	disruptionTypeCalamity    = "CALAMITY"
	disruptionTypeDisruption  = "DISRUPTION"
	disruptionTypeMaintenance = "MAINTENANCE"
)

type stop struct {
	Name            string `json:"name"`
	StationCode     string `json:"stationCode"`
	PlannedDateTime string `json:"plannedDateTime"`
	ActualDateTime  string `json:"actualDateTime"`
	PlannedTrack    string `json:"plannedTrack"`
	ActualTrack     string `json:"actualTrack"`
}

type note struct {
	Value string `json:"value"`
}

type product struct {
	CategoryCode     string `json:"categoryCode"`
	Number           string `json:"number"`
	LongCategoryName string `json:"longCategoryName"`
	OperatorName     string `json:"operatorName"`
}

type leg struct {
	Name                  string  `json:"name"`
	TravelType            string  `json:"travelType"`
	Direction             string  `json:"direction"`
	Cancelled             bool    `json:"cancelled"`
	PartCancelled         bool    `json:"partCancelled"`
	AlternativeTransport  bool    `json:"alternativeTransport"`
	Reachable             *bool   `json:"reachable"`
	CrossPlatformTransfer bool    `json:"crossPlatformTransfer"`
	ShorterStock          bool    `json:"shorterStock"`
	CrowdForecast         string  `json:"crowdForecast"`
	TransferTimeToNextLeg *int    `json:"transferTimeToNextLeg"`
	Origin                stop    `json:"origin"`
	Destination           stop    `json:"destination"`
	Product               product `json:"product"`
	Notes                 []note  `json:"notes"`
	TransferMessages      []struct {
		Message string `json:"message"`
	} `json:"transferMessages"`
}

type tripMessage struct {
	Head string `json:"head"`
	Text string `json:"text"`
}

type trip struct {
	Status                   string   `json:"status"`
	Transfers                int      `json:"transfers"`
	PlannedDurationInMinutes *int     `json:"plannedDurationInMinutes"`
	ActualDurationInMinutes  *int     `json:"actualDurationInMinutes"`
	CrowdForecast            string   `json:"crowdForecast"`
	Punctuality              *float64 `json:"punctuality"`
	Optimal                  bool     `json:"optimal"`
	Realtime                 *bool    `json:"realtime"`
	LabelListItems           []struct {
		Label string `json:"label"`
	} `json:"labelListItems"`
	Messages       []tripMessage `json:"messages"`
	PrimaryMessage *struct {
		Title   string       `json:"title"`
		Message *tripMessage `json:"message"`
	} `json:"primaryMessage"`
	ProductFare *struct {
		PriceInCents *int `json:"priceInCents"`
	} `json:"productFare"`
	Legs []leg `json:"legs"`
}

type tripsResponse struct {
	Trips []trip `json:"trips"`
}

type departure struct {
	Direction       string  `json:"direction"`
	Name            string  `json:"name"`
	PlannedDateTime string  `json:"plannedDateTime"`
	ActualDateTime  string  `json:"actualDateTime"`
	PlannedTrack    string  `json:"plannedTrack"`
	ActualTrack     string  `json:"actualTrack"`
	Product         product `json:"product"`
	Cancelled       bool    `json:"cancelled"`
	RouteStations   []struct {
		MediumName string `json:"mediumName"`
	} `json:"routeStations"`
	Messages []struct {
		Message string `json:"message"`
	} `json:"messages"`
}

type departuresResponse struct {
	Payload struct {
		Departures []departure `json:"departures"`
	} `json:"payload"`
}

type label struct {
	Label string `json:"label"`
}

type timespan struct {
	Start                string   `json:"start"`
	End                  string   `json:"end"`
	Period               string   `json:"period"`
	Situation            *label   `json:"situation"`
	Cause                *label   `json:"cause"`
	AdditionalTravelTime *label   `json:"additionalTravelTime"`
	AlternativeTransport *label   `json:"alternativeTransport"`
	Advices              []string `json:"advices"`
}

type disruption struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	Description      string `json:"description"`
	IsActive         *bool  `json:"isActive"`
	Start            string `json:"start"`
	End              string `json:"end"`
	ExpectedDuration *struct {
		Description string `json:"description"`
	} `json:"expectedDuration"`
	Timespans           []timespan `json:"timespans"`
	PublicationSections []struct {
		Section struct {
			Stations []struct {
				Name        string `json:"name"`
				StationCode string `json:"stationCode"`
			} `json:"stations"`
		} `json:"section"`
	} `json:"publicationSections"`
}

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

// timed renders an actual time against its plan: "08:04 (planned 08:01, 3 min late)".
func (v view) timed(planned, actual string) string {
	plannedAt, hasPlanned := parseTime(planned)
	actualAt, hasActual := parseTime(actual)
	switch {
	case !hasPlanned && !hasActual:
		return "time unknown"
	case !hasPlanned:
		return v.clock(actualAt)
	case !hasActual:
		return v.clock(plannedAt)
	}
	delay := int(math.Round(actualAt.Sub(plannedAt).Minutes()))
	switch {
	case delay > 0:
		return fmt.Sprintf("%s (planned %s, %d min late)", v.clock(actualAt), v.clock(plannedAt), delay)
	case delay < 0:
		return fmt.Sprintf("%s (planned %s, %d min early)", v.clock(actualAt), v.clock(plannedAt), -delay)
	default:
		return v.clock(plannedAt)
	}
}

func track(planned, actual string) string {
	planned, actual = clip(planned, 10), clip(actual, 10)
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

func joinPresent(separator string, parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, separator)
}

func renderTrips(v view, from, to, via station, hasVia, arriveBy bool, at time.Time, response tripsResponse) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "NS trips from %s to %s", from.name, to.name)
	if hasVia {
		fmt.Fprintf(&builder, " via %s", via.name)
	}
	if arriveBy {
		fmt.Fprintf(&builder, ", arriving by %s", at.In(v.location).Format("Mon 2 Jan 15:04"))
	} else {
		fmt.Fprintf(&builder, ", departing from %s", at.In(v.location).Format("Mon 2 Jan 15:04"))
	}
	builder.WriteString(" (Dutch local time):\n")
	if len(response.Trips) == 0 {
		builder.WriteString("NS found no trips for this request.")
		return builder.String()
	}
	for index, item := range response.Trips {
		renderTrip(&builder, v, index+1, item)
	}
	return strings.TrimRight(builder.String(), "\n")
}

func renderTrip(builder *strings.Builder, v view, number int, item trip) {
	if len(item.Legs) == 0 {
		fmt.Fprintf(builder, "%d. No journey details; status %s\n", number, text(item.Status))
		return
	}
	first, last := item.Legs[0], item.Legs[len(item.Legs)-1]
	status := ""
	if ride, ok := firstRide(item); ok {
		if planned, ok := parseTime(ride.Origin.PlannedDateTime); ok {
			status = departureFrom(transit.Departure{Planned: planned.In(v.location)}, item, 0, v.location).StatusText() + " — "
		}
	}
	fmt.Fprintf(builder, "%d. %sDepart %s %s", number, status, v.timed(first.Origin.PlannedDateTime, first.Origin.ActualDateTime), track(first.Origin.PlannedTrack, first.Origin.ActualTrack))
	fmt.Fprintf(builder, ", arrive %s %s", v.timed(last.Destination.PlannedDateTime, last.Destination.ActualDateTime), track(last.Destination.PlannedTrack, last.Destination.ActualTrack))

	var facts []string
	if duration := durationText(item.PlannedDurationInMinutes, item.ActualDurationInMinutes); duration != "" {
		facts = append(facts, duration)
	}
	switch item.Transfers {
	case 0:
		facts = append(facts, "direct")
	case 1:
		facts = append(facts, "1 transfer")
	default:
		facts = append(facts, fmt.Sprintf("%d transfers", item.Transfers))
	}
	facts = append(facts, "status "+text(item.Status))
	if crowd := crowdText(item.CrowdForecast); crowd != "" {
		facts = append(facts, crowd)
	}
	if item.Punctuality != nil {
		facts = append(facts, fmt.Sprintf("punctuality %.0f%%", *item.Punctuality))
	}
	if item.Realtime != nil && !*item.Realtime {
		facts = append(facts, "no live data")
	}
	if item.Optimal {
		facts = append(facts, "NS's recommended option")
	}
	if item.ProductFare != nil && item.ProductFare.PriceInCents != nil {
		facts = append(facts, fmt.Sprintf("fare €%d.%02d", *item.ProductFare.PriceInCents/100, *item.ProductFare.PriceInCents%100))
	}
	fmt.Fprintf(builder, "; %s\n", strings.Join(facts, ", "))

	var messages []string
	for _, item := range item.LabelListItems {
		messages = append(messages, text(item.Label))
	}
	if primary := item.PrimaryMessage; primary != nil {
		messages = append(messages, text(primary.Title))
		if primary.Message != nil {
			messages = append(messages, joinPresent(": ", text(primary.Message.Head), text(primary.Message.Text)))
		}
	}
	for _, message := range item.Messages {
		messages = append(messages, joinPresent(": ", text(message.Head), text(message.Text)))
	}
	if joined := joinPresent("; ", messages...); joined != "" {
		fmt.Fprintf(builder, "   Messages: %s\n", joined)
	}

	for index, part := range item.Legs {
		if index > 0 {
			renderChange(builder, item.Legs[index-1], part)
		}
		renderLeg(builder, v, part)
	}
}

func renderLeg(builder *strings.Builder, v view, part leg) {
	name := text(part.Name)
	if part.TravelType == "WALK" {
		name = "Walk"
	}
	var descriptors []string
	if category := text(part.Product.LongCategoryName); category != "" && !strings.EqualFold(category, name) {
		descriptors = append(descriptors, category)
	}
	if operator := text(part.Product.OperatorName); operator != "" && operator != "NS" {
		descriptors = append(descriptors, operator)
	}
	if direction := text(part.Direction); direction != "" {
		descriptors = append(descriptors, "direction "+direction)
	}
	fmt.Fprintf(builder, "   - %s", name)
	if len(descriptors) > 0 {
		fmt.Fprintf(builder, " (%s)", strings.Join(descriptors, ", "))
	}
	fmt.Fprintf(builder, ": %s %s", text(part.Origin.Name), joinPresent(" ", v.timed(part.Origin.PlannedDateTime, part.Origin.ActualDateTime), track(part.Origin.PlannedTrack, part.Origin.ActualTrack)))
	fmt.Fprintf(builder, " → %s %s", text(part.Destination.Name), joinPresent(" ", v.timed(part.Destination.PlannedDateTime, part.Destination.ActualDateTime), track(part.Destination.PlannedTrack, part.Destination.ActualTrack)))

	var flags []string
	if part.Cancelled {
		flags = append(flags, "CANCELLED")
	}
	if part.PartCancelled {
		flags = append(flags, "partly cancelled")
	}
	if part.AlternativeTransport {
		flags = append(flags, "alternative transport instead of a train")
	}
	if part.ShorterStock {
		flags = append(flags, "shorter train")
	}
	if crowd := crowdText(part.CrowdForecast); crowd != "" {
		flags = append(flags, crowd)
	}
	var notes []string
	for _, item := range part.Notes {
		notes = append(notes, text(item.Value))
	}
	if joined := joinPresent("; ", notes...); joined != "" {
		flags = append(flags, "notes: "+joined)
	}
	if len(flags) > 0 {
		fmt.Fprintf(builder, "; %s", strings.Join(flags, ", "))
	}
	builder.WriteString("\n")
}

// renderChange describes the transfer between two legs. NS puts the transfer messages on the leg
// being changed to and the transfer time on the leg being left.
func renderChange(builder *strings.Builder, previous, next leg) {
	var messages []string
	for _, message := range next.TransferMessages {
		messages = append(messages, text(message.Message))
	}
	detail := joinPresent("; ", messages...)
	if detail == "" && previous.TransferTimeToNextLeg != nil {
		detail = fmt.Sprintf("%d min to transfer", *previous.TransferTimeToNextLeg)
	}
	if (previous.CrossPlatformTransfer || next.CrossPlatformTransfer) && !strings.Contains(strings.ToLower(detail), "same platform") {
		detail = joinPresent("; ", detail, "same platform")
	}
	fmt.Fprintf(builder, "     change at %s", text(previous.Destination.Name))
	if detail != "" {
		fmt.Fprintf(builder, ": %s", detail)
	}
	builder.WriteString("\n")
}

func durationText(planned, actual *int) string {
	switch {
	case planned != nil && actual != nil && *actual != *planned:
		return fmt.Sprintf("%d min (planned %d)", *actual, *planned)
	case actual != nil:
		return fmt.Sprintf("%d min", *actual)
	case planned != nil:
		return fmt.Sprintf("%d min", *planned)
	default:
		return ""
	}
}

func crowdText(forecast string) string {
	switch forecast {
	case "LOW", "MEDIUM", "HIGH":
		return "crowding " + strings.ToLower(forecast)
	default:
		return ""
	}
}

func renderDepartures(v view, from station, at time.Time, limit int, response departuresResponse) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "NS departures from %s from %s (Dutch local time):\n", from.name, at.In(v.location).Format("Mon 2 Jan 15:04"))
	departures := response.Payload.Departures
	if len(departures) == 0 {
		builder.WriteString("NS lists no departures for this station and time.")
		return builder.String()
	}
	if len(departures) > limit {
		departures = departures[:limit]
	}
	for _, item := range departures {
		name := text(item.Name)
		if item.Product.CategoryCode != "" && item.Product.Number != "" {
			name = text(item.Product.CategoryCode + " " + item.Product.Number)
		}
		if operator := text(item.Product.OperatorName); operator != "" && operator != "NS" {
			name += " (" + operator + ")"
		}
		fmt.Fprintf(&builder, "- %s %s to %s", v.timed(item.PlannedDateTime, item.ActualDateTime), name, text(item.Direction))
		if where := track(item.PlannedTrack, item.ActualTrack); where != "" {
			fmt.Fprintf(&builder, ", %s", where)
		}
		var via []string
		for index, stop := range item.RouteStations {
			if index == maximumRouteStations {
				break
			}
			via = append(via, text(stop.MediumName))
		}
		if joined := joinPresent(", ", via...); joined != "" {
			fmt.Fprintf(&builder, ", via %s", joined)
		}
		if item.Cancelled {
			builder.WriteString("; CANCELLED")
		}
		var messages []string
		for index, message := range item.Messages {
			if index == maximumDepartureMessages {
				break
			}
			messages = append(messages, text(message.Message))
		}
		if joined := joinPresent("; ", messages...); joined != "" {
			fmt.Fprintf(&builder, "; %s", joined)
		}
		builder.WriteString("\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// selectDisruptions keeps what matters at the moment asked about, calamities first, then
// disruptions, then maintenance whose period covers that moment; the second result is the
// timespan to describe for each.
func selectDisruptions(entries []disruption, at time.Time) ([]disruption, []*timespan) {
	var selected []disruption
	var spans []*timespan
	for _, kind := range []string{disruptionTypeCalamity, disruptionTypeDisruption, disruptionTypeMaintenance} {
		for index := range entries {
			entry := entries[index]
			if entry.Type != kind {
				continue
			}
			covering := coveringTimespan(entry, at)
			if kind == disruptionTypeMaintenance {
				if covering == nil {
					continue
				}
			} else {
				if entry.IsActive != nil && !*entry.IsActive {
					continue
				}
				if end, ok := parseTime(entry.End); ok && !end.After(at) {
					continue
				}
				// at is whole minutes, so an entry starting within the minute asked about still counts.
				if start, ok := parseTime(entry.Start); ok && start.Truncate(time.Minute).After(at) {
					continue
				}
				if covering == nil && len(entry.Timespans) > 0 {
					covering = &entry.Timespans[0]
				}
			}
			selected = append(selected, entry)
			spans = append(spans, covering)
		}
	}
	return selected, spans
}

func coveringTimespan(entry disruption, at time.Time) *timespan {
	for index := range entry.Timespans {
		span := &entry.Timespans[index]
		start, hasStart := parseTime(span.Start)
		end, hasEnd := parseTime(span.End)
		if (!hasStart || !at.Before(start)) && (!hasEnd || at.Before(end)) && (hasStart || hasEnd) {
			return span
		}
	}
	if len(entry.Timespans) == 0 {
		start, hasStart := parseTime(entry.Start)
		end, hasEnd := parseTime(entry.End)
		if (!hasStart || !at.Before(start)) && (!hasEnd || at.Before(end)) {
			return &timespan{Start: entry.Start, End: entry.End}
		}
	}
	return nil
}

func renderDisruptions(v view, scope string, at time.Time, entries []disruption) string {
	selected, spans := selectDisruptions(entries, at)
	var builder strings.Builder
	when := at.In(v.location).Format("Mon 2 Jan 15:04")
	if len(selected) == 0 {
		fmt.Fprintf(&builder, "NS reports no disruptions, calamities, or engineering works %s at %s (Dutch local time).", scope, when)
		return builder.String()
	}
	fmt.Fprintf(&builder, "NS disruptions %s at %s (Dutch local time), %d in total:\n", scope, when, len(selected))
	for index, entry := range selected {
		if index == maximumDisruptions {
			fmt.Fprintf(&builder, "%d more not shown.\n", len(selected)-maximumDisruptions)
			break
		}
		builder.WriteString(clip(renderDisruption(entry, spans[index]), maximumDisruptionBytes))
		builder.WriteString("\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

func renderDisruption(entry disruption, span *timespan) string {
	parts := []string{sentence(text(entry.Title)), sentence(text(entry.Description))}
	labelled := func(name string, value *label) {
		if value != nil {
			parts = append(parts, labelledSentence(name, value.Label))
		}
	}
	if span != nil {
		labelled("Situation", span.Situation)
		labelled("Cause", span.Cause)
		labelled("Alternative transport", span.AlternativeTransport)
		labelled("Extra travel time", span.AdditionalTravelTime)
		var advices []string
		for _, advice := range span.Advices {
			advices = append(advices, text(advice))
		}
		parts = append(parts, labelledSentence("Advice", joinPresent("; ", advices...)), labelledSentence("Period", span.Period))
	}
	if entry.ExpectedDuration != nil {
		parts = append(parts, labelledSentence("Expected end", entry.ExpectedDuration.Description))
	}
	return fmt.Sprintf("- [%s] %s", strings.ToLower(text(entry.Type)), joinPresent(" ", parts...))
}

func labelledSentence(name, value string) string {
	if value = text(value); value == "" {
		return ""
	}
	return sentence(name + ": " + value)
}

func sentence(value string) string {
	if value == "" || strings.HasSuffix(value, ".") || strings.HasSuffix(value, "!") || strings.HasSuffix(value, "?") {
		return value
	}
	return value + "."
}

// text collapses the whitespace in one field of NS free text, which is data from outside, and
// bounds it.
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

package calendar

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
)

const (
	maximumDescriptionBytes = 8 << 10
	maximumTitleRunes       = 300
	maximumAttendees        = 50
	headerReserveBytes      = 2 << 10
	maximumProblemBytes     = 4 << 10
	moreProblemsBytes       = 100
	dayLayout               = "Mon 2006-01-02"
	clockLayout             = "15:04"
)

var (
	hiddenElements = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)\s*>`)
	anchors        = regexp.MustCompile(`(?is)<a\s[^>]*?href\s*=\s*["']([^"']*)["'][^>]*>(.*?)</a\s*>`)
	lineBreaks     = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/tr|/h[1-6]|/table|hr)\b[^>]*>`)
	tags           = regexp.MustCompile(`(?s)<[^>]*>`)
	markup         = regexp.MustCompile(`<[a-zA-Z/!][^>]*>`)
	blankRuns      = regexp.MustCompile(`[ \t\x{00a0}]+`)
	blankLines     = regexp.MustCompile(`\n\s*\n\s*\n+`)
)

// bounds reads an event's start and end: an all-day event is its dates in the configured zone,
// with the end exclusive as Google reports it.
func (s *Service) bounds(event Event) (start, end time.Time, allDay, ok bool) {
	if event.Start.DateTime != "" && event.End.DateTime != "" {
		from, err := time.Parse(time.RFC3339, event.Start.DateTime)
		to, endErr := time.Parse(time.RFC3339, event.End.DateTime)
		if err != nil || endErr != nil {
			return time.Time{}, time.Time{}, false, false
		}
		return from, to, false, true
	}
	from, err := time.ParseInLocation(time.DateOnly, event.Start.Date, s.location)
	to, endErr := time.ParseInLocation(time.DateOnly, event.End.Date, s.location)
	if err != nil || endErr != nil {
		return time.Time{}, time.Time{}, false, false
	}
	return from, to, true, true
}

// renderEvents lists the events that start before the continuation and fit the byte budget; the
// first one that does not fit, or the 101st, moves the continuation back to its start, and
// events starting at or after it are dropped. The header is written last, once the cut is known.
func renderEvents(events []listed, start, end time.Time, location *time.Location, query string, more continuation, problems []string) string {
	problems = boundProblems(problems)
	budget := eventsResultBytes - headerReserveBytes - len(fmt.Sprintf("%q", query))
	for _, problem := range problems {
		budget -= len(problem) + 1
	}
	var bodies []string
	used := 0
	for index, entry := range events {
		if !more.allows(entry.start) {
			break
		}
		var body strings.Builder
		writeEvent(&body, entry, location)
		if index >= maximumEvents || (index > 0 && used+body.Len()+1 > budget) {
			more.cutAt(entry.start)
			break
		}
		used += body.Len() + 1
		bodies = append(bodies, body.String())
	}
	shown := 0
	for shown < len(bodies) && more.allows(events[shown].start) {
		shown++
	}
	bodies = bodies[:shown]

	continues := more.needed && more.at.After(start)
	if continues {
		end = more.at
	}
	span := fmt.Sprintf("between %s and %s", start.In(location).Format("2006-01-02 15:04"), end.In(location).Format("2006-01-02 15:04"))
	if query != "" {
		span += fmt.Sprintf(" matching %q", query)
	}
	var builder strings.Builder
	if shown == 0 {
		fmt.Fprintf(&builder, "No events %s", span)
	} else {
		fmt.Fprintf(&builder, "%d events %s (times in %s)", shown, span, location)
	}
	switch {
	case continues:
		fmt.Fprintf(&builder, "; the listing stops there, so call again with start %s and the same end, query, and account to see the rest (events already listed can appear again)", more.at.In(location).Format(time.RFC3339))
	case more.needed:
		builder.WriteString("; more events exist than one result can hold, so narrow the range or add a query to see others")
	}
	if shown == 0 {
		builder.WriteString(".")
	} else {
		builder.WriteString(":\n")
	}
	for _, body := range bodies {
		builder.WriteString("\n")
		builder.WriteString(body)
	}
	for _, problem := range problems {
		fmt.Fprintf(&builder, "\n%s", problem)
	}
	return builder.String()
}

// boundProblems keeps the problem lines within maximumProblemBytes and says how many were left out.
func boundProblems(problems []string) []string {
	var kept []string
	used := 0
	for index, problem := range problems {
		if used+len(problem)+1 > maximumProblemBytes-moreProblemsBytes {
			return append(kept, fmt.Sprintf("and %d more calendars or accounts could not be read", len(problems)-index))
		}
		used += len(problem) + 1
		kept = append(kept, problem)
	}
	return kept
}

func writeEvent(builder *strings.Builder, entry listed, location *time.Location) {
	event := entry.event
	fmt.Fprintf(builder, "%s\n", eventTitle(event))
	fmt.Fprintf(builder, "When: %s\n", whenText(entry, location))
	fmt.Fprintf(builder, "Account: %s\n", entry.account.Address)
	fmt.Fprintf(builder, "Calendar: %s (id %s)\n", clip.Runes(entry.calendar.Name(), maximumTitleRunes), entry.calendar.ID)
	fmt.Fprintf(builder, "Event id: %s\n", event.ID)
	if place := strings.TrimSpace(event.Location); place != "" {
		fmt.Fprintf(builder, "Location: %s\n", clip.Runes(oneLine(place), maximumTitleRunes))
	}
	if response := ownResponse(event); response != "" {
		fmt.Fprintf(builder, "Your response: %s\n", response)
	}
}

// renderEvent prints an event's details and a window of its description text.
func renderEvent(entry listed, location *time.Location, offset int) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Times in %s.\n", location)
	writeEvent(&builder, entry, location)
	event := entry.event
	if event.Status == "cancelled" {
		builder.WriteString("Status: cancelled\n")
	}
	if name := personName(event.Organizer); name != "" {
		fmt.Fprintf(&builder, "Organizer: %s\n", name)
	}
	if link := conferenceLink(event); link != "" {
		fmt.Fprintf(&builder, "Conference link: %s\n", link)
	}
	switch {
	case len(event.Recurrence) > 0:
		fmt.Fprintf(&builder, "Recurrence: %s\n", clip.Runes(strings.Join(event.Recurrence, "; "), maximumTitleRunes))
	case event.RecurringEventID != "":
		fmt.Fprintf(&builder, "Recurrence: one occurrence of the recurring event %s\n", event.RecurringEventID)
	}
	if len(event.Attendees) > 0 {
		fmt.Fprintf(&builder, "Attendees (%d):\n", len(event.Attendees))
		for index, attendee := range event.Attendees {
			if index == maximumAttendees {
				fmt.Fprintf(&builder, "- and %d more\n", len(event.Attendees)-maximumAttendees)
				break
			}
			line := personName(attendee)
			if attendee.Optional {
				line += ", optional"
			}
			if attendee.Self {
				line += ", you"
			}
			fmt.Fprintf(&builder, "- %s: %s\n", line, responseText(attendee.ResponseStatus))
		}
	}

	description := descriptionText(event.Description)
	if description == "" {
		return strings.TrimRight(builder.String(), "\n")
	}
	window, next, truncated := descriptionWindow(description, offset, maximumDescriptionBytes)
	if offset >= len(description) {
		fmt.Fprintf(&builder, "\nThe description ends before that position; it is %d bytes.", len(description))
		return builder.String()
	}
	builder.WriteString("\nDescription:\n")
	builder.WriteString(window)
	if truncated {
		fmt.Fprintf(&builder, "\n\n[truncated: %d of %d bytes shown; continue with offset_bytes %d]", next, len(description), next)
	}
	return builder.String()
}

func eventTitle(event Event) string {
	if title := strings.TrimSpace(oneLine(event.Summary)); title != "" {
		return clip.Runes(title, maximumTitleRunes)
	}
	return "(no title)"
}

func whenText(entry listed, location *time.Location) string {
	if entry.allDay {
		last := entry.end.AddDate(0, 0, -1)
		if !last.After(entry.start) {
			return fmt.Sprintf("%s (all day)", entry.start.Format(dayLayout))
		}
		return fmt.Sprintf("%s to %s (all day)", entry.start.Format(dayLayout), last.Format(dayLayout))
	}
	from, to := entry.start.In(location), entry.end.In(location)
	if from.Format(time.DateOnly) == to.Format(time.DateOnly) {
		return fmt.Sprintf("%s %s to %s", from.Format(dayLayout), from.Format(clockLayout), to.Format(clockLayout))
	}
	return fmt.Sprintf("%s %s to %s %s", from.Format(dayLayout), from.Format(clockLayout), to.Format(dayLayout), to.Format(clockLayout))
}

func ownResponse(event Event) string {
	for _, attendee := range event.Attendees {
		if attendee.Self {
			return responseText(attendee.ResponseStatus)
		}
	}
	return ""
}

func responseText(status string) string {
	switch status {
	case "accepted":
		return "accepted"
	case "declined":
		return "declined"
	case "tentative":
		return "tentative"
	default:
		return "not answered"
	}
}

func personName(person Person) string {
	name, address := strings.TrimSpace(oneLine(person.DisplayName)), strings.TrimSpace(person.Email)
	switch {
	case name != "" && address != "":
		return clip.Runes(name, 100) + " <" + address + ">"
	case name != "":
		return clip.Runes(name, 100)
	default:
		return address
	}
}

func conferenceLink(event Event) string {
	for _, entry := range event.ConferenceData.EntryPoints {
		if entry.EntryPointType == "video" && entry.URI != "" {
			return entry.URI
		}
	}
	return event.HangoutLink
}

// descriptionText reduces a description to text. Calendar descriptions are plain text or a small
// subset of HTML, so text without tags is left as written.
func descriptionText(description string) string {
	description = strings.ToValidUTF8(description, "�")
	if !markup.MatchString(description) {
		return strings.TrimSpace(description)
	}
	text := hiddenElements.ReplaceAllString(description, "")
	text = anchors.ReplaceAllString(text, "$2 ($1)")
	text = lineBreaks.ReplaceAllString(text, "\n")
	text = tags.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	text = blankRuns.ReplaceAllString(text, " ")
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimSpace(line)
	}
	text = blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(text)
}

// descriptionWindow returns at most limit bytes of text from offset, never splitting a character:
// an offset inside one moves to the next character start, and a window ends before a cut one.
// The next offset is where a continuation resumes.
func descriptionWindow(text string, offset, limit int) (window string, next int, truncated bool) {
	if offset >= len(text) {
		return "", len(text), false
	}
	for offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset++
	}
	window = text[offset:]
	if len(window) > limit {
		window, truncated = window[:limit], true
		for len(window) > 0 && !utf8.RuneStart(text[offset+len(window)]) {
			window = window[:len(window)-1]
		}
	}
	return window, offset + len(window), truncated
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

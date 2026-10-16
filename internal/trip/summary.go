package trip

import (
	"strings"
	"time"
)

const (
	// summaryRunes keeps a summary well inside Telegram's 4096-character limit.
	summaryRunes = 3500

	summaryDateLayout = "Mon 2 Jan 2006"
	summaryTimeLayout = "Mon 2 Jan 15:04"

	SummaryEnded     = "Trip ended."
	SummaryCancelled = "Cancelled."
)

// Summary is the plain text pinned in a trip's tab: the trip, its bookings with their references,
// and when each check-in opens, which is when its reminder fires if one could be set, with times in
// the trip's zone. A trip that ended or was
// cancelled says so first, where the pinned bar shows it. It is built from the validated fields of
// the trip only.
func Summary(trip Trip, now time.Time) string {
	location := loadZone(trip.Zone)
	var lines []string
	switch {
	case trip.Status == StatusCancelled:
		lines = append(lines, SummaryCancelled)
	case trip.Ended(now):
		lines = append(lines, SummaryEnded)
	}
	lines = append(lines,
		trip.Title+" ("+trip.Destination+")",
		trip.StartsAt.In(location).Format(summaryDateLayout)+" to "+trip.EndLabel(summaryDateLayout, summaryDateLayout)+", times in "+trip.Zone,
	)
	if len(trip.Bookings) > 0 {
		lines = append(lines, "")
	}
	for _, booking := range trip.Bookings {
		line := "- " + booking.Title + ", " + booking.StartsAt.In(location).Format(summaryTimeLayout)
		if booking.Reference != "" {
			line += " (" + booking.Reference + ")"
		}
		if booking.CheckInAt.IsZero() {
			line += ". No check-in time"
		} else {
			line += ". Check-in opens " + booking.CheckInAt.In(location).Format(summaryTimeLayout)
		}
		lines = append(lines, line+".")
	}
	text := strings.Join(lines, "\n")
	if runes := []rune(text); len(runes) > summaryRunes {
		text = string(runes[:summaryRunes-1]) + "…"
	}
	return text
}

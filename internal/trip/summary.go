package trip

import (
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/localtime"
)

const (
	// summaryRunes keeps a summary well inside Telegram's 4096-character limit.
	summaryRunes = 3500

	SummaryEnded     = "Trip ended."
	SummaryCancelled = "Cancelled."
)

var (
	summaryEndedText     = locale.T("Trip ended.", "Поездка закончилась.")
	summaryCancelledText = locale.T("Cancelled.", "Отменена.")

	summaryRange = locale.T("%s to %s, times in %s", "с %[1]s по %[2]s, время: %[3]s")

	summaryBooking        = locale.T("- %s, %s. Check-in opens %s.", "- %[1]s, %[2]s. Регистрация откроется %[3]s.")
	summaryBookingNoTime  = locale.T("- %s, %s. No check-in time.", "- %[1]s, %[2]s. Время регистрации неизвестно.")
	summaryBookingRef     = locale.T("- %s, %s (%s). Check-in opens %s.", "- %[1]s, %[2]s (%[3]s). Регистрация откроется %[4]s.")
	summaryBookingRefNone = locale.T("- %s, %s (%s). No check-in time.", "- %[1]s, %[2]s (%[3]s). Время регистрации неизвестно.")
)

// Summary is the plain text pinned in a trip's tab, in English.
func Summary(trip Trip, now time.Time) string {
	return SummaryIn(locale.English, trip, now)
}

// SummaryIn is the plain text pinned in a trip's tab: the trip, its bookings with their references,
// and when each check-in opens, which is when its reminder fires if one could be set, with times in
// the trip's zone. A trip that ended or was cancelled says so first, where the pinned bar shows it.
// It is built from the validated fields of the trip only.
func SummaryIn(language locale.Language, trip Trip, now time.Time) string {
	location := localtime.SavedZone(trip.Zone)
	var lines []string
	switch {
	case trip.Status == StatusCancelled:
		lines = append(lines, summaryCancelledText.In(language))
	case trip.Ended(now):
		lines = append(lines, summaryEndedText.In(language))
	}
	endsOn := trip.EndsAt.In(location)
	if endsOn.Hour() == 0 && endsOn.Minute() == 0 && endsOn.Second() == 0 && trip.EndsAt.After(trip.StartsAt) {
		endsOn = endsOn.AddDate(0, 0, -1)
	}
	lines = append(lines,
		trip.Title+" ("+trip.Destination+")",
		summaryRange.FormatIn(language, locale.Date(language, trip.StartsAt.In(location), locale.DayYear), locale.Date(language, endsOn, locale.DayYear), location.String()),
	)
	if len(trip.Bookings) > 0 {
		lines = append(lines, "")
	}
	for _, booking := range trip.Bookings {
		starts := locale.Date(language, booking.StartsAt.In(location), locale.DayTime)
		var line string
		switch {
		case booking.Reference != "" && booking.CheckInAt.IsZero():
			line = summaryBookingRefNone.FormatIn(language, booking.Title, starts, booking.Reference)
		case booking.Reference != "":
			line = summaryBookingRef.FormatIn(language, booking.Title, starts, booking.Reference, locale.Date(language, booking.CheckInAt.In(location), locale.DayTime))
		case booking.CheckInAt.IsZero():
			line = summaryBookingNoTime.FormatIn(language, booking.Title, starts)
		default:
			line = summaryBooking.FormatIn(language, booking.Title, starts, locale.Date(language, booking.CheckInAt.In(location), locale.DayTime))
		}
		lines = append(lines, line)
	}
	text := strings.Join(lines, "\n")
	if runes := []rune(text); len(runes) > summaryRunes {
		text = string(runes[:summaryRunes-1]) + "…"
	}
	return text
}

package trip

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const (
	tripsTable    = "trips"
	bookingsTable = "trip_bookings"
)

// Exporter is the trips section of a user's export: each trip with its bookings. A trip's summary
// message belongs to one chat's tab, so it is left out.
func Exporter() export.Exporter { return exporter{} }

type exporter struct{}

func (exporter) Section() export.Section {
	return export.Section{Name: "trips", Label: "Trips", Version: 1, Tables: []export.Table{
		{Name: tripsTable, Omit: []string{"summary_message_id"}},
		{Name: bookingsTable},
	}}
}

func (exporter) Keep(string, export.Row) bool { return true }

// Plan adds each new trip with its bookings, which come only with their trip. A booking whose
// reference another of the principal's trips already holds is left out of the restored trip.
func (exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, tripsTable)
	if err != nil {
		return err
	}
	now := plan.Now()
	unfinished := map[string]int{}
	for _, row := range current {
		if ends, ok := row.Time("ends_at"); ok && ends.After(now) {
			unfinished[row.Text("status")]++
		}
	}
	currentBookings, err := plan.Current(ctx, bookingsTable)
	if err != nil {
		return err
	}
	fingerprints := map[string]bool{}
	for _, row := range currentBookings {
		if value := row.Text("fingerprint"); value != "" {
			fingerprints[value] = true
		}
	}
	unfinishedAfter := now.UTC().Format("2006-01-02T15:04:05.000Z")
	plan.Limit(export.Limit{Table: tripsTable, Where: "status = 'planned' AND ends_at > ?", Args: []any{unfinishedAfter}, Max: MaximumUnfinished})
	plan.Limit(export.Limit{Table: tripsTable, Where: "status = 'suggested' AND ends_at > ?", Args: []any{unfinishedAfter}, Max: MaximumSuggested})
	bookings := map[string][]export.Row{}
	var bookingIDs []string
	for _, row := range tables[bookingsTable] {
		bookings[row.Text("trip_id")] = append(bookings[row.Text("trip_id")], row)
		bookingIDs = append(bookingIDs, row.Text("id"))
	}
	trips := tables[tripsTable]
	ids := make([]string, 0, len(trips))
	for _, row := range trips {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, tripsTable, ids)
	if err != nil {
		return err
	}
	assignedBookings, err := plan.Assign(ctx, bookingsTable, bookingIDs)
	if err != nil {
		return err
	}
	for _, row := range trips {
		id := row.Text("id")
		title := export.Quote(row.Text("title"))
		assignment := assigned[id]
		if assignment.Present {
			plan.Present()
			plan.Refer(export.RefTrip, id, assignment.ID, true)
			// A trip already here keeps its bookings under the IDs they were given, derived ones
			// included, and a booking it no longer holds has no reminder to follow.
			for _, booking := range bookings[id] {
				bookingID := booking.Text("id")
				if held := assignedBookings[bookingID]; held.Present {
					plan.Refer(export.RefTripReminder, ReminderID(id, bookingID), ReminderID(assignment.ID, held.ID), true)
				} else {
					plan.Drop(export.RefTripReminder, ReminderID(id, bookingID))
				}
			}
			continue
		}
		skip := func(reason string) {
			plan.Skip(title, reason)
			for _, booking := range bookings[id] {
				plan.Drop(export.RefTripReminder, ReminderID(id, booking.Text("id")))
			}
		}
		draft, reason := tripDraft(row, bookings[id])
		if reason != "" {
			skip(reason)
			continue
		}
		normalized, text := normalizeDraft(draft, nil, false)
		if text != "" {
			skip(text)
			continue
		}
		status := row.Text("status")
		switch status {
		case StatusSuggested, StatusPlanned, StatusDismissed, StatusCancelled:
		default:
			skip("its status could not be read")
			continue
		}
		if draft.EndsAt.After(now) && (status == StatusPlanned && unfinished[status] >= MaximumUnfinished || status == StatusSuggested && unfinished[status] >= MaximumSuggested) {
			skip("you already have the most unfinished trips there may be")
			continue
		}
		write := row.Clone()
		write["id"] = assignment.ID
		plan.Add(tripsTable, write)
		var reminders [][2]string
		for index, booking := range bookings[id] {
			bookingID := booking.Text("id")
			bookingAssignment := assignedBookings[bookingID]
			if bookingAssignment.Present {
				plan.Skip(export.Quote(booking.Text("title"))+" of "+title, "the booking is already here under another trip")
				plan.Drop(export.RefTripReminder, ReminderID(id, bookingID))
				continue
			}
			if value := booking.Text("fingerprint"); value != "" {
				if fingerprints[value] {
					plan.Skip(export.Quote(booking.Text("title"))+" of "+title, "another of your trips already holds this booking")
					plan.Drop(export.RefTripReminder, ReminderID(id, bookingID))
					continue
				}
				fingerprints[value] = true
			}
			bookingWrite := booking.Clone()
			bookingWrite["id"], bookingWrite["trip_id"] = bookingAssignment.ID, assignment.ID
			bookingWrite["flight_number"] = normalized.Bookings[index].FlightNumber
			plan.Add(bookingsTable, bookingWrite)
			reminders = append(reminders, [2]string{ReminderID(id, bookingID), ReminderID(assignment.ID, bookingAssignment.ID)})
		}
		plan.Adding(title)
		plan.Refer(export.RefTrip, id, assignment.ID, false)
		for _, reminder := range reminders {
			plan.Refer(export.RefTripReminder, reminder[0], reminder[1], false)
		}
		if draft.EndsAt.After(now) {
			unfinished[status]++
		}
	}
	return nil
}

// tripDraft reads a stored trip and its bookings in the form normalizeDraft checks.
func tripDraft(row export.Row, bookings []export.Row) (Draft, string) {
	starts, startsOK := row.Time("starts_at")
	ends, endsOK := row.Time("ends_at")
	_, createdOK := row.Time("created_at")
	_, updatedOK := row.Time("updated_at")
	if !startsOK || !endsOK || !createdOK || !updatedOK {
		return Draft{}, "its dates could not be read"
	}
	draft := Draft{Title: row.Text("title"), Destination: row.Text("destination"), StartsAt: starts, EndsAt: ends, Zone: row.Text("zone"), Source: row.Text("source")}
	for _, booking := range bookings {
		starts, ok := booking.Time("starts_at")
		if !ok {
			return Draft{}, "a booking's time could not be read"
		}
		item := Booking{Kind: booking.Text("kind"), Title: booking.Text("title"), Reference: booking.Text("reference"), FlightNumber: booking.Text("flight_number"), StartsAt: starts, CheckInSource: booking.Text("check_in_source")}
		if !booking.Null("check_in_at") {
			if item.CheckInAt, ok = booking.Time("check_in_at"); !ok {
				return Draft{}, "a booking's check-in could not be read"
			}
		}
		draft.Bookings = append(draft.Bookings, item)
	}
	return draft, ""
}

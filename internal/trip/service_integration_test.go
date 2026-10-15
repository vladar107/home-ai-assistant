package trip_test

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

const zone = "Europe/Lisbon"

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

type fixture struct {
	t         *testing.T
	clock     *clock
	store     *sqlite.Store
	schedules *schedule.Service
	trips     *trip.Service
}

var start = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 202},
	}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, clock: &clock{now: start}, store: store}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.schedules, err = schedule.NewService(schedule.Config{Zone: zone, Now: f.clock.Now}, store, store, struct{ conversation.Deliverer }{}, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.schedules.Register(schedule.Reminder(f.clock.Now)); err != nil {
		t.Fatal(err)
	}
	f.trips, err = trip.NewService(trip.Config{Now: f.clock.Now}, store, trip.NewScheduleReminders(f.schedules), logger)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func days(n int) time.Time { return start.Add(time.Duration(n) * 24 * time.Hour) }

func booking(kind, title, reference string, startsAt time.Time, checkIn time.Time) trip.Booking {
	b := trip.Booking{Kind: kind, Title: title, Reference: reference, StartsAt: startsAt, CheckInAt: checkIn}
	if !checkIn.IsZero() {
		b.CheckInSource = trip.CheckInStated
	}
	return b
}

func draft(startDay, endDay int, bookings ...trip.Booking) trip.Draft {
	return trip.Draft{Title: "Example trip", Destination: "Example City", StartsAt: days(startDay), EndsAt: days(endDay), Zone: zone, Bookings: bookings}
}

func (f *fixture) reminders(principal string) map[string]schedule.Job {
	f.t.Helper()
	jobs, err := f.schedules.Jobs(f.t.Context(), principal)
	if err != nil {
		f.t.Fatal(err)
	}
	found := map[string]schedule.Job{}
	for _, job := range jobs {
		if job.Type == schedule.TypeReminder {
			found[job.ID] = job
		}
	}
	return found
}

func (f *fixture) add(d trip.Draft) trip.Trip {
	f.t.Helper()
	result, err := f.trips.Add(f.t.Context(), "alice", d)
	if err != nil || result.Rejection != "" {
		f.t.Fatalf("add: %v %q", err, result.Rejection)
	}
	return result.Trip
}

func TestPlannedTripSetsOneReminderPerBooking(t *testing.T) {
	f := newFixture(t)
	flightCheckIn, hotelCheckIn := days(2), days(3)
	planned := f.add(draft(3, 8,
		booking(trip.KindFlight, "Flight to Example City", "ABC123", days(3), flightCheckIn),
		booking(trip.KindHotel, "Example Hotel", "", days(3), hotelCheckIn)))

	reminders := f.reminders("alice")
	if len(reminders) != 2 {
		t.Fatalf("reminders = %d, want 2", len(reminders))
	}
	job := reminders[trip.ReminderID(planned.ID, planned.Bookings[0].ID)]
	if !job.RunAt.Equal(flightCheckIn.Truncate(time.Minute)) || !job.OneTime() {
		t.Fatalf("flight reminder at %v, want %v", job.RunAt, flightCheckIn)
	}
	if text := schedule.ReminderText(job.Config); text != "Check-in opens: Flight to Example City (ABC123)" {
		t.Fatalf("reminder text = %q", text)
	}
	hotel := reminders[trip.ReminderID(planned.ID, planned.Bookings[1].ID)]
	if text := schedule.ReminderText(hotel.Config); text != "Check-in opens: Example Hotel" {
		t.Fatalf("reminder text = %q", text)
	}
	if got := f.reminders("bob"); len(got) != 0 {
		t.Fatalf("another principal has %d reminders", len(got))
	}
}

func TestMovingBookingMovesReminderAndCancelRemovesThem(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(3, 8,
		booking(trip.KindFlight, "Flight to Example City", "ABC123", days(3), days(2)),
		booking(trip.KindHotel, "Example Hotel", "", days(3), days(3))))
	flight, hotel := planned.Bookings[0], planned.Bookings[1]

	moved := flight
	moved.CheckInAt = days(2).Add(5 * time.Hour)
	result, err := f.trips.Update(t.Context(), "alice", planned.ID, time.Time{}, draft(3, 8, moved, hotel))
	if err != nil || result.Rejection != "" || len(result.Unset) != 0 {
		t.Fatalf("update: %v %+v", err, result)
	}
	reminders := f.reminders("alice")
	if len(reminders) != 2 {
		t.Fatalf("reminders = %d, want 2", len(reminders))
	}
	if got := reminders[trip.ReminderID(planned.ID, flight.ID)].RunAt; !got.Equal(moved.CheckInAt) {
		t.Fatalf("moved reminder at %v, want %v", got, moved.CheckInAt)
	}

	if result, err = f.trips.Update(t.Context(), "alice", planned.ID, time.Time{}, draft(3, 8, moved)); err != nil || result.Rejection != "" {
		t.Fatalf("remove booking: %v %q", err, result.Rejection)
	}
	if got := f.reminders("alice"); len(got) != 1 {
		t.Fatalf("reminders after removing a booking = %d, want 1", len(got))
	}

	if result, err = f.trips.Cancel(t.Context(), "alice", planned.ID, time.Time{}); err != nil || result.Rejection != "" {
		t.Fatalf("cancel: %v %q", err, result.Rejection)
	}
	if got := f.reminders("alice"); len(got) != 0 {
		t.Fatalf("reminders after cancel = %d, want 0", len(got))
	}
}

func TestPastCheckInSetsNoReminderAndIsReported(t *testing.T) {
	f := newFixture(t)
	result, err := f.trips.Add(t.Context(), "alice", draft(1, 5,
		booking(trip.KindFlight, "Flight to Example City", "ABC123", days(1), start.Add(-time.Hour)),
		booking(trip.KindHotel, "Example Hotel", "", days(1), days(1))))
	if err != nil || result.Rejection != "" {
		t.Fatalf("add: %v %q", err, result.Rejection)
	}
	if len(result.Unset) != 1 || result.Unset[0].Reason != trip.UnsetPast || result.Unset[0].Title != "Flight to Example City" {
		t.Fatalf("unset = %+v", result.Unset)
	}
	if got := f.reminders("alice"); len(got) != 1 {
		t.Fatalf("reminders = %d, want 1", len(got))
	}
}

func TestReminderCapIsReportedAndTripStaysSaved(t *testing.T) {
	f := newFixture(t)
	var unset int
	for index := range 3 {
		var bookings []trip.Booking
		for n := range trip.MaximumBookings {
			bookings = append(bookings, booking(trip.KindOther, fmt.Sprintf("Booking %d-%d", index, n), "", days(10), days(5).Add(time.Duration(n)*time.Hour)))
		}
		result, err := f.trips.Add(t.Context(), "alice", draft(10, 12, bookings...))
		if err != nil || result.Rejection != "" {
			t.Fatalf("add: %v %q", err, result.Rejection)
		}
		for _, item := range result.Unset {
			if item.Reason != trip.UnsetCap {
				t.Fatalf("unset reason = %q", item.Reason)
			}
			unset++
		}
	}
	if got := len(f.reminders("alice")); got != schedule.MaximumOneTimeJobsPerPrincipal || unset != 60-got {
		t.Fatalf("reminders = %d, unset = %d", got, unset)
	}
	trips, err := f.trips.Trips(t.Context(), "alice")
	if err != nil || len(trips) != 3 {
		t.Fatalf("trips = %d, %v", len(trips), err)
	}
}

func mailTrip(startDay, endDay int, bookings ...trip.Booking) trip.Draft {
	d := draft(startDay, endDay, bookings...)
	d.Source = trip.SourceMail
	return d
}

func TestSuggestDeduplicatesByBookingFingerprint(t *testing.T) {
	f := newFixture(t)
	candidate := mailTrip(3, 8, booking(trip.KindFlight, "Flight to Example City", "ABC123", days(3), days(2)))

	first, err := f.trips.Suggest(t.Context(), "alice", candidate)
	if err != nil || first.Outcome != trip.OutcomeNew || first.Trip.Status != trip.StatusSuggested {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if len(f.reminders("alice")) != 0 {
		t.Fatal("a suggested trip set a reminder")
	}
	again, err := f.trips.Suggest(t.Context(), "alice", candidate)
	if err != nil || again.Outcome != trip.OutcomeKnown || again.Trip.ID != first.Trip.ID {
		t.Fatalf("again = %+v, %v", again, err)
	}
	spelled := mailTrip(3, 8, booking(trip.KindFlight, "Flight", "abc-123", days(3), days(2)))
	if again, err = f.trips.Suggest(t.Context(), "alice", spelled); err != nil || again.Outcome != trip.OutcomeKnown {
		t.Fatalf("respelled reference = %+v, %v", again, err)
	}

	if _, err := f.trips.Dismiss(t.Context(), "alice", first.Trip.ID); err != nil {
		t.Fatal(err)
	}
	if again, err = f.trips.Suggest(t.Context(), "alice", candidate); err != nil || again.Outcome != trip.OutcomeKnown {
		t.Fatalf("dismissed = %+v, %v", again, err)
	}
	other, err := f.trips.Suggest(t.Context(), "bob", candidate)
	if err != nil || other.Outcome != trip.OutcomeNew {
		t.Fatalf("another principal = %+v, %v", other, err)
	}
	if result, _ := f.trips.Suggest(t.Context(), "alice", mailTrip(3, 8, booking(trip.KindHotel, "Example Hotel", "", days(3), time.Time{}))); result.Rejection == "" {
		t.Fatal("a trip with nothing to recognise it by was accepted")
	}
}

func TestSuggestMergesBookingsAndKeepsWhatTheUserAdded(t *testing.T) {
	f := newFixture(t)
	suggested, err := f.trips.Suggest(t.Context(), "alice", mailTrip(3, 8, booking(trip.KindFlight, "Flight to Example City", "ABC123", days(3), days(2))))
	if err != nil {
		t.Fatal(err)
	}
	tracked, _ := f.trips.Track(t.Context(), "alice", suggested.Trip.ID)
	flight := tracked.Trip.Bookings[0]
	userAdded := booking(trip.KindCar, "Rental car", "", days(4), days(4).Add(-time.Hour))
	renamed := tracked.Trip
	renamed.Title = "My own title"
	d := draft(3, 8, flight, userAdded)
	d.Title = renamed.Title
	if result, err := f.trips.Update(t.Context(), "alice", tracked.Trip.ID, time.Time{}, d); err != nil || result.Rejection != "" {
		t.Fatalf("update: %+v, %v", result, err)
	}
	if got := len(f.reminders("alice")); got != 2 {
		t.Fatalf("reminders = %d, want 2", got)
	}

	found := mailTrip(3, 10,
		booking(trip.KindFlight, "Flight to Example City", "ABC123", days(3), days(2).Add(3*time.Hour)),
		booking(trip.KindHotel, "Example Hotel", "HOTEL9", days(3), days(3)))
	found.Title = "Scanned title"
	merged, err := f.trips.Suggest(t.Context(), "alice", found)
	if err != nil || merged.Outcome != trip.OutcomeUpdated || merged.Trip.ID != suggested.Trip.ID {
		t.Fatalf("merged = %+v, %v", merged, err)
	}
	if merged.Previous == nil || len(merged.Previous.Bookings) != 2 || len(merged.Added) != 1 || len(merged.Trip.Bookings) != 3 {
		t.Fatalf("previous = %+v, added = %v, bookings = %d", merged.Previous, merged.Added, len(merged.Trip.Bookings))
	}
	if merged.Trip.Title != "My own title" || !merged.Trip.EndsAt.Equal(days(10)) {
		t.Fatalf("title = %q, ends = %v", merged.Trip.Title, merged.Trip.EndsAt)
	}
	if merged.Trip.Bookings[0].ID != flight.ID || !merged.Trip.Bookings[0].CheckInAt.Equal(days(2).Add(3*time.Hour)) {
		t.Fatalf("flight = %+v", merged.Trip.Bookings[0])
	}
	reminders := f.reminders("alice")
	if len(reminders) != 3 {
		t.Fatalf("reminders = %d, want 3", len(reminders))
	}
	if got := reminders[trip.ReminderID(merged.Trip.ID, flight.ID)].RunAt; !got.Equal(days(2).Add(3 * time.Hour)) {
		t.Fatalf("flight reminder at %v", got)
	}
	if again, _ := f.trips.Suggest(t.Context(), "alice", found); again.Outcome != trip.OutcomeKnown {
		t.Fatalf("repeat = %+v", again)
	}
}

func TestChatTripWithReferenceIsMatchedByALaterScan(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(3, 8, booking(trip.KindFlight, "Flight to Example City", "ABC123", days(3), days(2))))
	found := mailTrip(3, 8, booking(trip.KindFlight, "Flight to Example City", "ABC123", days(3), days(2)))
	result, err := f.trips.Suggest(t.Context(), "alice", found)
	if err != nil || result.Outcome != trip.OutcomeKnown || result.Trip.ID != planned.ID {
		t.Fatalf("scan = %+v, %v", result, err)
	}
	if trips, _ := f.trips.Trips(t.Context(), "alice"); len(trips) != 1 {
		t.Fatalf("trips = %d, want 1", len(trips))
	}
}

func TestSuggestUpdatesPlannedTripWhoseBookingMoved(t *testing.T) {
	f := newFixture(t)
	suggested, _ := f.trips.Suggest(t.Context(), "alice", mailTrip(3, 8, booking(trip.KindFlight, "Flight to Example City", "ABC123", days(3), days(2))))
	if tracked, err := f.trips.Track(t.Context(), "alice", suggested.Trip.ID); err != nil || tracked.Rejection != "" || tracked.Trip.Status != trip.StatusPlanned {
		t.Fatalf("track = %+v, %v", tracked, err)
	}
	if len(f.reminders("alice")) != 1 {
		t.Fatal("tracking set no reminder")
	}
	updated, err := f.trips.Suggest(t.Context(), "alice", mailTrip(4, 9, booking(trip.KindFlight, "Flight to Example City", "ABC123", days(4), days(3))))
	if err != nil || updated.Outcome != trip.OutcomeUpdated || len(updated.Added) != 0 {
		t.Fatalf("updated = %+v, %v", updated, err)
	}
	reminders := f.reminders("alice")
	if len(reminders) != 1 {
		t.Fatalf("reminders = %d, want 1", len(reminders))
	}
	for _, job := range reminders {
		if !job.RunAt.Equal(days(3)) {
			t.Fatalf("reminder at %v, want %v", job.RunAt, days(3))
		}
	}
}

func TestCalendarSourceKeyIdentifiesABooking(t *testing.T) {
	f := newFixture(t)
	event := booking(trip.KindOther, "Conference", "", days(3), time.Time{})
	event.SourceKey = "event-1"
	d := draft(3, 5, event)
	d.Source = trip.SourceCalendar
	first, err := f.trips.Suggest(t.Context(), "alice", d)
	if err != nil || first.Outcome != trip.OutcomeNew {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if again, _ := f.trips.Suggest(t.Context(), "alice", d); again.Outcome != trip.OutcomeKnown {
		t.Fatalf("again = %+v", again)
	}
}

func TestSharedReferenceKeepsOneFingerprint(t *testing.T) {
	f := newFixture(t)
	d := mailTrip(3, 8,
		booking(trip.KindFlight, "Outbound", "ABC123", days(3), time.Time{}),
		booking(trip.KindFlight, "Return", "ABC123", days(8).Add(-time.Hour), time.Time{}))
	first, err := f.trips.Suggest(t.Context(), "alice", d)
	if err != nil || first.Outcome != trip.OutcomeNew || len(first.Trip.Bookings) != 2 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if again, _ := f.trips.Suggest(t.Context(), "alice", d); again.Outcome != trip.OutcomeKnown {
		t.Fatalf("again = %+v", again)
	}
}

func TestTravelingIsHalfOpenOnPlannedTrips(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(2, 5))
	if _, err := f.trips.Suggest(t.Context(), "alice", mailTrip(10, 12, booking(trip.KindFlight, "Flight", "XYZ789", days(10), time.Time{}))); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		at   time.Time
		want bool
	}{
		{days(2).Add(-time.Nanosecond), false},
		{days(2), true},
		{days(5).Add(-time.Nanosecond), true},
		{days(5), false},
		{days(11), false},
	}
	for _, c := range cases {
		got, err := f.trips.Traveling(t.Context(), "alice", c.at)
		if err != nil || got != c.want {
			t.Fatalf("Traveling(%v) = %v, %v; want %v", c.at, got, err, c.want)
		}
	}
	if got, _ := f.trips.Traveling(t.Context(), "bob", days(3)); got {
		t.Fatal("another principal is traveling")
	}
	if _, err := f.trips.Cancel(t.Context(), "alice", planned.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.trips.Traveling(t.Context(), "alice", days(3)); got {
		t.Fatal("a cancelled trip counts as traveling")
	}
}

func TestEndNowEndsAnActiveTrip(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(-1, 5))
	if result, err := f.trips.EndNow(t.Context(), "alice", planned.ID, time.Time{}); err != nil || result.Rejection != "" || !result.Trip.EndsAt.Equal(start) {
		t.Fatalf("end now = %+v, %v", result, err)
	}
	if got, _ := f.trips.Traveling(t.Context(), "alice", start); got {
		t.Fatal("still traveling after ending")
	}
	ahead := f.add(draft(3, 5))
	if result, err := f.trips.EndNow(t.Context(), "alice", ahead.ID, time.Time{}); err != nil || result.Rejection == "" {
		t.Fatalf("ending a trip that has not started: %+v, %v", result, err)
	}
}

func TestNoticesAreOwedOncePerTrip(t *testing.T) {
	f := newFixture(t)
	active := f.add(draft(-1, 5))
	ahead := f.add(draft(3, 5))

	due, err := f.trips.DueNotices(t.Context(), start)
	if err != nil || len(due) != 1 || due[0].Kind != trip.NoticeStart || due[0].Trip.ID != active.ID {
		t.Fatalf("due = %+v, %v", due, err)
	}
	if marked, err := f.trips.MarkStarted(t.Context(), "alice", active.ID, start); err != nil || !marked {
		t.Fatalf("mark started = %v, %v", marked, err)
	}
	if marked, _ := f.trips.MarkStarted(t.Context(), "alice", active.ID, start); marked {
		t.Fatal("start was marked twice")
	}
	if due, _ = f.trips.DueNotices(t.Context(), start); len(due) != 0 {
		t.Fatalf("due after start = %+v", due)
	}

	if _, err := f.trips.Cancel(t.Context(), "alice", active.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.trips.Cancel(t.Context(), "alice", ahead.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	due, err = f.trips.DueNotices(t.Context(), start)
	if err != nil || len(due) != 1 || due[0].Kind != trip.NoticeEnd || due[0].Trip.ID != active.ID {
		t.Fatalf("due after cancel = %+v, %v", due, err)
	}
	if marked, err := f.trips.MarkEnded(t.Context(), "alice", active.ID, start); err != nil || !marked {
		t.Fatalf("mark ended = %v, %v", marked, err)
	}
	if due, _ = f.trips.DueNotices(t.Context(), start); len(due) != 0 {
		t.Fatalf("due after end = %+v", due)
	}
}

func TestEndNoticeIsDueWhenAStartedTripEnds(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(1, 3))
	f.clock.Set(days(2))
	if _, err := f.trips.MarkStarted(t.Context(), "alice", planned.ID, days(2)); err != nil {
		t.Fatal(err)
	}
	if due, _ := f.trips.DueNotices(t.Context(), days(2)); len(due) != 0 {
		t.Fatalf("due during the trip = %+v", due)
	}
	due, err := f.trips.DueNotices(t.Context(), days(3))
	if err != nil || len(due) != 1 || due[0].Kind != trip.NoticeEnd {
		t.Fatalf("due after the trip = %+v, %v", due, err)
	}
}

func TestTurnLine(t *testing.T) {
	f := newFixture(t)
	if line, err := f.trips.TurnLine(t.Context(), "alice"); err != nil || line != "" {
		t.Fatalf("line = %q, %v", line, err)
	}
	f.add(draft(20, 25))
	if line, _ := f.trips.TurnLine(t.Context(), "alice"); line != "" {
		t.Fatalf("a trip beyond 14 days gave %q", line)
	}
	f.add(draft(5, 9))
	line, _ := f.trips.TurnLine(t.Context(), "alice")
	if !strings.Contains(line, "Example City") || !strings.Contains(line, "2026-10-20") || strings.Contains(line, "local time") {
		t.Fatalf("upcoming line = %q", line)
	}
	f.clock.Set(days(6))
	line, _ = f.trips.TurnLine(t.Context(), "alice")
	if !strings.Contains(line, "traveling") || !strings.Contains(line, "local time there is Wed 2026-10-21 13:00") {
		t.Fatalf("active line = %q", line)
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("line spans lines: %q", line)
	}
}

func TestPruneRemovesOldTripsOfEveryStatus(t *testing.T) {
	f := newFixture(t)
	oldSuggested, _ := f.trips.Suggest(t.Context(), "alice", mailTrip(-60, -50, booking(trip.KindFlight, "Flight", "OLD111", days(-60), time.Time{})))
	recentSuggested, _ := f.trips.Suggest(t.Context(), "alice", mailTrip(-20, -10, booking(trip.KindFlight, "Flight", "NEW222", days(-20), time.Time{})))
	oldDismissed, _ := f.trips.Suggest(t.Context(), "alice", mailTrip(-60, -50, booking(trip.KindFlight, "Flight", "OLD333", days(-60), time.Time{})))
	if _, err := f.trips.Dismiss(t.Context(), "alice", oldDismissed.Trip.ID); err != nil {
		t.Fatal(err)
	}
	oldPlanned := f.add(draft(-60, -50, booking(trip.KindFlight, "Flight", "OLD444", days(-60), time.Time{})))
	oldCancelled := f.add(draft(-60, -50))
	if _, err := f.trips.Cancel(t.Context(), "alice", oldCancelled.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	recentPlanned := f.add(draft(-20, -10))

	if err := f.trips.Prune(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{oldSuggested.Trip.ID: false, oldDismissed.Trip.ID: false, oldPlanned.ID: false, oldCancelled.ID: false, recentSuggested.Trip.ID: true, recentPlanned.ID: true} {
		if _, found, err := f.trips.Trip(t.Context(), "alice", id); err != nil || found != want {
			t.Fatalf("trip %s found = %v, want %v (%v)", id, found, want, err)
		}
	}
}

func TestEndNowRemovesRemindersAfterTheNewEnd(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(-1, 6,
		booking(trip.KindHotel, "Example Hotel", "", days(-1), time.Time{}),
		booking(trip.KindFlight, "Return flight", "RET555", days(6).Add(-time.Hour), days(5))))
	if got := len(f.reminders("alice")); got != 1 {
		t.Fatalf("reminders = %d, want 1", got)
	}
	result, err := f.trips.EndNow(t.Context(), "alice", planned.ID, time.Time{})
	if err != nil || result.Rejection != "" {
		t.Fatalf("end now: %+v, %v", result, err)
	}
	if got := len(f.reminders("alice")); got != 0 {
		t.Fatalf("reminders after ending = %d, want 0", got)
	}
	if len(result.Unset) != 1 || result.Unset[0].Reason != trip.UnsetAfterEnd {
		t.Fatalf("unset = %+v", result.Unset)
	}
}

func TestPostponedTripNoticesAgain(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(-1, 5))
	if _, err := f.trips.MarkStarted(t.Context(), "alice", planned.ID, start); err != nil {
		t.Fatal(err)
	}
	if result, err := f.trips.Update(t.Context(), "alice", planned.ID, time.Time{}, draft(2, 8)); err != nil || result.Rejection != "" {
		t.Fatalf("postpone: %+v, %v", result, err)
	}
	f.clock.Set(days(3))
	due, err := f.trips.DueNotices(t.Context(), days(3))
	if err != nil || len(due) != 1 || due[0].Kind != trip.NoticeStart {
		t.Fatalf("due = %+v, %v", due, err)
	}
}

func TestCheckInWithinTheCurrentMinuteIsPast(t *testing.T) {
	f := newFixture(t)
	f.clock.Set(start.Add(30 * time.Second))
	result, err := f.trips.Add(t.Context(), "alice", draft(1, 5,
		booking(trip.KindFlight, "Flight", "ABC123", days(1), start.Add(40*time.Second))))
	if err != nil || result.Rejection != "" || len(result.Unset) != 1 || result.Unset[0].Reason != trip.UnsetPast {
		t.Fatalf("add: %+v, %v", result, err)
	}
	result, err = f.trips.Add(t.Context(), "alice", draft(1, 5,
		booking(trip.KindFlight, "Flight", "DEF456", days(1), start.Add(90*time.Second))))
	if err != nil || len(result.Unset) != 0 || len(f.reminders("alice")) != 1 {
		t.Fatalf("add next minute: %+v, %v", result, err)
	}
}

func TestReminderFiresAtTheRightInstantWhenTheClocksFallBack(t *testing.T) {
	f := newFixture(t)
	repeated := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)
	planned := f.add(draft(8, 12, booking(trip.KindHotel, "Example Hotel", "", days(10), repeated)))
	job := f.reminders("alice")[trip.ReminderID(planned.ID, planned.Bookings[0].ID)]
	if !job.RunAt.Equal(repeated) || !job.NextRunAt.Equal(repeated) {
		t.Fatalf("reminder at %v (next %v), want %v", job.RunAt, job.NextRunAt, repeated)
	}
}

func TestRejectsUnsafeTextAndCaps(t *testing.T) {
	f := newFixture(t)
	for name, mutate := range map[string]func(*trip.Draft){
		"bare domain":         func(d *trip.Draft) { d.Title = "Visit example.com today" },
		"hidden bare domain":  func(d *trip.Draft) { d.Title = "Visit exam\u200bple.com" },
		"link in title":       func(d *trip.Draft) { d.Title = "See https://example.com" },
		"www in destination":  func(d *trip.Draft) { d.Destination = "www.example.com" },
		"long title":          func(d *trip.Draft) { d.Title = strings.Repeat("a", 81) },
		"empty destination":   func(d *trip.Draft) { d.Destination = " " },
		"end before start":    func(d *trip.Draft) { d.EndsAt = d.StartsAt },
		"unknown zone":        func(d *trip.Draft) { d.Zone = "Nowhere/Land" },
		"long reference":      func(d *trip.Draft) { d.Bookings[0].Reference = strings.Repeat("a", 41) },
		"unknown kind":        func(d *trip.Draft) { d.Bookings[0].Kind = "boat" },
		"time without source": func(d *trip.Draft) { d.Bookings[0].CheckInSource = "" },
	} {
		d := draft(3, 5, booking(trip.KindFlight, "Flight", "ABC123", days(3), days(2)))
		mutate(&d)
		if result, err := f.trips.Add(t.Context(), "alice", d); err != nil || result.Rejection == "" {
			t.Fatalf("%s: result = %+v, %v", name, result, err)
		}
	}

	d := draft(3, 5)
	d.Title = "Line one\nline\ttwo\x00"
	if got := f.add(d); got.Title != "Line one line two" {
		t.Fatalf("title = %q", got.Title)
	}

	d = draft(3, 5)
	d.Title = "Hotel St. Example, Terminal 2, Flight LH 123\u202e"
	if got := f.add(d); got.Title != "Hotel St. Example, Terminal 2, Flight LH 123" {
		t.Fatalf("title = %q", got.Title)
	}

	var many []trip.Booking
	for range trip.MaximumBookings + 1 {
		many = append(many, booking(trip.KindOther, "Booking", "", days(3), time.Time{}))
	}
	if result, _ := f.trips.Add(t.Context(), "alice", draft(3, 5, many...)); result.Rejection == "" {
		t.Fatal("too many bookings were accepted")
	}

	for range trip.MaximumUnfinished - 2 {
		f.add(draft(3, 5))
	}
	if result, err := f.trips.Add(t.Context(), "alice", draft(3, 5)); err != nil || result.Rejection == "" {
		t.Fatalf("over the trip cap: %+v, %v", result, err)
	}
}

func TestNoticeIsNotMarkedAfterThePostponedStart(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(-1, 5))
	due, err := f.trips.DueNotices(t.Context(), start)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %+v, %v", due, err)
	}
	if result, err := f.trips.Update(t.Context(), "alice", planned.ID, time.Time{}, draft(3, 8)); err != nil || result.Rejection != "" {
		t.Fatalf("postpone: %+v, %v", result, err)
	}
	if marked, err := f.trips.MarkStarted(t.Context(), "alice", planned.ID, start); err != nil || marked {
		t.Fatalf("marked = %v, %v; the postponed trip is not due", marked, err)
	}
	f.clock.Set(days(4))
	if due, _ = f.trips.DueNotices(t.Context(), days(4)); len(due) != 1 || due[0].Kind != trip.NoticeStart {
		t.Fatalf("due after the postponed start = %+v", due)
	}
}

func TestEndNoticeIsNotMarkedUnlessStartedAndEnded(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(-1, 5))
	if marked, _ := f.trips.MarkEnded(t.Context(), "alice", planned.ID, start); marked {
		t.Fatal("an end notice was recorded for a trip that never started")
	}
	f.trips.MarkStarted(t.Context(), "alice", planned.ID, start)
	if marked, _ := f.trips.MarkEnded(t.Context(), "alice", planned.ID, start); marked {
		t.Fatal("an end notice was recorded for a trip still under way")
	}
}

func TestStaleUpdateIsRejected(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(3, 8))
	f.clock.Set(start.Add(time.Hour))
	if _, err := f.trips.Update(t.Context(), "alice", planned.ID, time.Time{}, draft(3, 9)); err != nil {
		t.Fatal(err)
	}
	result, err := f.trips.Update(t.Context(), "alice", planned.ID, planned.UpdatedAt, draft(3, 10))
	if err != nil || result.Rejection == "" {
		t.Fatalf("stale update = %+v, %v", result, err)
	}
	if result, err = f.trips.Cancel(t.Context(), "alice", planned.ID, planned.UpdatedAt); err != nil || result.Rejection == "" {
		t.Fatalf("stale cancel = %+v, %v", result, err)
	}
}

func TestDateOnlyEndReadsAsThePreviousDay(t *testing.T) {
	f := newFixture(t)
	d := draft(3, 8)
	lisbon, _ := time.LoadLocation(zone)
	d.StartsAt = time.Date(2026, 11, 2, 9, 0, 0, 0, lisbon)
	d.EndsAt = time.Date(2026, 11, 7, 0, 0, 0, 0, lisbon)
	planned := f.add(d)
	if got := planned.EndLabel("2006-01-02 15:04", "2006-01-02"); got != "2026-11-06" {
		t.Fatalf("end = %s", got)
	}
}

func TestWeakerCheckInSourceNeverReplacesAStrongerOne(t *testing.T) {
	f := newFixture(t)
	stated := booking(trip.KindFlight, "Flight", "ABC123", days(10), days(9))
	first, _ := f.trips.Suggest(t.Context(), "alice", mailTrip(10, 15, stated))
	weaker := stated
	weaker.CheckInAt, weaker.CheckInSource = days(9).Add(5*time.Hour), trip.CheckInDefault
	if result, _ := f.trips.Suggest(t.Context(), "alice", mailTrip(10, 15, weaker)); result.Outcome != trip.OutcomeKnown {
		t.Fatalf("weaker find = %+v", result)
	}
	if got, _, _ := f.trips.Trip(t.Context(), "alice", first.Trip.ID); !got.Bookings[0].CheckInAt.Equal(days(9)) {
		t.Fatalf("check-in = %v", got.Bookings[0].CheckInAt)
	}
}

func TestSuggestedTripsHaveTheirOwnCap(t *testing.T) {
	f := newFixture(t)
	for index := range trip.MaximumSuggested {
		result, err := f.trips.Suggest(t.Context(), "alice", mailTrip(10, 12, booking(trip.KindFlight, "Flight", fmt.Sprintf("REF%03d", index), days(10), time.Time{})))
		if err != nil || result.Outcome != trip.OutcomeNew {
			t.Fatalf("suggest %d: %+v, %v", index, result, err)
		}
	}
	if result, _ := f.trips.Suggest(t.Context(), "alice", mailTrip(10, 12, booking(trip.KindFlight, "Flight", "EXTRA1", days(10), time.Time{}))); result.Rejection == "" {
		t.Fatal("suggested trips passed their cap")
	}
	for range trip.MaximumUnfinished {
		f.add(draft(3, 5))
	}
	trips, _ := f.trips.Trips(t.Context(), "alice")
	var suggested trip.Trip
	for _, item := range trips {
		if item.Status == trip.StatusSuggested {
			suggested = item
			break
		}
	}
	if result, _ := f.trips.Track(t.Context(), "alice", suggested.ID); result.Rejection == "" {
		t.Fatal("tracking passed the planned cap")
	}
}

func TestReminderOfACheckInThatJustOpenedIsLeftToFire(t *testing.T) {
	f := newFixture(t)
	checkIn := start.Add(time.Hour)
	planned := f.add(draft(3, 8, booking(trip.KindFlight, "Flight", "ABC123", days(3), checkIn)))
	f.clock.Set(checkIn.Add(10 * time.Second))
	if result, err := f.trips.Update(t.Context(), "alice", planned.ID, time.Time{}, draft(3, 9, planned.Bookings[0])); err != nil || result.Rejection != "" {
		t.Fatalf("update: %+v, %v", result, err)
	}
	if got := len(f.reminders("alice")); got != 1 {
		t.Fatalf("reminders = %d, want the one about to fire", got)
	}
}

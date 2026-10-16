package trip_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// recordingTabs stands for the Telegram side, which hears of each trip write.
type recordingTabs struct {
	mu      sync.Mutex
	changed []string
}

func (r *recordingTabs) TripChanged(_ context.Context, changed trip.Trip) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changed = append(r.changed, changed.ID+" "+changed.Status)
}

func (r *recordingTabs) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	taken := r.changed
	r.changed = nil
	return taken
}

// A check-in reminder is delivered to its trip, and an edit through the scheduler, as
// schedule_update makes one, keeps that.
func TestCheckInRemindersGoToTheirTripAndKeepItWhenEdited(t *testing.T) {
	f := newFixture(t)
	tabs := &recordingTabs{}
	f.trips.SetTabs(tabs)
	planned := f.add(draft(3, 6, booking(trip.KindFlight, "Flight to Example City", "ABC123", days(3), days(2))))
	if changed := tabs.take(); !slices.Equal(changed, []string{planned.ID + " planned"}) {
		t.Fatalf("tabs heard of %v, want the new trip", changed)
	}
	id := trip.ReminderID(planned.ID, planned.Bookings[0].ID)
	job := f.reminders("alice")[id]
	if job.Destination != thread.TripDestination(planned.ID) {
		t.Fatalf("reminder destination = %q, want the trip's", job.Destination)
	}

	location, err := schedule.LoadZone(zone)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := f.schedules.Prepare("alice", job.Name, schedule.TypeReminder,
		schedule.Timing{At: days(2).Add(time.Hour).In(location).Format("2006-01-02 15:04"), Zone: zone}, job.Config)
	if err != nil {
		t.Fatal(err)
	}
	if updated, err := f.schedules.Update(t.Context(), job, job.UpdatedAt, prepared); err != nil || !updated {
		t.Fatalf("edit = %v %v", updated, err)
	}
	if edited := f.reminders("alice")[id]; edited.Destination != job.Destination || !edited.RunAt.Equal(days(2).Add(time.Hour)) {
		t.Fatalf("edited reminder = %+v, want it moved with its destination", edited)
	}

	cancelled, err := f.trips.Cancel(t.Context(), "alice", planned.ID, time.Time{})
	if err != nil || cancelled.Rejection != "" {
		t.Fatalf("cancel: %v %q", err, cancelled.Rejection)
	}
	if changed := tabs.take(); !slices.Equal(changed, []string{planned.ID + " cancelled"}) {
		t.Fatalf("tabs heard of %v, want the cancelled trip", changed)
	}
}

// At start every planned trip that has not ended gives its destination to those of its reminders
// that exist without one, and changes nothing else: a reminder the user moved keeps its time, one
// the user deleted stays deleted, and one whose check-in has just opened, which may still be
// firing, is left as it is. Running it again changes nothing. Each such trip is handed to the tabs.
func TestSettleOnlyGivesExistingRemindersTheirTrip(t *testing.T) {
	f := newFixture(t)
	tabs := &recordingTabs{}
	f.trips.SetTabs(tabs)
	planned := f.add(draft(1, 4,
		booking(trip.KindFlight, "Flight to Example City", "ABC123", days(1), start.Add(time.Hour)),
		booking(trip.KindHotel, "Example Hotel", "H-77", days(1), days(1).Add(-time.Hour)),
		booking(trip.KindTrain, "Train to Example Town", "T-42", days(2), days(2).Add(-time.Hour))))
	f.add(draft(-5, -1))
	cancelled := f.add(draft(2, 3))
	if result, err := f.trips.Cancel(t.Context(), "alice", cancelled.ID, time.Time{}); err != nil || result.Rejection != "" {
		t.Fatalf("cancel: %v %q", err, result.Rejection)
	}
	tabs.take()
	flight := trip.ReminderID(planned.ID, planned.Bookings[0].ID)
	hotel := trip.ReminderID(planned.ID, planned.Bookings[1].ID)
	train := trip.ReminderID(planned.ID, planned.Bookings[2].ID)

	// The user moved the hotel's reminder before reminders had a destination, and deleted the
	// train's.
	old := f.reminders("alice")[hotel]
	for _, id := range []string{hotel, train} {
		if _, err := f.schedules.Delete(t.Context(), "alice", id); err != nil {
			t.Fatal(err)
		}
	}
	location, err := schedule.LoadZone(zone)
	if err != nil {
		t.Fatal(err)
	}
	moved := old.RunAt.Add(-2 * time.Hour)
	prepared, err := f.schedules.Prepare("alice", old.Name, schedule.TypeReminder,
		schedule.Timing{At: moved.In(location).Format("2006-01-02 15:04"), Zone: zone}, old.Config)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := f.schedules.Create(t.Context(), hotel, prepared); err != nil || !created {
		t.Fatalf("recreate: %v %v", created, err)
	}

	// The flight's check-in opened half a minute ago; its reminder has not been run yet.
	f.clock.Set(start.Add(time.Hour + 30*time.Second))
	firing := f.reminders("alice")[flight]
	for range 2 {
		if err := f.trips.Settle(t.Context()); err != nil {
			t.Fatal(err)
		}
		reminders := f.reminders("alice")
		if len(reminders) != 2 {
			t.Fatalf("reminders after settling = %v, want the flight's and the hotel's only", reminders)
		}
		if kept := reminders[flight]; !kept.UpdatedAt.Equal(firing.UpdatedAt) || !kept.RunAt.Equal(firing.RunAt) || kept.Destination != firing.Destination {
			t.Fatalf("firing reminder = %+v, want it untouched as %+v", kept, firing)
		}
		if reminders[hotel].Destination != thread.TripDestination(planned.ID) || !reminders[hotel].RunAt.Equal(moved) {
			t.Fatalf("hotel reminder = %+v, want it at the user's time with the trip's destination", reminders[hotel])
		}
		if changed := tabs.take(); !slices.Equal(changed, []string{planned.ID + " planned"}) {
			t.Fatalf("tabs heard of %v, want only the planned trip", changed)
		}
	}
}

// blockingTabs holds the first trip it hears of until released, as a slow Telegram would.
type blockingTabs struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingTabs) TripChanged(context.Context, trip.Trip) {
	first := false
	b.once.Do(func() { first = true })
	if first {
		close(b.entered)
		<-b.release
	}
}

// The tabs hear of a write after the trip service let go of its lock, so a slow Telegram holding
// up one trip's tab never holds up another write.
func TestTripWritesDoNotWaitForTheTabs(t *testing.T) {
	f := newFixture(t)
	tabs := &blockingTabs{entered: make(chan struct{}), release: make(chan struct{})}
	f.trips.SetTabs(tabs)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.add(draft(3, 6))
	}()
	select {
	case <-tabs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the tabs never heard of the first trip")
	}
	added := make(chan error, 1)
	go func() {
		result, err := f.trips.Add(t.Context(), "bob", draft(4, 7))
		if err == nil && result.Rejection != "" {
			err = errors.New(result.Rejection)
		}
		added <- err
	}()
	select {
	case err := <-added:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write waited for the tabs of another trip")
	}
	close(tabs.release)
	<-done
}

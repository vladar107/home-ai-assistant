package sqlite

import (
	"reflect"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/trip"
)

var tripBase = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func addTestTrip(t *testing.T, store *Store, principal, id string, endsAt time.Time) {
	t.Helper()
	if err := store.AddTrip(t.Context(), trip.Trip{
		ID: id, PrincipalID: principal, Title: "Conference", Destination: "Lisbon",
		StartsAt: endsAt.Add(-72 * time.Hour), EndsAt: endsAt, Zone: "Europe/Lisbon",
		Status: trip.StatusPlanned, Source: trip.SourceChat, CreatedAt: tripBase, UpdatedAt: tripBase,
	}); err != nil {
		t.Fatal(err)
	}
}

func tableRows(t *testing.T, store *Store, table string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSentTripUpdatesListNewestFirstWithLimit(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	addTestTrip(t, store, "alice", "aaaaaaaa", tripBase.Add(96*time.Hour))
	items := []trip.SentUpdate{
		{Source: trip.SourceMail, Reference: "msg-1", Origin: "Organiser", Title: "Doors moved", Text: "Doors open at 18:00.", SentAt: tripBase},
		{Source: "web", Reference: "example.org/news", Origin: "example.org", Title: "Strike", Text: "Trains are off on Friday.", SentAt: tripBase.Add(time.Hour)},
		{Source: trip.SourceCalendar, Reference: "ev-1:moved", Title: "Dinner moved", Text: "Now at 20:00.", SentAt: tripBase.Add(2 * time.Hour)},
	}
	if err := store.RecordSentUpdates(ctx, "alice", "aaaaaaaa", items); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListSentUpdates(ctx, "alice", "aaaaaaaa", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []trip.SentUpdate{items[2], items[1], items[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %+v, want %+v", got, want)
	}
	if got, err = store.ListSentUpdates(ctx, "alice", "aaaaaaaa", 2); err != nil || len(got) != 2 || got[0].Reference != "ev-1:moved" {
		t.Fatalf("limited updates = %+v %v", got, err)
	}
}

func TestSentTripUpdateWithKnownReferenceIsIgnored(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	addTestTrip(t, store, "alice", "aaaaaaaa", tripBase.Add(96*time.Hour))
	first := trip.SentUpdate{Source: trip.SourceMail, Reference: "msg-1", Origin: "Organiser", Title: "First", Text: "First text.", SentAt: tripBase}
	again := trip.SentUpdate{Source: trip.SourceMail, Reference: "msg-1", Origin: "Organiser", Title: "Second", Text: "Second text.", SentAt: tripBase.Add(time.Hour)}
	otherSource := trip.SentUpdate{Source: "web", Reference: "msg-1", Title: "Web", Text: "Web text.", SentAt: tripBase.Add(time.Hour)}
	if err := store.RecordSentUpdates(ctx, "alice", "aaaaaaaa", []trip.SentUpdate{first}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSentUpdates(ctx, "alice", "aaaaaaaa", []trip.SentUpdate{again, otherSource}); err != nil {
		t.Fatalf("duplicate reference: %v", err)
	}
	got, err := store.ListSentUpdates(ctx, "alice", "aaaaaaaa", 10)
	if err != nil || len(got) != 2 || got[0] != otherSource || got[1] != first {
		t.Fatalf("updates = %+v %v", got, err)
	}
}

func TestTripUpdateStateRoundTrip(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	addTestTrip(t, store, "alice", "aaaaaaaa", tripBase.Add(96*time.Hour))
	if _, found, err := store.LoadUpdateState(ctx, "alice", "aaaaaaaa"); err != nil || found {
		t.Fatalf("state before saving = %v %v", found, err)
	}
	empty := trip.UpdateState{CalendarSnapshot: []byte("[]"), Missing: []string{}}
	if err := store.SaveUpdateState(ctx, "alice", "aaaaaaaa", trip.UpdateState{}); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.LoadUpdateState(ctx, "alice", "aaaaaaaa")
	if err != nil || !found || !reflect.DeepEqual(got, empty) {
		t.Fatalf("empty state = %+v %v %v", got, found, err)
	}
	full := trip.UpdateState{
		CalendarSnapshot: []byte(`[{"id":"ev-1","title":"Dinner","start":"2026-11-03T19:00:00Z"}]`),
		Missing:          []string{"ev-2", "ev-3"},
		LastAttemptAt:    tripBase,
		LastSuccessAt:    tripBase.Add(time.Minute),
		MailReadAt:       tripBase.Add(-time.Hour),
	}
	if err := store.SaveUpdateState(ctx, "alice", "aaaaaaaa", full); err != nil {
		t.Fatal(err)
	}
	if got, found, err = store.LoadUpdateState(ctx, "alice", "aaaaaaaa"); err != nil || !found || !reflect.DeepEqual(got, full) {
		t.Fatalf("state = %+v %v %v, want %+v", got, found, err, full)
	}
	if err := store.SaveUpdateState(ctx, "alice", "aaaaaaaa", empty); err != nil {
		t.Fatal(err)
	}
	if got, _, err = store.LoadUpdateState(ctx, "alice", "aaaaaaaa"); err != nil || !got.LastAttemptAt.IsZero() || !got.MailReadAt.IsZero() {
		t.Fatalf("state after clearing = %+v %v", got, err)
	}
}

func TestMarkingTripUpdateAttemptKeepsTheRestOfTheState(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	addTestTrip(t, store, "alice", "aaaaaaaa", tripBase.Add(96*time.Hour))
	if err := store.MarkUpdateAttempt(ctx, "alice", "aaaaaaaa", tripBase); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.LoadUpdateState(ctx, "alice", "aaaaaaaa")
	if err != nil || !found || !got.LastAttemptAt.Equal(tripBase) || !got.LastSuccessAt.IsZero() || string(got.CalendarSnapshot) != "[]" {
		t.Fatalf("state after the first attempt = %+v %v %v", got, found, err)
	}
	saved := trip.UpdateState{
		CalendarSnapshot: []byte(`[{"id":"ev-1"}]`), Missing: []string{"ev-2"},
		LastAttemptAt: tripBase, LastSuccessAt: tripBase, MailReadAt: tripBase,
	}
	if err := store.SaveUpdateState(ctx, "alice", "aaaaaaaa", saved); err != nil {
		t.Fatal(err)
	}
	later := tripBase.Add(9 * time.Hour)
	if err := store.MarkUpdateAttempt(ctx, "alice", "aaaaaaaa", later); err != nil {
		t.Fatal(err)
	}
	saved.LastAttemptAt = later
	if got, _, err = store.LoadUpdateState(ctx, "alice", "aaaaaaaa"); err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatalf("state after the second attempt = %+v %v, want %+v", got, err, saved)
	}
}

func TestTripUpdatesGoWithTheirTrip(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	addTestTrip(t, store, "alice", "aaaaaaaa", tripBase.Add(-24*time.Hour))
	addTestTrip(t, store, "alice", "bbbbbbbb", tripBase.Add(96*time.Hour))
	for _, id := range []string{"aaaaaaaa", "bbbbbbbb"} {
		if err := store.RecordSentUpdates(ctx, "alice", id, []trip.SentUpdate{{Source: trip.SourceMail, Reference: "msg", Title: "News", Text: "Text.", SentAt: tripBase}}); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkUpdateAttempt(ctx, "alice", id, tripBase); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := store.PruneTrips(ctx, tripBase); err != nil || removed != 1 {
		t.Fatalf("pruned = %d %v", removed, err)
	}
	if sent, states := tableRows(t, store, "trip_updates_sent"), tableRows(t, store, "trip_update_states"); sent != 1 || states != 1 {
		t.Fatalf("rows after pruning = %d sent, %d states", sent, states)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM trips`); err != nil {
		t.Fatal(err)
	}
	if sent, states := tableRows(t, store, "trip_updates_sent"), tableRows(t, store, "trip_update_states"); sent != 0 || states != 0 {
		t.Fatalf("rows after deleting trips = %d sent, %d states", sent, states)
	}
}

func TestTripUpdatesBelongToTheTripsOwner(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	addTestTrip(t, store, "alice", "aaaaaaaa", tripBase.Add(96*time.Hour))
	item := trip.SentUpdate{Source: trip.SourceMail, Reference: "msg", Title: "News", Text: "Text.", SentAt: tripBase}
	if err := store.RecordSentUpdates(ctx, "bob", "aaaaaaaa", []trip.SentUpdate{item}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUpdateAttempt(ctx, "bob", "aaaaaaaa", tripBase); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveUpdateState(ctx, "bob", "aaaaaaaa", trip.UpdateState{MailReadAt: tripBase}); err != nil {
		t.Fatal(err)
	}
	if sent, states := tableRows(t, store, "trip_updates_sent"), tableRows(t, store, "trip_update_states"); sent != 0 || states != 0 {
		t.Fatalf("another principal wrote %d sent, %d states", sent, states)
	}
	if err := store.RecordSentUpdates(ctx, "alice", "aaaaaaaa", []trip.SentUpdate{item}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUpdateAttempt(ctx, "alice", "aaaaaaaa", tripBase); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ListSentUpdates(ctx, "bob", "aaaaaaaa", 10); err != nil || len(got) != 0 {
		t.Fatalf("another principal's updates = %+v %v", got, err)
	}
	if _, found, err := store.LoadUpdateState(ctx, "bob", "aaaaaaaa"); err != nil || found {
		t.Fatalf("another principal's state = %v %v", found, err)
	}
}

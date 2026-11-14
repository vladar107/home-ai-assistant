package sqlite

import (
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// Migration 060 rebuilds trip_updates_sent to take flight changes: every row sent before it keeps
// its fields, the recent index is back, the trip's cascade still holds, and a flight change can be
// recorded.
func TestMigrationTripFlightsKeepsEverySentUpdate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := migrateUpTo(t, ctx, "060")
	exec(t, ctx, db,
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO trips (id, principal_id, title, destination, starts_at, ends_at, zone, status, source, created_at, updated_at)
		 VALUES ('aaaaaaaa', 'alice', 'Example trip', 'Example City', '2026-11-09T10:00:00.000Z', '2026-11-12T10:00:00.000Z', 'Europe/Lisbon', 'planned', 'chat', '2026-11-01T10:00:00.000Z', '2026-11-01T10:00:00.000Z')`,
		`INSERT INTO trip_updates_sent (trip_id, principal_id, source, reference, origin, title, text, sent_at)
		 VALUES ('aaaaaaaa', 'alice', 'mail', 'message-1', 'Example Hotel', '', 'The check-in moved to 15:00.', '2026-11-05T10:00:00.000Z')`,
		`INSERT INTO trip_updates_sent (trip_id, principal_id, source, reference, origin, title, text, sent_at)
		 VALUES ('aaaaaaaa', 'alice', 'calendar', 'event-1:moved:abc', 'Calendar', 'Dinner', 'Dinner: Tue 10 Nov 19:00 (was 18:00)', '2026-11-06T10:00:00.000Z')`,
	)

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	sent, err := store.ListSentUpdates(ctx, "alice", "aaaaaaaa", 10)
	if err != nil || len(sent) != 2 {
		t.Fatalf("sent after migration = %+v %v", sent, err)
	}
	if sent[0].Source != "calendar" || sent[0].Title != "Dinner" || sent[1].Source != "mail" || sent[1].Origin != "Example Hotel" ||
		sent[1].Text != "The check-in moved to 15:00." || !sent[1].SentAt.Equal(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("sent after migration = %+v", sent)
	}
	if count := countRows(t, store, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'trip_updates_sent_recent_idx' AND tbl_name = 'trip_updates_sent'`); count != 1 {
		t.Fatal("the recent index is missing")
	}
	if err := store.RecordSentUpdates(ctx, "alice", "aaaaaaaa", []trip.SentUpdate{{
		Source: trip.UpdateSourceFlight, Reference: "EX123|2026-11-09|gate|2026-11-09T10:40:00Z", Origin: "AeroDataBox",
		Text: "EX123: gate B12 (was B7).", SentAt: time.Date(2026, 11, 9, 10, 40, 0, 0, time.UTC),
	}}); err != nil {
		t.Fatal(err)
	}
	foreignKeysHold(t, ctx, db)
	exec(t, ctx, db, `DELETE FROM trips WHERE id = 'aaaaaaaa'`)
	if count := countRows(t, store, `SELECT count(*) FROM trip_updates_sent`); count != 0 {
		t.Fatalf("%d sent updates outlived their trip", count)
	}
}

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// migrateUpTo applies the migrations before the named one, as an older database has them.
func migrateUpTo(t *testing.T, ctx context.Context, before string) *sql.DB {
	t.Helper()
	path, err := filepath.Abs(filepath.Join(t.TempDir(), "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.name >= before {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-10-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func exec(t *testing.T, ctx context.Context, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func foreignKeysHold(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var problems int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&problems); err != nil || problems != 0 {
		t.Fatalf("foreign key check = %d %v", problems, err)
	}
}

// Migration 047 rebuilds both schedule tables; every job and run must survive with the columns
// later migrations added, the travel flag, the destination, and the delivery progress, and the
// rebuilt tables must take repeating anchored jobs and refuse a job with two timings.
func TestMigrationAnchoredJobsKeepsEveryJobAndRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := migrateUpTo(t, ctx, "047")
	exec(t, ctx, db,
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO scheduled_jobs (id, principal_id, name, job_type, cron, zone, config, enabled, next_run_at, created_at, updated_at, travel_pause)
		 VALUES ('0123456789abcdef', 'alice', 'Digest', 'model.turn', '0 9 * * *', 'Europe/Amsterdam', '{"prompt":"x"}', 0, '2026-10-05T07:00:00.000Z', '2026-09-01T00:00:00.000Z', '2026-09-02T00:00:00.000Z', 1)`,
		`INSERT INTO scheduled_jobs (id, principal_id, name, job_type, run_at, zone, config, enabled, next_run_at, finished_at, created_at, updated_at, destination)
		 VALUES ('fedcba9876543210', 'alice', 'Check in', 'reminder.text', '2026-10-04T15:00:00.000Z', 'Europe/Amsterdam', '{"text":"Check in"}', 1, '2026-10-04T15:00:00.000Z', '2026-10-04T15:00:01.000Z', '2026-09-01T00:00:00.000Z', '2026-10-04T15:00:01.000Z', 'trip:abcd1234')`,
		`INSERT INTO schedule_runs (id, job_id, principal_id, scheduled_for, status, reply, created_at, updated_at, delivered_chunks, delivered_messages)
		 VALUES ('1111111111111111', '0123456789abcdef', 'alice', '2026-10-04T07:00:00.000Z', 'ready', 'report', '2026-10-04T07:00:00.000Z', '2026-10-04T07:00:05.000Z', 2, '[{"message_id":7,"thread_id":3}]')`,
		`INSERT INTO schedule_runs (id, job_id, principal_id, scheduled_for, manual, status, error_kind, created_at, updated_at, finished_at)
		 VALUES ('2222222222222222', 'fedcba9876543210', 'alice', '2026-10-04T15:00:00.000Z', 0, 'delivered', 'late', '2026-10-04T15:00:00.000Z', '2026-10-04T15:00:02.000Z', '2026-10-04T15:00:02.000Z')`,
	)

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	digest, found, err := store.LoadScheduledJob(ctx, "alice", "0123456789abcdef")
	if err != nil || !found || digest.Cron != "0 9 * * *" || digest.Enabled || !digest.TravelPause || digest.AnchorRepeats ||
		!digest.NextRunAt.Equal(time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("recurring job after migration = %+v %v %v", digest, found, err)
	}
	checkIn, found, err := store.LoadScheduledJob(ctx, "alice", "fedcba9876543210")
	if err != nil || !found || !checkIn.OneTime() || checkIn.FinishedAt.IsZero() || checkIn.Destination != "trip:abcd1234" {
		t.Fatalf("one-time job after migration = %+v %v %v", checkIn, found, err)
	}
	runs, err := store.ScheduledRuns(ctx, schedule.StatusReady)
	if err != nil || len(runs) != 1 || runs[0].Reply != "report" || runs[0].DeliveredChunks != 2 || len(runs[0].Delivered) != 1 || runs[0].Delivered[0].MessageID != 7 {
		t.Fatalf("ready run after migration = %+v %v", runs, err)
	}
	var status, kind string
	if err := db.QueryRowContext(ctx, `SELECT status, error_kind FROM schedule_runs WHERE id = '2222222222222222'`).Scan(&status, &kind); err != nil || status != "delivered" || kind != "late" {
		t.Fatalf("finished run after migration = %q %q %v", status, kind, err)
	}

	var indexes int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index'
		AND name IN ('scheduled_jobs_due_idx', 'scheduled_jobs_principal_idx', 'schedule_runs_open_idx')`).Scan(&indexes); err != nil || indexes != 3 {
		t.Fatalf("job indexes after migration = %d %v", indexes, err)
	}

	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	repeating := schedule.Job{
		ID: "00000000000000aa", PrincipalID: "alice", Name: "Agenda", Type: schedule.TypeReminder, Zone: "Europe/Amsterdam",
		Config:  json.RawMessage(`{"text":"Agenda","anchor":{"kind":"calendar_match","title":"Planning","offset_minutes":-60}}`),
		Enabled: true, NextRunAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now, AnchorRepeats: true, OccurrenceKey: "primary/p1",
	}
	if created, err := store.CreateScheduledJob(ctx, repeating, schedule.MaximumJobsPerPrincipal); err != nil || !created {
		t.Fatalf("repeating anchored job = %v %v", created, err)
	}
	if stored, _, err := store.LoadScheduledJob(ctx, "alice", repeating.ID); err != nil || !stored.AnchorRepeats || stored.OccurrenceKey != "primary/p1" || stored.OneTime() {
		t.Fatalf("stored anchored job = %+v %v", stored, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE scheduled_jobs SET cron = '0 9 * * *' WHERE id = ?`, repeating.ID); err == nil {
		t.Fatal("a job took both an anchor and a cron expression")
	}
	foreignKeysHold(t, ctx, db)
}

// Migration 048 rebuilds the three watch tables to admit weather and place watches; every watch,
// window state, and alert must survive with the travel flag, and the rebuilt tables must take a
// weather watch and its alert.
func TestMigrationWeatherAndPlaceWatchesKeepsEveryWatch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := migrateUpTo(t, ctx, "048")
	exec(t, ctx, db,
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO watches (id, principal_id, kind, config, enabled, muted_on, created_at, updated_at, travel_pause)
		 VALUES ('0123456789abcdef', 'alice', 'departure', '{"kind":"departure","language":"Russian"}', 0, '2026-10-05', '2026-09-01T00:00:00.000Z', '2026-09-02T00:00:00.000Z', 1)`,
		`INSERT INTO watches (id, principal_id, kind, config, enabled, created_at, updated_at)
		 VALUES ('fedcba9876543210', 'alice', 'agent', '{"kind":"agent","target":{"name":"Release date"}}', 1, '2026-09-01T00:00:00.000Z', '2026-09-01T00:00:00.000Z')`,
		`INSERT INTO watch_states (watch_id, principal_id, occurrence, state, alerted, messages, failures, failure_noticed, since, checked_at, updated_at)
		 VALUES ('fedcba9876543210', 'alice', '2026-10-05', 'met|1 december', 1, 4, 2, 1, '2026-10-05T06:00:00.000Z', '2026-10-05T06:20:00.000Z', '2026-10-05T06:20:00.000Z')`,
		`INSERT INTO watch_alerts (id, watch_id, principal_id, occurrence, sequence, type, text, status, expires_at, private, created_at, updated_at)
		 VALUES ('1111111111111111', 'fedcba9876543210', 'alice', '2026-10-05', 4, 'alert', 'announced', 'ready', '2026-10-05T06:50:00.000Z', 1, '2026-10-05T06:20:00.000Z', '2026-10-05T06:20:00.000Z')`,
		`INSERT INTO watch_alerts (id, watch_id, principal_id, occurrence, sequence, type, text, status, reason, expires_at, created_at, updated_at, finished_at)
		 VALUES ('2222222222222222', '0123456789abcdef', 'alice', '2026-10-05', 1, 'alert', NULL, 'dropped', 'muted', '2026-10-05T06:42:00.000Z', '2026-10-05T06:20:00.000Z', '2026-10-05T06:21:00.000Z', '2026-10-05T06:21:00.000Z')`,
	)

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	watches, err := store.ListWatches(ctx, "alice")
	if err != nil || len(watches) != 2 || watches[0].Kind != watch.KindDeparture || watches[0].Enabled || watches[0].MutedOn != "2026-10-05" || !watches[0].TravelPause ||
		watches[1].Kind != watch.KindAgent || watches[1].TravelPause {
		t.Fatalf("watches after migration = %+v %v", watches, err)
	}
	state, found, err := store.LoadWatchState(ctx, "fedcba9876543210", "2026-10-05")
	if err != nil || !found || state.Key != "met|1 december" || !state.Alerted || state.Messages != 4 || state.Failures != 2 || !state.FailureNoticed {
		t.Fatalf("state after migration = %+v %v %v", state, found, err)
	}
	alerts, err := store.ReadyWatchAlerts(ctx)
	if err != nil || len(alerts) != 1 || alerts[0].ID != "1111111111111111" || !alerts[0].Private {
		t.Fatalf("alerts after migration = %+v %v", alerts, err)
	}
	var reason string
	if err := db.QueryRowContext(ctx, `SELECT reason FROM watch_alerts WHERE id = '2222222222222222'`).Scan(&reason); err != nil || reason != "muted" {
		t.Fatalf("finished alert after migration = %q %v", reason, err)
	}
	var indexes int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name IN ('watches_principal_idx', 'watch_alerts_ready_idx')`).Scan(&indexes); err != nil || indexes != 2 {
		t.Fatalf("watch indexes after migration = %d %v", indexes, err)
	}

	created := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	latitude, longitude := 52.1, 5.1
	rain := watch.Watch{
		ID: "00000000000000aa", PrincipalID: "alice", Enabled: true, TravelPause: true, CreatedAt: created, UpdatedAt: created,
		Spec: watch.Spec{
			Kind: watch.KindWeather, Target: watch.Target{Name: "Exampletown", Latitude: &latitude, Longitude: &longitude},
			Weather: &watch.Weather{WithinMinutes: 60, ThresholdMM: 0.2}, Triggers: []string{watch.TriggerEnter, watch.TriggerClear},
			Days: []string{"mon"}, Start: "00:00", End: "24:00", Zone: "Europe/Amsterdam",
		},
	}
	if ok, err := store.CreateWatch(ctx, rain, watch.MaximumWatchesPerPrincipal); err != nil || !ok {
		t.Fatalf("weather watch after migration = %v %v", ok, err)
	}
	if stored, found, err := store.LoadWatch(ctx, "alice", rain.ID); err != nil || !found || stored.Weather == nil || *stored.Weather != *rain.Weather || !stored.TravelPause {
		t.Fatalf("stored weather watch = %+v %v %v", stored, found, err)
	}
	exec(t, ctx, db, `INSERT INTO watches (id, principal_id, kind, config, created_at, updated_at) VALUES ('00000000000000bb', 'alice', 'place', '{"kind":"place"}', '2026-10-06T09:00:00.000Z', '2026-10-06T09:00:00.000Z')`)
	foreignKeysHold(t, ctx, db)
}

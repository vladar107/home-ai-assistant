package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// Migration 022 rebuilds the three watch tables; the transit watch, window state, and queued alert
// stored before it must survive with their values, and the rebuilt tables must accept a home
// watch, ten messages in a window, and the start of a condition.
func TestMigrationHomeWatchesKeepsExistingRows(t *testing.T) {
	ctx := t.Context()
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
		if m.name >= "022" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-09-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO watches (id, principal_id, kind, config, enabled, muted_on, created_at, updated_at)
		 VALUES ('0123456789abcdef', 'alice', 'departure', '{"kind":"departure","language":"Russian"}', 0, '2026-10-05', '2026-09-01T00:00:00.000Z', '2026-09-02T00:00:00.000Z')`,
		`INSERT INTO watch_states (watch_id, principal_id, occurrence, state, alerted, messages, failures, failure_noticed, checked_at, updated_at)
		 VALUES ('0123456789abcdef', 'alice', '2026-10-05', 'cancelled', 1, 3, 0, 0, '2026-10-05T06:20:00.000Z', '2026-10-05T06:20:00.000Z')`,
		`INSERT INTO watch_alerts (id, watch_id, principal_id, occurrence, sequence, type, text, status, expires_at, created_at, updated_at)
		 VALUES ('1111111111111111', '0123456789abcdef', 'alice', '2026-10-05', 3, 'alert', 'cancelled', 'ready', '2026-10-05T06:42:00.000Z', '2026-10-05T06:20:00.000Z', '2026-10-05T06:20:00.000Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}

	existing, found, err := store.LoadWatch(ctx, "alice", "0123456789abcdef")
	if err != nil || !found {
		t.Fatalf("watch after migration = %v %v", found, err)
	}
	if existing.Kind != watch.KindDeparture || existing.Enabled || existing.MutedOn != "2026-10-05" || existing.Language != "Russian" {
		t.Fatalf("watch after migration = %+v", existing)
	}
	state, found, err := store.LoadWatchState(ctx, "0123456789abcdef", "2026-10-05")
	if err != nil || !found || state.Key != "cancelled" || !state.Alerted || state.Messages != 3 || !state.Since.IsZero() {
		t.Fatalf("state after migration = %+v %v %v", state, found, err)
	}
	alerts, err := store.ReadyWatchAlerts(ctx)
	if err != nil || len(alerts) != 1 || alerts[0].ID != "1111111111111111" || alerts[0].Text != "cancelled" || alerts[0].Language != "Russian" {
		t.Fatalf("alerts after migration = %+v %v", alerts, err)
	}

	created := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	started := time.Date(2026, 10, 6, 8, 30, 0, 0, time.UTC)
	home := watch.Watch{
		ID: "fedcba9876543210", PrincipalID: "alice", Enabled: true, CreatedAt: created, UpdatedAt: created,
		Spec: watch.Spec{
			Kind: watch.KindHome, Target: watch.Target{Entity: "binary_sensor.balcony_door", Name: "Balcony door"},
			Condition: &watch.Condition{State: []string{"open"}, ForMinutes: 10}, Triggers: []string{watch.TriggerEnter},
			Days: []string{"mon"}, Start: "00:00", End: "24:00", Zone: "Europe/Amsterdam",
		},
	}
	if ok, err := store.CreateWatch(ctx, home, 5); err != nil || !ok {
		t.Fatalf("home watch after migration = %v %v", ok, err)
	}
	stored, found, err := store.LoadWatch(ctx, "alice", home.ID)
	if err != nil || !found || stored.Condition == nil || stored.Condition.ForMinutes != 10 || stored.Target.Entity != "binary_sensor.balcony_door" {
		t.Fatalf("stored home watch = %+v %v %v", stored, found, err)
	}
	for sequence := 1; sequence <= 10; sequence++ {
		alert := &watch.Alert{ID: "00000000000000" + string(rune('a'+sequence-1)) + "0", WatchID: home.ID, PrincipalID: "alice", Occurrence: "2026-10-05", Sequence: sequence, Type: "alert", Text: "open", ExpiresAt: created}
		state := watch.State{WatchID: home.ID, PrincipalID: "alice", Occurrence: "2026-10-05", Key: "holding", Alerted: true, Messages: sequence, Since: started, CheckedAt: created}
		if err := store.SaveWatchCheck(ctx, state, alert, created); err != nil {
			t.Fatalf("message %d of a home window: %v", sequence, err)
		}
	}
	earlier, found, err := store.LatestWatchStateBefore(ctx, home.ID, "2026-10-06")
	if err != nil || !found || earlier.Occurrence != "2026-10-05" || earlier.Messages != 10 || !earlier.Since.Equal(started) {
		t.Fatalf("latest state before = %+v %v %v", earlier, found, err)
	}
	if _, found, err := store.LatestWatchStateBefore(ctx, home.ID, "2026-10-05"); err != nil || found {
		t.Fatalf("state before the first window = %v %v", found, err)
	}

	var foreignKeyProblems int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyProblems); err != nil || foreignKeyProblems != 0 {
		t.Fatalf("foreign key check = %d %v", foreignKeyProblems, err)
	}
}

// Migration 025 rebuilds the three watch tables again; transit and home watches, their window
// states with the start of a condition, and their queued and finished alerts must survive, and the
// rebuilt tables must accept an agent watch whose alert is private.
func TestMigrationAgentWatchesKeepsExistingRows(t *testing.T) {
	ctx := t.Context()
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
		if m.name >= "025" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-09-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO watches (id, principal_id, kind, config, enabled, muted_on, created_at, updated_at)
		 VALUES ('0123456789abcdef', 'alice', 'departure', '{"kind":"departure","language":"Russian"}', 0, '2026-10-05', '2026-09-01T00:00:00.000Z', '2026-09-02T00:00:00.000Z')`,
		`INSERT INTO watches (id, principal_id, kind, config, enabled, created_at, updated_at)
		 VALUES ('fedcba9876543210', 'alice', 'home', '{"kind":"home","target":{"entity":"binary_sensor.balcony_door","name":"Balcony door"}}', 1, '2026-09-01T00:00:00.000Z', '2026-09-01T00:00:00.000Z')`,
		`INSERT INTO watch_states (watch_id, principal_id, occurrence, state, alerted, messages, failures, failure_noticed, since, checked_at, updated_at)
		 VALUES ('fedcba9876543210', 'alice', '2026-10-05', 'holding', 1, 7, 0, 0, '2026-10-05T06:00:00.000Z', '2026-10-05T06:20:00.000Z', '2026-10-05T06:20:00.000Z')`,
		`INSERT INTO watch_alerts (id, watch_id, principal_id, occurrence, sequence, type, text, status, expires_at, created_at, updated_at)
		 VALUES ('1111111111111111', '0123456789abcdef', 'alice', '2026-10-05', 3, 'alert', 'cancelled', 'ready', '2026-10-05T06:42:00.000Z', '2026-10-05T06:20:00.000Z', '2026-10-05T06:20:00.000Z')`,
		`INSERT INTO watch_alerts (id, watch_id, principal_id, occurrence, sequence, type, text, status, reason, expires_at, created_at, updated_at, finished_at)
		 VALUES ('2222222222222222', 'fedcba9876543210', 'alice', '2026-10-05', 7, 'alert', NULL, 'dropped', 'muted', '2026-10-05T06:50:00.000Z', '2026-10-05T06:20:00.000Z', '2026-10-05T06:21:00.000Z', '2026-10-05T06:21:00.000Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}

	watches, err := store.ListWatches(ctx, "alice")
	if err != nil || len(watches) != 2 || watches[0].Kind != watch.KindDeparture || watches[0].Enabled || watches[0].MutedOn != "2026-10-05" ||
		watches[1].Kind != watch.KindHome || watches[1].Target.Entity != "binary_sensor.balcony_door" {
		t.Fatalf("watches after migration = %+v %v", watches, err)
	}
	since := time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)
	state, found, err := store.LoadWatchState(ctx, "fedcba9876543210", "2026-10-05")
	if err != nil || !found || state.Key != "holding" || !state.Alerted || state.Messages != 7 || !state.Since.Equal(since) {
		t.Fatalf("state after migration = %+v %v %v", state, found, err)
	}
	alerts, err := store.ReadyWatchAlerts(ctx)
	if err != nil || len(alerts) != 1 || alerts[0].ID != "1111111111111111" || alerts[0].Language != "Russian" || alerts[0].Private {
		t.Fatalf("alerts after migration = %+v %v", alerts, err)
	}
	var reason, finished string
	if err := db.QueryRowContext(ctx, `SELECT reason, finished_at FROM watch_alerts WHERE id = '2222222222222222'`).Scan(&reason, &finished); err != nil || reason != "muted" || finished == "" {
		t.Fatalf("finished alert after migration = %q %q %v", reason, finished, err)
	}

	created := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	agent := watch.Watch{
		ID: "00000000000000aa", PrincipalID: "alice", Enabled: true, CreatedAt: created, UpdatedAt: created,
		Spec: watch.Spec{
			Kind: watch.KindAgent, Target: watch.Target{Name: "Release date"},
			Check:    &watch.Check{Instruction: "Read the example project's release page.", Condition: "a release date is announced", IntervalMinutes: 120, Until: "2026-12-31"},
			Triggers: []string{watch.TriggerEnter, watch.TriggerChange}, Days: []string{"mon"}, Start: "00:00", End: "24:00", Zone: "Europe/Amsterdam",
		},
	}
	if ok, err := store.CreateWatch(ctx, agent, watch.MaximumWatchesPerPrincipal); err != nil || !ok {
		t.Fatalf("agent watch after migration = %v %v", ok, err)
	}
	stored, found, err := store.LoadWatch(ctx, "alice", agent.ID)
	if err != nil || !found || stored.Check == nil || *stored.Check != *agent.Check {
		t.Fatalf("stored agent watch = %+v %v %v", stored, found, err)
	}
	alert := &watch.Alert{ID: "00000000000000bb", WatchID: agent.ID, PrincipalID: "alice", Occurrence: "2026-10-06", Sequence: 1, Type: "alert", Text: "announced", ExpiresAt: created, Private: true}
	if err := store.SaveWatchCheck(ctx, watch.State{WatchID: agent.ID, PrincipalID: "alice", Occurrence: "2026-10-06", Key: "met|1 december", Alerted: true, Messages: 1, CheckedAt: created}, alert, created); err != nil {
		t.Fatal(err)
	}
	alerts, err = store.ReadyWatchAlerts(ctx)
	if err != nil || len(alerts) != 2 || alerts[1].ID != alert.ID || !alerts[1].Private || alerts[1].Kind != watch.KindAgent {
		t.Fatalf("alerts with the agent's = %+v %v", alerts, err)
	}

	var foreignKeyProblems int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyProblems); err != nil || foreignKeyProblems != 0 {
		t.Fatalf("foreign key check = %d %v", foreignKeyProblems, err)
	}
}

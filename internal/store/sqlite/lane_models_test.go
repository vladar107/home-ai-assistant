package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Migration 041 only adds: a task from before it keeps its row with no copied model, and lanes
// start with no choice.
func TestMigrationModelChoiceKeepsExistingRows(t *testing.T) {
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
		if m.name >= "041" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-10-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO tasks (id, principal_id, chat_id, request_message_id, title, brief, session_id, status, attempts, created_at, updated_at)
		 VALUES ('0123456789abcdef', 'alice', 101, 5, 'Compare', 'Compare two options', '01234567-89ab-cdef-0123-456789abcdef', 'queued', 0,
		         '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	if count := countRows(t, store, `SELECT count(*) FROM tasks WHERE id = '0123456789abcdef' AND model IS NULL AND effort IS NULL`); count != 1 {
		t.Fatalf("existing task after migration: %d matching rows", count)
	}
	if count := countRows(t, store, `SELECT count(*) FROM conversation_lane_models`); count != 0 {
		t.Fatalf("lane models after migration = %d", count)
	}
}

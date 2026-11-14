package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// Migration 062 flags the records whose summary starts with an intent warning, of either kind, and
// no other, and gives every record an empty user outcome.
func TestMigrationApprovalLanguageFlagsExistingWarnedRecords(t *testing.T) {
	t.Parallel()
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
		if m.name >= "062" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-09-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	hash := "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := db.ExecContext(ctx, `INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`); err != nil {
		t.Fatal(err)
	}
	summaries := map[string]string{
		"aaaaaaaaaaaaaaaa": approval.IntentWarning + "\n\nRename the note",
		"bbbbbbbbbbbbbbbb": approval.DirectIntentWarning + "\n\nRename the note",
		"cccccccccccccccc": "Rename the note",
		"dddddddddddddddd": "Rename the note\n\n" + approval.IntentWarning,
	}
	for id, summary := range summaries {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, created_at, expires_at)
			 VALUES (?, ?, 0, 'alice', 'note_write', '{}', ?, ?, 'pending', '2026-09-01T10:00:00.000Z', '2099-09-01T10:10:00.000Z')`,
			id, id, hash, summary); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	for id, want := range map[string]bool{"aaaaaaaaaaaaaaaa": true, "bbbbbbbbbbbbbbbb": true, "cccccccccccccccc": false, "dddddddddddddddd": false} {
		record, found, err := store.LoadApproval(ctx, id)
		if err != nil || !found || record.Warned != want || record.UserOutcome != "" {
			t.Fatalf("%s after migration = %+v %v %v, want warned %v", id, record, found, err, want)
		}
	}
}

package sqlite

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// Migration 027 rebuilds approvals for direct actions; records stored before it must survive
// unchanged as proposals, and a direct record then lives beside them without joining a bundle.
func TestMigrationDirectApprovalsKeepsExistingRows(t *testing.T) {
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
		if m.name >= "027" {
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
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, chat_id, message_id, created_at, expires_at)
		 VALUES ('aaaaaaaaaaaaaaaa', 'aaaaaaaaaaaaaaaa', 0, 'alice', 'note_write', '{"name":"a"}', '` + hash + `', 'First', 'pending', 101, 7, '2026-09-01T10:00:00.000Z', '2099-09-01T10:10:00.000Z')`,
		`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, chat_id, message_id, created_at, expires_at)
		 VALUES ('bbbbbbbbbbbbbbbb', 'aaaaaaaaaaaaaaaa', 1, 'alice', 'note_write', '{"name":"b"}', '` + hash + `', 'Second', 'pending', 101, 7, '2026-09-01T10:00:00.000Z', '2099-09-01T10:10:00.000Z')`,
		`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, outcome, created_at, expires_at, started_at, finished_at)
		 VALUES ('cccccccccccccccc', 'cccccccccccccccc', 0, 'alice', 'note_write', '{}', '` + hash + `', 'Third', 'executed', 'Saved.', '2026-08-01T10:00:00.000Z', '2026-08-01T10:10:00.000Z', '2026-08-01T10:01:00.000Z', '2026-08-01T10:01:01.000Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}

	pending, err := store.PendingApprovals(ctx, "alice")
	if err != nil || len(pending) != 2 || pending[0].Summary != "First" || pending[1].Position != 1 || pending[1].MessageID != 7 ||
		pending[0].Mode != approval.ModeProposed || pending[0].RequestID != "" || pending[0].Undo != nil {
		t.Fatalf("pending after migration = %+v %v", pending, err)
	}
	executed, found, err := store.LoadApproval(ctx, "cccccccccccccccc")
	if err != nil || !found || executed.Status != approval.StatusExecuted || executed.Outcome != "Saved." || executed.StartedAt.IsZero() || executed.FinishedAt.IsZero() {
		t.Fatalf("executed after migration = %+v %v %v", executed, found, err)
	}

	now := time.Now()
	direct := approval.Record{
		ID: "dddddddddddddddd", PrincipalID: "alice", Tool: "note_write", Arguments: json.RawMessage(`{"name":"d"}`), Hash: hash,
		Summary: "Direct", CreatedAt: now, ExpiresAt: now.Add(approval.UndoWindow), RequestID: "33333333-3333-4333-8333-333333333333",
	}
	if err := store.CreateDirectApproval(ctx, direct); err != nil {
		t.Fatal(err)
	}
	interrupted := direct
	interrupted.ID, interrupted.Summary = "eeeeeeeeeeeeeeee", "Interrupted"
	if err := store.CreateDirectApproval(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.PendingApprovals(ctx, "alice"); err != nil || len(pending) != 2 {
		t.Fatalf("pending beside direct records = %d %v", len(pending), err)
	}
	if found, ok, err := store.FindDirectApproval(ctx, "alice", "note_write", hash, direct.RequestID); err != nil || !ok || found.ID != direct.ID || found.Status != approval.StatusConfirmed || found.StartedAt.IsZero() {
		t.Fatalf("duplicate lookup = %+v %v %v", found, ok, err)
	}
	if finished, err := store.FinishDirectApproval(ctx, direct.ID, approval.StatusDone, "", "Saved.", json.RawMessage(`{"name":"d"}`), now.Add(approval.UndoWindow)); err != nil || !finished {
		t.Fatalf("finish direct = %v %v", finished, err)
	}
	if err := store.SetApprovalSummary(ctx, direct.ID, "Warned"); err != nil {
		t.Fatal(err)
	}
	listed, err := store.DirectApprovals(ctx, "alice", direct.RequestID)
	if err != nil || len(listed) != 2 || listed[0].Summary != "Warned" || listed[0].Status != approval.StatusDone || string(listed[0].Undo) != `{"name":"d"}` || listed[0].UndoUntil.IsZero() {
		t.Fatalf("direct records = %+v %v", listed, err)
	}

	expired, err := store.ExpireApprovals(ctx, now, true)
	if err != nil {
		t.Fatal(err)
	}
	var interruptedFound bool
	for _, record := range expired {
		if record.ID == interrupted.ID {
			interruptedFound = record.Status == approval.StatusFailed && record.Reason == approval.ReasonInterrupted
		}
		if record.ID == direct.ID {
			t.Fatal("the restart sweep touched a finished direct record")
		}
	}
	if !interruptedFound {
		t.Fatalf("restart sweep = %+v, want the started direct record failed as interrupted", expired)
	}

	finished, _, err := store.LoadApproval(ctx, direct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimUndo(ctx, direct.ID, "alice", now); err != nil || !claimed {
		t.Fatalf("first undo claim = %v %v", claimed, err)
	}
	if claimed, err := store.ClaimUndo(ctx, direct.ID, "alice", now); err != nil || claimed {
		t.Fatalf("second undo claim = %v %v", claimed, err)
	}
	if undoing, _, err := store.LoadApproval(ctx, direct.ID); err != nil || undoing.Status != approval.StatusDone || undoing.Reason != approval.ReasonUndoing || !undoing.FinishedAt.Equal(finished.FinishedAt) {
		t.Fatalf("claimed record = %+v %v, want done and undoing with its finish time", undoing, err)
	}

	// A restart while the Undo runs leaves the action done and lets a later press undo it.
	expired, err = store.ExpireApprovals(ctx, now, true)
	if err != nil || len(expired) != 1 || expired[0].ID != direct.ID || expired[0].Status != approval.StatusDone || expired[0].Reason != approval.ReasonUndoInterrupted {
		t.Fatalf("restart sweep during an undo = %+v %v", expired, err)
	}
	if claimed, err := store.ClaimUndo(ctx, direct.ID, "alice", now); err != nil || !claimed {
		t.Fatalf("undo claim after the restart = %v %v", claimed, err)
	}
	if err := store.CompleteUndo(ctx, direct.ID, "Removed."); err != nil {
		t.Fatal(err)
	}
	if undone, _, err := store.LoadApproval(ctx, direct.ID); err != nil || undone.Status != approval.StatusUndone || undone.Reason != "" || undone.Outcome != "Removed." || !undone.FinishedAt.Equal(finished.FinishedAt) {
		t.Fatalf("undone record = %+v %v", undone, err)
	}
	var foreignKeyProblems int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyProblems); err != nil || foreignKeyProblems != 0 {
		t.Fatalf("foreign key check = %d %v", foreignKeyProblems, err)
	}
}

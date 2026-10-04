package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Migration 015 rebuilds the conversation tables; sessions, requests, and attachments stored before
// it must survive unchanged and still reference each other, and a session can then be switched.
func TestMigrationConversationHistoryKeepsExistingRows(t *testing.T) {
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
		if m.name >= "015" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-09-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	const (
		closedID  = "11111111-1111-4111-8111-111111111111"
		activeID  = "22222222-2222-4222-8222-222222222222"
		requestID = "33333333-3333-4333-8333-333333333333"
	)
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, status, close_reason, created_at, updated_at, closed_at)
		 VALUES ('` + closedID + `', 'alice', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'closed', 'user_reset',
		         '2026-09-01T10:00:00.000Z', '2026-09-02T10:00:00.000Z', '2026-09-02T10:00:00.000Z')`,
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, created_at, updated_at, exposed_at)
		 VALUES ('` + activeID + `', 'alice', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', '2026-09-03T10:00:00.000Z', '2026-09-03T10:00:00.000Z', '2026-09-03T10:30:00.000Z')`,
		`INSERT INTO conversation_requests (id, bot_user_id, telegram_update_id, telegram_message_id, principal_id, chat_id, session_id, message, created_at)
		 VALUES ('` + requestID + `', 999, 7, 1007, 'alice', 101, '` + activeID + `', 'queued text', '2026-09-03T11:00:00.000Z')`,
		`INSERT INTO conversation_attachments (id, principal_id, session_id, request_id, position, kind, file_name, mime_type, size_bytes)
		 VALUES ('abcdefgh', 'alice', '` + activeID + `', '` + requestID + `', 0, 'file', 'notes.txt', 'text/plain', 12)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}

	request, found, err := store.NextConversationRequest(ctx, "alice")
	if err != nil || !found || request.ID != requestID || request.Message != "queued text" || request.TelegramMessageID != 1007 ||
		request.SessionID != activeID || !request.ResumedFrom.IsZero() {
		t.Fatalf("request after migration = %+v %v %v", request, found, err)
	}
	var attachments int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM conversation_attachments WHERE request_id = ? AND session_id = ?`, requestID, activeID).Scan(&attachments); err != nil || attachments != 1 {
		t.Fatalf("attachments after migration = %d %v", attachments, err)
	}
	if exposed, err := store.ConversationExposed(ctx, "alice", requestID); err != nil || !exposed {
		t.Fatalf("exposure after migration = %v %v", exposed, err)
	}
	sessions, err := store.ConversationSessions(ctx, "alice", 10)
	if err != nil || len(sessions) != 2 || sessions[0].ID != activeID || !sessions[0].Active || sessions[1].ID != closedID || sessions[1].Active {
		t.Fatalf("sessions after migration = %+v %v", sessions, err)
	}

	if _, err := store.CancelPendingConversation(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	switched, err := store.SwitchConversation(ctx, "alice", closedID)
	if err != nil || !switched.Found || !switched.Switched || switched.Previous != activeID {
		t.Fatalf("switch after migration = %+v %v", switched, err)
	}
	var reason string
	if err := db.QueryRowContext(ctx, `SELECT close_reason FROM conversation_sessions WHERE id = ?`, activeID).Scan(&reason); err != nil || reason != "switched" {
		t.Fatalf("close reason = %q %v", reason, err)
	}
	var foreignKeyProblems int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyProblems); err != nil || foreignKeyProblems != 0 {
		t.Fatalf("foreign key check = %d %v", foreignKeyProblems, err)
	}
}

package sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

// Migration 032 rebuilds the conversation tables again; everything stored before it must survive
// unchanged and still reference each other, and a tab still holds one active conversation.
func TestMigrationTabsKeepsExistingRows(t *testing.T) {
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
		if m.name >= "032" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-10-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	const (
		closedID  = "11111111-1111-4111-8111-111111111111"
		tabID     = "22222222-2222-4222-8222-222222222222"
		requestID = "33333333-3333-4333-8333-333333333333"
	)
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, status, close_reason, created_at, updated_at, closed_at)
		 VALUES ('` + closedID + `', 'alice', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'closed', 'switched',
		         '2026-10-01T10:00:00.000Z', '2026-10-02T10:00:00.000Z', '2026-10-02T10:00:00.000Z')`,
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, created_at, updated_at, exposed_at, external_at, private_at, thread_id)
		 VALUES ('` + tabID + `', 'alice', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', '2026-10-03T10:00:00.000Z', '2026-10-03T10:00:00.000Z',
		         '2026-10-03T10:30:00.000Z', '2026-10-03T10:30:00.000Z', '2026-10-03T10:40:00.000Z', 7)`,
		`INSERT INTO conversation_requests (id, bot_user_id, telegram_update_id, telegram_message_id, principal_id, chat_id, session_id, message, created_at, reply_to_message_id, thread_id)
		 VALUES ('` + requestID + `', 999, 7, 1007, 'alice', 101, '` + tabID + `', 'queued text', '2026-10-03T11:00:00.000Z', 900, 7)`,
		`INSERT INTO conversation_attachments (id, principal_id, session_id, request_id, position, kind, file_name, mime_type, size_bytes)
		 VALUES ('abcdefgh', 'alice', '` + tabID + `', '` + requestID + `', 0, 'file', 'notes.txt', 'text/plain', 12)`,
		`INSERT INTO conversation_reply_messages (principal_id, telegram_message_id, session_id, request_id) VALUES ('alice', 900, '` + closedID + `', 'earlier')`,
		`INSERT INTO reply_feedback (principal_id, telegram_message_id, class, emoji) VALUES ('alice', 900, 'positive', '👍')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}

	tab := conversation.Lane{PrincipalID: "alice", ThreadID: 7}
	request, found, err := store.NextConversationRequest(ctx, tab)
	if err != nil || !found || request.ID != requestID || request.Message != "queued text" || request.SessionID != tabID ||
		request.ReplyToMessageID != 900 || request.ThreadID != 7 {
		t.Fatalf("request after migration = %+v %v %v", request, found, err)
	}
	if count := countRows(t, store, `SELECT count(*) FROM conversation_attachments WHERE request_id = ? AND session_id = ?`, requestID, tabID); count != 1 {
		t.Fatalf("attachments after migration = %d", count)
	}
	if private, err := store.ConversationPrivate(ctx, "alice", requestID); err != nil || !private {
		t.Fatalf("privacy after migration = %v %v", private, err)
	}
	origin, found, err := store.ReplyOrigin(ctx, "alice", 900)
	if err != nil || !found || origin.SessionID != closedID || origin.RequestID != "earlier" {
		t.Fatalf("reply origin after migration = %+v %v %v", origin, found, err)
	}
	if count := countRows(t, store, `SELECT count(*) FROM reply_feedback WHERE principal_id = 'alice' AND telegram_message_id = 900 AND class = 'positive'`); count != 1 {
		t.Fatalf("reply feedback after migration = %d", count)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, thread_id)
		 VALUES ('44444444-4444-4444-8444-444444444444', 'alice', 'cccccccc-cccc-4ccc-8ccc-cccccccccccc', 7)`); err == nil {
		t.Fatal("a second active conversation was accepted in the same tab")
	}
	var foreignKeyProblems int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyProblems); err != nil || foreignKeyProblems != 0 {
		t.Fatalf("foreign key check = %d %v", foreignKeyProblems, err)
	}
}

func TestMigrationExistingTabNamesKeepsTabsAlreadyOpen(t *testing.T) {
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
		if m.name >= "036" {
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
		`INSERT INTO telegram_tabs (principal_id, thread_id, name, named_by) VALUES ('alice', 1, 'Opened without a name', 'bot')`,
		`INSERT INTO telegram_tabs (principal_id, thread_id, name, named_by) VALUES ('alice', 2, 'Named by Alice', 'user')`,
		`INSERT INTO telegram_tabs (principal_id, thread_id, name, named_by, subject) VALUES ('alice', 3, 'Reports', 'fixed', 'feed:reports')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	for threadID, want := range map[int64]string{1: TabNamedByUser, 2: TabNamedByUser, 3: TabNamedByFixed} {
		if tab, found, err := store.Tab(ctx, "alice", threadID); err != nil || !found || tab.NamedBy != want {
			t.Fatalf("tab %d after migration = %+v %v %v, want named by %s", threadID, tab, found, err, want)
		}
	}
	if claimed, err := store.ClaimTabName(ctx, "alice", 1, "55555555-5555-4555-8555-555555555555"); err != nil || claimed {
		t.Fatalf("claim of a tab open before the migration = %v %v", claimed, err)
	}
}

// tabMessage queues one message in a lane, finishes its turn, and returns the session it joined.
func tabMessage(t *testing.T, store *Store, lane conversation.Lane, update int64) string {
	t.Helper()
	ctx := t.Context()
	result, err := store.EnqueueConversation(ctx, conversation.EnqueueInput{
		BotUserID: 999, TelegramUpdateID: update, TelegramMessageID: 1000 + update, PrincipalID: lane.PrincipalID,
		ThreadID: lane.ThreadID, ChatID: 101, Message: "hello",
	}, conversation.Session{
		ID: fmt.Sprintf("%08d-0000-4000-8000-000000000000", update), PrincipalID: lane.PrincipalID,
		RuntimeSessionID: fmt.Sprintf("%08d-1111-4111-8111-111111111111", update),
	}, fmt.Sprintf("%08d-2222-4222-8222-222222222222", update), nil)
	if err != nil || !result.Created {
		t.Fatalf("enqueue %d = %+v %v", update, result, err)
	}
	if _, err := store.CancelPendingConversation(ctx, lane); err != nil {
		t.Fatal(err)
	}
	return result.Request.SessionID
}

func sessionRecord(t *testing.T, store *Store, id string) conversation.SessionRecord {
	t.Helper()
	sessions, err := store.ConversationSessions(t.Context(), "alice", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.ID == id {
			return session
		}
	}
	t.Fatalf("conversation %s is not listed in %+v", id, sessions)
	return conversation.SessionRecord{}
}

func TestGoneTabConversationComesBackWithTheNextMessage(t *testing.T) {
	store, ctx := openTestStore(t)
	tab := conversation.Lane{PrincipalID: "alice", ThreadID: 7}
	main := conversation.Lane{PrincipalID: "alice"}
	tabMessage(t, store, main, 1)
	gone := tabMessage(t, store, tab, 2)
	if added, err := store.AddTab(ctx, "alice", 7, "Trip notes", TabNamedByBot); err != nil || !added {
		t.Fatalf("add tab = %v %v", added, err)
	}
	if record := sessionRecord(t, store, gone); !record.Active || record.ThreadID != 7 || record.TabName != "Trip notes" {
		t.Fatalf("conversation with the tab open = %+v", record)
	}

	closed, ok, err := store.CloseLane(ctx, tab, conversation.CloseTabGone)
	if err != nil || !ok || closed != gone {
		t.Fatalf("close gone tab = %q %v %v", closed, ok, err)
	}
	if _, ok, err := store.CloseLane(ctx, tab, conversation.CloseTabGone); err != nil || ok {
		t.Fatalf("closing an empty tab = %v %v", ok, err)
	}
	if record := sessionRecord(t, store, gone); record.Active {
		t.Fatalf("conversation after the tab went = %+v", record)
	}

	if again := tabMessage(t, store, tab, 3); again != gone {
		t.Fatalf("a message in the gone tab joined %s, want %s", again, gone)
	}

	// Resumed elsewhere, the conversation leaves the tab, and the tab starts afresh.
	if _, _, err := store.CloseLane(ctx, tab, conversation.CloseTabGone); err != nil {
		t.Fatal(err)
	}
	other := conversation.Lane{PrincipalID: "alice", ThreadID: 9}
	switched, err := store.SwitchConversation(ctx, other, gone)
	if err != nil || !switched.Switched || switched.Session.ThreadID != 9 {
		t.Fatalf("resume of the gone tab's conversation = %+v %v", switched, err)
	}
	if fresh := tabMessage(t, store, tab, 4); fresh == gone {
		t.Fatal("the old tab took back a conversation another tab now holds")
	}
}

func TestClosedTabConversationIsNotReopenedByAMessage(t *testing.T) {
	store, ctx := openTestStore(t)
	tab := conversation.Lane{PrincipalID: "alice", ThreadID: 7}
	closed := tabMessage(t, store, tab, 1)
	if _, ok, err := store.CloseLane(ctx, tab, conversation.CloseTabClosed); err != nil || !ok {
		t.Fatalf("close tab = %v %v", ok, err)
	}
	if fresh := tabMessage(t, store, tab, 2); fresh == closed {
		t.Fatal("a message reopened a conversation whose tab the user closed")
	}
	var reason string
	if err := store.db.QueryRowContext(ctx, `SELECT close_reason FROM conversation_sessions WHERE id = ?`, closed).Scan(&reason); err != nil || reason != "tab_closed" {
		t.Fatalf("close reason = %q %v", reason, err)
	}
	if _, _, err := store.CloseLane(ctx, tab, "user_reset"); err == nil {
		t.Fatal("CloseLane accepted a reason that is not a tab's")
	}
}

func TestGoneTabConversationStaysClosedWhileAnotherTabHoldsItsSubject(t *testing.T) {
	store, ctx := openTestStore(t)
	tab := conversation.Lane{PrincipalID: "alice", ThreadID: 7}
	gone := tabMessage(t, store, tab, 1)
	if _, _, err := store.CloseLane(ctx, tab, conversation.CloseTabGone); err != nil {
		t.Fatal(err)
	}
	holder := tabMessage(t, store, conversation.Lane{PrincipalID: "alice", ThreadID: 9}, 2)
	if _, err := store.db.ExecContext(ctx, `UPDATE conversation_sessions SET subject = 'job:report' WHERE id IN (?, ?)`, gone, holder); err != nil {
		t.Fatal(err)
	}
	if fresh := tabMessage(t, store, tab, 3); fresh == gone {
		t.Fatal("a gone tab took back a subject another tab holds")
	}
}

func TestTabRegistryKeepsFixedNamesAndConfirmsOnce(t *testing.T) {
	store, ctx := openTestStore(t)
	if added, err := store.AddTab(ctx, "alice", 7, "Reports", TabNamedByFixed); err != nil || !added {
		t.Fatalf("add fixed tab = %v %v", added, err)
	}
	if added, err := store.AddTab(ctx, "alice", 7, "Something else", TabNamedByUser); err != nil || added {
		t.Fatalf("adding a known tab = %v %v", added, err)
	}
	if renamed, err := store.RenameTab(ctx, "alice", 7, "Mine"); err != nil || renamed {
		t.Fatalf("renaming a fixed tab = %v %v", renamed, err)
	}
	if renamed, err := store.RenameTab(ctx, "alice", 8, "Groceries"); err != nil || !renamed {
		t.Fatalf("renaming an unknown tab = %v %v", renamed, err)
	}
	if renamed, err := store.RenameTab(ctx, "alice", 8, "Groceries"); err != nil || renamed {
		t.Fatalf("renaming a tab to its own name = %v %v", renamed, err)
	}
	for range 2 {
		if confirmed, err := store.ConfirmTab(ctx, "alice", 7); err != nil || !confirmed {
			t.Fatalf("confirm a known tab = %v %v", confirmed, err)
		}
	}
	if confirmed, err := store.ConfirmTab(ctx, "alice", 9); err != nil || confirmed {
		t.Fatalf("confirm an unknown tab = %v %v", confirmed, err)
	}
	fixed, found, err := store.Tab(ctx, "alice", 7)
	if err != nil || !found || fixed.Name != "Reports" || fixed.NamedBy != TabNamedByFixed || !fixed.Confirmed {
		t.Fatalf("fixed tab = %+v %v %v", fixed, found, err)
	}
	renamed, found, err := store.Tab(ctx, "alice", 8)
	if err != nil || !found || renamed.Name != "Groceries" || renamed.NamedBy != TabNamedByUser || renamed.Confirmed {
		t.Fatalf("renamed tab = %+v %v %v", renamed, found, err)
	}
	if _, found, err := store.Tab(ctx, "bob", 7); err != nil || found {
		t.Fatalf("bob sees alice's tab: %v %v", found, err)
	}
	if err := store.DeleteTab(ctx, "alice", 7); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Tab(ctx, "alice", 7); err != nil || found {
		t.Fatalf("deleted tab = %v %v", found, err)
	}
}

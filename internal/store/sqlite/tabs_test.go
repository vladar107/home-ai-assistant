package sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// Migration 032 rebuilds the conversation tables again; everything stored before it must survive
// unchanged and still reference each other, and a tab still holds one active conversation.
func TestMigrationTabsKeepsExistingRows(t *testing.T) {
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
	for threadID, want := range map[int64]string{1: thread.NamedByUser, 2: thread.NamedByUser, 3: thread.NamedByFixed} {
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
	t.Parallel()
	store, ctx := openTestStore(t)
	tab := conversation.Lane{PrincipalID: "alice", ThreadID: 7}
	main := conversation.Lane{PrincipalID: "alice"}
	tabMessage(t, store, main, 1)
	gone := tabMessage(t, store, tab, 2)
	if added, err := store.AddTab(ctx, "alice", 7, "Trip notes", thread.NamedByBot); err != nil || !added {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	store, ctx := openTestStore(t)
	if added, err := store.AddTab(ctx, "alice", 7, "Reports", thread.NamedByFixed); err != nil || !added {
		t.Fatalf("add fixed tab = %v %v", added, err)
	}
	if added, err := store.AddTab(ctx, "alice", 7, "Something else", thread.NamedByUser); err != nil || added {
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
	if err != nil || !found || fixed.Name != "Reports" || fixed.NamedBy != thread.NamedByFixed || !fixed.Confirmed {
		t.Fatalf("fixed tab = %+v %v %v", fixed, found, err)
	}
	renamed, found, err := store.Tab(ctx, "alice", 8)
	if err != nil || !found || renamed.Name != "Groceries" || renamed.NamedBy != thread.NamedByUser || renamed.Confirmed {
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

func TestASubjectTabIsRecordedWhicheverOfItsTwoWritesLandsFirst(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	reports, reminders := thread.FeedSubject(thread.FeedReports), thread.FeedSubject(thread.FeedReminders)
	wantTab := func(threadID int64, name, namedBy, subject string) {
		t.Helper()
		tab, found, err := store.Tab(ctx, "alice", threadID)
		if err != nil || !found || tab.Name != name || tab.NamedBy != namedBy || tab.Subject != subject {
			t.Fatalf("tab %d = %+v %v %v, want %q named by %s for %q", threadID, tab, found, err, name, namedBy, subject)
		}
	}

	if err := store.AddSubjectTab(ctx, "alice", 7, "Reports", reports); err != nil {
		t.Fatal(err)
	}
	if added, err := store.AddTab(ctx, "alice", 7, "Reports", thread.NamedByBot); err != nil || added {
		t.Fatalf("creation notice after the record = %v %v", added, err)
	}
	wantTab(7, "Reports", thread.NamedByFixed, reports)

	if added, err := store.AddTab(ctx, "alice", 8, "As Telegram sent it", thread.NamedByBot); err != nil || !added {
		t.Fatalf("creation notice before the record = %v %v", added, err)
	}
	if err := store.AddSubjectTab(ctx, "alice", 8, "Reminders", reminders); err != nil {
		t.Fatalf("record after the creation notice: %v", err)
	}
	wantTab(8, "Reminders", thread.NamedByFixed, reminders)
	if threadID, found, err := store.SubjectTab(ctx, "alice", reminders); err != nil || !found || threadID != 8 {
		t.Fatalf("reminders tab = %d %v %v, want 8", threadID, found, err)
	}

	watches := thread.FeedSubject(thread.FeedWatches)
	if err := store.AddSubjectTab(ctx, "alice", 7, "Watches", watches); err == nil {
		t.Fatal("a tab that serves a subject was taken over")
	}
	wantTab(7, "Reports", thread.NamedByFixed, reports)
	if _, err := store.RenameTab(ctx, "alice", 9, "Groceries"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddSubjectTab(ctx, "alice", 9, "Watches", watches); err == nil {
		t.Fatal("a tab the user named was taken over")
	}
	wantTab(9, "Groceries", thread.NamedByUser, "")
	if threadID, found, err := store.SubjectTab(ctx, "alice", watches); err != nil || found {
		t.Fatalf("watches tab after the refusals = %d %v %v, want none", threadID, found, err)
	}
}

// tripTakeState is what a take of a tab changes, read back from the registry and the conversations.
type tripTakeState struct {
	tab      int64
	summary  int64
	activeIn []int64
}

func readTripTakeState(t *testing.T, store *Store, tripID string) tripTakeState {
	t.Helper()
	ctx := t.Context()
	var state tripTakeState
	threadID, found, err := store.SubjectTab(ctx, "alice", thread.TripDestination(tripID))
	if err != nil {
		t.Fatal(err)
	}
	if found {
		state.tab = threadID
	}
	var summary sql.NullInt64
	if err := store.db.QueryRowContext(ctx, `SELECT summary_message_id FROM trips WHERE id = ?`, tripID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	state.summary = summary.Int64
	rows, err := store.db.QueryContext(ctx, `SELECT thread_id FROM conversation_sessions WHERE principal_id = 'alice' AND subject = ? AND status = 'active' ORDER BY thread_id`, thread.TripDestination(tripID))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var threadID int64
		if err := rows.Scan(&threadID); err != nil {
			t.Fatal(err)
		}
		state.activeIn = append(state.activeIn, threadID)
	}
	return state
}

func laneSessionSubject(t *testing.T, store *Store, threadID int64) string {
	t.Helper()
	var subject string
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(subject, '') FROM conversation_sessions WHERE principal_id = 'alice' AND thread_id = ? AND status = 'active'`, threadID,
	).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	return subject
}

// Taking a tab moves the trip's subject from its old tab to the taken one, which keeps its name,
// forgets the trip's summary, and leaves one active conversation about the trip, the taken tab's,
// also when the trip's conversation was in a feed tab. A tab that no longer serves what it did
// is not taken, and a tab of an ended trip can be taken, leaving that trip without a tab.
func TestTakingATabMovesTheTripsSubjectSummaryAndConversation(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	const (
		tripTab  = 30
		userTab  = 50
		feedTab  = 40
		first    = "a1a1a1a1"
		second   = "b2b2b2b2"
		tripName = "Example trip"
	)
	addTestTrip(t, store, "alice", first, tripBase.Add(120*time.Hour))
	addTestTrip(t, store, "alice", second, tripBase.Add(240*time.Hour))
	if err := store.AddSubjectTab(ctx, "alice", tripTab, tripName, thread.TripDestination(first)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTripSummary(ctx, "alice", first, 900); err != nil {
		t.Fatal(err)
	}
	if added, err := store.AddTab(ctx, "alice", userTab, "Packing", thread.NamedByUser); err != nil || !added {
		t.Fatalf("add user tab = %v %v", added, err)
	}
	if err := store.AddSubjectTab(ctx, "alice", feedTab, "Reminders", thread.FeedSubject(thread.FeedReminders)); err != nil {
		t.Fatal(err)
	}
	tabMessage(t, store, conversation.Lane{PrincipalID: "alice", ThreadID: tripTab}, 1)
	mine := tabMessage(t, store, conversation.Lane{PrincipalID: "alice", ThreadID: userTab}, 2)
	if state := readTripTakeState(t, store, first); state.tab != tripTab || state.summary != 900 || len(state.activeIn) != 1 || state.activeIn[0] != tripTab {
		t.Fatalf("before the take = %+v", state)
	}

	if taken, err := store.TakeTab(ctx, "alice", userTab, thread.TripDestination(first), thread.FeedSubject(thread.FeedReminders)); err != nil || taken {
		t.Fatalf("take of a tab that serves something else = %v %v", taken, err)
	}
	if taken, err := store.TakeTab(ctx, "alice", userTab, thread.TripDestination(first), ""); err != nil || !taken {
		t.Fatalf("take = %v %v", taken, err)
	}
	if state := readTripTakeState(t, store, first); state.tab != userTab || state.summary != 0 || len(state.activeIn) != 1 || state.activeIn[0] != userTab {
		t.Fatalf("after the take = %+v", state)
	}
	if session := tabMessage(t, store, conversation.Lane{PrincipalID: "alice", ThreadID: userTab}, 3); session != mine {
		t.Fatalf("the taken tab's next message joined %s, want its conversation %s", session, mine)
	}
	left, _, err := store.Tab(ctx, "alice", tripTab)
	if err != nil || left.Subject != "" || left.NamedBy != thread.NamedByUser || left.Name != tripName {
		t.Fatalf("the tab the trip left = %+v %v", left, err)
	}
	if subject := laneSessionSubject(t, store, tripTab); subject != "" {
		t.Fatalf("the left tab's conversation is about %q", subject)
	}
	taken, _, err := store.Tab(ctx, "alice", userTab)
	if err != nil || taken.Subject != thread.TripDestination(first) || taken.NamedBy != thread.NamedByUser || taken.Name != "Packing" {
		t.Fatalf("the taken tab = %+v %v", taken, err)
	}
	if renamed, err := store.RenameTab(ctx, "alice", userTab, "Packing list"); err != nil || !renamed {
		t.Fatalf("rename of the taken tab = %v %v", renamed, err)
	}

	// The second trip's conversation is in the reminders tab, a reply to one of its notices.
	tabMessage(t, store, conversation.Lane{PrincipalID: "alice", ThreadID: feedTab}, 4)
	if _, err := store.db.ExecContext(ctx, `UPDATE conversation_sessions SET subject = ? WHERE principal_id = 'alice' AND thread_id = ? AND status = 'active'`,
		thread.TripDestination(second), feedTab); err != nil {
		t.Fatal(err)
	}
	if taken, err := store.TakeTab(ctx, "alice", tripTab, thread.TripDestination(second), ""); err != nil || !taken {
		t.Fatalf("take for the second trip = %v %v", taken, err)
	}
	if state := readTripTakeState(t, store, second); state.tab != tripTab || len(state.activeIn) != 1 || state.activeIn[0] != tripTab {
		t.Fatalf("second trip after the take = %+v", state)
	}
	if subject := laneSessionSubject(t, store, feedTab); subject != "" {
		t.Fatalf("the reminders tab's conversation is still about %q", subject)
	}

	// The first trip ended; the second takes its tab, and the first is left without one.
	if err := store.SetTripSummary(ctx, "alice", first, 901); err != nil {
		t.Fatal(err)
	}
	if taken, err := store.TakeTab(ctx, "alice", userTab, thread.TripDestination(second), thread.TripDestination(first)); err != nil || !taken {
		t.Fatalf("take of the ended trip's tab = %v %v", taken, err)
	}
	if state := readTripTakeState(t, store, first); state.tab != 0 || state.summary != 0 || len(state.activeIn) != 0 {
		t.Fatalf("ended trip after its tab was taken = %+v", state)
	}
	if state := readTripTakeState(t, store, second); state.tab != userTab || len(state.activeIn) != 1 || state.activeIn[0] != userTab {
		t.Fatalf("second trip after moving again = %+v", state)
	}
	if subject := laneSessionSubject(t, store, tripTab); subject != "" {
		t.Fatalf("the tab the second trip left keeps a conversation about %q", subject)
	}
	if again, _, err := store.Tab(ctx, "alice", userTab); err != nil || again.Name != "Packing list" || again.NamedBy != thread.NamedByUser {
		t.Fatalf("the taken tab after moving again = %+v %v", again, err)
	}
}

// The tab the bot made for a trip that is over stays fixed when another trip takes it, so its name
// follows the new trip; a tab the user or a conversation named is the user's once taken.
func TestATakenTabOfAnEndedTripStaysFixed(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	addTestTrip(t, store, "alice", "a1a1a1a1", tripBase.Add(-24*time.Hour))
	addTestTrip(t, store, "alice", "b2b2b2b2", tripBase.Add(240*time.Hour))
	if err := store.AddSubjectTab(ctx, "alice", 30, "Example visit", thread.TripDestination("a1a1a1a1")); err != nil {
		t.Fatal(err)
	}
	if added, err := store.AddTab(ctx, "alice", 31, "New chat", thread.NamedByBot); err != nil || !added {
		t.Fatalf("add bot tab = %v %v", added, err)
	}
	if taken, err := store.TakeTab(ctx, "alice", 30, thread.TripDestination("b2b2b2b2"), thread.TripDestination("a1a1a1a1")); err != nil || !taken {
		t.Fatalf("take of the ended trip's tab = %v %v", taken, err)
	}
	if tab, _, err := store.Tab(ctx, "alice", 30); err != nil || tab.NamedBy != thread.NamedByFixed || tab.Subject != thread.TripDestination("b2b2b2b2") {
		t.Fatalf("the ended trip's tab after the take = %+v %v", tab, err)
	}
	if taken, err := store.TakeTab(ctx, "alice", 31, thread.TripDestination("b2b2b2b2"), ""); err != nil || !taken {
		t.Fatalf("take of the bot's tab = %v %v", taken, err)
	}
	if tab, _, err := store.Tab(ctx, "alice", 31); err != nil || tab.NamedBy != thread.NamedByUser {
		t.Fatalf("the bot's tab after the take = %+v %v", tab, err)
	}
	if tab, _, err := store.Tab(ctx, "alice", 30); err != nil || tab.NamedBy != thread.NamedByUser || tab.Subject != "" {
		t.Fatalf("the tab the trip left = %+v %v", tab, err)
	}
}

// A closed conversation about a trip that moved to another tab comes back about nothing, whether by
// a reply to one of its answers in the tab it was in or from /sessions, so the trip keeps one
// active conversation.
func TestAConversationLeftBehindByATripComesBackAboutNothing(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	const tripID = "a1a1a1a1"
	addTestTrip(t, store, "alice", tripID, tripBase.Add(240*time.Hour))
	if err := store.AddSubjectTab(ctx, "alice", 30, "Example trip", thread.TripDestination(tripID)); err != nil {
		t.Fatal(err)
	}
	if added, err := store.AddTab(ctx, "alice", 50, "Packing", thread.NamedByUser); err != nil || !added {
		t.Fatalf("add user tab = %v %v", added, err)
	}
	left := conversation.Lane{PrincipalID: "alice", ThreadID: 30}
	answered := tabMessage(t, store, left, 1)
	resumable := tabMessage(t, store, conversation.Lane{PrincipalID: "alice", ThreadID: 60}, 2)
	if _, err := store.db.ExecContext(ctx, `UPDATE conversation_sessions SET status = 'closed', close_reason = 'switched', closed_at = updated_at WHERE id IN (?, ?)`, answered, resumable); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE conversation_sessions SET subject = ? WHERE id = ?`, thread.TripDestination(tripID), resumable); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO conversation_reply_messages (principal_id, telegram_message_id, session_id, request_id) VALUES ('alice', 900, ?, 'earlier')`, answered); err != nil {
		t.Fatal(err)
	}
	mine := tabMessage(t, store, conversation.Lane{PrincipalID: "alice", ThreadID: 50}, 3)
	if taken, err := store.TakeTab(ctx, "alice", 50, thread.TripDestination(tripID), ""); err != nil || !taken {
		t.Fatalf("take = %v %v", taken, err)
	}

	result, err := store.EnqueueConversation(ctx, conversation.EnqueueInput{
		BotUserID: 999, TelegramUpdateID: 4, TelegramMessageID: 1004, PrincipalID: "alice", ThreadID: 30, ChatID: 101,
		Message: "about that answer", ReplyToMessageID: 900,
	}, conversation.Session{
		ID: "00000004-0000-4000-8000-000000000000", PrincipalID: "alice", RuntimeSessionID: "00000004-1111-4111-8111-111111111111",
	}, "00000004-2222-4222-8222-222222222222", nil)
	if err != nil || !result.Created || result.Request.SessionID != answered {
		t.Fatalf("reply in the left tab = %+v %v, want it to reopen %s", result, err, answered)
	}
	if subject := laneSessionSubject(t, store, 30); subject != "" {
		t.Fatalf("the reopened conversation is about %q", subject)
	}
	if _, err := store.CancelPendingConversation(ctx, left); err != nil {
		t.Fatal(err)
	}

	switched, err := store.SwitchConversation(ctx, conversation.Lane{PrincipalID: "alice", ThreadID: 61}, resumable)
	if err != nil || !switched.Switched {
		t.Fatalf("resume from /sessions = %+v %v", switched, err)
	}
	if subject := laneSessionSubject(t, store, 61); subject != "" {
		t.Fatalf("the resumed conversation is about %q", subject)
	}
	if state := readTripTakeState(t, store, tripID); len(state.activeIn) != 1 || state.activeIn[0] != 50 {
		t.Fatalf("active conversations about the trip = %+v, want the taken tab's %s", state, mine)
	}
}

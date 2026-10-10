package sqlite

import "testing"

const feedbackSession = "11111111-1111-4111-8111-111111111111"

func insertFeedbackSession(t *testing.T, store *Store, principalID, sessionID, runtimeID string) {
	t.Helper()
	if _, err := store.db.ExecContext(t.Context(),
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id) VALUES (?, ?, ?)`,
		sessionID, principalID, runtimeID); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, store *Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestReplyOriginReturnsTheRequestOrEmptyForOlderReplies(t *testing.T) {
	store, ctx := openTestStore(t)
	insertFeedbackSession(t, store, "alice", feedbackSession, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")

	if err := store.RecordConversationReply(ctx, "alice", feedbackSession, "request-1", 500); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO conversation_reply_messages (principal_id, telegram_message_id, session_id) VALUES ('alice', 501, ?)`,
		feedbackSession); err != nil {
		t.Fatal(err)
	}

	session, request, found, err := store.ReplyOrigin(ctx, "alice", 500)
	if err != nil || !found || session != feedbackSession || request != "request-1" {
		t.Fatalf("origin of 500 = %q %q %v %v", session, request, found, err)
	}
	session, request, found, err = store.ReplyOrigin(ctx, "alice", 501)
	if err != nil || !found || session != feedbackSession || request != "" {
		t.Fatalf("origin of 501 = %q %q %v %v", session, request, found, err)
	}
	if _, _, found, err := store.ReplyOrigin(ctx, "bob", 500); err != nil || found {
		t.Fatalf("another principal found the reply: %v %v", found, err)
	}
}

func TestReplyFeedbackKeepsOnlyTheLatestReaction(t *testing.T) {
	store, ctx := openTestStore(t)
	insertFeedbackSession(t, store, "alice", feedbackSession, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err := store.RecordConversationReply(ctx, "alice", feedbackSession, "request-1", 500); err != nil {
		t.Fatal(err)
	}

	if err := store.SetReplyFeedback(ctx, "alice", 500, "positive", "👍"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetReplyFeedback(ctx, "alice", 500, "negative", "👎"); err != nil {
		t.Fatal(err)
	}
	var class, emoji string
	if err := store.db.QueryRowContext(ctx,
		`SELECT class, emoji FROM reply_feedback WHERE principal_id = 'alice' AND telegram_message_id = 500`).Scan(&class, &emoji); err != nil ||
		class != "negative" || emoji != "👎" {
		t.Fatalf("feedback = %q %q %v", class, emoji, err)
	}
	if n := countRows(t, store, `SELECT count(*) FROM reply_feedback`); n != 1 {
		t.Fatalf("feedback rows = %d", n)
	}

	if err := store.SetReplyFeedback(ctx, "alice", 999, "positive", "👍"); err == nil {
		t.Fatal("feedback on an unrecorded message was accepted")
	}
	if err := store.SetReplyFeedback(ctx, "alice", 500, "bogus", "👍"); err == nil {
		t.Fatal("an unknown class was accepted")
	}

	if err := store.DeleteReplyFeedback(ctx, "alice", 500); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteReplyFeedback(ctx, "alice", 500); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, store, `SELECT count(*) FROM reply_feedback`); n != 0 {
		t.Fatalf("feedback rows after delete = %d", n)
	}
}

func TestReplyReconsiderIsClaimedOncePerKey(t *testing.T) {
	store, ctx := openTestStore(t)

	for _, want := range []bool{true, false} {
		if got, err := store.ClaimReplyReconsider(ctx, "alice", "request-1"); err != nil || got != want {
			t.Fatalf("claim = %v %v, want %v", got, err, want)
		}
	}
	if got, err := store.ClaimReplyReconsider(ctx, "alice", "message:500"); err != nil || !got {
		t.Fatalf("claim of another key = %v %v", got, err)
	}
	if got, err := store.ClaimReplyReconsider(ctx, "bob", "request-1"); err != nil || !got {
		t.Fatalf("claim by another principal = %v %v", got, err)
	}
}

func TestReplyFeedbackGoesWithTheReplyRecords(t *testing.T) {
	store, ctx := openTestStore(t)
	insertFeedbackSession(t, store, "alice", feedbackSession, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	insertFeedbackSession(t, store, "bob", "22222222-2222-4222-8222-222222222222", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	for _, reply := range []struct {
		principal, session string
		message            int64
	}{
		{"alice", feedbackSession, 500},
		{"bob", "22222222-2222-4222-8222-222222222222", 500},
	} {
		if err := store.RecordConversationReply(ctx, reply.principal, reply.session, "request-1", reply.message); err != nil {
			t.Fatal(err)
		}
		if err := store.SetReplyFeedback(ctx, reply.principal, reply.message, "negative", "👎"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimReplyReconsider(ctx, reply.principal, "request-1"); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM conversation_sessions WHERE id = ?`, feedbackSession); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, store, `SELECT count(*) FROM reply_feedback WHERE principal_id = 'alice'`); n != 0 {
		t.Fatalf("alice's feedback survived her session: %d", n)
	}
	if n := countRows(t, store, `SELECT count(*) FROM reply_feedback WHERE principal_id = 'bob'`); n != 1 {
		t.Fatalf("bob's feedback = %d", n)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM principals WHERE id = 'bob'`); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, store, `SELECT (SELECT count(*) FROM reply_feedback WHERE principal_id = 'bob') + (SELECT count(*) FROM reply_reconsiders WHERE principal_id = 'bob')`); n != 0 {
		t.Fatalf("bob's feedback records survived the principal: %d", n)
	}
	if n := countRows(t, store, `SELECT count(*) FROM reply_reconsiders WHERE principal_id = 'alice'`); n != 1 {
		t.Fatalf("alice's reconsider = %d", n)
	}
}

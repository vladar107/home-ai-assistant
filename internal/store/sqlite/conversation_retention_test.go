package sqlite

import (
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

func TestClosedConversationsAreDeletedAfterRetention(t *testing.T) {
	env := newFeedEnv(t)
	expired := env.send("alice", 0, 0).Request
	if err := env.store.RecordConversationReply(env.ctx, "alice", expired.SessionID, expired.ID, 700); err != nil {
		t.Fatal(err)
	}
	if err := env.store.SetReplyFeedback(env.ctx, "alice", 700, "positive", "👍"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.ResetConversation(env.ctx, conversation.Lane{PrincipalID: "alice"}); err != nil {
		t.Fatal(err)
	}
	recent := env.send("alice", 0, 0).Request
	if _, _, err := env.store.ResetConversation(env.ctx, conversation.Lane{PrincipalID: "alice"}); err != nil {
		t.Fatal(err)
	}
	active := env.send("alice", 0, 0).Request
	cutoff := time.Now().Add(-90 * 24 * time.Hour)
	old := cutoff.Add(-time.Hour).UTC().Format(conversationTimeLayout)
	if _, err := env.store.db.ExecContext(env.ctx, `UPDATE conversation_sessions SET closed_at = ? WHERE id = ?`, old, expired.SessionID); err != nil {
		t.Fatal(err)
	}

	referenced, err := env.store.ReferencedSessions(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for runtimeID, want := range map[string]bool{expired.RuntimeSessionID: true, recent.RuntimeSessionID: true, active.RuntimeSessionID: false} {
		if closed, kept := referenced[runtimeID]; !kept || closed != want {
			t.Errorf("session %s referenced = %v closed = %v, want closed = %v", runtimeID, kept, closed, want)
		}
	}
	if runtimeID, found, err := env.store.ConversationRuntimeSession(env.ctx, "alice", recent.SessionID); err != nil || !found || runtimeID != recent.RuntimeSessionID {
		t.Fatalf("runtime session = %q %v %v", runtimeID, found, err)
	}
	if _, found, err := env.store.ConversationRuntimeSession(env.ctx, "bob", recent.SessionID); err != nil || found {
		t.Fatalf("another principal found the conversation: %v %v", found, err)
	}

	if err := env.store.DeleteClosedConversations(env.ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM conversation_sessions WHERE id = '` + expired.SessionID + `'`:         0,
		`SELECT count(*) FROM conversation_requests WHERE session_id = '` + expired.SessionID + `'`: 0,
		`SELECT count(*) FROM conversation_reply_messages WHERE telegram_message_id = 700`:          0,
		`SELECT count(*) FROM reply_feedback`:                                                       0,
		`SELECT count(*) FROM conversation_sessions`:                                                2,
	} {
		if got := countRows(t, env.store, query); got != want {
			t.Errorf("%s = %d, want %d", query, got, want)
		}
	}
}

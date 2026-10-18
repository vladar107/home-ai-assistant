package telegram

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// newHistoryHarness runs turns through the real Pi adapter, whose fixture records each exchange in
// a session file the way Pi does, and reads those files back for /sessions and the history tools.
func newHistoryHarness(t *testing.T) (*conversationHarness, string) {
	t.Helper()
	profiles := filepath.Join(t.TempDir(), "profiles")
	runtime := newFixtureRuntime(t, profiles)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{profilesDir: profiles})
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	return h, profiles
}

// talk sends a message and returns the answer the bot sent for it.
func (h *conversationHarness) talk(updateID, userID int64, text string) sentMessage {
	h.t.Helper()
	h.push(textUpdate(updateID, userID, text))
	answer := h.nextMessage()
	if answer.chatID != userID || answer.replyTo != requestMessageID(updateID) {
		h.t.Fatalf("answer = %+v, want one quoting message %d", answer, requestMessageID(updateID))
	}
	h.waitState(updateID, "delivered")
	return answer
}

func (h *conversationHarness) activeSession(principal string) (string, string) {
	h.t.Helper()
	var id, runtimeID string
	if err := h.db.QueryRowContext(h.ctx,
		`SELECT id, runtime_session_id FROM conversation_sessions WHERE principal_id = ? AND status = 'active'`, principal,
	).Scan(&id, &runtimeID); err != nil {
		h.t.Fatal(err)
	}
	return id, runtimeID
}

// sessionFiles returns the Pi session files recorded for one runtime session, joined.
func sessionFiles(t *testing.T, profiles, principal, runtimeID string) (int, string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(profiles, principal, "sessions", "*_"+runtimeID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		joined.Write(data)
	}
	return len(paths), joined.String()
}

func TestSessionsResumeContinuesTheEarlierPiSession(t *testing.T) {
	h, profiles := newHistoryHarness(t)
	h.talk(1, 101, "plan the heating schedule for winter")
	earlierID, earlierRuntimeID := h.activeSession("alice")
	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)
	h.talk(3, 101, "write a groceries list")
	laterID, _ := h.activeSession("alice")

	h.push(textUpdate(4, 101, "/sessions"))
	list := h.nextMessage()
	if !strings.HasPrefix(list.text, SessionsTitle) ||
		!strings.Contains(list.text, "1. ") || !strings.Contains(list.text, "write a groceries list (current)") ||
		!strings.Contains(list.text, "2. ") || !strings.Contains(list.text, "plan the heating schedule for winter") {
		t.Fatalf("/sessions = %q", list.text)
	}
	resume := "sessions:resume:" + earlierID
	if data := buttonData(list.keyboard); len(data) != 1 || data[0] != resume {
		t.Fatalf("/sessions buttons = %v, want only %s", data, resume)
	}

	h.push(callbackUpdate(5, 101, list.messageID, resume))
	h.wantAnswer(5)
	if edit := h.nextEdit(); edit.messageID != list.messageID || !strings.Contains(edit.text, "plan the heating schedule for winter (current)") {
		t.Fatalf("refreshed list = %+v", edit)
	}
	if note := h.nextMessage(); !strings.HasPrefix(note.text, "Back in the conversation from ") || !strings.Contains(note.text, "plan the heating schedule for winter") {
		t.Fatalf("resume note = %+v", note)
	}
	if id, runtimeID := h.activeSession("alice"); id != earlierID || runtimeID != earlierRuntimeID {
		t.Fatalf("active session = %s (%s), want %s (%s)", id, runtimeID, earlierID, earlierRuntimeID)
	}
	var reason string
	if err := h.db.QueryRowContext(h.ctx, `SELECT close_reason FROM conversation_sessions WHERE id = ?`, laterID).Scan(&reason); err != nil || reason != "switched" {
		t.Fatalf("later session close reason = %q (%v), want switched", reason, err)
	}

	h.talk(6, 101, "and for the bedroom?")
	files, recorded := sessionFiles(t, profiles, "alice", earlierRuntimeID)
	if files != 1 || !strings.Contains(recorded, "plan the heating schedule for winter") || !strings.Contains(recorded, "and for the bedroom?") {
		t.Fatalf("resumed turn did not continue the earlier Pi session: %d files\n%s", files, recorded)
	}

	// Bob's press on Alice's conversation reaches nothing.
	h.push(callbackUpdate(7, 202, list.messageID, "sessions:resume:"+laterID))
	h.wantAnswer(7)
	if edit := h.nextEdit(); edit.text != NoSessionsReply {
		t.Fatalf("Bob's list = %+v", edit)
	}
	if note := h.nextMessage(); note.chatID != 202 || note.text != SessionGoneReply {
		t.Fatalf("Bob's press = %+v", note)
	}
	if id, _ := h.activeSession("alice"); id != earlierID {
		t.Fatalf("Bob's press changed Alice's conversation to %s", id)
	}
}

func TestNewDeletesThePhotoFromTheConversationHistory(t *testing.T) {
	h, profiles := newHistoryHarness(t)
	picture := []byte("\xff\xd8\xff\xe0 pretend JPEG bytes")
	encoded := base64.StdEncoding.EncodeToString(picture)
	h.addFile("photo-1", "photos/file_1.jpg", picture)
	h.push(photoUpdate(1, 101, "what is on this label?", "photo-1", int64(len(picture))))
	h.nextMessage()
	h.waitState(1, "delivered")
	_, runtimeID := h.activeSession("alice")
	if _, history := sessionFiles(t, profiles, "alice", runtimeID); !strings.Contains(history, encoded) {
		t.Fatal("the Pi session did not record the photo")
	}

	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)
	files, history := sessionFiles(t, profiles, "alice", runtimeID)
	if files != 1 || strings.Contains(history, encoded) || !strings.Contains(history, "what is on this label?") {
		t.Fatalf("history after /new keeps the photo or lost its text: %d files: %s", files, history)
	}
}

func TestReplyToAnEarlierAnswerReturnsToItsConversation(t *testing.T) {
	h, profiles := newHistoryHarness(t)
	earlierAnswer := h.talk(1, 101, "which plants need water on Sundays")
	_, earlierRuntimeID := h.activeSession("alice")
	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)
	laterAnswer := h.talk(3, 101, "book a table for Friday")
	laterID, laterRuntimeID := h.activeSession("alice")

	reply := textUpdate(4, 101, "and the cactus?")
	reply.Message.ReplyToMessage = &Message{MessageID: earlierAnswer.messageID}
	h.push(reply)
	answer := h.nextMessage()
	if answer.replyTo != requestMessageID(4) || !strings.HasPrefix(answer.text, "Back in the conversation from ") ||
		!strings.HasSuffix(answer.text, ".\n\nreply: and the cactus?") {
		t.Fatalf("answer after replying to an earlier answer = %+v", answer)
	}
	h.waitState(4, "delivered")
	if _, runtimeID := h.activeSession("alice"); runtimeID != earlierRuntimeID {
		t.Fatalf("reply continued in %s, want %s", runtimeID, earlierRuntimeID)
	}
	if _, recorded := sessionFiles(t, profiles, "alice", earlierRuntimeID); !strings.Contains(recorded, "and the cactus?") {
		t.Fatalf("earlier Pi session did not get the reply:\n%s", recorded)
	}

	// Replying to the answer that just came keeps the conversation, without a note.
	followUp := textUpdate(5, 101, "thanks")
	followUp.Message.ReplyToMessage = &Message{MessageID: answer.messageID}
	h.push(followUp)
	h.wantReply(101, 5, "reply: thanks")
	h.waitState(5, "delivered")

	// Replying to the later conversation's answer switches back to it.
	back := textUpdate(6, 101, "make it 8 pm")
	back.Message.ReplyToMessage = &Message{MessageID: laterAnswer.messageID}
	h.push(back)
	if switched := h.nextMessage(); !strings.HasSuffix(switched.text, "reply: make it 8 pm") || !strings.HasPrefix(switched.text, "Back in the conversation from ") {
		t.Fatalf("answer after replying to the later answer = %+v", switched)
	}
	h.waitState(6, "delivered")
	if id, runtimeID := h.activeSession("alice"); id != laterID || runtimeID != laterRuntimeID {
		t.Fatalf("active session = %s, want %s", id, laterID)
	}

	// Bob replying to Alice's answer stays in his own conversation.
	h.talk(7, 202, "hello")
	bobID, _ := h.activeSession("bob")
	foreign := textUpdate(8, 202, "what about mine?")
	foreign.Message.ReplyToMessage = &Message{MessageID: earlierAnswer.messageID}
	h.push(foreign)
	h.wantReply(202, 8, "reply: what about mine?")
	h.waitState(8, "delivered")
	if id, _ := h.activeSession("bob"); id != bobID {
		t.Fatalf("Bob's reply to Alice's answer switched his conversation to %s", id)
	}
}

func TestConversationSearchFindsOnlyTheUsersOwnEarlierConversations(t *testing.T) {
	h, _ := newHistoryHarness(t)
	h.talk(1, 101, "the boiler service code is 4417")
	earlierID, _ := h.activeSession("alice")
	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)
	h.talk(3, 202, "my boiler service code is 9925")
	h.push(textUpdate(4, 202, "/new"))
	h.wantReply(202, 4, NewSessionReply)

	found := h.talk(5, 101, `fixture:call conversation_search {"query":"Boiler CODE"}`)
	if !strings.Contains(found.text, "conversation "+earlierID) || !strings.Contains(found.text, "message 1 by the user") ||
		!strings.Contains(found.text, "the boiler service code is 4417") || !strings.Contains(found.text, "message 2 by you") {
		t.Fatalf("Alice's search = %q", found.text)
	}
	if strings.Contains(found.text, "9925") {
		t.Fatalf("Alice's search found Bob's conversation: %q", found.text)
	}
	if strings.Contains(found.text, "(the current conversation)") {
		t.Fatalf("Alice's search matched the question it answers: %q", found.text)
	}
	// An earlier conversation may retell outside content, so reading it counts as reading that.
	currentID, _ := h.activeSession("alice")
	var exposed bool
	if err := h.db.QueryRowContext(h.ctx, `SELECT exposed_at IS NOT NULL FROM conversation_sessions WHERE id = ?`, currentID).Scan(&exposed); err != nil || !exposed {
		t.Fatalf("the conversation that searched is not marked exposed: %v %v", exposed, err)
	}

	read := h.talk(6, 101, `fixture:call conversation_read {"session_id":"`+earlierID+`","position":1}`)
	if !strings.Contains(read.text, "Messages 1 to 2 of 2") || !strings.Contains(read.text, "[1] the user") ||
		!strings.Contains(read.text, "the boiler service code is 4417") || !strings.Contains(read.text, "[2] you") {
		t.Fatalf("Alice's read = %q", read.text)
	}
	// Neither the search nor the read of a conversation that read no private content makes this one
	// private.
	h.wantPrivate("alice", 6, false)

	missing := h.talk(7, 202, `fixture:call conversation_search {"query":"4417"}`)
	if !strings.Contains(missing.text, "No message in the user's earlier conversations contains all of: 4417.") {
		t.Fatalf("Bob's search for Alice's code = %q", missing.text)
	}
	h.wantPrivate("bob", 7, false)
	foreign := h.talk(8, 202, `fixture:call conversation_read {"session_id":"`+earlierID+`","position":1}`)
	if !strings.Contains(foreign.text, "error:") || strings.Contains(foreign.text, "4417") {
		t.Fatalf("Bob's read of Alice's conversation = %q", foreign.text)
	}

	// A conversation that read private content is found without its excerpt, and only reading it
	// makes the current conversation private.
	h.push(textUpdate(9, 101, "/new"))
	h.wantReply(101, 9, NewSessionReply)
	h.talk(10, 101, "the alarm panel code is 2580")
	privateID, _ := h.activeSession("alice")
	if err := h.store.MarkConversationPrivate(h.ctx, "alice", h.requestID(10)); err != nil {
		t.Fatal(err)
	}
	h.push(textUpdate(11, 101, "/new"))
	h.wantReply(101, 11, NewSessionReply)
	withheld := h.talk(12, 101, `fixture:call conversation_search {"query":"alarm panel"}`)
	if !strings.Contains(withheld.text, "conversation "+privateID) || !strings.Contains(withheld.text, "message 1 by the user: (excerpt withheld:") ||
		strings.Contains(withheld.text, "2580") {
		t.Fatalf("Alice's search of a private conversation = %q", withheld.text)
	}
	h.wantPrivate("alice", 12, false)
	revealed := h.talk(13, 101, `fixture:call conversation_read {"session_id":"`+privateID+`","position":1}`)
	if !strings.Contains(revealed.text, "the alarm panel code is 2580") {
		t.Fatalf("Alice's read of a private conversation = %q", revealed.text)
	}
	h.wantPrivate("alice", 13, true)
	shown := h.talk(14, 101, `fixture:call conversation_search {"query":"alarm panel"}`)
	if !strings.Contains(shown.text, "conversation "+privateID) || !strings.Contains(shown.text, "the alarm panel code is 2580") {
		t.Fatalf("Alice's search from a private conversation = %q", shown.text)
	}
}

func (h *conversationHarness) wantPrivate(principal string, updateID int64, want bool) {
	h.t.Helper()
	private, err := h.store.ConversationPrivate(h.ctx, principal, h.requestID(updateID))
	if err != nil || private != want {
		h.t.Fatalf("conversation of update %d private = %v (%v), want %v", updateID, private, err, want)
	}
}

func TestResumeIsRefusedWhileATurnIsPending(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()
	h.push(textUpdate(1, 101, "first"))
	call := receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "first answer"}
	h.wantReply(101, 1, "first answer")
	h.waitState(1, "delivered")
	earlierID, _ := h.activeSession("alice")
	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)
	h.push(textUpdate(3, 101, "second"))
	pending := receive(t, h.ctx, runtime.calls)

	h.push(callbackUpdate(4, 101, sentMessageID(9), "sessions:resume:"+earlierID))
	h.wantAnswer(4)
	h.nextEdit()
	if note := h.nextMessage(); note.text != conversation.BusyReply {
		t.Fatalf("resume during a turn = %+v", note)
	}
	if id, _ := h.activeSession("alice"); id == earlierID {
		t.Fatal("resume switched conversations during a pending turn")
	}
	pending.answer <- agentruntime.RunResult{Response: "second answer"}
	h.wantReply(101, 3, "second answer")
}

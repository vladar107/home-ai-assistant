package telegram

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

type createdTopic struct {
	chatID, threadID int64
	name             string
}

type forwardedMessages struct {
	threadID int64
	ids      []int64
}

type refusedSend struct {
	method   string
	threadID int64
}

// tabSimulator stands for the chat's tabs: the ones Telegram answers "message thread not found"
// for, and the tab calls the bot made.
type tabSimulator struct {
	enabled      bool
	gone         map[int64]bool
	next         int64
	deleteStatus int
	createStatus int
	created      chan createdTopic
	deleted      chan int64
	forwards     chan forwardedMessages
	refused      chan refusedSend
	// icons is the icon set Telegram offers, and iconStatus fails reading it when set.
	icons      []TopicIcon
	iconStatus int
	topicEdits chan editedTopic
	// pins are the messages the bot pinned, and pinStatus fails pinning when set.
	pins      chan pinnedMessage
	pinStatus int
}

type pinnedMessage struct {
	chatID, messageID int64
	silent            bool
}

type editedTopic struct {
	threadID      int64
	name, iconID  string
	named, iconed bool
}

// testTopicIcons is the icon set the simulator offers. Telegram sends some emoji with a variation
// selector, as here the clock.
var testTopicIcons = []TopicIcon{
	{Emoji: "🌱", CustomEmojiID: "5000000000000000001"},
	{Emoji: "⏰\uFE0F", CustomEmojiID: "5000000000000000002"},
	{Emoji: "📚", CustomEmojiID: "5000000000000000003"},
}

func newTabSimulator(enabled bool) tabSimulator {
	return tabSimulator{
		enabled: enabled, gone: make(map[int64]bool), next: 500,
		created: make(chan createdTopic, 20), deleted: make(chan int64, 20),
		forwards: make(chan forwardedMessages, 20), refused: make(chan refusedSend, 50),
		icons: testTopicIcons, topicEdits: make(chan editedTopic, 20), pins: make(chan pinnedMessage, 20),
	}
}

func (h *conversationHarness) threadOf(raw string) int64 {
	if raw == "" {
		return 0
	}
	threadID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || threadID <= 0 {
		h.t.Errorf("message_thread_id %q is not a tab", raw)
	}
	return threadID
}

// refuseGoneThread answers a send to a gone tab the way Telegram does.
func (h *conversationHarness) refuseGoneThread(w http.ResponseWriter, r *http.Request, method string, threadID int64) bool {
	h.mu.Lock()
	gone := h.tabs.gone[threadID]
	h.mu.Unlock()
	if threadID <= 0 || !gone {
		return false
	}
	select {
	case h.tabs.refused <- refusedSend{method: method, threadID: threadID}:
	case <-r.Context().Done():
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message thread not found"})
	return true
}

// serveTabMethod answers the tab methods, which exist only while topics are on; a nil result means
// the answer was already written.
func (h *conversationHarness) serveTabMethod(w http.ResponseWriter, r *http.Request) (any, bool) {
	if !h.tabs.enabled {
		return nil, false
	}
	switch filepath.Base(r.URL.Path) {
	case "createForumTopic":
		chatID, err := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
		}
		h.mu.Lock()
		if status := h.tabs.createStatus; status != 0 {
			h.mu.Unlock()
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": "Too Many Requests: retry after 5"})
			return nil, true
		}
		h.tabs.next++
		threadID := h.tabs.next
		h.mu.Unlock()
		h.tabs.created <- createdTopic{chatID: chatID, threadID: threadID, name: r.Form.Get("name")}
		return map[string]any{"message_thread_id": threadID, "name": r.Form.Get("name")}, true
	case "deleteForumTopic":
		threadID := h.threadOf(r.Form.Get("message_thread_id"))
		h.tabs.deleted <- threadID
		if h.refuseGoneThread(w, r, "deleteForumTopic", threadID) {
			return nil, true
		}
		h.mu.Lock()
		status := h.tabs.deleteStatus
		if status == 0 {
			h.tabs.gone[threadID] = true
		}
		h.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": "Bad Request: TOPIC_ID_INVALID"})
			return nil, true
		}
		return true, true
	case "forwardMessages":
		threadID := h.threadOf(r.Form.Get("message_thread_id"))
		if h.refuseGoneThread(w, r, "forwardMessages", threadID) {
			return nil, true
		}
		var ids []int64
		if err := json.Unmarshal([]byte(r.Form.Get("message_ids")), &ids); err != nil {
			h.t.Error(err)
		}
		h.tabs.forwards <- forwardedMessages{threadID: threadID, ids: ids}
		forwarded := make([]map[string]int64, len(ids))
		for index := range ids {
			forwarded[index] = map[string]int64{"message_id": 8000 + int64(index)}
		}
		return forwarded, true
	case "getForumTopicIconStickers":
		h.mu.Lock()
		icons, status := h.tabs.icons, h.tabs.iconStatus
		h.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": "Internal Server Error"})
			return nil, true
		}
		stickers := make([]map[string]string, len(icons))
		for index, icon := range icons {
			stickers[index] = map[string]string{"file_id": "sticker-" + icon.CustomEmojiID, "type": "custom_emoji", "emoji": icon.Emoji, "custom_emoji_id": icon.CustomEmojiID}
		}
		return stickers, true
	case "pinChatMessage":
		chatID, chatErr := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		messageID, messageErr := strconv.ParseInt(r.Form.Get("message_id"), 10, 64)
		if chatErr != nil || messageErr != nil || r.Form.Has("message_thread_id") {
			h.t.Errorf("pinChatMessage needs a chat and a message and names no tab: %v", r.Form)
		}
		h.tabs.pins <- pinnedMessage{chatID: chatID, messageID: messageID, silent: r.Form.Get("disable_notification") == "true"}
		h.mu.Lock()
		status := h.tabs.pinStatus
		h.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": "Bad Request: not enough rights to manage pinned messages in the chat"})
			return nil, true
		}
		return true, true
	case "editForumTopic":
		_, named := r.Form["name"]
		_, iconed := r.Form["icon_custom_emoji_id"]
		h.tabs.topicEdits <- editedTopic{
			threadID: h.threadOf(r.Form.Get("message_thread_id")), name: r.Form.Get("name"), iconID: r.Form.Get("icon_custom_emoji_id"),
			named: named, iconed: iconed,
		}
		return true, true
	}
	return nil, false
}

func (h *conversationHarness) goneTab(threadID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tabs.gone[threadID] = true
}

func (h *conversationHarness) restoreTab(threadID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.tabs.gone, threadID)
}

func (h *conversationHarness) wantRefused(method string, threadID int64) {
	h.t.Helper()
	if refused := receive(h.t, h.ctx, h.tabs.refused); refused.method != method || refused.threadID != threadID {
		h.t.Fatalf("refused send = %+v, want %s to tab %d", refused, method, threadID)
	}
}

// newTabsHarness runs Alice's chat with topics on and returns the slash menu it registered.
func newTabsHarness(t *testing.T, runtime agentruntime.AgentRuntime, topics bool) (*conversationHarness, []BotCommand) {
	t.Helper()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{topics: topics})
	h.provision("alice", "acct-alice")
	h.start()
	registration := receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	return h, registration.commands
}

// tabMessage is a message of Alice's in a tab, replying, as every message in a tab does, to the
// message that created the tab.
func tabMessage(updateID, threadID int64, text string) Update {
	update := textUpdate(updateID, 101, text)
	update.Message.MessageThreadID = threadID
	update.Message.ReplyToMessage = &Message{
		MessageID: threadID, MessageThreadID: threadID, From: &User{ID: 101, FirstName: "Test"},
		Chat: Chat{ID: 101, Type: ChatTypePrivate}, ForumTopicCreated: &ForumTopicCreated{Name: "Trip notes"},
	}
	return update
}

func tabPress(updateID, threadID, messageID int64, data string) Update {
	update := callbackUpdate(updateID, 101, messageID, data)
	update.CallbackQuery.Message.MessageThreadID = threadID
	return update
}

// turnInTab runs one turn of Alice's in a tab and checks its answer lands there.
func (h *conversationHarness) turnInTab(runtime *controlledRuntime, updateID, threadID int64, text string) {
	h.t.Helper()
	if threadID > 0 {
		h.push(tabMessage(updateID, threadID, text))
	} else {
		h.push(textUpdate(updateID, 101, text))
	}
	call := receive(h.t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "answer to " + text}
	if answer := h.nextMessage(); answer.threadID != threadID || answer.text != "answer to "+text {
		h.t.Fatalf("answer = %+v, want it in tab %d", answer, threadID)
	}
	h.waitState(updateID, "delivered")
}

func (h *conversationHarness) laneSession(threadID int64) (id, status, reason string) {
	h.t.Helper()
	var closeReason *string
	if err := h.db.QueryRowContext(h.ctx,
		`SELECT id, status, close_reason FROM conversation_sessions WHERE principal_id = 'alice' AND thread_id = ?
		 ORDER BY updated_at DESC LIMIT 1`, threadID,
	).Scan(&id, &status, &closeReason); err != nil {
		h.t.Fatal(err)
	}
	if closeReason != nil {
		reason = *closeReason
	}
	return id, status, reason
}

// ageTab moves everything the tab recorded two days back.
func (h *conversationHarness) ageTab(threadID int64) {
	h.t.Helper()
	for _, statement := range []string{
		`UPDATE conversation_requests SET created_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-2 days') WHERE principal_id = 'alice' AND thread_id = ?`,
		`UPDATE telegram_tabs SET created_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-2 days'), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-2 days') WHERE principal_id = 'alice' AND thread_id = ?`,
	} {
		h.write(statement, threadID)
	}
}

// write changes the database beside the running service, waiting for its writes like the store does.
func (h *conversationHarness) write(statement string, args ...any) {
	h.t.Helper()
	conn, err := h.db.Conn(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(h.ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		h.t.Fatal(err)
	}
	if _, err := conn.ExecContext(h.ctx, statement, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *conversationHarness) registryTab(threadID int64) (sqlite.Tab, bool) {
	h.t.Helper()
	tab, found, err := h.store.Tab(h.ctx, "alice", threadID)
	if err != nil {
		h.t.Fatal(err)
	}
	return tab, found
}

func TestEverySendToATabFallsBackOnceAndReportsTheTab(t *testing.T) {
	h, _ := newTabsHarness(t, newControlledRuntime(), true)
	h.write(`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, thread_id)
		 VALUES ('77777777-7777-4777-8777-777777777777', 'alice', '77777777-0000-4000-8000-000000000000', 77)`)
	h.goneTab(77)
	tab := thread.With(h.ctx, 77)

	if err := h.client.SendReply(tab, 101, 0, "a reply"); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendMessage", 77)
	if message := h.nextMessage(); message.threadID != 0 || message.text != "a reply" {
		t.Fatalf("reply = %+v, want it in the main view", message)
	}
	if _, status, reason := h.laneSession(77); status != "closed" || reason != conversation.CloseTabGone {
		t.Fatalf("gone tab's conversation = %s %s", status, reason)
	}

	if err := h.client.SendKeyboard(tab, 101, 0, "a question", InlineKeyboard{{{Text: "Yes", CallbackData: "label"}}}); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendMessage", 77)
	if message := h.nextMessage(); message.threadID != 0 || len(message.keyboard) != 1 {
		t.Fatalf("keyboard message = %+v, want it in the main view", message)
	}

	if err := (TripNotifier{Client: h.client, Chats: h.store}).Send(tab, "alice", "", "a trip notice"); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendMessage", 77)
	if message := h.nextMessage(); message.threadID != 0 || message.text != "a trip notice" {
		t.Fatalf("trip notice = %+v, want it in the main view", message)
	}

	if err := h.client.SendPhoto(tab, 101, 0, []byte("\x89PNG card"), "a card"); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendPhoto", 77)
	if photo := h.nextPhoto(); photo.threadID != 0 || string(photo.data) != "\x89PNG card" {
		t.Fatalf("photo = %+v, want it in the main view", photo)
	}

	if err := h.client.SendDocument(tab, 101, "notes.txt", bytes.NewReader([]byte("file body")), 9); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendDocument", 77)
	if document := h.nextDocument(); document.threadID != 0 || string(document.data) != "file body" {
		t.Fatalf("document = %+v, want it whole in the main view", document)
	}

	if err := h.client.ForwardMessages(tab, 101, []int64{1001}); err == nil {
		t.Fatal("a forward to a gone tab went somewhere")
	}
	h.wantRefused("forwardMessages", 77)
	if count := strings.Count(h.logs.String(), "msg=telegram.send.thread_gone principal_id=alice thread_id=77"); count != 6 {
		t.Fatalf("gone tab reported %d times, want 6:\n%s", count, h.logs.String())
	}

	// With the tab back, sends go there again.
	h.restoreTab(77)
	if err := h.client.SendReply(tab, 101, 0, "back"); err != nil {
		t.Fatal(err)
	}
	if message := h.nextMessage(); message.threadID != 77 {
		t.Fatalf("reply to a live tab = %+v", message)
	}
}

func TestATabTheUserWroteInLatelyIsSuspectAndLaterGone(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, true)
	h.turnInTab(runtime, 1, 50, "about the trip")
	sessionID, _, _ := h.laneSession(50)
	if tab, found := h.registryTab(50); !found || tab.Name != "Trip notes" || tab.NamedBy != sqlite.TabNamedByUser || !tab.Confirmed {
		t.Fatalf("tab learned from the message = %+v %v", tab, found)
	}

	h.goneTab(50)
	if err := h.client.SendReply(thread.With(h.ctx, 50), 101, 0, "kept"); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendMessage", 50)
	h.nextMessage()
	if _, status, _ := h.laneSession(50); status != "active" {
		t.Fatalf("a tab written in an hour ago was taken for gone: %s", status)
	}
	if _, found := h.registryTab(50); !found {
		t.Fatal("a suspect tab left the registry")
	}
	if !strings.Contains(h.logs.String(), "msg=telegram.send.thread_gone_suspect principal_id=alice thread_id=50") {
		t.Fatalf("suspect tab not logged:\n%s", h.logs.String())
	}

	h.ageTab(50)
	if err := h.client.SendReply(thread.With(h.ctx, 50), 101, 0, "gone"); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendMessage", 50)
	h.nextMessage()
	if _, status, reason := h.laneSession(50); status != "closed" || reason != conversation.CloseTabGone {
		t.Fatalf("a tab last written in two days ago = %s %s, want gone", status, reason)
	}
	if _, found := h.registryTab(50); found {
		t.Fatal("a gone tab stayed in the registry")
	}

	// It was Telegram's mistake: the next message there continues the conversation.
	h.restoreTab(50)
	h.turnInTab(runtime, 2, 50, "still here")
	if id, status, _ := h.laneSession(50); id != sessionID || status != "active" {
		t.Fatalf("message in the tab joined %s (%s), want %s", id, status, sessionID)
	}
}

func TestOpenNamesTheTabAndPostsThere(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, true)
	created := tabMessage(1, 50, "")
	created.Message.Text, created.Message.ReplyToMessage = "", nil
	created.Message.ForumTopicCreated = &ForumTopicCreated{Name: "Trip notes"}
	h.push(created)
	h.waitOffset(2)
	h.turnInTab(runtime, 2, 50, "about the trip")
	h.turnInTab(runtime, 3, 0, "groceries")

	h.push(textUpdate(4, 101, "/sessions"))
	list := h.nextMessage()
	if !strings.Contains(list.text, "2. ") || !strings.Contains(list.text, "(open in “Trip notes”)") {
		t.Fatalf("/sessions = %q", list.text)
	}
	tabSession, _, _ := h.laneSession(50)
	if data := buttonData(list.keyboard); !slices.Equal(data, []string{"sessions:resume:" + tabSession, "sessions:close:" + tabSession}) {
		t.Fatalf("/sessions buttons = %v", data)
	}
	if label := list.keyboard[0][0].Text + "|" + list.keyboard[0][1].Text; label != "Open 2|Close 2" {
		t.Fatalf("/sessions labels = %s", label)
	}

	h.push(callbackUpdate(5, 101, list.messageID, "sessions:resume:"+tabSession))
	h.wantAnswer(5)
	if nudge := h.nextMessage(); nudge.threadID != 50 || nudge.text != SessionHere {
		t.Fatalf("nudge = %+v, want it in the tab", nudge)
	}
	h.nextEdit()
	if answer := h.nextMessage(); answer.threadID != 0 || answer.text != SessionInTab("Trip notes") {
		t.Fatalf("Open answer = %+v", answer)
	}
	if !strings.Contains(SessionInTab("Trip notes"), "does not let me switch tabs") {
		t.Fatalf("Open answer does not say the bot cannot switch tabs: %q", SessionInTab("Trip notes"))
	}
	if strings.Contains(h.logs.String(), "Trip notes") {
		t.Fatalf("a tab name reached the logs:\n%s", h.logs.String())
	}
}

func TestOpenOnAGoneTabMovesTheConversationToANewTab(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, true)
	h.turnInTab(runtime, 1, 50, "about the trip")
	h.turnInTab(runtime, 2, 0, "groceries")
	tabSession, _, _ := h.laneSession(50)
	h.goneTab(50)

	// Written in a moment ago, the tab may still be there, so it is offered for closing.
	h.push(callbackUpdate(3, 101, sentMessageID(9), "sessions:resume:"+tabSession))
	h.wantAnswer(3)
	h.wantRefused("sendMessage", 50)
	h.nextEdit()
	suspect := h.nextMessage()
	if suspect.threadID != 0 || suspect.text != TabSuspectReply || !slices.Equal(buttonData(suspect.keyboard), []string{"sessions:close:" + tabSession}) ||
		suspect.keyboard[0][0].Text != "Close 2" {
		t.Fatalf("Open on a suspect tab = %+v", suspect)
	}

	h.ageTab(50)
	h.push(callbackUpdate(4, 101, sentMessageID(9), "sessions:resume:"+tabSession))
	h.wantAnswer(4)
	h.wantRefused("sendMessage", 50)
	created := receive(t, h.ctx, h.tabs.created)
	if created.chatID != 101 || !strings.HasPrefix(created.name, untitledTab) {
		t.Fatalf("new tab = %+v", created)
	}
	forwarded := receive(t, h.ctx, h.tabs.forwards)
	if forwarded.threadID != created.threadID || !slices.Equal(forwarded.ids, []int64{requestMessageID(1), sentMessageID(1)}) {
		t.Fatalf("forwarded = %+v, want the tab's messages in tab %d", forwarded, created.threadID)
	}
	h.nextEdit()
	if answer := h.nextMessage(); answer.threadID != created.threadID || !strings.HasPrefix(answer.text, "Back in the conversation from ") {
		t.Fatalf("answer = %+v, want it in the new tab", answer)
	}
	if id, status, _ := h.laneSession(created.threadID); id != tabSession || status != "active" {
		t.Fatalf("new tab holds %s (%s), want %s", id, status, tabSession)
	}
	if tab, found := h.registryTab(created.threadID); !found || tab.NamedBy != sqlite.TabNamedByBot {
		t.Fatalf("new tab in the registry = %+v %v", tab, found)
	}
}

func TestCloseCancelsTheTurnClosesTheConversationAndDeletesTheTab(t *testing.T) {
	runtime := newControlledRuntime()
	h, commands := newTabsHarness(t, runtime, true)
	if !slices.ContainsFunc(commands, func(command BotCommand) bool { return command.Command == "close" }) {
		t.Fatalf("slash menu with tabs on = %+v", commands)
	}
	h.push(tabMessage(1, 50, "a long question"))
	running := receive(t, h.ctx, runtime.calls)
	tabSession, _, _ := h.laneSession(50)

	h.push(tabMessage(2, 50, "/close"))
	question := h.nextMessage()
	if question.threadID != 50 || question.text != CloseTabPrompt || !slices.Equal(buttonData(question.keyboard), []string{"tabs:close:50", "tabs:keep"}) {
		t.Fatalf("/close = %+v", question)
	}
	h.push(tabPress(3, 50, question.messageID, "tabs:close:50"))
	h.wantAnswer(3)
	select {
	case <-running.ctx.Done():
	case <-h.ctx.Done():
		t.Fatal("closing the tab left its turn running")
	}
	if deleted := receive(t, h.ctx, h.tabs.deleted); deleted != 50 {
		t.Fatalf("deleted tab %d", deleted)
	}
	if outcome := h.nextMessage(); outcome.threadID != 0 || outcome.text != TabClosedReply {
		t.Fatalf("outcome = %+v, want it in the main view", outcome)
	}
	h.waitState(1, "cancelled")
	if id, status, reason := h.laneSession(50); id != tabSession || status != "closed" || reason != conversation.CloseTabClosed {
		t.Fatalf("closed tab's conversation = %s %s %s", id, status, reason)
	}
	if _, found := h.registryTab(50); found {
		t.Fatal("a closed tab stayed in the registry")
	}

	// A tab Telegram will not delete still loses its conversation, from /sessions.
	h.turnInTab(runtime, 4, 60, "another topic")
	h.turnInTab(runtime, 5, 0, "groceries")
	other, _, _ := h.laneSession(60)
	h.mu.Lock()
	h.tabs.deleteStatus = http.StatusBadRequest
	h.mu.Unlock()
	h.push(callbackUpdate(6, 101, sentMessageID(9), "sessions:close:"+other))
	h.wantAnswer(6)
	ask := h.nextMessage()
	if ask.threadID != 0 || ask.text != CloseTabQuestion("Trip notes") || !slices.Equal(buttonData(ask.keyboard), []string{"tabs:close:60", "tabs:keep"}) {
		t.Fatalf("Close from /sessions = %+v", ask)
	}
	h.push(callbackUpdate(7, 101, ask.messageID, "tabs:close:60"))
	h.wantAnswer(7)
	receive(t, h.ctx, h.tabs.deleted)
	if edit := h.nextEdit(); edit.messageID != ask.messageID || edit.text != TabNotRemovedReply {
		t.Fatalf("failed delete = %+v", edit)
	}
	if _, status, reason := h.laneSession(60); status != "closed" || reason != conversation.CloseTabClosed {
		t.Fatalf("conversation of an undeletable tab = %s %s", status, reason)
	}

	h.push(callbackUpdate(8, 101, ask.messageID, "tabs:keep"))
	h.wantAnswer(8)
	if edit := h.nextEdit(); edit.text != TabKeptReply {
		t.Fatalf("Keep = %+v", edit)
	}

	// Closing a tab Telegram no longer finds removes it without reporting it gone.
	h.turnInTab(runtime, 9, 70, "a third topic")
	h.goneTab(70)
	h.push(callbackUpdate(10, 101, ask.messageID, "tabs:close:70"))
	h.wantAnswer(10)
	receive(t, h.ctx, h.tabs.deleted)
	h.wantRefused("deleteForumTopic", 70)
	if edit := h.nextEdit(); edit.text != TabClosedReply {
		t.Fatalf("closing a tab that is already gone = %+v", edit)
	}
	if _, status, reason := h.laneSession(70); status != "closed" || reason != conversation.CloseTabClosed {
		t.Fatalf("conversation of a tab already gone = %s %s", status, reason)
	}
	if strings.Contains(h.logs.String(), "msg=telegram.send.thread_gone") {
		t.Fatalf("closing tabs reported them gone:\n%s", h.logs.String())
	}

	h.push(textUpdate(11, 101, "/close"))
	h.wantReply(101, 11, CloseMainViewReply)
}

func TestATabIsConfirmedOnceTheRegistryKnowsIt(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, true)

	// A send into a tab the registry does not know confirms nothing, and is not taken as done.
	if err := h.client.SendReply(thread.With(h.ctx, 60), 101, 0, "before the row"); err != nil {
		t.Fatal(err)
	}
	h.nextMessage()
	if _, found := h.registryTab(60); found {
		t.Fatal("a send added a tab to the registry")
	}
	if _, err := h.store.AddTab(h.ctx, "alice", 60, "Made by the bot", sqlite.TabNamedByBot); err != nil {
		t.Fatal(err)
	}
	if err := h.client.SendReply(thread.With(h.ctx, 60), 101, 0, "after the row"); err != nil {
		t.Fatal(err)
	}
	h.nextMessage()
	if tab, found := h.registryTab(60); !found || !tab.Confirmed {
		t.Fatalf("tab after a send with its row = %+v %v, want confirmed", tab, found)
	}

	// The same for a row the user's message adds.
	if err := h.client.SendReply(thread.With(h.ctx, 61), 101, 0, "before the message"); err != nil {
		t.Fatal(err)
	}
	h.nextMessage()
	h.turnInTab(runtime, 1, 61, "a question")
	if tab, found := h.registryTab(61); !found || tab.NamedBy != sqlite.TabNamedByUser || !tab.Confirmed {
		t.Fatalf("tab after the user's message = %+v %v, want confirmed", tab, found)
	}
}

func TestTopicServiceMessagesNameTabsWithoutUndoingThem(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, true)
	service := func(updateID, threadID, from int64, created *ForumTopicCreated, edited *ForumTopicEdited) Update {
		return Update{UpdateID: updateID, Message: &Message{
			MessageID: requestMessageID(updateID), MessageThreadID: threadID, From: &User{ID: from, FirstName: "Test"},
			Chat: Chat{ID: 101, Type: ChatTypePrivate}, ForumTopicCreated: created, ForumTopicEdited: edited,
		}}
	}
	name := func(value string) *string { return &value }

	h.push(service(1, 40, 101, &ForumTopicCreated{Name: "Garden", IsNameImplicit: true}, nil))
	h.push(service(2, 41, 999, &ForumTopicCreated{Name: "Made by the bot"}, nil))
	if _, err := h.store.AddTab(h.ctx, "alice", 42, "Reports", sqlite.TabNamedByFixed); err != nil {
		t.Fatal(err)
	}
	h.push(service(3, 41, 999, nil, &ForumTopicEdited{Name: name("Renamed by the bot")}))
	h.push(service(4, 40, 101, nil, &ForumTopicEdited{Name: name("Vegetables")}))
	h.push(service(5, 42, 101, nil, &ForumTopicEdited{Name: name("Mine now")}))
	h.waitOffset(6)
	// Every message in a tab replies to the tab's creation, which never changes a known tab.
	h.turnInTab(runtime, 6, 42, "a report question")
	h.turnInTab(runtime, 7, 41, "a question in the bot's tab")

	for threadID, want := range map[int64]sqlite.Tab{
		40: {Name: "Vegetables", NamedBy: sqlite.TabNamedByUser},
		41: {Name: "Made by the bot", NamedBy: sqlite.TabNamedByBot},
		42: {Name: "Reports", NamedBy: sqlite.TabNamedByFixed},
	} {
		if tab, found := h.registryTab(threadID); !found || tab.Name != want.Name || tab.NamedBy != want.NamedBy {
			t.Fatalf("tab %d = %+v %v, want %+v", threadID, tab, found, want)
		}
	}
	logs := h.logs.String()
	if count := strings.Count(logs, "msg=telegram.topic.service"); count != 5 {
		t.Fatalf("topic service logged %d times, want once per service message:\n%s", count, logs)
	}
	if !strings.Contains(logs, "msg=telegram.topic.service principal_id=alice thread_id=40 kind=created source=message sender=user name_present=true name_implicit=true") {
		t.Fatalf("created tab not logged with its fields:\n%s", logs)
	}
	for _, tabName := range []string{"Garden", "Vegetables", "Made by the bot", "Renamed by the bot", "Mine now"} {
		if strings.Contains(logs, tabName) {
			t.Fatalf("tab name %q reached the logs", tabName)
		}
	}
	if request := h.count(`SELECT count(*) FROM conversation_requests WHERE reply_to_message_id IS NOT NULL`); request != 0 {
		t.Fatalf("%d messages in tabs were taken for replies to the tab's creation", request)
	}
}

func TestWithTopicsOffNoTabCallIsMade(t *testing.T) {
	runtime := newControlledRuntime()
	h, commands := newTabsHarness(t, runtime, false)
	if slices.ContainsFunc(commands, func(command BotCommand) bool { return command.Command == "close" }) {
		t.Fatalf("slash menu with tabs off = %+v", commands)
	}
	h.turnInTab(runtime, 1, 0, "groceries")
	h.write(`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, thread_id)
		 VALUES ('55555555-5555-4555-8555-555555555555', 'alice', '55555555-0000-4000-8000-000000000000', 5)`)
	h.push(textUpdate(2, 101, "/sessions"))
	list := h.nextMessage()
	if data := buttonData(list.keyboard); !slices.Equal(data, []string{"sessions:resume:55555555-5555-4555-8555-555555555555"}) {
		t.Fatalf("/sessions buttons with tabs off = %v", data)
	}
	h.push(callbackUpdate(3, 101, list.messageID, "sessions:resume:55555555-5555-4555-8555-555555555555"))
	h.wantAnswer(3)
	h.nextEdit()
	if answer := h.nextMessage(); answer.text != SessionElsewhere || answer.threadID != 0 {
		t.Fatalf("Open with tabs off = %+v", answer)
	}
	h.push(textUpdate(4, 101, "/close"))
	if reply := h.nextMessage(); !strings.HasPrefix(reply.text, HelpHeading) || strings.Contains(reply.text, "/close") {
		t.Fatalf("/close with tabs off = %q", reply.text)
	}
}

func TestACommandThatOpensATabIsAnsweredInTheMainView(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, true)

	// The client opens a tab for a command sent from the main view and sends the command into it.
	h.push(implicitTab(1, 70), tabMessage(2, 70, "/sessions"))
	if answer := h.nextMessage(); answer.threadID != 0 || answer.replyTo != 0 || answer.text != NoSessionsReply {
		t.Fatalf("answer = %+v, want it in the main view, not as a reply", answer)
	}
	if deleted := receive(t, h.ctx, h.tabs.deleted); deleted != 70 {
		t.Fatalf("deleted tab %d, want 70", deleted)
	}
	h.waitOffset(3)
	if tab, found := h.registryTab(70); found {
		t.Fatalf("the command's tab is still recorded: %+v", tab)
	}

	// An ordinary first message keeps its tab, and a command after it is answered there.
	h.push(implicitTab(3, 71))
	h.turnInTab(runtime, 4, 71, "about the garden")
	h.push(tabMessage(5, 71, "/sessions"))
	if answer := h.nextMessage(); answer.threadID != 71 {
		t.Fatalf("command in a tab in use answered in %d, want 71", answer.threadID)
	}

	// A command long after its tab was opened stays in the tab.
	h.push(implicitTab(6, 72))
	h.waitOffset(7)
	h.service.freshTabs.Store(chatTab{101, 72}, time.Now().Add(-3*time.Minute))
	h.push(tabMessage(7, 72, "/sessions"))
	if answer := h.nextMessage(); answer.threadID != 72 {
		t.Fatalf("late command answered in %d, want its tab", answer.threadID)
	}

	// A tab Telegram refuses to delete stays, and the answer is still in the main view.
	h.mu.Lock()
	h.tabs.deleteStatus = 400
	h.mu.Unlock()
	h.push(implicitTab(8, 73), tabMessage(9, 73, "/sessions"))
	if answer := h.nextMessage(); answer.threadID != 0 {
		t.Fatalf("answer with a refused delete = %+v, want the main view", answer)
	}
	if deleted := receive(t, h.ctx, h.tabs.deleted); deleted != 73 {
		t.Fatalf("deleted tab %d, want 73", deleted)
	}
	h.waitOffset(10)
	if _, found := h.registryTab(73); !found {
		t.Fatal("a tab Telegram kept was forgotten")
	}
	select {
	case deleted := <-h.tabs.deleted:
		t.Fatalf("unexpected delete of tab %d", deleted)
	default:
	}

	// A command whose answer failed is retried, and the retry still answers in the main view.
	h.mu.Lock()
	h.tabs.deleteStatus = 0
	h.sendStatuses = []int{http.StatusInternalServerError}
	h.mu.Unlock()
	h.push(implicitTab(10, 74), tabMessage(11, 74, "/sessions"))
	if answer := h.nextMessage(); answer.threadID != 0 || answer.status != http.StatusInternalServerError {
		t.Fatalf("first attempt = %+v, want a failed send to the main view", answer)
	}
	// Telegram hands the update over again once the bot asks from the same offset.
	h.waitOffset(11)
	h.push(tabMessage(11, 74, "/sessions"))
	if answer := h.nextMessage(); answer.threadID != 0 || answer.status != http.StatusOK {
		t.Fatalf("retried attempt = %+v, want it in the main view", answer)
	}
	if deleted := receive(t, h.ctx, h.tabs.deleted); deleted != 74 {
		t.Fatalf("deleted tab %d, want 74", deleted)
	}
}

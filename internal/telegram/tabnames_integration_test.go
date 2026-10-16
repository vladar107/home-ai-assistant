package telegram

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

// implicitTab is the service message of a tab Alice opened without naming it.
func implicitTab(updateID, threadID int64) Update {
	return Update{UpdateID: updateID, Message: &Message{
		MessageID: requestMessageID(updateID), MessageThreadID: threadID, From: &User{ID: 101, FirstName: "Test"},
		Chat: Chat{ID: 101, Type: ChatTypePrivate}, ForumTopicCreated: &ForumTopicCreated{Name: "New chat", IsNameImplicit: true},
	}}
}

// userRenamedTab is the service message of Alice renaming a tab.
func userRenamedTab(updateID, threadID int64, name string) Update {
	return Update{UpdateID: updateID, Message: &Message{
		MessageID: requestMessageID(updateID), MessageThreadID: threadID, From: &User{ID: 101, FirstName: "Test"},
		Chat: Chat{ID: 101, Type: ChatTypePrivate}, ForumTopicEdited: &ForumTopicEdited{Name: &name},
	}}
}

// namingCall takes the next model call, which must be the one naming a tab after the exchange
// that asked text, and answers it.
func (h *conversationHarness) namingCall(runtime *controlledRuntime, text, answer string) agentruntime.RunRequest {
	h.t.Helper()
	call := receive(h.t, h.ctx, runtime.calls)
	if !strings.HasPrefix(call.request.Message, "[system] Name the chat tab") || !strings.Contains(call.request.Message, "User:\n"+text+"\n") ||
		!strings.Contains(call.request.Message, "Assistant:\nanswer to "+text) {
		h.t.Fatalf("model call = %q, want the naming of the exchange %q", call.request.Message, text)
	}
	call.answer <- agentruntime.RunResult{Final: answer, Response: answer}
	return call.request
}

func (h *conversationHarness) runtimeSession(threadID int64) string {
	h.t.Helper()
	var runtimeSessionID string
	if err := h.db.QueryRowContext(h.ctx,
		`SELECT runtime_session_id FROM conversation_sessions WHERE principal_id = 'alice' AND thread_id = ? AND status = 'active'`, threadID,
	).Scan(&runtimeSessionID); err != nil {
		h.t.Fatal(err)
	}
	return runtimeSessionID
}

func TestABotNamedTabIsNamedOncePerConversationWithAnIcon(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, true)
	h.push(implicitTab(1, 40))
	h.waitOffset(2)

	h.turnInTab(runtime, 2, 40, "when do the tomatoes need water")
	sessionID, _, _ := h.laneSession(40)
	request := h.namingCall(runtime, "when do the tomatoes need water", `{"name":"Watering tomatoes","icon":"🌱"}`)
	if request.PrincipalID != "alice" || !request.Tools.Empty() || request.Memory != "" || request.Jobs != "" || request.WebSearch ||
		request.SessionID == "" || request.SessionID == h.runtimeSession(40) {
		t.Fatalf("naming call = %+v, want a fresh session on Alice's subscription without tools or memory", request)
	}
	for _, icon := range testTopicIcons {
		if !strings.Contains(request.Message, icon.Emoji) {
			t.Fatalf("naming call does not offer %q: %q", icon.Emoji, request.Message)
		}
	}
	if edit := receive(t, h.ctx, h.tabs.topicEdits); edit != (editedTopic{threadID: 40, name: "Watering tomatoes", iconID: "5000000000000000001", named: true, iconed: true}) {
		t.Fatalf("tab edit = %+v", edit)
	}
	h.waitTabName(40, "Watering tomatoes")
	if tab, _ := h.registryTab(40); tab.NamedBy != sqlite.TabNamedByBot || tab.IconCustomEmojiID != "5000000000000000001" || tab.NamedSessionID != sessionID {
		t.Fatalf("named tab in the registry = %+v", tab)
	}

	// The same conversation names the tab once, also after a restart, which forgets what the
	// service remembered.
	h.turnInTab(runtime, 3, 40, "and in a heat wave")
	h.service.namedTabs.Clear()
	h.turnInTab(runtime, 4, 40, "and in the rain")

	h.turnInTab(runtime, 5, 0, "groceries")
	h.push(textUpdate(6, 101, "/sessions"))
	if list := h.nextMessage(); !strings.Contains(list.text, "(open in “Watering tomatoes”)") {
		t.Fatalf("/sessions = %q, want the tab's new name", list.text)
	}

	// A new conversation in the tab may name it again.
	h.push(tabMessage(7, 40, "/new"))
	h.wantReply(101, 7, NewSessionReply)
	h.turnInTab(runtime, 8, 40, "which seeds to order")
	h.namingCall(runtime, "which seeds to order", `{"name":"Seed order","icon":"📚"}`)
	if edit := receive(t, h.ctx, h.tabs.topicEdits); edit.threadID != 40 || edit.name != "Seed order" || edit.iconID != "5000000000000000003" {
		t.Fatalf("tab edit after /new = %+v", edit)
	}
	h.waitTabName(40, "Seed order")
	h.turnInTab(runtime, 9, 0, "more groceries")

	logs := h.logs.String()
	for _, private := range []string{"Watering tomatoes", "Seed order", "tomatoes need water", "seeds to order"} {
		if strings.Contains(logs, private) {
			t.Fatalf("%q reached the logs:\n%s", private, logs)
		}
	}
	if !strings.Contains(logs, "msg=telegram.tab.named principal_id=alice thread_id=40 icon=true") {
		t.Fatalf("naming not logged:\n%s", logs)
	}

	// Without a connected subscription nothing is asked.
	if _, err := h.conversation.NameTab(h.ctx, "bob", "a question", "an answer", nil); err == nil {
		t.Fatal("a tab was named without a model connection")
	}
	if len(runtime.calls) != 0 {
		t.Fatal("naming without a model connection called the model")
	}
}

func TestUserFixedAndMainViewTabsAreNeverRenamed(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, true)
	if _, err := h.store.AddTab(h.ctx, "alice", 42, "Reports", sqlite.TabNamedByFixed); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AddTab(h.ctx, "alice", 41, "Conversation from 1 October", sqlite.TabNamedByBot); err != nil {
		t.Fatal(err)
	}
	// A tab opened without a name that the user then named keeps the user's name.
	h.push(implicitTab(1, 43), userRenamedTab(2, 43, "Vegetables"))
	h.waitOffset(3)

	h.turnInTab(runtime, 3, 50, "about the trip")
	h.turnInTab(runtime, 4, 42, "about the report")
	h.turnInTab(runtime, 5, 43, "about the vegetables")
	h.turnInTab(runtime, 6, 0, "about the groceries")
	// A tab that was open before the bot recorded it is first seen through the root its messages
	// reply to, and it keeps its name even when Telegram chose that name.
	existing := tabMessage(7, 44, "about the old tab")
	existing.Message.ReplyToMessage.ForumTopicCreated = &ForumTopicCreated{Name: "Old tab", IsNameImplicit: true}
	h.push(existing)
	call := receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "answer to about the old tab"}
	h.nextMessage()
	h.waitState(7, "delivered")
	// Only the tab the bot made is named.
	h.turnInTab(runtime, 8, 41, "about the book club")
	h.namingCall(runtime, "about the book club", `{"name":"Book club","icon":"📚"}`)
	if edit := receive(t, h.ctx, h.tabs.topicEdits); edit.threadID != 41 || edit.name != "Book club" {
		t.Fatalf("first tab edit = %+v, want the bot's tab", edit)
	}
	h.waitTabName(41, "Book club")
	h.turnInTab(runtime, 9, 50, "more about the trip")

	for threadID, want := range map[int64]sqlite.Tab{
		50: {Name: "Trip notes", NamedBy: sqlite.TabNamedByUser},
		42: {Name: "Reports", NamedBy: sqlite.TabNamedByFixed},
		43: {Name: "Vegetables", NamedBy: sqlite.TabNamedByUser},
		44: {Name: "Old tab", NamedBy: sqlite.TabNamedByUser},
	} {
		if tab, found := h.registryTab(threadID); !found || tab.Name != want.Name || tab.NamedBy != want.NamedBy || tab.NamedSessionID != "" {
			t.Fatalf("tab %d = %+v %v, want %+v", threadID, tab, found, want)
		}
	}
}

func TestATabNameOutsideTheRulesIsNotUsed(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, true)
	for _, threadID := range []int64{61, 62, 63, 64, 65} {
		if _, err := h.store.AddTab(h.ctx, "alice", threadID, "Made by the bot", sqlite.TabNamedByBot); err != nil {
			t.Fatal(err)
		}
	}

	h.turnInTab(runtime, 1, 61, "first")
	h.namingCall(runtime, "first", `{"name":"Garden","icon":"🚀"}`)
	if edit := receive(t, h.ctx, h.tabs.topicEdits); edit != (editedTopic{threadID: 61, name: "Garden", named: true}) {
		t.Fatalf("edit with an icon outside the set = %+v, want the name alone", edit)
	}
	h.waitTabName(61, "Garden")

	for index, answer := range []string{
		`{"name":"` + strings.Repeat("long ", 7) + `","icon":"🌱"}`,
		`{"name":"   ","icon":"🌱"}`,
		`Garden`,
	} {
		threadID, updateID := int64(62+index), int64(2+index)
		text := "question " + string(rune('a'+index))
		h.turnInTab(runtime, updateID, threadID, text)
		h.namingCall(runtime, text, answer)
	}

	// A fenced answer and an emoji without its variation selector are still taken.
	h.turnInTab(runtime, 5, 65, "last")
	h.namingCall(runtime, "last", "```json\n{\"name\":\"Alarm clock\",\"icon\":\"⏰\"}\n```")
	if edit := receive(t, h.ctx, h.tabs.topicEdits); edit.threadID != 65 || edit.name != "Alarm clock" || edit.iconID != "5000000000000000002" {
		t.Fatalf("edit after the rejected names = %+v, want tab 65's", edit)
	}
	h.waitTabName(65, "Alarm clock")

	// A rejected name is not asked for again in the same conversation.
	h.turnInTab(runtime, 6, 62, "another question")
	h.turnInTab(runtime, 7, 0, "groceries")
	for _, threadID := range []int64{62, 63, 64} {
		session, _, _ := h.laneSession(threadID)
		if tab, _ := h.registryTab(threadID); tab.Name != "Made by the bot" || tab.IconCustomEmojiID != "" || tab.NamedSessionID != session {
			t.Fatalf("tab %d after a rejected name = %+v", threadID, tab)
		}
	}
}

func TestAFailedIconReadStillNamesTabs(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{topics: true})
	h.tabs.iconStatus = 500
	h.provision("alice", "acct-alice")
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	h.push(implicitTab(1, 40))
	h.waitOffset(2)

	h.turnInTab(runtime, 2, 40, "when do the tomatoes need water")
	request := h.namingCall(runtime, "when do the tomatoes need water", `{"name":"Watering tomatoes","icon":"🌱"}`)
	if strings.Contains(request.Message, "🌱") {
		t.Fatalf("naming call offered icons it could not read: %q", request.Message)
	}
	if edit := receive(t, h.ctx, h.tabs.topicEdits); edit != (editedTopic{threadID: 40, name: "Watering tomatoes", named: true}) {
		t.Fatalf("tab edit without icons = %+v", edit)
	}
	h.waitTabName(40, "Watering tomatoes")
	if !strings.Contains(h.logs.String(), "msg=telegram.tab.icons_failed") {
		t.Fatalf("failed icon read not logged:\n%s", h.logs.String())
	}
	if _, ok := h.service.IconFor("🌱"); ok {
		t.Fatal("an icon was offered without being read")
	}
}

func TestAnEarlierConversationsNameDoesNotReplaceALaterOne(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, true)
	h.push(implicitTab(1, 40))
	h.waitOffset(2)

	h.turnInTab(runtime, 2, 40, "when do the tomatoes need water")
	stale := receive(t, h.ctx, runtime.calls)
	if !strings.HasPrefix(stale.request.Message, "[system] Name the chat tab") {
		t.Fatalf("model call = %q, want the first conversation's naming", stale.request.Message)
	}

	// While the first conversation's name is still being chosen, a new one names the tab.
	h.push(tabMessage(3, 40, "/new"))
	h.wantReply(101, 3, NewSessionReply)
	h.turnInTab(runtime, 4, 40, "which seeds to order")
	later, _, _ := h.laneSession(40)
	h.namingCall(runtime, "which seeds to order", `{"name":"Seed order","icon":"📚"}`)
	receive(t, h.ctx, h.tabs.topicEdits)
	h.waitTabName(40, "Seed order")

	stale.answer <- agentruntime.RunResult{Final: `{"name":"Watering tomatoes","icon":"🌱"}`}
	h.waitLog("msg=telegram.tab.name_failed principal_id=alice thread_id=40 reason=superseded")
	if len(h.tabs.topicEdits) != 0 {
		t.Fatalf("the earlier conversation's name reached Telegram: %+v", <-h.tabs.topicEdits)
	}
	if tab, _ := h.registryTab(40); tab.Name != "Seed order" || tab.IconCustomEmojiID != "5000000000000000003" || tab.NamedSessionID != later {
		t.Fatalf("tab after the earlier name came in = %+v", tab)
	}
}

func TestTabIconsAreFoundByEmoji(t *testing.T) {
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{topics: true})
	// Telegram may list an emoji twice; the first icon stays.
	h.tabs.icons = append(slices.Clone(testTopicIcons), TopicIcon{Emoji: "🌱", CustomEmojiID: "5000000000000000009"})
	h.provision("alice", "acct-alice")
	h.start()
	for emoji, want := range map[string]string{"⏰": "5000000000000000002", "⏰\uFE0F": "5000000000000000002", "🌱": "5000000000000000001"} {
		if id, ok := h.service.IconFor(emoji); !ok || id != want {
			t.Fatalf("IconFor(%q) = %q %v, want %q", emoji, id, ok, want)
		}
	}
	if id, ok := h.service.IconFor("🚀"); ok {
		t.Fatalf("IconFor of an emoji outside the set = %q", id)
	}
}

func TestWithTopicsOffNoTabIsNamed(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newTabsHarness(t, runtime, false)
	if _, err := h.store.AddTab(h.ctx, "alice", 40, "Made by the bot", sqlite.TabNamedByBot); err != nil {
		t.Fatal(err)
	}
	h.turnInTab(runtime, 1, 0, "groceries")
	h.service.TabReplied(h.ctx, conversation.TabReply{
		Lane: conversation.Lane{PrincipalID: "alice", ThreadID: 40}, ChatID: 101, SessionID: "a-session", Message: "a question", Reply: "an answer",
	})
	if len(runtime.calls) != 0 {
		t.Fatal("a tab was named with topics off")
	}
	if tab, _ := h.registryTab(40); tab.NamedSessionID != "" || tab.Name != "Made by the bot" {
		t.Fatalf("tab with topics off = %+v", tab)
	}
	if _, ok := h.service.IconFor("🌱"); ok {
		t.Fatal("icons were read with topics off")
	}
}

func (h *conversationHarness) waitLog(line string) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !strings.Contains(h.logs.String(), line) {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("%q never logged:\n%s", line, h.logs.String())
		}
	}
}

// waitTabName waits until the registry holds the tab under name, which it records after the edit.
func (h *conversationHarness) waitTabName(threadID int64, name string) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if tab, found := h.registryTab(threadID); found && tab.Name == name {
			return
		}
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("tab %d never got its name", threadID)
		}
	}
}

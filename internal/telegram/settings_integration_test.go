package telegram

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestSettingsCallbacksAreAuthorizedAnsweredAndRefreshed(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice-1111")
	h.start()
	h.requestSettings(1, 101)
	group := callbackUpdate(3, 101, settingsMessageID(1), "model:disconnect")
	group.CallbackQuery.Message.Chat = Chat{ID: -500, Type: "group"}
	noMessage := callbackUpdate(4, 101, settingsMessageID(1), "model:disconnect")
	noMessage.CallbackQuery.Message = nil
	h.push(
		callbackUpdate(2, 303, settingsMessageID(1), "model:disconnect"),
		group,
		noMessage,
		callbackUpdate(5, 101, settingsMessageID(1), "model:explode"),
		callbackUpdate(6, 101, settingsMessageID(1), "nope"),
		callbackUpdate(7, 101, settingsMessageID(1), strings.Repeat("model:disconnect", 5)),
	)
	// Only Alice's presses are answered, and an unknown action just refreshes the current view.
	for _, id := range []int64{5, 6, 7} {
		h.wantAnswer(id)
		edit := h.nextEdit()
		if edit.messageID != settingsMessageID(1) || !strings.Contains(edit.text, "ending in 1111") || !slices.Equal(buttonData(edit.keyboard), []string{"model:disconnect"}) {
			t.Fatalf("unknown action %d rendered %+v", id, edit)
		}
	}
	h.waitOffset(8)
	if _, exists := h.credentialFile("alice"); !exists {
		t.Fatal("an ignored press disconnected the model")
	}
	logs := h.logs.String()
	for _, want := range []string{"reason=non_private_chat", "reason=malformed_callback", "reason=unknown_action", "command=settings"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %s: %s", want, logs)
		}
	}
	if strings.Contains(logs, "model:explode") || strings.Contains(logs, "nope") {
		t.Fatalf("logs contain callback data: %s", logs)
	}
	h.stop()
	h.start()
	h.waitOffset(8)
	select {
	case answer := <-h.answers:
		t.Fatalf("an ignored or replayed press was answered: %+v", answer)
	case edit := <-h.edits:
		t.Fatalf("an ignored or replayed press edited a message: %+v", edit)
	default:
	}
}

func TestSettingsEditFallsBackWhenTelegramRejectsTheEdit(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	h.provision("alice", "acct-alice-1111")
	h.start()
	h.requestSettings(1, 101)
	h.mu.Lock()
	h.editStatus, h.editReason = http.StatusBadRequest, "Bad Request: message can't be edited"
	h.mu.Unlock()
	h.push(callbackUpdate(2, 101, settingsMessageID(1), "model:disconnect"))
	h.wantAnswer(2)
	if edit := h.nextEdit(); !strings.Contains(edit.text, ModelNotConnectedLine) {
		t.Fatalf("rejected edit = %+v", edit)
	}
	fresh := h.nextMessage()
	if fresh.chatID != 101 || fresh.replyTo != 0 || !strings.Contains(fresh.text, ModelNotConnectedLine) || !slices.Equal(buttonData(fresh.keyboard), []string{"model:connect"}) {
		t.Fatalf("fallback settings message = %+v", fresh)
	}

	h.mu.Lock()
	h.editReason = "Bad Request: message is not modified"
	h.mu.Unlock()
	h.push(callbackUpdate(3, 101, settingsMessageID(1), "model:disconnect"))
	h.wantAnswer(3)
	h.nextEdit()
	h.waitOffset(4)
	select {
	case message := <-h.messages:
		t.Fatalf("an unchanged edit sent a new message: %+v", message)
	default:
	}
}

func TestRemovedConnectionCommandsGetHelp(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	h.start()
	h.push(textUpdate(1, 101, "/connect"), textUpdate(2, 101, "/connection"), textUpdate(3, 101, "/disconnect"))
	for id := int64(1); id <= 3; id++ {
		h.wantReply(101, id, HelpReply)
	}
}

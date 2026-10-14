package telegram

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

func (h *conversationHarness) pressUndo(updateID, userID, messageID int64, data string) callbackAnswer {
	h.t.Helper()
	h.push(callbackUpdate(updateID, userID, messageID, data))
	answer := receive(h.t, h.ctx, h.answers)
	if answer.id != callbackID(updateID) {
		h.t.Fatalf("answered callback %q, want %q", answer.id, callbackID(updateID))
	}
	return answer
}

// An Undo button on a "Done" message undoes the action once and only for its owner; the message is
// edited, the next turn learns of it, and unknown Undo data never turns the message into settings.
func TestUndoButtonUndoesADirectActionOnceForItsOwner(t *testing.T) {
	h, runtime, book := newActHarness(t, readingTools{})
	h.push(textUpdate(1, 101, "save a note"))
	call := receive(t, h.ctx, runtime.calls)
	text, err := writeNote(call.ctx, h, "groceries", false)
	wantDone(t, text, err)
	h.finishTurn(call, 1, "saved")
	done := h.nextMessage()
	id, _, _, _ := h.directRecord("groceries")
	if fmt.Sprint(buttonData(done.keyboard)) != fmt.Sprint([]string{"undo:" + id}) {
		t.Fatalf("done message = %+v, want an Undo button", done)
	}
	h.waitMessageRecorded(done.messageID)

	if answer := h.pressUndo(2, 202, done.messageID, "undo:"+id); answer.text != "" {
		t.Fatalf("foreign press answer = %q, want none", answer.text)
	}
	if _, found := book.note("groceries"); !found {
		t.Fatal("another principal's press undid the note")
	}
	if _, status, _, _ := h.directRecord("groceries"); status != approval.StatusDone {
		t.Fatalf("status after a foreign press = %s", status)
	}

	if answer := h.pressUndo(3, 101, done.messageID, "undo:"+id); answer.text != "Undone." {
		t.Fatalf("press answer = %q, want Undone.", answer.text)
	}
	if _, found := book.note("groceries"); found {
		t.Fatal("the note is still there after Undo")
	}
	edit := h.nextEdit()
	if edit.messageID != done.messageID || !strings.Contains(edit.text, "Undone.") || len(buttonData(edit.keyboard)) != 0 {
		t.Fatalf("done message after undo = %+v", edit)
	}

	if answer := h.pressUndo(4, 101, done.messageID, "undo:"+id); answer.text != "This was already undone." {
		t.Fatalf("second press answer = %q", answer.text)
	}

	unknown := "undo:" + strings.Repeat("0", len(id))
	if answer := h.pressUndo(5, 101, done.messageID, unknown); answer.text != "" {
		t.Fatalf("unknown id answer = %q, want none", answer.text)
	}
	if answer := h.pressUndo(6, 101, done.messageID, "undo:short"); answer.text != "" {
		t.Fatalf("malformed id answer = %q, want none", answer.text)
	}
	h.expectNoEdit(200 * time.Millisecond)

	h.push(textUpdate(7, 101, "what changed?"))
	call = receive(t, h.ctx, runtime.calls)
	if !strings.Contains(call.request.Message, "pressed Undo on your note_write action; it was undone") {
		t.Fatalf("next turn message = %q, want the Undo notice", call.request.Message)
	}
	h.finishTurn(call, 7, "one undone")
}

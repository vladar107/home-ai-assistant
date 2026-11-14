package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// proposeLongBatch runs one model turn that proposes count one-time jobs with long prompts and
// returns the proposal messages Telegram received for the batch.
func (h *conversationHarness) proposeLongBatch(runtime *controlledRuntime, updateID int64, count int) []sentMessage {
	h.t.Helper()
	h.push(textUpdate(updateID, 101, "schedule all of these"))
	call := receive(h.t, h.ctx, runtime.calls)
	// A chat turn's call budget is smaller than a full batch, so the batch goes through the approval
	// service that the schedule tool proposes with.
	for index := 1; index <= count; index++ {
		confirmed := map[string]string{
			"name":   fmt.Sprintf("Check %d", index),
			"at":     fmt.Sprintf("2026-09-28 %02d:00", 8+index),
			"zone":   "Europe/Berlin",
			"prompt": fmt.Sprintf("Job %d: ", index) + strings.Repeat("Look up the forecast and summarise it. ", 22),
		}
		arguments, err := json.Marshal(confirmed)
		if err != nil {
			h.t.Fatal(err)
		}
		summary := fmt.Sprintf("Schedule %q\nRuns once: %s\n\n%s", confirmed["name"], confirmed["at"], confirmed["prompt"])
		if _, err := h.approvals.Propose(call.ctx, "alice", approval.Proposal{Tool: "schedule_propose", Arguments: arguments, Summary: summary}); err != nil {
			h.t.Fatalf("proposal %d: %v", index, err)
		}
	}
	call.answer <- agentruntime.RunResult{Response: "proposed all"}
	h.wantReply(101, updateID, "proposed all")
	h.waitState(updateID, "delivered")

	var messages []sentMessage
	for covered := 0; covered < count; {
		message := h.nextMessage()
		messages = append(messages, message)
		covered += strings.Count(message.text, "Job ")
	}
	h.expectNoMessage(100 * time.Millisecond)
	return messages
}

// requireSplitBatch checks that the batch is several messages that each stay under Telegram's
// limit, hold every item whole in exactly one of them, and carry buttons on the last alone.
func requireSplitBatch(t *testing.T, messages []sentMessage, count int) (confirm, reject string) {
	t.Helper()
	if len(messages) < 3 {
		t.Fatalf("a batch of %d long proposals was sent as %d messages", count, len(messages))
	}
	for index := 1; index <= count; index++ {
		seen := 0
		for _, message := range messages {
			if strings.Contains(message.text, fmt.Sprintf("\n\n%d. ", index)) {
				seen++
			}
		}
		if seen != 1 {
			t.Fatalf("item %d appears in %d messages", index, seen)
		}
	}
	for index, message := range messages {
		if units := len(utf16.Encode([]rune(message.text))); units > 4096 {
			t.Fatalf("message %d has %d units", index, units)
		}
		if index < len(messages)-1 && len(message.keyboard) != 0 {
			t.Fatalf("message %d before the last has buttons %v", index, buttonData(message.keyboard))
		}
	}
	if !strings.HasPrefix(messages[0].text, fmt.Sprintf("%d changes, confirmed or rejected together:", count)) {
		t.Fatalf("first message = %q", messages[0].text)
	}
	return approvalButtons(t, messages[len(messages)-1].keyboard)
}

func (h *conversationHarness) waitMessageRecorded(messageID int64) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for h.countRows(`SELECT count(*) FROM approvals WHERE message_id = ?`, messageID) == 0 {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("proposal message %d was never recorded", messageID)
		}
	}
}

// expirePendingApprovals back-dates every pending approval so that the next sweep expires it, as a
// lifetime running out would, without the test waiting for one. It first waits for the proposal's
// last message to be recorded, because a sweep that finds a record without its message cannot edit
// it.
func (h *conversationHarness) expirePendingApprovals(lastMessageID int64) {
	h.t.Helper()
	h.waitMessageRecorded(lastMessageID)
	if _, err := h.db.ExecContext(h.ctx, `UPDATE approvals SET created_at = '2026-08-31T00:00:00.000Z', expires_at = '2026-09-01T00:00:00.000Z' WHERE status = 'pending'`); err != nil {
		h.t.Fatal(err)
	}
}

func (h *conversationHarness) expectNoEdit(wait time.Duration) {
	h.t.Helper()
	select {
	case extra := <-h.edits:
		h.t.Fatalf("an earlier message was edited: %+v", extra)
	case <-time.After(wait):
	}
}

func TestLongBatchIsSentAcrossMessagesAndConfirmedTogether(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	messages := h.proposeLongBatch(runtime, 1, 10)
	confirm, _ := requireSplitBatch(t, messages, 10)
	last := messages[len(messages)-1]
	h.waitMessageRecorded(last.messageID)
	if count := h.countRows(`SELECT count(*) FROM approvals WHERE message_id IS NOT NULL`); count != 10 {
		t.Fatalf("records with a message = %d, want 10", count)
	}
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("a proposal created %d jobs before confirmation", count)
	}

	h.push(callbackUpdate(2, 101, last.messageID, confirm))
	h.wantAnswer(2)
	edit := h.nextEdit()
	if edit.messageID != last.messageID || len(edit.keyboard) != 0 || !strings.Contains(edit.text, "Scheduled for") {
		t.Fatalf("button message after confirm = %+v", edit)
	}
	h.expectNoEdit(100 * time.Millisecond)
	h.continueTurn(runtime, 2)
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 10 {
		t.Fatalf("jobs after confirm = %d, want 10", count)
	}
	if count := h.countRows(`SELECT count(*) FROM approvals WHERE status = ?`, approval.StatusExecuted); count != 10 {
		t.Fatalf("executed approvals = %d, want 10", count)
	}
}

func TestLongBatchRejectionEditsTheButtonMessageOnly(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	messages := h.proposeLongBatch(runtime, 1, 6)
	_, reject := requireSplitBatch(t, messages, 6)
	last := messages[len(messages)-1]
	h.waitMessageRecorded(last.messageID)

	oversized := approval.Proposal{Tool: "schedule_propose", Arguments: json.RawMessage(`{}`), Summary: strings.Repeat("x", 2600)}
	if _, err := h.approvals.Propose(h.ctx, "alice", oversized); !errors.Is(err, approval.ErrSummaryTooLong) {
		t.Fatalf("an oversized proposal = %v, want ErrSummaryTooLong", err)
	}

	h.push(callbackUpdate(2, 101, last.messageID, reject))
	h.wantAnswer(2)
	edit := h.nextEdit()
	if edit.messageID != last.messageID || len(edit.keyboard) != 0 || !strings.Contains(edit.text, "Rejected; nothing was written.") {
		t.Fatalf("button message after reject = %+v", edit)
	}
	h.expectNoEdit(100 * time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM approvals WHERE status = ?`, approval.StatusRejected); count != 6 {
		t.Fatalf("rejected approvals = %d, want 6", count)
	}
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("a rejected batch created %d jobs", count)
	}
}

func TestLongBatchExpiryEditsTheButtonMessageOnly(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, approvalSweep: 100 * time.Millisecond})
	h.provision("alice", "account-alice")
	h.start()

	messages := h.proposeLongBatch(runtime, 1, 6)
	requireSplitBatch(t, messages, 6)
	last := messages[len(messages)-1]
	h.expirePendingApprovals(last.messageID)

	edit := h.nextEdit()
	if edit.messageID != last.messageID || len(edit.keyboard) != 0 || !strings.Contains(edit.text, "Expired; nothing was written.") {
		t.Fatalf("button message after expiry = %+v", edit)
	}
	h.expectNoEdit(300 * time.Millisecond)
}

func TestProposeAllRecordsNothingWhenTheTurnsBundleCannotHoldIt(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	job := func(index int) approval.Proposal {
		arguments, err := json.Marshal(map[string]string{"name": fmt.Sprintf("Check %d", index), "at": fmt.Sprintf("2026-09-28 %02d:00", 8+index), "zone": "Europe/Berlin", "prompt": "Look up the forecast."})
		if err != nil {
			t.Fatal(err)
		}
		return approval.Proposal{Tool: "schedule_propose", Arguments: arguments, Summary: fmt.Sprintf("Schedule check %d", index)}
	}
	h.push(textUpdate(1, 101, "schedule all of these"))
	call := receive(t, h.ctx, runtime.calls)
	for index := 1; index <= 8; index++ {
		if _, err := h.approvals.Propose(call.ctx, "alice", job(index)); err != nil {
			t.Fatalf("proposal %d: %v", index, err)
		}
	}

	if _, err := h.approvals.ProposeAll(call.ctx, "alice", []approval.Proposal{job(9), job(10), job(11)}); !errors.Is(err, approval.ErrBundleFull) {
		t.Fatalf("a batch past the bundle's room = %v, want ErrBundleFull", err)
	}
	if count := h.countRows(`SELECT count(*) FROM approvals`); count != 8 {
		t.Fatalf("approvals after a refused batch = %d, want the 8 proposed before it", count)
	}
	if _, err := h.approvals.ProposeAll(call.ctx, "alice", []approval.Proposal{job(8), job(9), job(10)}); err != nil {
		t.Fatalf("a batch that fits once its pending duplicate is counted = %v", err)
	}
	if count := h.countRows(`SELECT count(*) FROM approvals`); count != 10 {
		t.Fatalf("approvals after a batch that fits = %d, want 10", count)
	}

	call.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
}

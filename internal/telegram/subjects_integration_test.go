package telegram

import (
	"strings"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// runReport runs the job now and returns the report it delivered, once the report is kept.
func (h *conversationHarness) runReport(runtime *controlledRuntime, job schedule.Job, text string) sentMessage {
	h.t.Helper()
	if queued, err := h.scheduler.RunNow(h.ctx, "alice", job.ID, ""); err != nil || !queued {
		h.t.Fatalf("run now = %v %v", queued, err)
	}
	h.scheduler.Wake()
	run := receive(h.t, h.ctx, runtime.calls)
	run.answer <- agentruntime.RunResult{Response: text, Final: text}
	report := h.nextMessage()
	h.waitRun(job.ID, schedule.StatusDelivered)
	return report
}

// answerTurn answers the next turn and returns its message and the answer the user got.
func (h *conversationHarness) answerTurn(runtime *controlledRuntime, updateID int64, answer string) (string, sentMessage) {
	h.t.Helper()
	call := receive(h.t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: answer}
	sent := h.nextMessage()
	h.waitState(updateID, "delivered")
	return call.request.Message, sent
}

func replyInTab(updateID, threadID int64, text string, messageID int64) Update {
	update := tabMessage(updateID, threadID, text)
	update.Message.ReplyToMessage = &Message{MessageID: messageID, MessageThreadID: threadID}
	return update
}

func TestFeedConversationsKnowTheirSubject(t *testing.T) {
	runtime := newControlledRuntime()
	h, _ := newFeedHarness(t, runtime, nil, true)
	bins := h.createJob("alice", "00000000000000f1", "Bins", "0 9 * * *", "Which bins go out?")
	plants := h.createJob("alice", "00000000000000f2", "Plants", "0 9 * * *", "Do the plants need water?")
	binsLine := "[system] This conversation is about the user's scheduled job “Bins” (id 00000000000000f1); schedule_get shows it in full."
	plantsLine := "[system] This conversation is about the user's scheduled job “Plants” (id 00000000000000f2)"

	first := h.runReport(runtime, bins, "Paper and plastic.")
	reports := first.threadID
	if reports == 0 {
		t.Fatalf("report = %+v, want it in the Reports tab", first)
	}

	// Typing right after a report discusses that report, and says so.
	h.push(tabMessage(1, reports, "which day?"))
	message, answer := h.answerTurn(runtime, 1, "Tuesday.")
	if !strings.Contains(message, binsLine) || answer.threadID != reports || answer.text != "About “Bins”:\n\nTuesday." {
		t.Fatalf("first turn about the report = %q, answered %+v", message, answer)
	}
	h.push(tabMessage(2, reports, "and paper?"))
	if message, answer := h.answerTurn(runtime, 2, "Thursday."); !strings.Contains(message, binsLine) || answer.text != "Thursday." {
		t.Fatalf("follow-up = %q, answered %+v", message, answer)
	}
	h.push(textUpdate(3, 101, "hello"))
	if message, _ := h.answerTurn(runtime, 3, "hi"); strings.Contains(message, "This conversation is about") {
		t.Fatalf("a turn without a subject was given one: %q", message)
	}

	h.runReport(runtime, plants, "Yes, water them.")
	h.push(tabMessage(4, reports, "how much?"))
	if message, answer := h.answerTurn(runtime, 4, "A litre."); !strings.Contains(message, plantsLine) || answer.text != "About “Plants”:\n\nA litre." {
		t.Fatalf("turn after another job's report = %q, answered %+v", message, answer)
	}

	// Replying to the first report returns to its conversation, saying both.
	h.push(replyInTab(5, reports, "back to the bins", first.messageID))
	message, answer = h.answerTurn(runtime, 5, "Paper on Thursday.")
	if !strings.Contains(message, binsLine) || !strings.HasPrefix(answer.text, "About “Bins”, back in the conversation from ") ||
		!strings.HasSuffix(answer.text, ".\n\nPaper on Thursday.") {
		t.Fatalf("reply to the first report = %q, answered %+v", message, answer)
	}
	if count := h.countRows(`SELECT count(*) FROM conversation_sessions WHERE principal_id = 'alice' AND subject IS NOT NULL`); count != 2 {
		t.Fatalf("%d conversations about jobs, want one per job", count)
	}
}

func TestARepliedAlertIsQuotedOnceWithItsMarks(t *testing.T) {
	runtime := newControlledRuntime()
	evaluator := &stubEvaluator{}
	evaluator.set(watch.Observation{State: "cancelled", Triggered: true, Headline: "It is cancelled.", Private: true})
	h, _ := newFeedHarness(t, runtime, evaluator, true)
	const trainID, agentID = "00000000000000a1", "00000000000000a9"
	h.createWatch("alice", trainID, trainWatch())
	h.watches.Cycle(h.ctx)
	alert := h.nextMessage()
	watches := alert.threadID

	h.push(replyInTab(1, watches, "is there another train?", alert.messageID))
	message, _ := h.answerTurn(runtime, 1, "The next one runs.")
	if count := strings.Count(message, "It is cancelled."); count != 1 || !strings.Contains(message, "The user is replying to this message from their watch") ||
		!strings.Contains(message, "(id "+trainID+"); watch_list shows it in full.") {
		t.Fatalf("turn replying to an alert quoted it %d times: %q", count, message)
	}
	if private := h.countRows(`SELECT count(*) FROM conversation_sessions WHERE subject = ? AND private_at IS NOT NULL AND external_at IS NULL`, "watch:"+trainID); private != 1 {
		t.Fatal("a private alert did not make its conversation private, or made it outside content")
	}

	// An agent watch's alert was written by a model from what it read outside the household.
	if err := h.store.SaveWatchAlertMessage(h.ctx, watch.AlertMessage{
		PrincipalID: "alice", MessageID: 9200, ThreadID: watches, WatchID: agentID, WatchLabel: "Library news",
		Text: "The library opens late.", External: true,
	}, 200); err != nil {
		t.Fatal(err)
	}
	h.push(replyInTab(2, watches, "until when?", 9200))
	if message, _ := h.answerTurn(runtime, 2, "Until ten."); !strings.Contains(message, "The library opens late.") {
		t.Fatalf("turn replying to an agent alert = %q", message)
	}
	if outside := h.countRows(`SELECT count(*) FROM conversation_sessions WHERE subject = ? AND external_at IS NOT NULL`, "watch:"+agentID); outside != 1 {
		t.Fatal("an agent watch's alert did not mark its conversation as having read outside content")
	}
}

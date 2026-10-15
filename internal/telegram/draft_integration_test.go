package telegram

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type stopRecorder struct {
	*conversation.Service
	mu      sync.Mutex
	stops   []string
	matches bool
}

func (r *stopRecorder) StopDraft(_ context.Context, lane conversation.Lane, draftID int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops = append(r.stops, lane.PrincipalID+":"+strconv.FormatInt(draftID, 10))
	return r.matches, nil
}

func (r *stopRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.stops)
}

func stopUpdate(id int64, chat Chat, draftID int64) Update {
	return Update{UpdateID: id, StoppedMessageGeneration: &MessageGenerationStopped{Chat: chat, DraftID: draftID}}
}

func TestSendMessageDraftSendsTheDraftParameters(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	h.start()
	receive(t, h.ctx, h.registrations)

	if err := h.client.SendMessageDraft(h.ctx, 101, 7, "", true); err != nil {
		t.Fatal(err)
	}
	if err := h.client.SendMessageDraft(h.ctx, 101, 8, "Searching the web", false); err != nil {
		t.Fatal(err)
	}
	want := []draftRequest{
		{chatID: 101, draftID: 7, canStop: true},
		{chatID: 101, draftID: 8, text: "Searching the web"},
	}
	if got := h.draftRequests(); !slices.Equal(got, want) {
		t.Fatalf("draft requests = %+v, want %+v", got, want)
	}
}

func TestStoppedDraftReachesTheConversationOnlyFromAnAuthorizedPrivateChat(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	h.provision("alice", "account-alice")
	recorder := &stopRecorder{Service: h.conversation, matches: true}
	h.service.conversation = recorder
	h.start()
	receive(t, h.ctx, h.registrations)

	h.push(
		stopUpdate(1, Chat{ID: 101, Type: "supergroup"}, 5),
		stopUpdate(2, Chat{ID: -101, Type: ChatTypePrivate}, 5),
		stopUpdate(3, Chat{ID: 303, Type: ChatTypePrivate}, 5),
		stopUpdate(4, Chat{ID: 101, Type: ChatTypePrivate}, 0),
		textUpdate(5, 101, "/status"),
	)
	h.nextMessage()
	if got := recorder.recorded(); len(got) != 0 {
		t.Fatalf("stops from foreign or non-private chats reached the conversation: %v", got)
	}

	h.push(stopUpdate(6, Chat{ID: 101, Type: ChatTypePrivate}, 42))
	if message := h.nextMessage(); message.chatID != 101 || message.replyTo != 0 || message.text != CancelledReply {
		t.Fatalf("message = %+v, want %q to chat 101", message, CancelledReply)
	}
	if got, want := recorder.recorded(), []string{"alice:42"}; !slices.Equal(got, want) {
		t.Fatalf("stops = %v, want %v", got, want)
	}

	recorder.mu.Lock()
	recorder.matches = false
	recorder.mu.Unlock()
	h.push(stopUpdate(7, Chat{ID: 101, Type: ChatTypePrivate}, 43), textUpdate(8, 101, "/status"))
	if message := h.nextMessage(); message.text == CancelledReply {
		t.Fatalf("stale stop was confirmed: %+v", message)
	}
	if got, want := recorder.recorded(), []string{"alice:42", "alice:43"}; !slices.Equal(got, want) {
		t.Fatalf("stops = %v, want %v", got, want)
	}
}

const (
	indicatorTyping = 40 * time.Millisecond
	indicatorDelay  = 200 * time.Millisecond
	indicatorQuiet  = 400 * time.Millisecond
)

var draftText = regexp.MustCompile(`^(.+) · [0-9]+s$`)

// mailProvider contributes one tool marked with the mail activity.
type mailProvider struct{}

func (mailProvider) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{Definitions: []tools.Definition{{
		Name: "mail_lookup", Description: "Look up mail.", Risk: tools.RiskRead, Activity: tools.ActivityMail,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}}, nil
}

func (mailProvider) Execute(context.Context, string, tools.Call) (tools.Result, error) {
	return tools.Result{Text: "a message"}, nil
}

func newIndicatorHarness(t *testing.T, runtime *controlledRuntime) *conversationHarness {
	t.Helper()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{
		providers:      []tools.Provider{mailProvider{}},
		typingInterval: indicatorTyping,
		draftDelay:     indicatorDelay,
		draftInterval:  300 * time.Millisecond,
	})
	h.provision("alice", "acct-alice")
	h.start()
	return h
}

// waitDraft waits for a draft request after the first from whose label is the given one.
func (h *conversationHarness) waitDraft(from int, label string) int {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		drafts := h.draftRequests()
		for index := from; index < len(drafts); index++ {
			if match := draftText.FindStringSubmatch(drafts[index].text); match != nil && match[1] == label {
				return index
			}
		}
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("no draft %q after request %d: %+v", label, from, drafts)
		}
	}
}

// wantIndicatorsQuiet expects no typing or draft request once the turn has ended.
func (h *conversationHarness) wantIndicatorsQuiet() {
	h.t.Helper()
	time.Sleep(10 * time.Millisecond)
	actions, drafts := len(h.chatActionTimes()), len(h.draftRequests())
	time.Sleep(indicatorQuiet)
	if got := len(h.chatActionTimes()); got != actions {
		h.t.Fatalf("%d typing actions after the turn ended", got-actions)
	}
	if got := h.draftRequests(); len(got) != drafts {
		h.t.Fatalf("drafts after the turn ended: %+v", got[drafts:])
	}
}

// wantSteadyTyping expects typing to have been resent within a few intervals each time from the
// first action on.
func (h *conversationHarness) wantSteadyTyping(minimum int) {
	h.t.Helper()
	actions := h.chatActionTimes()
	if len(actions) < minimum {
		h.t.Fatalf("%d typing actions, want at least %d", len(actions), minimum)
	}
	for index := 1; index < len(actions); index++ {
		if gap := actions[index].Sub(actions[index-1]); gap > 5*indicatorTyping {
			h.t.Fatalf("typing paused for %v", gap)
		}
	}
}

func (h *conversationHarness) waitTurnOutcome(outcome string) {
	h.t.Helper()
	for {
		if got := receive(h.t, h.ctx, h.turns); got.outcome == outcome {
			return
		}
	}
}

func TestLongTurnKeepsTypingAndItsDraftFollowsTheTools(t *testing.T) {
	runtime := newControlledRuntime()
	h := newIndicatorHarness(t, runtime)
	h.push(textUpdate(1, 101, "long request"))
	call := receive(t, h.ctx, runtime.calls)
	if got := call.request.Tools.Activities["mail_lookup"]; got != tools.ActivityMail {
		t.Fatalf("mail_lookup activity = %q", got)
	}

	next := h.waitDraft(0, "Thinking…") + 1
	emit := func(event agentruntime.Event) {
		t.Helper()
		if err := call.sink.Emit(call.ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	emit(agentruntime.Event{Type: "tool_call", Text: "mail_lookup"})
	next = h.waitDraft(next, "Reading your mail…") + 1
	emit(agentruntime.Event{Type: "tool_result", Tool: "mail_lookup", Text: "secret result from the mailbox"})
	next = h.waitDraft(next, "Thinking…") + 1
	emit(agentruntime.Event{Type: "tool_call", Text: "web_search"})
	next = h.waitDraft(next, "Searching the web…") + 1
	emit(agentruntime.Event{Type: "tool_call", Text: "unmarked_tool"})
	h.waitDraft(next, "Thinking…")

	call.answer <- agentruntime.RunResult{Response: "done"}
	h.wantReply(101, 1, "done")
	h.wantIndicatorsQuiet()
	h.wantSteadyTyping(int(indicatorDelay / indicatorTyping))
	drafts := h.draftRequests()
	for _, draft := range drafts {
		if draft.chatID != 101 || draft.draftID != drafts[0].draftID || draft.draftID == 0 || !draft.canStop || strings.Contains(draft.text, "secret") {
			t.Fatalf("draft %+v, want chat 101, one stoppable draft, and no tool content", draft)
		}
	}
}

func TestQuickTurnTypesWithoutADraft(t *testing.T) {
	runtime := newControlledRuntime()
	h := newIndicatorHarness(t, runtime)
	h.push(textUpdate(1, 101, "quick request"))
	call := receive(t, h.ctx, runtime.calls)
	for len(h.chatActionTimes()) == 0 {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-h.ctx.Done():
			t.Fatal("a turn sent no typing action")
		}
	}
	call.answer <- agentruntime.RunResult{Response: "quick answer"}
	h.wantReply(101, 1, "quick answer")
	h.wantIndicatorsQuiet()
	if drafts := h.draftRequests(); len(drafts) != 0 {
		t.Fatalf("a quick turn showed drafts: %+v", drafts)
	}
}

func TestTurnIndicatorsStopWithTheTurn(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		runtime := newControlledRuntime()
		h := newIndicatorHarness(t, runtime)
		h.push(textUpdate(1, 101, "failing request"))
		call := receive(t, h.ctx, runtime.calls)
		h.waitDraft(0, "Thinking…")
		call.answer <- agentruntime.RunResult{}
		h.wantReply(101, 1, conversation.UnavailableReply)
		h.wantIndicatorsQuiet()
	})
	t.Run("cancel", func(t *testing.T) {
		runtime := newControlledRuntime()
		h := newIndicatorHarness(t, runtime)
		h.push(textUpdate(1, 101, "cancelled request"))
		call := receive(t, h.ctx, runtime.calls)
		h.waitDraft(0, "Thinking…")
		h.push(textUpdate(2, 101, "/cancel"))
		h.wantReply(101, 2, CancelledReply)
		receive(t, h.ctx, call.ctx.Done())
		h.waitTurnOutcome("cancelled")
		h.wantIndicatorsQuiet()
	})
}

func TestDraftStopCancelsOnlyTheTurnItShows(t *testing.T) {
	runtime := newControlledRuntime()
	h := newIndicatorHarness(t, runtime)
	private := Chat{ID: 101, Type: ChatTypePrivate}
	h.push(textUpdate(1, 101, "long request"))
	call := receive(t, h.ctx, runtime.calls)
	shown := h.waitDraft(0, "Thinking…")
	draft := h.draftRequests()[shown].draftID

	h.push(stopUpdate(2, private, draft+1), textUpdate(3, 101, "/status"))
	h.wantReply(101, 3, ConversationStatusReply)
	if call.ctx.Err() != nil {
		t.Fatal("a stale draft stopped the turn")
	}

	h.push(stopUpdate(4, private, draft))
	if message := h.nextMessage(); message.chatID != 101 || message.replyTo != 0 || message.text != CancelledReply {
		t.Fatalf("message = %+v, want %q", message, CancelledReply)
	}
	receive(t, h.ctx, call.ctx.Done())
	h.waitState(1, "cancelled")
	h.waitTurnOutcome("cancelled")
	h.wantIndicatorsQuiet()

	h.push(stopUpdate(5, private, draft), textUpdate(6, 101, "/status"))
	h.wantReply(101, 6, ConversationStatusReply)
	h.push(textUpdate(7, 101, "next request"))
	next := receive(t, h.ctx, runtime.calls)
	next.answer <- agentruntime.RunResult{Response: "next answer"}
	h.wantReply(101, 7, "next answer")
	h.waitState(7, "delivered")
}

func TestTurnWaitingBehindAnotherKeepsTyping(t *testing.T) {
	runtime := newControlledRuntime()
	h := newIndicatorHarness(t, runtime)
	scheduled := make(chan error, 1)
	go func() {
		_, err := h.conversation.RunScheduled(h.ctx, "alice", "schedule/aaaa", "scheduled check")
		scheduled <- err
	}()
	first := receive(t, h.ctx, runtime.calls)
	if len(h.chatActionTimes()) != 0 {
		t.Fatal("a scheduled run sent typing")
	}

	h.push(textUpdate(1, 101, "chat request"))
	time.Sleep(2 * indicatorDelay)
	h.wantSteadyTyping(int(indicatorDelay / indicatorTyping))
	if drafts := h.draftRequests(); len(drafts) != 0 {
		t.Fatalf("a waiting turn showed drafts: %+v", drafts)
	}
	first.answer <- agentruntime.RunResult{Response: "scheduled answer", Final: "scheduled answer"}
	if err := receive(t, h.ctx, scheduled); err != nil {
		t.Fatal(err)
	}
	chat := receive(t, h.ctx, runtime.calls)
	chat.answer <- agentruntime.RunResult{Response: "chat answer"}
	h.wantReply(101, 1, "chat answer")
	h.wantIndicatorsQuiet()
	h.wantSteadyTyping(int(2 * indicatorDelay / indicatorTyping))
}

func TestFailingTurnIndicatorsAreCountedAndLeaveTheReplyAlone(t *testing.T) {
	runtime := newControlledRuntime()
	h := newIndicatorHarness(t, runtime)
	h.mu.Lock()
	h.actionStatus, h.draftStatus = http.StatusTooManyRequests, http.StatusTooManyRequests
	h.mu.Unlock()
	started := time.Now()
	h.push(textUpdate(1, 101, "long request"))
	call := receive(t, h.ctx, runtime.calls)
	h.waitDraft(0, "Thinking…")
	call.answer <- agentruntime.RunResult{Response: "done"}
	h.wantReply(101, 1, "done")
	elapsed := time.Since(started)
	h.wantIndicatorsQuiet()

	actions := len(h.chatActionTimes())
	if limit := int(elapsed/indicatorTyping) + 2; actions > limit {
		t.Fatalf("%d typing actions in %v, a failed one was retried", actions, elapsed)
	}
	h.mu.Lock()
	failures := maps.Clone(h.indicatorFailures)
	h.mu.Unlock()
	// A call in flight when the turn ends is cancelled after the server saw it, and is no failure.
	drafts := len(h.draftRequests())
	if !within(failures["typing"], actions-1, actions) || !within(failures["draft"], drafts-1, drafts) || failures["draft"] == 0 {
		t.Fatalf("failures = %v, want every one of %d typing actions and %d drafts", failures, actions, drafts)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "conversation.indicator.failed") || !strings.Contains(logs, "method=draft") || strings.Contains(logs, "Thinking") {
		t.Fatalf("indicator failures logged without the method or with the draft's text: %s", logs)
	}
}

func within(value, low, high int) bool {
	return value >= low && value <= high
}

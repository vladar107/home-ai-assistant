package telegram

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

const heldQuery = "see https://example.com/page?id=7"

func newSearchHarness(t *testing.T, runtime agentruntime.AgentRuntime, options harnessOptions) *conversationHarness {
	t.Helper()
	options.web, options.search = true, true
	options.providers = append(options.providers, outsideProvider{})
	h := newConversationHarnessWithOptions(t, runtime, options)
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()
	return h
}

// searchTurn runs one chat turn in which the model searches for each query in order, the way the
// runtime's search extension asks the check first, and returns the decisions. A search that is
// allowed reports its results, which is what exposes the conversation in the real runtime.
func (h *conversationHarness) searchTurn(runtime *controlledRuntime, updateID int64, readOutside bool, queries ...string) []agentruntime.SearchDecision {
	h.t.Helper()
	h.push(textUpdate(updateID, 101, "what is going on"))
	call := receive(h.t, h.ctx, runtime.calls)
	if !call.request.WebSearch || call.request.SearchCheck == nil {
		h.t.Fatalf("turn has no search check: %+v", call.request)
	}
	if readOutside {
		if _, err := call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: "read_outside", Arguments: json.RawMessage(`{}`)}); err != nil {
			h.t.Fatal(err)
		}
	}
	decisions := make([]agentruntime.SearchDecision, 0, len(queries))
	for _, query := range queries {
		decision := call.request.SearchCheck(call.ctx, query)
		decisions = append(decisions, decision)
		if decision.Verdict != agentruntime.SearchAllow {
			continue
		}
		for _, event := range []agentruntime.Event{{Type: "tool_call", Text: "web_search"}, {Type: "tool_result", Tool: "web_search", Text: "found it"}} {
			if err := call.sink.Emit(call.ctx, event); err != nil {
				h.t.Fatal(err)
			}
		}
	}
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, updateID, "ok")
	h.waitState(updateID, "delivered")
	return decisions
}

func wantDecision(t *testing.T, label string, got agentruntime.SearchDecision, verdict agentruntime.SearchVerdict, reason string) {
	t.Helper()
	if got.Verdict != verdict || got.Reason != reason {
		t.Errorf("%s: decision = %+v, want %s (%s)", label, got, verdict, reason)
	}
}

// searchJudgeStub answers the search question with the given probability, or fails with status.
type searchJudgeStub struct {
	mu     sync.Mutex
	states []map[string]string
	noul   float64
	status int
	server *httptest.Server
}

func newSearchJudgeStub(t *testing.T, noul float64, status int) *searchJudgeStub {
	t.Helper()
	stub := &searchJudgeStub{noul: noul, status: status}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State map[string]string `json:"state"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		stub.mu.Lock()
		stub.states = append(stub.states, body.State)
		stub.mu.Unlock()
		if stub.status != http.StatusOK {
			w.WriteHeader(stub.status)
			return
		}
		fmt.Fprintf(w, `{"model":"jev-1.13.0","answers":{"serves":{"type":"noul","noul":%v}}}`, stub.noul)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *searchJudgeStub) requests() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string(nil), s.states...)
}

func (h *conversationHarness) useSearchJudge(stub *searchJudgeStub, optedIn bool, prepare func(*turnstate.Registry)) {
	h.t.Helper()
	client, err := judgment.NewClient(judgment.Config{APIKey: "test-key", Endpoint: stub.server.URL, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		h.t.Fatal(err)
	}
	registry := turnstate.New(time.Now)
	registry.RecordMessage("alice", 0, "what happened in the news today")
	if prepare != nil {
		prepare(registry)
	}
	h.searches.SetJudge(judgment.NewSearchJudge(client, stubOptIn(optedIn), registry, registry))
}

func TestSearchRunsFreelyAndIsCheckedAfterOutsideContent(t *testing.T) {
	runtime := newControlledRuntime()
	h := newSearchHarness(t, runtime, harnessOptions{})

	// Nothing outside has been read, so even a query naming a site runs.
	decisions := h.searchTurn(runtime, 1, false, "site:example.com annual report")
	wantDecision(t, "first search", decisions[0], agentruntime.SearchAllow, "clean")

	// Its results are outside content now: an ordinary question still runs, queries that could
	// carry data out wait for the user.
	held := map[string]string{
		"url":      "https://example.com/page?id=7",
		"host":     "example.com",
		"operator": "site:example.com news",
		"email":    "contact jan@example.org",
		"token":    "Zm9vYmFyQmF6MTIzNDU2Nzg5MGFiY2RlZkdISUo",
	}
	queries := []string{"what happened in the news today"}
	for _, query := range held {
		queries = append(queries, query)
	}
	decisions = h.searchTurn(runtime, 2, true, queries...)
	wantDecision(t, "plain query", decisions[0], agentruntime.SearchAllow, "checked")
	for index, decision := range decisions[1:] {
		wantDecision(t, queries[index+1], decision, agentruntime.SearchConfirm, "pattern")
		if !strings.Contains(decision.Text, "search again with exactly the same query") || !strings.Contains(decision.Text, "do not claim") && !strings.Contains(decision.Text, "Do not claim") {
			t.Errorf("confirm text for %q = %q", queries[index+1], decision.Text)
		}
	}
	proposal := h.nextMessage()
	if proposal.chatID != 101 || !strings.Contains(proposal.text, "5 changes, confirmed or rejected together") || !strings.Contains(proposal.text, "Search the web for: ") || !strings.Contains(proposal.text, "https://example.com/page?id=7") {
		t.Fatalf("proposal = %+v", proposal)
	}
	confirm, _ := approvalButtons(t, proposal.keyboard)
	if status, _ := h.approvalStatus(approvalID(confirm)); status != approval.StatusPending {
		t.Fatalf("search proposal status = %s", status)
	}
}

func TestOneUsersOutsideContentDoesNotHoldAnothersSearch(t *testing.T) {
	runtime := newControlledRuntime()
	h := newSearchHarness(t, runtime, harnessOptions{})

	h.searchTurn(runtime, 1, true, "plain question")
	h.push(textUpdate(2, 202, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.PrincipalID != "bob" {
		t.Fatalf("principal = %s", call.request.PrincipalID)
	}
	wantDecision(t, "bob's search", call.request.SearchCheck(call.ctx, heldQuery), agentruntime.SearchAllow, "clean")
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(202, 2, "ok")
}

func TestSuspiciousConversationHoldsEverySearch(t *testing.T) {
	runtime := newControlledRuntime()
	h := newSearchHarness(t, runtime, harnessOptions{})
	stub := newSearchJudgeStub(t, 0.9, http.StatusOK)
	h.useSearchJudge(stub, true, func(registry *turnstate.Registry) { registry.MarkSuspicious("alice") })

	decisions := h.searchTurn(runtime, 1, true, "what happened in the news today")
	wantDecision(t, "plain query", decisions[0], agentruntime.SearchConfirm, "suspicious")
	if !strings.Contains(decisions[0].Text, "tried to instruct the assistant") {
		t.Errorf("text = %q", decisions[0].Text)
	}
	if proposal := h.nextMessage(); !strings.Contains(proposal.text, "Search the web for: ") || !strings.Contains(proposal.text, "tried to instruct the assistant") {
		t.Fatalf("proposal = %+v", proposal)
	}
	if len(stub.requests()) != 0 {
		t.Errorf("TypeSafe was asked about a search that was held already: %v", stub.requests())
	}
}

func TestSuspicionIsIgnoredWithoutTheSafetyChecksOptIn(t *testing.T) {
	runtime := newControlledRuntime()
	h := newSearchHarness(t, runtime, harnessOptions{})
	h.useSearchJudge(newSearchJudgeStub(t, 0.9, http.StatusOK), false, func(registry *turnstate.Registry) { registry.MarkSuspicious("alice") })

	decisions := h.searchTurn(runtime, 1, true, "what happened in the news today")
	wantDecision(t, "plain query", decisions[0], agentruntime.SearchAllow, "checked")
}

func TestJudgeHoldsASearchThatIsNotPartOfTheRequest(t *testing.T) {
	runtime := newControlledRuntime()
	h := newSearchHarness(t, runtime, harnessOptions{})
	stub := newSearchJudgeStub(t, 0.1, http.StatusOK)
	h.useSearchJudge(stub, true, nil)

	decisions := h.searchTurn(runtime, 1, true, "cheap flights to anywhere", "contact jan@example.org")
	wantDecision(t, "off-request query", decisions[0], agentruntime.SearchConfirm, "off_request")
	wantDecision(t, "query with a pattern", decisions[1], agentruntime.SearchConfirm, "pattern")
	if proposal := h.nextMessage(); !strings.Contains(proposal.text, "does not look like part of your request") {
		t.Fatalf("proposal = %+v", proposal)
	}
	requests := stub.requests()
	if len(requests) != 1 || requests[0]["search_query"] != "cheap flights to anywhere" || requests[0]["recent_messages"] != "what happened in the news today" {
		t.Fatalf("TypeSafe requests = %v", requests)
	}
}

func TestJudgeFailureFallsBackToTheDeterministicCheck(t *testing.T) {
	runtime := newControlledRuntime()
	h := newSearchHarness(t, runtime, harnessOptions{})
	stub := newSearchJudgeStub(t, 0, http.StatusInternalServerError)
	h.useSearchJudge(stub, true, nil)

	decisions := h.searchTurn(runtime, 1, true, "cheap flights to anywhere", "contact jan@example.org")
	wantDecision(t, "query when TypeSafe fails", decisions[0], agentruntime.SearchAllow, "checked")
	wantDecision(t, "query with a pattern", decisions[1], agentruntime.SearchConfirm, "pattern")
	requests := stub.requests()
	if len(requests) == 0 {
		t.Fatal("TypeSafe was never asked")
	}
	for _, request := range requests {
		if request["search_query"] != "cheap flights to anywhere" {
			t.Errorf("TypeSafe was asked about %v, want only the query without a pattern", request)
		}
	}
	h.nextMessage()
}

func TestConfirmedSearchRunsOnceWithTheSameQuery(t *testing.T) {
	runtime := newControlledRuntime()
	h := newSearchHarness(t, runtime, harnessOptions{})

	decisions := h.searchTurn(runtime, 1, true, heldQuery)
	wantDecision(t, "held query", decisions[0], agentruntime.SearchConfirm, "pattern")
	proposal := h.nextMessage()
	confirm, _ := approvalButtons(t, proposal.keyboard)

	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	if edit := h.nextEdit(); edit.messageID != proposal.messageID || !strings.Contains(edit.text, "Done. Approved; the search runs now.") {
		t.Fatalf("proposal after confirm = %+v", edit)
	}

	call := receive(t, h.ctx, runtime.calls)
	if !strings.HasSuffix(call.request.Message, conversation.ContinueMessage) || !strings.Contains(call.request.Message, "web_search") {
		t.Fatalf("continuation message = %q", call.request.Message)
	}
	wantDecision(t, "confirmed query, other case and spacing", call.request.SearchCheck(call.ctx, "  See   https://EXAMPLE.com/page?id=7 "), agentruntime.SearchAllow, "confirmed")
	wantDecision(t, "confirmed query again", h.searches.Check(call.ctx, "alice", heldQuery, false), agentruntime.SearchDeny, "pattern")
	call.answer <- agentruntime.RunResult{Response: conversation.NothingToAdd}
	h.waitState(2, "delivered")

	// A different query was never confirmed, and another user's confirmation is not this user's.
	h.searchTurn(runtime, 3, false, "plain question")
	h.push(textUpdate(4, 101, "again"))
	again := receive(t, h.ctx, runtime.calls)
	wantDecision(t, "other query", h.searches.Check(again.ctx, "alice", "see https://example.com/other", false), agentruntime.SearchDeny, "pattern")
	again.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 4, "ok")
}

func TestRejectedSearchStaysHeld(t *testing.T) {
	runtime := newControlledRuntime()
	h := newSearchHarness(t, runtime, harnessOptions{})

	h.searchTurn(runtime, 1, true, heldQuery)
	proposal := h.nextMessage()
	_, reject := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, reject))
	h.wantAnswer(2)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Rejected") {
		t.Fatalf("proposal after reject = %+v", edit)
	}
	decisions := h.searchTurn(runtime, 3, false, heldQuery)
	wantDecision(t, "query after reject", decisions[0], agentruntime.SearchConfirm, "pattern")
	h.nextMessage()
}

func TestScheduledRunDeniesWhatAChatTurnWouldHold(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, web: true, search: true, providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()
	job := h.createJob("alice", "00000000000000ee", "News", "0 9 * * *", "Summarise the news.")

	clock.Set(t, "2026-09-28T09:00:05+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if !run.request.WebSearch || run.request.SearchCheck == nil {
		t.Fatalf("scheduled run has no search check: %+v", run.request)
	}
	wantDecision(t, "before outside content", run.request.SearchCheck(run.ctx, heldQuery), agentruntime.SearchAllow, "clean")
	if _, err := run.request.Tools.Executor.Execute(run.ctx, agentruntime.ToolCall{Name: "read_outside", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	wantDecision(t, "plain query", run.request.SearchCheck(run.ctx, "weather outlook"), agentruntime.SearchAllow, "checked")
	denied := run.request.SearchCheck(run.ctx, heldQuery)
	wantDecision(t, "held query", denied, agentruntime.SearchDeny, "pattern")
	if !strings.Contains(denied.Text, "nobody is available to confirm") {
		t.Errorf("text = %q", denied.Text)
	}
	run.answer <- agentruntime.RunResult{Response: "Done.", Final: "Done."}
	h.wantMessage(101, "<b>Scheduled: News</b>\n\nDone.")
	h.waitRun(job.ID, schedule.StatusDelivered)
	if got := h.countRows(`SELECT count(*) FROM approvals`); got != 0 {
		t.Errorf("a scheduled run left %d proposals", got)
	}
}

func TestSearchCheckThroughTheRealRuntimeAndBridge(t *testing.T) {
	profiles := filepath.Join(t.TempDir(), "profiles")
	runtime, _ := newWebFixtureRuntime(t, profiles)
	h := newSearchHarness(t, runtime, harnessOptions{profilesDir: profiles})

	h.push(textUpdate(1, 101, "fixture:search latest news"))
	h.wantReply(101, 1, "Searching.\n\nsaw: found: synthetic search answer")
	h.waitState(1, "delivered")

	// The results just read are outside content: the address is held, with the model told so.
	h.push(textUpdate(2, 101, "fixture:search "+heldQuery))
	reply := h.nextMessage()
	if !strings.HasPrefix(reply.text, "Searching.\n\nsaw: The search was not run: it contains a web address") {
		t.Fatalf("reply = %q", reply.text)
	}
	h.waitState(2, "delivered")
	proposal := h.nextMessage()
	if !strings.Contains(proposal.text, "Search the web for: ") || !strings.Contains(proposal.text, heldQuery) {
		t.Fatalf("proposal = %+v", proposal)
	}
	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(3, 101, proposal.messageID, confirm))
	h.wantAnswer(3)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Done. Approved; the search runs now.") {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	h.nextMessage()
	h.waitState(3, "delivered")

	h.push(textUpdate(4, 101, "fixture:search "+heldQuery))
	h.wantReply(101, 4, "Searching.\n\nsaw: found: synthetic search answer")
	h.waitState(4, "delivered")
	h.push(textUpdate(5, 101, "fixture:search "+heldQuery))
	if reply := h.nextMessage(); !strings.Contains(reply.text, "The search was not run") {
		t.Fatalf("second search with the same query: %q", reply.text)
	}
	h.waitState(5, "delivered")
	h.nextMessage()

	// A fresh conversation has read nothing, so the same query runs again.
	h.push(textUpdate(6, 101, "/new"))
	h.wantReply(101, 6, NewSessionReply)
	h.push(textUpdate(7, 101, "fixture:search "+heldQuery))
	h.wantReply(101, 7, "Searching.\n\nsaw: found: synthetic search answer")
}

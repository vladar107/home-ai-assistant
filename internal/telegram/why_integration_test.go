package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/events"
	"github.com/vladar107/home-ai-assistant/internal/provenance"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/turn"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// sourceProvider grants one read tool whose label is the source it reads; a source starting with
// "broken" fails.
type sourceProvider struct{}

func (sourceProvider) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{Definitions: []tools.Definition{{
		Name: "source_read", Description: "Read one source.", Risk: tools.RiskRead, ThirdParty: true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"source":{"type":"string"}}}`),
		SourceLabel: tools.ArgumentLabel("source"),
	}}}, nil
}

func (sourceProvider) Execute(_ context.Context, _ string, call tools.Call) (tools.Result, error) {
	var arguments struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		return tools.Result{}, err
	}
	if strings.HasPrefix(arguments.Source, "broken") {
		return tools.Result{Text: "that source is unreachable", IsError: true}, nil
	}
	return tools.Result{Text: "read " + arguments.Source}, nil
}

// piCall is one tool call the fake runtime makes as Pi reports it: a start event with the call's id
// and arguments, the call through the bridge unless it is the runtime's own web search, and an end
// event with the error flag. failed fails a web search, which never reaches the broker.
type piCall struct {
	id, name, arguments string
	failed              bool
}

func (h *conversationHarness) startCall(call runtimeCall, tool piCall) {
	h.t.Helper()
	if err := call.sink.Emit(call.ctx, agentruntime.Event{Type: "tool_call", Text: tool.name, Tool: tool.name, CallID: tool.id, Arguments: json.RawMessage(tool.arguments)}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *conversationHarness) finishCall(call runtimeCall, tool piCall) {
	h.t.Helper()
	result := agentruntime.ToolResult{Text: "search results for " + tool.arguments, IsError: tool.failed}
	if tool.name != turn.WebSearchTool {
		var err error
		result, err = call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{ID: tool.id, Name: tool.name, Arguments: json.RawMessage(tool.arguments)})
		if err != nil {
			h.t.Fatal(err)
		}
	}
	if err := call.sink.Emit(call.ctx, agentruntime.Event{Type: "tool_result", Tool: tool.name, Text: result.Text, CallID: tool.id, IsError: result.IsError}); err != nil {
		h.t.Fatal(err)
	}
}

// runCalls starts and finishes each call in turn.
func (h *conversationHarness) runCalls(call runtimeCall, calls ...piCall) {
	h.t.Helper()
	for _, tool := range calls {
		h.startCall(call, tool)
		h.finishCall(call, tool)
	}
}

// replyUpdate is the user's message replying to one of the bot's messages.
func replyUpdate(id, userID int64, text string, replied sentMessage) Update {
	update := textUpdate(id, userID, text)
	update.Message.ReplyToMessage = &Message{MessageID: replied.messageID, Chat: Chat{ID: userID, Type: ChatTypePrivate}, Text: replied.text}
	return update
}

// waitTurnMessage waits until the delivered message is linked to its turn's trace.
func (h *conversationHarness) waitTurnMessage(messageID int64) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for h.countRows(`SELECT count(*) FROM turn_messages WHERE telegram_message_id = ?`, messageID) == 0 {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("message %d was never linked to a trace", messageID)
		}
	}
}

func (h *conversationHarness) waitRecentReply(recent *turnstate.Registry, principalID string) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !slices.ContainsFunc(recent.RecentExchange(principalID, 0), func(line turnstate.Line) bool { return line.Role == turnstate.RoleAssistant }) {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("the reply never joined %s's recent exchange", principalID)
		}
	}
}

// explain asks why about the message, answers the explanation turn, and returns that turn's
// request and the reply.
func (h *conversationHarness) explain(runtime *controlledRuntime, updateID int64, question string, replied sentMessage, explanation string) (agentruntime.RunRequest, sentMessage) {
	h.t.Helper()
	h.push(replyUpdate(updateID, 101, question, replied))
	turn := receive(h.t, h.ctx, runtime.calls)
	if !turn.request.Tools.Empty() || turn.request.WebSearch || turn.request.PrincipalID != "alice" {
		h.t.Fatalf("explanation turn = %+v", turn.request)
	}
	turn.answer <- agentruntime.RunResult{Response: explanation, Final: explanation}
	answer := h.nextMessage()
	if answer.chatID != 101 || answer.replyTo != requestMessageID(updateID) {
		h.t.Fatalf("answer = %+v, want a reply to update %d", answer, updateID)
	}
	return turn.request, answer
}

func wantContains(t *testing.T, what, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Errorf("%s does not contain %q:\n%s", what, part, text)
		}
	}
}

// A chat turn's calls are kept in the order the runtime started them, each matched to its own
// end and to the broker's label by its call id, even when parallel calls end in another order;
// the runtime's own web search is kept with its query, and a failed call as failed. /why and a
// short "why?" in reply explain the turn from that record, the user's own message is quoted to the
// explanation, and nothing of the record reaches the logs. Another user, a message without a
// record, and /why without a reply get no record.
func TestWhyExplainsAChatTurnFromItsTrace(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	recent := turnstate.New(nil)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{providers: []tools.Provider{sourceProvider{}}, recent: recent})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	h.push(textUpdate(1, 101, "do the trains run today?"))
	chat := receive(t, h.ctx, runtime.calls)
	search := piCall{id: "call-search", name: turn.WebSearchTool, arguments: `{"query":"rail strike zeta"}`}
	broken := piCall{id: "call-broken", name: "source_read", arguments: `{"source":"broken-ledger-omega"}`}
	timetable := piCall{id: "call-timetable", name: "source_read", arguments: `{"source":"timetable-sigma"}`}
	retry := piCall{id: "call-retry", name: turn.WebSearchTool, arguments: `{"query":"rail strike kappa"}`, failed: true}
	h.runCalls(chat, search)
	// Two calls in parallel: the second ends first.
	h.startCall(chat, broken)
	h.startCall(chat, timetable)
	h.finishCall(chat, timetable)
	h.finishCall(chat, broken)
	h.runCalls(chat, retry)
	chat.answer <- agentruntime.RunResult{Response: "The trains run normally today.", Model: "gpt-test"}
	reply := h.nextMessage()
	h.waitState(1, "delivered")
	h.waitTurnMessage(reply.messageID)
	// The reply joins the recent exchange only after the turn's follow-up, which runs after delivery.
	h.waitRecentReply(recent, "alice")

	trace, found, err := h.store.TurnTraceByMessage(h.ctx, "alice", 101, reply.messageID)
	if err != nil || !found {
		t.Fatalf("trace = %v %v", found, err)
	}
	type kept struct{ id, tool, outcome, label string }
	want := []kept{
		{"call-search", "web_search", "success", "rail strike zeta"},
		{"call-broken", "source_read", "tool_error", "broken-ledger-omega"},
		{"call-timetable", "source_read", "success", "timetable-sigma"},
		{"call-retry", "web_search", "failed", "rail strike kappa"},
	}
	if trace.Kind != turn.TraceChat || trace.Model != "gpt-test" || !trace.External || len(trace.Calls) != len(want) {
		t.Fatalf("trace = %+v", trace)
	}
	for index, call := range trace.Calls {
		if got := (kept{call.CallID, call.Tool, call.Outcome, call.Label}); got != want[index] || call.Sequence != index+1 {
			t.Errorf("call %d = %+v, want %+v", index+1, call, want[index])
		}
	}

	request, answer := h.explain(runtime, 2, "/why", reply, "You asked whether the trains run, and the search found no strike.")
	wantContains(t, "explanation turn", request.Message, "do the trains run today?", "The trains run normally today.", "rail strike zeta", "timetable-sigma")
	wantContains(t, "answer", answer.text, "What that message was based on", "A chat turn", "gpt-test",
		"1. web_search — “rail strike zeta”: succeeded", "2. source_read — “broken-ledger-omega”: failed",
		"3. source_read — “timetable-sigma”: succeeded", "4. web_search — “rail strike kappa”: failed",
		"It read content from outside the household", "the search found no strike", provenance.CaveatLine)

	// A short question about the message's sources is recognised the same way, and a second one
	// while the answer is still being written is turned away.
	h.push(replyUpdate(3, 101, "How do you know?", reply))
	pending := receive(t, h.ctx, runtime.calls)
	h.push(replyUpdate(7, 101, "/why", reply))
	h.wantReply(101, 7, provenance.BusyReply)
	pending.answer <- agentruntime.RunResult{Response: "Because the search found nothing.", Final: "Because the search found nothing."}
	again := h.nextMessage()
	if again.replyTo != requestMessageID(3) {
		t.Fatalf("answer to how do you know = %+v", again)
	}
	wantContains(t, "answer to how do you know?", again.text, "timetable-sigma", "Because the search found nothing.")

	// A bare "why?" is about the content as often as about its sources, so the conversation takes it.
	h.push(replyUpdate(8, 101, "why?", reply))
	followUp := receive(t, h.ctx, runtime.calls)
	if followUp.request.Tools.Empty() || !strings.Contains(followUp.request.Message, "why?") {
		t.Fatalf("a bare why? did not reach the conversation: %+v", followUp.request)
	}
	followUp.answer <- agentruntime.RunResult{Response: "Because no strike is announced."}
	h.wantReply(101, 8, "Because no strike is announced.")
	h.waitState(8, "delivered")

	// Bob replying to alice's message, a message without a record, and /why without a reply.
	update := textUpdate(9, 202, "/why")
	update.Message.ReplyToMessage = &Message{MessageID: reply.messageID, Chat: Chat{ID: 202, Type: ChatTypePrivate}, Text: reply.text}
	h.push(update)
	if refused := h.nextMessage(); refused.chatID != 202 || refused.text != provenance.NoRecordReply {
		t.Fatalf("bob's /why = %+v", refused)
	}
	h.push(replyUpdate(10, 101, "/why", sentMessage{messageID: 4242, text: "an old reply"}))
	h.wantReply(101, 10, provenance.NoRecordReply)
	h.push(textUpdate(11, 101, "/why"))
	h.wantReply(101, 11, provenance.UsageReply)
	if len(runtime.calls) != 0 {
		t.Fatal("a refused /why ran a model turn")
	}

	if logs := h.logs.String(); strings.Contains(logs, "zeta") || strings.Contains(logs, "omega") || strings.Contains(logs, "sigma") || strings.Contains(logs, "kappa") {
		t.Fatalf("a label reached the logs: %s", logs)
	}
}

// A scheduled report is explained from its run's trace.
func TestWhyExplainsAScheduledReport(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: newTestClock(t, mondayMorning), providers: []tools.Provider{sourceProvider{}}})
	h.provision("alice", "account-alice")
	h.start()
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Morning digest", "0 9 * * *", "Summarise the timetable.")
	if queued, err := h.scheduler.RunNow(h.ctx, "alice", job.ID, ""); err != nil || !queued {
		t.Fatalf("run now = %v %v", queued, err)
	}
	run := receive(t, h.ctx, runtime.calls)
	h.runCalls(run, piCall{id: "call-1", name: "source_read", arguments: `{"source":"timetable-delta"}`})
	run.answer <- agentruntime.RunResult{Response: "All trains on time.", Final: "All trains on time."}
	h.waitRun(job.ID, schedule.StatusDelivered)
	report := h.nextMessage()

	_, answer := h.explain(runtime, 1, "/why", report, "The digest read the timetable.")
	wantContains(t, "answer", answer.text, "A scheduled run", "1. source_read — “timetable-delta”: succeeded", "The digest read the timetable.")
}

// A background task's report is explained from the trace of the start that wrote it.
func TestWhyExplainsABackgroundTaskReport(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: testTasks(), providers: []tools.Provider{sourceProvider{}}})
	h.provision("alice", "account-alice")
	h.start()

	chat, status, _ := h.startTask(runtime, 1, "Timetables", "Read the timetables.")
	run := receive(t, h.ctx, runtime.calls)
	h.wantStatusEdit(status, "Working on: Timetables", true)
	h.runCalls(run, piCall{id: "call-1", name: "source_read", arguments: `{"source":"timetable-epsilon"}`})
	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")
	run.answer <- agentruntime.RunResult{Response: "The timetables agree.", Final: "The timetables agree."}
	report := h.nextMessage()
	h.wantStatusEdit(status, "Done: Timetables", false)
	h.waitTurnMessage(report.messageID)

	_, answer := h.explain(runtime, 2, "/why", report, "The task read one timetable.")
	wantContains(t, "answer", answer.text, "A background task", "1. source_read — “timetable-epsilon”: succeeded", "The task read one timetable.")
}

// An agent watch's alert is explained from its check turn, and a code-checked alert from the
// watch's rules with no model turn at all.
func TestWhyExplainsWatchAlerts(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	evaluator := &stubEvaluator{}
	evaluator.set(watch.Observation{State: "cancelled", Triggered: true, Headline: "It is cancelled."})
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: newTestClock(t, mondayMorning), watches: evaluator})
	h.provision("alice", "account-alice")
	h.start()

	const agentID, trainID = "00000000000000c1", "00000000000000a1"
	h.createWatch("alice", agentID, agentWatch())
	if err := h.watches.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.watches.Shutdown(context.Background()) })
	check := receive(t, h.ctx, runtime.calls)
	h.runCalls(check, piCall{id: "call-1", name: turn.WebSearchTool, arguments: `{"query":"example release notes eta"}`})
	check.answer <- agentruntime.RunResult{Final: `{"met": true, "value": "1 December", "headline": "The next release is on 1 December."}`}
	alert := h.nextMessage()
	h.waitTurnMessage(alert.messageID)

	_, answer := h.explain(runtime, 1, "/why", alert, "The check searched the release notes.")
	wantContains(t, "agent alert answer", answer.text, "A check", "1. web_search — “example release notes eta”: succeeded", "The check searched the release notes.")

	h.createWatch("alice", trainID, trainWatch())
	h.watches.Cycle(h.ctx)
	coded := h.nextMessage()
	if !strings.Contains(coded.text, "It is cancelled.") {
		t.Fatalf("train alert = %+v", coded)
	}
	h.waitTurnMessage(coded.messageID)
	h.push(replyUpdate(2, 101, "how do you know?", coded))
	explained := h.nextMessage()
	wantContains(t, "code alert answer", explained.text, "Code wrote it from the watch's rules, with no model turn",
		"08:30 train Utrecht Centraal → Amsterdam Centraal", "Tells you about:")
	if explained.replyTo != requestMessageID(2) || len(runtime.calls) != 0 {
		t.Fatalf("code alert answer = %+v, model turns waiting = %d", explained, len(runtime.calls))
	}
}

// A runtime without call ids still has its calls kept, matched to their ends by the name the start
// gave, even a model-invented name that is kept as "invalid".
func TestWhyKeepsCallsOfARuntimeWithoutIDs(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()
	h.push(textUpdate(1, 101, "search"))
	chat := receive(t, h.ctx, runtime.calls)
	for _, event := range []agentruntime.Event{
		{Type: "tool_call", Text: "Made Up!"}, {Type: "tool_result", Tool: "Made Up!", Text: "no such tool", IsError: true},
		{Type: "tool_call", Text: "web_search"}, {Type: "tool_result", Tool: "web_search", Text: "found"},
	} {
		if err := chat.sink.Emit(chat.ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	chat.answer <- agentruntime.RunResult{Response: "found it"}
	reply := h.nextMessage()
	h.waitState(1, "delivered")
	trace, found, err := h.store.TurnTraceByMessage(h.ctx, "alice", 101, reply.messageID)
	if err != nil || !found || len(trace.Calls) != 2 || trace.Calls[0].Tool != "invalid" || trace.Calls[0].Outcome != "failed" ||
		trace.Calls[1].Outcome != "success" || trace.Calls[1].Label != "" {
		t.Fatalf("trace = %+v %v %v", trace, found, err)
	}
	if count := h.countRows(`SELECT count(*) FROM turn_trace_calls WHERE label != ''`); count != 0 {
		t.Fatalf("labels without arguments = %d", count)
	}
}

// A turn of a hundred calls is answered in full: the record lists the first calls and counts the
// rest, and an answer longer than one message is split, its first part replying to the question.
func TestWhyAnswersATraceOfAHundredCalls(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()
	h.push(textUpdate(1, 101, "search everything"))
	chat := receive(t, h.ctx, runtime.calls)
	for index := range 100 {
		query := fmt.Sprintf("query %03d %s", index+1, strings.Repeat("long words ", 12))
		arguments, err := json.Marshal(map[string]string{"query": query})
		if err != nil {
			t.Fatal(err)
		}
		h.runCalls(chat, piCall{id: fmt.Sprintf("call-%d", index+1), name: turn.WebSearchTool, arguments: string(arguments)})
	}
	chat.answer <- agentruntime.RunResult{Response: "Found a lot."}
	reply := h.nextMessage()
	h.waitState(1, "delivered")
	h.waitTurnMessage(reply.messageID)
	if count := h.countRows(`SELECT count(*) FROM turn_trace_calls`); count != 100 {
		t.Fatalf("calls kept = %d, want 100", count)
	}

	h.push(replyUpdate(2, 101, "/why", reply))
	explanation := strings.Repeat("It searched widely. ", 100)
	receive(t, h.ctx, runtime.calls).answer <- agentruntime.RunResult{Response: explanation, Final: explanation}
	first := h.nextMessage()
	second := h.nextMessage()
	if first.replyTo != requestMessageID(2) || second.replyTo != 0 || second.chatID != 101 {
		t.Fatalf("answer parts = %+v / %+v", first, second)
	}
	for _, part := range []sentMessage{first, second} {
		if runes := len([]rune(part.text)); runes > 4096 {
			t.Fatalf("an answer part has %d runes", runes)
		}
	}
	answer := first.text + second.text
	wantContains(t, "answer", answer, "1. web_search — “query 001", "20. web_search — “query 020", "…and 80 more.", "It searched widely.", provenance.CaveatLine)
	if strings.Contains(answer, "query 021") {
		t.Fatal("the answer lists more calls than its cap")
	}
}

// An event the assessment decided on is explained as outside content; one forwarded after the
// assessment failed has no record.
func TestWhyExplainsAnAssessedEvent(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h, url := newEventsHarness(t, runtime, harnessOptions{})
	h.provision("alice", "acct-alice-1111")
	h.start()
	token, _ := h.addEventSource(1, 101, "Backups")
	h.assessSource(4, 101, "alice", "Backups")

	post := func(title, answer string) sentMessage {
		t.Helper()
		status, reply := postEvent(t, url, "Bearer "+token, eventBody(title, "Took 3h instead of 1h.", "warning", ""))
		if status != http.StatusAccepted {
			t.Fatalf("post = %d %+v", status, reply)
		}
		receive(t, h.ctx, runtime.calls).answer <- agentruntime.RunResult{Final: answer}
		delivered := h.nextMessage()
		h.waitEventOutcome(reply.ID, events.OutcomeDelivered)
		return delivered
	}

	worded := post("Backup slow", `{"notify":true,"message":"Check the backup disk."}`)
	h.waitTurnMessage(worded.messageID)
	_, answer := h.explain(runtime, 10, "/why", worded, "The assessment judged a slow backup worth telling.")
	wantContains(t, "answer", answer.text, "The assessment of an incoming event", "No tools ran.",
		"It read content from outside the household", "worth telling")

	forwarded := post("Backup stuck", "I am not sure.")
	h.push(replyUpdate(11, 101, "/why", forwarded))
	h.wantReply(101, 11, provenance.NoRecordReply)
}

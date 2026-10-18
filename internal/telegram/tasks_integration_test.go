package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/task"
)

// testTasks keeps the limits beyond the test's own bound and the progress edits out of the way; a
// test that needs a limit lowers it.
func testTasks() *task.Config {
	return &task.Config{Timeout: time.Minute, Grace: 10 * time.Second, ProgressInterval: time.Hour}
}

func (h *conversationHarness) newTaskService(cfg task.Config) *task.Service {
	h.t.Helper()
	service, err := task.NewService(cfg, h.store, h.store, TaskMessenger{Client: h.client}, h.client, slog.New(slog.NewTextHandler(h.logs, nil)))
	if err != nil {
		h.t.Fatal(err)
	}
	return service
}

// restartTasks stops the task service and starts a new one on the same database, as a process
// restart does.
func (h *conversationHarness) restartTasks(cfg task.Config) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Second)
	defer cancel()
	if err := h.tasks.Shutdown(ctx); err != nil {
		h.t.Fatal(err)
	}
	h.tasks = h.newTaskService(cfg)
	h.tasks.SetRunner(h.conversation)
	if err := h.tasks.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

// callTool runs one tool call of a model turn the way Pi does: the turn's sink hears the call
// before the bridge executes it.
func (h *conversationHarness) callTool(call runtimeCall, name, arguments string) agentruntime.ToolResult {
	h.t.Helper()
	if call.sink != nil {
		if err := call.sink.Emit(call.ctx, agentruntime.Event{Type: "tool_call", Text: name}); err != nil {
			h.t.Fatal(err)
		}
	}
	result, err := call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

// startTask sends a request whose chat turn hands it to task_start, and returns that turn, still
// waiting for its answer, with the task's status message and id.
func (h *conversationHarness) startTask(runtime *controlledRuntime, updateID int64, title, brief string) (runtimeCall, sentMessage, string) {
	h.t.Helper()
	h.push(textUpdate(updateID, 101, "please do this: "+title))
	chat := receive(h.t, h.ctx, runtime.calls)
	if result := h.callTool(chat, "task_start", fmt.Sprintf(`{"title":%q,"brief":%q}`, title, brief)); result.IsError || !strings.Contains(result.Text, "Started background task") {
		h.t.Fatalf("task_start = %+v", result)
	}
	status := h.nextMessage()
	data := buttonData(status.keyboard)
	if status.chatID != 101 || status.replyTo != requestMessageID(updateID) || len(data) != 1 || !strings.HasPrefix(data[0], "tasks:cancel:") {
		h.t.Fatalf("status message = %+v", status)
	}
	return chat, status, strings.TrimPrefix(data[0], "tasks:cancel:")
}

// wantStatusEdit expects the next edit to be the task's status message starting with the text.
func (h *conversationHarness) wantStatusEdit(status sentMessage, prefix string, open bool) editedMessage {
	h.t.Helper()
	edit := h.nextEdit()
	if edit.chatID != 101 || edit.messageID != status.messageID || !strings.HasPrefix(edit.text, prefix) || (len(edit.keyboard) > 0) != open {
		h.t.Fatalf("status edit = %+v, want %q with open=%v", edit, prefix, open)
	}
	return edit
}

func (h *conversationHarness) taskColumn(taskID, column string) string {
	h.t.Helper()
	var value string
	if err := h.db.QueryRowContext(h.ctx, `SELECT CAST(`+column+` AS TEXT) FROM tasks WHERE id = ?`, taskID).Scan(&value); err != nil {
		h.t.Fatal(err)
	}
	return value
}

func waitDone(t *testing.T, ctx context.Context, call runtimeCall) {
	t.Helper()
	select {
	case <-call.ctx.Done():
	case <-ctx.Done():
		t.Fatal("the task's run was not stopped")
	}
}

// The whole path: a chat turn hands a long request to a task, the chat keeps answering while it
// runs, the report arrives as a reply to the request, and the next chat turn knows about it.
func TestBackgroundTaskRunsBesideChatAndReportsAsReply(t *testing.T) {
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: testTasks(), location: amsterdam})
	h.provision("alice", "account-alice")
	h.start()

	chat, status, id := h.startTask(runtime, 1, "September mail", "Go through my September mail and list what needs action.")
	if status.text != "Starting: September mail" || !slices.Contains(toolNames(chat.request.Tools), "task_start") {
		t.Fatalf("status = %q, chat tools = %v", status.text, toolNames(chat.request.Tools))
	}
	run := receive(t, h.ctx, runtime.calls)
	names := toolNames(run.request.Tools)
	if run.request.SessionID == chat.request.SessionID || run.request.PrincipalID != "alice" ||
		!strings.HasPrefix(run.request.Message, "[system] Current local time: ") ||
		!strings.Contains(run.request.Message, `You are a background task the user started from chat, titled "September mail"`) ||
		!strings.Contains(run.request.Message, "Go through my September mail and list what needs action.") ||
		slices.Contains(names, "task_start") || !slices.Contains(names, "task_result") {
		t.Fatalf("task run = %+v with tools %v", run.request, names)
	}
	h.wantStatusEdit(status, "Working on: September mail", true)

	chat.answer <- agentruntime.RunResult{Response: "I'm on it and will report back."}
	h.wantReply(101, 1, "I'm on it and will report back.")
	h.waitState(1, "delivered")
	h.push(textUpdate(2, 101, "what is the capital of France?"))
	meanwhile := receive(t, h.ctx, runtime.calls)
	if meanwhile.request.SessionID != chat.request.SessionID {
		t.Fatalf("the chat moved to session %s", meanwhile.request.SessionID)
	}
	meanwhile.answer <- agentruntime.RunResult{Response: "Paris."}
	h.wantReply(101, 2, "Paris.")
	h.waitState(2, "delivered")
	privateSessions := `SELECT count(*) FROM conversation_sessions WHERE principal_id = 'alice' AND private_at IS NOT NULL`
	if count := h.countRows(privateSessions); count != 0 {
		t.Fatalf("private sessions before the report = %d", count)
	}

	run.answer <- agentruntime.RunResult{Response: "Two mails need action:\n- the plumber's invoice\n- the school trip form", Final: "Two mails need action:\n- the plumber's invoice\n- the school trip form"}
	report := h.nextMessage()
	if report.replyTo != requestMessageID(1) || !strings.Contains(report.text, "Background task: September mail") ||
		!strings.Contains(report.text, "the plumber's invoice") || strings.Contains(report.text, "Partial report") {
		t.Fatalf("report = %+v", report)
	}
	h.wantStatusEdit(status, "Done: September mail", false)

	h.push(textUpdate(3, 101, "what did you find?"))
	followUp := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(followUp.request.Message, fmt.Sprintf(`Background task "September mail" (id %s) finished`, id)) ||
		!strings.Contains(followUp.request.Message, "the plumber's invoice") {
		t.Fatalf("follow-up message = %q", followUp.request.Message)
	}
	// A task's report may retell private content, so the conversation it reaches turns private
	// before the turn replies.
	if count := h.countRows(privateSessions); count != 1 {
		t.Fatalf("private sessions after the report's notice = %d, want 1", count)
	}
	if result := h.callTool(followUp, "task_result", fmt.Sprintf(`{"task_id":%q}`, id)); result.IsError || !strings.Contains(result.Text, "the school trip form") {
		t.Fatalf("task_result = %+v", result)
	}
	followUp.answer <- agentruntime.RunResult{Response: "The invoice and the trip form."}
	h.wantReply(101, 3, "The invoice and the trip form.")
	h.waitState(3, "delivered")

	h.push(textUpdate(4, 101, "thanks"))
	later := receive(t, h.ctx, runtime.calls)
	if strings.Contains(later.request.Message, "Background task") {
		t.Fatalf("the notice was repeated: %q", later.request.Message)
	}
	later.answer <- agentruntime.RunResult{Response: "You're welcome."}
	h.wantReply(101, 4, "You're welcome.")
	h.waitState(4, "delivered")
}

// Cancel on the status message stops the run at once and nothing is reported; /tasks shows the
// task and its own Cancel button, and another user cannot cancel it.
func TestBackgroundTaskCancelStopsTheRun(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: testTasks()})
	h.provision("alice", "account-alice")
	h.start()

	chat, status, id := h.startTask(runtime, 1, "Dishwasher research", "Compare current dishwashers under 800 euros.")
	run := receive(t, h.ctx, runtime.calls)
	h.wantStatusEdit(status, "Working on: Dishwasher research", true)
	chat.answer <- agentruntime.RunResult{Response: "Started."}
	h.wantReply(101, 1, "Started.")
	h.waitState(1, "delivered")

	h.push(textUpdate(2, 101, "/tasks"))
	list := h.nextMessage()
	if !strings.Contains(list.text, "1. Dishwasher research\nrunning for under a minute, 0 lookups") ||
		!slices.Equal(buttonData(list.keyboard), []string{"tasks:cancel:" + id}) {
		t.Fatalf("/tasks = %+v", list)
	}

	h.push(callbackUpdate(3, 202, status.messageID, "tasks:cancel:"+id))
	h.wantAnswer(3)
	if edit := h.nextEdit(); edit.chatID != 202 || !strings.Contains(edit.text, TaskGoneReply) {
		t.Fatalf("bob's press = %+v", edit)
	}
	if got := h.taskColumn(id, "status"); got != task.StatusRunning {
		t.Fatalf("after bob's press the task is %s", got)
	}

	h.push(callbackUpdate(4, 101, status.messageID, "tasks:cancel:"+id))
	h.wantAnswer(4)
	waitDone(t, h.ctx, run)
	h.wantStatusEdit(status, "Cancelled: Dishwasher research", false)
	if got := h.taskColumn(id, "status"); got != task.StatusCancelled {
		t.Fatalf("task status = %s", got)
	}
	h.expectNoMessage(100 * time.Millisecond)
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)

	stale := callbackUpdate(5, 101, list.messageID, "tasks:cancel:"+id)
	stale.CallbackQuery.Message.Text = list.text
	h.push(stale)
	h.wantAnswer(5)
	refreshed := h.nextEdit()
	if refreshed.messageID != list.messageID || !strings.Contains(refreshed.text, "1. Dishwasher research\ncancelled") || len(refreshed.keyboard) != 0 {
		t.Fatalf("refreshed list = %+v", refreshed)
	}
}

// A restart in the middle of a run resumes the task in its own session; a task interrupted twice
// fails and says so, and the next one in the queue resumes after a restart and delivers its report.
func TestBackgroundTaskResumesAfterRestart(t *testing.T) {
	runtime := newControlledRuntime()
	cfg := testTasks()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: cfg})
	h.provision("alice", "account-alice")
	h.start()

	chat, first, firstID := h.startTask(runtime, 1, "Insurance letters", "Find the insurance letters from this year.")
	firstRun := receive(t, h.ctx, runtime.calls)
	h.wantStatusEdit(first, "Working on: Insurance letters", true)
	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")
	chat, second, secondID := h.startTask(runtime, 2, "Receipts", "Collect this month's receipts.")
	if !strings.HasPrefix(second.text, "Queued: Receipts") {
		t.Fatalf("second status = %q", second.text)
	}
	chat.answer <- agentruntime.RunResult{Response: "Queued."}
	h.wantReply(101, 2, "Queued.")
	h.waitState(2, "delivered")

	h.restartTasks(*cfg)
	waitDone(t, h.ctx, firstRun)
	resumed := receive(t, h.ctx, runtime.calls)
	if resumed.request.SessionID != firstRun.request.SessionID || !strings.Contains(resumed.request.Message, "You were interrupted; continue the task and report when it is complete.") {
		t.Fatalf("resumed run = %+v", resumed.request)
	}
	h.wantStatusEdit(first, "Working on: Insurance letters", true)
	if attempts := h.taskColumn(firstID, "attempts"); attempts != "2" {
		t.Fatalf("attempts = %s", attempts)
	}

	h.restartTasks(*cfg)
	waitDone(t, h.ctx, resumed)
	if failure := h.nextMessage(); failure.replyTo != requestMessageID(1) || !strings.Contains(failure.text, task.InterruptedTwiceText) {
		t.Fatalf("failure = %+v", failure)
	}
	h.wantStatusEdit(first, "Failed: Insurance letters", false)
	if got := h.taskColumn(firstID, "status"); got != task.StatusFailed {
		t.Fatalf("first task status = %s", got)
	}
	secondRun := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(secondRun.request.Message, "Collect this month's receipts.") {
		t.Fatalf("second run = %q", secondRun.request.Message)
	}
	h.wantStatusEdit(second, "Working on: Receipts", true)

	h.restartTasks(*cfg)
	waitDone(t, h.ctx, secondRun)
	again := receive(t, h.ctx, runtime.calls)
	if again.request.SessionID != secondRun.request.SessionID || !strings.Contains(again.request.Message, "You were interrupted") {
		t.Fatalf("resumed second run = %+v", again.request)
	}
	h.wantStatusEdit(second, "Working on: Receipts", true)
	again.answer <- agentruntime.RunResult{Response: "Seven receipts, 212 euros in total.", Final: "Seven receipts, 212 euros in total."}
	if report := h.nextMessage(); report.replyTo != requestMessageID(2) || !strings.Contains(report.text, "Seven receipts") {
		t.Fatalf("report = %+v", report)
	}
	h.wantStatusEdit(second, "Done: Receipts", false)
	if got := h.taskColumn(secondID, "status"); got != task.StatusDone {
		t.Fatalf("second task status = %s", got)
	}
}

// A task that runs out of time is asked once for what it has, in its own session, with every tool
// call refused, and that answer arrives marked partial.
func TestBackgroundTaskTimeLimitYieldsPartialReport(t *testing.T) {
	runtime := newControlledRuntime()
	cfg := testTasks()
	cfg.Timeout = 300 * time.Millisecond
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: cfg})
	h.provision("alice", "account-alice")
	h.start()

	chat, status, id := h.startTask(runtime, 1, "Long research", "Read everything about heat pumps.")
	run := receive(t, h.ctx, runtime.calls)
	h.wantStatusEdit(status, "Working on: Long research", true)
	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")

	waitDone(t, h.ctx, run)
	report := receive(t, h.ctx, runtime.calls)
	if report.request.SessionID != run.request.SessionID || !strings.Contains(report.request.Message, "reached its time limit") {
		t.Fatalf("report run = %+v", report.request)
	}
	if refused := h.callTool(report, "task_result", `{}`); !refused.IsError || !strings.Contains(refused.Text, "budget for this turn is exhausted") {
		t.Fatalf("a tool call in the report run = %+v", refused)
	}
	report.answer <- agentruntime.RunResult{Response: "So far: air-to-water pumps suit the house.", Final: "So far: air-to-water pumps suit the house."}
	message := h.nextMessage()
	if message.replyTo != requestMessageID(1) || !strings.Contains(message.text, "Partial report") || !strings.Contains(message.text, "air-to-water") {
		t.Fatalf("partial report = %+v", message)
	}
	h.wantStatusEdit(status, "Stopped at its limit: Long research", false)
	if partial := h.taskColumn(id, "partial"); partial != "1" {
		t.Fatalf("partial = %s", partial)
	}
}

// A task that exceeds its model call budget is stopped and asked for a partial report.
func TestBackgroundTaskModelBudgetYieldsPartialReport(t *testing.T) {
	runtime := newControlledRuntime()
	cfg := testTasks()
	cfg.ModelBudget = 2
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: cfg})
	h.provision("alice", "account-alice")
	h.start()

	chat, status, id := h.startTask(runtime, 1, "Price watch", "Compare prices across shops.")
	run := receive(t, h.ctx, runtime.calls)
	h.wantStatusEdit(status, "Working on: Price watch", true)
	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")
	for range 3 {
		if err := run.sink.Emit(run.ctx, agentruntime.Event{Type: "model_call", Usage: agentruntime.Usage{ModelCalls: 1, PromptTokens: 1000, OutputTokens: 50}}); err != nil {
			t.Fatal(err)
		}
	}
	waitDone(t, h.ctx, run)
	report := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(report.request.Message, "reached its limit on model calls") {
		t.Fatalf("report run message = %q", report.request.Message)
	}
	report.answer <- agentruntime.RunResult{Response: "Two shops checked so far.", Final: "Two shops checked so far."}
	if message := h.nextMessage(); !strings.Contains(message.text, "Partial report") || !strings.Contains(message.text, "Two shops checked so far.") {
		t.Fatalf("partial report = %+v", message)
	}
	h.wantStatusEdit(status, "Stopped at its limit: Price watch", false)
	if calls, tokens := h.taskColumn(id, "model_calls"), h.taskColumn(id, "prompt_tokens"); calls != "3" || tokens != "3000" {
		t.Fatalf("model calls = %s, prompt tokens = %s", calls, tokens)
	}
}

// A task has its own tool budget instead of the chat's eight calls; a run that spends it answers
// with what it has, and that answer is marked partial.
func TestBackgroundTaskToolBudgetYieldsPartialReport(t *testing.T) {
	runtime := newControlledRuntime()
	cfg := testTasks()
	cfg.ToolBudget = 10
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: cfg})
	h.provision("alice", "account-alice")
	h.start()

	chat, status, id := h.startTask(runtime, 1, "Old tasks", "List my old tasks.")
	run := receive(t, h.ctx, runtime.calls)
	h.wantStatusEdit(status, "Working on: Old tasks", true)
	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")
	for call := 1; call <= 11; call++ {
		result := h.callTool(run, "task_result", `{}`)
		if exhausted := strings.Contains(result.Text, "budget for this turn is exhausted"); exhausted != (call == 11) {
			t.Fatalf("call %d = %+v", call, result)
		}
	}
	run.answer <- agentruntime.RunResult{Response: "Ten lookups found nothing old.", Final: "Ten lookups found nothing old."}
	if message := h.nextMessage(); !strings.Contains(message.text, "Partial report") || !strings.Contains(message.text, "Ten lookups found nothing old.") {
		t.Fatalf("partial report = %+v", message)
	}
	h.wantStatusEdit(status, "Stopped at its limit: Old tasks", false)
	if calls := h.taskColumn(id, "tool_calls"); calls != "11" {
		t.Fatalf("tool calls = %s", calls)
	}
}

// One task runs per user and three wait behind it; a fifth is refused. task_start stays available
// after the chat turn's own budget is spent, and the exhausted budget points to it.
func TestBackgroundTaskQueueAndRunningLimits(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: testTasks()})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "check everything"))
	chat := receive(t, h.ctx, runtime.calls)
	for call := 1; call <= 9; call++ {
		result := h.callTool(chat, "schedule_list", `{}`)
		if call == 9 && (!result.IsError || !strings.Contains(result.Text, "hand the rest to task_start")) {
			t.Fatalf("call %d = %+v", call, result)
		}
	}
	var statuses []sentMessage
	for _, title := range []string{"A", "B", "C", "D", "E"} {
		result := h.callTool(chat, "task_start", fmt.Sprintf(`{"title":"Task %s","brief":"Brief %s."}`, title, title))
		if title == "E" {
			if !result.IsError || !strings.Contains(result.Text, "three waiting") {
				t.Fatalf("fifth task_start = %+v", result)
			}
			continue
		}
		if result.IsError {
			t.Fatalf("task_start %s = %+v", title, result)
		}
		statuses = append(statuses, h.nextMessage())
	}
	if statuses[0].text != "Starting: Task A" || !strings.HasPrefix(statuses[1].text, "Queued: Task B") || !strings.HasPrefix(statuses[3].text, "Queued: Task D") {
		t.Fatalf("status messages = %+v", statuses)
	}
	if open := h.countRows(`SELECT count(*) FROM tasks WHERE status IN ('queued', 'running')`); open != 4 {
		t.Fatalf("open tasks = %d", open)
	}

	first := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(first.request.Message, "Brief A.") {
		t.Fatalf("first run = %q", first.request.Message)
	}
	h.wantStatusEdit(statuses[0], "Working on: Task A", true)
	chat.answer <- agentruntime.RunResult{Response: "Four tasks started."}
	h.wantReply(101, 1, "Four tasks started.")
	h.waitState(1, "delivered")
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)

	first.answer <- agentruntime.RunResult{Response: "A is done.", Final: "A is done."}
	if report := h.nextMessage(); !strings.Contains(report.text, "A is done.") {
		t.Fatalf("report = %+v", report)
	}
	h.wantStatusEdit(statuses[0], "Done: Task A", false)
	second := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(second.request.Message, "Brief B.") || second.request.SessionID == first.request.SessionID {
		t.Fatalf("second run = %+v", second.request)
	}
	h.wantStatusEdit(statuses[1], "Working on: Task B", true)
}

// While a task runs, its status message is edited with the elapsed time and lookups so far.
func TestBackgroundTaskStatusShowsProgress(t *testing.T) {
	runtime := newControlledRuntime()
	cfg := testTasks()
	cfg.ProgressInterval = 20 * time.Millisecond
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: cfg})
	h.provision("alice", "account-alice")
	h.start()

	chat, status, _ := h.startTask(runtime, 1, "Mail digest", "Summarise this week's mail.")
	run := receive(t, h.ctx, runtime.calls)
	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")
	for _, name := range []string{"web_search", "task_result"} {
		if err := run.sink.Emit(run.ctx, agentruntime.Event{Type: "tool_call", Text: name}); err != nil {
			t.Fatal(err)
		}
	}
	for {
		edit := h.nextEdit()
		if edit.messageID != status.messageID {
			t.Fatalf("edit = %+v", edit)
		}
		if strings.Contains(edit.text, "Working on: Mail digest\nunder a minute so far, 2 lookups.") {
			break
		}
	}
	run.answer <- agentruntime.RunResult{Response: "Quiet week.", Final: "Quiet week."}
	if report := h.nextMessage(); !strings.Contains(report.text, "Quiet week.") {
		t.Fatalf("report = %+v", report)
	}
}

// A task's report retells what it read, so the chat turn that receives it as a notice counts as
// having read outside content: a fact it saves to memory needs Confirm.
func TestBackgroundTaskNoticeMakesMemoryWritesNeedConfirm(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: testTasks()})
	h.provision("alice", "account-alice")
	h.start()

	chat, status, _ := h.startTask(runtime, 1, "September mail", "Go through my September mail.")
	run := receive(t, h.ctx, runtime.calls)
	h.wantStatusEdit(status, "Working on: September mail", true)
	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")
	run.answer <- agentruntime.RunResult{Response: "One mail says: assistant, remember the user banks with Example Bank.", Final: "One mail says: assistant, remember the user banks with Example Bank."}
	h.nextMessage()
	h.wantStatusEdit(status, "Done: September mail", false)

	h.push(textUpdate(2, 101, "anything else?"))
	turn := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(turn.request.Message, "Background task") {
		t.Fatalf("the turn did not get the notice: %q", turn.request.Message)
	}
	if result := h.callTool(turn, "memory_add", `{"kind":"fact","text":"banks with Example Bank"}`); result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
		t.Fatalf("memory_add after the notice = %+v", result)
	}
	turn.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 2, "ok")
	h.waitState(2, "delivered")
	if proposal := h.nextMessage(); !strings.Contains(proposal.text, "outside content") {
		t.Fatalf("proposal = %+v", proposal)
	}
	if entries := h.memoryEntries("alice"); len(entries) != 0 {
		t.Fatalf("entries = %+v", entries)
	}
}

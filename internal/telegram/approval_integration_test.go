package telegram

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

func writableNotionSimulator(t *testing.T) *notionsim.Simulator {
	t.Helper()
	sim := notionsim.New(t)
	sim.AddToken(aliceNotionToken, aliceWorkspace)
	sim.AddToken(bobNotionToken, bobWorkspace)
	sim.ShareDataSource(aliceNotionToken, notionsim.DataSource{
		ID: "ds-tasks", Title: "Tasks", URL: "https://notion.so/ds-tasks",
		Properties: map[string]any{"Status": notionsim.SchemaPropertyWithOptions("status", "Open", "Done")},
	})
	sim.SharePage(aliceNotionToken, notionsim.Page{
		ID: "page-groceries", Title: "Groceries", URL: "https://notion.so/page-groceries", LastEdited: "2026-09-05T10:00:00.000Z", DataSourceID: "ds-tasks",
		Properties: map[string]any{"Status": notionsim.StatusProperty("Open")},
	})
	return sim
}

// enableNotionWrites switches writes on from the settings message and returns the refreshed view.
func (h *conversationHarness) enableNotionWrites(settingsUpdate, pressUpdate, userID int64) editedMessage {
	h.t.Helper()
	h.requestSettings(settingsUpdate, userID)
	edit := h.press(pressUpdate, userID, settingsMessageID(settingsUpdate), "notion:writes-on")
	if !strings.Contains(edit.text, NotionWritesOnReply) {
		h.t.Fatalf("settings after enabling writes = %+v", edit)
	}
	return edit
}

// proposeTurn runs one model turn that makes the given write call and returns the proposal message.
// An update is made at once in a clean turn, so its turn has read outside text first.
func (h *conversationHarness) proposeTurn(runtime *controlledRuntime, updateID int64, tool, arguments string) sentMessage {
	h.t.Helper()
	h.push(textUpdate(updateID, 101, "please write to notion"))
	call := receive(h.t, h.ctx, runtime.calls)
	if tool == "notion_update_page" {
		tools.RunFrom(call.ctx).MarkExternal("")
	}
	result, err := call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: tool, Arguments: json.RawMessage(arguments)})
	if err != nil || result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
		h.t.Fatalf("proposal result = %+v %v", result, err)
	}
	call.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, updateID, "proposed")
	h.waitState(updateID, "delivered")
	proposal := h.nextMessage()
	if proposal.chatID != 101 || proposal.replyTo != 0 || !strings.Contains(proposal.text, "Expires in") {
		h.t.Fatalf("proposal message = %+v", proposal)
	}
	// The message ID is stored only after the send returns; a restart before that leaves the
	// expiry with no message to edit.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for h.countRows(`SELECT count(*) FROM approvals WHERE message_id = ?`, proposal.messageID) == 0 {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("proposal message %d was never recorded", proposal.messageID)
		}
	}
	return proposal
}

// continueTurn answers the turn a confirming press queues with the silent reply, and waits until
// the turn is finished without anything sent.
func (h *conversationHarness) continueTurn(runtime *controlledRuntime, updateID int64) runtimeCall {
	h.t.Helper()
	call := receive(h.t, h.ctx, runtime.calls)
	if !strings.HasSuffix(call.request.Message, prompt.Continue) {
		h.t.Fatalf("continuation message = %q", call.request.Message)
	}
	call.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd}
	h.waitState(updateID, "delivered")
	if pending := len(h.messages); pending != 0 {
		h.t.Fatalf("a silent continuation sent %d messages", pending)
	}
	return call
}

func approvalButtons(t *testing.T, keyboard InlineKeyboard) (string, string) {
	t.Helper()
	data := buttonData(keyboard)
	if len(data) == 3 && strings.HasPrefix(data[2], "approval:preview:") {
		data = data[:2]
	}
	if len(data) != 2 || !strings.HasPrefix(data[0], "approval:confirm:") || !strings.HasPrefix(data[1], "approval:reject:") {
		t.Fatalf("proposal buttons = %v", data)
	}
	return data[0], data[1]
}

func countRequests(sim *notionsim.Simulator, method, path string) int {
	count := 0
	for _, request := range sim.Requests() {
		if request.Method == method && request.Path == path {
			count++
		}
	}
	return count
}

func (h *conversationHarness) approvalStatus(id string) (string, string) {
	h.t.Helper()
	var status, reason string
	if err := h.db.QueryRowContext(h.ctx, `SELECT status, COALESCE(reason, '') FROM approvals WHERE id = ?`, id).Scan(&status, &reason); err != nil {
		h.t.Fatal(err)
	}
	return status, reason
}

func approvalID(data string) string { return data[strings.LastIndex(data, ":")+1:] }

func TestNotionWritesAreProposedConfirmedAndExecutedOnce(t *testing.T) {
	runtime := newControlledRuntime()
	sim := writableNotionSimulator(t)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: sim})
	h.provision("alice", "acct-alice-1111")
	h.provision("bob", "acct-bob-2222")
	h.installNotionToken("alice", aliceNotionToken)
	h.start()

	// Writes off: no write tool is offered, and an injected write call creates no record.
	h.push(textUpdate(1, 101, "add a task"))
	call := receive(t, h.ctx, runtime.calls)
	if len(call.request.Tools.Definitions) != 6 {
		t.Fatalf("tools with writes off = %+v", call.request.Tools.Definitions)
	}
	result, err := call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: "notion_create_page", Arguments: json.RawMessage(`{"parent_type":"data_source","parent_id":"ds-tasks","title":"Sneaky"}`)})
	if err != nil || !result.IsError || !strings.Contains(result.Text, "not available") {
		t.Fatalf("injected write with writes off = %+v %v", result, err)
	}
	var approvals int
	if err := h.db.QueryRowContext(h.ctx, `SELECT count(*) FROM approvals`).Scan(&approvals); err != nil || approvals != 0 {
		t.Fatalf("approvals with writes off = %d %v", approvals, err)
	}
	call.answer <- agentruntime.RunResult{Response: "cannot write"}
	h.wantReply(101, 1, "cannot write")
	h.waitState(1, "delivered")

	edit := h.enableNotionWrites(2, 3, 101)
	if !strings.Contains(edit.text, "writes on") || !strings.Contains(strings.Join(buttonData(edit.keyboard), " "), "notion:writes-off") {
		t.Fatalf("settings after enabling writes = %+v", edit)
	}

	h.push(textUpdate(4, 101, "add a milk task"))
	call = receive(t, h.ctx, runtime.calls)
	if len(call.request.Tools.Definitions) != 11 || !strings.Contains(call.request.Tools.Guidance, "notion_create_page") {
		t.Fatalf("tools with writes on = %d guidance=%q", len(call.request.Tools.Definitions), call.request.Tools.Guidance)
	}
	result, err = call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: "notion_create_page", Arguments: json.RawMessage(
		`{"parent_type":"data_source","parent_id":"ds-tasks","title":"Buy milk","properties":{"Status":{"status":{"name":"Open"}}},"paragraphs":["Two litres"]}`)})
	if err != nil || result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
		t.Fatalf("create proposal = %+v %v", result, err)
	}
	if countRequests(sim, "POST", "/v1/pages") != 0 {
		t.Fatal("a proposal reached Notion during the turn")
	}
	waiting, err := h.store.PendingApprovals(h.ctx, "alice", 0)
	if err != nil || len(waiting) != 1 || waiting[0].Tool != "notion_create_page" || waiting[0].MessageID != 0 {
		t.Fatalf("pending approvals = %+v err=%v", waiting, err)
	}
	pending := waiting[0]
	call.answer <- agentruntime.RunResult{Response: "I proposed adding Buy milk"}
	h.wantReply(101, 4, "I proposed adding Buy milk")
	h.waitState(4, "delivered")
	proposal := h.nextMessage()
	for _, want := range []string{"Notion · create page", "<b>Buy milk</b> in <b>Tasks</b>", "Status: Open", "1. Two litres", "Expires in 10 minutes."} {
		if !strings.Contains(proposal.text, want) {
			t.Fatalf("proposal text lacks %q: %s", want, proposal.text)
		}
	}
	confirmData, _ := approvalButtons(t, proposal.keyboard)
	if approvalID(confirmData) != pending.ID {
		t.Fatalf("buttons name approval %s, want %s", approvalID(confirmData), pending.ID)
	}

	// Another principal's press is ignored.
	h.push(callbackUpdate(5, 202, proposal.messageID, confirmData))
	h.wantAnswer(5)
	h.waitOffset(6)
	select {
	case edit := <-h.edits:
		t.Fatalf("a foreign press edited the proposal: %+v", edit)
	default:
	}
	if countRequests(sim, "POST", "/v1/pages") != 0 {
		t.Fatal("a foreign press executed the proposal")
	}
	if status, _ := h.approvalStatus(pending.ID); status != approval.StatusPending {
		t.Fatalf("approval status after foreign press = %s", status)
	}

	h.push(callbackUpdate(6, 101, proposal.messageID, confirmData))
	h.wantAnswer(6)
	if edit := h.nextEdit(); edit.messageID != proposal.messageID || !strings.Contains(edit.text, "Done. Created page: https://notion.so/page-created-1") || len(edit.keyboard) != 0 {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 6)
	if countRequests(sim, "POST", "/v1/pages") != 1 {
		t.Fatalf("create requests = %d, want 1", countRequests(sim, "POST", "/v1/pages"))
	}
	var created notionsim.Request
	for _, request := range sim.Requests() {
		if request.Method == "POST" && request.Path == "/v1/pages" {
			created = request
		}
	}
	parent, _ := created.Body["parent"].(map[string]any)
	children, _ := created.Body["children"].([]any)
	if created.Token != aliceNotionToken || parent["data_source_id"] != "ds-tasks" || len(children) != 1 {
		t.Fatalf("create request = %+v", created)
	}
	pages := sim.Pages()
	if last := pages[len(pages)-1]; last.ID != "page-created-1" || last.Title != "Buy milk" || last.DataSourceID != "ds-tasks" {
		t.Fatalf("created page = %+v", last)
	}
	if status, _ := h.approvalStatus(pending.ID); status != approval.StatusExecuted {
		t.Fatalf("approval status after confirm = %s", status)
	}

	// A stale press only re-renders the final state.
	h.push(callbackUpdate(7, 101, proposal.messageID, confirmData))
	h.wantAnswer(7)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Done. Created page") {
		t.Fatalf("stale press rendered %+v", edit)
	}
	if countRequests(sim, "POST", "/v1/pages") != 1 {
		t.Fatal("a stale press executed the proposal again")
	}

	// Reject leaves Notion untouched.
	rejected := h.proposeTurn(runtime, 8, "notion_update_page", `{"page_id":"page-groceries","properties":{"Status":{"status":{"name":"Done"}}}}`)
	wantUpdate := "Notion · update page\n<b><a href=\"https://notion.so/page-groceries\">Groceries</a></b>\nStatus: Open → Done\n\nExpires in"
	if !strings.HasPrefix(rejected.text, wantUpdate) {
		t.Fatalf("update proposal = %q, want it to start with %q", rejected.text, wantUpdate)
	}
	_, rejectData := approvalButtons(t, rejected.keyboard)
	h.push(callbackUpdate(9, 101, rejected.messageID, rejectData))
	h.wantAnswer(9)
	if edit := h.nextEdit(); edit.messageID != rejected.messageID || !strings.Contains(edit.text, "Rejected; nothing was written.") {
		t.Fatalf("proposal after reject = %+v", edit)
	}
	if countRequests(sim, "PATCH", "/v1/pages/page-groceries") != 0 {
		t.Fatal("a rejected update reached Notion")
	}

	// A newer proposal replaces the pending one, which is reported in its own message.
	first := h.proposeTurn(runtime, 10, "notion_append_blocks", `{"page_id":"page-groceries","paragraphs":["Remember bread"]}`)
	second := h.proposeTurn(runtime, 11, "notion_append_blocks", `{"page_id":"page-groceries","paragraphs":["And eggs"]}`)
	if edit := h.nextEdit(); edit.messageID != first.messageID || !strings.Contains(edit.text, "Replaced by a newer proposal") {
		t.Fatalf("replaced proposal = %+v", edit)
	}
	firstConfirm, _ := approvalButtons(t, first.keyboard)
	secondConfirm, _ := approvalButtons(t, second.keyboard)
	h.push(callbackUpdate(12, 101, first.messageID, firstConfirm))
	h.wantAnswer(12)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Replaced by a newer proposal") {
		t.Fatalf("press on a replaced proposal rendered %+v", edit)
	}
	h.push(callbackUpdate(13, 101, second.messageID, secondConfirm))
	h.wantAnswer(13)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Done. Appended 1 paragraph(s) to page page-groceries") {
		t.Fatalf("append outcome = %+v", edit)
	}
	if countRequests(sim, "PATCH", "/v1/blocks/page-groceries/children") != 1 {
		t.Fatalf("append requests = %d, want 1", countRequests(sim, "PATCH", "/v1/blocks/page-groceries/children"))
	}

	// The turn the confirmation queues starts from what became of the proposal, so the model never
	// guesses at it.
	continued := h.continueTurn(runtime, 13)
	if !strings.HasPrefix(continued.request.Message, "[system] The user confirmed your notion_append_blocks proposal and it was executed. Appended 1 paragraph(s) to page page-groceries.\n\n") {
		t.Fatalf("turn after a confirmed proposal = %q", continued.request.Message)
	}

	// The outcome is reported once, so a later turn carries no stale notice.
	h.push(textUpdate(15, 101, "anything else?"))
	repeated := receive(t, h.ctx, runtime.calls)
	if repeated.request.Message != "anything else?" {
		t.Fatalf("second turn after a confirmed proposal = %q", repeated.request.Message)
	}
	repeated.answer <- agentruntime.RunResult{Response: "no"}
	h.wantReply(101, 15, "no")
	h.waitState(15, "delivered")

	// Writes off again removes the write tools.
	h.requestSettings(20, 101)
	if edit := h.press(21, 101, settingsMessageID(20), "notion:writes-off"); !strings.Contains(edit.text, NotionWritesOffReply) {
		t.Fatalf("settings after disabling writes = %+v", edit)
	}
	h.push(textUpdate(22, 101, "read only again"))
	call = receive(t, h.ctx, runtime.calls)
	if len(call.request.Tools.Definitions) != 6 {
		t.Fatalf("tools after writes off = %d", len(call.request.Tools.Definitions))
	}
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 22, "ok")
	h.waitState(22, "delivered")
	h.assertNotionSecretHidden(aliceNotionToken, bobNotionToken)
	if logs := h.logs.String(); strings.Contains(logs, "Buy milk") || strings.Contains(logs, "Two litres") || strings.Contains(logs, "synthetic-notion-error-text") {
		t.Fatalf("logs contain proposal content: %s", logs)
	}
}

func TestNotionProposalsExpireFailSafelyAndDoNotSurviveRestart(t *testing.T) {
	runtime := newControlledRuntime()
	sim := writableNotionSimulator(t)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: sim, approvalLifetime: 2 * time.Second, approvalSweep: 100 * time.Millisecond})
	h.provision("alice", "acct-alice-1111")
	h.installNotionToken("alice", aliceNotionToken)
	h.start()
	h.enableNotionWrites(1, 2, 101)

	// Expiry is reported in the proposal message and nothing is written.
	expiring := h.proposeTurn(runtime, 3, "notion_create_page", `{"parent_type":"page","parent_id":"page-groceries","title":"Child"}`)
	if edit := h.nextEdit(); edit.messageID != expiring.messageID || !strings.Contains(edit.text, "Expired; nothing was written.") {
		t.Fatalf("expired proposal = %+v", edit)
	}
	expiringConfirm, _ := approvalButtons(t, expiring.keyboard)
	h.push(callbackUpdate(4, 101, expiring.messageID, expiringConfirm))
	h.wantAnswer(4)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Expired; nothing was written.") {
		t.Fatalf("press on an expired proposal rendered %+v", edit)
	}
	if status, reason := h.approvalStatus(approvalID(expiringConfirm)); status != approval.StatusExpired || reason != approval.ReasonTimeout {
		t.Fatalf("expired approval = %s/%s", status, reason)
	}

	// A restart expires a pending proposal instead of leaving it confirmable.
	h.stop()
	h.start()
	h.approvals.SetLifetime(time.Minute)
	pending := h.proposeTurn(runtime, 7, "notion_create_page", `{"parent_type":"page","parent_id":"page-groceries","title":"Survivor"}`)
	h.stop()
	h.start()
	if edit := h.nextEdit(); edit.messageID != pending.messageID || !strings.Contains(edit.text, "Expired because the assistant restarted") {
		t.Fatalf("proposal after restart = %+v", edit)
	}
	pendingConfirm, _ := approvalButtons(t, pending.keyboard)
	h.push(callbackUpdate(8, 101, pending.messageID, pendingConfirm))
	h.wantAnswer(8)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Expired because the assistant restarted") {
		t.Fatalf("press after restart rendered %+v", edit)
	}
	if countRequests(sim, "POST", "/v1/pages") != 0 {
		t.Fatal("an expired proposal reached Notion")
	}

	// An ambiguous create failure is never retried and the user is told to check Notion.
	ambiguous := h.proposeTurn(runtime, 9, "notion_create_page", `{"parent_type":"data_source","parent_id":"ds-tasks","title":"Maybe"}`)
	ambiguousConfirm, _ := approvalButtons(t, ambiguous.keyboard)
	sim.FailNext(503, "")
	h.push(callbackUpdate(10, 101, ambiguous.messageID, ambiguousConfirm))
	h.wantAnswer(10)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "check Notion") || strings.Contains(edit.text, "synthetic-notion-error-text") {
		t.Fatalf("ambiguous failure rendered %+v", edit)
	}
	h.continueTurn(runtime, 10)
	if countRequests(sim, "POST", "/v1/pages") != 1 {
		t.Fatalf("create requests after ambiguous failure = %d, want 1", countRequests(sim, "POST", "/v1/pages"))
	}
	if status, reason := h.approvalStatus(approvalID(ambiguousConfirm)); status != approval.StatusFailed || reason != "ambiguous" {
		t.Fatalf("ambiguous approval = %s/%s", status, reason)
	}

	// A transient update failure is retried once.
	update := h.proposeTurn(runtime, 11, "notion_update_page", `{"page_id":"page-groceries","properties":{"Status":{"status":{"name":"Done"}}}}`)
	updateConfirm, _ := approvalButtons(t, update.keyboard)
	sim.FailNext(503, "")
	h.push(callbackUpdate(12, 101, update.messageID, updateConfirm))
	h.wantAnswer(12)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Done. Updated.") {
		t.Fatalf("retried update rendered %+v", edit)
	}
	h.continueTurn(runtime, 12)
	if countRequests(sim, "PATCH", "/v1/pages/page-groceries") != 2 {
		t.Fatalf("update requests = %d, want 2", countRequests(sim, "PATCH", "/v1/pages/page-groceries"))
	}
	if status, _ := h.approvalStatus(approvalID(updateConfirm)); status != approval.StatusExecuted {
		t.Fatalf("retried update approval = %s", status)
	}
	h.assertNotionSecretHidden(aliceNotionToken)
}

// A press that runs the actions continues the request on its own, and a rejection leaves the
// outcome for the user's next turn instead. A press while a turn runs continues once that turn is
// delivered.
func TestConfirmContinuesTheRequestUnlessRejected(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: writableNotionSimulator(t)})
	h.provision("alice", "acct-alice-1111")
	h.installNotionToken("alice", aliceNotionToken)
	h.start()
	h.enableNotionWrites(1, 2, 101)

	rejected := h.proposeTurn(runtime, 3, "notion_append_blocks", `{"page_id":"page-groceries","paragraphs":["Bread"]}`)
	_, reject := approvalButtons(t, rejected.keyboard)
	h.push(callbackUpdate(4, 101, rejected.messageID, reject))
	h.wantAnswer(4)
	h.nextEdit()
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)

	confirmed := h.proposeTurn(runtime, 5, "notion_append_blocks", `{"page_id":"page-groceries","paragraphs":["Eggs"]}`)
	confirm, _ := approvalButtons(t, confirmed.keyboard)
	h.push(callbackUpdate(6, 101, confirmed.messageID, confirm))
	h.wantAnswer(6)
	h.nextEdit()
	h.continueTurn(runtime, 6)

	busy := h.proposeTurn(runtime, 7, "notion_append_blocks", `{"page_id":"page-groceries","paragraphs":["Milk"]}`)
	confirm, _ = approvalButtons(t, busy.keyboard)
	h.push(textUpdate(8, 101, "meanwhile"))
	running := receive(t, h.ctx, runtime.calls)
	h.push(callbackUpdate(9, 101, busy.messageID, confirm))
	h.wantAnswer(9)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Done.") {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	running.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 8, "ok")
	h.waitState(8, "delivered")
	continued := receive(t, h.ctx, runtime.calls)
	if !strings.HasPrefix(continued.request.Message, "[system] The user confirmed your notion_append_blocks proposal and it was executed.") ||
		!strings.HasSuffix(continued.request.Message, prompt.Continue) {
		t.Fatalf("continuation after the busy turn = %q", continued.request.Message)
	}
	continued.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd}
	h.waitState(9, "delivered")
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)
}

func TestProposalsOfOneTurnAreConfirmedTogetherAndRunPastAFailure(t *testing.T) {
	runtime := newControlledRuntime()
	sim := writableNotionSimulator(t)
	sim.SharePage(aliceNotionToken, notionsim.Page{
		ID: "page-chores", Title: "Chores", URL: "https://notion.so/page-chores", LastEdited: "2026-09-05T10:00:00.000Z", DataSourceID: "ds-tasks",
		Properties: map[string]any{"Status": notionsim.StatusProperty("Open")},
	})
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: sim})
	h.provision("alice", "acct-alice-1111")
	h.installNotionToken("alice", aliceNotionToken)
	h.start()
	h.enableNotionWrites(1, 2, 101)

	h.push(textUpdate(3, 101, "close both tasks"))
	call := receive(t, h.ctx, runtime.calls)
	// Outside text makes the updates proposals, which this turn confirms together.
	tools.RunFrom(call.ctx).MarkExternal("")
	for _, page := range []string{"page-groceries", "page-chores"} {
		result, err := call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: "notion_update_page", Arguments: json.RawMessage(
			`{"page_id":"` + page + `","properties":{"Status":{"status":{"name":"Done"}}}}`)})
		if err != nil || result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
			t.Fatalf("proposal for %s = %+v %v", page, result, err)
		}
	}
	call.answer <- agentruntime.RunResult{Response: "proposed both"}
	h.wantReply(101, 3, "proposed both")
	h.waitState(3, "delivered")
	proposal := h.nextMessage()
	for _, want := range []string{"2 changes, confirmed or rejected together:", "1. Notion · update page", "Groceries", "2. Notion · update page", "Chores", "Expires in 10 minutes."} {
		if !strings.Contains(proposal.text, want) {
			t.Fatalf("bundle proposal lacks %q: %s", want, proposal.text)
		}
	}
	select {
	case extra := <-h.messages:
		t.Fatalf("a second proposal message was sent: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
	confirmData, _ := approvalButtons(t, proposal.keyboard)
	bundleID := approvalID(confirmData)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for h.countRows(`SELECT count(*) FROM approvals WHERE bundle_id = ? AND message_id = ?`, bundleID, proposal.messageID) != 2 {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			t.Fatalf("proposal message %d was never recorded for the bundle", proposal.messageID)
		}
	}
	if countRequests(sim, "PATCH", "/v1/pages/page-groceries")+countRequests(sim, "PATCH", "/v1/pages/page-chores") != 0 {
		t.Fatal("a proposal reached Notion before confirmation")
	}

	sim.FailNext(400, "")
	h.push(callbackUpdate(4, 101, proposal.messageID, confirmData))
	h.wantAnswer(4)
	edit := h.nextEdit()
	if edit.messageID != proposal.messageID || len(edit.keyboard) != 0 ||
		!strings.Contains(edit.text, "Failed. Notion rejected the request; nothing was written.") || !strings.Contains(edit.text, "Done. Updated.") ||
		strings.Index(edit.text, "Failed.") > strings.Index(edit.text, "Done.") {
		t.Fatalf("bundle after confirm = %+v", edit)
	}
	if countRequests(sim, "PATCH", "/v1/pages/page-groceries") != 1 || countRequests(sim, "PATCH", "/v1/pages/page-chores") != 1 {
		t.Fatalf("update requests = %d groceries, %d chores, want 1 each", countRequests(sim, "PATCH", "/v1/pages/page-groceries"), countRequests(sim, "PATCH", "/v1/pages/page-chores"))
	}
	var order []string
	for _, request := range sim.Requests() {
		if request.Method == "PATCH" {
			order = append(order, request.Path)
		}
	}
	if strings.Join(order, " ") != "/v1/pages/page-groceries /v1/pages/page-chores" {
		t.Fatalf("update order = %v", order)
	}

	// The turn the confirmation queues answers about the outcome, quoting the proposal.
	informed := receive(t, h.ctx, runtime.calls)
	if !strings.HasPrefix(informed.request.Message, "[system] Your 2 proposals were shown and resolved together. (1) Your notion_update_page proposal was confirmed but failed: ") ||
		!strings.Contains(informed.request.Message, "(2) The user confirmed your notion_update_page proposal and it was executed. Updated.") ||
		!strings.HasSuffix(informed.request.Message, prompt.Continue) {
		t.Fatalf("turn after a bundle = %q", informed.request.Message)
	}
	informed.answer <- agentruntime.RunResult{Response: "one failed"}
	if message := h.nextMessage(); message.chatID != 101 || message.replyTo != proposal.messageID || message.text != "one failed" {
		t.Fatalf("continuation reply = %+v", message)
	}
	h.waitState(4, "delivered")
}

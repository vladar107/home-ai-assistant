package telegram

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/memory"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

func newActMemoryHarness(t *testing.T, runtime *controlledRuntime) *conversationHarness {
	t.Helper()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{providers: []tools.Provider{readingTools{}}})
	h.provision("alice", "acct-alice")
	h.start()
	return h
}

// directRecordOf returns the ID and status of the principal's only direct record of a tool.
func (h *conversationHarness) directRecordOf(tool string) (string, string) {
	h.t.Helper()
	var id, status string
	if err := h.db.QueryRowContext(h.ctx,
		`SELECT id, status FROM approvals WHERE mode = 'direct' AND tool = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, tool,
	).Scan(&id, &status); err != nil {
		h.t.Fatalf("direct record of %s: %v", tool, err)
	}
	return id, status
}

func wantDoneResult(t *testing.T, result agentruntime.ToolResult) {
	t.Helper()
	if result.IsError || !strings.Contains(result.Text, "Done:") || !strings.Contains(result.Text, "Undo button") {
		t.Fatalf("result = %+v, want the change done with an Undo", result)
	}
}

func wantProposedResult(t *testing.T, result agentruntime.ToolResult) {
	t.Helper()
	if result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
		t.Fatalf("result = %+v, want a proposal", result)
	}
}

// An instruction asked for in a clean turn is written at once as the model's own, reported by the
// "Done" message rather than a reply line, and Undo removes it; a replaced instruction is restored
// as it was; and an entry changed since refuses Undo.
func TestMemoryInstructionActsInACleanTurnAndUndoes(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newActMemoryHarness(t, runtime)

	_, results := h.memoryTurn(runtime, 1, 101, "ok", addCall("instruction", "answer briefly"))
	wantDoneResult(t, results[0])
	entries := h.memoryEntries("alice")
	if len(entries) != 1 || entries[0].Kind != memory.KindInstruction || entries[0].Source != memory.SourceModel || entries[0].Text != "answer briefly" {
		t.Fatalf("entries = %+v, want the instruction written as the model's", entries)
	}
	added, status := h.directRecordOf("memory_add")
	done := h.nextMessage()
	if status != approval.StatusDone || !strings.HasPrefix(done.text, "Done:") || !strings.Contains(done.text, "answer briefly") ||
		fmt.Sprint(buttonData(done.keyboard)) != fmt.Sprint([]string{"undo:" + added}) {
		t.Fatalf("done message = %+v with record %s", done, status)
	}
	h.waitMessageRecorded(done.messageID)
	if undid, err := h.approvals.Undo(h.ctx, "alice", added); err != nil || undid.Answer != "Undone." {
		t.Fatalf("undo = %+v %v", undid, err)
	}
	if entries := h.memoryEntries("alice"); len(entries) != 0 {
		t.Fatalf("entries after undo = %+v", entries)
	}
	h.nextEdit()

	old := time.Date(2026, time.September, 1, 8, 0, 0, 0, time.UTC)
	if err := h.store.AddMemory(h.ctx, memory.Entry{ID: "aaaaaaa1", PrincipalID: "alice", Kind: memory.KindInstruction, Text: "answer in English", Source: memory.SourceConfirmed, CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	_, results = h.memoryTurn(runtime, 2, 101, "ok", memoryCall("memory_replace", `{"id":"aaaaaaa1","text":"answer in Dutch"}`))
	wantDoneResult(t, results[0])
	if entries := h.memoryEntries("alice"); len(entries) != 1 || entries[0].Text != "answer in Dutch" || entries[0].Source != memory.SourceModel {
		t.Fatalf("entries after replace = %+v", entries)
	}
	replaced, _ := h.directRecordOf("memory_replace")
	done = h.nextMessage()
	h.waitMessageRecorded(done.messageID)
	if undid, err := h.approvals.Undo(h.ctx, "alice", replaced); err != nil || undid.Answer != "Undone." {
		t.Fatalf("undo of the replace = %+v %v", undid, err)
	}
	if entries := h.memoryEntries("alice"); len(entries) != 1 || entries[0].Text != "answer in English" || entries[0].Source != memory.SourceConfirmed || !entries[0].UpdatedAt.Equal(old) {
		t.Fatalf("entries after undoing the replace = %+v, want the old entry back", entries)
	}
	h.nextEdit()

	_, results = h.memoryTurn(runtime, 3, 101, "ok", addCall("instruction", "use metric units"))
	wantDoneResult(t, results[0])
	changed, _ := h.directRecordOf("memory_add")
	h.nextMessage()
	var id string
	for _, entry := range h.memoryEntries("alice") {
		if entry.Text == "use metric units" {
			id = entry.ID
		}
	}
	if _, err := h.store.ReplaceMemory(h.ctx, "alice", id, "use imperial units", memory.SourceModel, time.Now()); err != nil {
		t.Fatal(err)
	}
	if undid, err := h.approvals.Undo(h.ctx, "alice", changed); err != nil || undid.Answer != "Not undone: that entry changed since." {
		t.Fatalf("undo of a changed entry = %+v %v", undid, err)
	}
	if entries := h.memoryEntries("alice"); len(entries) != 2 {
		t.Fatalf("entries after a refused undo = %+v", entries)
	}
}

// An instruction proposes after household content, which memory's stricter rule counts, and after
// outside content.
func TestMemoryInstructionProposesAfterHouseholdOrOutsideContent(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newActMemoryHarness(t, runtime)

	_, results := h.memoryTurn(runtime, 1, 101, "ok", memoryCall("home_read", `{}`), addCall("instruction", "answer briefly"))
	wantProposedResult(t, results[1])
	proposal := h.nextMessage()
	if !strings.HasPrefix(proposal.text, "Remember this instruction") {
		t.Fatalf("proposal = %+v", proposal)
	}
	approvalButtons(t, proposal.keyboard)

	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)
	_, results = h.memoryTurn(runtime, 3, 101, "ok", memoryCall("outside_read", `{}`), addCall("instruction", "answer in English"))
	wantProposedResult(t, results[1])
	h.nextMessage()
	for _, entry := range h.memoryEntries("alice") {
		if entry.Kind == memory.KindInstruction {
			t.Fatalf("an instruction was written after third-party content: %+v", entry)
		}
	}
	if count := h.countRows(`SELECT count(*) FROM approvals WHERE mode = 'direct'`); count != 0 {
		t.Fatalf("direct records = %d, want none", count)
	}
}

// The lesson of a turn a 👎 started stays a labelled proposal, however clean the conversation.
func TestReconsiderLessonStillProposes(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newActMemoryHarness(t, runtime)

	reconsider := h.thumbsDown(runtime, 1, 2)
	wantProposedResult(t, callTool(t, reconsider, addCall("instruction", "Use metric units for distances.")))
	reconsider.answer <- agentruntime.RunResult{Response: "Sorry, about 3 km."}
	h.nextMessage()
	h.waitState(2, "delivered")
	proposal := h.nextMessage()
	if !strings.HasPrefix(proposal.text, learnedPrefix) || !strings.Contains(proposal.text, "Use metric units for distances.") {
		t.Fatalf("lesson card = %+v", proposal)
	}
	approvalButtons(t, proposal.keyboard)
	if entries := h.memoryEntries("alice"); len(entries) != 0 {
		t.Fatalf("entries before confirm = %+v", entries)
	}
	if count := h.countRows(`SELECT count(*) FROM approvals WHERE mode = 'direct'`); count != 0 {
		t.Fatalf("direct records = %d, want none", count)
	}
}

func newActNotionHarness(t *testing.T) (*conversationHarness, *controlledRuntime, *notionsim.Simulator) {
	t.Helper()
	runtime := newControlledRuntime()
	sim := writableNotionSimulator(t)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: sim, providers: []tools.Provider{readingTools{}}})
	h.provision("alice", "acct-alice-1111")
	h.installNotionToken("alice", aliceNotionToken)
	h.start()
	h.enableNotionWrites(1, 2, 101)
	return h, runtime, sim
}

// pageStatus is the Status a simulated page holds.
func pageStatus(t *testing.T, sim *notionsim.Simulator, id string) string {
	t.Helper()
	for _, page := range sim.Pages() {
		if page.ID != id {
			continue
		}
		property, _ := page.Properties["Status"].(map[string]any)
		option, _ := property["status"].(map[string]any)
		name, _ := option["name"].(string)
		return name
	}
	t.Fatalf("no page %s", id)
	return ""
}

// editInNotion changes a page the way another Notion user would.
func editInNotion(t *testing.T, sim *notionsim.Simulator, pageID, body string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, sim.URL()+"/v1/pages/"+pageID, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+aliceNotionToken)
	request.Header.Set("Notion-Version", notionsim.Version)
	request.Header.Set("Content-Type", "application/json")
	response, err := sim.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("edit in Notion = %d", response.StatusCode)
	}
}

// A property update asked for in a clean turn is made at once, and Undo writes back the value it
// replaced, unless someone changed the property in Notion since.
func TestNotionUpdateActsInACleanTurnAndUndoWritesBackThePreviousValue(t *testing.T) {
	t.Parallel()
	h, runtime, sim := newActNotionHarness(t)

	h.push(textUpdate(3, 101, "mark groceries done"))
	call := receive(t, h.ctx, runtime.calls)
	wantDoneResult(t, executeTool(t, call.ctx, call.request.Tools, "notion_update_page", `{"page_id":"page-groceries","properties":{"Status":{"status":{"name":"Done"}}}}`))
	if status := pageStatus(t, sim, "page-groceries"); status != "Done" {
		t.Fatalf("status after the update = %q", status)
	}
	h.finishTurn(call, 3, "done")
	id, status := h.directRecordOf("notion_update_page")
	done := h.nextMessage()
	if status != approval.StatusDone || !strings.Contains(done.text, "Notion · update page") || !strings.Contains(done.text, "Status: Open → Done") ||
		fmt.Sprint(buttonData(done.keyboard)) != fmt.Sprint([]string{"undo:" + id}) {
		t.Fatalf("done message = %+v with record %s", done, status)
	}
	h.waitMessageRecorded(done.messageID)

	if undid, err := h.approvals.Undo(h.ctx, "alice", id); err != nil || undid.Answer != "Undone." {
		t.Fatalf("undo = %+v %v", undid, err)
	}
	if status := pageStatus(t, sim, "page-groceries"); status != "Open" {
		t.Fatalf("status after undo = %q, want the previous value", status)
	}
	var restore notionsim.Request
	for _, request := range sim.Requests() {
		if request.Method == http.MethodPatch && request.Path == "/v1/pages/page-groceries" {
			restore = request
		}
	}
	properties, _ := restore.Body["properties"].(map[string]any)
	if fmt.Sprint(properties) != fmt.Sprint(map[string]any{"Status": map[string]any{"status": map[string]any{"name": "Open"}}}) || restore.Token != aliceNotionToken {
		t.Fatalf("undo request = %+v", restore)
	}
	h.nextEdit()

	h.push(textUpdate(4, 101, "mark groceries done again"))
	call = receive(t, h.ctx, runtime.calls)
	wantDoneResult(t, executeTool(t, call.ctx, call.request.Tools, "notion_update_page", `{"page_id":"page-groceries","properties":{"Status":{"status":{"name":"Done"}}}}`))
	h.finishTurn(call, 4, "done again")
	changed, _ := h.directRecordOf("notion_update_page")
	h.nextMessage()
	editInNotion(t, sim, "page-groceries", `{"properties":{"Status":{"status":{"name":"Open"}}}}`)
	patches := countRequests(sim, http.MethodPatch, "/v1/pages/page-groceries")
	if undid, err := h.approvals.Undo(h.ctx, "alice", changed); err != nil || undid.Answer != "Not undone: changed in Notion since." {
		t.Fatalf("undo of a changed page = %+v %v", undid, err)
	}
	if countRequests(sim, http.MethodPatch, "/v1/pages/page-groceries") != patches {
		t.Fatal("a refused undo wrote to Notion")
	}
}

// After outside content the update proposes, and creating pages or appending to them always
// proposes, in a clean turn too.
func TestNotionCreateAndAppendStillProposeAndUpdatesProposeAfterOutsideContent(t *testing.T) {
	t.Parallel()
	h, runtime, sim := newActNotionHarness(t)

	h.push(textUpdate(3, 101, "add a task and a note"))
	call := receive(t, h.ctx, runtime.calls)
	wantProposedResult(t, executeTool(t, call.ctx, call.request.Tools, "notion_create_page", `{"parent_type":"data_source","parent_id":"ds-tasks","title":"Buy milk"}`))
	wantProposedResult(t, executeTool(t, call.ctx, call.request.Tools, "notion_append_blocks", `{"page_id":"page-groceries","paragraphs":["Bread"]}`))
	executeTool(t, call.ctx, call.request.Tools, "outside_read", `{}`)
	wantProposedResult(t, executeTool(t, call.ctx, call.request.Tools, "notion_update_page", `{"page_id":"page-groceries","properties":{"Status":{"status":{"name":"Done"}}}}`))
	h.finishTurn(call, 3, "proposed")
	bundle := h.nextMessage()
	if !strings.Contains(bundle.text, "3 changes") || !strings.Contains(bundle.text, "create page") || !strings.Contains(bundle.text, "append") || !strings.Contains(bundle.text, "update page") {
		t.Fatalf("bundle = %+v", bundle)
	}
	approvalButtons(t, bundle.keyboard)
	if h.countRows(`SELECT count(*) FROM approvals WHERE mode = 'direct'`) != 0 || countRequests(sim, http.MethodPatch, "/v1/pages/page-groceries") != 0 ||
		countRequests(sim, http.MethodPost, "/v1/pages") != 0 {
		t.Fatal("a write reached Notion before confirmation")
	}
}

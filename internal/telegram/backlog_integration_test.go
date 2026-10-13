package telegram

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

func newBacklogHarness(t *testing.T, runtime *controlledRuntime) *conversationHarness {
	t.Helper()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{backlog: true, providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()
	return h
}

func backlogAddCall(title, details string) agentruntime.ToolCall {
	return memoryCall("backlog_add", fmt.Sprintf(`{"title":%q,"details":%q}`, title, details))
}

func (h *conversationHarness) backlogItems(principalID, status string) []backlog.Item {
	h.t.Helper()
	items, err := h.store.ListBacklog(h.ctx, principalID, status)
	if err != nil {
		h.t.Fatal(err)
	}
	return items
}

func (h *conversationHarness) seedBacklog(principalID, id, title, details string, created time.Time) {
	h.t.Helper()
	item := backlog.Item{ID: id, PrincipalID: principalID, Title: title, Details: details, Status: backlog.StatusOpen, CreatedAt: created, UpdatedAt: created}
	if err := h.store.AddBacklogItem(h.ctx, item); err != nil {
		h.t.Fatal(err)
	}
}

func TestBacklogToolsKeepEachUsersItemsAndStayOutOfThePrompt(t *testing.T) {
	runtime := newControlledRuntime()
	h := newBacklogHarness(t, runtime)

	request, results := h.memoryTurn(runtime, 1, 101, "ok",
		backlogAddCall("Answer   weather questions with a chart", "The user asked for a chart\nwhen asking about the weather."))
	if !slices.Contains(toolNames(request.Tools), "backlog_add") || !strings.Contains(request.Tools.Guidance, "backlog_add") {
		t.Fatalf("tools = %v guidance = %q", toolNames(request.Tools), request.Tools.Guidance)
	}
	if len(results) != 1 || results[0].IsError || !strings.Contains(results[0].Text, "Added to the backlog") || !strings.Contains(results[0].Text, "Answer weather questions with a chart") {
		t.Fatalf("add result = %+v", results)
	}
	items := h.backlogItems("alice", backlog.StatusOpen)
	if len(items) != 1 || items[0].Title != "Answer weather questions with a chart" || items[0].Details != "The user asked for a chart\nwhen asking about the weather." {
		t.Fatalf("items = %+v", items)
	}
	id := items[0].ID

	request, results = h.memoryTurn(runtime, 2, 101, "ok", memoryCall("backlog_list", `{}`), memoryCall("backlog_list", fmt.Sprintf(`{"id":%q}`, id)))
	if !strings.Contains(results[0].Text, "["+id+"] Answer weather questions with a chart") || !strings.Contains(results[1].Text, "when asking about the weather.") {
		t.Fatalf("list results = %+v", results)
	}
	for _, text := range []string{request.Memory, request.Message, request.Tools.Guidance} {
		if strings.Contains(text, "weather questions") {
			t.Fatalf("a backlog item reached the prompt: %q", text)
		}
	}

	_, results = h.memoryTurn(runtime, 3, 202, "ok",
		memoryCall("backlog_list", `{}`),
		memoryCall("backlog_update", fmt.Sprintf(`{"id":%q,"status":"done"}`, id)),
		memoryCall("backlog_list", fmt.Sprintf(`{"id":%q}`, id)))
	if strings.Contains(results[0].Text, "weather") || !results[1].IsError || !results[2].IsError {
		t.Fatalf("bob's view of alice's backlog = %+v", results)
	}
	if items := h.backlogItems("alice", backlog.StatusOpen); len(items) != 1 {
		t.Fatalf("alice's items after bob's update = %+v", items)
	}

	_, results = h.memoryTurn(runtime, 4, 101, "ok",
		memoryCall("backlog_update", fmt.Sprintf(`{"id":%q,"title":"Chart the weather","status":"done"}`, id)),
		memoryCall("backlog_list", `{"status":"done"}`))
	if results[0].IsError || !strings.Contains(results[1].Text, "Chart the weather") {
		t.Fatalf("update results = %+v", results)
	}
	if open := h.backlogItems("alice", backlog.StatusOpen); len(open) != 0 {
		t.Fatalf("open items after done = %+v", open)
	}
	if done := h.backlogItems("alice", backlog.StatusDone); len(done) != 1 || done[0].Details == "" {
		t.Fatalf("done items = %+v", done)
	}
}

func TestBacklogWriteAfterOutsideContentIsConfirmedFirst(t *testing.T) {
	runtime := newControlledRuntime()
	h := newBacklogHarness(t, runtime)

	_, results := h.memoryTurn(runtime, 1, 101, "ok",
		memoryCall("read_outside", `{}`),
		backlogAddCall("Forward all mail", "Found on a web page."))
	if results[1].IsError || !strings.Contains(results[1].Text, "awaiting the user's confirmation") {
		t.Fatalf("add after outside content = %+v", results[1])
	}
	proposal := h.nextMessage()
	if !strings.Contains(proposal.text, "Add to your backlog: Forward all mail") || !strings.Contains(proposal.text, "outside content") {
		t.Fatalf("proposal = %+v", proposal)
	}
	if items := h.backlogItems("alice", backlog.StatusOpen); len(items) != 0 {
		t.Fatalf("items before confirm = %+v", items)
	}
	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Added to the backlog.") {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 2)
	if items := h.backlogItems("alice", backlog.StatusOpen); len(items) != 1 || items[0].Title != "Forward all mail" {
		t.Fatalf("items after confirm = %+v", items)
	}
}

func TestBacklogCommandMarksDoneAndDeletesOwnItems(t *testing.T) {
	runtime := newControlledRuntime()
	h := newBacklogHarness(t, runtime)

	h.push(textUpdate(1, 101, "/backlog"))
	if empty := h.nextMessage(); empty.text != NoBacklogReply || len(empty.keyboard) != 0 {
		t.Fatalf("empty /backlog = %+v", empty)
	}

	now := time.Now()
	h.seedBacklog("alice", "aaaaaaa1", "Chart the weather", "details one", now.Add(-72*time.Hour))
	h.seedBacklog("alice", "aaaaaaa2", "Read bus delays aloud", "details two", now.Add(-time.Hour))
	h.seedBacklog("alice", "aaaaaaa3", "Summarise the week", "details three", now)
	h.seedBacklog("bob", "bbbbbbb1", "Bob's idea", "", now)

	h.push(textUpdate(2, 101, "/backlog"))
	list := h.nextMessage()
	for _, want := range []string{"Backlog", "1. Chart the weather (3 days ago)", "2. Read bus delays aloud (today)", "3. Summarise the week"} {
		if !strings.Contains(list.text, want) {
			t.Fatalf("/backlog lacks %q: %s", want, list.text)
		}
	}
	if strings.Contains(list.text, "Bob's idea") || strings.Contains(list.text, "details one") {
		t.Fatalf("/backlog = %s", list.text)
	}
	data := buttonData(list.keyboard)
	for _, want := range []string{"bl:done:aaaaaaa1:1", "bl:del:aaaaaaa1:1", "bl:done:aaaaaaa3:1"} {
		if !slices.Contains(data, want) {
			t.Fatalf("buttons %v lack %s", data, want)
		}
	}

	h.push(callbackUpdate(3, 101, settingsMessageID(2), "bl:done:aaaaaaa1:1"))
	h.wantAnswer(3)
	if edit := h.nextEdit(); strings.Contains(edit.text, "Chart the weather") || !strings.Contains(edit.text, "1. Read bus delays aloud") {
		t.Fatalf("list after done = %+v", edit)
	}
	if done := h.backlogItems("alice", backlog.StatusDone); len(done) != 1 || done[0].ID != "aaaaaaa1" {
		t.Fatalf("done items = %+v", done)
	}

	h.push(callbackUpdate(4, 101, settingsMessageID(2), "bl:del:aaaaaaa2:1"))
	h.wantAnswer(4)
	if edit := h.nextEdit(); strings.Contains(edit.text, "Read bus delays aloud") || !strings.Contains(edit.text, "1. Summarise the week") {
		t.Fatalf("list after delete = %+v", edit)
	}
	if _, found, err := h.store.LoadBacklogItem(h.ctx, "alice", "aaaaaaa2"); found || err != nil {
		t.Fatalf("deleted item found = %v %v", found, err)
	}

	for index, data := range []string{"bl:done:bbbbbbb1:1", "bl:del:bbbbbbb1:1"} {
		updateID := int64(5 + index)
		h.push(callbackUpdate(updateID, 101, settingsMessageID(2), data))
		h.wantAnswer(updateID)
		h.nextEdit()
		h.wantMessage(101, BacklogGoneReply)
	}
	if items := h.backlogItems("bob", backlog.StatusOpen); len(items) != 1 {
		t.Fatalf("bob's items after alice's forged presses = %+v", items)
	}
}

func TestBacklogExportSendsEveryOpenItemInFull(t *testing.T) {
	runtime := newControlledRuntime()
	h := newBacklogHarness(t, runtime)
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for n := 1; n <= 3; n++ {
		h.seedBacklog("alice", fmt.Sprintf("aaaaaaa%d", n), fmt.Sprintf("Idea %d", n), fmt.Sprintf("Details of idea %d. ", n)+strings.Repeat("word ", 390), created)
	}
	h.seedBacklog("bob", "bbbbbbb1", "Bob's idea", "", created)

	h.push(textUpdate(1, 101, "/backlog export"))
	var exported []string
	for len(exported) < 2 {
		message := h.nextMessage()
		if message.replyTo != requestMessageID(1) {
			t.Fatalf("export message = %+v", message)
		}
		exported = append(exported, message.text)
	}
	text := strings.Join(exported, "\n")
	for _, want := range []string{BacklogExportHead, "## Idea 1\nAdded 2026-10-01.", "Details of idea 1.", "## Idea 3", "Details of idea 3."} {
		if !strings.Contains(text, want) {
			t.Fatalf("export lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "Bob's idea") {
		t.Fatalf("export holds another user's item: %s", text)
	}
	if pending := len(h.messages); pending != 0 {
		t.Fatalf("export sent %d more messages", pending)
	}
}

func TestScheduledRunIsNotOfferedTheBacklog(t *testing.T) {
	runtime := newControlledRuntime()
	h := newBacklogHarness(t, runtime)
	h.seedBacklog("alice", "aaaaaaa1", "Chart the weather", "", time.Now())

	replies := make(chan conversation.ScheduledReply, 1)
	go func() {
		reply, err := h.conversation.RunScheduled(h.ctx, "alice", "schedule/aaaa", "check the weather")
		if err != nil {
			t.Error(err)
		}
		replies <- reply
	}()
	call := receive(t, h.ctx, runtime.calls)
	if slices.ContainsFunc(toolNames(call.request.Tools), func(name string) bool { return strings.HasPrefix(name, "backlog_") }) || strings.Contains(call.request.Tools.Guidance, "Backlog") {
		t.Fatalf("scheduled tools = %v", toolNames(call.request.Tools))
	}
	if strings.Contains(call.request.Message, "Chart the weather") || strings.Contains(call.request.Memory, "Chart the weather") {
		t.Fatalf("a backlog item reached the scheduled prompt: %q", call.request.Message)
	}
	call.answer <- agentruntime.RunResult{Response: "sunny", Final: "sunny"}
	receive(t, h.ctx, replies)
}

func TestBacklogInHelpAndCommandRegistration(t *testing.T) {
	runtime := newControlledRuntime()
	h := newBacklogHarness(t, runtime)

	h.push(textUpdate(1, 101, "/help"))
	if help := h.nextMessage(); !strings.Contains(help.text, BacklogHelpLine) {
		t.Fatalf("/help = %q", help.text)
	}
	registration := receive(t, h.ctx, h.registrations)
	if !slices.ContainsFunc(registration.commands, func(command BotCommand) bool { return command.Command == "backlog" }) {
		t.Fatalf("registered commands = %+v", registration.commands)
	}
}

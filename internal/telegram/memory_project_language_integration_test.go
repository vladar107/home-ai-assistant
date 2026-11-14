package telegram

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/memory"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// memprojAllowed are the Latin-letter words the Russian texts below may hold: commands, and the
// values the test itself supplies.
var memprojAllowed = []string{"/memory", "memory", "close", "tasks", "/close", "/tasks", "Notion", "NAS", "id", "dentist", "Find", "a", "Mail", "Go", "through", "my", "mail", "and", "list", "what", "needs", "action", "answer", "briefly", "likes", "tea", "Plan", "train", "trip", "Look", "up", "the", "departures", "first", "Then", "platform", "Ask", "neighbours", "for", "names", "Call", "clinic", "A", "near", "home", "taking", "new", "patients", "due", "Use", "metric", "units", "Answer", "Keep", "answers", "short", "Rule", "Rule2", "Reply", "English", "Pick", "date", "morning", "Asked", "twice", "https", "example", "com"}

func newMemprojHarness(t *testing.T, runtime *controlledRuntime) (*conversationHarness, *testClock) {
	t.Helper()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{
		languages: true, schedules: clock, tasks: testTasks(), providers: []tools.Provider{outsideProvider{}},
	})
	h.provision("alice", "account-alice")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	return h, clock
}

var memprojID = regexp.MustCompile(`\(id [0-9a-f]+\)`)

func memprojRequireRussian(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, phrase := range want {
		if !strings.Contains(text, phrase) {
			t.Fatalf("text lacks %q:\n%s", phrase, text)
		}
	}
	if latin := localetest.Latin(memprojID.ReplaceAllString(text, "(id)"), memprojAllowed...); len(latin) != 0 {
		t.Fatalf("text holds Latin words %q:\n%s", latin, text)
	}
}

func memprojModelResult(t *testing.T, result agentruntime.ToolResult) {
	t.Helper()
	if result.IsError || cyrillic.MatchString(result.Text) {
		t.Fatalf("the model's result must be English and not an error: %+v", result)
	}
}

// After outside content, a memory entry, a procedure, a project and a task are proposed to a Russian
// user in Russian, confirmed with Russian outcomes, while what the model read stays English.
func TestMemoryProcedureProjectAndTaskProposalsReadInRussian(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h, _ := newMemprojHarness(t, runtime)

	h.push(textUpdate(1, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	run := func(name, arguments string) agentruntime.ToolResult {
		result := h.callTool(call, name, arguments)
		memprojModelResult(t, result)
		return result
	}
	run("read_outside", `{}`)
	run("memory_add", `{"kind":"instruction","text":"answer briefly"}`)
	run("memory_add", `{"kind":"fact","text":"likes tea"}`)
	run("procedure_save", `{"title":"Plan a train trip","steps":"Look up the departures first.\nThen the platform."}`)
	run("project_create", dentistProject)
	run("task_start", `{"title":"Mail","brief":"Go through my mail and list what needs action."}`)
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 1, "ok")
	h.waitState(1, "delivered")

	proposal := h.nextMessage()
	memprojRequireRussian(t, proposal.text,
		"Запомнить эту инструкцию: \"answer briefly\"",
		"Она будет действовать во всех следующих разговорах",
		"Запомнить этот факт: \"likes tea\"",
		"перед сохранением нужно твоё подтверждение",
		"Сохранить эту процедуру: Plan a train trip",
		"Я буду читать её перед похожими просьбами",
		"Начать проект Find a dentist\nЦель: A dentist near home taking new patients\nПункты:\n- Ask neighbours for names\n- Call the clinic (срок: 2026-10-01)",
		"Запустить фоновую задачу \"Mail\" с таким заданием:",
	)
	if strings.Contains(proposal.text, "Remember this") || strings.Contains(proposal.text, "Start the") {
		t.Fatalf("the proposal holds English fragments:\n%s", proposal.text)
	}
	if pending, err := h.store.PendingApprovals(h.ctx, "alice", 0); err != nil || len(pending) != 5 {
		t.Fatalf("pending approvals = %d %v", len(pending), err)
	}

	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	// The started task's own status message is edited too, in either order.
	edit := h.nextEdit()
	if !strings.Contains(edit.text, "Сохранено.") {
		edit = h.nextEdit()
	}
	memprojRequireRussian(t, edit.text, "Сохранено.", "Процедура сохранена.", "Проект начат (id ", "Фоновая задача \"Mail\" запущена.")
}

// A direct change and its Undo read in Russian: a memory instruction is forgotten again, and a
// started project is abandoned.
func TestMemoryAndProjectDirectChangesAndUndoReadInRussian(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h, _ := newMemprojHarness(t, runtime)

	h.push(textUpdate(1, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	memprojModelResult(t, h.callTool(call, "memory_add", `{"kind":"instruction","text":"answer briefly"}`))
	memprojModelResult(t, h.callTool(call, "project_create", dentistProject))
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 1, "ok")
	h.waitState(1, "delivered")

	remembered, _ := h.directRecordOf("memory_add")
	started, _ := h.directRecordOf("project_create")
	done := h.nextMessage()
	memprojRequireRussian(t, done.text, "Готово, 2 изменения", "Начать проект Find a dentist", "Отменить можно в течение 24 часов.")
	h.waitMessageRecorded(done.messageID)

	undid, err := h.approvals.Undo(h.ctx, "alice", remembered)
	if err != nil || !undid.Owned {
		t.Fatalf("undo = %+v %v", undid, err)
	}
	memprojRequireRussian(t, h.nextEdit().text, "Отменено.")
	if entries := h.memoryEntries("alice"); len(entries) != 0 {
		t.Fatalf("entries after undo = %+v", entries)
	}

	undid, err = h.approvals.Undo(h.ctx, "alice", started)
	if err != nil || !undid.Owned {
		t.Fatalf("undo = %+v %v", undid, err)
	}
	h.nextEdit()

	for id, want := range map[string][2]string{
		remembered: {"Forgot: answer briefly", "Забыто: answer briefly"},
		started:    {"Abandoned the project Find a dentist; close its tab with /close.", "Проект Find a dentist заброшен; закрой его вкладку командой /close."},
	} {
		var outcome, userOutcome string
		if err := h.db.QueryRowContext(h.ctx, `SELECT outcome, user_outcome FROM approvals WHERE id = ?`, id).Scan(&outcome, &userOutcome); err != nil {
			t.Fatal(err)
		}
		if outcome != want[0] || userOutcome != want[1] {
			t.Fatalf("stored outcome = %q, user outcome = %q, want %q", outcome, userOutcome, want)
		}
	}
}

// Changes to what is already kept, and a project's edits, are proposed in Russian, with the status
// and the kind of a link in words, and the stale-entry refusal after Confirm reads in Russian too.
func TestMemoryAndProjectChangeProposalsReadInRussian(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h, _ := newMemprojHarness(t, runtime)
	h.seedMemory("alice", "aaaaaaa1", memory.KindInstruction, "Answer briefly.")
	h.seedMemory("alice", "aaaaaaa2", memory.KindFact, "likes tea")

	h.push(textUpdate(1, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	memprojModelResult(t, h.callTool(call, "project_create", dentistProject))
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 1, "ok")
	h.waitState(1, "delivered")
	h.waitMessageRecorded(h.nextMessage().messageID)
	var projectID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM projects WHERE principal_id = 'alice'`).Scan(&projectID); err != nil {
		t.Fatal(err)
	}

	h.push(textUpdate(2, 101, "hello again"))
	call = receive(t, h.ctx, runtime.calls)
	for _, step := range [][2]string{
		{"read_outside", `{}`},
		{"memory_replace", `{"id":"aaaaaaa1","text":"Keep answers short."}`},
		{"memory_remove", `{"id":"aaaaaaa2"}`},
		{"project_update", `{"project_id":"` + projectID + `","goal":"","status":"paused"}`},
		{"project_item_add", `{"project_id":"` + projectID + `","items":[{"text":"Pick a date","due_on":"2026-10-05","note":"morning"}]}`},
		{"project_link", `{"project_id":"` + projectID + `","kind":"url","reference":"https://example.com/clinic"}`},
	} {
		memprojModelResult(t, h.callTool(call, step[0], step[1]))
	}
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 2, "ok")
	h.waitState(2, "delivered")

	proposal := h.nextMessage()
	memprojRequireRussian(t, proposal.text,
		"Изменить запомненную инструкцию\nбыло: \"Answer briefly.\"\nстанет: \"Keep answers short.\"",
		"Забыть запомненный факт: \"likes tea\"",
		"Изменить проект Find a dentist:\n- цель → нет\n- статус → на паузе",
		"Добавить в проект Find a dentist:\n- Pick a date (срок: 2026-10-05): morning",
		"Привязать проект Find a dentist: веб-адрес https://example.com/clinic",
	)

	if _, err := h.store.ReplaceMemory(h.ctx, "alice", "aaaaaaa2", "likes coffee", memory.SourceModel, time.Now()); err != nil {
		t.Fatal(err)
	}
	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(3, 101, proposal.messageID, confirm))
	h.wantAnswer(3)
	memprojRequireRussian(t, h.nextEdit().text, "Запись с тех пор изменилась, ничего не удалено.", "Изменено.", "Добавлено.", "Привязано.")
}

// A weekly review's batch heading, its cards and its report read in Russian for a Russian user,
// although the unattended run starts without a language; the review's prompt stays English.
func TestWeeklyReviewBatchReadsInRussian(t *testing.T) {
	t.Parallel()
	runtime := reviewRuntime{controlledRuntime: newControlledRuntime(), dir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(runtime.dir, "alice"), 0o700); err != nil {
		t.Fatal(err)
	}
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, topics: true, languages: true})
	h.provision("alice", "account-alice")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.seedMemory("alice", "aaaaaaa1", memory.KindInstruction, "Answer briefly.")
	h.seedMemory("alice", "aaaaaaa2", memory.KindInstruction, "Keep answers short.")
	h.push(textUpdate(1, 101, "/settings"))
	h.nextMessage()
	for index, data := range []string{"nav:memory", "memory:review_on", "memory:review_now"} {
		updateID := int64(2 + index)
		h.push(callbackUpdate(updateID, 101, settingsMessageID(1), data))
		h.wantAnswer(updateID)
		h.nextEdit()
	}

	review := receive(t, h.ctx, runtime.calls)
	if cyrillic.MatchString(review.request.Message) {
		t.Fatalf("the review's prompt carries Russian: %q", review.request.Message)
	}
	reports := h.wantFeedTab("Reports", thread.FeedReports)
	answer(t, review, []map[string]any{
		{"action": "add_instruction", "reason": "Asked twice.", "text": "Use metric units."},
		{"action": "merge_instructions", "ids": []string{"aaaaaaa1", "aaaaaaa2"}, "text": "Answer briefly."},
		{"action": "save_procedure", "title": "Plan a train trip", "steps": "Look up the departures first."},
	}, nil)

	card := h.nextMessage()
	if card.threadID != reports {
		t.Fatalf("review card = %+v, want it in tab %d", card, reports)
	}
	memprojRequireRussian(t, card.text,
		"Еженедельный обзор",
		"Из твоего еженедельного обзора: Asked twice.\nЗапомнить эту инструкцию: \"Use metric units.\"",
		"Из твоего еженедельного обзора:\nОбъединить эти инструкции в одну:\n- \"Answer briefly.\"\n- \"Keep answers short.\"\nбудет: \"Answer briefly.\"",
		"Из твоего еженедельного обзора:\nСохранить эту процедуру: Plan a train trip",
	)
	if strings.Contains(card.text, "weekly review") || strings.Contains(card.text, "Weekly review") {
		t.Fatalf("the card holds English:\n%s", card.text)
	}
	if report := h.nextMessage().text; !strings.Contains(report, "Твой еженедельный обзор выше: 3 предложения, которые можно подтвердить или отклонить.") {
		t.Fatalf("report = %q", report)
	}
	if count := h.countRows(`SELECT count(*) FROM approvals WHERE tool = 'memory_add' AND title = ?`, "Запомнить эту инструкцию: \"Use metric units.\""); count != 1 {
		t.Fatalf("the card's stored title is not the Russian summary line: %d", count)
	}
}

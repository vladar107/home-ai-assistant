package telegram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/memory"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// searchmemAllowed are the Latin-letter words the Russian texts below may hold: the values the
// test supplies.
var searchmemAllowed = []string{"https", "example", "com", "page", "id", "Answer", "briefly", "Keep", "answers", "short", "and", "it", "Use", "hour", "times", "prefers", "metric", "units", "code", "see"}

func searchmemRequire(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, phrase := range want {
		if !strings.Contains(text, phrase) {
			t.Fatalf("text lacks %q:\n%s", phrase, text)
		}
	}
	if latin := localetest.Latin(text, searchmemAllowed...); len(latin) != 0 {
		t.Fatalf("text holds Latin words %q:\n%s", latin, text)
	}
}

// A search the check holds after outside content is proposed to a Russian user in Russian and its
// confirmation reads in Russian, while the model is told in English.
func TestHeldSearchProposalAndConfirmationReadInRussian(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newSearchHarness(t, runtime, harnessOptions{languages: true})
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}

	decisions := h.searchTurn(runtime, 1, true, heldQuery)
	wantDecision(t, "held query", decisions[0], agentruntime.SearchConfirm, "pattern")
	if strings.ContainsAny(decisions[0].Text, "абвгдеёжзийклмнопрстуфхцчшщъыьэюя") || !strings.HasPrefix(decisions[0].Text, "The search was not run: it contains a web address") {
		t.Fatalf("the model's decision = %q", decisions[0].Text)
	}
	proposal := h.nextMessage()
	searchmemRequire(t, proposal.text, "Искать в интернете: ", heldQuery, "Нужно твоё подтверждение, потому что в нём есть веб-адрес или другой идентификатор.")

	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(3, 101, proposal.messageID, confirm))
	h.wantAnswer(3)
	searchmemRequire(t, h.nextEdit().text, "Подтверждено; поиск выполняется.")
	var outcome, userOutcome string
	if err := h.db.QueryRowContext(h.ctx, `SELECT outcome, user_outcome FROM approvals WHERE tool = 'web_search'`).Scan(&outcome, &userOutcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "Approved; the search runs now." || userOutcome != "Подтверждено; поиск выполняется." {
		t.Fatalf("stored outcome = %q, user outcome = %q", outcome, userOutcome)
	}
}

func newSearchmemReviewHarness(t *testing.T) *reviewHarness {
	t.Helper()
	runtime := reviewRuntime{controlledRuntime: newControlledRuntime(), dir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(runtime.dir, "alice"), 0o700); err != nil {
		t.Fatal(err)
	}
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, topics: true, languages: true})
	h.provision("alice", "account-alice")
	h.start()
	h.requestSettings(1, 101)
	h.openSettingsPage(2, 101, settingsMessageID(1), "memory")
	review := &reviewHarness{conversationHarness: h, runtime: runtime, clock: clock, settings: settingsMessageID(1)}
	review.seedMemory("alice", "aaaaaaa1", memory.KindInstruction, "Answer briefly.")
	review.seedMemory("alice", "aaaaaaa2", memory.KindInstruction, "Keep answers short.")
	review.turnOn(10)
	return review
}

// A confirmed merge of instructions reads in Russian, and so does its refusal when one of them
// changed since the card was sent.
func TestMemoryMergeOutcomesReadInRussian(t *testing.T) {
	t.Parallel()
	h := newSearchmemReviewHarness(t)
	review := h.reviewNow(11)
	reports := h.wantFeedTab("Reports", thread.FeedReports)
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	merge := []map[string]any{{"action": "merge_instructions", "reason": "They say the same.", "ids": []string{"aaaaaaa1", "aaaaaaa2"}, "text": "Answer briefly and keep it short."}}
	answer(t, review, merge, nil)
	card := h.reviewCard(reports, "1 proposal to confirm or reject.")
	if edit := h.confirmReview(12, card); !strings.Contains(edit, "Готово. Объединено.") {
		t.Fatalf("merge after Confirm = %s", edit)
	}
	if entries := h.memoryEntries("alice"); len(entries) != 1 || entries[0].Text != "Answer briefly and keep it short." {
		t.Fatalf("entries after the merge = %+v", entries)
	}
	var outcome, userOutcome string
	if err := h.db.QueryRowContext(h.ctx, `SELECT outcome, user_outcome FROM approvals WHERE tool = 'memory_merge'`).Scan(&outcome, &userOutcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "Merged." || userOutcome != "Объединено." {
		t.Fatalf("stored outcome = %q, user outcome = %q", outcome, userOutcome)
	}
}

func TestStaleMemoryMergeReadsInRussian(t *testing.T) {
	t.Parallel()
	h := newSearchmemReviewHarness(t)
	review := h.reviewNow(11)
	reports := h.wantFeedTab("Reports", thread.FeedReports)
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	answer(t, review, []map[string]any{{"action": "merge_instructions", "ids": []string{"aaaaaaa1", "aaaaaaa2"}, "text": "Answer briefly and keep it short."}}, nil)
	card := h.reviewCard(reports, "1 proposal to confirm or reject.")
	if _, err := h.store.ReplaceMemory(h.ctx, "alice", "aaaaaaa2", "Use 24-hour times.", memory.SourceModel, time.Now()); err != nil {
		t.Fatal(err)
	}
	if edit := h.confirmReview(12, card); !strings.Contains(edit, "Не удалось. Одна из этих инструкций с тех пор изменилась, ничего не изменено.") {
		t.Fatalf("stale merge after Confirm = %s", edit)
	}
}

// The lines a turn's memory writes add to its reply read in Russian for a Russian user; the model's
// result stays English.
func TestMemoryNotesAfterAReplyReadInRussian(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{languages: true})
	h.provision("alice", "acct-alice")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	_, results := h.memoryTurn(runtime, 1, 101, "ok\n\nЗапомнено: prefers metric units", addCall("fact", "prefers metric units"))
	if results[0].IsError || strings.ContainsAny(results[0].Text, "абвгдеёжзийклмнопрстуфхцчшщъыьэюя") {
		t.Fatalf("the model's result = %+v", results[0])
	}
}

package notion_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// notionLangAllowed are the Latin-letter words the Russian texts may hold: the product name, the
// command, and the page, table, property and option names the test supplies.
var notionLangAllowed = []string{"Notion", "Session", "https", "notion", "so", "row", "session", "Status", "In", "progress", "Done", "Tags", "home", "Week", "next", "October", "review", "Mentoring", "Not", "started", "Agenda", "First", "Second", "id"}

func notionLangRequire(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, phrase := range want {
		if !strings.Contains(text, phrase) {
			t.Fatalf("text lacks %q:\n%s", phrase, text)
		}
	}
	if latin := localetest.Latin(text, notionLangAllowed...); len(latin) != 0 {
		t.Fatalf("text holds Latin words %q:\n%s", latin, text)
	}
}

func (f *fixture) notionLangExecute(name, arguments string) tools.Result {
	f.t.Helper()
	result, err := f.service.Execute(locale.With(f.ctx, locale.Russian), "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		f.t.Fatal(err)
	}
	if result.IsError || strings.ContainsAny(result.Text, "абвгдеёжзийклмнопрстуфхцчшщъыьэюя") {
		f.t.Fatalf("the model's result of %s = %+v", name, result)
	}
	return result
}

// A Russian user reads the update, create and append proposals in Russian, the confirmed write's
// outcome and refusals, and the Undo of a direct update; the model's results and the outcome texts
// the records keep stay English.
func TestNotionProposalsOutcomesAndUndoReadInRussian(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMentoringTable(f)
	f.enableWrites()
	ctx := locale.With(f.ctx, locale.Russian)

	f.notionLangExecute("notion_update_page", `{"page_id":"row-session","properties":{"Status":{"status":{"name":"Done"}},"Tags":{"multi_select":[{"name":"home"}]},"Week":{"rich_text":[{"text":{"content":"next"}}]}}}`)
	update := f.proposal()
	notionLangRequire(t, update.Summary, "Notion · обновить страницу\n**[Session](https://notion.so/row-session)**\nStatus: In progress → Done\nTags: (пусто) → home\nWeek: (пусто) → next")

	f.notionLangExecute("notion_update_page", `{"page_id":"row-session","properties":{"Status":{"status":{"name":"In progress"}}}}`)
	notionLangRequire(t, f.proposal().Summary, "Status: In progress (без изменений)")

	f.notionLangExecute("notion_create_page", `{"parent_type":"data_source","parent_id":"ds-mentoring","title":"October review","properties":{"Status":{"status":{"name":"Not started"}}},"paragraphs":["Agenda"]}`)
	create := f.proposal()
	notionLangRequire(t, create.Summary, "Notion · создать страницу\n**October review** в **Mentoring**\nStatus: Not started\nТекст (1 абзац):\n1. Agenda")

	f.notionLangExecute("notion_append_blocks", `{"page_id":"row-session","paragraphs":["First","Second"]}`)
	appended := f.proposal()
	notionLangRequire(t, appended.Summary, "Notion · добавить 2 абзаца\n**[Session](https://notion.so/row-session)**\nТекст (2 абзаца):\n1. First\n2. Second")

	confirm := func(proposal approval.Proposal) approval.Outcome {
		t.Helper()
		outcome, err := f.service.ExecuteApproved(ctx, approval.Record{PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	if outcome := confirm(update); outcome.Text != "Updated." || outcome.UserText != "Обновлено." {
		t.Fatalf("update outcome = %+v", outcome)
	}
	if outcome := confirm(appended); outcome.Text != "Appended 2 paragraph(s) to page row-session." || outcome.UserText != "Добавлено 2 абзаца на страницу row-session." {
		t.Fatalf("append outcome = %+v", outcome)
	}
	if outcome := confirm(create); !strings.HasPrefix(outcome.Text, "Created page: ") || !strings.HasPrefix(outcome.UserText, "Страница создана: ") {
		t.Fatalf("create outcome = %+v", outcome)
	}

	f.sim.FailNext(400, "")
	_, err := f.service.ExecuteApproved(ctx, approval.Record{PrincipalID: "alice", Tool: update.Tool, Arguments: update.Arguments})
	var failed *approval.ActionError
	if !errors.As(err, &failed) || failed.Text != "Notion rejected the request; nothing was written." || failed.UserText != "Notion отклонил запрос; ничего не записано." {
		t.Fatalf("rejected write = %#v", err)
	}

	direct := approval.Record{PrincipalID: "alice", Tool: update.Tool, Arguments: json.RawMessage(`{"page_id":"row-session","properties":{"Status":{"status":{"name":"Not started"}}}}`), Mode: approval.ModeDirect}
	done, err := f.service.ExecuteApproved(ctx, direct)
	if err != nil || len(done.Undo) == 0 {
		t.Fatalf("direct update = %+v %v", done, err)
	}
	undone, err := f.service.Undo(ctx, direct, done.Undo)
	if err != nil || undone.Text != "Restored the previous values." || undone.UserText != "Прежние значения возвращены." {
		t.Fatalf("undo = %+v %v", undone, err)
	}
	_, err = f.service.Undo(ctx, direct, done.Undo)
	var refused *approval.UndoRefusal
	if !errors.As(err, &refused) || refused.Reason != "changed in Notion since." || refused.UserReason != "в Notion это уже изменили." {
		t.Fatalf("second undo = %#v", err)
	}
}

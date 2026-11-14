package nas_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// nasLangAllowed are the Latin-letter words the Russian texts may hold: the NAS, and the folder
// and file names the test supplies.
var nasLangAllowed = []string{"NAS", "scan", "pdf", "inbox", "Finance", "september"}

func nasLangRequire(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, phrase := range want {
		if !strings.Contains(text, phrase) {
			t.Fatalf("text lacks %q:\n%s", phrase, text)
		}
	}
	if latin := localetest.Latin(text, nasLangAllowed...); len(latin) != 0 {
		t.Fatalf("text holds Latin words %q:\n%s", latin, text)
	}
}

// A Russian user reads the save proposal, its outcome and a refusal after Confirm in Russian,
// while the model's result and the outcome text the record keeps stay English.
func TestSaveProposalAndOutcomeReadInRussian(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.files.add("alice", "att1", "scan.pdf", []byte("scanned bytes"))
	f.enableSaving()
	ctx := locale.With(f.ctx, locale.Russian)

	result, err := f.service.Execute(ctx, "alice", tools.Call{Name: "nas_save_attachment", Arguments: json.RawMessage(`{"attachment_id":"att1","folder":"inbox/Finance","name":"september.pdf"}`)})
	if err != nil || result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
		t.Fatalf("the model's result = %+v %v", result, err)
	}
	proposal := f.proposal()
	nasLangRequire(t, proposal.Summary,
		`Сохранить "scan.pdf" (13 байт) в твою папку на NAS как inbox/Finance/september.pdf`,
		"Будет создана папка inbox/Finance",
		"Ничего из того, что уже есть на NAS, не изменится и не удалится.")

	outcome, err := f.service.ExecuteApproved(ctx, approval.Record{PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Text != "Saved to inbox/Finance/september.pdf." || outcome.UserText != "Сохранено: inbox/Finance/september.pdf." {
		t.Fatalf("outcome = %+v", outcome)
	}

	f.files.add("alice", "att2", "scan.pdf", []byte("more"))
	if result, err := f.service.Execute(ctx, "alice", tools.Call{Name: "nas_save_attachment", Arguments: json.RawMessage(`{"attachment_id":"att2"}`)}); err != nil || result.IsError {
		t.Fatalf("second proposal = %+v %v", result, err)
	}
	second := f.proposal()
	if _, err := f.service.SetWrites(f.ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.ExecuteApproved(ctx, approval.Record{PrincipalID: "alice", Tool: second.Tool, Arguments: second.Arguments})
	failed := actionErrorOf(t, err)
	if failed.Text != "Saving to the NAS is off; nothing was saved." || failed.UserText != "Сохранение на NAS выключено; ничего не сохранено." {
		t.Fatalf("refusal = %+v", failed)
	}
}

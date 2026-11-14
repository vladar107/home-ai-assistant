package backlog_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type backlogLanguageProposer struct {
	summaries []string
	proposals []approval.Proposal
}

func (p *backlogLanguageProposer) Propose(_ context.Context, _ string, proposal approval.Proposal) (approval.Record, error) {
	p.summaries = append(p.summaries, proposal.Summary)
	p.proposals = append(p.proposals, proposal)
	return approval.Record{}, nil
}

type backlogLanguageExposure struct{ external atomic.Bool }

func (e *backlogLanguageExposure) External(context.Context, string) bool { return e.external.Load() }

func backlogLanguageService(t *testing.T) (*backlog.Service, *backlogLanguageProposer, *backlogLanguageExposure) {
	t.Helper()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	proposer, exposure := &backlogLanguageProposer{}, &backlogLanguageExposure{}
	service, err := backlog.NewService(backlog.Config{}, store, proposer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	service.SetExposure(exposure)
	return service, proposer, exposure
}

func backlogLanguageCall(t *testing.T, ctx context.Context, service *backlog.Service, name, arguments string) string {
	t.Helper()
	result, err := service.Execute(ctx, "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	return result.Text
}

func TestBacklogProposalsReadInRussian(t *testing.T) {
	t.Parallel()
	service, proposer, exposure := backlogLanguageService(t)
	ctx := locale.With(t.Context(), locale.Russian)

	text := backlogLanguageCall(t, ctx, service, "backlog_add", `{"title":"Идея","details":"Подробности идеи"}`)
	if !strings.HasPrefix(text, "Added to the backlog (id ") {
		t.Fatalf("the model's add result = %q", text)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(text, "Added to the backlog (id "), "): Идея")

	exposure.external.Store(true)
	backlogLanguageCall(t, ctx, service, "backlog_add", `{"title":"Вторая идея","details":"Ещё подробности"}`)
	backlogLanguageCall(t, ctx, service, "backlog_update", `{"id":"`+id+`","status":"done"}`)
	want := []string{
		"Добавить в бэклог: Вторая идея\nЕщё подробности\nВ этом разговоре был прочитан внешний контент, поэтому нужно твоё подтверждение.",
		"Изменить пункт бэклога \"Идея\" на:\nИдея (выполнен)\nПодробности идеи\nВ этом разговоре был прочитан внешний контент, поэтому нужно твоё подтверждение.",
	}
	if len(proposer.summaries) != len(want) {
		t.Fatalf("summaries = %q", proposer.summaries)
	}
	for index, summary := range proposer.summaries {
		if summary != want[index] {
			t.Fatalf("summary %d = %q, want %q", index, summary, want[index])
		}
		if latin := localetest.Latin(summary); len(latin) > 0 {
			t.Fatalf("Latin words %v in %q", latin, summary)
		}
	}

	for index, wantText := range []string{"Добавлено в бэклог.", "Обновлено."} {
		proposed := proposer.proposals[index]
		outcome, err := service.ExecuteApproved(ctx, approval.Record{ID: "0000000000000001", PrincipalID: "alice", Tool: proposed.Tool, Arguments: proposed.Arguments})
		if err != nil || outcome.UserText != wantText || outcome.Text != []string{"Added to the backlog.", "Updated."}[index] {
			t.Fatalf("outcome %d = %q / %q / %v", index, outcome.UserText, outcome.Text, err)
		}
	}
	_, err := service.ExecuteApproved(ctx, approval.Record{PrincipalID: "alice", Tool: "backlog_unknown"})
	var failed *approval.ActionError
	if !errors.As(err, &failed) || failed.UserText != "Это действие больше недоступно." || failed.Text != "That action is no longer available." {
		t.Fatalf("unknown action = %v", err)
	}

	proposal, reason := service.AddProposal(ctx, "Идея из обзора", "Подробности")
	if reason != "" || proposal.Summary != "Добавить в бэклог: Идея из обзора\nПодробности" {
		t.Fatalf("AddProposal = %q / %q", proposal.Summary, reason)
	}
}

func TestBacklogEnglishProposalsStayTheSame(t *testing.T) {
	t.Parallel()
	service, proposer, exposure := backlogLanguageService(t)
	exposure.external.Store(true)

	backlogLanguageCall(t, t.Context(), service, "backlog_add", `{"title":"Idea","details":"Details"}`)
	if want := "Add to your backlog: Idea\nDetails\nI read outside content in this conversation, so I need your confirmation."; proposer.summaries[0] != want {
		t.Fatalf("summary = %q, want %q", proposer.summaries[0], want)
	}
	if proposal, _ := service.AddProposal(context.Background(), "Idea", "Details"); proposal.Summary != "Add to your backlog: Idea\nDetails" {
		t.Fatalf("AddProposal = %q", proposal.Summary)
	}
}

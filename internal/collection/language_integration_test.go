package collection_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/collection"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// collectionLanguageRecorder keeps the summaries the service proposes, then lets the harness's
// confirmer execute them.
type collectionLanguageRecorder struct {
	inner     *confirmer
	summaries []string
	proposals []approval.Proposal
	outcome   approval.Outcome
}

func (r *collectionLanguageRecorder) Propose(ctx context.Context, principalID string, proposal approval.Proposal) (approval.Record, error) {
	r.summaries = append(r.summaries, proposal.Summary)
	r.proposals = append(r.proposals, proposal)
	record, outcome, err := r.inner.execute(ctx, principalID, proposal)
	r.inner.remember(record, outcome, err)
	r.outcome = outcome
	return record, nil
}

func (r *collectionLanguageRecorder) Act(ctx context.Context, principalID string, proposal approval.Proposal) (string, error) {
	r.summaries = append(r.summaries, proposal.Summary)
	r.proposals = append(r.proposals, proposal)
	record, outcome, err := r.inner.execute(ctx, principalID, proposal)
	r.inner.remember(record, outcome, err)
	r.outcome = outcome
	if err != nil {
		return "", err
	}
	return "Done: " + outcome.Text, nil
}

func (r *collectionLanguageRecorder) last() string { return r.summaries[len(r.summaries)-1] }

func collectionLanguageService(t *testing.T, h *harness) (*collection.Service, *collectionLanguageRecorder) {
	t.Helper()
	recorder := &collectionLanguageRecorder{inner: h.confirmer}
	service, err := collection.NewService(collection.Config{Location: time.UTC, Now: time.Now}, h.store, recorder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service, recorder
}

func collectionLanguageCall(t *testing.T, ctx context.Context, service *collection.Service, name, arguments string) tools.Result {
	t.Helper()
	result, err := service.Execute(ctx, "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s = %q", name, result.Text)
	}
	return result
}

func TestCollectionProposalsAndUndoReadInRussian(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	service, recorder := collectionLanguageService(t, h)
	ctx := locale.With(h.ctx, locale.Russian)
	var shown []string

	result := collectionLanguageCall(t, ctx, service, "collection_create", `{"name":"Подарки","description":"идеи","fields":[`+
		`{"name":"кому","type":"text","required":true},{"name":"куплено","type":"bool"},{"name":"повод","type":"choice","options":["праздник"]}]}`)
	if result.Text != approval.ProposedText {
		t.Fatalf("the model's result = %q, want the English proposed text", result.Text)
	}
	for _, want := range []string{"Создать коллекцию Подарки: идеи", "Поля:", "- кому: текст, обязательное", "- куплено: да/нет", "- повод: выбор (праздник)"} {
		if !strings.Contains(recorder.last(), want) {
			t.Fatalf("create summary %q lacks %q", recorder.last(), want)
		}
	}
	shown = append(shown, recorder.last())
	if recorder.outcome.UserText != "Создано." || recorder.outcome.Text != "Created." {
		t.Fatalf("create outcome = %q / %q", recorder.outcome.UserText, recorder.outcome.Text)
	}
	again := recorder.proposals[0]
	_, err := service.ExecuteApproved(ctx, approval.Record{ID: "0000000000000099", PrincipalID: "alice", Tool: again.Tool, Arguments: again.Arguments})
	var failed *approval.ActionError
	if !errors.As(err, &failed) || failed.UserText != "Коллекция с названием Подарки уже есть; ничего не создано." || failed.Text != "Another collection is already called Подарки; nothing was created." {
		t.Fatalf("second create = %v", err)
	}
	shown = append(shown, recorder.outcome.UserText, failed.UserText)

	collectionLanguageCall(t, ctx, service, "collection_add", `{"collection":"Подарки","records":[{"кому":"Аня","куплено":true},{"кому":"Боря"}]}`)
	for _, want := range []string{"Добавить в Подарки:", "- кому: Аня; куплено: да"} {
		if !strings.Contains(recorder.last(), want) {
			t.Fatalf("add summary %q lacks %q", recorder.last(), want)
		}
	}
	shown = append(shown, recorder.last())
	if recorder.outcome.UserText != "Добавлены 2 записи." || recorder.outcome.Text != "Added 2 records." {
		t.Fatalf("add outcome = %q / %q", recorder.outcome.UserText, recorder.outcome.Text)
	}
	shown = append(shown, recorder.outcome.UserText)
	undone, err := service.Undo(ctx, approval.Record{PrincipalID: "alice", Tool: "collection_add"}, h.confirmer.undo["collection_add"])
	if err != nil {
		t.Fatal(err)
	}
	if undone.UserText != "Удалены 2 записи." || undone.Text != "Removed 2 records." {
		t.Fatalf("add Undo = %q / %q", undone.UserText, undone.Text)
	}
	shown = append(shown, undone.UserText)

	collectionLanguageCall(t, ctx, service, "collection_add", `{"collection":"Подарки","records":[{"кому":"Вера"}]}`)
	found := collectionLanguageCall(t, ctx, service, "collection_query", `{"collection":"Подарки"}`).Text
	start := strings.Index(found, "- [")
	recordID := found[start+3 : start+19]
	collectionLanguageCall(t, ctx, service, "collection_update", `{"collection":"Подарки","record_id":"`+recordID+`","values":{"куплено":true}}`)
	for _, want := range []string{"Изменить запись в Подарки:", "→ кому: Вера; куплено: да"} {
		if !strings.Contains(recorder.last(), want) {
			t.Fatalf("update summary %q lacks %q", recorder.last(), want)
		}
	}
	shown = append(shown, recorder.last())
	staleUndo := h.confirmer.undo["collection_update"]
	collectionLanguageCall(t, ctx, service, "collection_update", `{"collection":"Подарки","record_id":"`+recordID+`","values":{"куплено":false}}`)
	_, err = service.Undo(ctx, approval.Record{PrincipalID: "alice", Tool: "collection_update"}, staleUndo)
	var refusal *approval.UndoRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Undo of a changed record = %v, want a refusal", err)
	}
	if refusal.UserReason != "это уже изменили; измени это новым запросом." || refusal.Reason != "it was changed since; change it with a new request instead." {
		t.Fatalf("refusal = %q / %q", refusal.UserReason, refusal.Reason)
	}
	shown = append(shown, refusal.UserReason)

	collectionLanguageCall(t, ctx, service, "collection_remove", `{"collection":"Подарки","record_id":"`+recordID+`"}`)
	if !strings.Contains(recorder.last(), "Удалить из Подарки: кому: Вера") {
		t.Fatalf("remove summary = %q", recorder.last())
	}
	shown = append(shown, recorder.last())

	collectionLanguageCall(t, ctx, service, "collection_alter", `{"collection":"Подарки","changes":[{"action":"add","field":"цена","type":"number"},{"action":"rename","field":"кому","new_name":"имя"}]}`)
	for _, want := range []string{"Изменить поля коллекции Подарки:", "- добавить поле цена: число", "- переименовать кому → имя"} {
		if !strings.Contains(recorder.last(), want) {
			t.Fatalf("alter summary %q lacks %q", recorder.last(), want)
		}
	}
	shown = append(shown, recorder.last())

	for _, text := range shown {
		if latin := localetest.Latin(text); len(latin) > 0 {
			t.Fatalf("Latin words %v in %q", latin, text)
		}
	}
}

func TestCollectionEnglishTextsStayTheSame(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	service, recorder := collectionLanguageService(t, h)

	collectionLanguageCall(t, h.ctx, service, "collection_create", `{"name":"Gifts","fields":[{"name":"idea","type":"text","required":true},{"name":"bought","type":"bool"}]}`)
	if want := "Create the collection Gifts\nFields:\n- idea: text, required\n- bought: bool"; recorder.last() != want {
		t.Fatalf("create summary = %q, want %q", recorder.last(), want)
	}
	collectionLanguageCall(t, h.ctx, service, "collection_add", `{"collection":"Gifts","records":[{"idea":"tea","bought":true}]}`)
	if want := "Add to Gifts:\n- idea: tea; bought: yes"; recorder.last() != want {
		t.Fatalf("add summary = %q, want %q", recorder.last(), want)
	}
	undone, err := service.Undo(h.ctx, approval.Record{PrincipalID: "alice", Tool: "collection_add"}, h.confirmer.undo["collection_add"])
	if err != nil {
		t.Fatal(err)
	}
	if undone.Text != "Removed 1 record." || undone.UserText != undone.Text {
		t.Fatalf("Undo = %q / %q", undone.Text, undone.UserText)
	}
}

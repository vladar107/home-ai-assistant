package location_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/location"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// placeLanguageActor acts on every proposal at once, as a clean chat turn would, and keeps the
// summary, the model's act result and the Undo payload.
type placeLanguageActor struct {
	clock     *clock
	service   *location.Service
	next      int
	summary   string
	proposal  approval.Proposal
	outcome   approval.Outcome
	undoByRun []json.RawMessage
}

func (a *placeLanguageActor) Act(ctx context.Context, principalID string, proposal approval.Proposal) (string, error) {
	a.next++
	a.clock.now = a.clock.now.Add(time.Minute)
	a.summary = proposal.Summary
	a.proposal = proposal
	outcome, err := a.service.ExecuteApproved(ctx, approval.Record{ID: fmt.Sprintf("%016x", a.next), PrincipalID: principalID, Tool: proposal.Tool, Arguments: proposal.Arguments})
	if err != nil {
		return "", err
	}
	a.outcome = outcome
	a.undoByRun = append(a.undoByRun, outcome.Undo)
	return "Done: " + outcome.Text, nil
}

func placeLanguageFixture(t *testing.T) (*location.Service, *placeLanguageActor) {
	t.Helper()
	store := newStore(t)
	ticking := &clock{now: base}
	service := newService(t, store, standard, ticking, nil)
	actor := &placeLanguageActor{service: service, clock: ticking}
	service.SetPlaceTools(actor, nil)
	if _, err := service.Share(t.Context(), location.Fix{PrincipalID: "alice", Latitude: 10, Longitude: 20, Accuracy: 10, Source: location.SourcePin, UpdatedAt: base}); err != nil {
		t.Fatal(err)
	}
	return service, actor
}

func placeLanguageCall(t *testing.T, ctx context.Context, service *location.Service, name, arguments string) string {
	t.Helper()
	result, err := service.Execute(ctx, "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s = %q", name, result.Text)
	}
	return result.Text
}

func TestPlaceProposalsAndUndoReadInRussian(t *testing.T) {
	t.Parallel()
	service, actor := placeLanguageFixture(t)
	ctx := locale.With(t.Context(), locale.Russian)
	var shown []string
	wantSummary := func(want string) {
		t.Helper()
		if actor.summary != want {
			t.Fatalf("summary = %q, want %q", actor.summary, want)
		}
		shown = append(shown, actor.summary)
	}
	undo := func(tool string, payload json.RawMessage) (approval.Outcome, error) {
		return service.Undo(ctx, approval.Record{PrincipalID: "alice", Tool: tool}, payload)
	}

	if text := placeLanguageCall(t, ctx, service, "place_save", `{"name":"Дом","radius_m":200}`); text != "Done: Saved the place Дом, radius 200 m." {
		t.Fatalf("the model's save result = %q", text)
	}
	wantSummary("Сохранить твою последнюю присланную точку как место Дом, радиус 200 м.")
	if actor.outcome.UserText != "Место Дом сохранено, радиус 200 м." || actor.outcome.Text != "Saved the place Дом, radius 200 m." {
		t.Fatalf("save outcome = %q / %q", actor.outcome.UserText, actor.outcome.Text)
	}
	shown = append(shown, actor.outcome.UserText)
	_, err := service.ExecuteApproved(ctx, approval.Record{ID: "00000000000000ff", PrincipalID: "alice", Tool: actor.proposal.Tool, Arguments: actor.proposal.Arguments})
	var failed *approval.ActionError
	if !errors.As(err, &failed) || failed.UserText != "Место с названием Дом уже есть; ничего не сохранено." || failed.Text != "Another place is already called Дом; nothing was saved." {
		t.Fatalf("second save = %v", err)
	}
	shown = append(shown, failed.UserText)
	saved := actor.undoByRun[0]
	undone, err := undo("place_save", saved)
	if err != nil {
		t.Fatal(err)
	}
	if undone.UserText != "Место Дом удалено." || undone.Text != "Deleted the place Дом." {
		t.Fatalf("save Undo = %q / %q", undone.UserText, undone.Text)
	}
	shown = append(shown, undone.UserText)

	placeLanguageCall(t, ctx, service, "place_save", `{"name":"Дом","radius_m":200}`)
	placeLanguageCall(t, ctx, service, "place_update", `{"name":"Дом","new_name":"Офис","radius_m":300}`)
	wantSummary("Изменить место Дом:\nназвание: Дом → Офис\nрадиус: 200 м → 300 м")
	if actor.outcome.UserText != "Изменено. Теперь место называется Офис, радиус 300 м." || actor.outcome.Text != "Changed. The place is now Офис, radius 300 m." {
		t.Fatalf("update outcome = %q / %q", actor.outcome.UserText, actor.outcome.Text)
	}
	shown = append(shown, actor.outcome.UserText)
	firstUpdate := actor.outcome.Undo
	placeLanguageCall(t, ctx, service, "place_update", `{"name":"Офис","radius_m":400}`)
	_, err = undo("place_update", firstUpdate)
	var refusal *approval.UndoRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Undo of a changed place = %v, want a refusal", err)
	}
	if refusal.UserReason != "это место уже изменили; измени его новым запросом." || refusal.Reason != "the place was changed since; change it with a new request instead." {
		t.Fatalf("refusal = %q / %q", refusal.UserReason, refusal.Reason)
	}
	shown = append(shown, refusal.UserReason)

	secondUpdate := actor.outcome.Undo
	restored, err := undo("place_update", secondUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if restored.UserText != "Место Офис возвращено как было, радиус 300 м." || restored.Text != "Restored the place Офис, radius 300 m." {
		t.Fatalf("update Undo = %q / %q", restored.UserText, restored.Text)
	}
	shown = append(shown, restored.UserText)

	placeLanguageCall(t, ctx, service, "place_delete", `{"name":"Офис"}`)
	wantSummary("Удалить место Офис (радиус 300 м).")
	if actor.outcome.UserText != "Место Офис удалено." || actor.outcome.Text != "Deleted the place Офис." {
		t.Fatalf("delete outcome = %q / %q", actor.outcome.UserText, actor.outcome.Text)
	}
	back, err := undo("place_delete", actor.outcome.Undo)
	if err != nil {
		t.Fatal(err)
	}
	if back.UserText != "Место Офис восстановлено." || back.Text != "Restored the place Офис." {
		t.Fatalf("delete Undo = %q / %q", back.UserText, back.Text)
	}
	shown = append(shown, back.UserText)

	for _, text := range shown {
		if latin := localetest.Latin(text); len(latin) > 0 {
			t.Fatalf("Latin words %v in %q", latin, text)
		}
	}
}

func TestPlaceEnglishTextsStayTheSame(t *testing.T) {
	t.Parallel()
	service, actor := placeLanguageFixture(t)

	placeLanguageCall(t, t.Context(), service, "place_save", `{"name":"Home","radius_m":200}`)
	if want := "Save your latest shared location pin as the place Home, radius 200 m."; actor.summary != want {
		t.Fatalf("summary = %q, want %q", actor.summary, want)
	}
	undone, err := service.Undo(t.Context(), approval.Record{PrincipalID: "alice", Tool: "place_save"}, actor.outcome.Undo)
	if err != nil {
		t.Fatal(err)
	}
	if undone.Text != "Deleted the place Home." || undone.UserText != undone.Text {
		t.Fatalf("Undo = %q / %q", undone.Text, undone.UserText)
	}
}

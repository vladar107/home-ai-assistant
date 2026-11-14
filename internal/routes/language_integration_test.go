package routes_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/routes"
)

// routeLanguageStations are the station and stop names the NS and bus fixtures return.
var routeLanguageStations = []string{"Amersfoort", "Schothorst", "Utrecht", "Centraal", "Stadhuis"}

func routeLanguageCommute() routes.Spec {
	spec := work()
	spec.Name = "работа"
	return spec
}

func routeLanguagePayload(t *testing.T, undo map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(undo)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRouteProposalsAndUndoReadInRussian(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ctx = locale.With(h.ctx, locale.Russian)
	h.save("alice", routeLanguageCommute())
	var shown []string
	proposed := func() string {
		t.Helper()
		summary := h.proposals.last(t).Summary
		shown = append(shown, summary)
		return summary
	}

	result := h.call("alice", "route_save", map[string]any{"name": "новый", "days": []string{"fri", "mon"}, "arrive_by": "08:45", "legs": []any{
		map[string]any{"mode": "bike", "minutes": 6},
		map[string]any{"mode": "train", "from": "amfs", "to": "Utrecht", "departures": []string{"08:12", "07:56"}},
		map[string]any{"mode": "bus", "stop": "Stadhuis", "line": "3", "towards": "Centraal"},
	}})
	if result.IsError {
		t.Fatal(result.Text)
	}
	want := "Сохранить маршрут новый: пн, пт, прибыть к 08:45\n1. на велосипеде 6 мин\n2. поезд Amersfoort Schothorst → Utrecht Centraal, отправления: 07:56, 08:12\n3. автобус 3 от остановки Amersfoort, Stadhuis в сторону Centraal, без фиксированных отправлений"
	if got := proposed(); got != want {
		t.Fatalf("save summary = %q, want %q", got, want)
	}

	saveProposal := h.proposals.last(t)
	executed, err := h.service.ExecuteApproved(h.ctx, approval.Record{ID: "00000000000000a1", PrincipalID: "alice", Tool: saveProposal.Tool, Arguments: saveProposal.Arguments})
	if err != nil || executed.UserText != "Сохранено." || executed.Text != "Saved." {
		t.Fatalf("save outcome = %q / %q / %v", executed.UserText, executed.Text, err)
	}
	_, err = h.service.ExecuteApproved(h.ctx, approval.Record{ID: "00000000000000a2", PrincipalID: "alice", Tool: saveProposal.Tool, Arguments: saveProposal.Arguments})
	var failed *approval.ActionError
	if !errors.As(err, &failed) || failed.UserText != "Маршрут с названием новый уже есть; ничего не сохранено." || failed.Text != "Another route is already called новый; nothing was saved." {
		t.Fatalf("second save = %v", err)
	}
	shown = append(shown, executed.UserText, failed.UserText)

	h.call("alice", "route_update", map[string]any{"name": "работа", "arrive_by": "", "days": []string{"mon", "tue"}, "leg_index": 3, "leg_action": "insert", "leg": map[string]any{"mode": "walk", "minutes": 3}})
	if got, want := proposed(), "Изменить маршрут работа:\nдни: пн–пт → пн, вт\nприбыть к: 08:45 → нет\nновый участок 3: пешком 3 мин"; got != want {
		t.Fatalf("update summary = %q, want %q", got, want)
	}
	h.call("alice", "route_update", map[string]any{"name": "работа", "leg_index": 1, "leg_action": "remove"})
	if got, want := proposed(), "Изменить маршрут работа:\nучасток 1 удалён: на велосипеде 6 мин"; got != want {
		t.Fatalf("removed leg summary = %q, want %q", got, want)
	}
	h.call("alice", "route_remove", map[string]any{"name": "работа"})
	if got, want := proposed(), "Удалить маршрут работа: пн–пт, прибыть к 08:45\nна велосипеде 6 мин; поезд Amersfoort Schothorst → Utrecht Centraal, отправления: 07:56, 08:12; пешком 10 мин"; got != want {
		t.Fatalf("remove summary = %q, want %q", got, want)
	}
	removal := h.proposals.last(t)

	wantText(t, h.call("alice", "route_get", map[string]any{"name": "работа"}).Text, "Mon–Fri, arrive by 08:45\n1. bike 6 min\n", "departures 07:56, 08:12\n3. walk 10 min")

	stored, found, err := h.store.LoadRoute(h.ctx, "alice", "000000000000000b")
	if err != nil || !found {
		t.Fatalf("saved route: %v %v", found, err)
	}
	stale := routeLanguagePayload(t, map[string]any{"route_id": stored.ID, "updated_at": stored.UpdatedAt.Add(-time.Hour)})
	_, err = h.service.Undo(h.ctx, approval.Record{PrincipalID: "alice", Tool: "route_save"}, stale)
	var refusal *approval.UndoRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Undo of a changed route = %v, want a refusal", err)
	}
	if refusal.UserReason != "этот маршрут уже изменили; измени его новым запросом." || refusal.Reason != "the route was changed since; change it with a new request instead." {
		t.Fatalf("refusal = %q / %q", refusal.UserReason, refusal.Reason)
	}
	shown = append(shown, refusal.UserReason)

	undone, err := h.service.Undo(h.ctx, approval.Record{PrincipalID: "alice", Tool: "route_save"}, routeLanguagePayload(t, map[string]any{"route_id": stored.ID, "updated_at": stored.UpdatedAt}))
	if err != nil {
		t.Fatal(err)
	}
	if undone.UserText != "Маршрут работа удалён." || undone.Text != "Deleted the route работа." {
		t.Fatalf("save Undo = %q / %q", undone.UserText, undone.Text)
	}
	shown = append(shown, undone.UserText)

	back, err := h.service.Undo(h.ctx, approval.Record{PrincipalID: "alice", Tool: "route_remove"}, routeLanguagePayload(t, map[string]any{"route_id": stored.ID, "removed": stored}))
	if err != nil {
		t.Fatal(err)
	}
	if back.UserText != "Маршрут работа восстановлен." || back.Text != "Restored the route работа." {
		t.Fatalf("remove Undo = %q / %q", back.UserText, back.Text)
	}
	shown = append(shown, back.UserText)

	removed, err := h.service.ExecuteApproved(h.ctx, approval.Record{ID: "00000000000000a3", PrincipalID: "alice", Tool: removal.Tool, Arguments: removal.Arguments})
	if err != nil || removed.UserText != "Удалено." || removed.Text != "Deleted." {
		t.Fatalf("remove outcome = %q / %q / %v", removed.UserText, removed.Text, err)
	}
	shown = append(shown, removed.UserText)

	for _, text := range shown {
		if latin := localetest.Latin(text, routeLanguageStations...); len(latin) > 0 {
			t.Fatalf("Latin words %v in %q", latin, text)
		}
	}
}

func TestRouteEnglishTextsStayTheSame(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.save("alice", work())
	h.call("alice", "route_update", map[string]any{"name": "work", "arrive_by": "", "leg_index": 3, "leg_action": "insert", "leg": map[string]any{"mode": "walk", "minutes": 3}})
	if got, want := h.proposals.last(t).Summary, "Change the route work:\narrival target: 08:45 → none\nnew leg 3: walk 3 min"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	stored, _, err := h.store.LoadRoute(h.ctx, "alice", "000000000000000b")
	if err != nil {
		t.Fatal(err)
	}
	undone, err := h.service.Undo(h.ctx, approval.Record{PrincipalID: "alice", Tool: "route_save"}, routeLanguagePayload(t, map[string]any{"route_id": stored.ID, "updated_at": stored.UpdatedAt}))
	if err != nil {
		t.Fatal(err)
	}
	if undone.Text != "Deleted the route work." || undone.UserText != undone.Text {
		t.Fatalf("Undo = %q / %q", undone.Text, undone.UserText)
	}
}

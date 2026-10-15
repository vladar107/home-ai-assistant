package watch_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

type fakeTravel struct {
	mu        sync.Mutex
	traveling bool
	failure   error
}

func (f *fakeTravel) set(traveling bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.traveling = traveling
}

func (f *fakeTravel) Traveling(context.Context, string, time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.traveling, f.failure
}

func (e *engine) createFlagged(principalID, id string) {
	e.t.Helper()
	now := e.clock.Now()
	created, err := e.store.CreateWatch(e.ctx, watch.Watch{ID: id, PrincipalID: principalID, Spec: departureSpec(), Enabled: true, TravelPause: true, CreatedAt: now, UpdatedAt: now}, watch.MaximumWatchesPerPrincipal)
	if err != nil || !created {
		e.t.Fatalf("create flagged watch: %v %v", created, err)
	}
}

func TestFlaggedWatchIsNeitherCheckedNorDeliveredDuringATrip(t *testing.T) {
	e := newEngine(t)
	travel := &fakeTravel{}
	e.service.SetTravel(travel)
	e.createFlagged("alice", watchID)
	e.create("alice", "0000000000000002", departureSpec())

	travel.set(true)
	e.at("2026-09-28 06:15")
	if calls, _ := e.evaluator.counts(); calls != 1 {
		t.Fatalf("checks during the trip = %d, want only the unflagged watch", calls)
	}

	travel.set(false)
	e.evaluator.set(late, nil)
	e.sender.fail(transientError{})
	e.at("2026-09-28 06:25")
	if status, _ := e.alertStatus(watchID, "2026-09-28", 1); status != "ready" {
		t.Fatalf("flagged alert status outside a trip = %s", status)
	}
	travel.set(true)
	e.sender.fail(nil)
	e.at("2026-09-28 06:26")
	if status, reason := e.alertStatus(watchID, "2026-09-28", 1); status != "dropped" || reason != "travel_paused" {
		t.Fatalf("flagged alert during a trip = %s %s", status, reason)
	}
	if status, _ := e.alertStatus("0000000000000002", "2026-09-28", 1); status != "delivered" {
		t.Fatalf("unflagged alert status = %s", status)
	}

	travel.set(false)
	e.sender.take()
	before, _ := e.evaluator.counts()
	e.at("2026-09-29 06:15")
	if after, _ := e.evaluator.counts(); after != before+2 {
		t.Fatalf("checks after the trip = %d, want both watches checked", after-before)
	}
	if names, err := e.service.TravelPausedNames(e.ctx, "alice"); err != nil || !slices.Equal(names, []string{departureSpec().Label()}) {
		t.Fatalf("paused names = %v %v", names, err)
	}
	if _, err := e.service.SetEnabled(e.ctx, "alice", watchID, false); err != nil {
		t.Fatal(err)
	}
	if names, err := e.service.TravelPausedNames(e.ctx, "alice"); err != nil || len(names) != 0 {
		t.Fatalf("paused names of a paused watch = %v %v", names, err)
	}
}

func TestWatchToolsSetAndKeepTheTravelFlag(t *testing.T) {
	e := newEngine(t)
	proposer := &fakeProposer{}
	service, err := watch.NewService(watch.Config{Zone: "Europe/Amsterdam", Now: e.clock.Now}, e.store, e.store, e.sender, proposer, slog.New(slog.NewTextHandler(e.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Register(watch.KindDeparture, e.evaluator); err != nil {
		t.Fatal(err)
	}
	last := func() approval.Proposal {
		proposer.mu.Lock()
		defer proposer.mu.Unlock()
		return proposer.proposals[len(proposer.proposals)-1].proposal
	}
	run := func(name, arguments string) {
		t.Helper()
		result, err := service.Execute(e.ctx, "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil || result.IsError {
			t.Fatalf("%s = %+v %v", name, result, err)
		}
	}
	confirm := func(id string) {
		t.Helper()
		proposal := last()
		if _, err := service.ExecuteApproved(e.ctx, approval.Record{ID: id, PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments}); err != nil {
			t.Fatal(err)
		}
	}
	flag := func(id string) bool {
		t.Helper()
		stored, found, err := e.store.LoadWatch(e.ctx, "alice", id)
		if err != nil || !found {
			t.Fatalf("load watch: %v %v", found, err)
		}
		return stored.TravelPause
	}

	run("watch_propose", `{"kind":"departure","mode":"train","from":"A","to":"B","departure":"07:42","days":["mon"],"pause_while_traveling":true}`)
	if !strings.Contains(last().Summary, "silent while you are traveling") {
		t.Fatalf("summary = %q", last().Summary)
	}
	confirm(watchID)
	if !flag(watchID) {
		t.Fatal("the confirmed watch lost the flag")
	}

	run("watch_update", `{"watch_id":"`+watchID+`","departure":"07:50"}`)
	confirm(watchID)
	if !flag(watchID) {
		t.Fatal("an update that did not mention the flag cleared it")
	}
	if listed, err := service.Execute(e.ctx, "alice", tools.Call{Name: "watch_list", Arguments: json.RawMessage(`{}`)}); err != nil || !strings.Contains(listed.Text, "pauses while traveling") {
		t.Fatalf("list = %+v %v", listed, err)
	}

	run("watch_update", `{"watch_id":"`+watchID+`","pause_while_traveling":false}`)
	confirm(watchID)
	if flag(watchID) {
		t.Fatal("pause_while_traveling false did not clear the flag")
	}

	legacy, err := json.Marshal(departureSpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteApproved(e.ctx, approval.Record{ID: "0000000000000009", PrincipalID: "alice", Tool: "watch_propose", Arguments: legacy}); err != nil {
		t.Fatalf("a proposal recorded before the flag existed: %v", err)
	}
	if flag("0000000000000009") {
		t.Fatal("a legacy proposal got the flag")
	}
}

func TestWatchFlagToggledAfterAProposalSurvivesConfirmAndUndo(t *testing.T) {
	e := newEngine(t)
	proposer := &fakeProposer{}
	service, err := watch.NewService(watch.Config{Zone: "Europe/Amsterdam", Now: e.clock.Now}, e.store, e.store, e.sender, proposer, slog.New(slog.NewTextHandler(e.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Register(watch.KindDeparture, e.evaluator); err != nil {
		t.Fatal(err)
	}
	e.createFlagged("alice", watchID)
	call := func(arguments string) {
		t.Helper()
		result, err := service.Execute(e.ctx, "alice", tools.Call{Name: "watch_update", Arguments: json.RawMessage(arguments)})
		if err != nil || result.IsError {
			t.Fatalf("watch_update = %+v %v", result, err)
		}
	}
	confirm := func() approval.Outcome {
		t.Helper()
		proposer.mu.Lock()
		proposal := proposer.proposals[len(proposer.proposals)-1].proposal
		proposer.mu.Unlock()
		outcome, err := service.ExecuteApproved(e.ctx, approval.Record{ID: "0000000000000009", PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	stored := func() watch.Watch {
		t.Helper()
		w, found, err := e.store.LoadWatch(e.ctx, "alice", watchID)
		if err != nil || !found {
			t.Fatalf("load watch: %v %v", found, err)
		}
		return w
	}

	call(`{"watch_id":"` + watchID + `","departure":"07:50"}`)
	if _, err := service.SetTravelPause(e.ctx, "alice", watchID, false); err != nil {
		t.Fatal(err)
	}
	outcome := confirm()
	if stored().TravelPause {
		t.Fatal("Confirm reverted the flag toggled in /jobs")
	}
	if _, err := service.SetTravelPause(e.ctx, "alice", watchID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Undo(e.ctx, approval.Record{PrincipalID: "alice", Tool: "watch_update"}, outcome.Undo); err != nil {
		t.Fatal(err)
	}
	if w := stored(); !w.TravelPause || w.End != "07:42" {
		t.Fatalf("watch after Undo = %+v", w)
	}

	before := stored()
	call(`{"watch_id":"` + watchID + `","pause_while_traveling":false}`)
	outcome = confirm()
	if w := stored(); w.TravelPause || !w.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a flag-only update = %+v, want only the flag changed", w)
	}
	if _, err := service.Undo(e.ctx, approval.Record{PrincipalID: "alice", Tool: "watch_update"}, outcome.Undo); err != nil {
		t.Fatal(err)
	}
	if !stored().TravelPause {
		t.Fatal("Undo did not restore the flag its update changed")
	}
}

func TestTravelLookupIsAskedOncePerPrincipalInACycle(t *testing.T) {
	e := newEngine(t)
	counting := &countingTravel{}
	e.service.SetTravel(counting)
	e.createFlagged("alice", watchID)
	e.createFlagged("alice", "0000000000000002")
	e.at("2026-09-28 06:15")
	if counting.calls != 1 {
		t.Fatalf("trip lookups in one cycle = %d, want 1", counting.calls)
	}
}

type countingTravel struct{ calls int }

func (c *countingTravel) Traveling(context.Context, string, time.Time) (bool, error) {
	c.calls++
	return false, nil
}

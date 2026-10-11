package watch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// homeFake answers every home check with the answer currently set, records what the service
// handed it, and resolves the one device name it knows.
type homeFake struct {
	mu      sync.Mutex
	current answer
	seen    []watch.Occurrence
}

func (h *homeFake) set(observation watch.Observation, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.current = answer{observation: observation, err: err}
}

func (h *homeFake) Evaluate(_ context.Context, _ string, _ watch.Watch, occurrence watch.Occurrence) (watch.Observation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, occurrence)
	return h.current.observation, h.current.err
}

func (h *homeFake) Resolve(_ context.Context, _ string, spec watch.Spec) (watch.Spec, error) {
	if spec.Target.Entity == "" && spec.Target.Name == "Balcony door" {
		spec.Target.Entity = "binary_sensor.balcony_door"
		return spec, nil
	}
	if spec.Target.Entity == "" {
		return watch.Spec{}, fmt.Errorf("no device is called %q", spec.Target.Name)
	}
	return spec, nil
}

func (h *homeFake) last() watch.Occurrence {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen[len(h.seen)-1]
}

func (h *homeFake) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.seen)
}

type homeEngine struct {
	*engine
	home     *homeFake
	proposer *fakeProposer
}

func newHomeEngine(t *testing.T) *homeEngine {
	t.Helper()
	e := newEngine(t)
	h := &homeEngine{engine: e, home: &homeFake{}, proposer: &fakeProposer{}}
	service, err := watch.NewService(watch.Config{Zone: "Europe/Amsterdam", Now: e.clock.Now}, e.store, e.store, e.sender, h.proposer, slog.New(slog.NewTextHandler(e.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Register(watch.KindDeparture, e.evaluator); err != nil {
		t.Fatal(err)
	}
	if err := service.Register(watch.KindHome, h.home); err != nil {
		t.Fatal(err)
	}
	service.SetTelemetry(e.telemetry)
	e.service = service
	return h
}

func homeSpec(start, end string) watch.Spec {
	return watch.Spec{
		Kind:      watch.KindHome,
		Target:    watch.Target{Entity: "binary_sensor.balcony_door", Name: "Balcony door"},
		Condition: &watch.Condition{State: []string{"open"}, ForMinutes: 10},
		Triggers:  []string{watch.TriggerEnter, watch.TriggerClear},
		Days:      []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
		Start:     start,
		End:       end,
		Zone:      "Europe/Amsterdam",
	}
}

var (
	doorClosed = watch.Observation{State: "ok", Headline: "Balcony door is closed."}
	doorOpen   = watch.Observation{State: "holding", Triggered: true, Headline: "Balcony door has been open for 10 minutes."}
)

func (h *homeEngine) wantState(occurrence string, check func(watch.State) bool) watch.State {
	h.t.Helper()
	state, found, err := h.store.LoadWatchState(h.ctx, watchID, occurrence)
	if err != nil || !found || !check(state) {
		h.t.Fatalf("state of %s = %+v %v %v", occurrence, state, found, err)
	}
	return state
}

// A skipped check counts no failure, stores nothing, and records no outcome; the first real
// answer after it behaves as the first check.
func TestHomeWatchSkipsWithoutFailureOrStateChange(t *testing.T) {
	h := newHomeEngine(t)
	h.create("alice", watchID, homeSpec("00:00", "24:00"))
	h.home.set(watch.Observation{}, fmt.Errorf("home switch is off: %w", watch.ErrSkip))
	for minute := 0; minute < 6; minute++ {
		h.at(fmt.Sprintf("2026-09-28 10:%02d", minute))
	}
	if h.home.calls() != 6 {
		t.Fatalf("skipped watch was evaluated %d times, want every cycle", h.home.calls())
	}
	h.wantSent(0)
	if _, found, err := h.store.LoadWatchState(h.ctx, watchID, "2026-09-28"); err != nil || found {
		t.Fatalf("a skipped watch stored a state: %v %v", found, err)
	}
	if h.telemetry.count("check alice home failed") != 0 || h.telemetry.count("upstream home none") != 0 {
		t.Fatalf("a skip was recorded: %v", h.telemetry.events)
	}

	h.home.set(doorOpen, nil)
	h.at("2026-09-28 10:06")
	h.wantSent(1, "Balcony door has been open for 10 minutes.")
}

// The 24:00 window has no blind minute: the last minute of one day and the first of the next are
// both checked, each in its own date's window.
func TestHomeWatchWindowEndsAtMidnightWithoutAGap(t *testing.T) {
	h := newHomeEngine(t)
	h.create("alice", watchID, homeSpec("00:00", "24:00"))
	h.home.set(doorClosed, nil)
	h.at("2026-09-28 23:59")
	if got := h.home.last(); got.Date != "2026-09-28" || !got.End.Equal(h.clock.Now().Add(time.Minute)) {
		t.Fatalf("occurrence at 23:59 = %+v", got)
	}
	h.at("2026-09-29 00:00")
	if got := h.home.last(); got.Date != "2026-09-29" || got.Previous != "ok" {
		t.Fatalf("occurrence at 00:00 = %+v", got)
	}
	if h.home.calls() != 2 {
		t.Fatalf("evaluated %d times, want 2", h.home.calls())
	}
}

// A door that stays open across midnight alerts once; the next day's window starts from the
// previous one's state and since, with its own counters, and its clear message still follows.
func TestHomeWatchCarriesStateAcrossMidnight(t *testing.T) {
	h := newHomeEngine(t)
	h.create("alice", watchID, homeSpec("00:00", "24:00"))
	opened := h.clock.Now().Add(18 * time.Hour)
	open := doorOpen
	open.Since = opened

	h.home.set(open, nil)
	h.at("2026-09-28 23:58")
	h.wantSent(1, "open for 10 minutes")
	h.at("2026-09-29 00:00")
	h.at("2026-09-29 00:01")
	h.wantSent(0)
	if got := h.home.last(); got.Previous != "holding" || !got.Since.Equal(opened) {
		t.Fatalf("occurrence after midnight = %+v", got)
	}
	state := h.wantState("2026-09-29", func(s watch.State) bool { return s.Alerted && s.Messages == 0 && s.Key == "holding" })
	if !state.Since.Equal(opened) {
		t.Fatalf("since after midnight = %v", state.Since)
	}

	h.home.set(doorClosed, nil)
	h.at("2026-09-29 00:05")
	h.wantSent(1, "Back to normal")
	h.wantState("2026-09-29", func(s watch.State) bool { return !s.Alerted && s.Messages == 1 && s.Since.IsZero() })
}

// Only a window that directly follows the previous day's carries state. After a narrowed
// window's overnight gap the engine cannot know what happened, so a door still open alerts again
// and the first check starts without a past, so there is no late clear.
func TestHomeWatchStartsFreshAfterANarrowedWindowsGap(t *testing.T) {
	h := newHomeEngine(t)
	h.create("alice", watchID, homeSpec("07:00", "23:00"))
	h.home.set(doorOpen, nil)
	h.at("2026-09-28 22:59")
	h.wantSent(1, "open for 10 minutes")
	h.at("2026-09-28 23:30")
	if h.home.calls() != 1 {
		t.Fatal("checked outside the window")
	}

	h.at("2026-09-29 07:00")
	h.wantSent(1, "open for 10 minutes")
	if got := h.home.seen[1]; got.Previous != "" || !got.Since.IsZero() {
		t.Fatalf("first check of the next window = %+v", got)
	}
	h.home.set(doorClosed, nil)
	h.at("2026-09-29 07:01")
	h.wantSent(1, "Back to normal")

	h.home.set(doorClosed, nil)
	h.at("2026-09-30 07:00")
	h.wantSent(0)
}

// Editing a watch resets its window state but not the alerts already queued that day; later alerts
// continue the numbering, so they are delivered, and still count against the cap.
func TestHomeWatchDeliversAlertsAfterAnEditMidDay(t *testing.T) {
	h := newHomeEngine(t)
	h.create("alice", watchID, homeSpec("00:00", "24:00"))
	h.home.set(doorOpen, nil)
	h.at("2026-09-28 10:00")
	h.wantSent(1, "open for 10 minutes")

	stored, found, err := h.store.LoadWatch(h.ctx, "alice", watchID)
	if err != nil || !found {
		t.Fatalf("load watch: %v %v", found, err)
	}
	edited := stored
	edited.Condition = &watch.Condition{State: []string{"open", "tilted"}}
	edited.UpdatedAt = stored.UpdatedAt.Add(time.Second)
	if updated, err := h.store.UpdateWatch(h.ctx, edited, stored.UpdatedAt); err != nil || !updated {
		t.Fatalf("update watch: %v %v", updated, err)
	}

	h.home.set(doorClosed, nil)
	h.at("2026-09-28 10:05")
	h.home.set(doorOpen, nil)
	h.at("2026-09-28 10:06")
	h.wantSent(1, "open for 10 minutes")
	h.wantState("2026-09-28", func(s watch.State) bool { return s.Messages == 2 })
}

// A whole-day watch starts fresh when it was not watching right up to midnight: muted or paused
// yesterday afternoon, a 00:00–23:00 window, or the service down overnight.
func TestHomeWatchStartsFreshWhenNotWatchingUpToMidnight(t *testing.T) {
	for _, test := range []struct {
		name, end string
		lastCheck string
		mute      bool
	}{
		{name: "muted in the afternoon", end: "24:00", lastCheck: "2026-09-28 15:00", mute: true},
		{name: "window ending at 23:00", end: "23:00", lastCheck: "2026-09-28 22:59"},
		{name: "service down overnight", end: "24:00", lastCheck: "2026-09-28 22:00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHomeEngine(t)
			h.create("alice", watchID, homeSpec("00:00", test.end))
			h.home.set(doorOpen, nil)
			h.at(test.lastCheck)
			h.wantSent(1, "open for 10 minutes")
			if test.mute {
				if muted, err := h.service.Mute(h.ctx, "alice", watchID); err != nil || !muted {
					t.Fatalf("mute: %v %v", muted, err)
				}
			}
			h.at("2026-09-29 00:00")
			h.wantSent(1, "open for 10 minutes")
			if got := h.home.last(); got.Previous != "" || !got.Since.IsZero() {
				t.Fatalf("first check of the next day = %+v", got)
			}
		})
	}
}

// A watch with only the enter trigger stays quiet when the condition clears, and one with only
// clear stays quiet when it starts, yet still tells when it ends.
func TestHomeWatchMessagesOnlyOnItsTriggers(t *testing.T) {
	h := newHomeEngine(t)
	enterOnly := homeSpec("00:00", "24:00")
	enterOnly.Triggers = []string{watch.TriggerEnter}
	h.create("alice", watchID, enterOnly)
	clearOnly := homeSpec("00:00", "24:00")
	clearOnly.Triggers = []string{watch.TriggerClear}
	h.create("alice", "0000000000000002", clearOnly)

	h.home.set(doorOpen, nil)
	h.at("2026-09-28 10:00")
	h.wantSent(1, "open for 10 minutes")
	h.home.set(doorClosed, nil)
	h.at("2026-09-28 10:05")
	h.wantSent(1, "Back to normal")
	h.home.set(doorOpen, nil)
	h.at("2026-09-28 10:10")
	h.wantSent(1, "open for 10 minutes")
}

// A home window may send ten messages, a transit one stays at five.
func TestHomeWatchSendsAtMostTenMessagesPerWindow(t *testing.T) {
	h := newHomeEngine(t)
	h.create("alice", watchID, homeSpec("00:00", "24:00"))
	for minute := 0; minute < 14; minute++ {
		if minute%2 == 0 {
			h.home.set(doorOpen, nil)
		} else {
			h.home.set(doorClosed, nil)
		}
		h.at(fmt.Sprintf("2026-09-28 10:%02d", minute))
	}
	h.wantSent(10)
}

// A home alert is deliverable for half an hour after it was queued, even past midnight, and is
// dropped after that.
func TestHomeAlertsExpireHalfAnHourAfterTheyAreQueued(t *testing.T) {
	for _, test := range []struct {
		name, retry, status, reason string
		sent                        int
	}{
		{"within half an hour", "2026-09-29 00:20", "delivered", "", 1},
		{"after half an hour", "2026-09-29 00:31", "dropped", "late", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHomeEngine(t)
			h.create("alice", watchID, homeSpec("00:00", "24:00"))
			h.sender.fail(transientError{})
			h.home.set(doorOpen, nil)
			for _, minute := range []string{"2026-09-28 23:55", "2026-09-28 23:57", "2026-09-28 23:59", "2026-09-29 00:00"} {
				h.at(minute)
			}
			h.wantSent(0)
			if status, _ := h.alertStatus(watchID, "2026-09-28", 1); status != "ready" {
				t.Fatalf("status = %s", status)
			}
			h.sender.fail(nil)
			h.at(test.retry)
			h.wantSent(test.sent)
			if status, reason := h.alertStatus(watchID, "2026-09-28", 1); status != test.status || reason != test.reason {
				t.Fatalf("status = %s %s", status, reason)
			}
		})
	}
}

// Since is stored with every successful check, silent ones included, and cleared when the
// condition stops holding.
func TestHomeWatchStoresSince(t *testing.T) {
	h := newHomeEngine(t)
	h.create("alice", watchID, homeSpec("00:00", "24:00"))
	started := h.clock.Now().Add(10 * time.Hour)

	h.home.set(watch.Observation{State: "pending", Silent: true, Since: started}, nil)
	h.at("2026-09-28 10:00")
	h.wantSent(0)
	h.wantState("2026-09-28", func(s watch.State) bool { return s.Since.Equal(started) && s.Key == "pending" })

	h.home.set(watch.Observation{State: "pending", Silent: true, Since: started.Add(-time.Minute)}, nil)
	h.at("2026-09-28 10:01")
	h.wantState("2026-09-28", func(s watch.State) bool { return s.Since.Equal(started.Add(-time.Minute)) })
	if got := h.home.last(); !got.Since.Equal(started) {
		t.Fatalf("since handed to the second check = %v", got.Since)
	}

	h.home.set(doorClosed, nil)
	h.at("2026-09-28 10:02")
	h.wantState("2026-09-28", func(s watch.State) bool { return s.Since.IsZero() })
}

func (h *homeEngine) propose(arguments map[string]any) tools.Result {
	h.t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		h.t.Fatal(err)
	}
	result, err := h.service.Execute(h.ctx, "alice", tools.Call{Name: "watch_propose", Arguments: raw})
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *homeEngine) proposed() watch.Spec {
	h.t.Helper()
	h.proposer.mu.Lock()
	defer h.proposer.mu.Unlock()
	if len(h.proposer.proposals) == 0 {
		h.t.Fatal("nothing was proposed")
	}
	var spec watch.Spec
	if err := json.Unmarshal(h.proposer.proposals[len(h.proposer.proposals)-1].proposal.Arguments, &spec); err != nil {
		h.t.Fatal(err)
	}
	return spec
}

// The model names the device and the condition; the resolver supplies the entity, the defaults
// follow the condition, and conditions that are not exactly one of state, above, or below are
// refused.
func TestHomeWatchProposalValidatesTheCondition(t *testing.T) {
	h := newHomeEngine(t)
	base := func(condition map[string]any) map[string]any {
		return map[string]any{"kind": "home", "name": "Balcony door", "days": []string{"mon"}, "condition": condition}
	}

	result := h.propose(base(map[string]any{"state": []string{" open ", "open", "Tilted"}, "for_minutes": 10}))
	if result.IsError {
		t.Fatalf("valid proposal refused: %s", result.Text)
	}
	spec := h.proposed()
	if spec.Target.Entity != "binary_sensor.balcony_door" || spec.Start != "00:00" || spec.End != "24:00" ||
		strings.Join(spec.Triggers, ",") != "enter" || strings.Join(spec.Condition.State, ",") != "open,Tilted" {
		t.Fatalf("state proposal = %+v", spec)
	}
	if label := spec.Label(); label != "Balcony door: open or Tilted for 10 min" {
		t.Fatalf("label = %q", label)
	}
	if description := spec.Describe(); !strings.Contains(description, "Window: all day") || !strings.Contains(description, "checked every minute") {
		t.Fatalf("description = %q", description)
	}

	freezer := base(map[string]any{"above": -15.5})
	freezer["name"] = "Balcony door"
	if result := h.propose(freezer); result.IsError {
		t.Fatalf("numeric proposal refused: %s", result.Text)
	}
	spec = h.proposed()
	if strings.Join(spec.Triggers, ",") != "enter,clear" || spec.Label() != "Balcony door: above -15.5" {
		t.Fatalf("numeric proposal = %+v", spec)
	}

	narrow := base(map[string]any{"below": 3})
	narrow["window_from"], narrow["window_until"] = "07:00", "24:00"
	if result := h.propose(narrow); result.IsError {
		t.Fatalf("narrowed proposal refused: %s", result.Text)
	}
	if spec = h.proposed(); spec.Start != "07:00" || spec.End != "24:00" {
		t.Fatalf("narrowed window = %+v", spec)
	}

	tooMany := make([]string, 11)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("state%d", i)
	}
	withControl := map[string]any{"state": []string{"open\x00"}}
	for name, arguments := range map[string]map[string]any{
		"no condition":                 {"kind": "home", "name": "Balcony door", "days": []string{"mon"}},
		"empty condition":              base(map[string]any{}),
		"state and threshold":          base(map[string]any{"state": []string{"open"}, "above": 3}),
		"above and below":              base(map[string]any{"above": 3, "below": 1}),
		"too long a hold":              base(map[string]any{"state": []string{"open"}, "for_minutes": 241}),
		"too many states":              base(map[string]any{"state": tooMany}),
		"blank state":                  base(map[string]any{"state": []string{"  "}}),
		"control character in a state": base(withControl),
		"unknown device":               {"kind": "home", "name": "Garage", "days": []string{"mon"}, "condition": map[string]any{"state": []string{"open"}}},
		"no device":                    {"kind": "home", "days": []string{"mon"}, "condition": map[string]any{"state": []string{"open"}}},
		"a departure":                  {"kind": "home", "name": "Balcony door", "departure": "07:42", "days": []string{"mon"}, "condition": map[string]any{"state": []string{"open"}}},
		"too short a window":           {"kind": "home", "name": "Balcony door", "days": []string{"mon"}, "window_from": "10:00", "window_until": "10:10", "condition": map[string]any{"state": []string{"open"}}},
		"a window across midnight":     {"kind": "home", "name": "Balcony door", "days": []string{"mon"}, "window_from": "22:00", "window_until": "06:00", "condition": map[string]any{"state": []string{"open"}}},
		"24:00 as the start":           {"kind": "home", "name": "Balcony door", "days": []string{"mon"}, "window_from": "24:00", "window_until": "24:00", "condition": map[string]any{"state": []string{"open"}}},
		"a delay threshold":            {"kind": "home", "name": "Balcony door", "days": []string{"mon"}, "triggers": []string{"delay"}, "condition": map[string]any{"state": []string{"open"}}},
		"a condition on a train": {"kind": "departure", "mode": "train", "from": "A", "to": "B", "departure": "07:42", "days": []string{"mon"},
			"condition": map[string]any{"state": []string{"open"}}},
		"24:00 for a train": {"kind": "departure", "mode": "train", "from": "A", "to": "B", "departure": "24:00", "days": []string{"mon"}},
	} {
		if result := h.propose(arguments); !result.IsError {
			t.Errorf("%s was proposed", name)
		}
	}
}

// Updating a home watch keeps what is not changed and drops the entity with a new name so the
// resolver runs again.
func TestHomeWatchUpdateKeepsTheConditionAndReresolvesANewName(t *testing.T) {
	h := newHomeEngine(t)
	h.create("alice", watchID, homeSpec("00:00", "24:00"))
	update := func(arguments map[string]any) tools.Result {
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		result, err := h.service.Execute(h.ctx, "alice", tools.Call{Name: "watch_update", Arguments: raw})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := update(map[string]any{"watch_id": watchID, "window_from": "07:00"}); result.IsError {
		t.Fatalf("update refused: %s", result.Text)
	}
	var confirmed struct{ Spec watch.Spec }
	h.proposer.mu.Lock()
	raw := h.proposer.proposals[len(h.proposer.proposals)-1].proposal.Arguments
	h.proposer.mu.Unlock()
	if err := json.Unmarshal(raw, &confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed.Spec.Start != "07:00" || confirmed.Spec.End != "24:00" || confirmed.Spec.Condition.ForMinutes != 10 ||
		confirmed.Spec.Target.Entity != "binary_sensor.balcony_door" {
		t.Fatalf("updated spec = %+v", confirmed.Spec)
	}
	if result := update(map[string]any{"watch_id": watchID, "name": "Garage"}); !result.IsError {
		t.Fatal("a new unknown name kept the old entity")
	}
}

// Without a home evaluator the kind is not offered and a proposal for it is refused.
func TestHomeKindIsOfferedOnlyWithAnEvaluator(t *testing.T) {
	e := newEngine(t)
	service, err := watch.NewService(watch.Config{Zone: "Europe/Amsterdam", Now: e.clock.Now}, e.store, e.store, e.sender, &fakeProposer{}, slog.New(slog.NewTextHandler(e.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Register(watch.KindDeparture, e.evaluator); err != nil {
		t.Fatal(err)
	}
	contribution, err := service.Contribute(e.ctx, "alice")
	if err != nil || strings.Contains(contribution.WriteGuidance, "home watch") || !strings.Contains(contribution.WriteGuidance, "available now are: departure.") {
		t.Fatalf("guidance without home = %q %v", contribution.WriteGuidance, err)
	}
	raw, _ := json.Marshal(map[string]any{"kind": "home", "name": "Balcony door", "days": []string{"mon"}, "condition": map[string]any{"state": []string{"open"}}})
	result, err := service.Execute(e.ctx, "alice", tools.Call{Name: "watch_propose", Arguments: raw})
	if err != nil || !result.IsError {
		t.Fatalf("home proposal without an evaluator = %+v %v", result, err)
	}
}

package homeassistant_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const (
	balconyDoor    = "- names: Balcony door\n  domain: binary_sensor\n  state: 'off'\n  attributes:\n    device_class: door\n"
	freezer        = "- names: Freezer\n  domain: sensor\n  state: '-18'\n  attributes:\n    unit_of_measurement: °C\n"
	washingMachine = "- names: Washing machine\n  domain: sensor\n  state: idle\n"
	hallLamp       = "- names: Hall lamp\n  domain: light\n  state: 'off'\n"
	robotStatus    = "- names: Robot status\n  domain: sensor\n  state: sleeping\n"
)

func homeStates(changed time.Time) []map[string]any {
	stamp := changed.Format(time.RFC3339)
	entity := func(id, state, name string, extra map[string]any) map[string]any {
		attributes := map[string]any{"friendly_name": name}
		for key, value := range extra {
			attributes[key] = value
		}
		return map[string]any{"entity_id": id, "state": state, "last_changed": stamp, "attributes": attributes}
	}
	return []map[string]any{
		entity("binary_sensor.balcony_door", "off", "Balcony door", map[string]any{"device_class": "door"}),
		entity("sensor.freezer", "-18", "Freezer", map[string]any{"unit_of_measurement": "°C"}),
		entity("sensor.washing_machine", "idle", "Washing machine", nil),
		entity("sensor.garage_note", "text", "Garage note", nil),
		entity("light.hall_lamp_one", "off", "Hall lamp", nil),
		entity("light.hall_lamp_two", "off", "Hall lamp", nil),
		entity("sensor.cellar_humidity", "55", "Cellar humidity", nil),
		entity("sensor.robot_status", "sleeping", "Robot status", map[string]any{"options": []any{"cleaning", "sleeping", "returning"}}),
	}
}

func homeFixture(t *testing.T, exposed ...string) *fixture {
	t.Helper()
	sim := newSimulator(exposed...)
	sim.set(func(s *simulator) { s.states = homeStates(start) })
	return newFixture(t, sim)
}

func setEntity(f *fixture, id, state string, changed time.Time) {
	f.sim.set(func(s *simulator) {
		for _, entity := range s.states {
			if entity["entity_id"] == id {
				entity["state"] = state
				entity["last_changed"] = changed.Format(time.RFC3339)
			}
		}
	})
}

func (f *fixture) statesRequests() int {
	f.sim.mu.Lock()
	defer f.sim.mu.Unlock()
	count := 0
	for _, call := range f.sim.httpCalls {
		if call.method == http.MethodGet && call.path == "/api/states" {
			count++
		}
	}
	return count
}

func doorWatch(forMinutes int) watch.Watch {
	return watch.Watch{ID: "w1", Spec: watch.Spec{
		Kind:      watch.KindHome,
		Target:    watch.Target{Entity: "binary_sensor.balcony_door", Name: "Balcony door"},
		Condition: &watch.Condition{State: []string{"on"}, ForMinutes: forMinutes},
	}}
}

func freezerWatch(condition watch.Condition) watch.Watch {
	return watch.Watch{ID: "w2", Spec: watch.Spec{
		Kind:      watch.KindHome,
		Target:    watch.Target{Entity: "sensor.freezer", Name: "Freezer"},
		Condition: &condition,
	}}
}

func number(value float64) *float64 { return &value }

func TestHomeWatchStateConditionAlertsWithSeededDuration(t *testing.T) {
	f := homeFixture(t, balconyDoor)
	ctx := t.Context()
	occ := watch.Occurrence{Cache: watch.NewCache()}

	closed, err := f.service.Evaluate(ctx, "alice", doorWatch(10), occ)
	if err != nil || closed.State != "ok" || closed.Triggered || !closed.Since.IsZero() || closed.Headline != "Balcony door is closed." {
		t.Fatalf("closed door = %+v, %v", closed, err)
	}

	opened := start.Add(-4 * time.Minute)
	setEntity(f, "binary_sensor.balcony_door", "on", opened)
	pending, err := f.service.Evaluate(ctx, "alice", doorWatch(10), watch.Occurrence{Cache: watch.NewCache(), Previous: "ok"})
	if err != nil || pending.State != "pending" || !pending.Silent || pending.Triggered || !pending.Since.Equal(opened) {
		t.Fatalf("recently opened door = %+v, %v", pending, err)
	}

	f.clock.advance(7 * time.Minute)
	held, err := f.service.Evaluate(ctx, "alice", doorWatch(10), watch.Occurrence{Cache: watch.NewCache(), Previous: "pending", Since: pending.Since})
	if err != nil || held.State != "holding" || !held.Triggered || !held.Since.Equal(opened) {
		t.Fatalf("door open for 11 minutes = %+v, %v", held, err)
	}
	if held.Headline != "Balcony door has been open for 10 minutes." {
		t.Fatalf("headline = %q", held.Headline)
	}

	immediate, err := f.service.Evaluate(ctx, "alice", doorWatch(0), watch.Occurrence{Cache: watch.NewCache()})
	if err != nil || !immediate.Triggered || immediate.Headline != "Balcony door is open." {
		t.Fatalf("no duration = %+v, %v", immediate, err)
	}
}

func TestHomeWatchStateKeepsItsStartWhenLastChangedJumpsWhileHolding(t *testing.T) {
	f := homeFixture(t, balconyDoor)
	began := start.Add(-30 * time.Minute)
	setEntity(f, "binary_sensor.balcony_door", "on", start.Add(-time.Minute))

	got, err := f.service.Evaluate(t.Context(), "alice", doorWatch(10), watch.Occurrence{
		Cache: watch.NewCache(), Previous: "holding", Since: began,
	})
	if err != nil || got.State != "holding" || !got.Triggered || !got.Since.Equal(began) {
		t.Fatalf("after a restart-style jump = %+v, %v", got, err)
	}
}

func TestHomeWatchNumericConditionsCountFromFirstCheck(t *testing.T) {
	f := homeFixture(t, freezer)
	ctx := t.Context()
	above := freezerWatch(watch.Condition{Above: number(-15), ForMinutes: 5})

	normal, err := f.service.Evaluate(ctx, "alice", above, watch.Occurrence{Cache: watch.NewCache()})
	if err != nil || normal.State != "ok" || normal.Headline != "Freezer is -18 °C." {
		t.Fatalf("normal freezer = %+v, %v", normal, err)
	}

	setEntity(f, "sensor.freezer", "-12", start.Add(-time.Hour))
	first, err := f.service.Evaluate(ctx, "alice", above, watch.Occurrence{Cache: watch.NewCache(), Previous: "ok"})
	if err != nil || first.State != "pending" || !first.Silent || !first.Since.Equal(start) {
		t.Fatalf("first warm check = %+v, %v", first, err)
	}

	f.clock.advance(4 * time.Minute)
	setEntity(f, "sensor.freezer", "-11", start)
	still, _ := f.service.Evaluate(ctx, "alice", above, watch.Occurrence{Cache: watch.NewCache(), Previous: "pending", Since: first.Since})
	if still.State != "pending" || !still.Since.Equal(start) {
		t.Fatalf("after 4 minutes = %+v", still)
	}

	f.clock.advance(time.Minute)
	held, err := f.service.Evaluate(ctx, "alice", above, watch.Occurrence{Cache: watch.NewCache(), Previous: "pending", Since: first.Since})
	if err != nil || held.State != "holding" || !held.Triggered || held.Headline != "Freezer is -11 °C, above -15 °C for 5 minutes." {
		t.Fatalf("after 5 minutes = %+v, %v", held, err)
	}

	below := freezerWatch(watch.Condition{Below: number(-15)})
	setEntity(f, "sensor.freezer", "-20", start)
	cold, err := f.service.Evaluate(ctx, "alice", below, watch.Occurrence{Cache: watch.NewCache()})
	if err != nil || !cold.Triggered || cold.Headline != "Freezer is -20 °C, below -15 °C." {
		t.Fatalf("below = %+v, %v", cold, err)
	}
}

func TestHomeWatchUnavailableStateFailsTheCheck(t *testing.T) {
	f := homeFixture(t, balconyDoor, freezer)
	setEntity(f, "binary_sensor.balcony_door", "unavailable", start)
	setEntity(f, "sensor.freezer", "unavailable", start)
	for _, w := range []watch.Watch{doorWatch(0), freezerWatch(watch.Condition{Above: number(-15)})} {
		if _, err := f.service.Evaluate(t.Context(), "alice", w, watch.Occurrence{Cache: watch.NewCache()}); err == nil || errors.Is(err, watch.ErrSkip) {
			t.Fatalf("unavailable %s err = %v", w.Target.Name, err)
		}
	}
	unavailable := doorWatch(0)
	unavailable.Condition.State = []string{"Unavailable"}
	got, err := f.service.Evaluate(t.Context(), "alice", unavailable, watch.Occurrence{Cache: watch.NewCache()})
	if err != nil || !got.Triggered {
		t.Fatalf("watching for unavailable = %+v, %v", got, err)
	}
}

func TestHomeWatchResolve(t *testing.T) {
	f := homeFixture(t, balconyDoor, freezer, hallLamp)
	f.sim.set(func(s *simulator) {
		s.registry = append(s.registry, map[string]any{"ei": "sensor.washing_machine", "lb": []string{"assistant_confirm"}})
	})
	ctx := t.Context()
	state := &watch.Condition{State: []string{"open"}}

	resolved, err := f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Name: "balcony DOOR"}, Condition: state})
	if err != nil || resolved.Target.Entity != "binary_sensor.balcony_door" || resolved.Target.Name != "Balcony door" {
		t.Fatalf("resolved = %+v, %v", resolved.Target, err)
	}
	resolved, err = f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Name: "old name", Entity: "sensor.washing_machine"}, Condition: state})
	if err != nil || resolved.Target.Entity != "sensor.washing_machine" || resolved.Target.Name != "Washing machine" {
		t.Fatalf("labelled entity = %+v, %v", resolved.Target, err)
	}

	_, err = f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Name: "Hall lamp"}, Condition: state})
	if err == nil {
		t.Fatal("an exposed name shared by two entities must not be visible")
	}
}

func TestHomeWatchResolveRefusesUnknownAndInvisible(t *testing.T) {
	f := homeFixture(t, balconyDoor, freezer)
	ctx := t.Context()
	state := &watch.Condition{State: []string{"open"}}

	_, err := f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Name: "Washing machine"}, Condition: state})
	if err == nil || !strings.Contains(err.Error(), "no device the assistant can see") {
		t.Fatalf("unexposed device err = %v", err)
	}
	_, err = f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Name: "Freezer door"}, Condition: state})
	if err == nil || !strings.Contains(err.Error(), "Freezer") || strings.Contains(err.Error(), "Washing machine") {
		t.Fatalf("candidates = %v", err)
	}
	_, err = f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Entity: "sensor.washing_machine"}, Condition: state})
	if err == nil {
		t.Fatal("an unexposed entity ID must be refused")
	}
}

func TestHomeWatchResolveRefusesAmbiguousName(t *testing.T) {
	f := homeFixture(t, hallLamp)
	f.sim.set(func(s *simulator) {
		s.registry = append(s.registry,
			map[string]any{"ei": "light.hall_lamp_one", "lb": []string{"assistant_confirm"}},
			map[string]any{"ei": "light.hall_lamp_two", "lb": []string{"assistant_confirm"}})
	})
	_, err := f.service.Resolve(t.Context(), "alice", watch.Spec{Target: watch.Target{Name: "Hall lamp"}, Condition: &watch.Condition{State: []string{"on"}}})
	if err == nil || !strings.Contains(err.Error(), "light.hall_lamp_one") || !strings.Contains(err.Error(), "light.hall_lamp_two") {
		t.Fatalf("ambiguous err = %v", err)
	}
}

func TestHomeWatchResolveRequiresNumericStateForThresholds(t *testing.T) {
	f := homeFixture(t, washingMachine, freezer)
	_, err := f.service.Resolve(t.Context(), "alice", watch.Spec{Target: watch.Target{Name: "Washing machine"}, Condition: &watch.Condition{Above: number(1)}})
	if err == nil || !strings.Contains(err.Error(), "not a number") {
		t.Fatalf("non-numeric err = %v", err)
	}
	resolved, err := f.service.Resolve(t.Context(), "alice", watch.Spec{Target: watch.Target{Name: "Freezer"}, Condition: &watch.Condition{Above: number(-15)}})
	if err != nil || resolved.Target.Entity != "sensor.freezer" {
		t.Fatalf("numeric = %+v, %v", resolved.Target, err)
	}
}

func TestHomeWatchFailsWhenEntityStopsBeingExposed(t *testing.T) {
	f := homeFixture(t, balconyDoor, freezer)
	if _, err := f.service.Evaluate(t.Context(), "alice", doorWatch(0), watch.Occurrence{Cache: watch.NewCache()}); err != nil {
		t.Fatal(err)
	}
	f.sim.set(func(s *simulator) { s.liveContext = overviewTop + freezer })
	f.clock.advance(10 * time.Minute)
	_, err := f.service.Evaluate(t.Context(), "alice", doorWatch(0), watch.Occurrence{Cache: watch.NewCache()})
	if err == nil || errors.Is(err, watch.ErrSkip) || !strings.Contains(err.Error(), "no longer exposed") {
		t.Fatalf("err = %v", err)
	}
}

func TestHomeWatchSkipsWhileTheHomeIsOff(t *testing.T) {
	f := homeFixture(t, balconyDoor)
	if _, err := f.service.SetEnabled(t.Context(), "alice", false); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Evaluate(t.Context(), "alice", doorWatch(0), watch.Occurrence{Cache: watch.NewCache()})
	if !errors.Is(err, watch.ErrSkip) {
		t.Fatalf("err = %v", err)
	}
	if f.statesRequests() != 0 {
		t.Fatal("a skipped check must not read states")
	}
	if _, err := f.service.Resolve(t.Context(), "alice", watch.Spec{Target: watch.Target{Name: "Balcony door"}, Condition: &watch.Condition{State: []string{"open"}}}); err == nil {
		t.Fatal("resolving with the home off must fail")
	}
	if _, err := f.service.Evaluate(t.Context(), "bob", doorWatch(0), watch.Occurrence{Cache: watch.NewCache()}); err != nil {
		t.Fatalf("bob's home is on: %v", err)
	}
}

func TestHomeWatchesShareOneStatesRequestPerCycle(t *testing.T) {
	f := homeFixture(t, balconyDoor, freezer)
	cache := watch.NewCache()
	for _, w := range []watch.Watch{doorWatch(0), freezerWatch(watch.Condition{Above: number(-15)}), doorWatch(5)} {
		if _, err := f.service.Evaluate(t.Context(), "alice", w, watch.Occurrence{Cache: cache}); err != nil {
			t.Fatal(err)
		}
	}
	// Building the snapshot reads the states once more.
	if got := f.statesRequests(); got != 2 {
		t.Fatalf("/api/states requests = %d, want 2 (snapshot + cycle)", got)
	}
	if _, err := f.service.Evaluate(t.Context(), "alice", doorWatch(0), watch.Occurrence{Cache: watch.NewCache()}); err != nil {
		t.Fatal(err)
	}
	if got := f.statesRequests(); got != 3 {
		t.Fatalf("/api/states requests after a second cycle = %d, want 3", got)
	}
}

func TestHomeWatchResolveMapsBinarySensorWords(t *testing.T) {
	f := homeFixture(t, balconyDoor)
	ctx := t.Context()
	resolve := func(states ...string) (watch.Spec, error) {
		return f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Name: "Balcony door"}, Condition: &watch.Condition{State: states}})
	}

	mapped, err := resolve("Open", "closed")
	if err != nil || len(mapped.Condition.State) != 2 || mapped.Condition.State[0] != "on" || mapped.Condition.State[1] != "off" {
		t.Fatalf("mapped = %+v, %v", mapped.Condition, err)
	}
	if _, err := resolve("ajar"); err == nil || !strings.Contains(err.Error(), "open is on") {
		t.Fatalf("unknown word err = %v", err)
	}
	if _, err := f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Name: "Balcony door"}, Condition: &watch.Condition{Above: number(1)}}); err == nil {
		t.Fatal("a threshold on a binary sensor must be refused")
	}

	closed, err := f.service.Evaluate(ctx, "alice", doorWatch(0), watch.Occurrence{Cache: watch.NewCache()})
	if err != nil || closed.Headline != "Balcony door is closed." {
		t.Fatalf("closed = %+v, %v", closed, err)
	}
	setEntity(f, "binary_sensor.balcony_door", "on", start)
	open, err := f.service.Evaluate(ctx, "alice", doorWatch(0), watch.Occurrence{Cache: watch.NewCache()})
	if err != nil || open.Headline != "Balcony door is open." {
		t.Fatalf("open = %+v, %v", open, err)
	}
}

func TestHomeWatchResolveChecksStatesAgainstTheSensorsOptions(t *testing.T) {
	f := homeFixture(t, robotStatus, washingMachine)
	ctx := t.Context()

	_, err := f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Name: "Robot status"}, Condition: &watch.Condition{State: []string{"vacuuming"}}})
	if err == nil || !strings.Contains(err.Error(), "cleaning, sleeping, returning") {
		t.Fatalf("unlisted state err = %v", err)
	}
	if _, err := f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Name: "Robot status"}, Condition: &watch.Condition{State: []string{"Cleaning"}}}); err != nil {
		t.Fatalf("listed state refused: %v", err)
	}
	if _, err := f.service.Resolve(ctx, "alice", watch.Spec{Target: watch.Target{Name: "Washing machine"}, Condition: &watch.Condition{State: []string{"done"}}}); err != nil {
		t.Fatalf("a sensor without options must accept any state: %v", err)
	}
}

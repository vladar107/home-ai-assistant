package homesim_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/homeassistant"
	"github.com/vladar107/home-ai-assistant/internal/testutil/homesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const seed = `{
  "time_zone": "Europe/Amsterdam",
  "entities": [
    {"entity_id": "light.study_lamp", "name": "Study lamp", "state": "off", "area": "Study", "exposed": true},
    {"entity_id": "sensor.hall_temperature", "name": "Hall temperature", "state": "21.4", "unit": "°C",
      "device_class": "temperature", "area": "Hall", "exposed": true,
      "statistics": [{"start": "2026-03-09T00:00:00Z", "mean": 20.5, "min": 19, "max": 22},
                     {"start": "2026-03-10T00:00:00Z", "mean": 21.5, "min": 20, "max": 23}]},
    {"entity_id": "vacuum.hall_robot", "name": "Hall robot", "state": "docked", "area": "Hall", "exposed": true,
      "attributes": {"schedule": "Mon 09:00; Thu 09:00"}},
    {"entity_id": "lock.garden_gate", "name": "Garden gate", "state": "locked", "labels": ["assistant-confirm"]}
  ],
  "tools": [
    {"name": "intent__HassTurnOn", "description": "Turns on a device or entity.",
      "input_schema": {"type": "object", "properties": {"name": {"type": "string"}}},
      "answer": "Turned on.", "effects": [{"entity_id": "light.study_lamp", "state": "on"}]}
  ],
  "services": [{"domain": "lock", "services": {"unlock": []}}]
}`

func fixture(t *testing.T) (*homeassistant.Service, *homesim.Server) {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC) }
	fake := homesim.New(t, json.RawMessage(seed), now)
	service, err := homeassistant.NewService(homeassistant.Config{Token: fake.Token(), BaseURL: fake.URL(), Now: now},
		simkit.AlwaysOn{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service, fake
}

func call(t *testing.T, service *homeassistant.Service, name, arguments string) tools.Result {
	t.Helper()
	result, err := service.Execute(t.Context(), "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s failed: %s", name, result.Text)
	}
	return result
}

func TestToolsAndDevicesComeFromSeed(t *testing.T) {
	service, _ := fixture(t)
	contribution, err := service.Contribute(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, definition := range contribution.Definitions {
		names = append(names, definition.Name)
	}
	for _, want := range []string{"ha_get_live_context", "ha_turn_on", "ha_history"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools %v lack %s", names, want)
		}
	}
	live := call(t, service, "ha_get_live_context", `{}`).Text
	for _, want := range []string{"Study lamp", "Hall robot", "Mon 09:00; Thu 09:00"} {
		if !strings.Contains(live, want) {
			t.Errorf("live context lacks %q:\n%s", want, live)
		}
	}
	if strings.Contains(live, "Garden gate") {
		t.Errorf("an entity that is not exposed is in the live context:\n%s", live)
	}
}

func TestActionChangesState(t *testing.T) {
	service, fake := fixture(t)
	if text := call(t, service, "ha_turn_on", `{"name":"Study lamp"}`).Text; text != "Turned on." {
		t.Fatalf("got %q", text)
	}
	if state, _ := fake.State("light.study_lamp"); state != "on" {
		t.Fatalf("state = %q", state)
	}
	if calls := fake.Calls(); len(calls) == 0 || calls[len(calls)-1].Tool != "intent__HassTurnOn" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestHistoryAndTrendFromSeededStatistics(t *testing.T) {
	service, _ := fixture(t)
	if text := call(t, service, "ha_history", `{"name":"Hall temperature","period":"all"}`).Text; !strings.Contains(text, "mean: 21") {
		t.Errorf("history lacks the mean:\n%s", text)
	}
	if text := call(t, service, "ha_history", `{"name":"Hall temperature","period":"last_7_days","points":"day"}`).Text; !strings.Contains(text, "2026-03-10: 21.5 °C") {
		t.Errorf("trend lacks the seeded day:\n%s", text)
	}
}

func TestConfirmedDeviceListsSeededServices(t *testing.T) {
	service, _ := fixture(t)
	if text := call(t, service, "ha_confirmed_devices", `{}`).Text; !strings.Contains(text, "Garden gate") || !strings.Contains(text, "unlock") {
		t.Errorf("confirmed devices:\n%s", text)
	}
}

func TestValidateRejectsUnknownFieldsAndBadEffects(t *testing.T) {
	if err := homesim.Validate(json.RawMessage(seed)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"entities":[],"nonsense":1}`,
		`{"tools":[{"name":"x","effects":[{"entity_id":"light.none","state":"on"}]}]}`,
		`{"entities":[{"entity_id":"light.a","name":"A"},{"entity_id":"light.a","name":"B"}]}`,
	} {
		if err := homesim.Validate(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

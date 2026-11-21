package bussim_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/bus"
	"github.com/vladar107/home-ai-assistant/internal/testutil/bussim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const seed = `{
  "stops": [
    {"id": "100", "code": "30000001", "name": "Eastbrook, Market Square", "lat": 52.1, "lon": 5.1, "platform": "A"},
    {"id": "101", "code": "30000002", "name": "Westfield, Station", "lat": 52.2, "lon": 5.2}
  ],
  "routes": [{"id": "r340", "agency": "UOV", "short": "340", "long": "Eastbrook - Westfield"}],
  "timing_points": [{"code": "30000001", "passes": {"J1": {
    "LinePublicNumber": "340", "JourneyNumber": 1, "UserStopOrderNumber": 1,
    "DestinationName50": "Westfield", "TransportType": "BUS", "TripStopStatus": "PLANNED",
    "JourneyStopType": "FIRST", "TargetDepartureTime": "2026-03-10T08:15:00",
    "ExpectedDepartureTime": "2026-03-10T08:17:00"}}}],
  "alerts": [{"effect": 9, "header": "Market Square platform A moved", "from": "2026-03-09T00:00:00Z",
    "informed": [{"stop_id": "100"}]}],
  "places": [{"type": "STOP", "name": "Harbourside Terminal", "lat": 52.3, "lon": 5.3, "country": "NL",
    "areas": [{"name": "Netherlands", "adminLevel": 2}, {"name": "Westfield", "adminLevel": 8}, {"name": "Westfield", "adminLevel": 10}]}],
  "plans": [{"from": {"lat": 52.1, "lon": 5.1}, "to": {"lat": 52.3, "lon": 5.3}, "itinerary": {
    "duration": 2400, "startTime": "2026-03-10T08:30:00Z", "endTime": "2026-03-10T09:10:00Z", "transfers": 0,
    "legs": [{"mode": "BUS", "routeShortName": "340", "headsign": "Harbourside", "duration": 2400,
      "startTime": "2026-03-10T08:30:00Z", "endTime": "2026-03-10T09:10:00Z",
      "scheduledStartTime": "2026-03-10T08:30:00Z", "scheduledEndTime": "2026-03-10T09:10:00Z",
      "from": {"name": "Eastbrook, Market Square", "departure": "2026-03-10T08:30:00Z", "scheduledDeparture": "2026-03-10T08:30:00Z"},
      "to": {"name": "Harbourside Terminal", "arrival": "2026-03-10T09:10:00Z", "scheduledArrival": "2026-03-10T09:10:00Z"}}]}}]
}`

func service(t *testing.T) *bus.Service {
	t.Helper()
	fake := bussim.New(t, json.RawMessage(seed))
	location, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	service, err := bus.NewService(bus.Config{
		OVapiURL:      fake.OVapiURL(),
		GTFSURL:       fake.GTFSURL(),
		TransitousURL: fake.TransitousURL(),
		Now:           func() time.Time { return time.Date(2026, 3, 10, 8, 0, 0, 0, location) },
	}, simkit.AlwaysOn{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func call(t *testing.T, service *bus.Service, name, arguments string) string {
	t.Helper()
	result, err := service.Execute(t.Context(), "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s failed: %s", name, result.Text)
	}
	return result.Text
}

func TestStopBoardShowsSeededPassAndAlert(t *testing.T) {
	text := call(t, service(t), "bus_stop_status", `{"stop":"Market Square"}`)
	for _, want := range []string{"340", "Westfield", "platform A moved"} {
		if !strings.Contains(text, want) {
			t.Errorf("board lacks %q:\n%s", want, text)
		}
	}
}

func TestTripUsesSeededGeocodeAndPlan(t *testing.T) {
	text := call(t, service(t), "bus_trip", `{"from":"Market Square","to":"Harbourside Terminal","date_time":"2026-03-10T09:30"}`)
	if !strings.Contains(text, "340") || !strings.Contains(text, "Harbourside") {
		t.Fatalf("trip lacks the seeded itinerary:\n%s", text)
	}
}

func TestTripGeocodesOnlyInTheNamedTown(t *testing.T) {
	service := service(t)
	text := call(t, service, "bus_trip", `{"from":"Market Square","to":"Westfield, Harbourside Terminal","date_time":"2026-03-10T09:30"}`)
	if !strings.Contains(text, "340") {
		t.Fatalf("trip lacks the seeded itinerary:\n%s", text)
	}
	result, err := service.Execute(t.Context(), "alice", tools.Call{Name: "bus_trip",
		Arguments: json.RawMessage(`{"from":"Market Square","to":"Eastbrook, Harbourside Terminal","date_time":"2026-03-10T09:30"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Text, `"Eastbrook, Harbourside Terminal"`) {
		t.Fatalf("a place in another town was accepted: %s", result.Text)
	}
}

func TestUnknownQueryParameterIsRefused(t *testing.T) {
	fake := bussim.New(t, json.RawMessage(seed))
	response, err := http.Get(fake.TransitousURL() + "/v1/geocode?text=x&bogus=1")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", response.StatusCode)
	}
}

func TestValidateRejectsUnknownFields(t *testing.T) {
	if err := bussim.Validate(json.RawMessage(seed)); err != nil {
		t.Fatal(err)
	}
	if err := bussim.Validate(json.RawMessage(`{"stops":[],"nonsense":1}`)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

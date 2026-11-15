package nssim_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/ns"
	"github.com/vladar107/home-ai-assistant/internal/testutil/nssim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const seed = `{
  "stations": [
    {"code": "EBK", "name": "Eastbrook Centraal"},
    {"code": "WFD", "name": "Westfield"}
  ],
  "trips": [{"from": "EBK", "to": "WFD", "trip": {
    "status": "NORMAL", "transfers": 0, "plannedDurationInMinutes": 24,
    "legs": [{"name": "IC 1100", "travelType": "PUBLIC_TRANSIT", "direction": "Westfield",
      "product": {"categoryCode": "IC", "number": "1100", "longCategoryName": "Intercity"},
      "origin": {"name": "Eastbrook Centraal", "stationCode": "EBK", "plannedDateTime": "2026-03-10T08:10:00+0100", "plannedTrack": "4"},
      "destination": {"name": "Westfield", "stationCode": "WFD", "plannedDateTime": "2026-03-10T08:34:00+0100"}}]}}],
  "departures": [{"station": "EBK", "departure": {"direction": "Westfield", "name": "NS 1100",
    "plannedDateTime": "2026-03-10T08:10:00+0100", "plannedTrack": "4", "cancelled": true,
    "product": {"categoryCode": "IC", "number": "1100"}}}],
  "disruptions": [{"type": "DISRUPTION", "id": "d1", "title": "Eastbrook - Westfield signal fault", "isActive": true,
    "publicationSections": [{"section": {"stations": [{"name": "Westfield", "stationCode": "WFD"}]}}]}],
  "ovfiets": [{"station": "EBK", "location": {"name": "Eastbrook Centraal", "stationCode": "EBK",
    "description": "OV-fiets Eastbrook", "extra": {"rentalBikes": "7"}}}]
}`

func service(t *testing.T) *ns.Service {
	t.Helper()
	fake := nssim.New(t, json.RawMessage(seed))
	service, err := ns.NewService(ns.Config{
		APIKey:        "test-key",
		BaseURL:       fake.BaseURL(),
		PlacesBaseURL: fake.PlacesBaseURL(),
		Now:           func() time.Time { return time.Date(2026, 3, 10, 7, 30, 0, 0, time.UTC) },
	}, simkit.AlwaysOn{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func call(t *testing.T, service *ns.Service, name, arguments string) string {
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

func TestConnectorReadsSeededTimetable(t *testing.T) {
	service := service(t)
	for name, check := range map[string]struct{ arguments, want string }{
		"ns_trips":       {`{"from":"Eastbrook Centraal","to":"Westfield","date_time":"2026-03-10T08:00"}`, "IC 1100"},
		"ns_departures":  {`{"station":"EBK","date_time":"2026-03-10T08:00"}`, "Westfield"},
		"ns_disruptions": {`{"station":"Westfield"}`, "signal fault"},
		"ns_ovfiets":     {`{"station":"EBK"}`, "OV-fiets Eastbrook"},
	} {
		if text := call(t, service, name, check.arguments); !strings.Contains(text, check.want) {
			t.Errorf("%s answered %q, want it to contain %q", name, text, check.want)
		}
	}
}

func TestArriveByAnswersWithTripsAroundTheTime(t *testing.T) {
	service := service(t)
	text := call(t, service, "ns_trips", `{"from":"Eastbrook Centraal","to":"Westfield","date_time":"2026-03-10T08:30","arrive_by":true}`)
	if !strings.Contains(text, "arrive 08:34") {
		t.Errorf("arrive-by 08:30 answered %q, want the trip arriving at 08:34", text)
	}
	text = call(t, service, "ns_trips", `{"from":"Eastbrook Centraal","to":"Westfield","date_time":"2026-03-10T11:00","arrive_by":true}`)
	if !strings.Contains(text, "no trips") {
		t.Errorf("arrive-by 11:00 answered %q, want no trips outside the window", text)
	}
}

func TestUnknownQueryParameterIsRefused(t *testing.T) {
	fake := nssim.New(t, json.RawMessage(seed))
	response, err := http.Get(fake.BaseURL() + "/v3/trips?fromStation=EBK&toStation=WFD&dateTime=2026-03-10T08:00:00%2B01:00&bogus=1")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "bogus") {
		t.Fatalf("got %d %s, want a 400 naming the parameter", response.StatusCode, body)
	}
}

func TestValidateRejectsUnknownFields(t *testing.T) {
	if err := nssim.Validate(json.RawMessage(seed)); err != nil {
		t.Fatal(err)
	}
	if err := nssim.Validate(json.RawMessage(`{"stations":[],"nonsense":1}`)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

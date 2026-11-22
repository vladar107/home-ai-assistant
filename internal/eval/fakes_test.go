package eval

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	userlocation "github.com/vladar107/home-ai-assistant/internal/location"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	trainsSeed = `{
  "stations": [{"code": "EBK", "name": "Eastbrook Centraal"}, {"code": "WFD", "name": "Westfield"}],
  "trips": [{"from": "EBK", "to": "WFD", "trip": {
    "status": "NORMAL", "transfers": 0, "plannedDurationInMinutes": 24,
    "legs": [{"name": "IC 1100", "travelType": "PUBLIC_TRANSIT", "direction": "Westfield",
      "product": {"categoryCode": "IC", "number": "1100", "longCategoryName": "Intercity"},
      "origin": {"name": "Eastbrook Centraal", "stationCode": "EBK", "plannedDateTime": "2026-03-10T08:40:00+0100", "plannedTrack": "4"},
      "destination": {"name": "Westfield", "stationCode": "WFD", "plannedDateTime": "2026-03-10T09:04:00+0100"}}]}}]
}`
	busesSeed = `{
  "stops": [
    {"id": "100", "code": "30000001", "name": "Eastbrook, Market Square", "lat": 52.1, "lon": 5.1, "platform": "A"},
    {"id": "101", "code": "30000002", "name": "Westfield, Station", "lat": 52.2, "lon": 5.2}
  ],
  "routes": [{"id": "r340", "agency": "UOV", "short": "340", "long": "Eastbrook - Westfield"}],
  "timing_points": [{"code": "30000001", "passes": {"J1": {
    "LinePublicNumber": "340", "JourneyNumber": 1, "UserStopOrderNumber": 1,
    "DestinationName50": "Westfield", "TransportType": "BUS", "TripStopStatus": "PLANNED",
    "JourneyStopType": "FIRST", "TargetDepartureTime": "2026-03-10T08:45:00",
    "ExpectedDepartureTime": "2026-03-10T08:47:00"}}}]
}`
	weatherSeed = `{
  "places": [{"name": "Eastbrook", "latitude": 52.1, "longitude": 5.1, "country": "Netherlands",
    "country_code": "NL", "timezone": "Europe/Amsterdam"}],
  "forecasts": [{"latitude": 52.1, "longitude": 5.1, "timezone": "Europe/Amsterdam",
    "current": {"time": "2026-03-10T08:00", "temperature": 7.5, "apparent": 5, "humidity": 80,
      "precipitation": 0, "code": 3, "wind": 18, "gusts": 30},
    "days": [{"date": "2026-03-10", "code": 61, "high": 9, "low": 3, "rain": 4.2, "probability": 70,
      "wind": 25, "sunrise": "2026-03-10T06:45", "sunset": "2026-03-10T18:20"}]}]
}`
	pagesSeed = `{
  "pages": [{"host": "news.example", "path": "/market", "title": "Eastbrook market hours",
    "body": "<p>The Eastbrook market opens at 9:00 on Saturdays.</p>"}]
}`
	placesSearchSeed = `{
  "elements": [{"lat": 52.1010, "lon": 5.1020, "tags": {"amenity": "pharmacy", "name": "Eastbrook Pharmacy"}}]
}`
	flightsSeed = `{
  "flights": [{"number": "EX123", "date": "2026-03-10", "status": "Delayed", "airline": "Example Air",
    "departure": {"iata": "EBK", "name": "Eastbrook Airport", "city": "Eastbrook", "timezone": "Europe/Amsterdam",
      "scheduled": "2026-03-10T10:00:00Z", "revised": "2026-03-10T10:50:00Z"},
    "arrival": {"iata": "WFD", "name": "Westfield Airport", "city": "Westfield", "timezone": "Europe/Lisbon",
      "scheduled": "2026-03-10T13:10:00Z"}}]
}`
	monitoringSeed = `{
  "dashboards": [{"uid": "svc-health", "title": "Service health", "folder": "Operations",
    "document": {"title": "Service health", "panels": []}}]
}`
	nasSeed = `{
  "roots": [{"name": "home", "files": {"Documents/notes.md": "Buy milk and bread."}}]
}`
	homeSeed = `{
  "entities": [
    {"entity_id": "light.study_lamp", "name": "Study lamp", "state": "off", "area": "Study", "exposed": true}
  ],
  "tools": [{"name": "intent__HassTurnOn", "description": "Turns on a device or entity.",
    "input_schema": {"type": "object", "properties": {"name": {"type": "string"}}},
    "answer": "Turned on.", "effects": [{"entity_id": "light.study_lamp", "state": "on"}]}]
}`
	mcpSeed = `{
  "kitchen": {"tools": [{"name": "lookup_recipe", "description": "Finds a recipe by dish name.",
    "input_schema": {"type": "object", "properties": {"dish": {"type": "string"}}, "required": ["dish"]},
    "annotations": {"readOnlyHint": true}, "text": "No recipe found.",
    "answers": [{"arguments": {"dish": "soup"}, "text": "Soup: simmer vegetables for 30 minutes."}]}]}
}`
)

func TestSeededFakesAnswerTheToolsThatReadThem(t *testing.T) {
	t.Parallel()
	w := newTestWorld(t, worldConfig{sections: map[string]any{
		SectionTrains:       json.RawMessage(trainsSeed),
		SectionBuses:        json.RawMessage(busesSeed),
		SectionWeather:      json.RawMessage(weatherSeed),
		SectionPages:        json.RawMessage(pagesSeed),
		SectionPlacesSearch: json.RawMessage(placesSearchSeed),
		SectionFlights:      json.RawMessage(flightsSeed),
		SectionMonitoring:   json.RawMessage(monitoringSeed),
		SectionNAS:          json.RawMessage(nasSeed),
		SectionHome:         json.RawMessage(homeSeed),
		SectionMCP:          json.RawMessage(mcpSeed),
	}})
	checks := []struct{ tool, arguments, want string }{
		{"ns_trips", `{"from":"Eastbrook Centraal","to":"Westfield","date_time":"2026-03-10T08:30"}`, "IC 1100"},
		{"bus_stop_status", `{"stop":"Market Square"}`, "340"},
		{"weather_forecast", `{"location":"Eastbrook","days":1}`, "7.5"},
		{"fetch_page", `{"url":"http://news.example/market"}`, "opens at 9:00"},
		{"nearby_places", `{"latitude":52.1,"longitude":5.1,"kind":"pharmacy","radius":1000}`, "Eastbrook Pharmacy"},
		{"flight_status", `{"flight_number":"EX 123","date":"2026-03-10"}`, "Westfield"},
		{"grafana_dashboards", `{}`, "Service health"},
		{"nas_read_file", `{"path":"home/Documents/notes.md"}`, "Buy milk and bread."},
		{"ha_get_live_context", `{}`, "Study lamp"},
		{"mcp_kitchen_lookup_recipe", `{"dish":"soup"}`, "simmer vegetables"},
	}
	// A turn's tool budget is smaller than the list.
	for _, batch := range [][]struct{ tool, arguments, want string }{checks[:5], checks[5:]} {
		var lines []string
		for _, check := range batch {
			lines = append(lines, call(check.tool, check.arguments))
		}

		record := w.play(Step{Say: strings.Join(lines, "\n")})

		for _, check := range batch {
			result := w.toolResult(record, check.tool)
			if result.IsError || !strings.Contains(result.Result, check.want) {
				t.Errorf("%s: want %q, got error=%v %q", check.tool, check.want, result.IsError, result.Result)
			}
		}
	}
}

func TestConnectorWithoutASectionStaysUnconfigured(t *testing.T) {
	t.Parallel()
	w := newTestWorld(t, worldConfig{sections: map[string]any{SectionTrains: json.RawMessage(trainsSeed)}})

	offered := offeredTools(t, w)

	if !slices.Contains(offered, "ns_trips") {
		t.Errorf("ns_trips is not offered with a trains section: %v", offered)
	}
	for _, tool := range []string{"ha_get_live_context", "grafana_dashboards", "nas_read_file", "flight_status", "bus_stop_status", "weather_forecast", "fetch_page", "nearby_places"} {
		if slices.Contains(offered, tool) {
			t.Errorf("%s is offered without its section", tool)
		}
	}
	for _, tool := range offered {
		if strings.HasPrefix(tool, "mcp_") {
			t.Errorf("%s is offered without an mcp section", tool)
		}
	}
}

func TestPlacesReverseGeocodingReadsThePlacesSearchFake(t *testing.T) {
	t.Parallel()
	const reverse = `{"reverse": [{"lat": 52.1, "lon": 5.1, "type": "ADDRESS", "street": "Market Street", "house_number": "1",
    "areas": [{"name": "Eastbrook", "admin_level": 8, "default": true}]}]}`
	w := newTestWorld(t, worldConfig{sections: map[string]any{
		SectionPlaces:       []placeSeed{{Name: "Study", Lat: 52.1, Lon: 5.1}},
		SectionPlacesSearch: json.RawMessage(reverse),
	}})

	text, err := w.Assembly.Locations.Share(t.Context(), userlocation.Fix{
		PrincipalID: PrincipalID, Latitude: 52.1, Longitude: 5.1, Source: userlocation.SourcePin, UpdatedAt: w.Clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Market Street 1, Eastbrook") {
		t.Fatalf("got %q", text)
	}
}

func TestNewSectionsRejectBadSeeds(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		SectionTrains: `{"bogus": 1}`,
		SectionHome:   `{"entities": [{"entity_id": ""}]}`,
		SectionMCP:    `{"Kitchen": {"tools": [{"name": "t"}]}}`,
	} {
		if err := DefaultRegistry.worlds[name](json.RawMessage(raw)); err == nil {
			t.Errorf("%s accepted %s", name, raw)
		}
	}
	if err := DefaultRegistry.worlds[SectionMCP](json.RawMessage(`{"kitchen": {"tools": []}}`)); err == nil {
		t.Error("an mcp server without tools was accepted")
	}
}

// offeredTools plays a plain turn and returns the names of the tools the model was offered.
func offeredTools(t *testing.T, w testWorld) []string {
	t.Helper()
	record := w.play(Step{Say: "Hello."})
	if len(record.Runs) == 0 {
		t.Fatal("the turn made no run")
	}
	request := record.Runs[0].Request
	names := toolNames(request.Tools)
	// The runtime's own search extension is not a broker tool; the request says whether it is loaded.
	if request.WebSearch {
		names = append(names, "web_search")
	}
	return names
}

func toolNames(set agentruntime.ToolSet) []string {
	names := make([]string, 0, len(set.Definitions))
	for _, definition := range set.Definitions {
		names = append(names, definition.Name)
	}
	return names
}

// namedTools lists the tools a case's expectations name, each with where.
func namedTools(c Case) map[string][]string {
	named := map[string][]string{}
	for i, step := range c.Steps {
		if step.Expect == nil {
			continue
		}
		where := func(field string) string { return fmt.Sprintf("steps[%d].expect.%s", i, field) }
		for _, expected := range step.Expect.Calls {
			for _, tool := range expected.Names() {
				named[tool] = append(named[tool], where("calls"))
			}
		}
		for _, tool := range step.Expect.NotCalled {
			named[tool] = append(named[tool], where("not_called"))
		}
		if step.Expect.Proposals != nil {
			for _, tool := range step.Expect.Proposals.Tools {
				named[tool] = append(named[tool], where("proposals.tools"))
			}
		}
	}
	return named
}

// worldOffers builds the case's world with the Pi fixture and every switch on, and returns the tool
// names the model is offered for a chat turn.
func worldOffers(t *testing.T, c Case) []string {
	t.Helper()
	c.Steps = []Step{{Say: "Hello."}}
	c.User.Switches = map[string]bool{"widgets": true}
	return offeredTools(t, newCaseWorld(t, c, ""))
}

func TestCasesNameOnlyToolsTheirWorldOffers(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{filepath.Join("..", "..", "evals", "cases"), filepath.Join("testdata", "offered")} {
		cases, err := LoadCases(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			t.Run(filepath.Base(c.File), func(t *testing.T) {
				t.Parallel()
				offered := worldOffers(t, c)
				for tool, places := range namedTools(c) {
					if !slices.Contains(offered, tool) {
						t.Errorf("%s: %s names %s, which the case's world does not offer", c.File, strings.Join(places, ", "), tool)
					}
				}
				assertWorldOnlyTools(t, c, offered)
			})
		}
	}
}

// assertWorldOnlyTools checks that a home or catalogue tool is offered only when the case's world
// defines it.
func assertWorldOnlyTools(t *testing.T, c Case, offered []string) {
	t.Helper()
	_, hasHome := c.World[SectionHome]
	_, hasMCP := c.World[SectionMCP]
	for _, tool := range offered {
		switch {
		case strings.HasPrefix(tool, "ha_") && !hasHome:
			t.Errorf("%s: %s is offered although the world has no home section", c.File, tool)
		case strings.HasPrefix(tool, "mcp_") && !hasMCP:
			t.Errorf("%s: %s is offered although the world has no mcp section", c.File, tool)
		}
	}
}

func TestToolNameCheckFailsForAToolTheWorldLacks(t *testing.T) {
	t.Parallel()
	cases, err := LoadCases(filepath.Join("testdata", "offered"))
	if err != nil {
		t.Fatal(err)
	}
	var plain Case
	for _, c := range cases {
		if c.ID == "offered-plain" {
			plain = c
		}
	}
	offered := worldOffers(t, plain)
	if slices.Contains(offered, "ha_turn_on") || slices.Contains(offered, "mcp_kitchen_lookup_recipe") {
		t.Fatalf("a world without home or catalogue sections offers them: %v", offered)
	}
	plain.Steps = []Step{{Say: "x", Expect: &Expect{NotCalled: []string{"ha_turn_on"}}}}
	var missing []string
	for tool := range namedTools(plain) {
		if !slices.Contains(offered, tool) {
			missing = append(missing, tool)
		}
	}
	if !slices.Equal(missing, []string{"ha_turn_on"}) {
		t.Fatalf("missing = %v, want ha_turn_on", missing)
	}
}

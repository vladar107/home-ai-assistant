package places_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/places"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const contact = "ops@example.org"

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

type request struct {
	method string
	path   string
	agent  string
	query  string
	form   string
}

// simulator stands in for Overpass and Transitous, and records every request.
type simulator struct {
	mu           sync.Mutex
	overpass     string
	overpassCode int
	geocode      string
	geocodeCode  int
	requests     []request
}

func (s *simulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, request{method: r.Method, path: r.URL.Path, agent: r.UserAgent(), query: r.URL.RawQuery, form: string(body)})
	overpass, code, geocode, geocodeCode := s.overpass, s.overpassCode, s.geocode, s.geocodeCode
	s.mu.Unlock()
	switch r.URL.Path {
	case "/overpass":
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write([]byte(overpass))
	case "/transitous/v1/reverse-geocode":
		if geocodeCode != 0 {
			w.WriteHeader(geocodeCode)
			return
		}
		_, _ = w.Write([]byte(geocode))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *simulator) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, r := range s.requests {
		if r.path == path {
			total++
		}
	}
	return total
}

func (s *simulator) last() request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[len(s.requests)-1]
}

// Offsets of 0.001 degrees of latitude are about 111 m.
const overpassBody = `{"elements":[
 {"type":"node","id":1,"lat":52.003,"lon":4.5,"tags":{"amenity":"pharmacy","name":"Far Apotheek","opening_hours":"Mo-Fr 09:00-18:00"}},
 {"type":"node","id":2,"lat":52.001,"lon":4.5,"tags":{"amenity":"pharmacy","name":"Near Apotheek","addr:street":"Kerkstraat","addr:housenumber":"5","addr:city":"Leiden","phone":"+31 71 000","website":"https://near.example"}},
 {"type":"way","id":3,"center":{"lat":52.0,"lon":4.502},"tags":{"healthcare":"pharmacy"}},
 {"type":"node","id":4,"lat":52.002,"lon":4.5,"tags":{"amenity":"pharmacy","name":"Medium Pharmacy","brand":"Etos"}}
]}`

func newService(t *testing.T, sim *simulator, now func() time.Time) *places.Service {
	t.Helper()
	server := httptest.NewServer(sim)
	t.Cleanup(server.Close)
	service, err := places.NewService(places.Config{
		OverpassURL:   server.URL + "/overpass",
		TransitousURL: server.URL + "/transitous",
		Contact:       contact,
		Now:           now,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func call(t *testing.T, service *places.Service, arguments string) tools.Result {
	t.Helper()
	result, err := service.Execute(context.Background(), "principal-1", tools.Call{Name: places.ToolNearbyPlaces, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestKindsBecomeOverpassFilters(t *testing.T) {
	cases := map[string][]string{
		"pharmacy":    {`nwr["amenity"="pharmacy"](around:1500,52.000000,4.500000)`, `nwr["healthcare"="pharmacy"](around:1500,52.000000,4.500000)`},
		"atm":         {`nwr["amenity"="atm"]`, `nwr["amenity"="bank"]["atm"="yes"]`},
		"supermarket": {`nwr["shop"="supermarket"]`},
		"bike_repair": {`nwr["shop"="bicycle"]["service:bicycle:repair"="yes"]`, `nwr["amenity"="bicycle_repair_station"]`},
		"doctor":      {`nwr["amenity"="doctors"]`, `nwr["amenity"="clinic"]`},
		"ev_charging": {`nwr["amenity"="charging_station"]`},
	}
	for kind, filters := range cases {
		t.Run(kind, func(t *testing.T) {
			sim := &simulator{overpass: `{"elements":[]}`}
			service := newService(t, sim, nil)
			result := call(t, service, `{"latitude":52,"longitude":4.5,"kind":"`+kind+`","radius":1500,"name":"SECRETNAME"}`)
			if result.IsError {
				t.Fatalf("unexpected error: %s", result.Text)
			}
			sent := sim.last()
			if sent.method != http.MethodPost || !strings.Contains(sent.agent, contact) {
				t.Fatalf("request = %+v", sent)
			}
			if strings.Contains(sent.form, "SECRETNAME") {
				t.Fatalf("user text reached the query: %s", sent.form)
			}
			query := mustDecodeForm(t, sent.form)
			if !strings.HasPrefix(query, "[out:json][timeout:10];(") || !strings.Contains(query, "out center tags") {
				t.Fatalf("query = %s", query)
			}
			for _, filter := range filters {
				if !strings.Contains(query, filter) {
					t.Fatalf("query %s lacks %s", query, filter)
				}
			}
			if !strings.Contains(query, "(around:1500,52.000000,4.500000)") {
				t.Fatalf("query lacks around clause: %s", query)
			}
		})
	}
}

func mustDecodeForm(t *testing.T, form string) string {
	t.Helper()
	values, err := parseForm(form)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func parseForm(form string) (string, error) {
	request, err := http.NewRequest(http.MethodPost, "http://x", strings.NewReader(form))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := request.ParseForm(); err != nil {
		return "", err
	}
	return request.PostForm.Get("data"), nil
}

func TestResultsSortFilterAndLimit(t *testing.T) {
	sim := &simulator{overpass: overpassBody}
	service := newService(t, sim, nil)

	result := call(t, service, `{"latitude":52,"longitude":4.5,"kind":"pharmacy"}`)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Text)
	}
	text := result.Text
	if !strings.HasPrefix(text, "4 pharmacy within 1000 m") {
		t.Fatalf("header = %s", text)
	}
	near, medium, far := strings.Index(text, "Near Apotheek"), strings.Index(text, "Medium Pharmacy"), strings.Index(text, "Far Apotheek")
	if near < 0 || near > medium || medium > far {
		t.Fatalf("not sorted by distance:\n%s", text)
	}
	for _, want := range []string{
		"Near Apotheek, 110 m N; Kerkstraat 5, Leiden; opening hours: not listed; phone +31 71 000; https://near.example",
		"unnamed pharmacy, 140 m E",
		"Far Apotheek, 330 m N; opening hours: Mo-Fr 09:00-18:00",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}

	limited := call(t, service, `{"latitude":52,"longitude":4.5,"kind":"pharmacy","limit":2}`)
	if !strings.HasPrefix(limited.Text, "2 pharmacy") || strings.Contains(limited.Text, "Far Apotheek") {
		t.Fatalf("limit not applied:\n%s", limited.Text)
	}

	byName := call(t, service, `{"latitude":52,"longitude":4.5,"kind":"pharmacy","name":"etos"}`)
	if !strings.HasPrefix(byName.Text, "1 pharmacy") || !strings.Contains(byName.Text, "Medium Pharmacy") {
		t.Fatalf("name filter by brand failed:\n%s", byName.Text)
	}
	none := call(t, service, `{"latitude":52,"longitude":4.5,"kind":"pharmacy","name":"nothing like it"}`)
	if none.IsError || !strings.Contains(none.Text, "No pharmacy found") || !strings.Contains(none.Text, "bigger radius") {
		t.Fatalf("empty result = %+v", none)
	}
	if sim.count("/overpass") != 1 {
		t.Fatalf("name, limit, and repeats should share one fetch, got %d", sim.count("/overpass"))
	}
}

func TestAnswersAreCachedForTenMinutes(t *testing.T) {
	sim := &simulator{overpass: overpassBody}
	c := &clock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	service := newService(t, sim, c.Now)
	arguments := `{"latitude":52,"longitude":4.5,"kind":"pharmacy"}`

	call(t, service, arguments)
	c.advance(9 * time.Minute)
	call(t, service, `{"latitude":52.00004,"longitude":4.50004,"kind":"pharmacy"}`)
	if sim.count("/overpass") != 1 {
		t.Fatalf("expected one upstream request, got %d", sim.count("/overpass"))
	}
	call(t, service, `{"latitude":52,"longitude":4.5,"kind":"cafe"}`)
	if sim.count("/overpass") != 2 {
		t.Fatalf("another kind must not share the entry, got %d", sim.count("/overpass"))
	}
	c.advance(2 * time.Minute)
	call(t, service, arguments)
	if sim.count("/overpass") != 3 {
		t.Fatalf("expected a refetch after expiry, got %d", sim.count("/overpass"))
	}
}

func TestOverpassFailureTellsTheModelToFallBack(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusGatewayTimeout} {
		sim := &simulator{overpassCode: code}
		service := newService(t, sim, nil)
		result := call(t, service, `{"latitude":52,"longitude":4.5,"kind":"pharmacy"}`)
		if !result.IsError || !strings.Contains(result.Text, "unavailable") || !strings.Contains(result.Text, "web search") {
			t.Fatalf("status %d: result = %+v", code, result)
		}
	}
}

func TestInputsAreValidated(t *testing.T) {
	sim := &simulator{overpass: overpassBody}
	service := newService(t, sim, nil)
	for name, arguments := range map[string]string{
		"missing latitude": `{"longitude":4.5,"kind":"pharmacy"}`,
		"latitude range":   `{"latitude":91,"longitude":4.5,"kind":"pharmacy"}`,
		"longitude range":  `{"latitude":52,"longitude":-181,"kind":"pharmacy"}`,
		"unknown kind":     `{"latitude":52,"longitude":4.5,"kind":"casino"}`,
		"radius low":       `{"latitude":52,"longitude":4.5,"kind":"cafe","radius":50}`,
		"radius high":      `{"latitude":52,"longitude":4.5,"kind":"cafe","radius":3001}`,
		"limit zero":       `{"latitude":52,"longitude":4.5,"kind":"cafe","limit":0}`,
		"limit high":       `{"latitude":52,"longitude":4.5,"kind":"cafe","limit":11}`,
		"long name":        `{"latitude":52,"longitude":4.5,"kind":"cafe","name":"` + strings.Repeat("a", 61) + `"}`,
		"extra field":      `{"latitude":52,"longitude":4.5,"kind":"cafe","query":"x"}`,
		"not json":         `[]`,
	} {
		if result := call(t, service, arguments); !result.IsError {
			t.Errorf("%s: expected a refusal, got %+v", name, result)
		}
	}
	if sim.count("/overpass") != 0 {
		t.Fatalf("refused calls must not reach Overpass")
	}
}

func TestToolIsOfferedToEveryPrincipal(t *testing.T) {
	service := newService(t, &simulator{}, nil)
	contribution, err := service.Contribute(context.Background(), "anyone")
	if err != nil {
		t.Fatal(err)
	}
	if len(contribution.Definitions) != 1 || contribution.Definitions[0].Name != places.ToolNearbyPlaces || contribution.Definitions[0].Risk != tools.RiskRead {
		t.Fatalf("contribution = %+v", contribution)
	}
	broker, err := tools.NewBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(service); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.ToolsFor(context.Background(), "anyone"); err != nil {
		t.Fatalf("definitions rejected by the broker: %v", err)
	}
}

const geocodeBody = `[
 {"type":"STOP","name":"Leiden Centraal","lat":52.0,"lon":4.5,"areas":[{"name":"Leiden","adminLevel":8,"default":true}]},
 {"type":"PLACE","name":"Some Park","areas":[{"name":"Leiden","adminLevel":8}]},
 {"type":"ADDRESS","name":"Stationsplein","street":"Stationsplein","houseNumber":"1","areas":[
   {"name":"2312","adminLevel":9,"zip":true},
   {"name":"Zuid-Holland","adminLevel":4},
   {"name":"Binnenstad","adminLevel":10},
   {"name":"Leiden","adminLevel":8,"matched":true,"default":true}]}
]`

func TestDescribeNamesThePositionAndCachesByGrid(t *testing.T) {
	sim := &simulator{geocode: geocodeBody}
	c := &clock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	service := newService(t, sim, c.Now)

	name, err := service.Describe(context.Background(), 52.0, 4.5)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Stationsplein 1, Leiden" {
		t.Fatalf("name = %q", name)
	}
	sent := sim.last()
	if sent.path != "/transitous/v1/reverse-geocode" || sent.query != "place=52.000000%2C4.500000" || !strings.Contains(sent.agent, contact) {
		t.Fatalf("request = %+v", sent)
	}

	c.advance(23 * time.Hour)
	if _, err := service.Describe(context.Background(), 52.0001, 4.5001); err != nil {
		t.Fatal(err)
	}
	if sim.count("/transitous/v1/reverse-geocode") != 1 {
		t.Fatalf("a nearby point should share the entry, got %d requests", sim.count("/transitous/v1/reverse-geocode"))
	}
	if _, err := service.Describe(context.Background(), 52.01, 4.5); err != nil {
		t.Fatal(err)
	}
	if sim.count("/transitous/v1/reverse-geocode") != 2 {
		t.Fatalf("a distant point needs its own request")
	}
	c.advance(2 * time.Hour)
	if _, err := service.Describe(context.Background(), 52.0, 4.5); err != nil {
		t.Fatal(err)
	}
	if sim.count("/transitous/v1/reverse-geocode") != 3 {
		t.Fatalf("expected a refetch after a day")
	}
}

func TestDescribeRemembersAFailureBriefly(t *testing.T) {
	sim := &simulator{geocode: geocodeBody, geocodeCode: http.StatusServiceUnavailable}
	c := &clock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	service := newService(t, sim, c.Now)

	if _, err := service.Describe(context.Background(), 52.0, 4.5); err == nil {
		t.Fatal("expected the failure")
	}
	failed := sim.count("/transitous/v1/reverse-geocode")
	if _, err := service.Describe(context.Background(), 52.01, 4.5); err == nil {
		t.Fatal("expected the remembered failure")
	}
	if got := sim.count("/transitous/v1/reverse-geocode"); got != failed {
		t.Fatalf("a failure should spare the next point a request, got %d requests after %d", got, failed)
	}

	sim.mu.Lock()
	sim.geocodeCode = 0
	sim.mu.Unlock()
	c.advance(time.Minute)
	name, err := service.Describe(context.Background(), 52.0, 4.5)
	if err != nil || name != "Stationsplein 1, Leiden" {
		t.Fatalf("after the pause: %q, %v", name, err)
	}
}

func TestDescribeFallsBackAndHandlesEmptyAnswers(t *testing.T) {
	sim := &simulator{geocode: `[{"type":"STOP","name":"Leiden Centraal","areas":[{"name":"Leiden","adminLevel":8}]}]`}
	service := newService(t, sim, nil)
	name, err := service.Describe(context.Background(), 52.0, 4.5)
	if err != nil || name != "Leiden Centraal, Leiden" {
		t.Fatalf("name = %q, err = %v", name, err)
	}

	sim.mu.Lock()
	sim.geocode = `[]`
	sim.mu.Unlock()
	name, err = service.Describe(context.Background(), -33.0, 151.0)
	if err != nil || name != "" {
		t.Fatalf("empty answer gave %q, %v", name, err)
	}
	if _, err := service.Describe(context.Background(), 100, 0); err == nil {
		t.Fatal("expected an error for out-of-range coordinates")
	}
}

func TestLifecycle(t *testing.T) {
	service := newService(t, &simulator{}, nil)
	if err := service.Ready(context.Background()); err == nil {
		t.Fatal("not ready before start")
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

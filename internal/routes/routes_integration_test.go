package routes_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/connectors/bus"
	"github.com/vladar107/home-ai-assistant/internal/connectors/ns"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

// The NS fixtures plan trips on Monday 5 October 2026 around 08:00 in Amsterdam; the OVapi fixture
// was captured at 18:15 on Sunday 4 October.
var (
	nsMorning   = time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)
	busCaptured = time.Date(2026, 10, 4, 16, 15, 0, 0, time.UTC)
)

const nsPrefix = "/reisinformatie-api/api/"

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

// gateway stands in for NS, for OVapi with its GTFS directory, and for a Transitous timetable with
// nothing in it, serving recorded responses.
type gateway struct {
	t *testing.T

	mu    sync.Mutex
	trips []byte
	// longer answers a trips query that asks for extra transfer time, when set.
	longer      []byte
	tripQueries []url.Values
	status      int
	points      map[string]json.RawMessage
	archive     []byte
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.URL.Path == nsPrefix+"v2/stations":
		_, _ = w.Write(fixture(g.t, "ns", "stations.json"))
	case r.URL.Path == nsPrefix+"v3/trips":
		g.tripQueries = append(g.tripQueries, r.URL.Query())
		if g.status != 0 {
			w.WriteHeader(g.status)
			_, _ = w.Write([]byte("{}"))
			return
		}
		if g.longer != nil && r.URL.Query().Get("addChangeTime") != "" {
			_, _ = w.Write(g.longer)
			return
		}
		_, _ = w.Write(g.trips)
	case r.URL.Path == nsPrefix+"v2/departures":
		_, _ = w.Write(fixture(g.t, "ns", "departures.json"))
	case strings.HasPrefix(r.URL.Path, "/ovapi/tpc/"):
		answer := make(map[string]json.RawMessage)
		for _, code := range strings.Split(strings.TrimPrefix(r.URL.Path, "/ovapi/tpc/"), ",") {
			if point, found := g.points[code]; found {
				answer[code] = point
			}
		}
		_ = json.NewEncoder(w).Encode(answer)
	case r.URL.Path == "/transitous/v5/stoptimes":
		_, _ = w.Write([]byte(`{"stopTimes":[]}`))
	case r.URL.Path == "/gtfs/gtfs-nl.zip":
		http.ServeContent(w, r, "gtfs-nl.zip", time.Time{}, bytes.NewReader(g.archive))
	default:
		http.NotFound(w, r)
	}
}

func (g *gateway) serveTrips(body []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.trips, g.status = body, 0
}

// asked lists the trips queries answered so far.
func (g *gateway) asked() []url.Values {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.tripQueries)
}

func (g *gateway) serveLonger(body []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.longer = body
}

func (g *gateway) fail(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status = status
}

func fixture(t *testing.T, connector, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "connectors", connector, "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// tripsVariant is the recorded trips with the first one, the 07:56, changed by change.
func tripsVariant(t *testing.T, change func(trip map[string]any)) []byte {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(fixture(t, "ns", "trips.json"), &response); err != nil {
		t.Fatal(err)
	}
	change(response["trips"].([]any)[0].(map[string]any))
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func gtfsArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for _, entry := range []struct {
		name string
		body []byte
	}{
		{"agency.txt", []byte("agency_id,agency_name\r\nUOV,U-OV\r\n")},
		{"routes.txt", fixture(t, "bus", "routes.txt")},
		{"stops.txt", fixture(t, "bus", "stops.txt")},
	} {
		writer, err := archive.CreateHeader(&zip.FileHeader{Name: entry.name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// proposals records what the route tools propose, standing in for the approval service.
type proposals struct {
	mu   sync.Mutex
	list []approval.Proposal
}

func (p *proposals) Propose(_ context.Context, _ string, proposal approval.Proposal) (approval.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.list = append(p.list, proposal)
	return approval.Record{}, nil
}

// Act records the change as a proposal, as it is made outside a clean chat turn.
func (p *proposals) Act(ctx context.Context, principalID string, proposal approval.Proposal) (string, error) {
	_, err := p.Propose(ctx, principalID, proposal)
	return approval.ProposedText, err
}

func (p *proposals) last(t *testing.T) approval.Proposal {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.list) == 0 {
		t.Fatal("nothing was proposed")
	}
	return p.list[len(p.list)-1]
}

type harness struct {
	t         *testing.T
	ctx       context.Context
	store     *sqlite.Store
	gateway   *gateway
	nsClock   *clock
	trains    *ns.Service
	service   *routes.Service
	proposals *proposals
	location  *time.Location
	saved     int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	httpretry.WithoutWaits(t)
	ctx := t.Context()
	state, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if err := state.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	var points map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t, "bus", "tpc_amersfoort.json"), &points); err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t: t, ctx: ctx, store: state,
		gateway:   &gateway{t: t, trips: fixture(t, "ns", "trips.json"), points: points, archive: gtfsArchive(t)},
		nsClock:   &clock{now: nsMorning},
		proposals: &proposals{},
	}
	server := httptest.NewServer(h.gateway)
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	switches, err := integration.NewSwitches(state, logger)
	if err != nil {
		t.Fatal(err)
	}
	trains, err := ns.NewService(ns.Config{APIKey: "test-key", BaseURL: server.URL + strings.TrimSuffix(nsPrefix, "/"), Now: h.nsClock.Now}, switches, logger)
	if err != nil {
		t.Fatal(err)
	}
	busClock := &clock{now: busCaptured}
	buses, err := bus.NewService(bus.Config{OVapiURL: server.URL + "/ovapi", GTFSURL: server.URL + "/gtfs", TransitousURL: server.URL + "/transitous", Now: busClock.Now}, switches, logger)
	if err != nil {
		t.Fatal(err)
	}
	h.trains = trains
	if h.location, err = time.LoadLocation("Europe/Amsterdam"); err != nil {
		t.Fatal(err)
	}
	h.service, err = routes.NewService(routes.Config{Location: h.location, Trains: trains, Buses: buses, Now: h.nsClock.Now}, state, h.proposals, logger)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) save(principalID string, spec routes.Spec) {
	h.t.Helper()
	h.saved++
	now := h.nsClock.Now().Add(time.Duration(h.saved) * time.Second)
	id := strings.Repeat("0", 15) + string(rune('a'+h.saved))
	created, err := h.store.CreateRoute(h.ctx, routes.Route{ID: id, PrincipalID: principalID, Spec: spec, CreatedAt: now, UpdatedAt: now}, routes.MaximumRoutesPerPrincipal)
	if err != nil || !created {
		h.t.Fatalf("save route: %v %v", created, err)
	}
}

func (h *harness) call(principalID, name string, arguments any) tools.Result {
	h.t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		h.t.Fatal(err)
	}
	result, err := h.service.Execute(h.ctx, principalID, tools.Call{Name: name, Arguments: raw})
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *harness) day(value string) time.Time {
	h.t.Helper()
	day, err := time.ParseInLocation("2006-01-02", value, h.location)
	if err != nil {
		h.t.Fatal(err)
	}
	return day
}

func wantText(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("missing %q in:\n%s", fragment, text)
		}
	}
}

// work is a weekday commute: bike to the station, one of two fixed trains, and a walk at the end.
func work() routes.Spec {
	return routes.Spec{
		Name:     "work",
		Days:     []string{"mon", "tue", "wed", "thu", "fri"},
		ArriveBy: "08:45",
		Legs: []routes.Leg{
			{Mode: routes.ModeBike, Minutes: 6},
			{Mode: routes.ModeTrain, From: "Amersfoort Schothorst", To: "Utrecht Centraal", Departures: []string{"07:56", "08:12"}},
			{Mode: routes.ModeWalk, Minutes: 10},
		},
	}
}

func TestRouteCheckTimesEachLegAndTheArrival(t *testing.T) {
	h := newHarness(t)
	h.save("alice", work())
	monday := h.day("2026-10-05")

	result, err := h.service.Check(h.ctx, "alice", "", monday)
	if err != nil {
		t.Fatal(err)
	}
	if result.Route.Name != "work" || result.Verdict != routes.VerdictMet || len(result.FailedLegs) != 0 || len(result.Legs) != 3 {
		t.Fatalf("on time = %+v", result)
	}
	clock := func(at time.Time) string { return at.In(h.location).Format("15:04") }
	if train := result.Legs[1]; train.Status != "on time" || train.Taken != 0 || clock(train.Start) != "07:56" || clock(train.End) != "08:23" {
		t.Fatalf("train leg = %+v", train)
	}
	if clock(result.Leave) != "07:50" || clock(result.Arrival) != "08:33" {
		t.Fatalf("leave %s, arrive %s", clock(result.Leave), clock(result.Arrival))
	}
	wantText(t, h.call("alice", "route_check", map[string]any{"name": "Work", "date": "2026-10-05"}).Text,
		"Check of the route work: Mon–Fri, arrive by 08:45 on Mon 5 Oct",
		"Leg 1, bike 6 min: as stated, 07:50–07:56\n",
		"Leg 2, train Amersfoort Schothorst → Utrecht Centraal, departures 07:56, 08:12: on time, taking the 07:56, 07:56–08:23\n   1. on time — the 07:56 from Amersfoort Schothorst to Utrecht Centraal\n",
		"   2. on time — the 08:12 from Amersfoort Schothorst to Utrecht Centraal\n",
		"Leg 3, walk 10 min: as stated, 08:23–08:33\n",
		"Result: arrives 08:33; the arrival target 08:45 is met with 12 min to spare. Leave by 07:50.\nLegs that failed to check: none.",
	)

	// The 07:56 is cancelled: the route takes the 08:12 instead and misses the target.
	h.nsClock.advance(time.Minute)
	h.gateway.serveTrips(tripsVariant(t, func(trip map[string]any) {
		trip["status"] = "CANCELLED"
		trip["legs"].([]any)[0].(map[string]any)["cancelled"] = true
	}))
	result, err = h.service.Check(h.ctx, "alice", "work", monday)
	if err != nil {
		t.Fatal(err)
	}
	if train := result.Legs[1]; train.Departures[0].Status != transit.StatusCancelled || train.Taken != 1 || result.Verdict != routes.VerdictMissed {
		t.Fatalf("cancelled = %+v", result)
	}
	wantText(t, h.service.Render(result),
		"on time, taking the 08:12, 08:12–08:39\n   1. cancelled — the 07:56 from Amersfoort Schothorst",
		"Result: arrives 08:49; the arrival target 08:45 is missed by 4 min. Leave by 08:06.",
	)

	// NS cannot be asked: the leg failed to check, and the arrival is unknown rather than guessed.
	h.nsClock.advance(time.Minute)
	h.gateway.fail(http.StatusServiceUnavailable)
	result, err = h.service.Check(h.ctx, "alice", "work", monday)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != routes.VerdictUnknown || len(result.FailedLegs) != 1 || result.FailedLegs[0] != 2 || !result.Arrival.IsZero() {
		t.Fatalf("failed check = %+v", result)
	}
	wantText(t, h.service.Render(result),
		"Leg 2, train Amersfoort Schothorst → Utrecht Centraal, departures 07:56, 08:12: check failed\n   1. check failed — the 07:56",
		"Leg 3, walk 10 min: as stated\n",
		"Result: the arrival cannot be worked out (leg 2: check failed), so whether the arrival target 08:45 is met is unknown.",
		"Legs that failed to check: 2.",
	)
}

func TestRouteCheckTimesABusLegByItsLiveDeparture(t *testing.T) {
	h := newHarness(t)
	h.save("alice", routes.Spec{
		Name: "evening",
		Days: []string{"sun"},
		Legs: []routes.Leg{
			{Mode: routes.ModeWalk, Minutes: 4},
			{Mode: routes.ModeBus, Stop: "Amersfoort, Stadhuis", Line: "3", Departures: []string{"18:18"}, Minutes: 12},
		},
	})
	result, err := h.service.Check(h.ctx, "alice", "", h.day("2026-10-04"))
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, h.service.Render(result),
		"Leg 2, bus 3 from Amersfoort, Stadhuis, departures 18:18, ride 12 min: delayed +1 min, taking the 18:18, 18:18–18:30\n   1. delayed +1 min — the 18:18 from Amersfoort, Stadhuis to Centraal Station",
		"Leg 1, walk 4 min: as stated, 18:14–18:18\n",
		"Result: arrives 18:30; no arrival target is saved. Leave by 18:14.",
	)
}

func TestRouteCheckChoosesTheRouteByWeekday(t *testing.T) {
	h := newHarness(t)
	h.save("alice", work())
	weekend := work()
	weekend.Name, weekend.Days, weekend.ArriveBy = "weekend", []string{"sat", "sun"}, ""
	h.save("alice", weekend)

	for _, test := range []struct{ date, want string }{{"2026-10-05", "work"}, {"2026-10-10", "weekend"}} {
		result, err := h.service.Check(h.ctx, "alice", "", h.day(test.date))
		if err != nil || result.Route.Name != test.want {
			t.Fatalf("route on %s = %q %v, want %s", test.date, result.Route.Name, err, test.want)
		}
	}
	// Without a date the check is for today, a Monday.
	if result, err := h.service.Check(h.ctx, "alice", "", time.Time{}); err != nil || result.Route.Name != "work" || result.Date.Format("2006-01-02") != "2026-10-05" {
		t.Fatalf("today = %q on %s, %v", result.Route.Name, result.Date, err)
	}

	gym := work()
	gym.Name, gym.Days = "gym", []string{"mon"}
	h.save("alice", gym)
	var selection *routes.SelectionError
	if _, err := h.service.Check(h.ctx, "alice", "", h.day("2026-10-05")); !errors.As(err, &selection) {
		t.Fatalf("two Monday routes = %v", err)
	}
	wantText(t, selection.Text, `Several routes apply on Monday: "work" (Mon–Fri), "gym" (Mon). Name one.`)
	if result := h.call("alice", "route_check", map[string]any{"date": "2026-10-07"}); result.IsError || !strings.Contains(result.Text, "Check of the route work:") {
		t.Fatalf("Wednesday = %+v", result)
	}
	if result := h.call("alice", "route_check", map[string]any{"name": "school"}); !result.IsError || !strings.Contains(result.Text, `No route is called "school"`) {
		t.Fatalf("unknown name = %+v", result)
	}
}

func TestRoutesBelongToTheirOwner(t *testing.T) {
	h := newHarness(t)
	h.save("alice", work())

	if result := h.call("bob", "route_list", map[string]any{}); result.Text != "The user has no saved routes." {
		t.Fatalf("bob's list = %+v", result)
	}
	for _, call := range []struct {
		name      string
		arguments map[string]any
	}{
		{"route_get", map[string]any{"name": "work"}},
		{"route_check", map[string]any{"name": "work"}},
		{"route_update", map[string]any{"name": "work", "arrive_by": "09:00"}},
		{"route_remove", map[string]any{"name": "work"}},
	} {
		if result := h.call("bob", call.name, call.arguments); !result.IsError || !strings.Contains(result.Text, "no saved routes") {
			t.Fatalf("bob's %s = %+v", call.name, result)
		}
	}
	routesOfAlice, err := h.service.Routes(h.ctx, "alice")
	if err != nil || len(routesOfAlice) != 1 {
		t.Fatalf("alice's routes = %v %v", routesOfAlice, err)
	}
	if deleted, err := h.service.Delete(h.ctx, "bob", routesOfAlice[0].ID); err != nil || deleted {
		t.Fatalf("bob deleted alice's route: %v %v", deleted, err)
	}
	if result := h.call("bob", "route_save", map[string]any{"name": "work", "days": []string{"mon"}, "legs": []any{
		map[string]any{"mode": "train", "from": "Amersfoort Schothorst", "to": "Utrecht", "departures": []string{"07:56"}},
	}}); result.IsError {
		t.Fatalf("bob may use a name alice uses: %+v", result)
	}
}

func TestRouteProposalsNeverInventLegValues(t *testing.T) {
	h := newHarness(t)
	for _, test := range []struct {
		legs []any
		want string
	}{
		{[]any{map[string]any{"mode": "walk"}, map[string]any{"mode": "train", "from": "Amersfoort Schothorst", "to": "Utrecht"}}, "leg 1: a walk leg needs the minutes the user stated; ask them"},
		{[]any{map[string]any{"mode": "bike", "minutes": 8}}, "a route needs at least one train or bus leg"},
		{[]any{map[string]any{"mode": "train", "from": "Amersfoort Schothorst"}}, "leg 1: a train leg needs the from and to stations"},
		{[]any{map[string]any{"mode": "bus", "stop": "Amersfoort, Stadhuis"}}, "leg 1: a bus leg needs the stop and the line"},
		{[]any{map[string]any{"mode": "train", "from": "Amersfoort Schothorst", "to": "Nowhere Junction"}}, "leg 1: "},
	} {
		result := h.call("alice", "route_save", map[string]any{"name": "work", "days": []string{"mon"}, "legs": test.legs})
		if !result.IsError || !strings.Contains(result.Text, test.want) {
			t.Fatalf("legs %v = %+v, want %q", test.legs, result, test.want)
		}
	}
	if result := h.call("alice", "route_save", map[string]any{"name": "work", "legs": []any{map[string]any{"mode": "bus", "stop": "Stadhuis", "line": "3"}}}); !result.IsError || !strings.Contains(result.Text, "weekdays") {
		t.Fatalf("no weekdays = %+v", result)
	}
	h.proposals.mu.Lock()
	proposed := len(h.proposals.list)
	h.proposals.mu.Unlock()
	if proposed != 0 {
		t.Fatalf("%d proposals from invalid routes", proposed)
	}

	// Names are looked up when proposed, so the summary shows the stations the check will use.
	result := h.call("alice", "route_save", map[string]any{"name": " work ", "days": []string{"fri", "mon"}, "arrive_by": "08:45", "legs": []any{
		map[string]any{"mode": "bike", "minutes": 6},
		map[string]any{"mode": "train", "from": "amfs", "to": "Utrecht", "departures": []string{"08:12", "07:56"}},
		map[string]any{"mode": "bus", "stop": "Stadhuis", "line": "3", "towards": "Centraal"},
	}})
	if result.IsError {
		t.Fatal(result.Text)
	}
	proposal := h.proposals.last(t)
	if proposal.Tool != "route_save" || proposal.Summary != "Save the route work: Mon, Fri, arrive by 08:45\n1. bike 6 min\n2. train Amersfoort Schothorst → Utrecht Centraal, departures 07:56, 08:12\n3. bus 3 from Amersfoort, Stadhuis towards Centraal, no fixed departures" {
		t.Fatalf("proposal = %+v", proposal)
	}
}

func TestRouteUpdatesShowOnlyWhatChanges(t *testing.T) {
	h := newHarness(t)
	h.save("alice", work())

	h.call("alice", "route_update", map[string]any{"name": "work", "leg_index": 2, "leg": map[string]any{"departures": []string{"07:56", "08:12", "08:34"}}})
	if proposal := h.proposals.last(t); proposal.Tool != "route_update" || proposal.Summary != "Change the route work:\nleg 2: train Amersfoort Schothorst → Utrecht Centraal, departures 07:56, 08:12 → train Amersfoort Schothorst → Utrecht Centraal, departures 07:56, 08:12, 08:34" {
		t.Fatalf("one leg = %+v", proposal)
	}
	h.call("alice", "route_update", map[string]any{"name": "work", "arrive_by": "", "days": []string{"mon", "tue"}, "leg_index": 3, "leg_action": "insert", "leg": map[string]any{"mode": "walk", "minutes": 3}})
	if proposal := h.proposals.last(t); proposal.Summary != "Change the route work:\ndays: Mon–Fri → Mon, Tue\narrival target: 08:45 → none\nnew leg 3: walk 3 min" {
		t.Fatalf("fields and an inserted leg = %+v", proposal)
	}
	h.call("alice", "route_update", map[string]any{"name": "work", "leg_index": 1, "leg_action": "remove"})
	if proposal := h.proposals.last(t); proposal.Summary != "Change the route work:\nleg 1 removed: bike 6 min" {
		t.Fatalf("a removed leg = %+v", proposal)
	}
	h.call("alice", "route_remove", map[string]any{"name": "WORK"})
	if proposal := h.proposals.last(t); proposal.Tool != "route_remove" || proposal.Summary != "Delete the route work: Mon–Fri, arrive by 08:45\nbike 6 min; train Amersfoort Schothorst → Utrecht Centraal, departures 07:56, 08:12; walk 10 min" {
		t.Fatalf("removal = %+v", proposal)
	}

	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"name": "work", "leg_index": 2, "leg": map[string]any{"departures": []string{"07:56", "08:12"}}}, "That changes nothing"},
		{map[string]any{"name": "work", "leg_index": 1, "leg": map[string]any{"minutes": 0}}, "leg 1: a bike leg needs the minutes the user stated"},
		{map[string]any{"name": "work", "leg": map[string]any{"minutes": 7}}, "give leg_index"},
		{map[string]any{"name": "work", "leg_index": 5, "leg": map[string]any{"minutes": 7}}, "the route has 3 legs"},
		{map[string]any{"name": "work", "leg_index": 2, "leg": map[string]any{"to": "Nowhere Junction"}}, "leg 2: "},
	} {
		if result := h.call("alice", "route_update", test.arguments); !result.IsError || !strings.Contains(result.Text, test.want) {
			t.Fatalf("update %v = %+v, want %q", test.arguments, result, test.want)
		}
	}
}

// tripsEdited is the recorded trips, each as a JSON object, changed by change. They run 07:56,
// 08:04, 08:12, 08:26, and 08:34 from Amersfoort Schothorst; the 08:04 changes trains with 2
// minutes to spare.
func tripsEdited(t *testing.T, change func(trips []map[string]any) []map[string]any) []byte {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(fixture(t, "ns", "trips.json"), &response); err != nil {
		t.Fatal(err)
	}
	var trips []map[string]any
	for _, item := range response["trips"].([]any) {
		trips = append(trips, item.(map[string]any))
	}
	response["trips"] = change(trips)
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func legOf(trip map[string]any, index int) map[string]any {
	return trip["legs"].([]any)[index].(map[string]any)
}

// bothSavedPartlyCancelled is the recorded trips with the 07:56 and the 08:12 partly cancelled.
func bothSavedPartlyCancelled(t *testing.T) []byte {
	return tripsEdited(t, func(trips []map[string]any) []map[string]any {
		legOf(trips[0], 1)["cancelled"] = true
		legOf(trips[2], 0)["partCancelled"] = true
		return trips
	})
}

func changeTimes(queries []url.Values) []string {
	var steps []string
	for _, query := range queries {
		steps = append(steps, query.Get("addChangeTime"))
	}
	return steps
}

func TestRouteCheckContinuesOnAnAlternativeWhenNoSavedTrainRuns(t *testing.T) {
	h := newHarness(t)
	h.save("alice", work())
	h.gateway.serveTrips(bothSavedPartlyCancelled(t))
	// With 10 extra minutes for each transfer, NS has the 08:04 make the next IC.
	h.gateway.serveLonger(tripsEdited(t, func(trips []map[string]any) []map[string]any {
		trips[1]["legs"].([]any)[1] = legOf(trips[3], 1)
		return []map[string]any{trips[1], trips[3]}
	}))

	result, err := h.service.Check(h.ctx, "alice", "work", h.day("2026-10-05"))
	if err != nil {
		t.Fatal(err)
	}
	clock := func(at time.Time) string { return at.In(h.location).Format("15:04") }
	train := result.Legs[1]
	if train.Status != "on time" || train.SavedStatus != "no usable departure" || train.Unusable() || !train.SavedUnusable() || train.Taken != 2 || len(train.Saved()) != 2 || len(train.Departures) != 5 {
		t.Fatalf("train leg = %+v", train)
	}
	if !train.Departures[train.Taken].Alternative || clock(train.Start) != "08:04" || clock(train.End) != "08:23" {
		t.Fatalf("taken = %+v, %s–%s", train.Departures[train.Taken], clock(train.Start), clock(train.End))
	}
	if result.Verdict != routes.VerdictMet || clock(result.Leave) != "07:58" || clock(result.Arrival) != "08:33" || len(result.FailedLegs) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if got := changeTimes(h.gateway.asked()); !slices.Equal(got, []string{"", "10"}) {
		t.Fatalf("change times asked = %q, want one plain plan shared by the check and the alternatives, and one with extra transfer time", got)
	}
	wantText(t, h.call("alice", "route_check", map[string]any{"name": "work", "date": "2026-10-05"}).Text,
		"Leg 1, bike 6 min: as stated, 07:58–08:04\n",
		"Leg 2, train Amersfoort Schothorst → Utrecht Centraal, departures 07:56, 08:12: on time, taking the alternative 08:04 instead of a saved departure, 08:04–08:23\n"+
			"   Saved departures: none usable; each is cancelled or does not run:\n"+
			"   1. partly cancelled — the 07:56 from Amersfoort Schothorst to Utrecht Centraal\n",
		"   2. partly cancelled — the 08:12 from Amersfoort Schothorst to Utrecht Centraal\n",
		"   Alternatives the train service plans in their place, each with its own status:\n"+
			"   1. on time — the 08:04 from Amersfoort Schothorst to Utrecht Centraal\n"+
			"      - ICD 1822: Amersfoort Schothorst 08:04",
		"If the 2-minute change is missed: IC 1724 at 08:40 from Amersfoort Centraal, arriving 08:53.\n",
		"   2. on time — the 08:26 from Amersfoort Schothorst to Utrecht Centraal\n",
		"   3. on time — the 08:34 from Amersfoort Schothorst to Utrecht Centraal\n",
		"Leg 3, walk 10 min: as stated, 08:23–08:33\n",
		"Result: arrives 08:33; the arrival target 08:45 is met with 12 min to spare. Leave by 07:58.",
	)
}

// The second train leg's saved 08:12 runs, but the first leg only reaches the station at 08:23, so
// the alternatives start then and the trips before it are left out. Its saved 08:10 does not run,
// and no departure near it is looked up.
func TestRouteCheckPlansAlternativesFromWhenTheRouteGetsThere(t *testing.T) {
	h := newHarness(t)
	h.save("alice", routes.Spec{
		Name:     "twice",
		Days:     []string{"mon"},
		ArriveBy: "09:00",
		Legs: []routes.Leg{
			{Mode: routes.ModeTrain, From: "Amersfoort Schothorst", To: "Utrecht Centraal", Departures: []string{"07:56"}},
			{Mode: routes.ModeTrain, From: "Amersfoort Schothorst", To: "Utrecht Centraal", Departures: []string{"08:10", "08:12"}},
		},
	})
	result, err := h.service.Check(h.ctx, "alice", "twice", h.day("2026-10-05"))
	if err != nil {
		t.Fatal(err)
	}
	clock := func(at time.Time) string { return at.In(h.location).Format("15:04") }
	second := result.Legs[1]
	if second.Status != "on time" || second.SavedStatus != "connection missed" || clock(second.Departures[second.Taken].Planned) != "08:26" || len(second.Departures) != 4 {
		t.Fatalf("second leg = %+v", second)
	}
	if len(second.Replace) != 1 || second.Replace[0].Checked || second.Replace[0].Found {
		t.Fatalf("replace = %+v", second.Replace)
	}
	if result.Verdict != routes.VerdictMet || clock(result.Arrival) != "08:53" {
		t.Fatalf("result = %+v", result)
	}
	queries := h.gateway.asked()
	if last, previous := queries[len(queries)-1], queries[len(queries)-2]; last.Get("dateTime") != "2026-10-05T08:23:00+02:00" || last.Has("addChangeTime") || previous.Get("dateTime") == last.Get("dateTime") {
		t.Fatalf("trips queries = %v", queries)
	}
	wantText(t, h.service.Render(result),
		"Leg 2, train Amersfoort Schothorst → Utrecht Centraal, departures 08:10, 08:12: on time, taking the alternative 08:26 instead of a saved departure, 08:26–08:53\n"+
			"   Saved departures: none usable; those that run leave before the route gets there:\n"+
			"   1. not in the timetable — the 08:10 from Amersfoort Schothorst to Utrecht Centraal\n",
		"   2. on time — the 08:12 from Amersfoort Schothorst to Utrecht Centraal\n",
		"   The saved 08:10 is not in the timetable on this day; no nearby departure was looked up on this check. If it never runs, offer to replace it in the route.\n",
		"   Alternatives the train service plans in their place, each with its own status:\n   1. on time — the 08:26",
		"   2. on time — the 08:34",
		"Result: arrives 08:53; the arrival target 09:00 is met with 7 min to spare. Leave by 07:56.",
	)
}

func TestRouteCheckNamesASavedTrainThatIsNotInTheTimetable(t *testing.T) {
	h := newHarness(t)
	route := work()
	route.Legs[1].Departures = []string{"07:56", "08:10"}
	h.save("alice", route)

	result, err := h.service.Check(h.ctx, "alice", "work", h.day("2026-10-05"))
	if err != nil {
		t.Fatal(err)
	}
	clock := func(at time.Time) string { return at.In(h.location).Format("15:04") }
	train := result.Legs[1]
	if train.Status != "on time" || train.Taken != 0 || train.SavedStatus != "" || len(train.Departures) != 2 || train.Departures[1].Status != transit.StatusNotInTimetable {
		t.Fatalf("train leg = %+v", train)
	}
	if len(train.Replace) != 1 || clock(train.Replace[0].Planned) != "08:10" || !train.Replace[0].Checked || !train.Replace[0].Found || clock(train.Replace[0].Nearest.Planned) != "08:12" {
		t.Fatalf("replace = %+v", train.Replace)
	}
	text := h.service.Render(result)
	wantText(t, text,
		"on time, taking the 07:56, 07:56–08:23\n   1. on time — the 07:56",
		"   2. not in the timetable — the 08:10 from Amersfoort Schothorst to Utrecht Centraal\n",
		"   The saved 08:10 is not in the timetable on this day; the nearest real departure is the 08:12. If it never runs, offer to replace it in the route.\n",
	)
	if strings.Contains(text, "Alternatives") {
		t.Fatalf("a leg that runs lists alternatives:\n%s", text)
	}

	watched, err := h.service.CheckForWatch(h.ctx, "alice", "work", h.day("2026-10-05"))
	if err != nil {
		t.Fatal(err)
	}
	if train := watched.Legs[1]; train.Taken != 0 || len(train.Replace) != 0 || len(train.Departures) != 2 {
		t.Fatalf("a watched leg that runs looked for a replacement: %+v", train)
	}
}

// Without the alternatives a trains service may offer, and for a bus, a leg whose departures cannot
// be taken stays unusable.
func TestRouteCheckWithoutAlternativesKeepsTheLegUnusable(t *testing.T) {
	h := newHarness(t)
	h.save("alice", work())
	h.save("alice", routes.Spec{
		Name: "evening",
		Days: []string{"sun"},
		Legs: []routes.Leg{{Mode: routes.ModeBus, Stop: "Amersfoort, Stadhuis", Line: "3", Departures: []string{"18:19"}, Minutes: 12}},
	})
	h.gateway.serveTrips(bothSavedPartlyCancelled(t))
	plain, err := routes.NewService(routes.Config{Location: h.location, Trains: struct{ transit.Departures }{h.trains}, Now: h.nsClock.Now}, h.store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := plain.Check(h.ctx, "alice", "work", h.day("2026-10-05"))
	if err != nil {
		t.Fatal(err)
	}
	if train := result.Legs[1]; train.Status != "no usable departure" || train.Taken != -1 || train.SavedStatus != "" || len(train.Departures) != 2 || result.Verdict != routes.VerdictUnknown {
		t.Fatalf("result = %+v", result)
	}
	if text := plain.Render(result); strings.Contains(text, "Alternatives") || strings.Contains(text, "Saved departures") {
		t.Fatalf("alternatives without the interface:\n%s", text)
	}
	if got := changeTimes(h.gateway.asked()); !slices.Equal(got, []string{""}) {
		t.Fatalf("change times asked = %q", got)
	}

	asked := len(h.gateway.asked())
	bus, err := h.service.Check(h.ctx, "alice", "evening", h.day("2026-10-04"))
	if err != nil {
		t.Fatal(err)
	}
	if leg := bus.Legs[0]; leg.Status != "no usable departure" || leg.Departures[0].Status != transit.StatusNotInTimetable || len(leg.Departures) != 1 || len(leg.Replace) != 0 {
		t.Fatalf("bus leg = %+v", leg)
	}
	if text := h.service.Render(bus); strings.Contains(text, "Alternatives") || strings.Contains(text, "The saved") {
		t.Fatalf("a bus leg with alternatives:\n%s", text)
	}
	if got := len(h.gateway.asked()); got != asked {
		t.Fatalf("a bus leg asked NS for %d trips", got-asked)
	}
}

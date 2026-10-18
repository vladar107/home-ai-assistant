package watch_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const nsTimeLayout = "2006-01-02T15:04:05-0700"

// recorded reads a response recorded from a real service, kept beside its connector.
func recorded(t *testing.T, connector, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "connectors", connector, "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// gateway stands in for the NS gateway and the bus services: it answers by path and counts the
// requests to each.
type gateway struct {
	mu       sync.Mutex
	bodies   map[string][]byte
	statuses map[string]int
	requests map[string]int
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/ovapi/tpc/") {
		path = "/ovapi/tpc/"
	}
	g.mu.Lock()
	g.requests[path]++
	body, found := g.bodies[path]
	status := g.statuses[path]
	g.mu.Unlock()
	switch {
	case status != 0:
		w.WriteHeader(status)
	case !found:
		http.NotFound(w, r)
	case strings.HasSuffix(path, ".zip"):
		http.ServeContent(w, r, "gtfs.zip", time.Time{}, bytes.NewReader(body))
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func (g *gateway) serve(path string, body []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bodies[path] = body
	delete(g.statuses, path)
}

func (g *gateway) fail(path string, status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.statuses[path] = status
}

func (g *gateway) count(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests[path]
}

type recordedProposal struct {
	principalID string
	proposal    approval.Proposal
}

type fakeProposer struct {
	mu        sync.Mutex
	proposals []recordedProposal
}

func (f *fakeProposer) Propose(_ context.Context, principalID string, proposal approval.Proposal) (approval.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proposals = append(f.proposals, recordedProposal{principalID: principalID, proposal: proposal})
	return approval.Record{}, nil
}

// Act records the change as a proposal, as it is made outside a clean chat turn.
func (f *fakeProposer) Act(ctx context.Context, principalID string, proposal approval.Proposal) (string, error) {
	_, err := f.Propose(ctx, principalID, proposal)
	return approval.ProposedText, err
}

// transitEngine is the watch service with the real departure and disruption evaluators, on the
// real NS and bus connectors talking to a gateway that serves recorded responses.
type transitEngine struct {
	t         *testing.T
	ctx       context.Context
	store     *sqlite.Store
	clock     *testClock
	gateway   *gateway
	switches  *integration.Switches
	trains    *ns.Service
	buses     *bus.Service
	routes    *routes.Service
	sender    *fakeSender
	telemetry *fakeTelemetry
	proposer  *fakeProposer
	service   *watch.Service
}

func newTransitEngine(t *testing.T) *transitEngine {
	t.Helper()
	httpretry.WithoutWaits(t)
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 202},
	}); err != nil {
		t.Fatal(err)
	}
	e := &transitEngine{
		t: t, ctx: ctx, store: store, clock: &testClock{},
		gateway:  &gateway{bodies: map[string][]byte{}, statuses: map[string]int{}, requests: map[string]int{}},
		sender:   &fakeSender{},
		proposer: &fakeProposer{}, telemetry: &fakeTelemetry{},
	}
	server := httptest.NewServer(e.gateway)
	t.Cleanup(server.Close)
	e.gateway.serve("/ns/v2/stations", recorded(t, "ns", "stations.json"))
	e.gateway.serve("/ns/v3/trips", recorded(t, "ns", "trips.json"))
	e.gateway.serve("/ns/v2/departures", recorded(t, "ns", "departures.json"))
	e.gateway.serve("/gtfs/gtfs-nl.zip", stopDirectory(t))
	e.gateway.serve("/ovapi/tpc/", recorded(t, "bus", "tpc_amersfoort.json"))

	logger := slog.New(slog.NewTextHandler(&lockedBuffer{}, nil))
	e.switches, err = integration.NewSwitches(store, logger)
	if err != nil {
		t.Fatal(err)
	}
	e.trains, err = ns.NewService(ns.Config{APIKey: "test-key", BaseURL: server.URL + "/ns", Now: e.clock.Now}, e.switches, logger)
	if err != nil {
		t.Fatal(err)
	}
	e.buses, err = bus.NewService(bus.Config{OVapiURL: server.URL + "/ovapi", GTFSURL: server.URL + "/gtfs", TransitousURL: server.URL + "/transitous", Now: e.clock.Now}, e.switches, logger)
	if err != nil {
		t.Fatal(err)
	}
	e.service, err = watch.NewService(watch.Config{Zone: "Europe/Amsterdam", Now: e.clock.Now}, store, store, e.sender, e.proposer, logger)
	if err != nil {
		t.Fatal(err)
	}
	departures, err := watch.NewDepartureEvaluator(e.trains, e.buses)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.service.Register(watch.KindDeparture, departures); err != nil {
		t.Fatal(err)
	}
	disruptions, err := watch.NewDisruptionsEvaluator(e.trains)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.service.Register(watch.KindDisruptions, disruptions); err != nil {
		t.Fatal(err)
	}
	zone, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	e.routes, err = routes.NewService(routes.Config{Location: zone, Trains: e.trains, Buses: e.buses, Now: e.clock.Now}, store, e.proposer, logger)
	if err != nil {
		t.Fatal(err)
	}
	routeKind, err := watch.NewRouteEvaluator(e.routes)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.service.Register(watch.KindRoute, routeKind); err != nil {
		t.Fatal(err)
	}
	e.service.SetTelemetry(e.telemetry)
	return e
}

// stopDirectory is the national stop directory the bus connector downloads, built from the
// recorded tables.
func stopDirectory(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for name, body := range map[string][]byte{
		"agency.txt": []byte("agency_id,agency_name\r\nUOV,U-OV\r\n"),
		"routes.txt": recorded(t, "bus", "routes.txt"),
		"stops.txt":  recorded(t, "bus", "stops.txt"),
	} {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// trips is the recorded trip plan with its first trip, the 07:56 from Amersfoort Schothorst, changed.
func trips(t *testing.T, change func(trip map[string]any, legs []map[string]any)) []byte {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(recorded(t, "ns", "trips.json"), &response); err != nil {
		t.Fatal(err)
	}
	first := response["trips"].([]any)[0].(map[string]any)
	var legs []map[string]any
	for _, item := range first["legs"].([]any) {
		legs = append(legs, item.(map[string]any))
	}
	change(first, legs)
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func departingLate(t *testing.T, minutes int) []byte {
	t.Helper()
	return trips(t, func(_ map[string]any, legs []map[string]any) {
		origin := legs[0]["origin"].(map[string]any)
		planned, err := time.Parse(nsTimeLayout, origin["plannedDateTime"].(string))
		if err != nil {
			t.Fatal(err)
		}
		origin["actualDateTime"] = planned.Add(time.Duration(minutes) * time.Minute).Format(nsTimeLayout)
	})
}

func (e *transitEngine) at(local string) {
	e.t.Helper()
	e.clock.Set(e.t, local)
	e.service.Cycle(e.ctx)
}

func (e *transitEngine) create(principalID, id string, spec watch.Spec) {
	e.t.Helper()
	now := e.clock.Now()
	created, err := e.store.CreateWatch(e.ctx, watch.Watch{ID: id, PrincipalID: principalID, Spec: spec, Enabled: true, CreatedAt: now, UpdatedAt: now}, watch.MaximumWatchesPerPrincipal)
	if err != nil || !created {
		e.t.Fatalf("create watch: %v %v", created, err)
	}
}

func (e *transitEngine) propose(principalID string, arguments map[string]any) tools.Result {
	e.t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		e.t.Fatal(err)
	}
	result, err := e.service.Execute(e.ctx, principalID, tools.Call{Name: "watch_propose", Arguments: raw})
	if err != nil {
		e.t.Fatal(err)
	}
	return result
}

// wantSent checks what was sent since the last call, in order, each to its owner's chat.
func (e *transitEngine) wantSent(chatID int64, contains ...string) {
	e.t.Helper()
	sent := e.sender.take()
	if len(sent) != len(contains) {
		e.t.Fatalf("sent %d messages, want %d: %+v", len(sent), len(contains), sent)
	}
	for index, want := range contains {
		if sent[index].chatID != chatID || !strings.Contains(sent[index].text, want) {
			e.t.Fatalf("message %d to %d = %q, want it in chat %d containing %q", index, sent[index].chatID, sent[index].text, chatID, want)
		}
	}
}

// Monday 5 October 2026, the day the recorded trip plan is for.
func trainSpec() watch.Spec {
	return watch.Spec{
		Kind:         watch.KindDeparture,
		Target:       watch.Target{Mode: watch.ModeTrain, From: "Amersfoort Schothorst", To: "Utrecht Centraal"},
		Triggers:     []string{watch.TriggerCancelled, watch.TriggerPartlyCancelled, watch.TriggerDelay, watch.TriggerPlatform},
		DelayMinutes: 5,
		Days:         []string{"mon", "tue", "wed", "thu", "fri"},
		Start:        "06:26",
		End:          "07:56",
		Zone:         "Europe/Amsterdam",
	}
}

// An on-time train stays silent; a delay over the threshold alerts once with NS's own status
// words, a few more minutes of it do not, and its end sends "back to normal". A delay a step
// further alerts again, and a cancellation alerts.
func TestDepartureWatchAlertsOnRealTrainChanges(t *testing.T) {
	e := newTransitEngine(t)
	e.create("alice", watchID, trainSpec())

	e.at("2026-10-05 06:30")
	e.wantSent(101)

	e.gateway.serve("/ns/v3/trips", departingLate(t, 6))
	e.at("2026-10-05 06:35")
	e.wantSent(101, "07:56 Amersfoort Schothorst → Utrecht Centraal: delayed +6 min\n- SPR 5822: Amersfoort Schothorst 08:02 (planned 07:56, 6 min late)")

	e.gateway.serve("/ns/v3/trips", departingLate(t, 8))
	e.at("2026-10-05 06:40")
	e.wantSent(101)

	e.gateway.serve("/ns/v3/trips", recorded(t, "ns", "trips.json"))
	e.at("2026-10-05 06:45")
	e.wantSent(101, "Back to normal. 07:56 Amersfoort Schothorst → Utrecht Centraal: on time")

	e.gateway.serve("/ns/v3/trips", departingLate(t, 11))
	e.at("2026-10-05 06:50")
	e.wantSent(101, "delayed +11 min")
	e.gateway.serve("/ns/v3/trips", departingLate(t, 13))
	e.at("2026-10-05 06:55")
	e.wantSent(101)

	e.gateway.serve("/ns/v3/trips", trips(t, func(trip map[string]any, legs []map[string]any) {
		trip["status"] = "CANCELLED"
		legs[0]["cancelled"] = true
	}))
	e.at("2026-10-05 07:00")
	e.wantSent(101, "07:56 Amersfoort Schothorst → Utrecht Centraal: cancelled")

	if e.telemetry.count("check alice departure normal") != 2 || e.telemetry.count("check alice departure triggered") != 5 {
		t.Fatalf("check metrics = %v", e.telemetry.events)
	}
}

// A train that leaves the timetable alerts, while a delay on a watch that only asks about
// cancellations stays silent.
func TestDepartureWatchAlertsWhenTheTrainIsGone(t *testing.T) {
	e := newTransitEngine(t)
	gone := trainSpec()
	gone.End = "08:10"
	e.create("alice", watchID, gone)
	cancellations := trainSpec()
	cancellations.Triggers, cancellations.DelayMinutes = []string{watch.TriggerCancelled}, 0
	e.create("alice", "0000000000000002", cancellations)

	e.gateway.serve("/ns/v3/trips", departingLate(t, 20))
	e.at("2026-10-05 07:00")
	e.wantSent(101, "08:10 Amersfoort Schothorst → Utrecht Centraal: not in the timetable\nNS plans no trip whose first train leaves Amersfoort Schothorst at exactly 08:10")
}

// Three failed NS checks in a row send one notice, and the cycle asks NS once for two watches on
// the same train.
func TestDepartureWatchTellsOnceWhenNSKeepsFailing(t *testing.T) {
	e := newTransitEngine(t)
	e.create("alice", watchID, trainSpec())
	e.create("alice", "0000000000000002", trainSpec())

	e.at("2026-10-05 06:30")
	if requests := e.gateway.count("/ns/v3/trips"); requests != 1 {
		t.Fatalf("NS was asked %d times for one train in one cycle", requests)
	}
	e.gateway.fail("/ns/v3/trips", http.StatusServiceUnavailable)
	e.at("2026-10-05 06:35")
	e.at("2026-10-05 06:40")
	e.wantSent(101)
	e.at("2026-10-05 06:45")
	e.wantSent(101, "I can't check this right now", "I can't check this right now")
	e.at("2026-10-05 06:50")
	e.wantSent(101)
	if e.telemetry.count("upstream departure ns") != 8 {
		t.Fatalf("upstream errors = %v", e.telemetry.events)
	}

	e.gateway.serve("/ns/v3/trips", recorded(t, "ns", "trips.json"))
	e.at("2026-10-05 06:55")
	e.wantSent(101)
}

// A live bus delay from OVapi alerts under the bus switch.
func TestDepartureWatchChecksABus(t *testing.T) {
	e := newTransitEngine(t)
	e.create("alice", watchID, watch.Spec{
		Kind:         watch.KindDeparture,
		Target:       watch.Target{Mode: watch.ModeBus, Stop: "Amersfoort, Stadhuis", Line: "3"},
		Triggers:     []string{watch.TriggerDelay},
		DelayMinutes: 1,
		Days:         []string{"sun"},
		Start:        "17:00",
		End:          "18:18",
		Zone:         "Europe/Amsterdam",
	})

	e.at("2026-10-04 18:15")
	e.wantSent(101, "18:18 Amersfoort, Stadhuis → Centraal Station: delayed +1 min\n- Bus 3 towards Centraal Station")
}

// One user's trains and buses switches decide only their own watches: a user with trains off
// cannot propose a train watch, and a stored one fails its checks without asking NS, while
// another user's watch on the same train runs. Names resolve when a watch is proposed.
func TestDepartureWatchRespectsEachUsersSwitches(t *testing.T) {
	e := newTransitEngine(t)
	e.clock.Set(t, "2026-10-05 05:00")
	if err := e.switches.SetEnabled(e.ctx, "bob", ns.Switch, false); err != nil {
		t.Fatal(err)
	}
	if err := e.switches.SetEnabled(e.ctx, "alice", bus.Switch, false); err != nil {
		t.Fatal(err)
	}

	train := map[string]any{"kind": "departure", "mode": "train", "from": "Schothorst", "to": "Utrecht", "departure": "07:56", "days": []string{"mon"}}
	if result := e.propose("bob", train); !result.IsError || result.Text != ns.Switch.OffText() {
		t.Fatalf("bob's train proposal = %+v", result)
	}
	if result := e.propose("alice", map[string]any{"kind": "departure", "mode": "bus", "stop": "Amersfoort, Stadhuis", "line": "3", "departure": "18:18", "days": []string{"sun"}}); !result.IsError || result.Text != bus.Switch.OffText() {
		t.Fatalf("alice's bus proposal = %+v", result)
	}
	if result := e.propose("alice", map[string]any{"kind": "departure", "mode": "train", "from": "Nowhere Junction", "to": "Utrecht", "departure": "07:56", "days": []string{"mon"}}); !result.IsError || !strings.Contains(result.Text, "Nowhere Junction") {
		t.Fatalf("an unknown station = %+v", result)
	}
	if result := e.propose("alice", train); result.IsError {
		t.Fatalf("alice's train proposal = %+v", result)
	}
	if len(e.proposer.proposals) != 1 || e.proposer.proposals[0].principalID != "alice" ||
		!strings.Contains(e.proposer.proposals[0].proposal.Summary, "07:56 train Amersfoort Schothorst → Utrecht Centraal") {
		t.Fatalf("proposals = %+v", e.proposer.proposals)
	}

	e.create("alice", watchID, trainSpec())
	e.create("bob", "0000000000000002", trainSpec())
	e.gateway.serve("/ns/v3/trips", departingLate(t, 6))
	for _, local := range []string{"2026-10-05 06:30", "2026-10-05 06:35", "2026-10-05 06:40"} {
		e.at(local)
	}
	if requests := e.gateway.count("/ns/v3/trips"); requests != 3 {
		t.Fatalf("NS was asked %d times in three cycles; bob's checks must not reach it", requests)
	}
	if e.telemetry.count("check bob departure failed") != 3 || e.telemetry.count("upstream departure ns") != 0 {
		t.Fatalf("bob's checks = %v", e.telemetry.events)
	}
	sent := e.sender.take()
	if len(sent) != 2 || sent[0].chatID != 101 || !strings.Contains(sent[0].text, "delayed +6 min") || sent[1].chatID != 202 || !strings.Contains(sent[1].text, "I can't check this right now") {
		t.Fatalf("sent = %+v", sent)
	}
}

// disruptions is a station's answer made of recorded NS entries by ID, each changed by change when
// it is given.
func disruptions(t *testing.T, change func(entry map[string]any), ids ...string) []byte {
	t.Helper()
	var recordedEntries []map[string]any
	if err := json.Unmarshal(recorded(t, "ns", "disruptions.json"), &recordedEntries); err != nil {
		t.Fatal(err)
	}
	answer := []map[string]any{}
	for _, id := range ids {
		for _, entry := range recordedEntries {
			if entry["id"] == id {
				if change != nil {
					change(entry)
				}
				answer = append(answer, entry)
			}
		}
	}
	data, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const (
	bredaPath      = "/ns/v3/disruptions/station/BD"
	sloterdijkPath = "/ns/v3/disruptions/station/ASS"
	signalFailure  = "7008222"
	brusselsLine   = "7008228"
)

// A disruption watch alerts on a new disruption at its stations once, however often NS repeats
// it or reflows its text; a new expected end alerts as a change; one of two ending is silent; and
// when none is left it says so once. Three failed checks send one notice.
func TestDisruptionWatchAlertsOnNewAndChangedDisruptions(t *testing.T) {
	e := newTransitEngine(t)
	e.create("alice", watchID, watch.Spec{
		Kind:     watch.KindDisruptions,
		Target:   watch.Target{Stations: []string{"Breda", "Amsterdam Sloterdijk"}},
		Triggers: []string{watch.TriggerNew, watch.TriggerChanged},
		Days:     []string{"sun"},
		Start:    "18:00",
		End:      "19:30",
		Zone:     "Europe/Amsterdam",
	})
	e.gateway.serve(bredaPath, disruptions(t, nil))
	e.gateway.serve(sloterdijkPath, disruptions(t, nil))

	e.at("2026-10-04 18:00")
	e.wantSent(101)

	e.gateway.serve(bredaPath, disruptions(t, nil, signalFailure))
	e.at("2026-10-04 18:05")
	e.wantSent(101, "**⚠️ Disruptions at Breda, Amsterdam Sloterdijk**\n\nNew disruption: Rotterdam - Breda; Rotterdam - Breda - Brussel.\n"+
		"Door een seinstoring: tussen Rotterdam Centraal en Breda rijden minder treinen.\nExpected end: Dit duurt tot ongeveer maandag 5 oktober 0:00.")

	e.gateway.serve(bredaPath, disruptions(t, func(entry map[string]any) {
		entry["title"] = "  Rotterdam -  Breda; Rotterdam - Breda - Brussel. "
		entry["phase"] = map[string]any{"id": "3", "label": "Update"}
	}, signalFailure))
	e.at("2026-10-04 18:10")
	e.wantSent(101)

	shorter := func(entry map[string]any) {
		if entry["id"] == signalFailure {
			entry["expectedDuration"] = map[string]any{"description": "Dit duurt tot ongeveer 21:00."}
		}
	}
	e.gateway.serve(bredaPath, disruptions(t, shorter, signalFailure))
	e.at("2026-10-04 18:15")
	e.wantSent(101, "Changed disruption: Rotterdam - Breda; Rotterdam - Breda - Brussel.\nDoor een seinstoring: tussen Rotterdam Centraal en Breda rijden minder treinen.\nExpected end: Dit duurt tot ongeveer 21:00.")

	e.gateway.serve(bredaPath, disruptions(t, shorter, brusselsLine, signalFailure))
	e.gateway.serve(sloterdijkPath, disruptions(t, nil, brusselsLine))
	e.at("2026-10-04 18:20")
	e.wantSent(101, "Expected end: Dit duurt tot ongeveer woensdag 7 oktober 1:00.\nStill in effect: Rotterdam - Breda; Rotterdam - Breda - Brussel.")
	e.at("2026-10-04 18:25")
	e.wantSent(101)

	e.gateway.serve(bredaPath, disruptions(t, nil, brusselsLine))
	e.at("2026-10-04 18:30")
	e.wantSent(101)

	e.gateway.serve(bredaPath, disruptions(t, nil))
	e.gateway.serve(sloterdijkPath, disruptions(t, nil))
	e.at("2026-10-04 18:35")
	e.wantSent(101, "Back to normal. NS reports no disruptions or engineering works at Breda, Amsterdam Sloterdijk.")

	e.gateway.fail(bredaPath, http.StatusServiceUnavailable)
	e.at("2026-10-04 18:40")
	e.at("2026-10-04 18:45")
	e.at("2026-10-04 18:50")
	e.wantSent(101, "I can't check this right now")
	if e.telemetry.count("upstream disruptions ns") != 3 {
		t.Fatalf("upstream errors = %v", e.telemetry.events)
	}
}

// A disruption watch is proposed with full station names, and refused for a user with trains off,
// whose stored watch is then never checked against NS.
func TestDisruptionWatchRespectsEachUsersTrainsSwitch(t *testing.T) {
	e := newTransitEngine(t)
	e.clock.Set(t, "2026-10-04 12:00")
	if err := e.switches.SetEnabled(e.ctx, "bob", ns.Switch, false); err != nil {
		t.Fatal(err)
	}
	stations := map[string]any{"kind": "disruptions", "stations": []string{"Sloterdijk", "Breda", "BD"}, "window_from": "18:00", "window_until": "19:30", "days": []string{"sun"}}
	if result := e.propose("bob", stations); !result.IsError || result.Text != ns.Switch.OffText() {
		t.Fatalf("bob's proposal = %+v", result)
	}
	if result := e.propose("alice", stations); result.IsError {
		t.Fatalf("alice's proposal = %+v", result)
	}
	if len(e.proposer.proposals) != 1 || !strings.Contains(e.proposer.proposals[0].proposal.Summary, "Disruptions at Amsterdam Sloterdijk, Breda\n") {
		t.Fatalf("proposals = %+v", e.proposer.proposals)
	}

	spec := watch.Spec{Kind: watch.KindDisruptions, Target: watch.Target{Stations: []string{"Breda"}}, Triggers: []string{watch.TriggerNew, watch.TriggerChanged},
		Days: []string{"sun"}, Start: "18:00", End: "19:30", Zone: "Europe/Amsterdam"}
	e.create("alice", watchID, spec)
	e.create("bob", "0000000000000002", spec)
	e.gateway.serve(bredaPath, disruptions(t, nil, signalFailure))
	e.at("2026-10-04 18:05")
	e.wantSent(101, "New disruption: Rotterdam - Breda")
	if e.telemetry.count("check bob disruptions failed") != 1 || e.telemetry.count("check alice disruptions triggered") != 1 {
		t.Fatalf("checks = %v", e.telemetry.events)
	}
}

// saveRoute stores a route for principalID: a bike ride, the recorded train, and a walk, arriving
// by 08:45 on weekdays.
func (e *transitEngine) saveRoute(principalID, id, name string, departures ...string) {
	e.t.Helper()
	now := e.clock.Now()
	spec := routes.Spec{
		Name:     name,
		Days:     []string{"mon", "tue", "wed", "thu", "fri"},
		ArriveBy: "08:45",
		Legs: []routes.Leg{
			{Mode: routes.ModeBike, Minutes: 6},
			{Mode: routes.ModeTrain, From: "Amersfoort Schothorst", To: "Utrecht Centraal", Departures: departures},
			{Mode: routes.ModeWalk, Minutes: 10},
		},
	}
	created, err := e.store.CreateRoute(e.ctx, routes.Route{ID: id, PrincipalID: principalID, Spec: spec, CreatedAt: now, UpdatedAt: now}, routes.MaximumRoutesPerPrincipal)
	if err != nil || !created {
		e.t.Fatalf("save route: %v %v", created, err)
	}
}

func routeSpec(name string) watch.Spec {
	return watch.Spec{
		Kind:         watch.KindRoute,
		Target:       watch.Target{Route: name},
		Triggers:     []string{watch.TriggerLeg, watch.TriggerArrival},
		DelayMinutes: 5,
		Days:         []string{"mon", "tue", "wed", "thu", "fri"},
		Start:        "06:20",
		End:          "07:50",
		Zone:         "Europe/Amsterdam",
	}
}

// A route on time stays silent. A cancelled only train alerts once with the route check's own
// leg lines and its recovery says back to normal; a later departure that still runs leaves the
// leg alone and alerts only on the arrival target, and a delay alerts.
// wantSentAnyOrder is wantSent for watches checked in the same cycle, whose messages come in no
// particular order.
func (e *transitEngine) wantSentAnyOrder(chatID int64, contains ...string) {
	e.t.Helper()
	sent := e.sender.take()
	if len(sent) != len(contains) {
		e.t.Fatalf("sent %d messages, want %d: %+v", len(sent), len(contains), sent)
	}
	for _, want := range contains {
		if !slices.ContainsFunc(sent, func(message sentAlert) bool { return message.chatID == chatID && strings.Contains(message.text, want) }) {
			e.t.Fatalf("no message to chat %d containing %q in %+v", chatID, want, sent)
		}
	}
}

func TestRouteWatchAlertsOnLegsAndTheArrivalTarget(t *testing.T) {
	e := newTransitEngine(t)
	e.saveRoute("alice", "0000000000000001", "short", "07:56")
	e.saveRoute("alice", "0000000000000002", "work", "07:56", "08:12")
	e.create("alice", watchID, routeSpec("short"))
	e.create("alice", "0000000000000003", routeSpec("work"))

	e.at("2026-10-05 06:30")
	e.wantSent(101)

	e.gateway.serve("/ns/v3/trips", trips(t, func(trip map[string]any, legs []map[string]any) {
		trip["status"] = "CANCELLED"
		legs[0]["cancelled"] = true
	}))
	e.at("2026-10-05 06:35")
	e.wantSentAnyOrder(101,
		"Route \"work\": arrival target missed\nLeg 1, bike 6 min: as stated",
		"Route \"short\": leg 2 cancelled; arrival target missed\nLeg 1, bike 6 min: as stated",
	)
	e.at("2026-10-05 06:40")
	e.wantSent(101)

	e.gateway.serve("/ns/v3/trips", recorded(t, "ns", "trips.json"))
	e.at("2026-10-05 06:45")
	e.wantSentAnyOrder(101, "Back to normal. Route \"work\": on track", "Back to normal. Route \"short\": on track")

	e.gateway.serve("/ns/v3/trips", departingLate(t, 6))
	e.at("2026-10-05 06:50")
	e.wantSentAnyOrder(101, "Route \"work\": leg 2 delayed +6 min", "Route \"short\": leg 2 delayed +6 min")
}

// A route watch kept in a zone ahead of the household's checks the route on its own day. Its
// Monday window in Tokyo ends before midnight at home, so read in the household's zone the window
// would check Sunday's trains instead of Monday's.
func TestRouteWatchInAnotherZoneChecksItsOwnDay(t *testing.T) {
	e := newTransitEngine(t)
	e.saveRoute("alice", "0000000000000001", "work", "07:56")
	spec := routeSpec("work")
	spec.Days = []string{"mon"}
	spec.Start, spec.End, spec.Zone = "05:00", "06:30", "Asia/Tokyo"
	e.create("alice", watchID, spec)

	e.at("2026-10-04 22:30")
	e.wantSent(101)
	if e.gateway.count("/ns/v3/trips") == 0 || e.telemetry.count("check alice route normal") != 1 {
		t.Fatalf("NS asked %d times, metrics = %v", e.gateway.count("/ns/v3/trips"), e.telemetry.events)
	}
}

// Route names are looked up among the owner's own routes when a watch is proposed, whatever the
// case, and the saved name is kept; no one can watch another user's route.
func TestRouteWatchProposalNeedsTheOwnersRoute(t *testing.T) {
	e := newTransitEngine(t)
	e.clock.Set(t, "2026-10-05 05:00")
	e.saveRoute("alice", "0000000000000001", "work", "07:56")
	e.saveRoute("alice", "0000000000000002", "gym", "07:56")

	arguments := func(name string) map[string]any {
		return map[string]any{"kind": "route", "route": name, "departure": "07:50", "days": []string{"mon"}}
	}
	if result := e.propose("alice", arguments("school")); !result.IsError || !strings.Contains(result.Text, `No route is called "school"`) || !strings.Contains(result.Text, `"work"`) || !strings.Contains(result.Text, `"gym"`) {
		t.Fatalf("unknown route = %+v", result)
	}
	if result := e.propose("bob", arguments("work")); !result.IsError || result.Text != "The user has no saved routes." {
		t.Fatalf("another user's route = %+v", result)
	}
	if len(e.proposer.proposals) != 0 {
		t.Fatalf("proposals = %+v", e.proposer.proposals)
	}
	if result := e.propose("alice", arguments(" WORK ")); result.IsError {
		t.Fatalf("alice's route = %+v", result)
	}
	if len(e.proposer.proposals) != 1 || !strings.Contains(e.proposer.proposals[0].proposal.Summary, `Route "work", leaving 07:50`) {
		t.Fatalf("proposals = %+v", e.proposer.proposals)
	}
}

// A watch whose route has been deleted fails its checks as a plain error, so the owner gets the
// failure notice and NS is never asked.
func TestRouteWatchTellsWhenTheRouteIsGone(t *testing.T) {
	e := newTransitEngine(t)
	e.saveRoute("alice", "0000000000000001", "work", "07:56")
	e.create("alice", watchID, routeSpec("work"))
	if deleted, err := e.store.DeleteRoute(e.ctx, "alice", "0000000000000001"); err != nil || !deleted {
		t.Fatalf("delete route: %v %v", deleted, err)
	}

	for _, local := range []string{"2026-10-05 06:30", "2026-10-05 06:35", "2026-10-05 06:40"} {
		e.at(local)
	}
	e.wantSent(101, "I can't check this right now")
	if requests := e.gateway.count("/ns/v3/trips"); requests != 0 || e.telemetry.count("check alice route failed") != 3 {
		t.Fatalf("NS asked %d times, metrics = %v", requests, e.telemetry.events)
	}
}

// A route watch whose owner turned trains off fails its checks as they are, while every leg
// failing because NS is down is an upstream failure.
func TestRouteWatchTellsTrainsOffApartFromNSFailing(t *testing.T) {
	e := newTransitEngine(t)
	e.saveRoute("alice", "0000000000000001", "work", "07:56")
	e.saveRoute("bob", "0000000000000002", "work", "07:56")
	e.create("alice", watchID, routeSpec("work"))
	e.create("bob", "0000000000000003", routeSpec("work"))
	if err := e.switches.SetEnabled(e.ctx, "bob", ns.Switch, false); err != nil {
		t.Fatal(err)
	}
	e.gateway.fail("/ns/v3/trips", http.StatusServiceUnavailable)

	e.at("2026-10-05 06:30")
	if e.telemetry.count("check alice route failed") != 1 || e.telemetry.count("check bob route failed") != 1 || e.telemetry.count("upstream route routes") != 1 {
		t.Fatalf("checks = %v", e.telemetry.events)
	}
}

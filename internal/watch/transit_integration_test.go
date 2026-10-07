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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/connectors/bus"
	"github.com/vladar107/home-ai-assistant/internal/connectors/ns"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/identity"
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

// transitEngine is the watch service with the real departure and disruption evaluators, on the
// real NS and bus connectors talking to a gateway that serves recorded responses.
type transitEngine struct {
	t         *testing.T
	ctx       context.Context
	store     *sqlite.Store
	clock     *testClock
	gateway   *gateway
	trains    *ns.Service
	buses     *bus.Service
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
	e.trains, err = ns.NewService(ns.Config{APIKey: "test-key", BaseURL: server.URL + "/ns", Now: e.clock.Now}, store, logger)
	if err != nil {
		t.Fatal(err)
	}
	e.buses, err = bus.NewService(bus.Config{OVapiURL: server.URL + "/ovapi", GTFSURL: server.URL + "/gtfs", TransitousURL: server.URL + "/transitous", Now: e.clock.Now}, store, logger)
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
	if _, err := e.trains.SetEnabled(e.ctx, "bob", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.buses.SetEnabled(e.ctx, "alice", false); err != nil {
		t.Fatal(err)
	}

	propose := func(principalID string, arguments map[string]any) tools.Result {
		t.Helper()
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		result, err := e.service.Execute(e.ctx, principalID, tools.Call{Name: "watch_propose", Arguments: raw})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	train := map[string]any{"kind": "departure", "mode": "train", "from": "Schothorst", "to": "Utrecht", "departure": "07:56", "days": []string{"mon"}}
	if result := propose("bob", train); !result.IsError || result.Text != ns.DisabledText {
		t.Fatalf("bob's train proposal = %+v", result)
	}
	if result := propose("alice", map[string]any{"kind": "departure", "mode": "bus", "stop": "Amersfoort, Stadhuis", "line": "3", "departure": "18:18", "days": []string{"sun"}}); !result.IsError || result.Text != bus.DisabledText {
		t.Fatalf("alice's bus proposal = %+v", result)
	}
	if result := propose("alice", map[string]any{"kind": "departure", "mode": "train", "from": "Nowhere Junction", "to": "Utrecht", "departure": "07:56", "days": []string{"mon"}}); !result.IsError || !strings.Contains(result.Text, "Nowhere Junction") {
		t.Fatalf("an unknown station = %+v", result)
	}
	if result := propose("alice", train); result.IsError {
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

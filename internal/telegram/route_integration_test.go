package telegram

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// stationsGateway is an NS gateway that knows the recorded station list and nothing else, which is
// all saving a route needs.
func stationsGateway(t *testing.T) string {
	t.Helper()
	stations, err := os.ReadFile(filepath.Join("..", "connectors", "ns", "testdata", "stations.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v2/stations") {
			_, _ = w.Write(stations)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// A route is saved, changed one leg at a time, and removed only on Confirm, with summaries built
// from the route as it will be stored; /jobs lists it with a delete button that works only for its
// owner.
func TestRouteIsSavedChangedAndRemovedOnConfirm(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: newTestClock(t, mondayMorning), ns: stationsGateway(t), providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	confirm := func(updateID int64, want string) {
		t.Helper()
		proposal := h.nextMessage()
		if !strings.Contains(proposal.text, want) {
			t.Fatalf("proposal = %q, want %q", proposal.text, want)
		}
		accept, _ := approvalButtons(t, proposal.keyboard)
		h.push(callbackUpdate(updateID, 101, proposal.messageID, accept))
		h.wantAnswer(updateID)
		h.nextEdit()
		h.continueTurn(runtime, updateID)
	}
	legs := func() string {
		t.Helper()
		var stored string
		if err := h.db.QueryRowContext(h.ctx, `SELECT legs FROM routes WHERE principal_id = 'alice'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		return stored
	}

	saved := h.runExposedTool(runtime, 1, 101, "route_save", `{"name":"work","days":["mon","tue","wed","thu","fri"],"arrive_by":"08:45","legs":[`+
		`{"mode":"bike","minutes":6},{"mode":"train","from":"amfs","to":"Utrecht","departures":["07:56"]},{"mode":"walk","minutes":10}]}`)
	if saved.IsError {
		t.Fatalf("save = %+v", saved)
	}
	if count := h.countRows(`SELECT count(*) FROM routes`); count != 0 {
		t.Fatalf("a proposal saved %d routes", count)
	}
	confirm(2, "Save the route work: Mon–Fri, arrive by 08:45\n1. bike 6 min\n2. train Amersfoort Schothorst → Utrecht Centraal, departures 07:56\n3. walk 10 min")
	if stored := legs(); stored != `[{"mode":"bike","minutes":6},{"mode":"train","from":"Amersfoort Schothorst","to":"Utrecht Centraal","departures":["07:56"]},{"mode":"walk","minutes":10}]` {
		t.Fatalf("stored legs = %s", stored)
	}

	if changed := h.runTool(runtime, 3, 101, "route_update", `{"name":"work","leg_index":2,"leg":{"departures":["07:56","08:12"]}}`); changed.IsError {
		t.Fatalf("update = %+v", changed)
	}
	confirm(4, "Change the route work:\nleg 2: train Amersfoort Schothorst → Utrecht Centraal, departures 07:56 → train Amersfoort Schothorst → Utrecht Centraal, departures 07:56, 08:12")
	if stored := legs(); !strings.Contains(stored, `"departures":["07:56","08:12"]`) || !strings.Contains(stored, `{"mode":"bike","minutes":6}`) {
		t.Fatalf("legs after the update = %s", stored)
	}

	if listed := h.runTool(runtime, 5, 202, "route_list", `{}`); listed.IsError || !strings.HasSuffix(listed.Text, "The user has no saved routes.") {
		t.Fatalf("bob's list = %+v", listed)
	}
	if removed := h.runTool(runtime, 6, 202, "route_remove", `{"name":"work"}`); !removed.IsError {
		t.Fatalf("bob's removal = %+v", removed)
	}

	jobs := h.requestJobs(7)
	if !strings.Contains(jobs.text, "1. 🧭 work: Mon–Fri, arrive by 08:45\nbike 6 min; train Amersfoort Schothorst → Utrecht Centraal, departures 07:56, 08:12; walk 10 min") {
		t.Fatalf("jobs = %q", jobs.text)
	}
	var id string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM routes`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if data := buttonData(jobs.keyboard); !slices.Equal(data, []string{"jobs:rdelete:" + id + ":1"}) {
		t.Fatalf("route buttons = %v", data)
	}
	h.push(callbackUpdate(8, 202, jobs.messageID, "jobs:rdelete:"+id+":1"))
	h.wantAnswer(8)
	h.nextEdit()
	h.wantMessage(202, RouteGoneReply)
	if count := h.countRows(`SELECT count(*) FROM routes`); count != 1 {
		t.Fatalf("bob's press deleted alice's route: %d left", count)
	}

	if removed := h.runTool(runtime, 9, 101, "route_remove", `{"name":"work"}`); removed.IsError {
		t.Fatalf("removal = %+v", removed)
	}
	confirm(10, "Delete the route work: Mon–Fri, arrive by 08:45")
	if count := h.countRows(`SELECT count(*) FROM routes`); count != 0 {
		t.Fatalf("routes after removal = %d", count)
	}
}

func TestRouteDeletesFromJobs(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: newTestClock(t, mondayMorning), ns: stationsGateway(t)})
	h.provision("alice", "account-alice")
	h.start()

	if saved := h.runTool(runtime, 1, 101, "route_save", `{"name":"gym","days":["sat"],"legs":[{"mode":"train","from":"Amersfoort","to":"Utrecht"}]}`); saved.IsError || !strings.Contains(saved.Text, "Done: the change is made") {
		t.Fatalf("save = %+v", saved)
	}
	if done := h.nextMessage(); !strings.HasPrefix(done.text, "Done:") {
		t.Fatalf("done message = %+v", done)
	}

	jobs := h.requestJobs(3)
	data := buttonData(jobs.keyboard)
	if len(data) != 1 || !strings.HasPrefix(data[0], "jobs:rdelete:") {
		t.Fatalf("jobs = %+v", jobs)
	}
	if deleted := h.pressJob(4, jobs.messageID, data[0]); deleted.text != NoJobsReply {
		t.Fatalf("after delete = %+v", deleted)
	}
	if count := h.countRows(`SELECT count(*) FROM routes`); count != 0 {
		t.Fatalf("routes after delete = %d", count)
	}
}

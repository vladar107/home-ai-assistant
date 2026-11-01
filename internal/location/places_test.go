package location_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/location"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

// The place every test watches: a made-up point with the default radius of 150 m.
const (
	placeLatitude  = 10.0
	placeLongitude = 20.0
	metresPerDeg   = 111195.0
)

type recordedEvents struct {
	mu     sync.Mutex
	events []location.PlaceEvent
}

func (r *recordedEvents) PlaceEvent(_ context.Context, event location.PlaceEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordedEvents) edges() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	edges := make([]string, 0, len(r.events))
	for _, event := range r.events {
		edges = append(edges, event.PrincipalID+":"+event.PlaceName+":"+event.Edge)
	}
	return edges
}

// presenceFixture walks one principal's live share around a saved place.
type presenceFixture struct {
	t        *testing.T
	store    *sqlite.Store
	clock    *clock
	service  *location.Service
	events   *recordedEvents
	message  int64
	sentAt   time.Time
	position time.Time
}

func newPresenceFixture(t *testing.T) *presenceFixture {
	t.Helper()
	store := newStore(t)
	c := &clock{now: base}
	service := newService(t, store, standard, c, nil)
	events := &recordedEvents{}
	service.SetPlaceListener(events)
	f := &presenceFixture{t: t, store: store, clock: c, service: service, events: events}
	f.savePlace("alice", "gym-1", "Gym")
	return f
}

func (f *presenceFixture) savePlace(principalID, id, name string) {
	f.t.Helper()
	place := location.Place{ID: id, PrincipalID: principalID, Name: name, Latitude: placeLatitude, Longitude: placeLongitude,
		RadiusM: location.DefaultRadius, CreatedAt: base, UpdatedAt: base}
	if created, err := f.store.CreatePlace(f.t.Context(), place, location.MaximumPlacesPerPrincipal); err != nil || !created {
		f.t.Fatalf("create place: %v %v", created, err)
	}
}

// fix is a position metres north of the place with the accuracy, 30 seconds after the last one.
func (f *presenceFixture) fix(principalID string, metres, accuracy float64) location.Fix {
	f.clock.now = f.clock.now.Add(30 * time.Second)
	return location.Fix{
		PrincipalID: principalID, Latitude: placeLatitude + metres/metresPerDeg, Longitude: placeLongitude, Accuracy: accuracy,
		Source: location.SourceLive, LiveMessageID: f.message, UpdatedAt: f.clock.now,
	}
}

// startLive begins a live share metres north of the place.
func (f *presenceFixture) startLive(metres, accuracy float64) {
	f.t.Helper()
	f.message++
	if _, err := f.service.Share(f.t.Context(), f.fix("alice", metres, accuracy)); err != nil {
		f.t.Fatal(err)
	}
}

// move sends edits of the live share at each distance north of the place.
func (f *presenceFixture) move(accuracy float64, distances ...float64) {
	f.t.Helper()
	for _, metres := range distances {
		if moved, err := f.service.Move(f.t.Context(), f.fix("alice", metres, accuracy)); err != nil || !moved {
			f.t.Fatalf("move to %v m: %v %v", metres, moved, err)
		}
	}
}

func (f *presenceFixture) state() string {
	f.t.Helper()
	presences, err := f.store.ListPresence(f.t.Context(), "alice")
	if err != nil {
		f.t.Fatal(err)
	}
	if len(presences) == 0 {
		return "unknown"
	}
	return presences[0].State
}

func (f *presenceFixture) wantEdges(want ...string) {
	f.t.Helper()
	got := f.events.edges()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		f.t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestArrivingNeedsTwoGoodFixesAndLeavingGivesOneLeave(t *testing.T) {
	f := newPresenceFixture(t)
	f.startLive(1000, 10)
	if f.state() != location.PresenceOutside {
		t.Fatalf("first fix set %q", f.state())
	}
	f.wantEdges()

	f.move(10, 60)
	f.wantEdges()
	f.move(10, 40, 30, 20)
	f.wantEdges("alice:Gym:arrive")
	if f.state() != location.PresenceInside {
		t.Fatalf("state after arriving = %q", f.state())
	}
	if line, err := f.service.TurnLine(t.Context(), "alice"); err != nil || !strings.HasSuffix(line, " At Gym (saved place).") {
		t.Fatalf("turn line inside = %q %v", line, err)
	}

	f.move(10, 900)
	f.wantEdges("alice:Gym:arrive")
	f.move(10, 1000, 1200)
	f.wantEdges("alice:Gym:arrive", "alice:Gym:leave")
	if line, _ := f.service.TurnLine(t.Context(), "alice"); strings.Contains(line, "saved place") {
		t.Fatalf("turn line outside = %q", line)
	}
}

func TestJitterAtTheBoundaryAndPoorOrMissingAccuracyChangeNothing(t *testing.T) {
	f := newPresenceFixture(t)
	f.startLive(20, 10)
	f.wantEdges()

	// Inside, a fix just past the radius is not far enough out to count as leaving.
	f.move(10, 140, 170, 145, 200, 130, 220, 100)
	f.wantEdges()
	// A fix far away whose accuracy is unknown tells nothing, however many come.
	f.move(0, 2000, 2000, 2000, 2000, 2000, 2000)
	f.wantEdges()
	if f.state() != location.PresenceInside {
		t.Fatalf("state = %q", f.state())
	}

	f.move(10, 1000, 1000)
	f.wantEdges("alice:Gym:leave")
	// Outside, jitter just beyond the outer band never reaches the radius.
	f.move(10, 240, 230, 250, 200, 300, 160)
	// A fix inside the radius whose accuracy is wider than it tells nothing either.
	f.move(400, 10, 10, 10, 10, 10, 10)
	f.wantEdges("alice:Gym:leave")
	if f.state() != location.PresenceOutside {
		t.Fatalf("state = %q", f.state())
	}
}

func TestOneFixThatLastsTwoMinutesCountsWithoutAnother(t *testing.T) {
	f := newPresenceFixture(t)
	f.startLive(1000, 10)
	f.move(10, 30)
	f.clock.now = f.clock.now.Add(time.Minute)
	if err := f.service.SettlePresence(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantEdges()
	f.clock.now = f.clock.now.Add(time.Minute)
	if err := f.service.SettlePresence(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantEdges("alice:Gym:arrive")
	if err := f.service.SettlePresence(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantEdges("alice:Gym:arrive")
}

func TestEndingOrExpiringAShareForgetsPresenceWithoutAnEvent(t *testing.T) {
	f := newPresenceFixture(t)
	f.startLive(20, 10)
	stopped := f.fix("alice", 2000, 10)
	stopped.EndedAt = stopped.UpdatedAt
	if moved, err := f.service.Move(t.Context(), stopped); err != nil || !moved {
		t.Fatalf("stop = %v %v", moved, err)
	}
	if f.state() != "unknown" {
		t.Fatalf("state after stopping = %q", f.state())
	}

	// A share that runs out sends nothing; the prune finds it no longer live.
	f.message++
	share := f.fix("alice", 20, 10)
	share.LiveUntil = share.UpdatedAt.Add(15 * time.Minute)
	if _, err := f.service.Share(t.Context(), share); err != nil {
		t.Fatal(err)
	}
	if f.state() != location.PresenceInside {
		t.Fatalf("state of the new share = %q", f.state())
	}
	f.clock.now = share.LiveUntil.Add(time.Minute)
	if err := f.service.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.state() != "unknown" {
		t.Fatalf("state after the share ran out = %q", f.state())
	}

	// The next share sets the state again without an event, though the user is elsewhere now.
	f.startLive(2000, 10)
	if f.state() != location.PresenceOutside {
		t.Fatalf("state of the next share = %q", f.state())
	}
	f.wantEdges()
}

func TestPinsNeverMovePresenceAndOneReplacingAShareResetsIt(t *testing.T) {
	f := newPresenceFixture(t)
	pin := f.fix("alice", 20, 10)
	pin.Source, pin.LiveMessageID = location.SourcePin, 0
	if _, err := f.service.Share(t.Context(), pin); err != nil {
		t.Fatal(err)
	}
	if f.state() != "unknown" {
		t.Fatalf("a pin set %q", f.state())
	}

	f.startLive(20, 10)
	if f.state() != location.PresenceInside {
		t.Fatalf("state = %q", f.state())
	}
	pin = f.fix("alice", 3000, 10)
	pin.Source, pin.LiveMessageID = location.SourcePin, 0
	if _, err := f.service.Share(t.Context(), pin); err != nil {
		t.Fatal(err)
	}
	if f.state() != "unknown" {
		t.Fatalf("state after a pin replaced the share = %q", f.state())
	}
	// Later edits of the replaced share are dropped, so they cannot set it again.
	if moved, err := f.service.Move(t.Context(), f.fix("alice", 3000, 10)); err != nil || moved {
		t.Fatalf("edit of the replaced share = %v %v", moved, err)
	}
	if f.state() != "unknown" {
		t.Fatalf("state after a stale edit = %q", f.state())
	}
	f.wantEdges()
}

func TestForgettingTheLocationForgetsPresence(t *testing.T) {
	f := newPresenceFixture(t)
	f.startLive(20, 10)
	if f.state() != location.PresenceInside {
		t.Fatalf("state = %q", f.state())
	}
	if forgotten, err := f.service.Forget(t.Context(), "alice"); err != nil || !forgotten {
		t.Fatalf("forget = %v %v", forgotten, err)
	}
	if f.state() != "unknown" {
		t.Fatalf("state after forgetting = %q", f.state())
	}
	f.wantEdges()
}

func TestAnotherPrincipalsPlacesAndPresenceStayApart(t *testing.T) {
	f := newPresenceFixture(t)
	f.startLive(1000, 10)
	f.move(10, 20, 20)
	f.wantEdges("alice:Gym:arrive")

	// Bob walks the same path, and has no place of alice's to arrive at.
	f.message++
	if _, err := f.service.Share(t.Context(), f.fix("bob", 1000, 10)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := f.service.Move(t.Context(), f.fix("bob", 20, 10)); err != nil {
			t.Fatal(err)
		}
	}
	f.wantEdges("alice:Gym:arrive")
	if presences, err := f.store.ListPresence(t.Context(), "bob"); err != nil || len(presences) != 0 {
		t.Fatalf("bob's presence = %+v %v", presences, err)
	}
	if _, found, err := f.service.PlaceNamed(t.Context(), "bob", "Gym"); err != nil || found {
		t.Fatalf("bob sees alice's place: %v %v", found, err)
	}
	if _, found, err := f.service.PlaceByID(t.Context(), "bob", "gym-1"); err != nil || found {
		t.Fatalf("bob loads alice's place by id: %v %v", found, err)
	}
	if line, _ := f.service.TurnLine(t.Context(), "bob"); strings.Contains(line, "saved place") {
		t.Fatalf("bob's turn line = %q", line)
	}

	// A presence written for a place under another principal's name is never stored.
	if err := f.store.SavePresence(t.Context(), location.Presence{PrincipalID: "bob", PlaceID: "gym-1", State: location.PresenceInside, LiveMessageID: 1, UpdatedAt: base}); err != nil {
		t.Fatal(err)
	}
	if presences, _ := f.store.ListPresence(t.Context(), "bob"); len(presences) != 0 {
		t.Fatalf("bob's presence on alice's place = %+v", presences)
	}
	if f.state() != location.PresenceInside {
		t.Fatalf("alice's state = %q", f.state())
	}
}

func TestResizingAPlaceForgetsItsPresenceAndRenamingDoesNot(t *testing.T) {
	f := newPresenceFixture(t)
	f.startLive(20, 10)
	place, found, err := f.store.LoadPlace(t.Context(), "alice", "gym-1")
	if err != nil || !found {
		t.Fatalf("load = %v %v", found, err)
	}
	renamed := place
	renamed.Name, renamed.UpdatedAt = "Studio", base.Add(time.Hour)
	if updated, err := f.store.UpdatePlace(t.Context(), renamed, place.UpdatedAt); err != nil || !updated {
		t.Fatalf("rename = %v %v", updated, err)
	}
	if f.state() != location.PresenceInside {
		t.Fatalf("state after renaming = %q", f.state())
	}
	resized := renamed
	resized.RadiusM, resized.UpdatedAt = 500, base.Add(2*time.Hour)
	if updated, err := f.store.UpdatePlace(t.Context(), resized, renamed.UpdatedAt); err != nil || !updated {
		t.Fatalf("resize = %v %v", updated, err)
	}
	if f.state() != "unknown" {
		t.Fatalf("state after resizing = %q", f.state())
	}
	if updated, err := f.store.UpdatePlace(t.Context(), resized, place.UpdatedAt); err != nil || updated {
		t.Fatalf("stale update = %v %v", updated, err)
	}
}

// A live share that falls silent past LiveSilence is pruned, but its message may still move: the
// presence it left stays, so moving away afterwards is a leaving.
func TestASilentShareThatResumesStillLeaves(t *testing.T) {
	f := newPresenceFixture(t)
	f.startLive(20, 10)
	f.clock.now = f.clock.now.Add(standard.LiveSilence + time.Minute)
	if err := f.service.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fixes, _ := f.store.ListLocations(t.Context()); len(fixes) != 0 {
		t.Fatalf("the silent share was not pruned: %+v", fixes)
	}
	if f.state() != location.PresenceInside {
		t.Fatalf("state after the silence = %q", f.state())
	}
	f.move(10, 1000, 1000)
	f.wantEdges("alice:Gym:leave")
}

// A prune that resets an ended share's presence leaves a newer share's presence alone.
func TestResettingPresenceKeepsOtherSharesRows(t *testing.T) {
	f := newPresenceFixture(t)
	f.savePlace("alice", "pool-1", "Pool")
	for index, placeID := range []string{"gym-1", "pool-1"} {
		if err := f.store.SavePresence(t.Context(), location.Presence{PrincipalID: "alice", PlaceID: placeID, State: location.PresenceInside, LiveMessageID: int64(index + 1), UpdatedAt: base}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.ResetPresence(t.Context(), "alice", 1); err != nil {
		t.Fatal(err)
	}
	presences, err := f.store.ListPresence(t.Context(), "alice")
	if err != nil || len(presences) != 1 || presences[0].PlaceID != "pool-1" {
		t.Fatalf("presence after resetting message 1 = %+v %v", presences, err)
	}
}

// Edits Telegram queued while the bot was down still move presence, but an arrival they show from
// minutes ago is not told.
func TestALateArrivalMovesPresenceWithoutAnEvent(t *testing.T) {
	f := newPresenceFixture(t)
	f.startLive(1000, 10)
	shared := f.clock.now
	f.clock.now = shared.Add(20 * time.Minute)
	for index := range 2 {
		late := f.fix("alice", 20, 10)
		late.UpdatedAt = shared.Add(time.Duration(index+1) * time.Minute)
		if moved, err := f.service.Move(t.Context(), late); err != nil || !moved {
			t.Fatalf("late edit = %v %v", moved, err)
		}
	}
	if f.state() != location.PresenceInside {
		t.Fatalf("state after the late edits = %q", f.state())
	}
	f.wantEdges()
}

// Of two saved places that overlap, the turn line names the smaller.
func TestTheTurnLineNamesTheSmallestPlaceTheUserIsIn(t *testing.T) {
	f := newPresenceFixture(t)
	small := location.Place{ID: "desk-1", PrincipalID: "alice", Name: "Desk", Latitude: placeLatitude, Longitude: placeLongitude,
		RadiusM: location.MinimumRadius, CreatedAt: base, UpdatedAt: base}
	if created, err := f.store.CreatePlace(t.Context(), small, location.MaximumPlacesPerPrincipal); err != nil || !created {
		t.Fatalf("create = %v %v", created, err)
	}
	f.startLive(10, 10)
	if line, err := f.service.TurnLine(t.Context(), "alice"); err != nil || !strings.HasSuffix(line, " At Desk (saved place).") {
		t.Fatalf("turn line = %q %v", line, err)
	}
}

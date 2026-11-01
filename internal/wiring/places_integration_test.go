package wiring

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	userlocation "github.com/vladar107/home-ai-assistant/internal/location"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// chatExposure answers every call as the user's own chat request, which has read outside content
// once external is set, so changes are proposed on a card instead of made at once.
type chatExposure struct{ external atomic.Bool }

func (e *chatExposure) UserChatRequest(ctx context.Context) (string, bool) {
	return string(tools.TurnFrom(ctx)), tools.TurnFrom(ctx) != ""
}
func (e *chatExposure) External(context.Context, string) bool { return e.external.Load() }
func (e *chatExposure) Exposed(context.Context, string) bool  { return e.external.Load() }

// traveling is a trip lookup that is either on or off.
type traveling struct{ on atomic.Bool }

func (t *traveling) Traveling(context.Context, string, time.Time) (bool, error) {
	return t.on.Load(), nil
}

type quietApprovals struct{}

func (quietApprovals) SendText(context.Context, int64, string) (int64, error) { return 1, nil }
func (quietApprovals) SendProposal(context.Context, int64, string, approval.Buttons) (int64, error) {
	return 1, nil
}
func (quietApprovals) EditText(context.Context, int64, int64, string) error { return nil }
func (quietApprovals) EditProposal(context.Context, int64, int64, string, approval.Buttons) error {
	return nil
}
func (quietApprovals) SendPreview(context.Context, int64, int64, string) error { return nil }
func (quietApprovals) SendDirectActions(context.Context, int64, approval.DirectActions) (int64, error) {
	return 1, nil
}
func (quietApprovals) EditDirectActions(context.Context, int64, int64, approval.DirectActions) error {
	return nil
}

type quietChat struct{}

func (quietChat) SendReply(context.Context, int64, int64, string) error { return nil }
func (quietChat) SendTrackedReply(context.Context, int64, int64, string) (int64, int64, error) {
	return 1, 0, nil
}
func (quietChat) SendTyping(context.Context, int64) error                            { return nil }
func (quietChat) SendMessageDraft(context.Context, int64, int64, string, bool) error { return nil }

type quietAlerts struct{}

func (quietAlerts) SendAlert(context.Context, int64, string, string) (int64, int64, error) {
	return 1, 0, nil
}

type placesFixture struct {
	t         *testing.T
	store     *sqlite.Store
	approvals *approval.Service
	exposure  *chatExposure
	travel    *traveling
	db        *sql.DB
	locations *userlocation.Service
	scheduler *schedule.Service
	watches   *watch.Service
	mu        sync.Mutex
	now       time.Time
	turns     int
	// messages is each principal's live location message.
	messages map[string]int64
}

func newPlacesFixture(t *testing.T) *placesFixture {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state", "assistant.sqlite")
	store := testutil.OpenStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	f := &placesFixture{t: t, db: testutil.OpenDB(t, path), now: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC), messages: map[string]int64{"bob": 5}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	approvals, err := approval.NewService(approval.Config{}, store, quietApprovals{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	f.store, f.approvals, f.exposure, f.travel = store, approvals, &chatExposure{}, &traveling{}
	approvals.SetExposure(f.exposure)
	f.locations, err = userlocation.NewService(userlocation.Config{
		Policy:        userlocation.Policy{PinTTL: time.Hour, LiveSilence: time.Hour, LiveGrace: 15 * time.Minute},
		PruneInterval: time.Hour, Zone: time.UTC, Now: f.clock,
	}, store, logger)
	if err != nil {
		t.Fatal(err)
	}
	f.scheduler, err = schedule.NewService(schedule.Config{Zone: "UTC", TickInterval: time.Hour, Now: f.clock, Exposure: f.exposure}, store, store, quietChat{}, approvals, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.Register(schedule.Reminder(f.clock)); err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.RegisterAnchor(schedule.AnchorPlace, placeAnchors{locations: f.locations}); err != nil {
		t.Fatal(err)
	}
	f.watches, err = watch.NewService(watch.Config{Zone: "UTC", Now: f.clock, Travel: f.travel}, store, store, quietAlerts{}, approvals, logger)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, err := watch.NewPlaceEvaluator(watchPlaces{locations: f.locations})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.watches.Register(watch.KindPlace, evaluator); err != nil {
		t.Fatal(err)
	}
	f.locations.SetPlaceListener(placeEvents{scheduler: f.scheduler, watches: f.watches, logger: logger})
	f.locations.SetPlaceTools(approvals, placeDependents{scheduler: f.scheduler, watches: f.watches})
	for _, executor := range []approval.Executor{f.locations, f.scheduler, f.watches} {
		if err := approvals.RegisterExecutor(executor); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *placesFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *placesFixture) advance(by time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(by)
	return f.now
}

// call runs a tool in a chat turn of its own, as the user's own request.
func (f *placesFixture) call(principalID, name, arguments string) tools.Result {
	f.t.Helper()
	f.turns++
	turn := tools.WithTurn(f.t.Context(), outbox.ChatTurn("turn-"+string(rune('a'+f.turns))))
	var result tools.Result
	var err error
	switch {
	case strings.HasPrefix(name, "place_"):
		result, err = f.locations.Execute(turn, principalID, tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	case strings.HasPrefix(name, "watch_"):
		result, err = f.watches.Execute(turn, principalID, tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	default:
		result, err = f.scheduler.Execute(turn, principalID, tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	}
	if err != nil {
		f.t.Fatalf("%s: %v", name, err)
	}
	return result
}

// at is a live fix metres north of the made-up point the test saves as a place.
func (f *placesFixture) at(principalID string, metres float64) userlocation.Fix {
	return userlocation.Fix{
		PrincipalID: principalID, Latitude: 10 + metres/111195.0, Longitude: 20, Accuracy: 10,
		Source: userlocation.SourceLive, LiveMessageID: f.messages[principalID], UpdatedAt: f.advance(30 * time.Second),
	}
}

func (f *placesFixture) share(principalID string, metres float64) {
	f.t.Helper()
	f.messages[principalID] += 10
	if _, err := f.locations.Share(f.t.Context(), f.at(principalID, metres)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *placesFixture) walk(principalID string, distances ...float64) {
	f.t.Helper()
	for _, metres := range distances {
		if moved, err := f.locations.Move(f.t.Context(), f.at(principalID, metres)); err != nil || !moved {
			f.t.Fatalf("move: %v %v", moved, err)
		}
	}
}

func (f *placesFixture) count(query string, arguments ...any) int {
	f.t.Helper()
	var count int
	if err := f.db.QueryRowContext(f.t.Context(), query, arguments...).Scan(&count); err != nil {
		f.t.Fatal(err)
	}
	return count
}

// A saved place is the target of a watch that alerts on leaving it and the anchor of a reminder that
// runs once on each arrival; a place in use cannot be deleted, and another principal sees none of it.
func TestSavedPlacesDriveWatchesAndAnchoredReminders(t *testing.T) {
	f := newPlacesFixture(t)
	if result := f.call("alice", "place_save", `{"name":"Gym"}`); !result.IsError || !strings.Contains(result.Text, "shared no location") {
		t.Fatalf("place_save without a location = %+v", result)
	}
	f.share("alice", 0)
	if result := f.call("alice", "place_save", `{"name":"Gym"}`); result.IsError || !strings.Contains(result.Text, "Saved the place Gym, radius 150 m.") {
		t.Fatalf("place_save = %+v", result)
	}
	if result := f.call("alice", "place_save", `{"name":"gym"}`); !result.IsError || !strings.Contains(result.Text, "already exists") {
		t.Fatalf("place_save of a taken name = %+v", result)
	}
	listed := f.call("alice", "place_list", `{}`)
	if listed.IsError || !strings.Contains(listed.Text, ": Gym (10.0000, 20.0000; radius 150 m)") {
		t.Fatalf("place_list = %+v", listed)
	}
	place, found, err := f.locations.PlaceNamed(t.Context(), "alice", "GYM")
	if err != nil || !found {
		t.Fatalf("place = %v %v", found, err)
	}

	watched := f.call("alice", "watch_propose", `{"kind":"place","name":"gym","triggers":["leave"]}`)
	if watched.IsError || !strings.Contains(watched.Text, "Watching.") {
		t.Fatalf("watch_propose = %+v", watched)
	}
	watches, err := f.watches.Watches(t.Context(), "alice")
	if err != nil || len(watches) != 1 || watches[0].Target.Place != place.ID || watches[0].Label() != "Leaving Gym" {
		t.Fatalf("watches = %+v %v", watches, err)
	}
	reminded := f.call("alice", "reminder_set", `{"text":"Stretch first","anchor":{"kind":"place","place_id":"`+place.ID+`","edge":"arrive","offset_minutes":0}}`)
	if reminded.IsError || !strings.Contains(reminded.Text, "on arriving at Gym; next: waits for arrival") || strings.Contains(reminded.Text, "not sharing") {
		t.Fatalf("reminder_set = %+v", reminded)
	}
	if result := f.call("alice", "reminder_set", `{"text":"Too late","anchor":{"kind":"place","place_id":"`+place.ID+`","edge":"arrive","offset_minutes":-5}}`); !result.IsError {
		t.Fatalf("a place anchor before the arrival = %+v", result)
	}
	var jobID string
	if err := f.db.QueryRowContext(t.Context(), `SELECT id FROM scheduled_jobs WHERE principal_id = 'alice'`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	refused := f.call("alice", "place_delete", `{"name":"Gym"}`)
	if !refused.IsError || !strings.Contains(refused.Text, `the watch "Leaving Gym" (id `+watches[0].ID+`)`) || !strings.Contains(refused.Text, `the reminder "Stretch first"`) {
		t.Fatalf("place_delete of a place in use = %+v", refused)
	}

	// Bob sees none of alice's places, and his own walk there moves none of her jobs or watches.
	if result := f.call("bob", "place_list", `{}`); result.IsError || result.Text != "The user has no saved places." {
		t.Fatalf("bob's place_list = %+v", result)
	}
	if result := f.call("bob", "watch_propose", `{"kind":"place","name":"Gym"}`); !result.IsError || !strings.Contains(result.Text, `no saved place is called "Gym"`) {
		t.Fatalf("bob's watch on alice's place = %+v", result)
	}
	if result := f.call("bob", "reminder_set", `{"text":"Not mine","anchor":{"kind":"place","place_id":"`+place.ID+`","edge":"arrive","offset_minutes":0}}`); !result.IsError {
		t.Fatalf("bob's reminder on alice's place = %+v", result)
	}
	f.share("bob", 2000)
	f.walk("bob", 0, 0, 0)

	// Alice's live share sets her inside without an event, then she leaves and comes back.
	f.walk("alice", 10)
	if alerts := f.count(`SELECT COUNT(*) FROM watch_alerts`); alerts != 0 {
		t.Fatalf("alerts before leaving = %d", alerts)
	}
	f.walk("alice", 800, 900)
	if alerts := f.count(`SELECT COUNT(*) FROM watch_alerts WHERE watch_id = ? AND status = 'ready'`, watches[0].ID); alerts != 1 {
		t.Fatalf("alerts after leaving = %d", alerts)
	}
	if err := f.scheduler.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.scheduler.Shutdown(context.Background()) })
	f.walk("alice", 20, 10, 15)
	deadline := time.Now().Add(5 * time.Second)
	for f.count(`SELECT COUNT(*) FROM schedule_runs WHERE job_id = ?`, jobID) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if runs := f.count(`SELECT COUNT(*) FROM schedule_runs WHERE job_id = ?`, jobID); runs != 1 {
		t.Fatalf("reminder runs after one arrival = %d", runs)
	}
	if alerts := f.count(`SELECT COUNT(*) FROM watch_alerts`); alerts != 1 {
		t.Fatalf("arriving alerted a leave watch: %d alerts", alerts)
	}
	jobs, err := f.scheduler.Jobs(t.Context(), "alice")
	if err != nil || len(jobs) != 1 || schedule.NextRunText(jobs[0], "UTC") != "waits for arrival" {
		t.Fatalf("the reminder after its run = %+v %v", jobs, err)
	}

	// Once nothing uses it, the place is deleted, and Undo brings it back.
	if result := f.call("alice", "watch_remove", `{"watch_id":"`+watches[0].ID+`"}`); result.IsError {
		t.Fatalf("watch_remove = %+v", result)
	}
	if result := f.call("alice", "reminder_cancel", `{"job_id":"`+jobID+`"}`); result.IsError {
		t.Fatalf("reminder_cancel = %+v", result)
	}
	if result := f.call("alice", "place_delete", `{"name":"Gym"}`); result.IsError || !strings.Contains(result.Text, "Deleted the place Gym.") {
		t.Fatalf("place_delete = %+v", result)
	}
	if _, found, err := f.locations.PlaceNamed(t.Context(), "alice", "Gym"); err != nil || found {
		t.Fatalf("place after delete = %v %v", found, err)
	}
}

// A place watch or a place-anchored reminder set up while no live location is shared says it works
// only while one is.
func TestPlaceTriggersSayTheyNeedALiveShare(t *testing.T) {
	f := newPlacesFixture(t)
	pin := f.at("alice", 0)
	pin.Source, pin.LiveMessageID = userlocation.SourcePin, 0
	if _, err := f.locations.Share(t.Context(), pin); err != nil {
		t.Fatal(err)
	}
	if result := f.call("alice", "place_save", `{"name":"Office","radius_m":300}`); result.IsError {
		t.Fatalf("place_save = %+v", result)
	}
	place, _, err := f.locations.PlaceNamed(t.Context(), "alice", "office")
	if err != nil {
		t.Fatal(err)
	}
	if result := f.call("alice", "watch_propose", `{"kind":"place","name":"Office"}`); result.IsError || !strings.Contains(result.Text, "you are not sharing it now") {
		t.Fatalf("watch_propose = %+v", result)
	}
	if result := f.call("alice", "reminder_set", `{"text":"Water the plants","anchor":{"kind":"place","place_id":"`+place.ID+`","edge":"leave","once":true,"offset_minutes":10}}`); result.IsError ||
		!strings.Contains(result.Text, "10 min after leaving Office, once") || !strings.Contains(result.Text, "you are not sharing it now") {
		t.Fatalf("reminder_set = %+v", result)
	}
	if result := f.call("alice", "place_update", `{"name":"office","new_name":"Work","radius_m":500}`); result.IsError || !strings.Contains(result.Text, "now Work, radius 500 m") {
		t.Fatalf("place_update = %+v", result)
	}
	if result := f.call("alice", "place_list", `{}`); !strings.Contains(result.Text, "Work (10.0000, 20.0000; radius 500 m)") || !strings.Contains(result.Text, "not sharing it now") {
		t.Fatalf("place_list = %+v", result)
	}
}

// A place's point stays only in places: no approval of a save, change, or deletion copies it, the
// deletion can be undone, and once the undo window ends the deleted place is removed for good.
func TestAPlacesPointIsKeptOnlyInPlaces(t *testing.T) {
	f := newPlacesFixture(t)
	f.messages["alice"] += 10
	if _, err := f.locations.Share(t.Context(), userlocation.Fix{PrincipalID: "alice", Latitude: 12.3456789, Longitude: 23.4567891, Accuracy: 10,
		Source: userlocation.SourceLive, LiveMessageID: f.messages["alice"], UpdatedAt: f.advance(time.Second)}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ tool, arguments, want string }{
		{"place_save", `{"name":"Gym"}`, "Saved the place Gym"},
		{"place_update", `{"name":"Gym","new_name":"Club","radius_m":300}`, "now Club, radius 300 m"},
		{"place_delete", `{"name":"Club"}`, "Deleted the place Club."},
	} {
		if result := f.call("alice", step.tool, step.arguments); result.IsError || !strings.Contains(result.Text, step.want) {
			t.Fatalf("%s = %+v", step.tool, result)
		}
	}
	copies := func() int {
		return f.count(`SELECT COUNT(*) FROM approvals WHERE arguments || summary || COALESCE(outcome, '') || COALESCE(undo, '') LIKE '%12.34%'
			OR arguments || summary || COALESCE(outcome, '') || COALESCE(undo, '') LIKE '%23.45%'`)
	}
	if n := copies(); n != 0 {
		t.Fatalf("%d approvals hold the point", n)
	}

	var deletion approval.Record
	var undo string
	if err := f.db.QueryRowContext(t.Context(), `SELECT id, undo FROM approvals WHERE tool = 'place_delete'`).Scan(&deletion.ID, &undo); err != nil {
		t.Fatal(err)
	}
	deletion.Tool, deletion.PrincipalID = "place_delete", "alice"
	if outcome, err := f.locations.Undo(t.Context(), deletion, json.RawMessage(undo)); err != nil || outcome.Text != "Restored the place Club." {
		t.Fatalf("undo = %+v %v", outcome, err)
	}
	if place, found, err := f.locations.PlaceNamed(t.Context(), "alice", "club"); err != nil || !found || place.RadiusM != 300 || place.Latitude != 12.3456789 {
		t.Fatalf("restored place = %+v %v %v", place, found, err)
	}
	if _, err := f.locations.Undo(t.Context(), deletion, json.RawMessage(undo)); err == nil {
		t.Fatal("a second undo restored the place again")
	}

	if result := f.call("alice", "place_delete", `{"name":"Club"}`); result.IsError {
		t.Fatalf("place_delete = %+v", result)
	}
	if n := copies(); n != 0 {
		t.Fatalf("%d approvals hold the point", n)
	}
	f.advance(approval.UndoWindow + time.Hour)
	if err := f.locations.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT COUNT(*) FROM places`); n != 0 {
		t.Fatalf("%d places are kept after the undo window", n)
	}
}

// A place watch proposed on a card, whose place is deleted before Confirm, is refused rather than
// left on no place; so is a place-anchored reminder.
func TestConfirmingAPlaceTriggerAfterItsPlaceIsDeletedIsRefused(t *testing.T) {
	f := newPlacesFixture(t)
	f.share("alice", 0)
	if result := f.call("alice", "place_save", `{"name":"Gym"}`); result.IsError {
		t.Fatalf("place_save = %+v", result)
	}
	place, _, err := f.locations.PlaceNamed(t.Context(), "alice", "Gym")
	if err != nil {
		t.Fatal(err)
	}
	f.exposure.external.Store(true)
	for _, call := range []struct{ tool, arguments string }{
		{"watch_propose", `{"kind":"place","name":"Gym","triggers":["arrive"]}`},
		{"reminder_set", `{"text":"Stretch","anchor":{"kind":"place","place_id":"` + place.ID + `","edge":"arrive","offset_minutes":0}}`},
	} {
		if result := f.call("alice", call.tool, call.arguments); result.IsError || result.Text != approval.ProposedText {
			t.Fatalf("%s after outside content = %+v", call.tool, result)
		}
	}
	f.exposure.external.Store(false)
	if result := f.call("alice", "place_delete", `{"name":"Gym"}`); result.IsError {
		t.Fatalf("place_delete with only proposals pending = %+v", result)
	}
	pending, err := f.store.PendingApprovals(t.Context(), "alice", 0)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v %v", pending, err)
	}
	for _, record := range pending {
		var outcome approval.Outcome
		switch record.Tool {
		case "watch_propose":
			outcome, err = f.watches.ExecuteApproved(t.Context(), record)
		default:
			outcome, err = f.scheduler.ExecuteApproved(t.Context(), record)
		}
		var refusal *approval.ActionError
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Text, "no longer exists") {
			t.Fatalf("confirming %s = %+v %v", record.Tool, outcome, err)
		}
	}
	if watches, _ := f.watches.Watches(t.Context(), "alice"); len(watches) != 0 {
		t.Fatalf("watches = %+v", watches)
	}
	if jobs, _ := f.scheduler.Jobs(t.Context(), "alice"); len(jobs) != 0 {
		t.Fatalf("jobs = %+v", jobs)
	}
}

// An arrival alerts a place watch only inside its window, and never while it is muted or paused
// for a trip.
func TestAPlaceWatchKeepsItsWindowMuteAndTravelPause(t *testing.T) {
	f := newPlacesFixture(t)
	f.share("alice", 0)
	if result := f.call("alice", "place_save", `{"name":"Gym"}`); result.IsError {
		t.Fatalf("place_save = %+v", result)
	}
	place, _, err := f.locations.PlaceNamed(t.Context(), "alice", "Gym")
	if err != nil {
		t.Fatal(err)
	}
	if result := f.call("alice", "watch_propose", `{"kind":"place","name":"Gym","triggers":["arrive"],"window_from":"09:00","window_until":"18:00","pause_while_traveling":true}`); result.IsError {
		t.Fatalf("watch_propose = %+v", result)
	}
	watches, err := f.watches.Watches(t.Context(), "alice")
	if err != nil || len(watches) != 1 {
		t.Fatalf("watches = %+v %v", watches, err)
	}
	alerts := func() int { return f.count(`SELECT COUNT(*) FROM watch_alerts`) }
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	arrive := func(at time.Time) {
		f.watches.PlaceEvent(t.Context(), "alice", place.ID, place.Name, watch.TriggerArrive, at)
	}
	arrive(day.Add(8 * time.Hour))
	if n := alerts(); n != 0 {
		t.Fatalf("an arrival before the window alerted: %d", n)
	}
	f.watches.PlaceEvent(t.Context(), "alice", place.ID, place.Name, watch.TriggerLeave, day.Add(10*time.Hour))
	if n := alerts(); n != 0 {
		t.Fatalf("a leaving alerted an arrive watch: %d", n)
	}
	f.travel.on.Store(true)
	arrive(day.Add(10 * time.Hour))
	if n := alerts(); n != 0 {
		t.Fatalf("an arrival while traveling alerted: %d", n)
	}
	f.travel.on.Store(false)
	arrive(day.Add(11 * time.Hour))
	if n := alerts(); n != 1 {
		t.Fatalf("an arrival in the window gave %d alerts", n)
	}
	f.mu.Lock()
	f.now = day.Add(12 * time.Hour)
	f.mu.Unlock()
	if muted, err := f.watches.Mute(t.Context(), "alice", watches[0].ID); err != nil || !muted {
		t.Fatalf("mute = %v %v", muted, err)
	}
	arrive(day.Add(13 * time.Hour))
	if n := alerts(); n != 1 {
		t.Fatalf("an arrival while muted alerted: %d", n)
	}
	f.watches.PlaceEvent(t.Context(), "bob", place.ID, place.Name, watch.TriggerArrive, day.Add(36*time.Hour))
	if n := alerts(); n != 1 {
		t.Fatalf("another principal's event alerted alice's watch: %d", n)
	}
}

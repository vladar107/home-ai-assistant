package trip_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

type exposure struct{ external atomic.Bool }

func (*exposure) UserChatRequest(context.Context) (string, bool) { return "a1", true }
func (e *exposure) External(context.Context, string) bool        { return e.external.Load() }
func (e *exposure) Exposed(context.Context, string) bool         { return e.external.Load() }

type deliverer struct{}

func (deliverer) SendText(context.Context, int64, string) (int64, error) { return 1, nil }
func (deliverer) SendProposal(context.Context, int64, string, approval.Buttons) (int64, error) {
	return 1, nil
}
func (deliverer) EditText(context.Context, int64, int64, string) error { return nil }
func (deliverer) EditProposal(context.Context, int64, int64, string, approval.Buttons) error {
	return nil
}
func (deliverer) SendPreview(context.Context, int64, int64, string) error { return nil }
func (deliverer) SendDirectActions(context.Context, int64, approval.DirectActions) (int64, error) {
	return 1, nil
}
func (deliverer) EditDirectActions(context.Context, int64, int64, approval.DirectActions) error {
	return nil
}

type toolsFixture struct {
	*fixture
	tools    *trip.Tools
	approval *approval.Service
	exposure *exposure
	chat     context.Context
}

func newToolsFixture(t *testing.T) *toolsFixture {
	t.Helper()
	f := newFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := approval.NewService(approval.Config{}, f.store, deliverer{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	exposed := &exposure{}
	service.SetExposure(exposed)
	offered, err := trip.NewTools(trip.ToolsConfig{Zone: zone}, f.trips, service, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterExecutor(offered); err != nil {
		t.Fatal(err)
	}
	return &toolsFixture{fixture: f, tools: offered, approval: service, exposure: exposed,
		chat: tools.WithTurn(t.Context(), outbox.ChatTurn("a1"))}
}

func (f *toolsFixture) call(name, arguments string) tools.Result {
	f.t.Helper()
	result, err := f.tools.Execute(f.chat, "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

const addCall = `{"title":"Example trip","destination":"Example City","starts_at":"2026-11-02 09:00","ends_at":"2026-11-06",` +
	`"bookings":[` +
	`{"kind":"flight","title":"Flight to Example City","reference":"ABC123","starts_at":"2026-11-02 09:00","check_in_at":"2026-11-01 09:00","check_in_source":"stated"},` +
	`{"kind":"hotel","title":"Example Hotel","starts_at":"2026-11-02 14:00","check_in_at":"2026-11-01 14:00","check_in_source":"default"},` +
	`{"kind":"car","title":"Example Rental","starts_at":"2026-11-02 12:00","check_in_at":"2026-10-14 12:00","check_in_source":"looked_up"},` +
	`{"kind":"train","title":"Example Train","starts_at":"2026-11-04 10:00"}]}`

func TestChatTripAddActsAtOnceAndNamesUnsetReminders(t *testing.T) {
	f := newToolsFixture(t)
	result := f.call("trip_add", addCall)
	if result.IsError {
		t.Fatalf("trip_add: %s", result.Text)
	}
	for _, want := range []string{"Saved the trip", "Flight to Example City at 2026-11-01 09:00 (stated in the booking)",
		"Example Hotel at 2026-11-01 14:00 (assumed: 24 hours before the booking starts)",
		"No reminder for: Example Rental (check-in is already open)", "No check-in time, so no reminder: Example Train"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("reply lacks %q:\n%s", want, result.Text)
		}
	}
	trips, err := f.trips.Trips(t.Context(), "alice")
	if err != nil || len(trips) != 1 || len(trips[0].Bookings) != 4 {
		t.Fatalf("trips = %+v, %v", trips, err)
	}
	if got := trips[0].EndsAt.In(lisbon(t)).Format("2006-01-02 15:04"); got != "2026-11-07 00:00" {
		t.Fatalf("a date-only end = %s, want the end of that day", got)
	}
	if got := f.reminders("alice"); len(got) != 2 {
		t.Fatalf("reminders = %d, want 2", len(got))
	}
}

func TestTripAddAfterOutsideContentIsProposedAndConfirmMakesIt(t *testing.T) {
	f := newToolsFixture(t)
	f.exposure.external.Store(true)
	result := f.call("trip_add", addCall)
	if result.IsError || result.Text != approval.ProposedText {
		t.Fatalf("trip_add = %+v, want a proposal", result)
	}
	if trips, _ := f.trips.Trips(t.Context(), "alice"); len(trips) != 0 {
		t.Fatalf("a proposal saved %d trips", len(trips))
	}
	pending, err := f.store.PendingApprovals(t.Context(), "alice")
	if err != nil || len(pending) != 1 || pending[0].Tool != "trip_add" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	outcome, err := f.tools.ExecuteApproved(t.Context(), pending[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outcome.Text, "Reminders set: Flight to Example City") {
		t.Fatalf("outcome = %q", outcome.Text)
	}
	if trips, _ := f.trips.Trips(t.Context(), "alice"); len(trips) != 1 {
		t.Fatalf("trips after confirm = %d, want 1", len(trips))
	}
	if got := f.reminders("alice"); len(got) != 2 {
		t.Fatalf("reminders = %d, want 2", len(got))
	}
}

func TestTripUpdateChangesBookingsAndEndsTheTrip(t *testing.T) {
	f := newToolsFixture(t)
	f.call("trip_add", addCall)
	trips, _ := f.trips.Trips(t.Context(), "alice")
	saved := trips[0]
	flight := saved.Bookings[0]

	change, _ := json.Marshal(map[string]any{
		"id":                 saved.ID,
		"destination":        "Other City",
		"change_bookings":    []map[string]any{{"id": flight.ID, "check_in_at": "2026-11-01 07:00", "check_in_source": "looked_up"}},
		"remove_booking_ids": []string{saved.Bookings[2].ID},
	})
	result := f.call("trip_update", string(change))
	if result.IsError || !strings.Contains(result.Text, "Flight to Example City at 2026-11-01 07:00 (looked up on the provider's site)") {
		t.Fatalf("trip_update = %+v", result)
	}
	updated, _, _ := f.trips.Trip(t.Context(), "alice", saved.ID)
	if updated.Destination != "Other City" || len(updated.Bookings) != 3 {
		t.Fatalf("updated = %+v", updated)
	}

	if result = f.call("trip_update", `{"id":"`+saved.ID+`","cancel":true,"title":"Renamed"}`); !result.IsError {
		t.Fatalf("cancel with an edit = %+v, want an error", result)
	}
	if result = f.call("trip_update", `{"id":"`+saved.ID+`","cancel":true}`); result.IsError || !strings.Contains(result.Text, "Cancelled") {
		t.Fatalf("cancel = %+v", result)
	}
	if got := f.reminders("alice"); len(got) != 0 {
		t.Fatalf("reminders after cancel = %d, want 0", len(got))
	}
}

func TestTripUpdateEndNowEndsAnActiveTrip(t *testing.T) {
	f := newToolsFixture(t)
	f.call("trip_add", addCall)
	trips, _ := f.trips.Trips(t.Context(), "alice")
	f.clock.Set(days(18))
	result := f.call("trip_update", `{"id":"`+trips[0].ID+`","end_now":true}`)
	if result.IsError || !strings.Contains(result.Text, "Ended the trip") {
		t.Fatalf("end_now = %+v", result)
	}
	if traveling, _ := f.trips.Traveling(t.Context(), "alice", f.clock.Now().Add(time.Minute)); traveling {
		t.Fatal("the trip is still active after end_now")
	}
}

func TestTripListShowsBookingsAndWhichHaveNoReminder(t *testing.T) {
	f := newToolsFixture(t)
	if result := f.call("trip_list", `{}`); result.IsError || !strings.Contains(result.Text, "no upcoming or active trips") {
		t.Fatalf("empty list = %+v", result)
	}
	f.call("trip_add", addCall)
	result := f.call("trip_list", `{}`)
	for _, want := range []string{`"Example trip", "Example City" (planned; source: chat): 2026-11-02 09:00 to 2026-11-06, Europe/Lisbon`,
		`flight: "Flight to Example City" ("ABC123"), starts 2026-11-02 09:00; check-in 2026-11-01 09:00 (stated in the booking)`,
		`"Example Rental", starts 2026-11-02 12:00; check-in 2026-10-14 12:00 (looked up on the provider's site); no reminder: check-in is already open`,
		`"Example Train", starts 2026-11-04 10:00; no check-in time, no reminder`} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("list lacks %q:\n%s", want, result.Text)
		}
	}
	trips, _ := f.trips.Trips(t.Context(), "alice")
	if one := f.call("trip_list", `{"id":"`+trips[0].ID+`"}`); one.IsError || !strings.Contains(one.Text, "["+trips[0].ID+"]") {
		t.Fatalf("one trip = %+v", one)
	}
	if other, err := f.tools.Execute(f.chat, "bob", tools.Call{Name: "trip_list", Arguments: json.RawMessage(`{"id":"` + trips[0].ID + `"}`)}); err != nil || !other.IsError {
		t.Fatalf("another principal read the trip: %+v %v", other, err)
	}
}

func TestTripToolsRejectUnknownFieldsAndOfferWritesOnChatOnly(t *testing.T) {
	f := newToolsFixture(t)
	for _, call := range []struct{ name, arguments string }{
		{"trip_add", `{"title":"Example trip","destination":"Example City","starts_at":"2026-11-02","ends_at":"2026-11-03","extra":1}`},
		{"trip_add", `{"title":"Example trip","destination":"Example City","starts_at":"2026-11-02","ends_at":"2026-11-03","bookings":[{"kind":"hotel","title":"Hotel","starts_at":"2026-11-02 14:00","note":"x"}]}`},
		{"trip_update", `{"id":"abc","unknown":true}`},
		{"trip_list", `{"status":"open"}`},
	} {
		if result := f.call(call.name, call.arguments); !result.IsError || !strings.Contains(result.Text, "do not match the tool's schema") {
			t.Errorf("%s with unknown fields = %+v", call.name, result)
		}
	}
	if result := f.call("trip_add", `{"title":"Example trip","destination":"see https://example.com","starts_at":"2026-11-02","ends_at":"2026-11-03"}`); !result.IsError {
		t.Errorf("a link in the destination = %+v, want an error", result)
	}

	names := func(ctx context.Context) []string {
		contribution, err := f.tools.Contribute(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, definition := range contribution.Definitions {
			found = append(found, definition.Name)
		}
		return found
	}
	if got := names(f.chat); len(got) != 3 {
		t.Fatalf("chat tools = %v", got)
	}
	if got := names(tools.WithTurn(t.Context(), outbox.ScheduledTurn("b2"))); len(got) != 1 || got[0] != "trip_list" {
		t.Fatalf("scheduled tools = %v", got)
	}
}

func lisbon(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func TestConfirmedUpdateIsRejectedWhenTheTripChangedSincePropose(t *testing.T) {
	f := newToolsFixture(t)
	suggested, err := f.trips.Suggest(t.Context(), "alice", mailTrip(10, 15, booking(trip.KindFlight, "Flight to Example City", "ABC123", days(10), time.Time{})))
	if err != nil {
		t.Fatal(err)
	}
	id := suggested.Trip.ID
	f.exposure.external.Store(true)
	if result := f.call("trip_update", `{"id":"`+id+`","title":"Renamed trip"}`); result.Text != approval.ProposedText {
		t.Fatalf("update = %+v, want a proposal", result)
	}
	pending, err := f.store.PendingApprovals(t.Context(), "alice")
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	f.clock.Set(start.Add(time.Hour))
	merged, err := f.trips.Suggest(t.Context(), "alice", mailTrip(10, 15,
		booking(trip.KindFlight, "Flight to Example City", "ABC123", days(10), time.Time{}),
		booking(trip.KindHotel, "Example Hotel", "HOTEL9", days(10), time.Time{})))
	if err != nil || merged.Outcome != trip.OutcomeUpdated {
		t.Fatalf("merge = %+v, %v", merged, err)
	}
	_, err = f.tools.ExecuteApproved(t.Context(), pending[0])
	var refused *approval.ActionError
	if !errors.As(err, &refused) || !strings.Contains(refused.Text, "changed since") {
		t.Fatalf("confirm = %v, want a stale rejection", err)
	}
	if got, _, _ := f.trips.Trip(t.Context(), "alice", id); got.Title == "Renamed trip" || len(got.Bookings) != 2 {
		t.Fatalf("the stale change was applied: %+v", got)
	}
}

func TestTripListHidesSuggestedTripsAndQuotesTrackedOnes(t *testing.T) {
	f := newToolsFixture(t)
	suggested, err := f.trips.Suggest(t.Context(), "alice", mailTrip(10, 15, booking(trip.KindFlight, "Flight to Example City", "ABC123", days(10), time.Time{})))
	if err != nil {
		t.Fatal(err)
	}
	if result := f.call("trip_list", `{}`); !strings.Contains(result.Text, "no upcoming or active trips") {
		t.Fatalf("list = %s", result.Text)
	}
	if result := f.call("trip_list", `{"id":"`+suggested.Trip.ID+`"}`); !strings.Contains(result.Text, "not tracked yet") || strings.Contains(result.Text, "Flight") {
		t.Fatalf("one = %s", result.Text)
	}
	if _, err := f.trips.Track(t.Context(), "alice", suggested.Trip.ID); err != nil {
		t.Fatal(err)
	}
	if result := f.call("trip_list", `{}`); !strings.Contains(result.Text, "source: mail") || !strings.Contains(result.Text, `"Flight to Example City"`) {
		t.Fatalf("list = %s", result.Text)
	}
}

func TestEndNowIsNotProposedForATripThatIsNotUnderWay(t *testing.T) {
	f := newToolsFixture(t)
	planned := f.add(draft(3, 5))
	if result := f.call("trip_update", `{"id":"`+planned.ID+`","end_now":true}`); !result.IsError {
		t.Fatalf("end_now = %+v", result)
	}
}

func TestCancelOfASuggestedTripDismissesIt(t *testing.T) {
	f := newToolsFixture(t)
	suggested, _ := f.trips.Suggest(t.Context(), "alice", mailTrip(10, 15, booking(trip.KindFlight, "Flight", "ABC123", days(10), time.Time{})))
	if result := f.call("trip_update", `{"id":"`+suggested.Trip.ID+`","cancel":true}`); result.IsError || !strings.Contains(result.Text, "Dismissed") {
		t.Fatalf("cancel = %+v", result)
	}
	if got, _, _ := f.trips.Trip(t.Context(), "alice", suggested.Trip.ID); got.Status != trip.StatusDismissed {
		t.Fatalf("status = %s", got.Status)
	}
}

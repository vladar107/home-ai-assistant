package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

type noCheck struct{}

func (noCheck) RunCheck(context.Context, string, string) (string, string, error) {
	return `{"trips":[]}`, "", nil
}

type tripHarness struct {
	*conversationHarness
	clock *testClock
	trips *trip.Service
}

func newTripHarness(t *testing.T) (*tripHarness, *googlesim.Simulator) {
	t.Helper()
	clock := newTestClock(t, mondayMorning)
	sim := googleSimulator(t)
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{schedules: clock, google: sim, calendar: calendarSimulator(t, sim)})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	trips, err := trip.NewService(trip.Config{Now: clock.Now}, h.store, trip.NewScheduleReminders(h.scheduler), logger)
	if err != nil {
		t.Fatal(err)
	}
	notifier := TripNotifier{Client: h.client, Chats: h.store}
	if err := h.scheduler.Register(trip.ScanJobType(noCheck{}, trips, notifier, notifier, clock.Now, logger)); err != nil {
		t.Fatal(err)
	}
	h.service.SetTrips(trips)
	h.start()
	return &tripHarness{conversationHarness: h, clock: clock, trips: trips}, sim
}

func (h *tripHarness) suggest(reference string) trip.Trip {
	h.t.Helper()
	now := h.clock.Now()
	result, err := h.trips.Suggest(h.ctx, "alice", trip.Draft{
		Title: "Example trip", Destination: "Example City", Zone: "Europe/Lisbon", Source: trip.SourceMail,
		StartsAt: now.Add(72 * time.Hour), EndsAt: now.Add(120 * time.Hour),
		Bookings: []trip.Booking{{
			Kind: trip.KindFlight, Title: "Flight to Example City", Reference: reference,
			StartsAt: now.Add(72 * time.Hour), CheckInAt: now.Add(48 * time.Hour), CheckInSource: trip.CheckInLookedUp,
		}, {
			Kind: trip.KindHotel, Title: "Example Hotel", Reference: reference + "H",
			StartsAt: now.Add(73 * time.Hour), CheckInAt: now.Add(-time.Hour), CheckInSource: trip.CheckInDefault,
		}},
	})
	if err != nil || result.Rejection != "" || result.Outcome != trip.OutcomeNew {
		h.t.Fatalf("suggest: %v %+v", err, result)
	}
	return result.Trip
}

func (h *tripHarness) tripStatus(id string) string {
	h.t.Helper()
	var status string
	if err := h.db.QueryRowContext(h.ctx, `SELECT status FROM trips WHERE id = ?`, id).Scan(&status); err != nil {
		h.t.Fatal(err)
	}
	return status
}

func (h *tripHarness) card(found trip.Trip) sentMessage {
	h.t.Helper()
	if err := (TripNotifier{Client: h.client, Chats: h.store}).SendTripCard(h.ctx, found); err != nil {
		h.t.Fatal(err)
	}
	return h.nextMessage()
}

func (h *tripHarness) pressCard(updateID, userID, messageID int64, data string) editedMessage {
	h.t.Helper()
	h.push(callbackUpdate(updateID, userID, messageID, data))
	h.wantAnswer(updateID)
	return h.nextEdit()
}

// A suggested trip arrives as a plain card with Track and Ignore; only its owner's press counts,
// Track plans it with its reminders and says which could not be set, Ignore dismisses it for good,
// and a stale press answers without changing anything.
func TestTripCardTracksAndIgnoresForItsOwnerOnly(t *testing.T) {
	h, _ := newTripHarness(t)
	first, second := h.suggest("ABC123"), h.suggest("XYZ789")

	card := h.card(first)
	want := []string{"trip:track:" + first.ID, "trip:ignore:" + first.ID}
	if card.chatID != 101 || card.parseMode != "" || !slices.Equal(buttonData(card.keyboard), want) {
		t.Fatalf("card = %+v", card)
	}
	for _, part := range []string{"Example trip (Example City)", "Flight to Example City", "(ABC123)", "looked up on the provider's site", "assumed, 24 h before the start"} {
		if !strings.Contains(card.text, part) {
			t.Fatalf("card lacks %q: %q", part, card.text)
		}
	}

	if edit := h.pressCard(1, 202, card.messageID, want[0]); !strings.Contains(edit.text, TripGoneReply) || h.tripStatus(first.ID) != trip.StatusSuggested {
		t.Fatalf("bob's press = %+v, status %s", edit, h.tripStatus(first.ID))
	}

	edit := h.pressCard(2, 101, card.messageID, want[0])
	if !strings.HasPrefix(edit.text, "Tracking: Example trip (Example City)") || !strings.Contains(edit.text, "Reminder for Flight to Example City") ||
		!strings.Contains(edit.text, "No reminder for Example Hotel: check-in is already open.") || len(buttonData(edit.keyboard)) != 0 {
		t.Fatalf("track edit = %+v", edit)
	}
	if h.tripStatus(first.ID) != trip.StatusPlanned || h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE job_type = ?`, schedule.TypeReminder) != 1 {
		t.Fatalf("status %s, reminders %d", h.tripStatus(first.ID), h.countRows(`SELECT count(*) FROM scheduled_jobs`))
	}
	if edit := h.pressCard(3, 101, card.messageID, want[1]); !strings.Contains(edit.text, TripTrackedReply) || h.tripStatus(first.ID) != trip.StatusPlanned {
		t.Fatalf("stale ignore = %+v", edit)
	}

	other := h.card(second)
	ignore := "trip:ignore:" + second.ID
	if edit := h.pressCard(4, 101, other.messageID, ignore); !strings.Contains(edit.text, TripIgnoredReply) || h.tripStatus(second.ID) != trip.StatusDismissed {
		t.Fatalf("ignore = %+v", edit)
	}
	if edit := h.pressCard(5, 101, other.messageID, "trip:track:"+second.ID); !strings.Contains(edit.text, TripIgnoredReply) || h.tripStatus(second.ID) != trip.StatusDismissed {
		t.Fatalf("stale track = %+v", edit)
	}
	if edit := h.pressCard(6, 101, other.messageID, "trip:track:ffffffff"); !strings.Contains(edit.text, TripGoneReply) {
		t.Fatalf("unknown trip = %+v", edit)
	}
}

// The Trips switch is off by default, is offered only once Mail or Calendar can be read, and
// creates, resumes, and deletes the principal's own scan job.
func TestTripsSwitchNeedsGoogleAndManagesItsJob(t *testing.T) {
	h, sim := newTripHarness(t)

	h.requestSettings(1, 101)
	messageID := settingsMessageID(1)
	page := h.openSettingsPage(2, 101, messageID, "tools")
	if !strings.Contains(page.text, TripsNeedsGoogleLine) || slices.Contains(buttonData(page.keyboard), "trips:on") {
		t.Fatalf("page without Google = %+v", page)
	}
	if page = h.press(3, 101, messageID, "trips:on"); h.countRows(`SELECT count(*) FROM scheduled_jobs`) != 0 {
		t.Fatalf("the switch created a job without Google: %+v", page)
	}

	if reply := h.connectGoogle(sim, 10, 101, aliceMailbox, bothProducts); !strings.Contains(reply, "is connected read-only") {
		t.Fatalf("connected reply = %q", reply)
	}
	page = h.openSettingsPage(20, 101, settingsMessageID(10), "tools")
	if !strings.Contains(page.text, TripsOffLine) || !slices.Contains(buttonData(page.keyboard), "trips:on") {
		t.Fatalf("page with Google = %+v", page)
	}
	messageID = settingsMessageID(10)
	page = h.press(21, 101, messageID, "trips:on")
	if !strings.Contains(page.text, TripsOnLine) || !slices.Contains(buttonData(page.keyboard), "trips:off") {
		t.Fatalf("page after on = %+v", page)
	}
	h.press(22, 101, messageID, "trips:on")

	jobs, err := h.scheduler.Jobs(h.ctx, "alice")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	if job := jobs[0]; job.ID != schedule.TripsJobID("alice") || job.Name != schedule.TripsName || job.Cron != schedule.TripsCron ||
		job.Type != schedule.TripScanType || job.Zone != h.scheduler.DefaultZone() || !job.Enabled || !json.Valid(job.Config) {
		t.Fatalf("job = %+v", job)
	}
	if bobs, err := h.scheduler.Jobs(h.ctx, "bob"); err != nil || len(bobs) != 0 {
		t.Fatalf("bob's jobs = %+v %v", bobs, err)
	}

	page = h.press(23, 101, messageID, "trips:off")
	if !strings.Contains(page.text, TripsOffLine) || h.countRows(`SELECT count(*) FROM scheduled_jobs`) != 0 {
		t.Fatalf("page after off = %+v", page)
	}
}

func TestTripCardStaysUnderTelegramsLimitWithManyBookings(t *testing.T) {
	found := trip.Trip{ID: "abcd1234", Title: strings.Repeat("t", 80), Destination: strings.Repeat("d", 80), Zone: "Europe/Lisbon",
		Source: trip.SourceMail, StartsAt: time.Now(), EndsAt: time.Now().Add(48 * time.Hour)}
	for range trip.MaximumBookings {
		found.Bookings = append(found.Bookings, trip.Booking{Kind: trip.KindOther, Title: strings.Repeat("b", 80), Reference: strings.Repeat("r", 40),
			StartsAt: time.Now(), CheckInAt: time.Now(), CheckInSource: trip.CheckInStated})
	}
	text := tripCardText(found)
	if len([]rune(text)) > 4096 || !strings.Contains(text, "...and 12 more") {
		t.Fatalf("card = %d runes: %s", len([]rune(text)), text)
	}
}

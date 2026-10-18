package telegram

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// scanReply is the model, which answers each Trips scan with nothing found until a test sets the
// answer. While hold is set, a scan waits for it to close.
type scanReply struct {
	mu   sync.Mutex
	text string
	hold chan struct{}
}

func (r *scanReply) Run(ctx context.Context, _ agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
	r.mu.Lock()
	text, hold := r.text, r.hold
	r.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return agentruntime.RunResult{}, ctx.Err()
		}
	}
	return agentruntime.RunResult{Final: cmp.Or(text, `{"trips":[]}`)}, nil
}

func (r *scanReply) holdScans() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hold = make(chan struct{})
	return r.hold
}

func (r *scanReply) set(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.text = text
}

type tripHarness struct {
	*conversationHarness
	clock *testClock
	trips *trip.Service
	scan  *scanReply
}

func newTripHarness(t *testing.T) (*tripHarness, *googlesim.Simulator) {
	t.Helper()
	clock := newTestClock(t, mondayMorning)
	sim := googleSimulator(t)
	reply := &scanReply{}
	h := newConversationHarnessWithOptions(t, reply, harnessOptions{schedules: clock, google: sim, calendar: calendarSimulator(t, sim)})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	return &tripHarness{conversationHarness: h, clock: clock, trips: h.trips, scan: reply}, sim
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

// Turning Trips on scans at once, both when it creates the job and when it resumes a paused one,
// and Scan now queues one more run; a press on a switch that is already on queues nothing. A run
// with nothing new says so, and one that sent a card says how many it found.
func TestTripsSwitchScansAtOnceAndScanNowScansAgain(t *testing.T) {
	h, sim := newTripHarness(t)
	h.connectGoogle(sim, 1, 101, aliceMailbox, bothProducts)
	id := schedule.TripsJobID("alice")
	manualRuns := func() int {
		return h.countRows(`SELECT count(*) FROM schedule_runs WHERE job_id = ? AND manual = 1`, id)
	}
	wantQuietReport := func() {
		t.Helper()
		if report := h.nextMessage(); report.chatID != 101 || !strings.Contains(report.text, schedule.QuietManualText) {
			t.Fatalf("report = %+v", report)
		}
	}

	h.requestSettings(10, 101)
	messageID := settingsMessageID(10)
	h.openSettingsPage(11, 101, messageID, "tools")
	page := h.press(12, 101, messageID, "trips:on")
	if !strings.Contains(page.text, TripsScanningNote) || !slices.Contains(buttonData(page.keyboard), "trips:scan") || manualRuns() != 1 {
		t.Fatalf("page after on = %+v, manual runs %d", page, manualRuns())
	}
	wantQuietReport()
	if page = h.press(13, 101, messageID, "trips:on"); strings.Contains(page.text, TripsScanningNote) || manualRuns() != 1 {
		t.Fatalf("a second on press = %+v, manual runs %d", page, manualRuns())
	}

	if changed, err := h.scheduler.SetEnabled(h.ctx, "alice", id, false); err != nil || !changed {
		t.Fatalf("pause: %v %v", changed, err)
	}
	if page = h.press(14, 101, messageID, "trips:scan"); slices.Contains(buttonData(page.keyboard), "trips:scan") || manualRuns() != 1 {
		t.Fatalf("scan while paused = %+v, manual runs %d", page, manualRuns())
	}
	h.clock.Set(t, "2026-09-28T08:05:00+02:00")
	if page = h.press(15, 101, messageID, "trips:on"); !strings.Contains(page.text, TripsScanningNote) || manualRuns() != 2 {
		t.Fatalf("page after resume = %+v, manual runs %d", page, manualRuns())
	}
	wantQuietReport()

	h.clock.Set(t, "2026-09-28T08:10:00+02:00")
	if page = h.press(16, 101, messageID, "trips:scan"); !strings.Contains(page.text, TripsScanningNote) || manualRuns() != 3 {
		t.Fatalf("page after scan now = %+v, manual runs %d", page, manualRuns())
	}
	wantQuietReport()

	h.scan.set(`{"trips":[{"title":"Example trip","destination":"Example City","timezone":"Europe/Lisbon",` +
		`"starts_at":"2026-10-10T09:00:00Z","ends_at":"2026-10-15T18:00:00Z","bookings":[{"kind":"flight","title":"Flight to Example City",` +
		`"reference":"ABC123","starts_at":"2026-10-10T09:00:00Z","check_in_at":"2026-10-09T09:00:00Z","check_in_source":"stated"}]}]}`)
	h.clock.Set(t, "2026-09-28T08:15:00+02:00")
	h.press(17, 101, messageID, "trips:scan")
	if card := h.nextMessage(); !strings.HasPrefix(card.text, "Trip found in mail:") || len(buttonData(card.keyboard)) != 2 {
		t.Fatalf("card = %+v", card)
	}
	if report := h.nextMessage(); !strings.Contains(report.text, "Found 1 new trip in your mail and calendar; its card is above.") ||
		strings.Contains(report.text, schedule.QuietManualText) {
		t.Fatalf("report after a find = %+v", report)
	}
}

// While a scan is queued or running, neither Scan now nor turning Trips back on queues another.
func TestTripsScanNowWaitsForTheScanUnderWay(t *testing.T) {
	h, sim := newTripHarness(t)
	h.connectGoogle(sim, 1, 101, aliceMailbox, bothProducts)
	id := schedule.TripsJobID("alice")
	manualRuns := func() int {
		return h.countRows(`SELECT count(*) FROM schedule_runs WHERE job_id = ? AND manual = 1`, id)
	}
	release := h.scan.holdScans()

	h.requestSettings(10, 101)
	messageID := settingsMessageID(10)
	h.openSettingsPage(11, 101, messageID, "tools")
	if page := h.press(12, 101, messageID, "trips:on"); !strings.Contains(page.text, TripsScanningNote) || manualRuns() != 1 {
		t.Fatalf("page after on = %+v, manual runs %d", page, manualRuns())
	}
	h.clock.Set(t, "2026-09-28T08:05:00+02:00")
	if page := h.press(13, 101, messageID, "trips:scan"); !strings.Contains(page.text, TripsBusyNote) || manualRuns() != 1 {
		t.Fatalf("scan now during a scan = %+v, manual runs %d", page, manualRuns())
	}
	if changed, err := h.scheduler.SetEnabled(h.ctx, "alice", id, false); err != nil || !changed {
		t.Fatalf("pause: %v %v", changed, err)
	}
	if page := h.press(14, 101, messageID, "trips:on"); !strings.Contains(page.text, TripsBusyNote) || manualRuns() != 1 {
		t.Fatalf("resume during a scan = %+v, manual runs %d", page, manualRuns())
	}

	close(release)
	if report := h.nextMessage(); !strings.Contains(report.text, schedule.QuietManualText) {
		t.Fatalf("report = %+v", report)
	}
	h.waitRun(id, schedule.StatusDelivered)
	if page := h.press(15, 101, messageID, "trips:scan"); !strings.Contains(page.text, TripsScanningNote) || manualRuns() != 2 {
		t.Fatalf("scan now after the scan = %+v, manual runs %d", page, manualRuns())
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

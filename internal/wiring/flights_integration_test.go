package wiring

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/flights"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// departs is when the made-up flight EX123 is scheduled to leave Example City (XAA, Lisbon time,
// UTC in November) for Other City (XBB, Athens time, UTC+2); it lands at 17:55 UTC.
var (
	departs = time.Date(2026, 11, 9, 13, 40, 0, 0, time.UTC)
	arrives = time.Date(2026, 11, 9, 17, 55, 0, 0, time.UTC)
)

// fakeLeg is what the fake AeroDataBox reports for EX123 right now.
type fakeLeg struct {
	status   string
	revised  time.Time
	arrival  time.Time
	runway   time.Time
	gate     string
	terminal string
	live     bool
	divertTo bool
	// arrivalScheduled replaces the arrival's schedule when set.
	arrivalScheduled time.Time
}

// flightProvider stands in for AeroDataBox at its HTTP boundary: it answers lookups of EX123 for
// its origin-local day, or for its arrival's day when asked for either end, from the current
// fakeLeg, can fail or hang, and counts what it was asked.
type flightProvider struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	leg    fakeLeg
	fail   bool
	hang   bool
	asked  []string
	// arrived hears of each request as it arrives, when set.
	arrived chan struct{}
}

func newFlightProvider(t *testing.T) *flightProvider {
	p := &flightProvider{t: t, leg: fakeLeg{status: "Expected", live: true, gate: "", terminal: "1"}}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	return p
}

func (p *flightProvider) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.asked = append(p.asked, r.URL.Path+"?"+r.URL.Query().Get("dateLocalRole"))
	leg, fail, hang, arrived := p.leg, p.fail, p.hang, p.arrived
	p.mu.Unlock()
	if arrived != nil {
		arrived <- struct{}{}
	}
	if hang {
		<-r.Context().Done()
		return
	}
	if fail {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	designator, day := parts[len(parts)-2], parts[len(parts)-1]
	onDay := departs.Format(time.DateOnly) == day ||
		r.URL.Query().Get("dateLocalRole") == "Both" && arrives.In(athens(p.t)).Format(time.DateOnly) == day
	if designator != "EX123" || !onDay {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = json.NewEncoder(w).Encode([]any{leg.json()})
}

func (p *flightProvider) set(change func(*fakeLeg)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(&p.leg)
}

func (p *flightProvider) failing(fail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = fail
}

func (p *flightProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.asked)
}

func (p *flightProvider) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.asked)
}

func athens(t *testing.T) *time.Location {
	location, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func (l fakeLeg) json() map[string]any {
	at := func(instant time.Time) map[string]string {
		return map[string]string{"utc": instant.UTC().Format("2006-01-02 15:04Z"), "local": instant.Format("2006-01-02 15:04Z07:00")}
	}
	quality := []string{"Basic"}
	if l.live {
		quality = append(quality, "Live")
	}
	departure := map[string]any{
		"airport":       map[string]string{"iata": "XAA", "name": "Example City International", "municipalityName": "Example City", "timeZone": "Europe/Lisbon"},
		"scheduledTime": at(departs), "terminal": l.terminal, "quality": quality,
	}
	if l.gate != "" {
		departure["gate"] = l.gate
	}
	if !l.revised.IsZero() {
		departure["revisedTime"] = at(l.revised)
	}
	if !l.runway.IsZero() {
		departure["runwayTime"] = at(l.runway)
	}
	destination := map[string]string{"iata": "XBB", "name": "Other City Airport", "municipalityName": "Other City", "timeZone": "Europe/Athens"}
	if l.divertTo {
		destination = map[string]string{"iata": "XCC", "name": "Third City Airport", "municipalityName": "Third City", "timeZone": "Europe/Athens"}
	}
	arrival := map[string]any{"airport": destination, "scheduledTime": at(arrives), "quality": quality}
	if !l.arrival.IsZero() {
		arrival["predictedTime"] = at(l.arrival)
	}
	if !l.arrivalScheduled.IsZero() {
		arrival["scheduledTime"] = at(l.arrivalScheduled)
	}
	return map[string]any{"number": "EX 123", "status": l.status, "codeshareStatus": "IsOperator", "airline": map[string]string{"name": "Example Air"},
		"departure": departure, "arrival": arrival}
}

type flightClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *flightClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *flightClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

type scanSwitch struct {
	mu  sync.Mutex
	off map[string]bool
}

func (s *scanSwitch) ScanEnabled(_ context.Context, principalID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.off[principalID], nil
}

type tabMessage struct {
	principalID, tripID, text string
}

// tabSender records what reaches each trip's tab.
type tabSender struct {
	mu   sync.Mutex
	sent []tabMessage
}

func (s *tabSender) Send(_ context.Context, principalID, tripID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, tabMessage{principalID, tripID, text})
	return nil
}

func (s *tabSender) TripTab(context.Context, string, string) (int64, error) { return 0, nil }

// flightTexts are the flight messages sent so far, without trip notices.
func (s *tabSender) flightTexts(principalID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var texts []string
	for _, message := range s.sent {
		if message.principalID == principalID && strings.HasSuffix(message.text, flights.Attribution) {
			texts = append(texts, message.text)
		}
	}
	return texts
}

// tripLanguage writes every message in the user's language as the 0146 rewrite would: each line
// marked, its numbers kept. While hold is set it waits until released or the rewrite gives up.
type tripLanguage struct {
	mu      sync.Mutex
	hold    chan struct{}
	waiting chan struct{}
	once    sync.Once
}

func (l *tripLanguage) LatestMessage(context.Context, string, int64, bool) (string, bool, error) {
	return "Hoe laat vertrekt mijn vlucht?", true, nil
}

func (l *tripLanguage) RewriteAlertLike(ctx context.Context, _, _, text string) (string, error) {
	l.mu.Lock()
	hold, waiting := l.hold, l.waiting
	l.mu.Unlock()
	if hold != nil {
		l.once.Do(func() { close(waiting) })
		select {
		case <-hold:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = "NL " + line
	}
	return strings.Join(lines, "\n"), nil
}

type flightFixture struct {
	t        *testing.T
	ctx      context.Context
	clock    *flightClock
	provider *flightProvider
	store    *sqlite.Store
	path     string
	sender   *tabSender
	scan     *scanSwitch
	language *tripLanguage
	switches *integration.Switches
	service  *flights.Service
	loop     *trip.Loop
}

func newFlightFixture(t *testing.T, monthly, resetDay int) *flightFixture {
	t.Helper()
	f := &flightFixture{t: t, ctx: t.Context(), clock: &flightClock{now: departs.Add(-48 * time.Hour)}, provider: newFlightProvider(t),
		path: filepath.Join(t.TempDir(), "home-agent.db"), sender: &tabSender{}, scan: &scanSwitch{off: map[string]bool{}}, language: &tripLanguage{}}
	f.store = testutil.OpenStore(t, f.path)
	t.Cleanup(func() { _ = f.store.Close() })
	if err := f.store.BootstrapConfiguredPrincipals(f.ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var err error
	if f.switches, err = integration.NewSwitches(f.store, logger); err != nil {
		t.Fatal(err)
	}
	f.service, err = flights.NewService(flights.Config{
		APIKey: "test-key", MonthlyUnits: monthly, QuotaResetDay: resetDay, Zone: time.UTC, BaseURL: f.provider.server.URL,
		Now: f.clock.Now, Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	}, f.store, f.switches, logger)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := schedule.NewService(schedule.Config{Zone: "Europe/Lisbon", Now: f.clock.Now}, f.store, f.store, struct{ conversation.Deliverer }{}, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	trips, err := trip.NewService(trip.Config{Now: f.clock.Now}, f.store, trip.NewScheduleReminders(scheduler), logger)
	if err != nil {
		t.Fatal(err)
	}
	language, err := trip.NewLanguage(f.sender, f.language, f.language, f.store, logger)
	if err != nil {
		t.Fatal(err)
	}
	tracking, err := trip.NewFlights(trip.FlightsConfig{Now: f.clock.Now, Language: language, Attribution: flights.Attribution},
		f.store, f.store, f.store, tripFlights{service: f.service}, f.scan, f.sender, trip.NewUpdateNotices(), logger)
	if err != nil {
		t.Fatal(err)
	}
	if f.loop, err = trip.NewLoop(trip.LoopConfig{Now: f.clock.Now, Flights: tracking}, trips, nil, f.sender, logger); err != nil {
		t.Fatal(err)
	}
	return f
}

// addTrip plans a trip to Other City for the principal with one flight booking of EX123 at the
// stored instant, in the trip's zone.
func (f *flightFixture) addTrip(principalID, id string, booked time.Time) {
	f.t.Helper()
	created := f.clock.Now()
	if err := f.store.AddTrip(f.ctx, trip.Trip{
		ID: id, PrincipalID: principalID, Title: "Other City", Destination: "Other City",
		StartsAt: departs.Add(-2 * time.Hour), EndsAt: departs.Add(96 * time.Hour), Zone: "Europe/Athens",
		Status: trip.StatusPlanned, Source: trip.SourceChat, CreatedAt: created, UpdatedAt: created,
		Bookings: []trip.Booking{{ID: id[:4] + "b001", Kind: trip.KindFlight, Title: "Flight to Other City", FlightNumber: "EX123", StartsAt: booked}},
	}); err != nil {
		f.t.Fatal(err)
	}
}

// at runs one loop cycle at the instant and waits for its lookup and messages.
func (f *flightFixture) at(instant time.Time) {
	f.t.Helper()
	f.clock.Set(instant)
	// A cycle starts at most one lookup, so one runs per trip at each instant: every leg due then
	// is served, as the minute-long cycles of the real loop would within the cache's lifetime.
	for range 2 {
		f.loop.Cycle(f.ctx)
		f.loop.Wait()
	}
}

// walk runs the cycles every five minutes from one instant to another, both included, calling
// step first at each.
func (f *flightFixture) walk(from, to time.Time, step func(time.Time)) {
	f.t.Helper()
	for at := from; !at.After(to); at = at.Add(5 * time.Minute) {
		if step != nil {
			step(at)
		}
		f.at(at)
	}
}

func stripped(texts []string) []string {
	out := make([]string, 0, len(texts))
	for _, text := range texts {
		lines := strings.Split(strings.TrimSuffix(text, "\n"+flights.Attribution), "\n")
		for index, line := range lines {
			lines[index] = strings.TrimPrefix(line, "NL ")
		}
		out = append(out, strings.Join(lines, "\n"))
	}
	return out
}

// A leg is looked up eight times from its match to twenty minutes after it lands, never outside
// its window; the baseline of an on-time flight is silent, and a 45-minute delay is told once, in
// the tab's language with the source line untranslated after it.
func TestFlightTrackingFollowsALegOnItsSlotsAndTellsADelayOnce(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	f.addTrip("alice", "aaaa0001", departs)
	f.walk(departs.Add(-48*time.Hour), departs.Add(-38*time.Hour-5*time.Minute), nil)
	if f.provider.count() != 0 {
		t.Fatalf("looked up before the window: %v", f.provider.requests())
	}
	f.walk(departs.Add(-38*time.Hour), arrives.Add(3*time.Hour), func(at time.Time) {
		if at.Equal(departs.Add(-4 * time.Hour)) {
			f.provider.set(func(leg *fakeLeg) { leg.revised = departs.Add(45 * time.Minute) })
		}
		if at.Equal(departs.Add(-100 * time.Minute)) {
			// A wobble of ten minutes around the reported delay says nothing.
			f.provider.set(func(leg *fakeLeg) { leg.revised = departs.Add(55 * time.Minute) })
		}
		if at.Equal(departs.Add(50 * time.Minute)) {
			f.provider.set(func(leg *fakeLeg) { leg.status, leg.runway = "EnRoute", departs.Add(56*time.Minute) })
		}
		if at.Equal(arrives.Add(10 * time.Minute)) {
			f.provider.set(func(leg *fakeLeg) { leg.status = "Arrived" })
		}
	})
	requests := f.provider.requests()
	if len(requests) != 8 || requests[0] != "/flights/number/EX123/2026-11-09?Both" || requests[1] != "/flights/number/EX123/2026-11-09?Departure" {
		t.Fatalf("lookups = %v", requests)
	}
	texts := f.sender.flightTexts("alice")
	if want := []string{"EX123 Example City → Other City: departure delayed 45 min, now 14:25 (was 13:40)."}; !slices.Equal(stripped(texts), want) {
		t.Fatalf("messages = %q", texts)
	}
	if !strings.HasPrefix(texts[0], "NL ") || !strings.HasSuffix(texts[0], "\n"+flights.Attribution) {
		t.Fatalf("message not in the tab's language with the source after: %q", texts[0])
	}
	sent, err := f.store.ListSentUpdates(f.ctx, "alice", "aaaa0001", 10)
	if err != nil || len(sent) != 1 || sent[0].Source != trip.UpdateSourceFlight || !strings.HasPrefix(sent[0].Reference, "EX123|2026-11-09|departure|") {
		t.Fatalf("sent log = %+v %v", sent, err)
	}

	// A week after the leg ended, its lookup and its messages' text are gone; the identifiers stay.
	f.at(arrives.Add(8 * 24 * time.Hour))
	state, found, err := f.store.LoadFlightState(f.ctx, "alice", "aaaa0001", "aaaab001")
	if err != nil || !found || string(state.Lookup) != "{}" || state.Day != "2026-11-09" || state.Origin != "XAA" {
		t.Fatalf("state after a week = %+v %v %v", state, found, err)
	}
	if sent, _ := f.store.ListSentUpdates(f.ctx, "alice", "aaaa0001", 10); len(sent) != 1 || sent[0].Text != "" {
		t.Fatalf("sent log after a week = %+v", sent)
	}
}

// Each change is told once on the slot that finds it: a gate going B7, B12, B7, B12 gives three
// lines, then a possible cancellation, a return to on time, and a diversion.
func TestFlightTrackingTellsEachChangeOnce(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	f.addTrip("alice", "aaaa0001", departs)
	steps := []struct {
		at     time.Time
		change func(*fakeLeg)
	}{
		{departs.Add(-38 * time.Hour), func(leg *fakeLeg) { leg.gate = "B7" }},
		{departs.Add(-6 * time.Hour), func(leg *fakeLeg) { leg.gate = "B12" }},
		{departs.Add(-3 * time.Hour), func(leg *fakeLeg) { leg.gate, leg.revised = "B7", departs.Add(30*time.Minute) }},
		{departs.Add(-90 * time.Minute), func(leg *fakeLeg) { leg.gate, leg.status = "B12", "CanceledUncertain" }},
		{departs.Add(-45 * time.Minute), func(leg *fakeLeg) { leg.revised, leg.status = departs.Add(5*time.Minute), "Boarding" }},
		{departs.Add(-15 * time.Minute), nil},
		{departs.Add(30 * time.Minute), func(leg *fakeLeg) {
			leg.status, leg.runway, leg.divertTo = "Diverted", departs.Add(10*time.Minute), true
		}},
	}
	for _, step := range steps {
		if step.change != nil {
			f.provider.set(step.change)
		}
		f.at(step.at)
	}
	want := []string{
		"EX123: gate B12 (was B7), terminal 1.",
		"EX123 Example City → Other City: departure delayed 30 min, now 14:10 (was 13:40).\nEX123: gate B7 (was B12), terminal 1.",
		"EX123 may be cancelled. Check the airline's app.\nEX123: gate B12 (was B7), terminal 1.",
		"EX123 is back on time: departs 13:45.",
		"EX123 is diverted to Third City (XCC).",
	}
	if got := stripped(f.sender.flightTexts("alice")); !slices.Equal(got, want) {
		t.Fatalf("messages =\n%q\nwant\n%q", got, want)
	}
}

// A cancellation is told at once and ends the tracking of the leg.
func TestFlightTrackingStopsAfterACancellation(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	f.addTrip("alice", "aaaa0001", departs)
	f.at(departs.Add(-38 * time.Hour))
	f.provider.set(func(leg *fakeLeg) { leg.status = "Canceled" })
	f.walk(departs.Add(-6*time.Hour), arrives.Add(time.Hour), nil)
	if got := stripped(f.sender.flightTexts("alice")); !slices.Equal(got, []string{"EX123 is cancelled. Check the airline's app for rebooking."}) || f.provider.count() != 2 {
		t.Fatalf("messages %q, lookups %d", got, f.provider.count())
	}
}

// An outbound booking typed as its local time in the trip's zone, the destination's, is two hours
// off as an instant, and still matches its leg on the first lookup; one stored as a true instant
// matches too. Two users on one flight, stepped to the same slot, cost one lookup.
func TestFlightTrackingMatchesBookingsInEitherZoneAndSharesLookups(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	typed := time.Date(2026, 11, 9, 13, 40, 0, 0, athens(t))
	f.addTrip("alice", "aaaa0001", typed)
	f.addTrip("bob", "bbbb0001", departs)
	f.walk(departs.Add(-38*time.Hour), departs.Add(-6*time.Hour), nil)
	for _, owner := range []struct{ principal, trip, booking string }{{"alice", "aaaa0001", "aaaab001"}, {"bob", "bbbb0001", "bbbbb001"}} {
		state, found, err := f.store.LoadFlightState(f.ctx, owner.principal, owner.trip, owner.booking)
		if err != nil || !found || state.Day != "2026-11-09" || state.Origin != "XAA" || state.Destination != "XBB" {
			t.Fatalf("%s's state = %+v %v %v", owner.principal, state, found, err)
		}
	}
	// One match and one D−6h lookup, each shared by both trips.
	if requests := f.provider.requests(); len(requests) != 2 || requests[1] != "/flights/number/EX123/2026-11-09?Departure" {
		t.Fatalf("lookups = %v", requests)
	}
}

// Nothing is looked up for an owner whose Trips scan is off or who turned Flight status off.
func TestFlightTrackingNeedsTheScanAndTheSwitch(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	f.addTrip("alice", "aaaa0001", departs)
	f.addTrip("bob", "bbbb0001", departs)
	f.scan.off["alice"] = true
	if err := f.switches.SetEnabled(f.ctx, "bob", flights.Switch, false); err != nil {
		t.Fatal(err)
	}
	f.walk(departs.Add(-38*time.Hour), departs.Add(-37*time.Hour), nil)
	if f.provider.count() != 0 {
		t.Fatalf("lookups = %v", f.provider.requests())
	}
}

// A leg the provider has no live data for is looked up a day and three hours ahead only, and says
// so once.
func TestFlightTrackingLooksAScheduleOnlyLegUpTwice(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	f.provider.set(func(leg *fakeLeg) { leg.live = false })
	f.addTrip("alice", "aaaa0001", departs)
	f.walk(departs.Add(-38*time.Hour), arrives.Add(time.Hour), nil)
	if got := stripped(f.sender.flightTexts("alice")); f.provider.count() != 2 || !slices.Equal(got, []string{"EX123: no live status for this flight; check the airline's app."}) {
		t.Fatalf("lookups %d, messages %q", f.provider.count(), got)
	}
}

// Two failed slots in a row before departure give one line, and failures after it none.
func TestFlightTrackingSaysOnceWhenItCannotCheck(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	f.addTrip("alice", "aaaa0001", departs)
	f.at(departs.Add(-38 * time.Hour))
	f.provider.failing(true)
	f.walk(departs.Add(-6*time.Hour), departs.Add(45*time.Minute), nil)
	if got := stripped(f.sender.flightTexts("alice")); !slices.Equal(got, []string{"Flight status for EX123 could not be checked; check the airline's app."}) {
		t.Fatalf("messages = %q", got)
	}
}

// A spent allowance pauses every lookup until the period turns, tells each user with a flight in
// the window once, and the tracking resumes once it has turned.
func TestFlightTrackingPausesWhenTheAllowanceIsSpent(t *testing.T) {
	// The period turns at 00:00 UTC on the 9th, the day the flight leaves.
	f := newFlightFixture(t, 340, 9)
	start, _ := flights.NewBudget(340, 9).Period(departs.Add(-38 * time.Hour))
	if refused, err := f.store.ReserveFlightUnits(f.ctx, start.Format(time.DateOnly), 340, []flights.UnitLimit{{Scope: flights.ScopeAll, Limit: 340}}); err != nil || refused != "" {
		t.Fatal(refused, err)
	}
	f.addTrip("alice", "aaaa0001", departs)
	f.addTrip("bob", "bbbb0001", departs)
	turns := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	f.walk(departs.Add(-38*time.Hour), turns.Add(-5*time.Minute), nil)
	paused := "Flight tracking is paused until Mon 9 Nov: the household's free flight-status allowance for this month is used up. Check the airline's app."
	for _, principal := range []string{"alice", "bob"} {
		if got := stripped(f.sender.flightTexts(principal)); !slices.Equal(got, []string{paused}) {
			t.Fatalf("%s's messages = %q", principal, got)
		}
	}
	if f.provider.count() != 0 {
		t.Fatalf("lookups while paused = %v", f.provider.requests())
	}
	f.walk(turns, departs.Add(-3*time.Hour), nil)
	// The match, shared by both trips, then D−6h and D−3h.
	if f.provider.count() != 3 {
		t.Fatalf("lookups after the period turned = %v", f.provider.requests())
	}
	if got := f.sender.flightTexts("alice"); len(got) != 1 {
		t.Fatalf("alice's messages after the period turned = %q", got)
	}
}

// When the budget left cannot cover the slots still ahead, D−6h goes first, then D+30, then
// D−90; the match, D−3h, D−45, D−15, and A+20 are kept.
func TestFlightTrackingDropsSlotsWhenTheBudgetIsShort(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	start, _ := flights.NewBudget(340, 1).Period(departs)
	// 330 units are already spent, so 10 are left: five lookups for the eight slots.
	if refused, err := f.store.ReserveFlightUnits(f.ctx, start.Format(time.DateOnly), 330, []flights.UnitLimit{{Scope: flights.ScopeAll, Limit: 340}}); err != nil || refused != "" {
		t.Fatal(refused, err)
	}
	f.addTrip("alice", "aaaa0001", departs)
	f.walk(departs.Add(-38*time.Hour), arrives.Add(time.Hour), nil)
	if f.provider.count() != 5 {
		t.Fatalf("lookups = %d, want the match, D-3h, D-45, D-15, and A+20", f.provider.count())
	}
}

// A rewrite that hangs holds up no lookup, and shutdown during a lookup returns promptly.
func TestFlightTrackingIsNotHeldUpByARewriteOrAShutdown(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	f.addTrip("alice", "aaaa0001", departs)
	f.at(departs.Add(-38 * time.Hour))
	release := make(chan struct{})
	waiting := make(chan struct{})
	f.language.mu.Lock()
	f.language.hold, f.language.waiting = release, waiting
	f.language.mu.Unlock()
	f.provider.set(func(leg *fakeLeg) { leg.revised = departs.Add(45 * time.Minute) })
	arrived := make(chan struct{}, 10)
	f.provider.mu.Lock()
	f.provider.arrived = arrived
	f.provider.mu.Unlock()
	stopping, stop := context.WithCancel(f.ctx)
	f.clock.Set(departs.Add(-6 * time.Hour))
	f.loop.Cycle(stopping)
	<-arrived
	<-waiting
	// The message about the delay is stuck in its rewrite, its lookup stored, when the loop that
	// ran it is told to stop: it is still sent.
	stop()
	// The next slot's lookup runs all the same, and its own message waits behind the first.
	f.provider.set(func(leg *fakeLeg) { leg.revised = departs.Add(90 * time.Minute) })
	f.clock.Set(departs.Add(-3 * time.Hour))
	f.loop.Cycle(f.ctx)
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("no lookup ran while a rewrite hung")
	}
	close(release)
	f.loop.Wait()
	want := []string{
		"EX123 Example City \u2192 Other City: departure delayed 45 min, now 14:25 (was 13:40).",
		"EX123 Example City \u2192 Other City: departure delayed 90 min, now 15:10 (was 14:25).",
	}
	if got := stripped(f.sender.flightTexts("alice")); !slices.Equal(got, want) {
		t.Fatalf("messages after the rewrite was released = %q", got)
	}

	f.provider.mu.Lock()
	f.provider.hang = true
	f.provider.mu.Unlock()
	ctx, cancel := context.WithCancel(f.ctx)
	f.clock.Set(departs.Add(-90 * time.Minute))
	f.loop.Cycle(ctx)
	<-arrived
	cancel()
	done := make(chan struct{})
	go func() {
		f.loop.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited for the lookup")
	}
}

// A cancellation the match finds is told and kept in the sent log, as one a later slot finds is.
func TestFlightTrackingRecordsACancellationFoundAtTheMatch(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	f.provider.set(func(leg *fakeLeg) { leg.status = "Canceled" })
	f.addTrip("alice", "aaaa0001", departs)
	f.walk(departs.Add(-38*time.Hour), departs.Add(-37*time.Hour), nil)
	if got := stripped(f.sender.flightTexts("alice")); !slices.Equal(got, []string{"EX123 is cancelled. Check the airline's app for rebooking."}) {
		t.Fatalf("messages = %q", got)
	}
	sent, err := f.store.ListSentUpdates(f.ctx, "alice", "aaaa0001", 10)
	if err != nil || len(sent) != 1 || !strings.HasPrefix(sent[0].Reference, "EX123|2026-11-09|status|") {
		t.Fatalf("sent log = %+v %v", sent, err)
	}
}

// A lookup without a gate between two with one compares the gate with the one last told: the same
// gate says nothing, another one names the gate last told.
func TestFlightTrackingComparesAReturningGateWithTheLastOneTold(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	f.addTrip("alice", "aaaa0001", departs)
	f.at(departs.Add(-38 * time.Hour))
	for _, step := range []struct {
		at   time.Time
		gate string
	}{
		{departs.Add(-3 * time.Hour), "B7"},
		{departs.Add(-90 * time.Minute), ""},
		{departs.Add(-45 * time.Minute), "B7"},
		{departs.Add(-15 * time.Minute), "B9"},
	} {
		f.provider.set(func(leg *fakeLeg) { leg.gate = step.gate })
		f.at(step.at)
	}
	want := []string{"EX123: gate B7, terminal 1.", "EX123: gate B9 (was B7), terminal 1."}
	if got := stripped(f.sender.flightTexts("alice")); !slices.Equal(got, want) {
		t.Fatalf("messages = %q", got)
	}
}

// An expected arrival that keeps moving later costs at most one more lookup after A+20 min, and
// the leg ends by twelve hours after its scheduled arrival.
func TestFlightTrackingEndsALegWhoseArrivalKeepsMoving(t *testing.T) {
	f := newFlightFixture(t, 340, 1)
	f.addTrip("alice", "aaaa0001", departs)
	f.walk(departs.Add(-38*time.Hour), departs.Add(time.Hour), nil)
	before := f.provider.count()
	f.provider.set(func(leg *fakeLeg) { leg.status, leg.runway = "EnRoute", departs })
	f.walk(departs.Add(time.Hour+5*time.Minute), arrives.Add(14*time.Hour), func(at time.Time) {
		f.provider.set(func(leg *fakeLeg) { leg.arrival = at.Add(30 * time.Minute) })
	})
	if extra := f.provider.count() - before; extra < 1 || extra > 2 {
		t.Fatalf("lookups after departure = %d, want A+20 and at most one more", extra)
	}
	state, _, err := f.store.LoadFlightState(f.ctx, "alice", "aaaa0001", "aaaab001")
	if err != nil || state.EndedAt.IsZero() || state.EndedAt.After(arrives.Add(12*time.Hour+5*time.Minute)) {
		t.Fatalf("state = %+v %v", state, err)
	}
}

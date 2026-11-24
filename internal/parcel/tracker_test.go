package parcel_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

const (
	numberA = "3SABCD12345678"
	numberB = "3SABCD12345679"
	numberC = "3SABCD12345670"
)

var trackerZone = time.FixedZone("test", 3600)

type trackerClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *trackerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *trackerClock) Set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

type fakeSources struct {
	mu       sync.Mutex
	listed   []parcel.Parcel
	reads    []time.Time
	clock    *trackerClock
	failWith error
	block    bool
	assigned []string
	entered  chan struct{}
}

func (s *fakeSources) Assigned(string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.assigned)
}

func (s *fakeSources) assign(entries ...string) {
	s.mu.Lock()
	s.assigned = entries
	s.mu.Unlock()
}

func (s *fakeSources) setBlock(block bool) {
	s.mu.Lock()
	s.block = block
	s.mu.Unlock()
}

func (s *fakeSources) fail(err error) {
	s.mu.Lock()
	s.failWith = err
	s.mu.Unlock()
}

func (s *fakeSources) List(context.Context, string) ([]parcel.SourceInfo, error) { return nil, nil }

func (s *fakeSources) Read(ctx context.Context, _ string) (parcel.Reading, error) {
	s.mu.Lock()
	s.reads = append(s.reads, s.clock.Now())
	listed, failWith, block := slices.Clone(s.listed), s.failWith, s.block
	s.mu.Unlock()
	if block {
		signal(s.entered)
		<-ctx.Done()
		return parcel.Reading{}, ctx.Err()
	}
	return parcel.Reading{Parcels: listed}, failWith
}

func (s *fakeSources) set(listed ...parcel.Parcel) {
	s.mu.Lock()
	s.listed = listed
	s.mu.Unlock()
}

func (s *fakeSources) readTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reads)
}

func signal(entered chan struct{}) {
	if entered == nil {
		return
	}
	select {
	case entered <- struct{}{}:
	default:
	}
}

type fakeMail struct {
	mu      sync.Mutex
	runs    []time.Time
	active  int
	maximum int
	release chan struct{}
	entered chan struct{}
	onRun   func(now time.Time) (parcel.MailResult, error)
}

func (m *fakeMail) hold() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.release = make(chan struct{})
	return m.release
}

func (m *fakeMail) setOnRun(onRun func(now time.Time) (parcel.MailResult, error)) {
	m.mu.Lock()
	m.onRun = onRun
	m.mu.Unlock()
}

func (m *fakeMail) Run(ctx context.Context, _ string, now time.Time) (parcel.MailResult, error) {
	m.mu.Lock()
	m.runs = append(m.runs, now)
	m.active++
	m.maximum = max(m.maximum, m.active)
	release, onRun := m.release, m.onRun
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.active--
		m.mu.Unlock()
	}()
	if release != nil {
		signal(m.entered)
		select {
		case <-release:
		case <-ctx.Done():
			return parcel.MailResult{}, ctx.Err()
		}
	}
	if onRun != nil {
		return onRun(now)
	}
	return parcel.MailResult{}, nil
}

func (m *fakeMail) times() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.runs)
}

type cardEvent struct {
	kind   string
	id     string
	status parcel.Status
	mode   parcel.CardMode
	count  int
}

type fakeCards struct {
	mu       sync.Mutex
	events   []cardEvent
	next     int64
	gone     map[string]bool
	editFail error
	sendFail error
}

func (c *fakeCards) failSends(err error) {
	c.mu.Lock()
	c.sendFail = err
	c.mu.Unlock()
}

func (c *fakeCards) failEdits(err error) {
	c.mu.Lock()
	c.editFail = err
	c.mu.Unlock()
}

func (c *fakeCards) record(event cardEvent) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *fakeCards) SendCard(_ context.Context, found parcel.Parcel) (int64, parcel.CardKind, error) {
	c.mu.Lock()
	c.next++
	message, failure := 1000+c.next, c.sendFail
	c.mu.Unlock()
	c.record(cardEvent{kind: "send", id: found.ID, status: found.Status})
	if failure != nil {
		return 0, "", failure
	}
	return message, parcel.CardText, nil
}

func (c *fakeCards) EditCard(_ context.Context, found parcel.Parcel, mode parcel.CardMode) error {
	c.mu.Lock()
	gone, failure := c.gone[found.ID], c.editFail
	c.mu.Unlock()
	c.record(cardEvent{kind: "edit", id: found.ID, status: found.Status, mode: mode})
	if gone {
		return parcel.ErrCardGone
	}
	return failure
}

func (c *fakeCards) SendNotice(_ context.Context, found parcel.Parcel) error {
	c.record(cardEvent{kind: "notice", id: found.ID, status: found.Status})
	return nil
}

func (c *fakeCards) SendSummary(_ context.Context, _ string, count int) error {
	c.record(cardEvent{kind: "summary", count: count})
	return nil
}

func (c *fakeCards) list(kind string) []cardEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var found []cardEvent
	for _, event := range c.events {
		if event.kind == kind {
			found = append(found, event)
		}
	}
	return found
}

func (c *fakeCards) reset() {
	c.mu.Lock()
	c.events = nil
	c.mu.Unlock()
}

type fakeUntracker struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (u *fakeUntracker) Untrack(_ context.Context, _, carrier, number string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, carrier+"|"+number)
	return u.err
}

type fakeSwitch struct {
	mu sync.Mutex
	on bool
}

func (s *fakeSwitch) Enabled(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on, nil
}

func (s *fakeSwitch) set(on bool) {
	s.mu.Lock()
	s.on = on
	s.mu.Unlock()
}

type trackerHarness struct {
	t         *testing.T
	store     *sqlite.Store
	clock     *trackerClock
	sources   *fakeSources
	mail      *fakeMail
	cards     *fakeCards
	untracker *fakeUntracker
	enabled   *fakeSwitch
	loop      *parcel.Loop
	start     time.Time
	zone      *time.Location
}

func newTrackerHarness(t *testing.T, start time.Time, withMail bool) *trackerHarness {
	t.Helper()
	return newTrackerHarnessIn(t, start, withMail, trackerZone)
}

func newTrackerHarnessIn(t *testing.T, start time.Time, withMail bool, zone *time.Location) *trackerHarness {
	t.Helper()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	clock := &trackerClock{now: start}
	h := &trackerHarness{
		t: t, store: store, clock: clock, start: start, zone: zone,
		sources: &fakeSources{clock: clock}, cards: &fakeCards{gone: map[string]bool{}}, untracker: &fakeUntracker{}, enabled: &fakeSwitch{on: true},
	}
	if withMail {
		h.mail = &fakeMail{}
	}
	h.loop = h.newLoop()
	return h
}

// newLoop builds a loop over the same store and fakes, as after a restart.
func (h *trackerHarness) newLoop() *parcel.Loop {
	h.t.Helper()
	cfg := parcel.LoopConfig{
		Repository: h.store, Sources: h.sources, Untracker: h.untracker, Cards: h.cards, Enabled: h.enabled,
		Principals: func() []string { return []string{"alice"} }, Now: h.clock.Now, Location: h.zone,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if h.mail != nil {
		cfg.Mail = h.mail
	}
	loop, err := parcel.NewLoop(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.restart(loop)
}

func (h *trackerHarness) restart(loop *parcel.Loop) *parcel.Loop {
	h.loop = loop
	return loop
}

func (h *trackerHarness) cycle() {
	h.t.Helper()
	h.loop.Cycle(h.t.Context())
	h.loop.Wait()
}

// walk sets the clock to each slot from start to end and cycles.
func (h *trackerHarness) walk(from, to time.Time, step time.Duration) {
	h.t.Helper()
	for at := from; !at.After(to); at = at.Add(step) {
		h.clock.Set(at)
		h.cycle()
	}
}

func (h *trackerHarness) parcels() []parcel.Parcel {
	h.t.Helper()
	found, err := h.store.ListParcels(h.t.Context(), "alice")
	if err != nil {
		h.t.Fatal(err)
	}
	return found
}

func (h *trackerHarness) byNumber(number string) parcel.Parcel {
	h.t.Helper()
	for _, found := range h.parcels() {
		if found.Number == number {
			return found
		}
	}
	h.t.Fatalf("no parcel %s in %+v", number, h.parcels())
	return parcel.Parcel{}
}

func (h *trackerHarness) seed(number string, status parcel.Status) parcel.Parcel {
	h.t.Helper()
	stored, _, err := h.store.UpsertParcel(h.t.Context(), parcel.Parcel{
		PrincipalID: "alice", Carrier: "postnl", Number: number, Source: parcel.SourceHomeAssistant, SourceKey: "e1:" + number,
		Status: status, CreatedAt: h.start, UpdatedAt: h.start,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return stored
}

func listed(number string, status parcel.Status) parcel.Parcel {
	return parcel.Parcel{Carrier: "postnl", Number: number, SourceKey: "e1:" + number, Status: status, Title: "Example kettle"}
}

func times(from time.Time, offsets ...time.Duration) []time.Time {
	var found []time.Time
	for _, offset := range offsets {
		found = append(found, from.Add(offset))
	}
	return found
}

func sameTimes(t *testing.T, got, want []time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("times = %v, want %v", got, want)
	}
	for i := range got {
		if !got[i].Equal(want[i]) {
			t.Fatalf("times = %v, want %v", got, want)
		}
	}
}

func TestHomeAssistantIsReadEveryTenMinutes(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.walk(start, start.Add(time.Hour), time.Minute)
	var want []time.Duration
	for offset := time.Duration(0); offset <= time.Hour; offset += 10 * time.Minute {
		want = append(want, offset)
	}
	sameTimes(t, h.sources.readTimes(), times(start, want...))
}

func TestMailRunsEveryHalfHourByDayWithAnOpenParcelAndEveryThreeHoursOtherwise(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 6, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, true)
	h.seed(numberA, parcel.StatusInTransit)
	h.sources.set(listed(numberA, parcel.StatusInTransit))
	h.cycle()
	h.walk(start, start.Add(21*time.Hour), time.Minute)
	// 06:00 is night, so the first run is on the three hour rule; from 07:00 until 22:00 it is every
	// half hour; the day's last run is 21:30 and the next, at night, three hours after it.
	want := []time.Duration{0, time.Hour}
	for offset := 90 * time.Minute; offset <= 15*time.Hour+30*time.Minute; offset += 30 * time.Minute {
		want = append(want, offset)
	}
	want = append(want, 18*time.Hour+30*time.Minute)
	sameTimes(t, h.mail.times(), times(start, want...))
}

func TestMailRunsEveryThreeHoursWhenNothingIsOnItsWay(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, true)
	h.seed(numberA, parcel.StatusDelivered)
	h.cycle()
	h.walk(start, start.Add(15*time.Hour), time.Minute)
	sameTimes(t, h.mail.times(), times(start, 0, 3*time.Hour, 6*time.Hour, 9*time.Hour, 12*time.Hour, 15*time.Hour))
}

func TestMailRunsNeverOverlapForOnePrincipal(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, true)
	h.seed(numberA, parcel.StatusInTransit)
	h.cycle()
	release := h.mail.hold()
	for i := 1; i <= 6; i++ {
		h.clock.Set(start.Add(time.Duration(i) * 30 * time.Minute))
		h.loop.Cycle(t.Context())
	}
	close(release)
	h.loop.Wait()
	h.mail.mu.Lock()
	maximum := h.mail.maximum
	h.mail.mu.Unlock()
	if maximum != 1 || len(h.mail.times()) != 2 {
		t.Fatalf("overlap = %d, runs = %v", maximum, h.mail.times())
	}
}

// A slot whose mail run finds the previous one not started yet starts none: the run is claimed when
// the slot comes, not when its worker gets to run, which could be after the previous run ended.
func TestASlotThatFindsTheMailRunTakenStartsNoOther(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, true)
	h.seed(numberA, parcel.StatusInTransit)
	var held []func()
	parcel.SetSpawn(h.loop, func(work func()) { held = append(held, work) })
	drain := func() {
		for len(held) > 0 {
			work := held[0]
			held = held[1:]
			work()
		}
	}
	h.loop.Cycle(t.Context())
	drain()
	for slot := 1; slot <= 3; slot++ {
		h.clock.Set(start.Add(time.Duration(slot) * 30 * time.Minute))
		h.loop.Cycle(t.Context())
	}
	drain()
	if runs := h.mail.times(); len(runs) != 2 {
		t.Fatalf("mail runs = %v, want the baseline's and one more", runs)
	}
}

func TestBaselineIsSilentPostsOneSummaryAndSurvivesARestart(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	delivered := listed(numberC, parcel.StatusDelivered)
	delivered.DeliveredAt = start.Add(-time.Hour)
	h.sources.set(listed(numberA, parcel.StatusInTransit), listed(numberB, parcel.StatusOutForDelivery), delivered)
	h.cycle()

	sends, notices, summaries := h.cards.list("send"), h.cards.list("notice"), h.cards.list("summary")
	if len(sends) != 2 || len(notices) != 0 || len(summaries) != 1 || summaries[0].count != 2 {
		t.Fatalf("sends = %+v, notices = %+v, summaries = %+v", sends, notices, summaries)
	}
	if h.byNumber(numberC).CardMessage != 0 || h.byNumber(numberA).CardMessage == 0 {
		t.Fatalf("cards = %+v", h.parcels())
	}

	h.cards.reset()
	h.restart(h.newLoop())
	h.walk(start.Add(10*time.Minute), start.Add(30*time.Minute), 10*time.Minute)
	if len(h.cards.events) != 0 {
		t.Fatalf("a restart repeated %+v", h.cards.events)
	}
}

func TestBaselineWithNothingToTrackPostsNoSummary(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.cycle()
	if len(h.cards.events) != 0 {
		t.Fatalf("events = %+v", h.cards.events)
	}
}

func TestEachTransitionSendsOneNoticeAndARestartDoesNotRepeatIt(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.sources.set(listed(numberA, parcel.StatusInTransit), listed(numberB, parcel.StatusInTransit), listed(numberC, parcel.StatusInTransit))
	h.cycle()
	h.cards.reset()

	step := func(minutes int, items ...parcel.Parcel) {
		h.sources.set(items...)
		h.clock.Set(start.Add(time.Duration(minutes) * time.Minute))
		h.cycle()
	}
	step(10, listed(numberA, parcel.StatusOutForDelivery), listed(numberB, parcel.StatusInTransit), listed(numberC, parcel.StatusInTransit))
	step(20, listed(numberA, parcel.StatusOutForDelivery), listed(numberB, parcel.StatusAtPickupPoint), listed(numberC, parcel.StatusProblem))
	step(30, listed(numberA, parcel.StatusDelivered), listed(numberB, parcel.StatusAtPickupPoint), listed(numberC, parcel.StatusProblem))

	got := map[string][]parcel.Status{}
	for _, event := range h.cards.list("notice") {
		got[event.id] = append(got[event.id], event.status)
	}
	a, b, c := h.byNumber(numberA).ID, h.byNumber(numberB).ID, h.byNumber(numberC).ID
	if !slices.Equal(got[a], []parcel.Status{parcel.StatusOutForDelivery, parcel.StatusDelivered}) ||
		!slices.Equal(got[b], []parcel.Status{parcel.StatusAtPickupPoint}) || !slices.Equal(got[c], []parcel.Status{parcel.StatusProblem}) {
		t.Fatalf("notices = %v", got)
	}

	h.cards.reset()
	h.restart(h.newLoop())
	step(40, listed(numberA, parcel.StatusDelivered), listed(numberB, parcel.StatusAtPickupPoint), listed(numberC, parcel.StatusProblem))
	// A problem that comes back is not told twice.
	step(50, listed(numberA, parcel.StatusDelivered), listed(numberB, parcel.StatusAtPickupPoint), listed(numberC, parcel.StatusInTransit))
	step(60, listed(numberA, parcel.StatusDelivered), listed(numberB, parcel.StatusAtPickupPoint), listed(numberC, parcel.StatusProblem))
	if notices := h.cards.list("notice"); len(notices) != 0 {
		t.Fatalf("notices repeated: %+v", notices)
	}
}

func TestAWindowChangeOnlyEditsTheCard(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	item := listed(numberA, parcel.StatusInTransit)
	h.sources.set(item)
	h.cycle()
	h.cards.reset()
	item.WindowFrom, item.WindowTo = start.Add(2*time.Hour), start.Add(4*time.Hour)
	h.sources.set(item)
	h.clock.Set(start.Add(10 * time.Minute))
	h.cycle()
	if edits := h.cards.list("edit"); len(edits) != 1 || edits[0].mode != parcel.CardCurrent || len(h.cards.list("notice")) != 0 || len(h.cards.list("send")) != 0 {
		t.Fatalf("events = %+v", h.cards.events)
	}
	h.cards.reset()
	h.clock.Set(start.Add(20 * time.Minute))
	h.cycle()
	if len(h.cards.events) != 0 {
		t.Fatalf("an unchanged parcel was touched: %+v", h.cards.events)
	}
}

func TestAGoneCardIsSentAgain(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.sources.set(listed(numberA, parcel.StatusInTransit))
	h.cycle()
	first := h.byNumber(numberA)
	h.cards.reset()
	h.cards.gone[first.ID] = true
	h.sources.set(listed(numberA, parcel.StatusOutForDelivery))
	h.clock.Set(start.Add(10 * time.Minute))
	h.cycle()
	again := h.byNumber(numberA)
	if sends := h.cards.list("send"); len(sends) != 1 || again.CardMessage == first.CardMessage || again.CardMessage == 0 {
		t.Fatalf("sends = %+v, cards = %d then %d", sends, first.CardMessage, again.CardMessage)
	}
	if notices := h.cards.list("notice"); len(notices) != 1 {
		t.Fatalf("notices = %+v", notices)
	}
}

func TestAStoppedParcelStillListedNeverReturns(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.sources.set(listed(numberA, parcel.StatusInTransit))
	h.cycle()
	id := h.byNumber(numberA).ID
	if _, found, err := h.loop.Stop(t.Context(), "alice", id); err != nil || !found {
		t.Fatalf("stop = %v %v", found, err)
	}
	h.cards.reset()
	h.sources.set(listed(numberA, parcel.StatusOutForDelivery))
	h.walk(start.Add(10*time.Minute), start.Add(time.Hour), 10*time.Minute)
	if parcels := h.parcels(); len(parcels) != 1 || !parcels[0].Stopped() || len(h.cards.events) != 0 {
		t.Fatalf("parcels = %+v, events = %+v", parcels, h.cards.events)
	}
}

func TestRetentionDeletesAfterFourteenDaysAndTheParcelDoesNotReturn(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	item := listed(numberA, parcel.StatusDelivered)
	item.DeliveredAt = start
	h.sources.set(item)
	h.walk(start, start.Add(13*24*time.Hour), time.Hour)
	if len(h.parcels()) != 1 {
		t.Fatalf("deleted early: %+v", h.parcels())
	}
	h.walk(start.Add(13*24*time.Hour+time.Hour), start.Add(14*24*time.Hour+2*time.Hour), time.Hour)
	if len(h.parcels()) != 0 {
		t.Fatalf("not deleted: %+v", h.parcels())
	}
	h.walk(start.Add(14*24*time.Hour+3*time.Hour), start.Add(14*24*time.Hour+6*time.Hour), time.Hour)
	if len(h.parcels()) != 0 || len(h.cards.events) != 0 {
		t.Fatalf("returned: %+v %+v", h.parcels(), h.cards.events)
	}
}

func TestAParcelNoSourceUpdatedForThirtyDaysIsNoLongerFollowed(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, true)
	h.seed(numberA, parcel.StatusInTransit)
	h.sources.set(listed(numberB, parcel.StatusInTransit))
	h.cycle()
	h.cards.reset()

	gone := h.byNumber(numberA)
	idle := start.Add(parcel.IdleLifetime + time.Hour)
	h.clock.Set(idle)
	h.cycle()
	if found := h.parcels(); len(found) != 1 || found[0].Number != numberB || found[0].Stopped() {
		t.Fatalf("parcels = %+v", found)
	}
	if edits := h.cards.list("edit"); len(edits) != 1 || edits[0].id != gone.ID || edits[0].mode != parcel.CardIdle || len(h.cards.list("notice")) != 0 {
		t.Fatalf("events = %+v", h.cards.events)
	}
	if tombstoned, err := h.store.TombstoneSeenAny(t.Context(), "alice", gone.Keys(), idle); err != nil || tombstoned {
		t.Fatalf("an idle parcel was tombstoned: %v %v", tombstoned, err)
	}

	h.sources.set()
	h.clock.Set(idle.Add(parcel.IdleLifetime + time.Hour))
	h.cycle()
	runs := len(h.mail.times())
	h.walk(h.clock.Now().Add(10*time.Minute), h.clock.Now().Add(2*time.Hour), 10*time.Minute)
	if len(h.mail.times()) != runs {
		t.Fatalf("mail still runs every half hour for parcels no longer followed: %v", h.mail.times())
	}
}

func TestStopUntracksBestEffortAndEditsTheCard(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.sources.set(listed(numberA, parcel.StatusInTransit), listed(numberB, parcel.StatusInTransit))
	h.cycle()
	if err := h.store.SaveAddedNumber(t.Context(), parcel.AddedNumber{PrincipalID: "alice", Carrier: "postnl", Number: numberA, Domain: "example", EntryID: "e1", AddedAt: start}); err != nil {
		t.Fatal(err)
	}
	h.cards.reset()
	h.untracker.err = errors.New("Home Assistant is down")

	stopped, found, err := h.loop.Stop(t.Context(), "alice", h.byNumber(numberA).ID)
	if err != nil || !found || !stopped.Stopped() {
		t.Fatalf("stop = %+v %v %v", stopped, found, err)
	}
	if !slices.Equal(h.untracker.calls, []string{"postnl|" + numberA}) {
		t.Fatalf("untrack calls = %v", h.untracker.calls)
	}
	if edits := h.cards.list("edit"); len(edits) != 1 || edits[0].mode != parcel.CardStopped {
		t.Fatalf("edits = %+v", edits)
	}

	h.untracker.calls = nil
	if _, found, err := h.loop.Stop(t.Context(), "alice", h.byNumber(numberB).ID); err != nil || !found || len(h.untracker.calls) != 0 {
		t.Fatalf("a number nobody added was untracked: %v %v %v", found, err, h.untracker.calls)
	}
	if _, found, err := h.loop.Stop(t.Context(), "alice", "missing"); err != nil || found {
		t.Fatalf("unknown = %v %v", found, err)
	}
}

func TestParcelsOffStopsEverythingAndOnEditsTheCardsBack(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, true)
	delivered := listed(numberC, parcel.StatusDelivered)
	delivered.DeliveredAt = start
	h.sources.set(listed(numberA, parcel.StatusInTransit), listed(numberB, parcel.StatusOutForDelivery), delivered)
	h.cycle()
	if len(h.cards.list("send")) != 2 {
		t.Fatalf("events = %+v", h.cards.events)
	}
	h.cards.reset()

	h.enabled.set(false)
	begin := time.Now()
	if err := h.loop.Switched(t.Context(), "alice", false); err != nil || time.Since(begin) > time.Second || len(h.cards.events) != 0 {
		t.Fatalf("switched = %v after %v with %+v", err, time.Since(begin), h.cards.events)
	}
	h.cycle()
	if off := h.cards.list("edit"); len(off) != 2 || off[0].mode != parcel.CardTrackingOff || off[1].mode != parcel.CardTrackingOff {
		t.Fatalf("off edits = %+v", h.cards.events)
	}
	reads, runs := len(h.sources.readTimes()), len(h.mail.times())
	h.cards.reset()
	h.sources.set(listed(numberA, parcel.StatusOutForDelivery), listed(numberB, parcel.StatusOutForDelivery), delivered)
	h.walk(start.Add(10*time.Minute), start.Add(6*time.Hour), 10*time.Minute)
	if len(h.sources.readTimes()) != reads || len(h.mail.times()) != runs || len(h.cards.events) != 0 {
		t.Fatalf("worked while off: reads %d->%d, runs %d->%d, events %+v", reads, len(h.sources.readTimes()), runs, len(h.mail.times()), h.cards.events)
	}

	h.enabled.set(true)
	if err := h.loop.Switched(t.Context(), "alice", true); err != nil {
		t.Fatal(err)
	}
	h.clock.Set(start.Add(6*time.Hour + 10*time.Minute))
	h.cycle()
	edits, summaries := h.cards.list("edit"), h.cards.list("summary")
	if len(h.cards.list("send")) != 0 || len(h.cards.list("notice")) != 0 || len(summaries) != 1 || summaries[0].count != 2 {
		t.Fatalf("on again = %+v", h.cards.events)
	}
	for _, edit := range edits {
		if edit.mode != parcel.CardCurrent {
			t.Fatalf("edit = %+v", edit)
		}
	}
	if len(edits) != 2 {
		t.Fatalf("edits = %+v", edits)
	}
}

func TestRunStopsPromptlyWhileASourceAndMailAreBusy(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, true)
	h.seed(numberA, parcel.StatusInTransit)
	h.cycle()
	h.sources.entered, h.mail.entered = make(chan struct{}, 1), make(chan struct{}, 1)
	h.sources.setBlock(true)
	h.mail.hold()
	h.clock.Set(start.Add(31 * time.Minute))
	loop, err := parcel.NewLoop(parcel.LoopConfig{
		Repository: h.store, Sources: h.sources, Mail: h.mail, Cards: h.cards, Enabled: h.enabled,
		Principals: func() []string { return []string{"alice"} }, Now: h.clock.Now, Location: trackerZone, Tick: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()
	for _, entered := range []chan struct{}{h.sources.entered, h.mail.entered} {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the loop never started its work")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

package trip_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/trip"
)

type sentNotice struct {
	principal string
	text      string
}

type recordingSender struct {
	mu   sync.Mutex
	sent []sentNotice
	fail bool
}

func (s *recordingSender) Send(_ context.Context, principalID, _, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("chat unavailable")
	}
	s.sent = append(s.sent, sentNotice{principalID, text})
	return nil
}

func (s *recordingSender) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var texts []string
	for _, notice := range s.sent {
		texts = append(texts, notice.text)
	}
	return texts
}

type fakePaused []string

func (p fakePaused) TravelPausedNames(context.Context, string) ([]string, error) {
	return p, nil
}

func (f *fixture) loop(sender trip.Sender, paused ...trip.Paused) *trip.Loop {
	f.t.Helper()
	loop, err := trip.NewLoop(trip.LoopConfig{Now: f.clock.Now}, f.trips, paused, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		f.t.Fatal(err)
	}
	return loop
}

func TestLoopSendsEachNoticeOnceAcrossRestarts(t *testing.T) {
	f := newFixture(t)
	sender := &recordingSender{}
	f.add(draft(1, 3))
	paused := []trip.Paused{fakePaused{"Daily digest", "Rain watch"}, fakePaused{"Rain watch", "Bin day"}}

	loop := f.loop(sender, paused...)
	loop.Cycle(t.Context())
	if got := sender.texts(); len(got) != 0 {
		t.Fatalf("before the trip: %q", got)
	}

	f.clock.Set(days(1).Add(time.Minute))
	loop.Cycle(t.Context())
	loop.Cycle(t.Context())
	restarted := f.loop(sender, paused...)
	restarted.Cycle(t.Context())
	want := "Trip Example trip to Example City has started (until Sun 2026-10-18 13:00). Paused while traveling: Bin day, Daily digest, Rain watch."
	if got := sender.texts(); len(got) != 1 || got[0] != want {
		t.Fatalf("start notices = %q, want [%q]", got, want)
	}

	f.clock.Set(days(3))
	restarted.Cycle(t.Context())
	f.loop(sender, paused...).Cycle(t.Context())
	got := sender.texts()
	if len(got) != 2 || got[1] != "Trip Example trip has ended. Resumed: Bin day, Daily digest, Rain watch." {
		t.Fatalf("notices = %q", got)
	}
	if sender.sent[1].principal != "alice" {
		t.Fatalf("sent to %q", sender.sent[1].principal)
	}
}

func TestLoopSaysNothingIsPausedAndLimitsNames(t *testing.T) {
	f := newFixture(t)
	f.add(draft(0, 2))
	sender := &recordingSender{}
	f.loop(sender).Cycle(t.Context())
	if got := sender.texts(); len(got) != 1 || !strings.HasSuffix(got[0], "Nothing is paused.") {
		t.Fatalf("notices = %q", got)
	}

	f.clock.Set(days(2))
	var many fakePaused
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		many = append(many, name)
	}
	f.loop(sender, many).Cycle(t.Context())
	if got := sender.texts(); len(got) != 2 || got[1] != "Trip Example trip has ended. Resumed: a, b, c, d, e, f, g, h, i, j and 2 more." {
		t.Fatalf("notices = %q", got)
	}
}

func TestLoopCancelledActiveTripSendsEndNotice(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(0, 5))
	sender := &recordingSender{}
	loop := f.loop(sender, fakePaused{"Daily digest"})
	loop.Cycle(t.Context())

	if result, err := f.trips.Cancel(t.Context(), "alice", planned.ID, time.Time{}); err != nil || result.Rejection != "" {
		t.Fatalf("cancel: %v %q", err, result.Rejection)
	}
	loop.Cycle(t.Context())
	loop.Cycle(t.Context())
	got := sender.texts()
	if len(got) != 2 || got[1] != "Trip Example trip was cancelled; resumed: Daily digest." {
		t.Fatalf("notices = %q", got)
	}
}

func TestLoopCancelledTripThatNeverStartedSendsNothing(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(2, 5))
	sender := &recordingSender{}
	loop := f.loop(sender)
	if _, err := f.trips.Cancel(t.Context(), "alice", planned.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	loop.Cycle(t.Context())
	f.clock.Set(days(6))
	loop.Cycle(t.Context())
	if got := sender.texts(); len(got) != 0 {
		t.Fatalf("notices = %q", got)
	}
}

func TestLoopRetriesAFailedSendOnTheNextCycle(t *testing.T) {
	f := newFixture(t)
	f.add(draft(0, 5))
	sender := &recordingSender{fail: true}
	loop := f.loop(sender)
	loop.Cycle(t.Context())
	if got := sender.texts(); len(got) != 0 {
		t.Fatalf("notices = %q", got)
	}
	sender.mu.Lock()
	sender.fail = false
	sender.mu.Unlock()
	loop.Cycle(t.Context())
	loop.Cycle(t.Context())
	if got := sender.texts(); len(got) != 1 {
		t.Fatalf("notices = %q, want one", got)
	}
}

func TestLoopRunsFromStart(t *testing.T) {
	f := newFixture(t)
	f.add(draft(0, 5))
	sender := &recordingSender{}
	loop, err := trip.NewLoop(trip.LoopConfig{Interval: 10 * time.Millisecond, Now: f.clock.Now}, f.trips, nil, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := loop.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(sender.texts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := loop.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := sender.texts(); len(got) != 1 {
		t.Fatalf("notices = %q, want one", got)
	}
}

func TestEndNoticeDoesNotSayResumedWhileAnotherTripIsUnderWay(t *testing.T) {
	f := newFixture(t)
	first := f.add(draft(-2, 1))
	second := f.add(draft(-1, 6))
	second.Title = "Second trip"
	sender := &recordingSender{}
	loop := f.loop(sender, fakePaused{"Water plants"})
	loop.Cycle(t.Context())
	f.clock.Set(days(2))
	loop.Cycle(t.Context())
	for _, id := range []string{first.ID, second.ID} {
		if _, found, _ := f.trips.Trip(t.Context(), "alice", id); !found {
			t.Fatal("trip missing")
		}
	}
	var ended string
	for _, text := range sender.texts() {
		if strings.Contains(text, "has ended") {
			ended = text
		}
	}
	if ended == "" || strings.Contains(ended, "Resumed") || !strings.Contains(ended, "Still traveling") {
		t.Fatalf("notices = %q", sender.texts())
	}
}

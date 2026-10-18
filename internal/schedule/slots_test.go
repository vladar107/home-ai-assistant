package schedule_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

// heldTurn answers a model turn at once, except for the held principal, whose turns wait as they
// would behind that principal's chat turn until released.
type heldTurn struct {
	held    string
	release chan struct{}
	mu      sync.Mutex
	started map[string]int
}

func (*heldTurn) Validate(json.RawMessage) error { return nil }

func (h *heldTurn) Run(ctx context.Context, occurrence schedule.Occurrence) (schedule.Result, error) {
	h.mu.Lock()
	h.started[occurrence.PrincipalID]++
	h.mu.Unlock()
	if occurrence.PrincipalID == h.held {
		select {
		case <-h.release:
		case <-ctx.Done():
			return schedule.Result{}, ctx.Err()
		}
	}
	return schedule.Result{Text: "report"}, nil
}

func (h *heldTurn) count(principalID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started[principalID]
}

func TestHeldModelTurnsLeaveRoomForOthersAndReminders(t *testing.T) {
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	now := utc(5, 7, 0)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	service, err := schedule.NewService(schedule.Config{Zone: "UTC", TickInterval: time.Hour, Now: clock},
		store, store, discardingDeliverer{}, &recordingProposer{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	turns := &heldTurn{held: "alice", release: make(chan struct{}), started: make(map[string]int)}
	released := false
	t.Cleanup(func() {
		if !released {
			close(turns.release)
		}
	})
	if err := service.Register(schedule.JobType{Name: schedule.TypeModelTurn, Handler: turns, CatchUp: 6 * time.Hour, MinimumInterval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := service.Register(schedule.Reminder(clock)); err != nil {
		t.Fatal(err)
	}
	create := func(principalID, id, name, jobType string, timing schedule.Timing, config json.RawMessage) {
		t.Helper()
		prepared, err := service.Prepare(principalID, name, jobType, timing, config)
		if err != nil {
			t.Fatal(err)
		}
		if created, err := service.Create(t.Context(), id, prepared); err != nil || !created {
			t.Fatalf("create %s: %v %v", name, created, err)
		}
	}
	prompt, _ := schedule.ModelTurnConfig("Tell me about the day.")
	create("alice", "a000000000000001", "Morning digest", schedule.TypeModelTurn, schedule.Timing{Cron: "0 8 * * *"}, prompt)
	create("alice", "a000000000000002", "Morning news", schedule.TypeModelTurn, schedule.Timing{Cron: "0 8 * * *"}, prompt)
	create("bob", "b000000000000001", "Morning digest", schedule.TypeModelTurn, schedule.Timing{Cron: "0 8 * * *"}, prompt)
	reminder, _ := schedule.ReminderConfig("water the plants")
	create("bob", "b000000000000002", "water the plants", schedule.TypeReminder, schedule.Timing{At: "2026-10-05 08:00"}, reminder)
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	delivered := func(jobIDs ...string) bool {
		runs, err := store.ScheduledRuns(t.Context(), schedule.StatusDelivered)
		if err != nil {
			t.Fatal(err)
		}
		found := make(map[string]bool)
		for _, run := range runs {
			found[run.JobID] = true
		}
		for _, id := range jobIDs {
			if !found[id] {
				return false
			}
		}
		return true
	}
	waitDelivered := func(jobIDs ...string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !delivered(jobIDs...) {
			if time.Now().After(deadline) {
				t.Fatalf("runs of %v were not delivered", jobIDs)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	mu.Lock()
	now = utc(5, 8, 0).Add(10 * time.Second)
	mu.Unlock()
	service.Wake()
	waitDelivered("b000000000000001", "b000000000000002")
	for deadline := time.Now().Add(5 * time.Second); turns.count("alice") == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	service.Wake()
	time.Sleep(50 * time.Millisecond)
	if got := turns.count("alice"); got != 1 {
		t.Fatalf("alice's held turns took %d slots, want 1", got)
	}

	// No tick is due for an hour, so the second turn starts only because the first one's slot
	// freed.
	close(turns.release)
	released = true
	waitDelivered("a000000000000001", "a000000000000002")
}

package schedule_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

// runRecorder is a job handler and a chat that keep whose runs they saw.
type runRecorder struct {
	mu    sync.Mutex
	ran   []string
	chats []int64
}

func (r *runRecorder) Validate(json.RawMessage) error { return nil }
func (r *runRecorder) Run(_ context.Context, occurrence schedule.Occurrence) (schedule.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = append(r.ran, occurrence.PrincipalID)
	return schedule.Result{Text: "Good morning."}, nil
}
func (r *runRecorder) SendReply(context.Context, int64, int64, string) error { return nil }
func (r *runRecorder) SendTrackedReply(_ context.Context, chatID, _ int64, _ string) (int64, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chats = append(r.chats, chatID)
	return int64(len(r.chats)), 0, nil
}
func (r *runRecorder) SendTyping(context.Context, int64) error { return nil }
func (r *runRecorder) SendMessageDraft(context.Context, int64, int64, string, bool) error {
	return nil
}

// A user taken out of the users file gets no scheduled run after the next start: the run is
// skipped before its turn, so neither their chat nor their subscription is used.
func TestScheduledRunSkipsAUserRemovedFromTheUsersFile(t *testing.T) {
	ctx := t.Context()
	alice := identity.ConfiguredPrincipal{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}
	bob := identity.ConfiguredPrincipal{ID: "bob", DisplayName: "Bob", TelegramUserID: 202}
	path := filepath.Join(t.TempDir(), "state", "assistant.sqlite")
	var mu sync.Mutex
	now := utc(5, 8, 0)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	recorder := &runRecorder{}
	newService := func(store *sqlite.Store) *schedule.Service {
		service, err := schedule.NewService(
			schedule.Config{Zone: "UTC", TickInterval: time.Hour, Now: clock},
			store, store, recorder, &recordingProposer{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Register(schedule.JobType{Name: schedule.TypeModelTurn, Handler: recorder, CatchUp: time.Hour, MinimumInterval: time.Hour}); err != nil {
			t.Fatal(err)
		}
		return service
	}

	store := testutil.OpenStore(t, path)
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{alice, bob}); err != nil {
		t.Fatal(err)
	}
	before := newService(store)
	config, err := schedule.ModelTurnConfig("Tell me about the day.")
	if err != nil {
		t.Fatal(err)
	}
	for id, principalID := range map[string]string{"00000000000000a1": "alice", "00000000000000b1": "bob"} {
		prepared, err := before.Prepare(principalID, "Morning", schedule.TypeModelTurn, schedule.Timing{Cron: "0 9 * * *"}, config)
		if err != nil {
			t.Fatal(err)
		}
		if created, err := before.Create(ctx, id, prepared); err != nil || !created {
			t.Fatalf("create %s's job: %v %v", principalID, created, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{alice}); err != nil {
		t.Fatal(err)
	}
	service := newService(store)
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	db := testutil.OpenDB(t, path)
	finished := func() map[string]string {
		rows, err := db.QueryContext(ctx,
			`SELECT principal_id, status || ':' || coalesce(error_kind, '') FROM schedule_runs WHERE finished_at IS NOT NULL`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		outcomes := map[string]string{}
		for rows.Next() {
			var principalID, outcome string
			if err := rows.Scan(&principalID, &outcome); err != nil {
				t.Fatal(err)
			}
			outcomes[principalID] = outcome
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return outcomes
	}
	mu.Lock()
	now = utc(5, 9, 1)
	mu.Unlock()
	service.Wake()
	deadline := time.Now().Add(5 * time.Second)
	for len(finished()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if outcomes := finished(); outcomes["alice"] != "delivered:" || outcomes["bob"] != "skipped:no_chat" {
		t.Fatalf("run outcomes = %v", outcomes)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if !slices.Equal(recorder.ran, []string{"alice"}) || !slices.Equal(recorder.chats, []int64{101}) {
		t.Fatalf("turns ran for %v and reports went to %v", recorder.ran, recorder.chats)
	}
}

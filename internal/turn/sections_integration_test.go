package turn_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/turn"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

type sectionsRuntime struct {
	mu       sync.Mutex
	requests []agentruntime.RunRequest
}

func (r *sectionsRuntime) Run(_ context.Context, request agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
	return agentruntime.RunResult{Response: "done", Final: "done"}, nil
}

func (r *sectionsRuntime) last() agentruntime.RunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[len(r.requests)-1]
}

type connected struct{}

func (connected) Ready(context.Context, string) (bool, error) { return true, nil }

type noTools struct{}

func (noTools) ToolsFor(context.Context, string) (agentruntime.ToolSet, error) {
	return agentruntime.ToolSet{}, nil
}

func (noTools) ReadOnlyToolsFor(context.Context, string) (agentruntime.ToolSet, error) {
	return agentruntime.ToolSet{}, nil
}

type plainTelemetry struct{}

func (plainTelemetry) ContextLogger(_ context.Context, logger *slog.Logger) *slog.Logger {
	return logger
}

// unreadableRoutes is the store with its route list failing, as a locked or damaged table would.
type unreadableRoutes struct{ routes.Repository }

func (unreadableRoutes) ListRoutes(context.Context, string) ([]routes.Route, error) {
	return nil, errors.New("routes table is unreadable")
}

// A section whose read fails is left out and logged while the others stay and the turn answers;
// a user with no routes or watches gets no section, and an agent watch check gets none either.
func TestAFailingSectionReadLeavesOnlyThatSectionOut(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "sam", DisplayName: "Sam", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 6, 0, 0, 0, time.UTC)
	route := routes.Route{ID: "00000000000000c1", PrincipalID: "alice", CreatedAt: now, UpdatedAt: now, Spec: routes.Spec{
		Name: "work", Days: []string{"mon"}, Legs: []routes.Leg{{Mode: routes.ModeTrain, From: "Eastbrook", To: "Westfield"}},
	}}
	if created, err := store.CreateRoute(ctx, route, routes.MaximumRoutesPerPrincipal); err != nil || !created {
		t.Fatalf("create route = %v %v", created, err)
	}
	departure := watch.Watch{ID: "00000000000000a1", PrincipalID: "alice", Enabled: true, CreatedAt: now, UpdatedAt: now, Spec: watch.Spec{
		Kind: watch.KindDeparture, Target: watch.Target{Mode: watch.ModeTrain, From: "Eastbrook", To: "Westfield"},
		Triggers: []string{watch.TriggerCancelled}, Days: []string{"mon"}, Start: "07:30", End: "08:12", Zone: "Europe/Amsterdam",
	}}
	if created, err := store.CreateWatch(ctx, departure, watch.MaximumWatchesPerPrincipal); err != nil || !created {
		t.Fatalf("create watch = %v %v", created, err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	sections := []turn.Section{routes.NewPromptSection(unreadableRoutes{store}, logger), watch.NewPromptSection(store, logger)}
	runtime := &sectionsRuntime{}
	engine, err := turn.New(turn.Config{TurnTimeout: time.Minute, Sections: sections, ScheduledSections: sections}, runtime, connected{}, noTools{}, logger, plainTelemetry{})
	if err != nil {
		t.Fatal(err)
	}

	result, err := engine.Run(ctx, turn.Run{PrincipalID: "alice", SessionID: "11111111-1111-4111-8111-111111111111", Message: "how do I get to work?"})
	if err != nil || result.Final != "done" {
		t.Fatalf("chat turn = %+v %v", result, err)
	}
	chat := runtime.last().Jobs
	if !strings.HasPrefix(chat, "Watches: the user's enabled watches") || !strings.Contains(chat, "id 00000000000000a1: ") || strings.Contains(chat, "Saved routes") {
		t.Fatalf("chat sections = %q", chat)
	}
	if !strings.Contains(logs.String(), "routes.prompt.failed") {
		t.Fatalf("the failed read was not logged: %s", logs.String())
	}

	if reply, err := engine.RunScheduled(ctx, "alice", "schedule/aaaa", "check the commute", agentruntime.ModelEffort{}); err != nil || reply.Text != "done" {
		t.Fatalf("scheduled turn = %+v %v", reply, err)
	}
	if scheduled := runtime.last().Jobs; scheduled != chat {
		t.Fatalf("scheduled sections = %q, want %q", scheduled, chat)
	}

	if reply, err := engine.RunCheck(ctx, "alice", "Check the line."); err != nil || reply.Text != "done" {
		t.Fatalf("check = %+v %v", reply, err)
	}
	if check := runtime.last().Jobs; check != "" {
		t.Fatalf("an agent watch check got sections: %q", check)
	}

	if _, err := engine.Run(ctx, turn.Run{PrincipalID: "sam", SessionID: "22222222-2222-4222-8222-222222222222", Message: "hello"}); err != nil {
		t.Fatal(err)
	}
	if empty := runtime.last().Jobs; empty != "" {
		t.Fatalf("a user with no routes or watches got sections: %q", empty)
	}
}

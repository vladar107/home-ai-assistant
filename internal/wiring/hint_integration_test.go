package wiring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

const routesSentence = "`routes` is the user's saved routes"

type optedIn struct{}

func (optedIn) JudgmentsEnabled(context.Context, string) (bool, error) { return true, nil }

// unreadableRoutes is the store with its route list failing, as a locked or damaged table would.
type unreadableRoutes struct{ routes.Repository }

func (unreadableRoutes) ListRoutes(context.Context, string) ([]routes.Route, error) {
	return nil, errors.New("routes table is unreadable")
}

type hintRequest struct {
	State     map[string]string `json:"state"`
	Questions map[string]struct {
		Instructions string `json:"instructions"`
	} `json:"questions"`
}

type hintCapture struct {
	body    string
	request hintRequest
	logs    string
}

// routeStore is a store where alice has two routes and sam none.
func routeStore(t *testing.T) *sqlite.Store {
	t.Helper()
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
	for _, route := range []routes.Route{
		{ID: "00000000000000c1", PrincipalID: "alice", CreatedAt: now, UpdatedAt: now, Spec: routes.Spec{
			Name: "To the office", Days: []string{"mon", "tue", "wed", "thu", "fri"}, ArriveBy: "09:00",
			Legs: []routes.Leg{{Mode: routes.ModeWalk, Minutes: 7}, {Mode: routes.ModeTrain, From: "Eastbrook", To: "Westfield", Via: "Millbank", Departures: []string{"07:42"}}},
		}},
		{ID: "00000000000000c2", PrincipalID: "alice", CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute), Spec: routes.Spec{
			Name: "Quiz night", Days: []string{"mon"},
			Legs: []routes.Leg{{Mode: routes.ModeBus, Stop: "Northgate Market", Line: "N47", Towards: "Riverside", Departures: []string{"19:05"}}},
		}},
	} {
		if created, err := store.CreateRoute(ctx, route, routes.MaximumRoutesPerPrincipal); err != nil || !created {
			t.Fatalf("create route = %v %v", created, err)
		}
	}
	return store
}

// askHint runs one tool hint for the principal against a fake TypeSafe and returns what it sent.
func askHint(t *testing.T, repository routes.Repository, principalID string, offered []string) hintCapture {
	t.Helper()
	var (
		mu       sync.Mutex
		captured hintCapture
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured.body = string(body)
		if err := json.Unmarshal(body, &captured.request); err != nil {
			t.Errorf("request is not JSON: %s", body)
		}
		mu.Unlock()
		_, _ = io.WriteString(w, `{"model":"jev-1.13.0","answers":{"tool":{"type":"choice","choice":"`+offered[0]+`","confidence":0.9}}}`)
	}))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	client, err := judgment.NewClient(judgment.Config{APIKey: "k", Endpoint: server.URL, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	definitions := make([]agentruntime.ToolDefinition, 0, len(offered))
	for _, name := range offered {
		definitions = append(definitions, agentruntime.ToolDefinition{Name: name, Description: "does " + name})
	}
	name, answered, err := judgment.NewToolHinter(client, optedIn{}, hintRoutes{repository: repository}).
		Hint(t.Context(), principalID, "How do I get to the office on Monday?", nil, definitions)
	if err != nil || !answered || name != offered[0] {
		t.Fatalf("hint = %q %v %v", name, answered, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if captured.body == "" {
		t.Fatal("no hint request was sent")
	}
	captured.logs = logs.String()
	return captured
}

// With route_check offered, the hint sees each of the user's routes by name and weekdays, and
// nothing of their legs, nor another user's routes.
func TestTheToolHintSeesSavedRouteNamesAndWeekdaysOnly(t *testing.T) {
	t.Parallel()
	got := askHint(t, routeStore(t), "alice", []string{"route_check", "ns_trips", "bus_trip"})

	if want := "To the office (Mon–Fri); Quiz night (Mon)"; got.request.State["routes"] != want {
		t.Errorf("routes state = %q, want %q", got.request.State["routes"], want)
	}
	if !strings.Contains(got.request.Questions["tool"].Instructions, routesSentence+" with their weekdays: a message about travelling where or when one of them goes needs route_check.") {
		t.Errorf("instructions = %q", got.request.Questions["tool"].Instructions)
	}
	for _, leaked := range []string{"Eastbrook", "Westfield", "Millbank", "Northgate", "Market", "N47", "Riverside", "07:42", "19:05", "09:00", "walk"} {
		if strings.Contains(got.body, leaked) {
			t.Errorf("request leaks %q: %s", leaked, got.body)
		}
	}
}

// Without saved routes, or without route_check among the offered tools, the hint has no routes
// state and no routes sentence.
func TestTheToolHintLeavesRoutesOutWithoutRoutesOrRouteCheck(t *testing.T) {
	t.Parallel()
	store := routeStore(t)
	for _, tc := range []struct {
		name      string
		principal string
		offered   []string
	}{
		{"no routes", "sam", []string{"route_check", "ns_trips"}},
		{"route_check not offered", "alice", []string{"ns_trips", "bus_trip"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := askHint(t, store, tc.principal, tc.offered)
			if routes, ok := got.request.State["routes"]; ok {
				t.Errorf("routes state = %q", routes)
			}
			if strings.Contains(got.request.Questions["tool"].Instructions, routesSentence) {
				t.Errorf("instructions = %q", got.request.Questions["tool"].Instructions)
			}
			if strings.Contains(got.body, "Quiz night") {
				t.Errorf("request names a route: %s", got.body)
			}
		})
	}
}

// A failing route read is logged and the hint is still asked, without routes.
func TestAFailingRouteReadStillSendsTheHint(t *testing.T) {
	t.Parallel()
	got := askHint(t, unreadableRoutes{routeStore(t)}, "alice", []string{"route_check", "ns_trips"})

	if routes, ok := got.request.State["routes"]; ok {
		t.Errorf("routes state = %q", routes)
	}
	if strings.Contains(got.request.Questions["tool"].Instructions, routesSentence) {
		t.Errorf("instructions = %q", got.request.Questions["tool"].Instructions)
	}
	if got.request.State["message"] == "" {
		t.Errorf("state = %v", got.request.State)
	}
	if !strings.Contains(got.logs, "judgment.hint.routes_failed") {
		t.Errorf("logs = %q", got.logs)
	}
}

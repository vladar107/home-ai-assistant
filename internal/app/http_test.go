package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

type readyChecker struct{}

func (readyChecker) Ready(context.Context) error { return nil }

type apiItem struct {
	ID          string `json:"id"`
	PrincipalID string `json:"principal_id"`
	Title       string `json:"title"`
	Details     string `json:"details"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func newBacklogAPI(t *testing.T) (*httptest.Server, *sqlite.Store) {
	t.Helper()
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := backlog.NewService(backlog.Config{}, store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpHandler(readyChecker{}, observability.Noop(), service))
	t.Cleanup(server.Close)
	return server, store
}

func seedBacklogItem(t *testing.T, store *sqlite.Store, principalID, id, title, status string, created time.Time) {
	t.Helper()
	item := backlog.Item{ID: id, PrincipalID: principalID, Title: title, Details: "Details of " + title, Status: status, CreatedAt: created, UpdatedAt: created}
	if err := store.AddBacklogItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, method, url string, body any) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%s %s content type = %q", method, url, response.Header.Get("Content-Type"))
	}
	if err := json.NewDecoder(response.Body).Decode(body); err != nil {
		t.Fatalf("%s %s: decode: %v", method, url, err)
	}
	return response.StatusCode
}

func TestBacklogAPIServesEveryPrincipalsItems(t *testing.T) {
	server, store := newBacklogAPI(t)
	created := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	seedBacklogItem(t, store, "bob", "bbbbbbb1", "Read bus delays aloud", backlog.StatusOpen, created.Add(time.Hour))
	seedBacklogItem(t, store, "alice", "aaaaaaa1", "Chart the weather", backlog.StatusOpen, created)
	seedBacklogItem(t, store, "alice", "aaaaaaa2", "Summarise the week", backlog.StatusDone, created.Add(30*time.Minute))

	var list struct {
		Items []apiItem `json:"items"`
	}
	if status := call(t, http.MethodGet, server.URL+"/api/backlog", &list); status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	if len(list.Items) != 2 || list.Items[0].ID != "aaaaaaa1" || list.Items[0].PrincipalID != "alice" ||
		list.Items[1].ID != "bbbbbbb1" || list.Items[1].PrincipalID != "bob" {
		t.Fatalf("list = %+v", list.Items)
	}

	var item apiItem
	if status := call(t, http.MethodGet, server.URL+"/api/backlog/aaaaaaa2", &item); status != http.StatusOK {
		t.Fatalf("get status = %d", status)
	}
	want := apiItem{
		ID: "aaaaaaa2", PrincipalID: "alice", Title: "Summarise the week", Details: "Details of Summarise the week",
		Status: backlog.StatusDone, CreatedAt: "2026-03-01T09:30:00Z", UpdatedAt: "2026-03-01T09:30:00Z",
	}
	if item != want {
		t.Fatalf("item = %+v, want %+v", item, want)
	}

	var failure struct {
		Error string `json:"error"`
	}
	if status := call(t, http.MethodGet, server.URL+"/api/backlog/ffffffff", &failure); status != http.StatusNotFound || failure.Error != "not_found" {
		t.Fatalf("unknown get = %d %+v", status, failure)
	}
}

func TestBacklogAPIMarksItemsDone(t *testing.T) {
	server, store := newBacklogAPI(t)
	created := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	seedBacklogItem(t, store, "bob", "bbbbbbb1", "Read bus delays aloud", backlog.StatusOpen, created)

	var item apiItem
	if status := call(t, http.MethodPost, server.URL+"/api/backlog/bbbbbbb1/done", &item); status != http.StatusOK {
		t.Fatalf("done status = %d", status)
	}
	if item.ID != "bbbbbbb1" || item.PrincipalID != "bob" || item.Status != backlog.StatusDone || item.UpdatedAt == item.CreatedAt {
		t.Fatalf("done item = %+v", item)
	}
	if open, err := store.ListBacklog(t.Context(), "bob", backlog.StatusOpen); err != nil || len(open) != 0 {
		t.Fatalf("open items after done = %+v, %v", open, err)
	}

	var list struct {
		Items []apiItem `json:"items"`
	}
	if status := call(t, http.MethodGet, server.URL+"/api/backlog", &list); status != http.StatusOK || list.Items == nil || len(list.Items) != 0 {
		t.Fatalf("list after done = %d %+v", status, list.Items)
	}

	var failure struct {
		Error string `json:"error"`
	}
	if status := call(t, http.MethodPost, server.URL+"/api/backlog/bbbbbbb1/done", &failure); status != http.StatusConflict || failure.Error != "already_done" {
		t.Fatalf("second done = %d %+v", status, failure)
	}
	failure.Error = ""
	if status := call(t, http.MethodPost, server.URL+"/api/backlog/ffffffff/done", &failure); status != http.StatusNotFound || failure.Error != "not_found" {
		t.Fatalf("unknown done = %d %+v", status, failure)
	}
}

func TestHTTPHandlerWithoutBacklogServesOnlyHealth(t *testing.T) {
	server := httptest.NewServer(httpHandler(readyChecker{}, observability.Noop(), nil))
	t.Cleanup(server.Close)

	var health struct {
		Status string `json:"status"`
	}
	if status := call(t, http.MethodGet, server.URL+"/healthz", &health); status != http.StatusOK || health.Status != "ok" {
		t.Fatalf("healthz = %d %+v", status, health)
	}
	response, err := http.Get(server.URL + "/api/backlog")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("backlog without service = %d", response.StatusCode)
	}
}

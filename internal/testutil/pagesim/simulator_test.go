package pagesim_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pagesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const seed = `{
  "pages": [
    {"host": "news.example", "path": "/market", "title": "Eastbrook market hours",
     "body": "<p>The Eastbrook market opens at 9:00 on Saturdays.</p>"},
    {"host": "news.example", "path": "/old", "redirect_to": "http://news.example/market"},
    {"host": "news.example", "path": "/gone", "status": 404},
    {"host": "news.example", "path": "/stuck", "timeout": true}
  ]
}`

func fetch(t *testing.T, ctx context.Context, address string) tools.Result {
	t.Helper()
	router := simkit.NewRouter()
	pagesim.New(t, json.RawMessage(seed)).Mount(router)
	service, err := web.NewService(web.Config{
		Resolver: router,
		Dial:     router.DialContext,
		Now:      func() time.Time { return time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC) },
	}, simkit.AlwaysOn{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(ctx, "alice", tools.Call{Name: "fetch_page", Arguments: json.RawMessage(`{"url":"` + address + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSeededPageAndRedirectAreRead(t *testing.T) {
	for _, address := range []string{"http://news.example/market", "http://news.example/old"} {
		result := fetch(t, t.Context(), address)
		if result.IsError || !strings.Contains(result.Text, "opens at 9:00 on Saturdays") || !strings.Contains(result.Text, "Eastbrook market hours") {
			t.Errorf("%s: got %+v", address, result)
		}
	}
}

func TestErrorStatusAndUnknownHost(t *testing.T) {
	if result := fetch(t, t.Context(), "http://news.example/gone"); !result.IsError || !strings.Contains(result.Text, "404") {
		t.Errorf("got %+v, want the 404 told", result)
	}
	if result := fetch(t, t.Context(), "http://elsewhere.example/"); !result.IsError {
		t.Errorf("an unseeded host was reached: %+v", result)
	}
}

func TestTimeoutPageNeverAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	result := fetch(t, ctx, "http://news.example/stuck")
	if !result.IsError || !strings.Contains(result.Text, "did not respond in time") {
		t.Fatalf("got %+v, want a timeout", result)
	}
}

func TestValidateRejectsUnknownFields(t *testing.T) {
	if err := pagesim.Validate(json.RawMessage(seed)); err != nil {
		t.Fatal(err)
	}
	if err := pagesim.Validate(json.RawMessage(`{"pages":[{"path":"/a","nonsense":1}]}`)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

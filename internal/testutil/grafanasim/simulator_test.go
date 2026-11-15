package grafanasim_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/grafana"
	"github.com/vladar107/home-ai-assistant/internal/testutil/grafanasim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const seed = `{
  "dashboards": [{"uid": "svc-health", "title": "Service health", "folder": "Operations", "tags": ["ops"],
    "document": {"title": "Service health", "panels": [{"id": 1, "title": "Up", "type": "stat",
      "targets": [{"expr": "up{job=\"web\"}"}]}]}}],
  "metrics": [{"query": "up{job=\"web\"}", "series": [{"metric": {"job": "web"}, "value": 1,
    "values": [{"time": "2026-03-10T07:00:00Z", "value": 1}, {"time": "2026-03-10T07:30:00Z", "value": 0}]}]}],
  "logs": [{"contains": "web", "streams": [{"labels": {"container": "web"},
    "lines": [{"time": "2026-03-10T07:31:00Z", "line": "connection refused"}]}]}],
  "rules": [{"name": "WebDown", "health": "ok", "instances": [{"labels": {"severity": "critical"},
    "annotations": {"summary": "web is down"}, "state": "Alerting", "active_at": "2026-03-10T07:30:00Z"}]}],
  "annotations": [{"alert_name": "WebDown", "prev_state": "Normal", "new_state": "Alerting",
    "time": "2026-03-10T07:30:00Z"}]
}`

var now = time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC)

func call(t *testing.T, name, arguments string) tools.Result {
	t.Helper()
	fake := grafanasim.New(t, json.RawMessage(seed))
	service, err := grafana.NewService(grafana.Config{
		Token: fake.Token(), BaseURL: fake.URL(), Principals: []string{"alice"},
		Now: func() time.Time { return now },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(t.Context(), "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestConnectorReadsSeededGrafana(t *testing.T) {
	for _, check := range []struct{ tool, arguments, want string }{
		{"grafana_dashboards", `{}`, "Service health"},
		{"grafana_dashboards", `{"uid":"svc-health"}`, `up{job="web"}`},
		{"grafana_metrics", `{"query":"up{job=\"web\"}"}`, `job="web"`},
		{"grafana_metrics", `{"query":"up{job=\"web\"}","range":"1h"}`, "web"},
		{"grafana_logs", `{"query":"{container=\"web\"}"}`, "connection refused"},
		{"grafana_alerts", `{}`, "WebDown"},
		{"grafana_alerts", `{"view":"history"}`, "WebDown"},
	} {
		result := call(t, check.tool, check.arguments)
		if result.IsError || !strings.Contains(result.Text, check.want) {
			t.Errorf("%s %s: lacks %q: %+v", check.tool, check.arguments, check.want, result)
		}
	}
}

func TestUnseededQueryIsRejectedLikeABadQuery(t *testing.T) {
	result := call(t, "grafana_metrics", `{"query":"nonsense_metric"}`)
	if !result.IsError || !strings.Contains(result.Text, "rejected the query") {
		t.Fatalf("got %+v, want a query error", result)
	}
}

func TestWrongTokenAndUnknownParameterAreRefused(t *testing.T) {
	fake := grafanasim.New(t, json.RawMessage(seed))
	for _, check := range []struct {
		path, token string
		want        int
	}{
		{"/api/search", "wrong", http.StatusUnauthorized},
		{"/api/search?bogus=1", fake.Token(), http.StatusBadRequest},
	} {
		request, _ := http.NewRequest(http.MethodGet, fake.URL()+check.path, nil)
		request.Header.Set("Authorization", "Bearer "+check.token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != check.want {
			t.Errorf("%s: got %d, want %d", check.path, response.StatusCode, check.want)
		}
	}
}

func TestValidateRejectsUnknownFields(t *testing.T) {
	if err := grafanasim.Validate(json.RawMessage(seed)); err != nil {
		t.Fatal(err)
	}
	if err := grafanasim.Validate(json.RawMessage(`{"rules":[],"nonsense":1}`)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

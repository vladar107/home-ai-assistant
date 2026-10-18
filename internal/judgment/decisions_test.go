package judgment

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

type decisionFixture struct {
	client *Client
	reader sdkmetric.Reader
}

func newDecisionFixture(t *testing.T, status int, answers string) decisionFixture {
	t.Helper()
	telemetry, reader := newMetricsTelemetry(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = io.WriteString(w, `{"model":"jev-1.13.0","answers":`+answers+`}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(Config{
		APIKey:    "test-key",
		Endpoint:  server.URL,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Telemetry: telemetry,
	})
	if err != nil {
		t.Fatal(err)
	}
	return decisionFixture{client: client, reader: reader}
}

func (f decisionFixture) decisions(t *testing.T, purpose, result string) int64 {
	t.Helper()
	return metricTotal(t, f.reader, "home_agent.judgment.decisions", map[string]string{
		"home_agent.principal": "alice", "home_agent.purpose": purpose, "home_agent.result": result,
	})
}

type markerStub struct{}

func (markerStub) MarkSuspicious(string) {}

func TestIntentDecisionsAreRecorded(t *testing.T) {
	intent := approval.Intent{Verb: "append to", TargetKind: "page", TargetName: "Groceries"}
	check := func(f decisionFixture, prepare func(*turnstate.Registry)) {
		t.Helper()
		registry := turnstate.New(nil)
		registry.RecordMessage("alice", 0, "add bread to my shopping page")
		if prepare != nil {
			prepare(registry)
		}
		_, _ = NewIntentChecker(f.client, stubOptIn(true), registry, registry).Mismatch(context.Background(), "alice", "notion_append_blocks", intent)
	}

	warned := newDecisionFixture(t, http.StatusOK, `{"requested":{"type":"noul","noul":0.1}}`)
	check(warned, nil)
	clean := newDecisionFixture(t, http.StatusOK, `{"requested":{"type":"noul","noul":0.9}}`)
	check(clean, nil)
	suspicious := newDecisionFixture(t, http.StatusInternalServerError, "")
	check(suspicious, func(r *turnstate.Registry) { r.MarkSuspicious("alice") })
	failed := newDecisionFixture(t, http.StatusInternalServerError, "")
	check(failed, nil)

	if got := warned.decisions(t, "intent", "warned"); got != 1 {
		t.Errorf("warned = %d, want 1", got)
	}
	if got := clean.decisions(t, "intent", "clean"); got != 1 {
		t.Errorf("clean = %d, want 1", got)
	}
	if got := suspicious.decisions(t, "intent", "suspicious"); got != 1 {
		t.Errorf("suspicious without a request = %d, want 1", got)
	}
	if got := failed.decisions(t, "intent", "warned") + failed.decisions(t, "intent", "clean"); got != 0 {
		t.Errorf("a failed check recorded %d decisions", got)
	}
}

func TestScreenDecisionsAreRecorded(t *testing.T) {
	screen := func(f decisionFixture, optedIn bool) {
		t.Helper()
		if _, err := NewScreener(f.client, stubOptIn(optedIn), markerStub{}).Screen(context.Background(), "alice", "read_page", "text"); err != nil {
			t.Fatal(err)
		}
	}
	flagged := newDecisionFixture(t, http.StatusOK, `{"instructions_for_ai":{"type":"noul","noul":0.96},"requests_action":{"type":"noul","noul":0.1}}`)
	screen(flagged, true)
	clean := newDecisionFixture(t, http.StatusOK, `{"instructions_for_ai":{"type":"noul","noul":0.1},"requests_action":{"type":"noul","noul":0.1}}`)
	screen(clean, true)
	notOptedIn := newDecisionFixture(t, http.StatusOK, `{"instructions_for_ai":{"type":"noul","noul":0.96},"requests_action":{"type":"noul","noul":0.1}}`)
	screen(notOptedIn, false)

	if got := flagged.decisions(t, "screen", "flagged"); got != 1 {
		t.Errorf("flagged = %d, want 1", got)
	}
	if got := clean.decisions(t, "screen", "clean"); got != 1 {
		t.Errorf("clean = %d, want 1", got)
	}
	if got := notOptedIn.decisions(t, "screen", "flagged") + notOptedIn.decisions(t, "screen", "clean"); got != 0 {
		t.Errorf("a principal who did not opt in recorded %d decisions", got)
	}
}

func TestHintDecisionsAreRecorded(t *testing.T) {
	definitions := []agentruntime.ToolDefinition{{Name: "nas_search", Description: "Search files."}}
	hint := func(f decisionFixture, optedIn bool) {
		t.Helper()
		if _, _, err := NewToolHinter(f.client, stubOptIn(optedIn)).Hint(context.Background(), "alice", "find my tax file", nil, definitions); err != nil {
			t.Fatal(err)
		}
	}
	hinted := newDecisionFixture(t, http.StatusOK, `{"tool":{"type":"choice","choice":"nas_search","confidence":0.9}}`)
	hint(hinted, true)
	none := newDecisionFixture(t, http.StatusOK, `{"tool":{"type":"choice","choice":"none","confidence":0.9}}`)
	hint(none, true)
	unsure := newDecisionFixture(t, http.StatusOK, `{"tool":{"type":"choice","choice":"nas_search","confidence":0.3}}`)
	hint(unsure, true)
	notOptedIn := newDecisionFixture(t, http.StatusOK, `{"tool":{"type":"choice","choice":"nas_search","confidence":0.9}}`)
	hint(notOptedIn, false)

	if got := hinted.decisions(t, "hint", "hinted"); got != 1 {
		t.Errorf("hinted = %d, want 1", got)
	}
	if got := none.decisions(t, "hint", "none"); got != 1 {
		t.Errorf("none = %d, want 1", got)
	}
	if got := unsure.decisions(t, "hint", "none"); got != 1 {
		t.Errorf("low confidence = %d, want none once", got)
	}
	if got := notOptedIn.decisions(t, "hint", "hinted") + notOptedIn.decisions(t, "hint", "none"); got != 0 {
		t.Errorf("a principal who did not opt in recorded %d decisions", got)
	}
}

// metricTotal sums a counter's value, or a histogram's sample count, over the points whose
// attributes include every wanted label.
func metricTotal(t *testing.T, reader sdkmetric.Reader, name string, want map[string]string) int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	matches := func(set attribute.Set) bool {
		for key, value := range want {
			got, ok := set.Value(attribute.Key(key))
			if !ok || got.String() != value {
				return false
			}
		}
		return true
	}
	var total int64
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					if matches(p.Attributes) {
						total += p.Value
					}
				}
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					if matches(p.Attributes) {
						total += int64(p.Count)
					}
				}
			}
		}
	}
	return total
}

func newMetricsTelemetry(t *testing.T) (*observability.Telemetry, sdkmetric.Reader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	telemetry, err := observability.NewWithReader(reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = telemetry.Shutdown(context.Background()) })
	return telemetry, reader
}

type stubOptIn bool

func (s stubOptIn) JudgmentsEnabled(context.Context, string) (bool, error) { return bool(s), nil }

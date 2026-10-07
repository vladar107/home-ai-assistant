package pi_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/observability"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const testStallTimeout = 1500 * time.Millisecond

func stallRuntime(t *testing.T) (*pi.Runtime, pi.Config, *metric.ManualReader) {
	t.Helper()
	reader := metric.NewManualReader()
	telemetry, err := observability.NewWithReader(reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := webSearchConfig(t)
	cfg.StallTimeout = testStallTimeout
	r := newRuntime(t, cfg)
	r.SetTelemetry(telemetry)
	return r, cfg, reader
}

func stallOutcomes(t *testing.T, reader *metric.ManualReader) map[string]int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "home_agent.model.stalls" {
				continue
			}
			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				outcome, _ := point.Attributes.Value("home_agent.outcome")
				outcomes[outcome.AsString()] += point.Value
			}
		}
	}
	return outcomes
}

func userMessages(t *testing.T, cfg pi.Config, session, message string) int {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(sessionDir(cfg, testPrincipal), "*_"+session+".jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("session files = %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, `"role":"user"`) && strings.Contains(line, `"content":"`+message+`"`) {
			count++
		}
	}
	return count
}

func TestSilentTurnIsRetriedOnceWithoutRepeatingTheUserMessage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prior bool
	}{
		{"new session", false},
		{"resumed session", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, cfg, reader := stallRuntime(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if tc.prior {
				if _, err := r.Run(ctx, request("stalled", "remember amber"), &eventSink{}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := r.Run(ctx, request("stalled", "fixture:stall-once"), &eventSink{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Response != "recovered" {
				t.Fatalf("response = %q", result.Response)
			}
			if count := userMessages(t, cfg, "stalled", "fixture:stall-once"); count != 1 {
				t.Errorf("session holds the user message %d times, want once", count)
			}
			if tc.prior && userMessages(t, cfg, "stalled", "remember amber") != 1 {
				t.Error("the earlier turn was lost from the session")
			}
			if got := stallOutcomes(t, reader); len(got) != 1 || got["retried_ok"] != 1 {
				t.Errorf("stall outcomes = %v, want one retried_ok", got)
			}
		})
	}
}

func TestSilenceAfterAToolCallWaitsForTheTurnDeadline(t *testing.T) {
	r, cfg, reader := stallRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	run := request("tool-stall", "fixture:stall-after-tool")
	run.WebSearch = true
	_, err := r.Run(ctx, run, &eventSink{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the turn's own deadline", err)
	}
	starts, err := os.ReadFile(filepath.Join(sessionDir(cfg, testPrincipal), "tool-stall.starts"))
	if err != nil || len(starts) != 1 {
		t.Errorf("Pi started %d times (%v), want once", len(starts), err)
	}
	if got := stallOutcomes(t, reader); len(got) != 1 || got["not_retried"] != 1 {
		t.Errorf("stall outcomes = %v, want one not_retried", got)
	}
}

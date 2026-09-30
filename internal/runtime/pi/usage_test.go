package pi_test

import (
	"context"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/observability"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func runUsageTurn(t *testing.T, message string) (agentruntime.RunResult, metricdata.ResourceMetrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	telemetry, err := observability.NewWithReader(reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := webSearchConfig(t)
	r := newRuntime(t, cfg)
	r.SetTelemetry(telemetry)
	run := principalRequest(testPrincipal, "usage-session", message)
	run.WebSearch = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Run(ctx, run, &eventSink{})
	if err != nil {
		t.Fatal(err)
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &collected); err != nil {
		t.Fatal(err)
	}
	return result, collected
}

func modelCallCounts(t *testing.T, collected metricdata.ResourceMetrics) map[string]uint64 {
	t.Helper()
	counts := map[string]uint64{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "home_agent.model.call.duration" {
				continue
			}
			for _, point := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				provider, _ := point.Attributes.Value("home_agent.provider")
				model, _ := point.Attributes.Value("home_agent.model")
				stop, _ := point.Attributes.Value("home_agent.stop_reason")
				counts[provider.AsString()+"/"+model.AsString()+"/"+stop.AsString()] += point.Count
			}
		}
	}
	return counts
}

func modelTokenCounts(t *testing.T, collected metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	counts := map[string]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "home_agent.model.tokens" {
				continue
			}
			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				kind, _ := point.Attributes.Value("home_agent.type")
				counts[kind.AsString()] += point.Value
			}
		}
	}
	return counts
}

func TestAssistantMessagesRecordModelCallsAndTokens(t *testing.T) {
	result, collected := runUsageTurn(t, "fixture:usage")
	wantUsage := agentruntime.Usage{ModelCalls: 2, PromptTokens: 160 + 150, OutputTokens: 25}
	if result.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", result.Usage, wantUsage)
	}
	calls := modelCallCounts(t, collected)
	if len(calls) != 2 || calls["fixture-provider/fixture-model/toolUse"] != 1 || calls["fixture-provider/fixture-model/stop"] != 1 {
		t.Errorf("model calls = %v", calls)
	}
	tokens := modelTokenCounts(t, collected)
	want := map[string]int64{"input": 130, "output": 25, "cache_read": 170, "cache_write": 10}
	for kind, count := range want {
		if tokens[kind] != count {
			t.Errorf("tokens[%s] = %d, want %d (all: %v)", kind, tokens[kind], count, tokens)
		}
	}
}

func TestAssistantMessageWithoutUsageCountsTheCallOnly(t *testing.T) {
	result, collected := runUsageTurn(t, "fixture:usage-missing")
	wantUsage := agentruntime.Usage{ModelCalls: 2, PromptTokens: 160, OutputTokens: 20}
	if result.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", result.Usage, wantUsage)
	}
	if calls := modelCallCounts(t, collected); calls["fixture-provider/fixture-model/stop"] != 1 || calls["fixture-provider/fixture-model/toolUse"] != 1 {
		t.Errorf("model calls = %v", calls)
	}
	tokens := modelTokenCounts(t, collected)
	want := map[string]int64{"input": 100, "output": 20, "cache_read": 50, "cache_write": 10}
	for kind, count := range want {
		if tokens[kind] != count {
			t.Errorf("tokens[%s] = %d, want %d", kind, tokens[kind], count)
		}
	}
}

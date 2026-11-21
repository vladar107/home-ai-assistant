package pi_test

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/observability"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func runUsageTurn(t *testing.T, message string, adjust ...func(*agentruntime.RunRequest)) (agentruntime.RunResult, metricdata.ResourceMetrics, *eventSink) {
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
	for _, change := range adjust {
		change(&run)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sink := &eventSink{}
	result, err := r.Run(ctx, run, sink)
	if err != nil {
		t.Fatal(err)
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &collected); err != nil {
		t.Fatal(err)
	}
	return result, collected, sink
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

func tokenPointLabels(collected metricdata.ResourceMetrics) map[string]int64 {
	labels := map[string]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "home_agent.model.tokens" {
				continue
			}
			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				kind, _ := point.Attributes.Value("home_agent.kind")
				tools, _ := point.Attributes.Value("home_agent.tools")
				call, _ := point.Attributes.Value("home_agent.call")
				labels[kind.AsString()+"/"+tools.AsString()+"/"+call.AsString()] += point.Value
			}
		}
	}
	return labels
}

func TestTokenPointsCarryKindToolsAndCallPosition(t *testing.T) {
	t.Parallel()
	withTools := func(run *agentruntime.RunRequest) {
		run.Kind = agentruntime.RunChat
		run.Tools = toolSet(t, &echoProvider{principal: testPrincipal}, testPrincipal)
	}
	_, collected, _ := runUsageTurn(t, "fixture:usage", withTools)
	// First call: 100+20+50+10; later call: 30+5+120+0.
	want := map[string]int64{"chat/yes/first": 180, "chat/yes/later": 155}
	if got := tokenPointLabels(collected); !maps.Equal(got, want) {
		t.Errorf("chat turn with tools: %v, want %v", got, want)
	}
	noWebSearch := func(run *agentruntime.RunRequest) { run.WebSearch = false }
	// The errored attempt and its retry both reach the provider as the first call.
	_, collected, _ = runUsageTurn(t, "fixture:usage-retry", noWebSearch)
	want = map[string]int64{"background/no/first": 35}
	if got := tokenPointLabels(collected); !maps.Equal(got, want) {
		t.Errorf("retried first call: %v, want %v", got, want)
	}
}

func TestAssistantMessagesRecordModelCallsAndTokens(t *testing.T) {
	t.Parallel()
	result, collected, sink := runUsageTurn(t, "fixture:usage")
	wantUsage := agentruntime.Usage{ModelCalls: 2, PromptTokens: 160 + 150, OutputTokens: 25}
	if result.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", result.Usage, wantUsage)
	}
	var streamed agentruntime.Usage
	for _, event := range sink.events {
		if event.Type == "model_call" {
			streamed.ModelCalls += event.Usage.ModelCalls
			streamed.PromptTokens += event.Usage.PromptTokens
			streamed.OutputTokens += event.Usage.OutputTokens
		}
	}
	if streamed != wantUsage {
		t.Errorf("model_call events add up to %+v, want %+v", streamed, wantUsage)
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
	t.Parallel()
	result, collected, _ := runUsageTurn(t, "fixture:usage-missing")
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

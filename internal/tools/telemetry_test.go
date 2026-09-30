package tools

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vladar107/home-ai-assistant/internal/observability"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func TestExecuteRecordsToolCallsWithBoundNamesOnly(t *testing.T) {
	telemetry, reader := newMetricsTelemetry(t)
	broker := testBroker(t)
	broker.SetTelemetry(telemetry)
	provider := &fakeProvider{
		principal:   testPrincipal,
		definitions: []Definition{testDefinition("ok_tool", 0), testDefinition("bad_tool", 0)},
		respond: func(_ context.Context, call Call) (Result, error) {
			return Result{Text: "text", IsError: call.Name == "bad_tool"}, nil
		},
	}
	if err := broker.Register(provider); err != nil {
		t.Fatal(err)
	}
	set, err := broker.ToolsFor(context.Background(), testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string) {
		t.Helper()
		if _, err := set.Executor.Execute(context.Background(), agentruntime.ToolCall{Name: name, Arguments: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	call("ok_tool")
	call("bad_tool")
	call("model-invented-name")

	for _, want := range []map[string]string{
		{"home_agent.principal": testPrincipal, "home_agent.tool": "ok_tool", "home_agent.outcome": "success"},
		{"home_agent.principal": testPrincipal, "home_agent.tool": "bad_tool", "home_agent.outcome": "tool_error"},
		{"home_agent.principal": testPrincipal, "home_agent.tool": "unknown", "home_agent.outcome": "unknown_tool"},
	} {
		if got := metricTotal(t, reader, "home_agent.tool.calls", want); got != 1 {
			t.Errorf("tool.calls %v = %d, want 1", want, got)
		}
		if got := metricTotal(t, reader, "home_agent.tool.duration", map[string]string{"home_agent.tool": want["home_agent.tool"], "home_agent.outcome": want["home_agent.outcome"]}); got != 1 {
			t.Errorf("tool.duration %v = %d, want 1", want, got)
		}
	}
	if got := metricTotal(t, reader, "home_agent.tool.calls", map[string]string{"home_agent.tool": "model-invented-name"}); got != 0 {
		t.Errorf("a model-chosen name was used as a label: %d", got)
	}
}

func TestExecuteRecordsBudgetExhaustionForABoundTool(t *testing.T) {
	telemetry, reader := newMetricsTelemetry(t)
	broker := testBroker(t)
	broker.SetTelemetry(telemetry)
	provider := &fakeProvider{
		principal:   testPrincipal,
		definitions: []Definition{testDefinition("busy_tool", 0)},
		respond:     func(context.Context, Call) (Result, error) { return Result{Text: "ok"}, nil },
	}
	if err := broker.Register(provider); err != nil {
		t.Fatal(err)
	}
	set, err := broker.ToolsFor(context.Background(), testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	for range maximumCallsPerTurn + 2 {
		if _, err := set.Executor.Execute(context.Background(), agentruntime.ToolCall{Name: "busy_tool", Arguments: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := metricTotal(t, reader, "home_agent.tool.calls", map[string]string{"home_agent.tool": "busy_tool", "home_agent.outcome": "success"}); got != maximumCallsPerTurn {
		t.Errorf("successes = %d, want %d", got, maximumCallsPerTurn)
	}
	if got := metricTotal(t, reader, "home_agent.tool.calls", map[string]string{"home_agent.tool": "busy_tool", "home_agent.outcome": "budget_exhausted"}); got != 2 {
		t.Errorf("budget_exhausted = %d, want 2", got)
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
			if !ok || got.Emit() != value {
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

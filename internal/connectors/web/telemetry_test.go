package web_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vladar107/home-ai-assistant/internal/observability"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
)

func TestWeatherCardRecordsTheRender(t *testing.T) {
	telemetry, reader := newMetricsTelemetry(t)
	f, _ := newCardFixture(t)
	f.service.SetTelemetry(telemetry)
	f.openMeteoDay()

	f.callInTurn(outbox.ChatTurn(turnID), map[string]any{
		"title":   "Mon 28.09 · Utrecht",
		"summary": "A stormy evening.",
		"legs": []map[string]any{
			{"place": "Utrecht", "date": "2026-09-28", "from_hour": 17, "to_hour": 21},
		},
	})
	if got := metricTotal(t, reader, "home_agent.daycard.render.duration", map[string]string{"home_agent.outcome": "success"}); got != 1 {
		t.Fatalf("successful renders = %d, want 1", got)
	}
	if got := metricTotal(t, reader, "home_agent.daycard.render.duration", map[string]string{"home_agent.outcome": "error"}); got != 0 {
		t.Fatalf("failed renders = %d, want 0", got)
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

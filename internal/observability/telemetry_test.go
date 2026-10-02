package observability

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestBusinessMetricsAreRecordedWithNormalizedLabels(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	telemetry, err := NewWithReader(reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = telemetry.Shutdown(context.Background()) })
	ctx := context.Background()
	_, end := telemetry.StartConversationTurn(ctx, "", "")
	end("success", "")
	telemetry.RecordReply(ctx, "alice", "chat", "delivered", "", time.Second)
	telemetry.RecordReplyStage(ctx, "alice", "model", time.Second)
	telemetry.RecordModelCall(ctx, "alice", "", "m1", "stop", time.Second)
	telemetry.RecordModelTokens(ctx, "alice", "p", "m1", 10, 0, 5, 0)
	telemetry.RecordModelTurn(ctx, "alice", "on", 2, 100, 20)
	telemetry.RecordToolCall(ctx, "alice", "calendar", "", time.Millisecond)
	telemetry.RecordToolCallsPerTurn(ctx, "alice", "on", 1)
	telemetry.RecordDayCardRender(ctx, "success", time.Millisecond)
	telemetry.RecordApproval(ctx, "alice", "notion_create_page", "executed", true)
	telemetry.RecordJudgmentDecision(ctx, "alice", "hint", "hinted")
	telemetry.RecordHintFollowThrough(ctx, "alice", "followed")
	telemetry.RecordScheduleRun(ctx, "alice", "success")

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &collected); err != nil {
		t.Fatal(err)
	}
	points := map[string][]attribute.Set{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					points[m.Name] = append(points[m.Name], p.Attributes)
				}
			case metricdata.Histogram[int64]:
				for _, p := range data.DataPoints {
					points[m.Name] = append(points[m.Name], p.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					points[m.Name] = append(points[m.Name], p.Attributes)
				}
			}
		}
	}

	tests := []struct {
		metric string
		want   map[string]string
		count  int
	}{
		{"home_agent.conversation.turns", map[string]string{"home_agent.principal": "unknown", "home_agent.safety_checks": "unavailable"}, 1},
		{"home_agent.reply.duration", map[string]string{"home_agent.principal": "alice", "home_agent.kind": "chat", "home_agent.outcome": "delivered", "home_agent.safety_checks": "unavailable"}, 1},
		{"home_agent.reply.stage.duration", map[string]string{"home_agent.stage": "model"}, 1},
		{"home_agent.model.call.duration", map[string]string{"home_agent.provider": "unknown", "home_agent.model": "m1", "home_agent.stop_reason": "stop"}, 1},
		{"home_agent.model.tokens", map[string]string{"home_agent.type": "input", "home_agent.provider": "p"}, 2},
		{"home_agent.model.calls_per_turn", map[string]string{"home_agent.safety_checks": "on"}, 1},
		{"home_agent.model.turn_tokens", map[string]string{"home_agent.direction": "prompt"}, 2},
		{"home_agent.tool.calls", map[string]string{"home_agent.tool": "calendar", "home_agent.outcome": "unknown", "home_agent.principal": "alice"}, 1},
		{"home_agent.tool.duration", map[string]string{"home_agent.tool": "calendar", "home_agent.outcome": "unknown"}, 1},
		{"home_agent.tool.calls_per_turn", map[string]string{"home_agent.principal": "alice"}, 1},
		{"home_agent.daycard.render.duration", map[string]string{"home_agent.outcome": "success"}, 1},
		{"home_agent.approvals", map[string]string{"home_agent.status": "executed", "home_agent.warned": "true"}, 1},
		{"home_agent.judgment.decisions", map[string]string{"home_agent.purpose": "hint", "home_agent.result": "hinted"}, 1},
		{"home_agent.hint.follow_through", map[string]string{"home_agent.result": "followed"}, 1},
		{"home_agent.schedule.runs", map[string]string{"home_agent.outcome": "success"}, 1},
	}
	for _, tt := range tests {
		sets := points[tt.metric]
		if len(sets) != tt.count {
			t.Errorf("%s: got %d data points, want %d", tt.metric, len(sets), tt.count)
			continue
		}
		if !anyMatches(sets, tt.want) {
			t.Errorf("%s: no data point carries labels %v", tt.metric, tt.want)
		}
	}
}

func TestRareOutcomeSeriesStartAtZero(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	telemetry, err := NewWithReader(reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = telemetry.Shutdown(context.Background()) })
	ctx := context.Background()
	principals := []string{"alice", "bob"}
	errorKinds := []string{"timeout", "runtime", "tool_provider", "attachments", "not_connected", "provider_quota", "provider_auth"}
	telemetry.ZeroRareOutcomes(ctx, principals, errorKinds)

	type series struct{ metric, labels string }
	collect := func() map[series]int64 {
		t.Helper()
		var collected metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &collected); err != nil {
			t.Fatal(err)
		}
		values := map[series]int64{}
		for _, scope := range collected.ScopeMetrics {
			for _, m := range scope.Metrics {
				if m.Name != "home_agent.conversation.turns" && m.Name != "home_agent.model.calls" {
					continue
				}
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
					for _, p := range sum.DataPoints {
						values[series{m.Name, p.Attributes.Encoded(attribute.DefaultEncoder())}] = p.Value
					}
				}
			}
		}
		return values
	}
	zeroed := collect()
	turnSeries, callSeries := 0, 0
	for key, value := range zeroed {
		if value != 0 {
			t.Errorf("%s{%s} = %d, want 0", key.metric, key.labels, value)
		}
		if key.metric == "home_agent.conversation.turns" {
			turnSeries++
		} else {
			callSeries++
		}
	}
	if want := len(principals) * len(errorKinds) * 3; turnSeries != want {
		t.Errorf("zeroed turn series = %d, want %d", turnSeries, want)
	}
	if want := len(principals) * 3; callSeries != want {
		t.Errorf("zeroed model call series = %d, want %d", callSeries, want)
	}

	// Real increments must land on the zeroed series rather than start new ones.
	for _, kind := range errorKinds {
		for _, safetyChecks := range []string{"on", "off", "unavailable"} {
			_, end := telemetry.StartConversationTurn(ctx, "alice", safetyChecks)
			end("error", kind)
		}
	}
	for _, outcome := range []string{"timeout", "cancelled", "failed"} {
		_, _, end := telemetry.StartModelCall(ctx, "alice")
		end(outcome)
	}
	incremented := collect()
	if len(incremented) != len(zeroed) {
		t.Fatalf("increments created %d new series", len(incremented)-len(zeroed))
	}
	for key := range zeroed {
		want := int64(0)
		if strings.Contains(key.labels, "home_agent.principal=alice") {
			want = 1
		}
		if incremented[key] != want {
			t.Errorf("%s{%s} = %d after increments, want %d", key.metric, key.labels, incremented[key], want)
		}
	}
}

func anyMatches(sets []attribute.Set, want map[string]string) bool {
	for _, set := range sets {
		matched := true
		for key, value := range want {
			got, ok := set.Value(attribute.Key(key))
			if !ok || got.Emit() != value {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

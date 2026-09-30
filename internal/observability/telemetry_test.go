package observability

import (
	"context"
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

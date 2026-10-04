package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	taskDurationBoundaries = []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 2700, 3600, 5400, 7200}
	taskCallBoundaries     = []float64{0, 1, 2, 5, 10, 20, 30, 50, 75, 100, 150, 200}
)

type taskInstruments struct {
	runs       metric.Int64Counter
	duration   metric.Float64Histogram
	modelCalls metric.Int64Histogram
	toolCalls  metric.Int64Histogram
	tokens     metric.Int64Histogram
}

func (t *Telemetry) createTaskInstruments(meter metric.Meter) error {
	var err error
	if t.tasks.runs, err = meter.Int64Counter("home_agent.task.runs",
		metric.WithDescription("Background task runs ended, by outcome."), metric.WithUnit("{run}")); err != nil {
		return fmt.Errorf("create home_agent.task.runs counter: %w", err)
	}
	if t.tasks.duration, err = meter.Float64Histogram("home_agent.task.duration",
		metric.WithDescription("Background task run duration."), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(taskDurationBoundaries...)); err != nil {
		return fmt.Errorf("create home_agent.task.duration histogram: %w", err)
	}
	if t.tasks.modelCalls, err = meter.Int64Histogram("home_agent.task.model_calls",
		metric.WithDescription("Model calls a background task used so far when a run ended."), metric.WithUnit("{call}"),
		metric.WithExplicitBucketBoundaries(taskCallBoundaries...)); err != nil {
		return fmt.Errorf("create home_agent.task.model_calls histogram: %w", err)
	}
	if t.tasks.toolCalls, err = meter.Int64Histogram("home_agent.task.tool_calls",
		metric.WithDescription("Tool calls a background task used so far when a run ended."), metric.WithUnit("{call}"),
		metric.WithExplicitBucketBoundaries(taskCallBoundaries...)); err != nil {
		return fmt.Errorf("create home_agent.task.tool_calls histogram: %w", err)
	}
	if t.tasks.tokens, err = meter.Int64Histogram("home_agent.task.tokens",
		metric.WithDescription("Model tokens a background task used so far when a run ended."), metric.WithUnit("{token}"),
		metric.WithExplicitBucketBoundaries(tokenBoundaries...)); err != nil {
		return fmt.Errorf("create home_agent.task.tokens histogram: %w", err)
	}
	return nil
}

// StartTask opens the root span of one run of a background task, so its model and tool calls form
// one trace, and returns the function that ends it with the outcome and what the task used.
func (t *Telemetry) StartTask(ctx context.Context, principal string) (context.Context, func(string, int, int, int64, int64)) {
	principal = labelOr(principal, "unknown")
	ctx, span := t.tracer.Start(trace.ContextWithSpanContext(ctx, trace.SpanContext{}), "home_agent.task.run",
		trace.WithAttributes(attribute.String("home_agent.principal", principal)),
	)
	started := time.Now()
	return ctx, func(outcome string, modelCalls, toolCalls int, promptTokens, outputTokens int64) {
		span.SetAttributes(
			attribute.Int("home_agent.model_calls", modelCalls),
			attribute.Int("home_agent.tool_calls", toolCalls),
		)
		finishSpan(span, outcome, nil)
		attributes := metric.WithAttributes(principalAttr(principal), labelAttr("home_agent.outcome", outcome, "unknown"))
		t.tasks.runs.Add(ctx, 1, attributes)
		t.tasks.duration.Record(ctx, time.Since(started).Seconds(), attributes)
		t.tasks.modelCalls.Record(ctx, int64(modelCalls), metric.WithAttributes(principalAttr(principal)))
		t.tasks.toolCalls.Record(ctx, int64(toolCalls), metric.WithAttributes(principalAttr(principal)))
		for _, tokens := range []struct {
			direction string
			count     int64
		}{{"prompt", promptTokens}, {"output", outputTokens}} {
			t.tasks.tokens.Record(ctx, tokens.count, metric.WithAttributes(
				principalAttr(principal),
				attribute.String("home_agent.direction", tokens.direction),
			))
		}
	}
}

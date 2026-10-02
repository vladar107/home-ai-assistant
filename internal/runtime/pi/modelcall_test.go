package pi_test

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/observability"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pifixture"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type modelCallTurn struct {
	err      error
	outcomes map[string]int64
	// finished counts the duration histogram's records, one per assistant message.
	finished uint64
	spans    []sdktrace.ReadOnlySpan
	turn     trace.SpanContext
	logs     string
}

// runModelCallTurn runs one turn under a conversation turn span and collects the model call
// counter, the model call spans, and the runtime's log.
func runModelCallTurn(t *testing.T, message string, timeout time.Duration) modelCallTurn {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	recorder := tracetest.NewSpanRecorder()
	telemetry, err := observability.NewWithReader(reader, sdktrace.WithSpanProcessor(recorder))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	cfg := webSearchConfig(t)
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	r := newRuntime(t, cfg)
	r.SetTelemetry(telemetry)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	turnCtx, endTurn := telemetry.StartConversationTurn(ctx, testPrincipal, "on")
	run := principalRequest(testPrincipal, "model-calls", message)
	run.WebSearch = true
	_, runErr := r.Run(turnCtx, run, &eventSink{})
	endTurn("success", "")

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	result := modelCallTurn{err: runErr, outcomes: map[string]int64{}, turn: trace.SpanContextFromContext(turnCtx), logs: logs.String()}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "home_agent.model.call.duration" {
				for _, point := range m.Data.(metricdata.Histogram[float64]).DataPoints {
					result.finished += point.Count
				}
			}
			if m.Name != "home_agent.model.calls" {
				continue
			}
			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				principal, _ := point.Attributes.Value("home_agent.principal")
				outcome, _ := point.Attributes.Value("home_agent.outcome")
				if principal.AsString() != testPrincipal {
					t.Errorf("model call principal = %q", principal.AsString())
				}
				result.outcomes[outcome.AsString()] += point.Value
			}
		}
	}
	for _, span := range recorder.Ended() {
		if span.Name() == "home_agent.model.call" {
			result.spans = append(result.spans, span)
		}
	}
	return result
}

func spanOutcome(span sdktrace.ReadOnlySpan) string {
	for _, attribute := range span.Attributes() {
		if attribute.Key == "home_agent.outcome" {
			return attribute.Value.AsString()
		}
	}
	return ""
}

func assertModelCalls(t *testing.T, turn modelCallTurn, want map[string]int64) {
	t.Helper()
	if !maps.Equal(turn.outcomes, want) {
		t.Errorf("model call outcomes = %v, want %v", turn.outcomes, want)
	}
	spans := map[string]int64{}
	for _, span := range turn.spans {
		outcome := spanOutcome(span)
		spans[outcome]++
		if span.Parent().SpanID() != turn.turn.SpanID() || span.SpanContext().TraceID() != turn.turn.TraceID() {
			t.Errorf("%s model call span is not a child of the turn span", outcome)
		}
		wantStatus := codes.Error
		if outcome == "completed" {
			wantStatus = codes.Ok
		}
		if span.Status().Code != wantStatus {
			t.Errorf("%s model call span status = %v, want %v", outcome, span.Status().Code, wantStatus)
		}
	}
	if !maps.Equal(spans, want) {
		t.Errorf("model call spans by outcome = %v, want %v", spans, want)
	}
}

func TestUnansweredModelCallTimesOut(t *testing.T) {
	for _, tc := range []struct{ name, message string }{
		{"never answers", "fixture:stubborn"},
		// Pi exits on SIGINT, so its exit may race ctx.Done in the runtime's select.
		{"exits on interrupt", "fixture:hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn := runModelCallTurn(t, tc.message, 500*time.Millisecond)
			if turn.err == nil {
				t.Fatal("unanswered turn succeeded")
			}
			assertModelCalls(t, turn, map[string]int64{"timeout": 1})
		})
	}
}

func TestModelCallOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		want          map[string]int64
	}{
		{"parallel tools", "fixture:pi-tools", map[string]int64{"completed": 2}},
		{"provider error", "fixture:unrelated-error", map[string]int64{"failed": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn := runModelCallTurn(t, tc.message, 10*time.Second)
			assertModelCalls(t, turn, tc.want)
		})
	}
}

func TestPiRetriesStayInsideOneModelCall(t *testing.T) {
	for _, tc := range []struct {
		name, message, outcome string
		succeeds               bool
	}{
		{"recovered", "fixture:pi-retry", "completed", true},
		{"retries exhausted", "fixture:pi-retry-exhausted", "failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn := runModelCallTurn(t, tc.message, 10*time.Second)
			if (turn.err == nil) != tc.succeeds {
				t.Fatalf("error = %v", turn.err)
			}
			assertModelCalls(t, turn, map[string]int64{tc.outcome: 1})
			if turn.finished != 2 {
				t.Errorf("duration histogram recorded %d messages, want both attempts", turn.finished)
			}
			for _, span := range turn.spans {
				events := span.Events()
				if len(events) != 1 || events[0].Name != "pi.auto_retry" ||
					len(events[0].Attributes) != 1 || events[0].Attributes[0].Value.AsString() != "network" {
					t.Errorf("model call events = %+v, want one network pi.auto_retry", events)
				}
				if duration := span.EndTime().Sub(span.StartTime()); duration < pifixture.RetryBackoff {
					t.Errorf("model call lasted %s, shorter than the %s backoff", duration, pifixture.RetryBackoff)
				}
				if count := strings.Count(turn.logs, "pi.model_call.retried"); count != 1 ||
					!strings.Contains(turn.logs, "level=WARN") || !strings.Contains(turn.logs, "category=network") ||
					!strings.Contains(turn.logs, "retries=1") || !strings.Contains(turn.logs, "span_id="+span.SpanContext().SpanID().String()) {
					t.Errorf("retry log = %q, want one warning with the category, retry count, and model call span", turn.logs)
				}
			}
		})
	}
}

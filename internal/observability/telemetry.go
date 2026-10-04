package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const instrumentationName = "github.com/vladar107/home-ai-assistant"

// Every duration histogram records seconds, while the SDK default boundaries
// are sized for milliseconds and collapse whole sub-second distributions into
// the first bucket.
var (
	requestDurationBoundaries   = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	turnDurationBoundaries      = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 180, 300}
	lifecycleDurationBoundaries = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60}
	callCountBoundaries         = []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	tokenBoundaries             = []float64{500, 1000, 2000, 4000, 8000, 16000, 32000, 64000, 128000, 256000, 512000, 1024000}
)

type Telemetry struct {
	enabled        bool
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	tracer         trace.Tracer
	meter          metric.Meter

	applicationLifecycle         metric.Int64Counter
	applicationLifecycleDuration metric.Float64Histogram
	applicationRunning           metric.Int64UpDownCounter
	httpRequests                 metric.Int64Counter
	httpRequestDuration          metric.Float64Histogram
	telegramAPIRequests          metric.Int64Counter
	telegramAPIRequestDuration   metric.Float64Histogram
	telegramUpdates              metric.Int64Counter
	telegramUpdateDuration       metric.Float64Histogram
	telegramRetries              metric.Int64Counter
	conversationTurns            metric.Int64Counter
	conversationTurnDuration     metric.Float64Histogram
	judgmentRequests             metric.Int64Counter
	judgmentDuration             metric.Float64Histogram
	storeOperations              metric.Int64Counter
	storeOperationDuration       metric.Float64Histogram
	replyDuration                metric.Float64Histogram
	replyStageDuration           metric.Float64Histogram
	modelCallDuration            metric.Float64Histogram
	modelCalls                   metric.Int64Counter
	modelTokens                  metric.Int64Counter
	modelCallsPerTurn            metric.Int64Histogram
	modelTurnTokens              metric.Int64Histogram
	toolCalls                    metric.Int64Counter
	toolDuration                 metric.Float64Histogram
	toolCallsPerTurn             metric.Int64Histogram
	dayCardRenderDuration        metric.Float64Histogram
	approvals                    metric.Int64Counter
	judgmentDecisions            metric.Int64Counter
	hintFollowThrough            metric.Int64Counter
	scheduleRuns                 metric.Int64Counter
	memoryWrites                 metric.Int64Counter
	conversationResumes          metric.Int64Counter
	conversationSearches         metric.Int64Counter
	tasks                        taskInstruments
}

func New(ctx context.Context, cfg config.ObservabilityConfig, logger *slog.Logger) (*Telemetry, error) {
	if !cfg.Enabled {
		return Noop(), nil
	}

	res, err := resource.New(
		ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceNamespace("home"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create OpenTelemetry resource: %w", err)
	}

	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("initialize OTLP/HTTP trace exporter: %w", err)
	}
	metricExporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		return nil, fmt.Errorf("initialize OTLP/HTTP metric exporter: %w", err)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	metricReader := sdkmetric.NewPeriodicReader(
		metricExporter,
		sdkmetric.WithInterval(cfg.MetricExportInterval),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(metricReader),
		sdkmetric.WithResource(res),
	)

	telemetry, err := newEnabledTelemetry(tracerProvider, meterProvider)
	if err != nil {
		_ = errors.Join(tracerProvider.Shutdown(ctx), meterProvider.Shutdown(ctx))
		return nil, err
	}

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {
		logger.Warn("telemetry.export.failed")
	}))

	return telemetry, nil
}

// NewWithReader builds enabled telemetry whose metrics flow to reader and whose traces are not
// exported unless options add a span processor.
func NewWithReader(reader sdkmetric.Reader, options ...sdktrace.TracerProviderOption) (*Telemetry, error) {
	return newEnabledTelemetry(
		sdktrace.NewTracerProvider(options...),
		sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
	)
}

func newEnabledTelemetry(tracerProvider *sdktrace.TracerProvider, meterProvider *sdkmetric.MeterProvider) (*Telemetry, error) {
	telemetry, err := newTelemetry(
		tracerProvider.Tracer(instrumentationName),
		meterProvider.Meter(instrumentationName),
	)
	if err != nil {
		return nil, err
	}
	telemetry.enabled = true
	telemetry.tracerProvider = tracerProvider
	telemetry.meterProvider = meterProvider
	return telemetry, nil
}

func Noop() *Telemetry {
	telemetry, err := newTelemetry(
		tracenoop.NewTracerProvider().Tracer(instrumentationName),
		noop.NewMeterProvider().Meter(instrumentationName),
	)
	if err != nil {
		panic(err)
	}
	return telemetry
}

func newTelemetry(tracer trace.Tracer, meter metric.Meter) (*Telemetry, error) {
	t := &Telemetry{tracer: tracer, meter: meter}
	var err error
	if t.applicationLifecycle, err = meter.Int64Counter(
		"home_agent.application.lifecycle",
		metric.WithDescription("Application lifecycle phase completions."),
		metric.WithUnit("{event}"),
	); err != nil {
		return nil, fmt.Errorf("create application lifecycle counter: %w", err)
	}
	if t.applicationLifecycleDuration, err = meter.Float64Histogram(
		"home_agent.application.lifecycle.duration",
		metric.WithDescription("Application lifecycle phase duration."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(lifecycleDurationBoundaries...),
	); err != nil {
		return nil, fmt.Errorf("create application lifecycle duration histogram: %w", err)
	}
	if t.applicationRunning, err = meter.Int64UpDownCounter(
		"home_agent.application.running",
		metric.WithDescription("Whether the application is running."),
		metric.WithUnit("{application}"),
	); err != nil {
		return nil, fmt.Errorf("create application running counter: %w", err)
	}
	if t.httpRequests, err = meter.Int64Counter(
		"home_agent.http.server.requests",
		metric.WithDescription("Health HTTP requests completed."),
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("create HTTP request counter: %w", err)
	}
	if t.httpRequestDuration, err = meter.Float64Histogram(
		"home_agent.http.server.request.duration",
		metric.WithDescription("Health HTTP request duration."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(requestDurationBoundaries...),
	); err != nil {
		return nil, fmt.Errorf("create HTTP request duration histogram: %w", err)
	}
	if t.telegramAPIRequests, err = meter.Int64Counter(
		"home_agent.telegram.api.requests",
		metric.WithDescription("Telegram Bot API requests completed."),
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("create Telegram API request counter: %w", err)
	}
	if t.telegramAPIRequestDuration, err = meter.Float64Histogram(
		"home_agent.telegram.api.request.duration",
		metric.WithDescription("Telegram Bot API request duration."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(requestDurationBoundaries...),
	); err != nil {
		return nil, fmt.Errorf("create Telegram API request duration histogram: %w", err)
	}
	if t.telegramUpdates, err = meter.Int64Counter(
		"home_agent.telegram.updates",
		metric.WithDescription("Telegram updates processed."),
		metric.WithUnit("{update}"),
	); err != nil {
		return nil, fmt.Errorf("create Telegram update counter: %w", err)
	}
	if t.telegramUpdateDuration, err = meter.Float64Histogram(
		"home_agent.telegram.update.duration",
		metric.WithDescription("Telegram update processing duration."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(turnDurationBoundaries...),
	); err != nil {
		return nil, fmt.Errorf("create Telegram update duration histogram: %w", err)
	}
	if t.telegramRetries, err = meter.Int64Counter(
		"home_agent.telegram.poll.retries",
		metric.WithDescription("Telegram polling retries scheduled."),
		metric.WithUnit("{retry}"),
	); err != nil {
		return nil, fmt.Errorf("create Telegram retry counter: %w", err)
	}
	if t.conversationTurns, err = meter.Int64Counter(
		"home_agent.conversation.turns",
		metric.WithDescription("Conversation turns completed."),
		metric.WithUnit("{turn}"),
	); err != nil {
		return nil, fmt.Errorf("create conversation turn counter: %w", err)
	}
	if t.conversationTurnDuration, err = meter.Float64Histogram(
		"home_agent.conversation.turn.duration",
		metric.WithDescription("Conversation turn duration."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(turnDurationBoundaries...),
	); err != nil {
		return nil, fmt.Errorf("create conversation turn duration histogram: %w", err)
	}
	if t.judgmentRequests, err = meter.Int64Counter(
		"home_agent.judgment.requests",
		metric.WithDescription("TypeSafe judgment requests completed."),
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("create judgment request counter: %w", err)
	}
	if t.judgmentDuration, err = meter.Float64Histogram(
		"home_agent.judgment.duration",
		metric.WithDescription("TypeSafe judgment request duration."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(requestDurationBoundaries...),
	); err != nil {
		return nil, fmt.Errorf("create judgment duration histogram: %w", err)
	}
	if t.storeOperations, err = meter.Int64Counter(
		"home_agent.store.operations",
		metric.WithDescription("Application store operations completed."),
		metric.WithUnit("{operation}"),
	); err != nil {
		return nil, fmt.Errorf("create store operation counter: %w", err)
	}
	if t.storeOperationDuration, err = meter.Float64Histogram(
		"home_agent.store.operation.duration",
		metric.WithDescription("Application store operation duration."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(requestDurationBoundaries...),
	); err != nil {
		return nil, fmt.Errorf("create store operation duration histogram: %w", err)
	}
	if err := t.createBusinessInstruments(meter); err != nil {
		return nil, err
	}
	if err := registerRuntimeMetrics(meter); err != nil {
		return nil, err
	}

	return t, nil
}

func (t *Telemetry) createBusinessInstruments(meter metric.Meter) error {
	var err error
	seconds := func(name, description string, boundaries []float64) (metric.Float64Histogram, error) {
		histogram, err := meter.Float64Histogram(name,
			metric.WithDescription(description),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(boundaries...),
		)
		if err != nil {
			return nil, fmt.Errorf("create %s histogram: %w", name, err)
		}
		return histogram, nil
	}
	counts := func(name, description, unit string, boundaries []float64) (metric.Int64Histogram, error) {
		histogram, err := meter.Int64Histogram(name,
			metric.WithDescription(description),
			metric.WithUnit(unit),
			metric.WithExplicitBucketBoundaries(boundaries...),
		)
		if err != nil {
			return nil, fmt.Errorf("create %s histogram: %w", name, err)
		}
		return histogram, nil
	}
	counter := func(name, description, unit string) (metric.Int64Counter, error) {
		c, err := meter.Int64Counter(name, metric.WithDescription(description), metric.WithUnit(unit))
		if err != nil {
			return nil, fmt.Errorf("create %s counter: %w", name, err)
		}
		return c, nil
	}
	if t.replyDuration, err = seconds("home_agent.reply.duration", "Time from accepted message to final reply delivered.", turnDurationBoundaries); err != nil {
		return err
	}
	if t.replyStageDuration, err = seconds("home_agent.reply.stage.duration", "Reply pipeline stage duration.", turnDurationBoundaries); err != nil {
		return err
	}
	if t.modelCallDuration, err = seconds("home_agent.model.call.duration", "Model call duration.", turnDurationBoundaries); err != nil {
		return err
	}
	if t.modelCalls, err = counter("home_agent.model.calls", "Model calls ended, by outcome.", "{call}"); err != nil {
		return err
	}
	if t.modelTokens, err = counter("home_agent.model.tokens", "Model tokens reported by the runtime.", "{token}"); err != nil {
		return err
	}
	if t.modelCallsPerTurn, err = counts("home_agent.model.calls_per_turn", "Model calls made in one turn.", "{call}", callCountBoundaries); err != nil {
		return err
	}
	if t.modelTurnTokens, err = counts("home_agent.model.turn_tokens", "Model tokens used in one turn.", "{token}", tokenBoundaries); err != nil {
		return err
	}
	if t.toolCalls, err = counter("home_agent.tool.calls", "Tool calls completed.", "{call}"); err != nil {
		return err
	}
	if t.toolDuration, err = seconds("home_agent.tool.duration", "Tool call duration.", requestDurationBoundaries); err != nil {
		return err
	}
	if t.toolCallsPerTurn, err = counts("home_agent.tool.calls_per_turn", "Tool calls made in one turn.", "{call}", callCountBoundaries); err != nil {
		return err
	}
	if t.dayCardRenderDuration, err = seconds("home_agent.daycard.render.duration", "Day card render duration.", requestDurationBoundaries); err != nil {
		return err
	}
	if t.approvals, err = counter("home_agent.approvals", "Approval lifecycle events.", "{approval}"); err != nil {
		return err
	}
	if t.judgmentDecisions, err = counter("home_agent.judgment.decisions", "TypeSafe judgment decisions made.", "{decision}"); err != nil {
		return err
	}
	if t.hintFollowThrough, err = counter("home_agent.hint.follow_through", "Hint follow-through results.", "{hint}"); err != nil {
		return err
	}
	if t.scheduleRuns, err = counter("home_agent.schedule.runs", "Scheduled runs completed.", "{run}"); err != nil {
		return err
	}
	if t.memoryWrites, err = counter("home_agent.memory.writes", "Memory writes by kind and outcome.", "{write}"); err != nil {
		return err
	}
	if t.conversationResumes, err = counter("home_agent.conversation.resumes", "Returns to an earlier conversation, by trigger.", "{resume}"); err != nil {
		return err
	}
	if t.conversationSearches, err = counter("home_agent.conversation.searches", "Searches of earlier conversations, by outcome.", "{search}"); err != nil {
		return err
	}
	return t.createTaskInstruments(meter)
}

func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil || !t.enabled {
		return nil
	}
	return errors.Join(
		t.tracerProvider.ForceFlush(ctx),
		t.meterProvider.ForceFlush(ctx),
		t.tracerProvider.Shutdown(ctx),
		t.meterProvider.Shutdown(ctx),
	)
}

func (t *Telemetry) ContextLogger(ctx context.Context, logger *slog.Logger) *slog.Logger {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return logger
	}
	return logger.With(
		"trace_id", spanContext.TraceID().String(),
		"span_id", spanContext.SpanID().String(),
	)
}

func (t *Telemetry) StartLifecycle(ctx context.Context, phase string) (context.Context, func(error)) {
	ctx, span := t.tracer.Start(ctx, "home_agent.application."+phase,
		trace.WithAttributes(attribute.String("home_agent.phase", phase)),
	)
	started := time.Now()
	return ctx, func(err error) {
		outcome := outcomeForError(err)
		finishSpan(span, outcome, err)
		attributes := metric.WithAttributes(
			attribute.String("home_agent.phase", phase),
			attribute.String("home_agent.outcome", outcome),
		)
		t.applicationLifecycle.Add(ctx, 1, attributes)
		t.applicationLifecycleDuration.Record(ctx, time.Since(started).Seconds(), attributes)
	}
}

func (t *Telemetry) SetRunning(ctx context.Context, running bool) {
	delta := int64(-1)
	if running {
		delta = 1
	}
	t.applicationRunning.Add(ctx, delta)
}

func (t *Telemetry) StartHTTP(ctx context.Context, endpoint string) (context.Context, func(string, int)) {
	ctx, span := t.tracer.Start(ctx, "home_agent.http.health",
		trace.WithAttributes(attribute.String("home_agent.endpoint", endpoint)),
	)
	started := time.Now()
	return ctx, func(result string, statusCode int) {
		outcome := "success"
		if statusCode >= 500 {
			outcome = "error"
		}
		finishSpan(span, outcome, nil)
		attributes := metric.WithAttributes(
			attribute.String("home_agent.endpoint", endpoint),
			attribute.String("home_agent.result", result),
			attribute.Int("http.response.status_code", statusCode),
		)
		t.httpRequests.Add(ctx, 1, attributes)
		t.httpRequestDuration.Record(ctx, time.Since(started).Seconds(), attributes)
	}
}

func (t *Telemetry) StartTelegramAPI(ctx context.Context, operation string) (context.Context, func(error)) {
	ctx, span := t.tracer.Start(ctx, "home_agent.telegram.api",
		trace.WithAttributes(attribute.String("home_agent.operation", operation)),
	)
	started := time.Now()
	return ctx, func(err error) {
		outcome := outcomeForError(err)
		finishSpan(span, outcome, err)
		attributes := metric.WithAttributes(
			attribute.String("home_agent.operation", operation),
			attribute.String("home_agent.outcome", outcome),
		)
		t.telegramAPIRequests.Add(ctx, 1, attributes)
		t.telegramAPIRequestDuration.Record(ctx, time.Since(started).Seconds(), attributes)
	}
}

func (t *Telemetry) StartTelegramUpdate(ctx context.Context) (context.Context, func(string, string, string, error)) {
	ctx, span := t.tracer.Start(ctx, "home_agent.telegram.update")
	started := time.Now()
	return ctx, func(outcome, reason, command string, err error) {
		if reason == "" {
			reason = "none"
		}
		if command == "" {
			command = "none"
		}
		finishSpan(span, outcome, err)
		attributes := metric.WithAttributes(
			attribute.String("home_agent.outcome", outcome),
			attribute.String("home_agent.reason", reason),
			attribute.String("home_agent.command", command),
		)
		t.telegramUpdates.Add(ctx, 1, attributes)
		t.telegramUpdateDuration.Record(ctx, time.Since(started).Seconds(), attributes)
	}
}

func (t *Telemetry) RecordTelegramRetry(ctx context.Context, reason string) {
	t.telegramRetries.Add(ctx, 1, metric.WithAttributes(attribute.String("home_agent.reason", reason)))
}

func (t *Telemetry) StartConversationTurn(ctx context.Context, principal, safetyChecks string) (context.Context, func(string, string)) {
	principal = labelOr(principal, "unknown")
	safetyChecks = labelOr(safetyChecks, "unavailable")
	ctx, span := t.tracer.Start(ctx, "home_agent.conversation.turn",
		trace.WithAttributes(
			attribute.String("home_agent.principal", principal),
			attribute.String("home_agent.safety_checks", safetyChecks),
		),
	)
	started := time.Now()
	return ctx, func(outcome, errorKind string) {
		if errorKind == "" {
			errorKind = "none"
		}
		span.SetAttributes(attribute.String("home_agent.error_kind", errorKind))
		finishSpan(span, outcome, nil)
		attributes := metric.WithAttributes(
			attribute.String("home_agent.outcome", outcome),
			attribute.String("home_agent.error_kind", errorKind),
			attribute.String("home_agent.principal", principal),
			attribute.String("home_agent.safety_checks", safetyChecks),
		)
		t.conversationTurns.Add(ctx, 1, attributes)
		t.conversationTurnDuration.Record(ctx, time.Since(started).Seconds(), attributes)
	}
}

func (t *Telemetry) RecordJudgment(ctx context.Context, purpose, outcome string, duration time.Duration) {
	attributes := metric.WithAttributes(
		attribute.String("home_agent.purpose", purpose),
		attribute.String("home_agent.outcome", outcome),
	)
	t.judgmentRequests.Add(ctx, 1, attributes)
	t.judgmentDuration.Record(ctx, duration.Seconds(), attributes)
}

func (t *Telemetry) RecordReply(ctx context.Context, principal, kind, outcome, safetyChecks string, duration time.Duration) {
	t.replyDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.kind", kind, "unknown"),
		labelAttr("home_agent.outcome", outcome, "unknown"),
		safetyChecksAttr(safetyChecks),
	))
}

func (t *Telemetry) RecordReplyStage(ctx context.Context, principal, stage string, duration time.Duration) {
	t.replyStageDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.stage", stage, "unknown"),
	))
}

func (t *Telemetry) RecordModelCall(ctx context.Context, principal, provider, model, stopReason string, duration time.Duration) {
	t.modelCallDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.provider", provider, "unknown"),
		labelAttr("home_agent.model", model, "unknown"),
		labelAttr("home_agent.stop_reason", stopReason, "unknown"),
	))
}

// StartModelCall opens a model call span under the turn; the first function adds a Pi retry event
// and the second ends the call with its outcome.
func (t *Telemetry) StartModelCall(ctx context.Context, principal string) (context.Context, func(string), func(string)) {
	ctx, span := t.tracer.Start(ctx, "home_agent.model.call", trace.WithAttributes(principalAttr(principal)))
	retried := func(category string) {
		span.AddEvent("pi.auto_retry", trace.WithAttributes(labelAttr("home_agent.category", category, "unknown")))
	}
	return ctx, retried, func(outcome string) {
		outcome = labelOr(outcome, "unknown")
		span.SetAttributes(attribute.String("home_agent.outcome", outcome))
		if outcome == "completed" {
			span.SetStatus(codes.Ok, "")
		} else {
			span.SetStatus(codes.Error, "model call "+outcome)
		}
		span.End()
		t.modelCalls.Add(ctx, 1, metric.WithAttributes(principalAttr(principal), labelAttr("home_agent.outcome", outcome, "unknown")))
	}
}

// StartToolCall opens a tool call span under the turn; the returned function ends it with the
// call's outcome. The tool must already be a bound name or "unknown".
func (t *Telemetry) StartToolCall(ctx context.Context, principal, tool string) (context.Context, func(string)) {
	ctx, span := t.tracer.Start(ctx, "home_agent.tool.call", trace.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.tool", tool, "unknown"),
	))
	return ctx, func(outcome string) {
		outcome = labelOr(outcome, "unknown")
		span.SetAttributes(attribute.String("home_agent.outcome", outcome))
		if outcome == "success" {
			span.SetStatus(codes.Ok, "")
		} else {
			span.SetStatus(codes.Error, "tool call "+outcome)
		}
		span.End()
	}
}

// ZeroRareOutcomes creates each principal's rare failure series at zero, so increase() sees their
// first occurrence. The label tuples must match the ones StartConversationTurn and StartModelCall
// record.
func (t *Telemetry) ZeroRareOutcomes(ctx context.Context, principals, turnErrorKinds []string) {
	for _, principal := range principals {
		for _, errorKind := range turnErrorKinds {
			for _, safetyChecks := range []string{"on", "off", "unavailable"} {
				t.conversationTurns.Add(ctx, 0, metric.WithAttributes(
					attribute.String("home_agent.outcome", "error"),
					attribute.String("home_agent.error_kind", errorKind),
					principalAttr(principal),
					safetyChecksAttr(safetyChecks),
				))
			}
		}
		for _, outcome := range []string{"timeout", "cancelled", "failed"} {
			t.modelCalls.Add(ctx, 0, metric.WithAttributes(principalAttr(principal), labelAttr("home_agent.outcome", outcome, "unknown")))
		}
	}
}

func (t *Telemetry) RecordModelTokens(ctx context.Context, principal, provider, model string, input, output, cacheRead, cacheWrite int64) {
	for _, tokens := range []struct {
		kind  string
		count int64
	}{{"input", input}, {"output", output}, {"cache_read", cacheRead}, {"cache_write", cacheWrite}} {
		if tokens.count == 0 {
			continue
		}
		t.modelTokens.Add(ctx, tokens.count, metric.WithAttributes(
			principalAttr(principal),
			labelAttr("home_agent.provider", provider, "unknown"),
			labelAttr("home_agent.model", model, "unknown"),
			attribute.String("home_agent.type", tokens.kind),
		))
	}
}

func (t *Telemetry) RecordModelTurn(ctx context.Context, principal, safetyChecks string, calls int, promptTokens, outputTokens int64) {
	// A runtime that reported no model calls reported nothing, which is not a zero-call turn.
	if calls == 0 {
		return
	}
	t.modelCallsPerTurn.Record(ctx, int64(calls), metric.WithAttributes(principalAttr(principal), safetyChecksAttr(safetyChecks)))
	if promptTokens == 0 && outputTokens == 0 {
		return
	}
	for _, tokens := range []struct {
		direction string
		count     int64
	}{{"prompt", promptTokens}, {"output", outputTokens}} {
		t.modelTurnTokens.Record(ctx, tokens.count, metric.WithAttributes(
			principalAttr(principal),
			safetyChecksAttr(safetyChecks),
			attribute.String("home_agent.direction", tokens.direction),
		))
	}
}

func (t *Telemetry) RecordToolCall(ctx context.Context, principal, tool, outcome string, duration time.Duration) {
	toolAttr := labelAttr("home_agent.tool", tool, "unknown")
	outcomeAttr := labelAttr("home_agent.outcome", outcome, "unknown")
	t.toolCalls.Add(ctx, 1, metric.WithAttributes(principalAttr(principal), toolAttr, outcomeAttr))
	t.toolDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(toolAttr, outcomeAttr))
}

func (t *Telemetry) RecordToolCallsPerTurn(ctx context.Context, principal, safetyChecks string, calls int) {
	t.toolCallsPerTurn.Record(ctx, int64(calls), metric.WithAttributes(principalAttr(principal), safetyChecksAttr(safetyChecks)))
}

func (t *Telemetry) RecordDayCardRender(ctx context.Context, outcome string, duration time.Duration) {
	t.dayCardRenderDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		labelAttr("home_agent.outcome", outcome, "unknown"),
	))
}

func (t *Telemetry) RecordApproval(ctx context.Context, principal, tool, status string, warned bool) {
	t.approvals.Add(ctx, 1, metric.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.tool", tool, "unknown"),
		labelAttr("home_agent.status", status, "unknown"),
		attribute.Bool("home_agent.warned", warned),
	))
}

func (t *Telemetry) RecordJudgmentDecision(ctx context.Context, principal, purpose, result string) {
	t.judgmentDecisions.Add(ctx, 1, metric.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.purpose", purpose, "unknown"),
		labelAttr("home_agent.result", result, "unknown"),
	))
}

func (t *Telemetry) RecordHintFollowThrough(ctx context.Context, principal, result string) {
	t.hintFollowThrough.Add(ctx, 1, metric.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.result", result, "unknown"),
	))
}

func (t *Telemetry) RecordConversationResume(ctx context.Context, principal, trigger string) {
	t.conversationResumes.Add(ctx, 1, metric.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.trigger", trigger, "unknown"),
	))
}

func (t *Telemetry) RecordConversationSearch(ctx context.Context, principal, outcome string) {
	t.conversationSearches.Add(ctx, 1, metric.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.outcome", outcome, "unknown"),
	))
}

func (t *Telemetry) RecordScheduleRun(ctx context.Context, principal, outcome string) {
	t.scheduleRuns.Add(ctx, 1, metric.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.outcome", outcome, "unknown"),
	))
}

func (t *Telemetry) RecordMemoryWrite(ctx context.Context, principal, kind, outcome string) {
	t.memoryWrites.Add(ctx, 1, metric.WithAttributes(
		principalAttr(principal),
		labelAttr("home_agent.kind", kind, "unknown"),
		labelAttr("home_agent.outcome", outcome, "unknown"),
	))
}

func (t *Telemetry) StartStore(ctx context.Context, operation string) (context.Context, func(error)) {
	ctx, span := t.tracer.Start(ctx, "home_agent.store.operation",
		trace.WithAttributes(attribute.String("home_agent.operation", operation)),
	)
	started := time.Now()
	return ctx, func(err error) {
		outcome := outcomeForError(err)
		finishSpan(span, outcome, err)
		attributes := metric.WithAttributes(
			attribute.String("home_agent.operation", operation),
			attribute.String("home_agent.outcome", outcome),
		)
		t.storeOperations.Add(ctx, 1, attributes)
		t.storeOperationDuration.Record(ctx, time.Since(started).Seconds(), attributes)
	}
}

func labelOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func labelAttr(key, value, fallback string) attribute.KeyValue {
	return attribute.String(key, labelOr(value, fallback))
}

func principalAttr(principal string) attribute.KeyValue {
	return labelAttr("home_agent.principal", principal, "unknown")
}

func safetyChecksAttr(safetyChecks string) attribute.KeyValue {
	return labelAttr("home_agent.safety_checks", safetyChecks, "unavailable")
}

func outcomeForError(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

func finishSpan(span trace.Span, outcome string, err error) {
	span.SetAttributes(attribute.String("home_agent.outcome", outcome))
	if err != nil {
		span.RecordError(err)
	}
	if err != nil || outcome == "error" || outcome == "failed" {
		span.SetStatus(codes.Error, "operation failed")
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

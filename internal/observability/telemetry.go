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
	storeOperations              metric.Int64Counter
	storeOperationDuration       metric.Float64Histogram
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

	telemetry, err := newTelemetry(
		tracerProvider.Tracer(instrumentationName),
		meterProvider.Meter(instrumentationName),
	)
	if err != nil {
		_ = errors.Join(tracerProvider.Shutdown(ctx), meterProvider.Shutdown(ctx))
		return nil, err
	}
	telemetry.enabled = true
	telemetry.tracerProvider = tracerProvider
	telemetry.meterProvider = meterProvider

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {
		logger.Warn("telemetry.export.failed")
	}))

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
	); err != nil {
		return nil, fmt.Errorf("create conversation turn duration histogram: %w", err)
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
	); err != nil {
		return nil, fmt.Errorf("create store operation duration histogram: %w", err)
	}
	if err := registerRuntimeMetrics(meter); err != nil {
		return nil, err
	}

	return t, nil
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

func (t *Telemetry) StartConversationTurn(ctx context.Context) (context.Context, func(string, string)) {
	ctx, span := t.tracer.Start(ctx, "home_agent.conversation.turn")
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
		)
		t.conversationTurns.Add(ctx, 1, attributes)
		t.conversationTurnDuration.Record(ctx, time.Since(started).Seconds(), attributes)
	}
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

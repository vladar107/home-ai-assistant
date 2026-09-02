package telegram

import (
	"context"
	"log/slog"
)

type Telemetry interface {
	ContextLogger(context.Context, *slog.Logger) *slog.Logger
	RecordTelegramRetry(context.Context, string)
	StartStore(context.Context, string) (context.Context, func(error))
	StartTelegramAPI(context.Context, string) (context.Context, func(error))
	StartTelegramUpdate(context.Context) (context.Context, func(string, string, string, error))
}

type noopTelemetry struct{}

func (noopTelemetry) ContextLogger(_ context.Context, logger *slog.Logger) *slog.Logger {
	return logger
}

func (noopTelemetry) RecordTelegramRetry(context.Context, string) {}

func (noopTelemetry) StartStore(ctx context.Context, _ string) (context.Context, func(error)) {
	return ctx, func(error) {}
}

func (noopTelemetry) StartTelegramAPI(ctx context.Context, _ string) (context.Context, func(error)) {
	return ctx, func(error) {}
}

func (noopTelemetry) StartTelegramUpdate(ctx context.Context) (context.Context, func(string, string, string, error)) {
	return ctx, func(string, string, string, error) {}
}

func telemetryOrNoop(telemetry Telemetry) Telemetry {
	if telemetry == nil {
		return noopTelemetry{}
	}
	return telemetry
}

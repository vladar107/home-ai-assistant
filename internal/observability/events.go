package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

func (t *Telemetry) createEventInstruments(meter metric.Meter) error {
	var err error
	if t.inboundEvents, err = meter.Int64Counter("home_agent.events.inbound",
		metric.WithDescription("Events the users' own systems posted that ended, by mode and outcome; source names are never labels."), metric.WithUnit("{event}")); err != nil {
		return fmt.Errorf("create home_agent.events.inbound counter: %w", err)
	}
	return nil
}

func (t *Telemetry) RecordInboundEvent(ctx context.Context, principal, mode, outcome string) {
	t.inboundEvents.Add(ctx, 1, metric.WithAttributes(principalAttr(principal),
		labelAttr("home_agent.mode", mode, "unknown"), labelAttr("home_agent.outcome", outcome, "unknown")))
}

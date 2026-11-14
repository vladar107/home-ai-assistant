package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type flightInstruments struct {
	lookups metric.Int64Counter
	units   metric.Int64Gauge
}

func (t *Telemetry) createFlightInstruments(meter metric.Meter) error {
	var err error
	if t.flights.lookups, err = meter.Int64Counter("home_agent.flights.lookups",
		metric.WithDescription("Flight status lookups, by purpose (tool or tracking) and outcome; cached ones cost nothing."), metric.WithUnit("{lookup}")); err != nil {
		return fmt.Errorf("create home_agent.flights.lookups counter: %w", err)
	}
	if t.flights.units, err = meter.Int64Gauge("home_agent.flights.units",
		metric.WithDescription("The current quota period's flight status units, as used and as budgeted."), metric.WithUnit("{unit}")); err != nil {
		return fmt.Errorf("create home_agent.flights.units gauge: %w", err)
	}
	return nil
}

func (t *Telemetry) RecordFlightLookup(ctx context.Context, purpose, outcome string) {
	t.flights.lookups.Add(ctx, 1, metric.WithAttributes(labelAttr("home_agent.purpose", purpose, "unknown"), labelAttr("home_agent.outcome", outcome, "unknown")))
}

func (t *Telemetry) RecordFlightUnits(ctx context.Context, used, budget int64) {
	t.flights.units.Record(ctx, used, metric.WithAttributes(attribute.String("home_agent.kind", "used")))
	t.flights.units.Record(ctx, budget, metric.WithAttributes(attribute.String("home_agent.kind", "budget")))
}

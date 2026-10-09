package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

type watchInstruments struct {
	checks         metric.Int64Counter
	alerts         metric.Int64Counter
	upstreamErrors metric.Int64Counter
	rewrites       metric.Int64Counter
}

func (t *Telemetry) createWatchInstruments(meter metric.Meter) error {
	var err error
	if t.watches.checks, err = meter.Int64Counter("home_agent.watch.checks",
		metric.WithDescription("Watch checks, by kind and outcome."), metric.WithUnit("{check}")); err != nil {
		return fmt.Errorf("create home_agent.watch.checks counter: %w", err)
	}
	if t.watches.alerts, err = meter.Int64Counter("home_agent.watch.alerts",
		metric.WithDescription("Watch messages ended, by kind, type (alert, recovered, or failure notice), and outcome."), metric.WithUnit("{message}")); err != nil {
		return fmt.Errorf("create home_agent.watch.alerts counter: %w", err)
	}
	if t.watches.upstreamErrors, err = meter.Int64Counter("home_agent.watch.upstream_errors",
		metric.WithDescription("Watch checks that failed because an outside service did, by kind and service."), metric.WithUnit("{error}")); err != nil {
		return fmt.Errorf("create home_agent.watch.upstream_errors counter: %w", err)
	}
	if t.watches.rewrites, err = meter.Int64Counter("home_agent.watch.rewrites",
		metric.WithDescription("Watch messages rewritten into the watch's language, by outcome (rewritten, rejected, failed, or timeout)."), metric.WithUnit("{message}")); err != nil {
		return fmt.Errorf("create home_agent.watch.rewrites counter: %w", err)
	}
	return nil
}

// ZeroWatchOutcomes creates each principal's failed checks and failure notices at zero for every
// kind, so increase() sees their first occurrence. The labels must match the ones recorded.
func (t *Telemetry) ZeroWatchOutcomes(ctx context.Context, principals, kinds []string) {
	for _, principal := range principals {
		for _, kind := range kinds {
			t.watches.checks.Add(ctx, 0, metric.WithAttributes(principalAttr(principal),
				labelAttr("home_agent.kind", kind, "unknown"), labelAttr("home_agent.outcome", "failed", "unknown")))
			t.watches.alerts.Add(ctx, 0, metric.WithAttributes(principalAttr(principal),
				labelAttr("home_agent.kind", kind, "unknown"), labelAttr("home_agent.type", "failure", "unknown"),
				labelAttr("home_agent.outcome", "delivered", "unknown")))
		}
	}
}

func (t *Telemetry) RecordWatchCheck(ctx context.Context, principal, kind, outcome string) {
	t.watches.checks.Add(ctx, 1, metric.WithAttributes(principalAttr(principal),
		labelAttr("home_agent.kind", kind, "unknown"), labelAttr("home_agent.outcome", outcome, "unknown")))
}

func (t *Telemetry) RecordWatchAlert(ctx context.Context, principal, kind, alertType, outcome string) {
	t.watches.alerts.Add(ctx, 1, metric.WithAttributes(principalAttr(principal),
		labelAttr("home_agent.kind", kind, "unknown"), labelAttr("home_agent.type", alertType, "unknown"),
		labelAttr("home_agent.outcome", outcome, "unknown")))
}

func (t *Telemetry) RecordWatchUpstreamError(ctx context.Context, kind, service string) {
	t.watches.upstreamErrors.Add(ctx, 1, metric.WithAttributes(
		labelAttr("home_agent.kind", kind, "unknown"), labelAttr("home_agent.service", service, "unknown")))
}

func (t *Telemetry) RecordWatchRewrite(ctx context.Context, principal, outcome string) {
	t.watches.rewrites.Add(ctx, 1, metric.WithAttributes(principalAttr(principal),
		labelAttr("home_agent.outcome", outcome, "unknown")))
}

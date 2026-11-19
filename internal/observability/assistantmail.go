package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

type assistantMailInstruments struct {
	polls           metric.Int64Counter
	messages        metric.Int64Counter
	refreshFailures metric.Int64Counter
}

func (t *Telemetry) createAssistantMailInstruments(meter metric.Meter) error {
	var err error
	if t.assistantMail.polls, err = meter.Int64Counter("home_agent.assistant_mail.polls",
		metric.WithDescription("Polls of the assistant's own mailbox, by outcome (started, polled, recovered, paused, unavailable, or failed)."), metric.WithUnit("{poll}")); err != nil {
		return fmt.Errorf("create home_agent.assistant_mail.polls counter: %w", err)
	}
	if t.assistantMail.messages, err = meter.Int64Counter("home_agent.assistant_mail.messages",
		metric.WithDescription("New messages in the assistant's own mailbox, by result (routed, unrouted, spam, sent, skipped, or unreadable)."), metric.WithUnit("{message}")); err != nil {
		return fmt.Errorf("create home_agent.assistant_mail.messages counter: %w", err)
	}
	if t.assistantMail.refreshFailures, err = meter.Int64Counter("home_agent.assistant_mail.refresh_failures",
		metric.WithDescription("Failed access token refreshes of the assistant's own mailbox, by reason (rejected, wrong_account, or failed)."), metric.WithUnit("{failure}")); err != nil {
		return fmt.Errorf("create home_agent.assistant_mail.refresh_failures counter: %w", err)
	}
	return nil
}

func (t *Telemetry) RecordAssistantMailPoll(ctx context.Context, outcome string) {
	t.assistantMail.polls.Add(ctx, 1, metric.WithAttributes(labelAttr("home_agent.outcome", outcome, "unknown")))
}

func (t *Telemetry) RecordAssistantMailMessage(ctx context.Context, result string) {
	t.assistantMail.messages.Add(ctx, 1, metric.WithAttributes(labelAttr("home_agent.result", result, "unknown")))
}

func (t *Telemetry) RecordAssistantMailRefreshFailure(ctx context.Context, reason string) {
	t.assistantMail.refreshFailures.Add(ctx, 1, metric.WithAttributes(labelAttr("home_agent.reason", reason, "unknown")))
}

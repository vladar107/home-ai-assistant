package telegram

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vladar107/home-ai-assistant/internal/judgment"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

func approvalsRecorded(t *testing.T, reader sdkmetric.Reader, status string, warned bool) int64 {
	t.Helper()
	value := "false"
	if warned {
		value = "true"
	}
	return metricTotal(t, reader, "home_agent.approvals", map[string]string{
		"home_agent.principal": "alice", "home_agent.tool": "notion_create_page", "home_agent.status": status, "home_agent.warned": value,
	})
}

// Serial: waits out a 2 s approval lifetime with a 100 ms sweep.
func TestApprovalLifecycleIsRecordedOncePerResolution(t *testing.T) {
	telemetry, reader := newMetricsTelemetry(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: writableNotionSimulator(t), approvalLifetime: 2 * time.Second, approvalSweep: 100 * time.Millisecond})
	h.provision("alice", "acct-alice-1111")
	h.installNotionToken("alice", aliceNotionToken)
	h.approvals.SetTelemetry(telemetry)
	h.start()
	h.enableNotionWrites(1, 2, 101)

	const arguments = `{"parent_type":"data_source","parent_id":"ds-tasks","title":"Buy milk"}`
	executed := h.proposeTurn(runtime, 3, "notion_create_page", arguments)
	confirm, _ := approvalButtons(t, executed.keyboard)
	h.push(callbackUpdate(4, 101, executed.messageID, confirm))
	h.wantAnswer(4)
	h.nextEdit()
	h.continueTurn(runtime, 4)
	h.push(callbackUpdate(5, 101, executed.messageID, confirm))
	h.wantAnswer(5)
	h.nextEdit()

	rejected := h.proposeTurn(runtime, 6, "notion_create_page", arguments)
	_, reject := approvalButtons(t, rejected.keyboard)
	h.push(callbackUpdate(7, 101, rejected.messageID, reject))
	h.wantAnswer(7)
	h.nextEdit()

	stub := newTypeSafeStub(t, 0.1, http.StatusOK)
	client, err := judgment.NewClient(judgment.Config{
		APIKey: "test-key", Endpoint: stub.server.URL, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Telemetry: telemetry,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := turnstate.New(time.Now)
	registry.RecordMessage("alice", 0, "hello")
	h.approvals.SetIntentChecker(judgment.NewIntentChecker(client, stubOptIn(true), registry, registry))
	expiring := h.proposeTurn(runtime, 8, "notion_create_page", arguments)
	if edit := h.nextEdit(); edit.messageID != expiring.messageID {
		t.Fatalf("expiry edit = %+v", edit)
	}
	h.push(callbackUpdate(9, 101, expiring.messageID, confirm))
	h.wantAnswer(9)
	h.nextEdit()

	if got := approvalsRecorded(t, reader, "proposed", false); got != 3 {
		t.Errorf("proposed = %d, want 3", got)
	}
	for _, want := range []struct {
		status string
		warned bool
	}{{"executed", false}, {"rejected", false}, {"expired", true}} {
		if got := approvalsRecorded(t, reader, want.status, want.warned); got != 1 {
			t.Errorf("%s warned=%v = %d, want 1", want.status, want.warned, got)
		}
	}
	if got := metricTotal(t, reader, "home_agent.approvals", map[string]string{"home_agent.status": "failed"}); got != 0 {
		t.Errorf("failed = %d, want 0", got)
	}
	if got := metricTotal(t, reader, "home_agent.judgment.decisions", map[string]string{"home_agent.principal": "alice", "home_agent.purpose": "intent", "home_agent.result": "warned"}); got != 1 {
		t.Errorf("intent warned decisions = %d, want 1", got)
	}
}

func TestScheduledRunRecordsItsOutcomeAndReplyTime(t *testing.T) {
	t.Parallel()
	telemetry, reader := newMetricsTelemetry(t)
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.scheduler.SetTelemetry(telemetry)
	h.start()

	job := h.createJob("alice", "0123456789abcdef", "Weekly tasks", "0 9 * * 1", "List my overdue Notion tasks.")
	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	run.answer <- agentruntime.RunResult{Response: "Two tasks are overdue.", Final: "Two tasks are overdue."}
	h.wantMessage(101, "<b>Scheduled: Weekly tasks</b>\n\nTwo tasks are overdue.")
	h.waitRun(job.ID, schedule.StatusDelivered)

	deadline := time.After(5 * time.Second)
	for metricTotal(t, reader, "home_agent.schedule.runs", map[string]string{"home_agent.outcome": "delivered"}) == 0 {
		select {
		case <-deadline:
			t.Fatal("no delivered schedule run was recorded")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := metricTotal(t, reader, "home_agent.schedule.runs", map[string]string{"home_agent.principal": "alice", "home_agent.outcome": "delivered"}); got != 1 {
		t.Errorf("schedule.runs delivered = %d, want 1", got)
	}
	if got := metricTotal(t, reader, "home_agent.reply.duration", map[string]string{"home_agent.principal": "alice", "home_agent.kind": "scheduled", "home_agent.outcome": "delivered"}); got != 1 {
		t.Errorf("scheduled reply.duration = %d, want 1", got)
	}
}

// metricTotal sums a counter's value, or a histogram's sample count, over the points whose
// attributes include every wanted label.
func metricTotal(t *testing.T, reader sdkmetric.Reader, name string, want map[string]string) int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	matches := func(set attribute.Set) bool {
		for key, value := range want {
			got, ok := set.Value(attribute.Key(key))
			if !ok || got.String() != value {
				return false
			}
		}
		return true
	}
	var total int64
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					if matches(p.Attributes) {
						total += p.Value
					}
				}
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					if matches(p.Attributes) {
						total += int64(p.Count)
					}
				}
			}
		}
	}
	return total
}

func newMetricsTelemetry(t *testing.T) (*observability.Telemetry, sdkmetric.Reader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	telemetry, err := observability.NewWithReader(reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = telemetry.Shutdown(context.Background()) })
	return telemetry, reader
}

package conversation_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type sizedProvider struct {
	name     string
	guidance string
}

func (p sizedProvider) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{Guidance: p.guidance, Definitions: []tools.Definition{{
		Name:        p.name,
		Description: "A test tool.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Risk:        tools.RiskRead,
	}}}, nil
}

func (sizedProvider) Execute(context.Context, string, tools.Call) (tools.Result, error) {
	return tools.Result{Text: "ok"}, nil
}

type switchedSizedProvider struct {
	sizedProvider
	sw integration.Switch
}

func (p switchedSizedProvider) Switch() integration.Switch { return p.sw }

// unlinkedProvider is switched on but offers nothing, as a connector without an account does.
type unlinkedProvider struct{ sw integration.Switch }

func (unlinkedProvider) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{}, nil
}

func (unlinkedProvider) Execute(context.Context, string, tools.Call) (tools.Result, error) {
	return tools.Result{}, nil
}

func (p unlinkedProvider) Switch() integration.Switch { return p.sw }

func (e *metricsEnv) brokerWith(t *testing.T, providers ...tools.Provider) *tools.Broker {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	switches, err := integration.NewSwitches(e.store, logger)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := tools.NewBroker(logger)
	if err != nil {
		t.Fatal(err)
	}
	broker.SetSwitches(switches)
	for _, provider := range providers {
		if err := broker.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	return broker
}

func TestChatTurnRecordsOfferedToolBytesBySource(t *testing.T) {
	env := newMetricsEnv(t)
	trains := integration.Switch{Integration: "trains", Name: "Train information", DefaultOn: true}
	broker := env.brokerWith(t,
		sizedProvider{name: "core_tool", guidance: "Core guidance."},
		switchedSizedProvider{sizedProvider{name: "train_times"}, trains},
		unlinkedProvider{integration.Switch{Integration: "mailbox", Name: "Mailbox", DefaultOn: true}},
	)
	service := env.serviceWithTools(t, answering("done", agentruntime.Usage{ModelCalls: 1}), broker, conversation.Config{})
	env.enqueue(t, service, 1)
	env.awaitReply(t)

	attrs := func(part, source string) map[string]string {
		return map[string]string{"home_agent.principal": "alice", "home_agent.part": part, "home_agent.source": source}
	}
	for _, tc := range []struct {
		part, source string
		want         int
	}{
		{"definitions", "builtin", len(`{"name":"core_tool","description":"A test tool.","parameters":{"type":"object"}}`)},
		{"guidance", "builtin", len("Core guidance.")},
		{"definitions", "trains", len(`{"name":"train_times","description":"A test tool.","parameters":{"type":"object"}}`)},
	} {
		got := env.awaitObserved(t, "home_agent.tool.offered_bytes", attrs(tc.part, tc.source))
		if got.count != 1 || got.sum != float64(tc.want) {
			t.Errorf("%s of %s = %+v, want one point of %d bytes", tc.part, tc.source, got, tc.want)
		}
	}
	if got := env.observed(t, "home_agent.tool.offered_bytes", attrs("guidance", "trains")); got.count != 0 {
		t.Errorf("guidance recorded for a source without any: %+v", got)
	}
	if got := env.observed(t, "home_agent.tool.offered_bytes", attrs("definitions", "mailbox")); got.count != 0 {
		t.Errorf("a source that offered no tools was recorded: %+v", got)
	}
}

func TestChatTurnWithoutToolsRecordsNoOfferedToolBytes(t *testing.T) {
	env := newMetricsEnv(t)
	service := env.serviceWithTools(t, answering("done", agentruntime.Usage{ModelCalls: 1}), env.brokerWith(t), conversation.Config{})
	env.enqueue(t, service, 1)
	env.awaitReply(t)
	env.awaitObserved(t, "home_agent.model.calls_per_turn", nil)

	if got := env.observed(t, "home_agent.tool.offered_bytes", nil); got.count != 0 {
		t.Errorf("offered bytes recorded for a turn without tools: %+v", got)
	}
}

package conversation_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/turn"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

type scriptedRuntime struct {
	run func(context.Context, agentruntime.RunRequest, agentruntime.EventSink) (agentruntime.RunResult, error)
}

func (r scriptedRuntime) Run(ctx context.Context, request agentruntime.RunRequest, sink agentruntime.EventSink) (agentruntime.RunResult, error) {
	return r.run(ctx, request, sink)
}

type fixedHint struct {
	name     string
	answered bool
}

func (f fixedHint) Hint(context.Context, string, string, []turnstate.Line, []agentruntime.ToolDefinition) (string, bool, error) {
	return f.name, f.answered, nil
}

type safetyStatus struct {
	enabled bool
	err     error
}

func (s safetyStatus) JudgmentsEnabled(context.Context, string) (bool, error) {
	return s.enabled, s.err
}

type metricsEnv struct {
	ctx       context.Context
	store     *sqlite.Store
	reader    sdkmetric.Reader
	telemetry *observability.Telemetry
	replies   chan string
	// engine is the one the latest service runs its turns through.
	engine *turn.Engine
}

func newMetricsEnv(t *testing.T) *metricsEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	identities, err := identity.NewService(store, []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}})
	if err != nil {
		t.Fatal(err)
	}
	if err := identities.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	reader := sdkmetric.NewManualReader()
	telemetry, err := observability.NewWithReader(reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = telemetry.Shutdown(context.Background()) })
	return &metricsEnv{ctx: ctx, store: store, reader: reader, telemetry: telemetry, replies: make(chan string, 4)}
}

func (e *metricsEnv) service(t *testing.T, runtime agentruntime.AgentRuntime, config conversation.Config) *conversation.Service {
	t.Helper()
	config.TurnTimeout = 10 * time.Second
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.engine = testEngine(t, runtime, hintTools{names: []string{"nas_search", "gmail_search"}}, logger, e.telemetry)
	config.Engine = e.engine
	service, err := conversation.NewService(config, e.store, hintDeliverer{replies: e.replies}, logger, e.telemetry)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return service
}

func (e *metricsEnv) enqueue(t *testing.T, service *conversation.Service, update int64) {
	t.Helper()
	if err := service.Enqueue(e.ctx, conversation.EnqueueInput{
		BotUserID: 999, TelegramUpdateID: update, TelegramMessageID: update + 1, PrincipalID: "alice", ChatID: 101, Message: "what is on my NAS",
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *metricsEnv) awaitReply(t *testing.T) {
	t.Helper()
	select {
	case <-e.replies:
	case <-e.ctx.Done():
		t.Fatal("reply was never delivered")
	}
}

type observation struct {
	count uint64
	sum   float64
}

// observed sums the points of one metric whose attributes include all of want.
func (e *metricsEnv) observed(t *testing.T, name string, want map[string]string) observation {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := e.reader.Collect(e.ctx, &collected); err != nil {
		t.Fatal(err)
	}
	var total observation
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					if attributesMatch(p.Attributes, want) {
						total.count += p.Count
						total.sum += p.Sum
					}
				}
			case metricdata.Histogram[int64]:
				for _, p := range data.DataPoints {
					if attributesMatch(p.Attributes, want) {
						total.count += p.Count
						total.sum += float64(p.Sum)
					}
				}
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					if attributesMatch(p.Attributes, want) {
						total.count++
						total.sum += float64(p.Value)
					}
				}
			}
		}
	}
	return total
}

// awaitObserved waits for a recording that happens just after the reply is handed to Telegram.
func (e *metricsEnv) awaitObserved(t *testing.T, name string, want map[string]string) observation {
	t.Helper()
	for {
		if got := e.observed(t, name, want); got.count > 0 {
			return got
		}
		select {
		case <-e.ctx.Done():
			t.Fatalf("metric %s %v was never recorded", name, want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func answering(text string, usage agentruntime.Usage) scriptedRuntime {
	return scriptedRuntime{run: func(context.Context, agentruntime.RunRequest, agentruntime.EventSink) (agentruntime.RunResult, error) {
		time.Sleep(20 * time.Millisecond)
		return agentruntime.RunResult{Response: text, Usage: usage}, nil
	}}
}

func TestDeliveredChatTurnRecordsReplyAndStages(t *testing.T) {
	cases := []struct {
		name   string
		status conversation.SafetyChecks
		label  string
	}{
		{"on", safetyStatus{enabled: true}, "on"},
		{"off", safetyStatus{}, "off"},
		{"lookup failed", safetyStatus{err: errors.New("boom")}, "unavailable"},
		{"not configured", nil, "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMetricsEnv(t)
			config := conversation.Config{}
			if tc.status != nil {
				config.SafetyChecks = tc.status
			}
			service := env.service(t, answering("done", agentruntime.Usage{ModelCalls: 2, PromptTokens: 100, OutputTokens: 20}), config)
			env.enqueue(t, service, 1)
			env.awaitReply(t)

			reply := env.awaitObserved(t, "home_agent.reply.duration", map[string]string{
				"home_agent.principal": "alice", "home_agent.kind": "chat", "home_agent.outcome": "delivered", "home_agent.safety_checks": tc.label,
			})
			if reply.count != 1 {
				t.Fatalf("reply count = %d", reply.count)
			}
			var stages float64
			for _, stage := range []string{"queue", "setup", "model", "delivery"} {
				got := env.observed(t, "home_agent.reply.stage.duration", map[string]string{"home_agent.principal": "alice", "home_agent.stage": stage})
				if got.count != 1 || got.sum < 0 {
					t.Errorf("stage %s = %+v", stage, got)
				}
				if stage == "model" && got.sum < 0.02 {
					t.Errorf("model stage %v is shorter than the run", got.sum)
				}
				stages += got.sum
			}
			if stages > reply.sum {
				t.Errorf("stages %v exceed the reply duration %v", stages, reply.sum)
			}

			calls := env.observed(t, "home_agent.model.calls_per_turn", map[string]string{"home_agent.principal": "alice", "home_agent.safety_checks": tc.label})
			if calls.count != 1 || calls.sum != 2 {
				t.Errorf("model calls = %+v", calls)
			}
			prompt := env.observed(t, "home_agent.model.turn_tokens", map[string]string{"home_agent.safety_checks": tc.label, "home_agent.direction": "prompt"})
			output := env.observed(t, "home_agent.model.turn_tokens", map[string]string{"home_agent.safety_checks": tc.label, "home_agent.direction": "output"})
			if prompt.sum != 100 || output.sum != 20 {
				t.Errorf("turn tokens prompt=%v output=%v", prompt.sum, output.sum)
			}
			turns := env.observed(t, "home_agent.conversation.turns", map[string]string{"home_agent.principal": "alice", "home_agent.safety_checks": tc.label})
			if turns.count == 0 {
				t.Error("conversation turn lacks the principal and safety labels")
			}
		})
	}
}

func TestErrorTurnRecordsFailedReplyWithoutStages(t *testing.T) {
	env := newMetricsEnv(t)
	service := env.service(t, scriptedRuntime{run: func(context.Context, agentruntime.RunRequest, agentruntime.EventSink) (agentruntime.RunResult, error) {
		return agentruntime.RunResult{}, errors.New("model exploded")
	}}, conversation.Config{})
	env.enqueue(t, service, 1)
	env.awaitReply(t)

	env.awaitObserved(t, "home_agent.reply.duration", map[string]string{"home_agent.kind": "chat", "home_agent.outcome": "failed"})
	if got := env.observed(t, "home_agent.reply.stage.duration", nil); got.count != 0 {
		t.Errorf("stages recorded for a failed reply: %+v", got)
	}
	for _, name := range []string{"home_agent.model.calls_per_turn", "home_agent.tool.calls_per_turn"} {
		if got := env.observed(t, name, nil); got.count != 0 {
			t.Errorf("%s recorded for a failed run: %+v", name, got)
		}
	}
}

func TestRequestCreatedBeforeStartIsRecovered(t *testing.T) {
	env := newMetricsEnv(t)
	started := make(chan struct{})
	first := env.service(t, scriptedRuntime{run: func(ctx context.Context, _ agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
		close(started)
		<-ctx.Done()
		return agentruntime.RunResult{}, ctx.Err()
	}}, conversation.Config{})
	env.enqueue(t, first, 1)
	select {
	case <-started:
	case <-env.ctx.Done():
		t.Fatal("first turn never started")
	}
	if err := first.Shutdown(env.ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	env.service(t, answering("done", agentruntime.Usage{}), conversation.Config{})
	env.awaitReply(t)

	env.awaitObserved(t, "home_agent.reply.duration", map[string]string{"home_agent.kind": "chat", "home_agent.outcome": "recovered"})
	if got := env.observed(t, "home_agent.reply.duration", map[string]string{"home_agent.outcome": "delivered"}); got.count != 0 {
		t.Errorf("recovered request counted as delivered: %+v", got)
	}
	if got := env.observed(t, "home_agent.reply.stage.duration", nil); got.count != 0 {
		t.Errorf("stages recorded for a recovered reply: %+v", got)
	}
}

func TestToolCallsAndHintFollowThroughAreCountedFromRuntimeEvents(t *testing.T) {
	cases := []struct {
		name   string
		hint   fixedHint
		called []string
		calls  float64
		result string
	}{
		{"followed", fixedHint{"nas_search", true}, []string{"gmail_search", "nas_search"}, 2, "followed"},
		{"other tool", fixedHint{"nas_search", true}, []string{"gmail_search"}, 1, "other_tool"},
		{"no tool", fixedHint{"nas_search", true}, nil, 0, "no_tool"},
		{"missed", fixedHint{"", true}, []string{"gmail_search"}, 1, "missed"},
		{"answered none without a tool call", fixedHint{"", true}, nil, 0, ""},
		{"no answer", fixedHint{}, []string{"nas_search", "web_search", "nas_search"}, 3, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMetricsEnv(t)
			runtime := scriptedRuntime{run: func(ctx context.Context, _ agentruntime.RunRequest, sink agentruntime.EventSink) (agentruntime.RunResult, error) {
				for _, name := range tc.called {
					if err := sink.Emit(ctx, agentruntime.Event{Type: "tool_call", Text: name}); err != nil {
						return agentruntime.RunResult{}, err
					}
				}
				if err := sink.Emit(ctx, agentruntime.Event{Type: "message", Text: "not a tool"}); err != nil {
					return agentruntime.RunResult{}, err
				}
				return agentruntime.RunResult{Response: "done"}, nil
			}}
			service := env.service(t, runtime, conversation.Config{ToolHint: tc.hint})
			env.enqueue(t, service, 1)
			env.awaitReply(t)

			got := env.awaitObserved(t, "home_agent.tool.calls_per_turn", map[string]string{"home_agent.principal": "alice"})
			if got.count != 1 || got.sum != tc.calls {
				t.Errorf("tool calls per turn = %+v, want %v", got, tc.calls)
			}
			for _, result := range []string{"followed", "other_tool", "no_tool", "missed"} {
				follow := env.observed(t, "home_agent.hint.follow_through", map[string]string{"home_agent.principal": "alice", "home_agent.result": result})
				want := 0.0
				if result == tc.result {
					want = 1
				}
				if follow.sum != want {
					t.Errorf("follow_through %s = %v, want %v", result, follow.sum, want)
				}
			}
		})
	}
}

func attributesMatch(set attribute.Set, want map[string]string) bool {
	for key, value := range want {
		got, ok := set.Value(attribute.Key(key))
		if !ok || got.String() != value {
			return false
		}
	}
	return true
}

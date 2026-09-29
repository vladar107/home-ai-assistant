package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const testPrincipal = "principal-1"
const testSchema = `{"type":"object"}`

// fakeProvider grants one principal the definitions it is given and delegates every call to
// respond, so a test can observe the context the broker built for it.
type fakeProvider struct {
	principal   string
	definitions []Definition
	respond     func(context.Context, Call) (Result, error)
}

func (p *fakeProvider) Contribute(_ context.Context, principal string) (Contribution, error) {
	if principal != p.principal {
		return Contribution{}, nil
	}
	return Contribution{Guidance: "test guidance", Definitions: p.definitions}, nil
}

func (p *fakeProvider) Execute(ctx context.Context, _ string, call Call) (Result, error) {
	return p.respond(ctx, call)
}

func testDefinition(name string, timeout time.Duration) Definition {
	return Definition{
		Name:        name,
		Description: "A test tool.",
		InputSchema: json.RawMessage(testSchema),
		Risk:        RiskRead,
		Timeout:     timeout,
	}
}

func testBroker(t *testing.T) *Broker {
	t.Helper()
	broker, err := NewBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

func TestToolsForRejectsAnInvalidTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{"negative", -time.Second},
		{"above maximum", maximumCallDeadline + time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := testBroker(t)
			provider := &fakeProvider{principal: testPrincipal, definitions: []Definition{testDefinition("slow_tool", tc.timeout)}}
			if err := broker.Register(provider); err != nil {
				t.Fatal(err)
			}
			if _, err := broker.ToolsFor(context.Background(), testPrincipal); err == nil {
				t.Fatal("want an error for an invalid timeout")
			}
		})
	}
}

func TestExecuteAppliesThePerToolDeadline(t *testing.T) {
	broker := testBroker(t)
	remaining := make(map[string]time.Duration)
	provider := &fakeProvider{
		principal: testPrincipal,
		definitions: []Definition{
			testDefinition("custom_tool", 60*time.Second),
			testDefinition("default_tool", 0),
		},
		respond: func(ctx context.Context, call Call) (Result, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("call context has no deadline")
			}
			remaining[call.Name] = time.Until(deadline)
			return Result{Text: "ok"}, nil
		},
	}
	if err := broker.Register(provider); err != nil {
		t.Fatal(err)
	}
	set, err := broker.ToolsFor(context.Background(), testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"custom_tool", "default_tool"} {
		if _, err := set.Executor.Execute(context.Background(), agentruntime.ToolCall{Name: name, Arguments: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}

	// Generous tolerance keeps this deterministic on a slow runner without ever sleeping.
	const tolerance = 5 * time.Second
	if diff := remaining["custom_tool"] - 60*time.Second; diff < -tolerance || diff > tolerance {
		t.Errorf("custom_tool deadline remaining = %v, want ~60s", remaining["custom_tool"])
	}
	if diff := remaining["default_tool"] - callDeadline; diff < -tolerance || diff > tolerance {
		t.Errorf("default_tool deadline remaining = %v, want ~%v", remaining["default_tool"], callDeadline)
	}
}

func TestExecuteTimesOutWhenTheProviderOutrunsItsDeadline(t *testing.T) {
	broker := testBroker(t)
	provider := &fakeProvider{
		principal:   testPrincipal,
		definitions: []Definition{testDefinition("slow_tool", 20*time.Millisecond)},
		respond: func(ctx context.Context, _ Call) (Result, error) {
			<-ctx.Done()
			return Result{}, ctx.Err()
		},
	}
	if err := broker.Register(provider); err != nil {
		t.Fatal(err)
	}
	set, err := broker.ToolsFor(context.Background(), testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	result, err := set.Executor.Execute(context.Background(), agentruntime.ToolCall{Name: "slow_tool", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Text != timedOutText {
		t.Errorf("result = %+v, want the timed-out text", result)
	}
}

// The runtime may call a tool on a context of its own, so the turn key collected with the tools
// is what the provider must see.
func TestExecuteCarriesTheTurnKeyToTheProvider(t *testing.T) {
	broker := testBroker(t)
	seen := make(chan string, 1)
	provider := &fakeProvider{
		principal:   testPrincipal,
		definitions: []Definition{testDefinition("turn_tool", 0)},
		respond: func(ctx context.Context, _ Call) (Result, error) {
			seen <- TurnFrom(ctx)
			return Result{Text: "ok"}, nil
		},
	}
	if err := broker.Register(provider); err != nil {
		t.Fatal(err)
	}
	set, err := broker.ReadOnlyToolsFor(WithTurn(context.Background(), "turn-1"), testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Executor.Execute(context.Background(), agentruntime.ToolCall{Name: "turn_tool", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if turn := <-seen; turn != "turn-1" {
		t.Fatalf("provider saw turn %q, want turn-1", turn)
	}
}

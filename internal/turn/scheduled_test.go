package turn

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type recordingRuntime struct {
	requests []agentruntime.RunRequest
	// called keeps the result of the one tool call a run makes while call names a tool.
	call   string
	called []string
}

func (r *recordingRuntime) Run(ctx context.Context, request agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
	r.requests = append(r.requests, request)
	if r.call != "" {
		// The runtime calls tools back on a context of its own, as the Pi bridge does.
		result, err := request.Tools.Executor.Execute(context.Background(), agentruntime.ToolCall{ID: "c1", Name: r.call, Arguments: json.RawMessage(`{}`)})
		if err != nil {
			return agentruntime.RunResult{}, err
		}
		r.called = append(r.called, result.Text)
	}
	return agentruntime.RunResult{Final: "done"}, nil
}

type readyGate struct{}

func (readyGate) Ready(context.Context, string) (bool, error) { return true, nil }

type quietTelemetry struct{}

func (quietTelemetry) ContextLogger(_ context.Context, logger *slog.Logger) *slog.Logger {
	return logger
}

type fixedMemory struct{}

func (fixedMemory) Render(context.Context, string) (string, error) {
	return "The user lives at a private address.", nil
}
func (fixedMemory) TakeNotes(string, string) []string { return nil }

type webOn struct{}

func (webOn) WebSearchEnabled(context.Context, string) (bool, error) { return true, nil }

type collectionLine struct{}

func (collectionLine) TurnLine(context.Context, string) (string, error) { return "Recipes", nil }

type scopedProvider struct {
	names []string
}

type boundKey struct{}

func (p scopedProvider) Contribute(context.Context, string) (tools.Contribution, error) {
	var definitions []tools.Definition
	for _, name := range p.names {
		definitions = append(definitions, tools.Definition{Name: name, Description: "A test tool.", InputSchema: json.RawMessage(`{"type":"object"}`), Risk: tools.RiskRead})
	}
	return tools.Contribution{Guidance: "guidance of " + p.names[0], Definitions: definitions}, nil
}

func (p scopedProvider) Execute(ctx context.Context, _ string, call tools.Call) (tools.Result, error) {
	bound, _ := ctx.Value(boundKey{}).(string)
	return tools.Result{Text: call.Name + " bound to " + bound}, nil
}

// A confined check is offered only its scope's tools, bound to the scope, with no web search,
// memory, place, or collections; a plain check is offered everything, as before.
func TestARunConfinedToAScopeSeesOnlyItsTools(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker, err := tools.NewBroker(logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []tools.Provider{scopedProvider{names: []string{"mail_list", "mail_read"}}, scopedProvider{names: []string{"notes_read"}}} {
		if err := broker.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	runtime := &recordingRuntime{call: "mail_read"}
	engine, err := New(Config{
		TurnTimeout: time.Minute, Memory: fixedMemory{}, WebSearch: webOn{}, Collections: collectionLine{},
	}, runtime, readyGate{}, broker, logger, quietTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	scope := tools.Scope{Tools: []string{"mail_list", "mail_read"}, Bind: func(ctx context.Context) context.Context {
		return context.WithValue(ctx, boundKey{}, "thread-1")
	}}
	if reply, err := engine.RunConfined(t.Context(), "alice", "Triage the thread.", scope); err != nil || reply.ErrorKind != "" {
		t.Fatalf("confined run = %+v %v", reply, err)
	}
	runtime.call = ""
	if reply, err := engine.RunCheck(t.Context(), "alice", "Check the thread."); err != nil || reply.ErrorKind != "" {
		t.Fatalf("check = %+v %v", reply, err)
	}

	offered := func(request agentruntime.RunRequest) []string {
		var names []string
		for _, definition := range request.Tools.Definitions {
			names = append(names, definition.Name)
		}
		return names
	}
	confined, check := runtime.requests[0], runtime.requests[1]
	if got := offered(confined); !slices.Equal(got, []string{"mail_list", "mail_read"}) {
		t.Fatalf("confined tools = %v", got)
	}
	if confined.WebSearch || confined.Memory != "" || strings.Contains(confined.Message, "Recipes") || strings.Contains(confined.Tools.Guidance, "notes_read") {
		t.Fatalf("the confined run reaches beyond its scope: %+v", confined)
	}
	if len(runtime.called) != 1 || !strings.HasSuffix(runtime.called[0], "mail_read bound to thread-1") {
		t.Fatalf("confined calls = %v", runtime.called)
	}
	if got := offered(check); !slices.Equal(got, []string{"mail_list", "mail_read", "notes_read"}) || !check.WebSearch || check.Memory == "" ||
		!strings.Contains(check.Message, "Recipes") {
		t.Fatalf("plain check = tools %v, %+v", got, check)
	}
}

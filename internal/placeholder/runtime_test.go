package placeholder_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/placeholder"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// callingRuntime makes one tool call with the given arguments and answers with its result.
type callingRuntime struct {
	arguments json.RawMessage
}

func (r callingRuntime) Run(ctx context.Context, request agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
	result, err := request.Tools.Executor.Execute(ctx, agentruntime.ToolCall{Name: "echo", Arguments: r.arguments})
	if err != nil {
		return agentruntime.RunResult{}, err
	}
	return agentruntime.RunResult{Response: result.Text, Final: result.Text}, nil
}

type countingExecutor struct{ calls int }

func (e *countingExecutor) Execute(context.Context, agentruntime.ToolCall) (agentruntime.ToolResult, error) {
	e.calls++
	return agentruntime.ToolResult{Text: "done"}, nil
}

func TestUndecodableArgumentsFailTheCallNotTheRun(t *testing.T) {
	t.Parallel()
	for _, arguments := range []string{`{"text":`, `{"a":1} {"b":2}`} {
		inner := &countingExecutor{}
		runtime := placeholder.NewRuntime(callingRuntime{arguments: json.RawMessage(arguments)}, newVault(), true, nil)
		result, err := runtime.Run(t.Context(), agentruntime.RunRequest{PrincipalID: "alice", Tools: agentruntime.ToolSet{Executor: inner}}, nil)
		if err != nil || result.Response != "The tool arguments could not be read; send them as one JSON object." || inner.calls != 0 {
			t.Fatalf("arguments %s: result %+v, error %v, tool calls %d", arguments, result, err, inner.calls)
		}
	}
}

// guidanceRuntime answers with the tool guidance it was given.
type guidanceRuntime struct{}

func (guidanceRuntime) Run(_ context.Context, request agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
	return agentruntime.RunResult{Final: request.Tools.Guidance}, nil
}

func TestHidingTellsTheModelToPassPlaceholdersAsWritten(t *testing.T) {
	t.Parallel()
	tools := agentruntime.ToolSet{
		Definitions: []agentruntime.ToolDefinition{{Name: "echo"}},
		Guidance:    "Use echo.",
		Executor:    &countingExecutor{},
	}
	for _, hide := range []bool{true, false} {
		result, err := placeholder.NewRuntime(guidanceRuntime{}, newVault(), hide, nil).Run(t.Context(), agentruntime.RunRequest{PrincipalID: "alice", Tools: tools}, nil)
		if err != nil {
			t.Fatal(err)
		}
		told := strings.Contains(result.Final, "Pass a placeholder to a tool exactly as written")
		if !strings.HasPrefix(result.Final, "Use echo.") || told != hide {
			t.Errorf("hide %v: guidance %q", hide, result.Final)
		}
	}
}

func TestAVaultErrorWhileResolvingFailsTheRun(t *testing.T) {
	t.Parallel()
	vault := newVault()
	vault.fail = errors.New("vault unavailable")
	inner := &countingExecutor{}
	runtime := placeholder.NewRuntime(callingRuntime{arguments: json.RawMessage(`{"text":"email-abcdef"}`)}, vault, false, nil)
	if _, err := runtime.Run(t.Context(), agentruntime.RunRequest{PrincipalID: "alice", Tools: agentruntime.ToolSet{Executor: inner}}, nil); err == nil || inner.calls != 0 {
		t.Fatalf("error = %v, tool calls %d, want the run failed before the tool", err, inner.calls)
	}
}

// searchingRuntime asks the run's search check about each query and answers with the verdicts.
type searchingRuntime struct {
	queries []string
}

func (r searchingRuntime) Run(ctx context.Context, request agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
	var verdicts []string
	for _, query := range r.queries {
		decision := request.SearchCheck(ctx, query)
		verdicts = append(verdicts, string(decision.Verdict)+": "+decision.Text)
	}
	return agentruntime.RunResult{Response: strings.Join(verdicts, "\n")}, nil
}

func TestSearchesHoldingPlaceholdersAreDeniedBeforeTheCheck(t *testing.T) {
	t.Parallel()
	vault := newVault()
	vault.seed("alice", placeholder.Email, "jane@example.com", "k3f7qa")
	queries := []string{"offers from ⟦email-k3f7qa example.com⟧", "offers from email-k3f7qa", "offers from ⟦email-k3f7", "offers in Springfield"}
	var checked []string
	check := func(_ context.Context, query string) agentruntime.SearchDecision {
		checked = append(checked, query)
		return agentruntime.SearchDecision{Verdict: agentruntime.SearchAllow}
	}
	runtime := placeholder.NewRuntime(searchingRuntime{queries: queries}, vault, true, nil)
	result, err := runtime.Run(t.Context(), agentruntime.RunRequest{PrincipalID: "alice", SearchCheck: check}, nil)
	if err != nil {
		t.Fatal(err)
	}
	denied := "deny: The search was not run: its query holds a placeholder, and hidden values cannot be searched. Search without them."
	if want := strings.Join([]string{denied, denied, denied, "allow: "}, "\n"); result.Response != want {
		t.Fatalf("verdicts =\n%s\nwant\n%s", result.Response, want)
	}
	if len(checked) != 1 || checked[0] != "offers in Springfield" {
		t.Fatalf("the check was asked about %q, want only the query without a placeholder", checked)
	}
}

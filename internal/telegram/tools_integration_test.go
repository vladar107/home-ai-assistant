package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// grantProvider contributes one read tool to a single principal and records executions; a
// failing contribution simulates a connector that cannot report its grant.
type grantProvider struct {
	principal string
	mu        sync.Mutex
	fail      bool
	executed  []string
}

func (p *grantProvider) Contribute(_ context.Context, principal string) (tools.Contribution, error) {
	p.mu.Lock()
	fail := p.fail
	p.mu.Unlock()
	if fail {
		return tools.Contribution{}, errors.New("synthetic-private-text")
	}
	if principal != p.principal {
		return tools.Contribution{}, nil
	}
	return tools.Contribution{
		Guidance: "Use lookup_note to read the user's notes.",
		Definitions: []tools.Definition{{
			Name: "lookup_note", Description: "Read one note by title.", Risk: tools.RiskRead,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`),
		}},
	}, nil
}

func (p *grantProvider) Execute(_ context.Context, principal string, call tools.Call) (tools.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.executed = append(p.executed, principal+":"+call.Name+":"+string(call.Arguments))
	return tools.Result{Text: "note body secret-note-content"}, nil
}

func (p *grantProvider) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.executed...)
}

func TestTurnsCarryOnlyThePrincipalsGrantedTools(t *testing.T) {
	runtime := newControlledRuntime()
	provider := &grantProvider{principal: "alice"}
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{providers: []tools.Provider{provider}})
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()

	h.push(textUpdate(1, 101, "what did I note?"))
	alice := receive(t, h.ctx, runtime.calls)
	set := alice.request.Tools
	if !slices.Equal(toolNames(set), chatTools([]string{"lookup_note"})) || !strings.HasPrefix(set.Guidance, "Use lookup_note to read the user's notes.\n") || set.Executor == nil {
		t.Fatalf("Alice's turn tools = %+v", set)
	}
	result, err := set.Executor.Execute(alice.ctx, agentruntime.ToolCall{Name: "lookup_note", Arguments: json.RawMessage(`{"title":"groceries"}`)})
	if err != nil || result.IsError || !strings.HasSuffix(result.Text, "note body secret-note-content") {
		t.Fatalf("execution = %+v %v", result, err)
	}
	if got := provider.recorded(); len(got) != 1 || got[0] != `alice:lookup_note:{"title":"groceries"}` {
		t.Fatalf("provider executions = %v", got)
	}
	alice.answer <- agentruntime.RunResult{Response: "you noted groceries"}
	h.wantReply(101, 1, "you noted groceries")
	h.waitState(1, "delivered")

	h.push(textUpdate(2, 202, "what did I note?"))
	bob := receive(t, h.ctx, runtime.calls)
	if !onlyCoreTools(bob.request.Tools) {
		t.Fatalf("Bob received Alice's tools: %+v", bob.request.Tools)
	}
	bob.answer <- agentruntime.RunResult{Response: "no notes"}
	h.wantReply(202, 2, "no notes")
	h.waitState(2, "delivered")

	logs := h.logs.String()
	if !strings.Contains(logs, "tools.call.completed") || !strings.Contains(logs, "principal_id=alice") || !strings.Contains(logs, "tool=lookup_note") || !strings.Contains(logs, "outcome=success") {
		t.Fatalf("tool audit log missing: %s", logs)
	}
	if strings.Contains(logs, "secret-note-content") || strings.Contains(logs, "groceries") {
		t.Fatalf("logs contain tool content: %s", logs)
	}

	provider.mu.Lock()
	provider.fail = true
	provider.mu.Unlock()
	h.push(textUpdate(3, 101, "again?"))
	h.wantReply(101, 3, conversation.UnavailableReply)
	h.waitState(3, "delivered")
	for range 2 {
		receive(t, h.ctx, h.turns)
	}
	if turn := receive(t, h.ctx, h.turns); turn.outcome != "error" || turn.errorKind != "tool_provider" {
		t.Fatalf("telemetry turn = %+v, want a tool_provider error", turn)
	}
	select {
	case call := <-runtime.calls:
		t.Fatalf("a failed grant still reached the runtime: %+v", call.request)
	default:
	}
	if logs := h.logs.String(); strings.Contains(logs, "synthetic-private-text") {
		t.Fatal("logs contain provider error text")
	}
}

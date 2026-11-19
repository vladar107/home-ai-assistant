package tools

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

type threadKey struct{}

// A scoped tool set offers only the scope's tools, and each call, made on the runtime's own
// context, carries the scope's binding; an unscoped set offers every tool, as before.
func TestAScopeOffersOnlyItsToolsAndBindsEveryCall(t *testing.T) {
	t.Parallel()
	broker := testBroker(t)
	var bound []any
	respond := func(ctx context.Context, call Call) (Result, error) {
		bound = append(bound, ctx.Value(threadKey{}))
		return Result{Text: "ok"}, nil
	}
	mail := &fakeProvider{principal: testPrincipal, respond: respond, definitions: []Definition{testDefinition("mail_list", 0), testDefinition("mail_read", 0)}}
	notes := &fakeProvider{principal: testPrincipal, respond: respond, definitions: []Definition{testDefinition("notes_read", 0)}}
	for _, provider := range []Provider{mail, notes} {
		if err := broker.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	names := func(set agentruntime.ToolSet) []string {
		var offered []string
		for _, definition := range set.Definitions {
			offered = append(offered, definition.Name)
		}
		return offered
	}

	scope := Scope{Tools: []string{"mail_list", "mail_read"}, Bind: func(ctx context.Context) context.Context {
		return context.WithValue(ctx, threadKey{}, "thread-1")
	}}
	set, err := broker.ReadOnlyToolsFor(WithScope(context.Background(), scope), testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(set); !slices.Equal(got, []string{"mail_list", "mail_read"}) {
		t.Fatalf("scoped tools = %v", got)
	}
	for _, name := range []string{"mail_read", "notes_read"} {
		if _, err := set.Executor.Execute(context.Background(), agentruntime.ToolCall{Name: name, Arguments: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(bound, []any{"thread-1"}) {
		t.Fatalf("bound = %v, want only the scoped call, bound to its thread", bound)
	}

	bound = nil
	set, err = broker.ReadOnlyToolsFor(context.Background(), testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(set); !slices.Equal(got, []string{"mail_list", "mail_read", "notes_read"}) {
		t.Fatalf("unscoped tools = %v", got)
	}
	if _, err := set.Executor.Execute(context.Background(), agentruntime.ToolCall{Name: "notes_read", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(bound, []any{nil}) {
		t.Fatalf("an unscoped call was bound: %v", bound)
	}
}

// A read-only run is offered a write tool only when its scope names it among its writes, and never
// without a scope.
func TestAScopeOffersAReadOnlyRunOnlyTheWritesItNames(t *testing.T) {
	t.Parallel()
	broker := testBroker(t)
	reply := testDefinition("mail_reply", 0)
	reply.Risk = RiskWrite
	send := testDefinition("mail_send", 0)
	send.Risk = RiskWrite
	mail := &fakeProvider{principal: testPrincipal, definitions: []Definition{testDefinition("mail_read", 0), reply, send}}
	if err := broker.Register(mail); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		scope *Scope
		want  []string
	}{
		{nil, []string{"mail_read"}},
		{&Scope{Tools: []string{"mail_read", "mail_reply", "mail_send"}}, []string{"mail_read"}},
		{&Scope{Tools: []string{"mail_read", "mail_reply"}, Writes: []string{"mail_reply", "mail_send"}}, []string{"mail_read", "mail_reply"}},
	} {
		ctx := context.Background()
		if check.scope != nil {
			ctx = WithScope(ctx, *check.scope)
		}
		set, err := broker.ReadOnlyToolsFor(ctx, testPrincipal)
		if err != nil {
			t.Fatal(err)
		}
		var offered []string
		for _, definition := range set.Definitions {
			offered = append(offered, definition.Name)
		}
		if !slices.Equal(offered, check.want) {
			t.Fatalf("scope %+v offers %v, want %v", check.scope, offered, check.want)
		}
	}
}

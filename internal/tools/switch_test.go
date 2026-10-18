package tools_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

var (
	trainsSwitch = integration.Switch{Integration: "trains", Name: "Train information", DefaultOn: true}
	extrasSwitch = integration.Switch{Integration: "extras", Name: "Extras"}
)

// switchedProvider offers its tool to everyone and counts the calls that reach it; the broker alone
// decides whether its integration is on.
type switchedProvider struct {
	sw       integration.Switch
	tool     string
	guidance string
	calls    atomic.Int32
}

func (p *switchedProvider) Switch() integration.Switch { return p.sw }

func (p *switchedProvider) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{Guidance: p.guidance, Definitions: []tools.Definition{{
		Name:        p.tool,
		Description: "A test tool.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Risk:        tools.RiskRead,
	}}}, nil
}

func (p *switchedProvider) Execute(context.Context, string, tools.Call) (tools.Result, error) {
	p.calls.Add(1)
	return tools.Result{Text: "ran " + p.tool}, nil
}

type switchFixture struct {
	t        *testing.T
	switches *integration.Switches
	broker   *tools.Broker
	trains   *switchedProvider
	extras   *switchedProvider
}

func newSwitchFixture(t *testing.T) *switchFixture {
	t.Helper()
	ctx := t.Context()
	state, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if err := state.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := &switchFixture{
		t:      t,
		trains: &switchedProvider{sw: trainsSwitch, tool: "train_times", guidance: "Trains guidance."},
		extras: &switchedProvider{sw: extrasSwitch, tool: "extra_tool", guidance: "Extras guidance."},
	}
	if f.switches, err = integration.NewSwitches(state, logger); err != nil {
		t.Fatal(err)
	}
	if f.broker, err = tools.NewBroker(logger); err != nil {
		t.Fatal(err)
	}
	f.broker.SetSwitches(f.switches)
	for _, provider := range []tools.Provider{f.trains, f.extras} {
		if err := f.broker.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *switchFixture) toolSet(principalID string) agentruntime.ToolSet {
	f.t.Helper()
	set, err := f.broker.ToolsFor(f.t.Context(), principalID)
	if err != nil {
		f.t.Fatal(err)
	}
	return set
}

func (f *switchFixture) set(principalID string, sw integration.Switch, enabled bool) {
	f.t.Helper()
	if err := f.switches.SetEnabled(f.t.Context(), principalID, sw, enabled); err != nil {
		f.t.Fatal(err)
	}
}

func names(set agentruntime.ToolSet) []string {
	var names []string
	for _, definition := range set.Definitions {
		names = append(names, definition.Name)
	}
	return names
}

func call(t *testing.T, set agentruntime.ToolSet, name string) agentruntime.ToolResult {
	t.Helper()
	result, err := set.Executor.Execute(t.Context(), agentruntime.ToolCall{Name: name, Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSwitchedOffIntegrationIsNeitherOfferedNorRun(t *testing.T) {
	f := newSwitchFixture(t)
	set := f.toolSet("alice")
	if got := names(set); !slices.Equal(got, []string{"train_times"}) || !strings.Contains(set.Guidance, "Trains guidance.") {
		t.Fatalf("tools by default = %v, guidance %q", got, set.Guidance)
	}

	f.set("alice", trainsSwitch, false)
	if off := f.toolSet("alice"); !off.Empty() {
		t.Fatalf("tools with trains off = %v", names(off))
	}
	if readOnly, err := f.broker.ReadOnlyToolsFor(t.Context(), "alice"); err != nil || !readOnly.Empty() {
		t.Fatalf("scheduled tools with trains off = %v %v", names(readOnly), err)
	}
	refused := call(t, set, "train_times")
	if !refused.IsError || refused.Text != "Train information is off for this user; they can turn it on in /settings." {
		t.Fatalf("call offered before the switch = %+v", refused)
	}
	if got := f.trains.calls.Load(); got != 0 {
		t.Fatalf("provider ran %d calls with trains off", got)
	}
	if got := names(f.toolSet("bob")); !slices.Equal(got, []string{"train_times"}) {
		t.Fatalf("bob's tools after alice turned trains off = %v", got)
	}

	f.set("alice", trainsSwitch, true)
	if result := call(t, f.toolSet("alice"), "train_times"); result.IsError || !strings.Contains(result.Text, "ran train_times") {
		t.Fatalf("call after turning trains back on = %+v", result)
	}
}

func TestDefaultOffIntegrationWaitsForThePrincipal(t *testing.T) {
	f := newSwitchFixture(t)
	f.set("alice", extrasSwitch, true)
	set := f.toolSet("alice")
	if got := names(set); !slices.Equal(got, []string{"train_times", "extra_tool"}) || !strings.Contains(set.Guidance, "Extras guidance.") {
		t.Fatalf("alice's tools = %v, guidance %q", got, set.Guidance)
	}
	if got := names(f.toolSet("bob")); !slices.Equal(got, []string{"train_times"}) {
		t.Fatalf("bob's tools = %v", got)
	}
	if result := call(t, set, "extra_tool"); result.IsError {
		t.Fatalf("call = %+v", result)
	}
}

func TestSwitchedProviderNeedsSwitches(t *testing.T) {
	broker, err := tools.NewBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(&switchedProvider{sw: trainsSwitch, tool: "train_times"}); err == nil {
		t.Fatal("a switched provider was registered without switches")
	}
}

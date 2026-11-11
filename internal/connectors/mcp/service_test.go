package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/mcp"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/mcpsim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	aliceKey       = "alice-key-0123456789abcdef"
	aliceNewKey    = "alice-key-00112233445566778899"
	bobKey         = "bob-key-fedcba9876543210"
	rotatedKey     = "rotated-key-0123456789"
	someoneElseKey = "someone-else-0123456789"
	// refreshBound is the fixture's refresh timeout.
	refreshBound = 300 * time.Millisecond
)

// everyKey is every key the tests use, none of which may reach a log line.
var everyKey = []string{aliceKey, aliceNewKey, bobKey, rotatedKey, someoneElseKey}

// failingStore fails saving or deleting a connection record on demand, as a full disk would.
type failingStore struct {
	integration.Repository
	failSave   atomic.Bool
	failDelete atomic.Bool
}

func (r *failingStore) DeleteIntegrationConnection(ctx context.Context, principalID, name string) (bool, error) {
	if r.failDelete.Load() {
		return false, errors.New("deleting the connection failed")
	}
	return r.Repository.DeleteIntegrationConnection(ctx, principalID, name)
}

func (r *failingStore) SaveIntegrationConnection(ctx context.Context, record integration.Connection) error {
	if r.failSave.Load() {
		return errors.New("saving the connection failed")
	}
	return r.Repository.SaveIntegrationConnection(ctx, record)
}

// failingSwitches fails changing a switch on demand.
type failingSwitches struct {
	*integration.Switches
	failSet atomic.Bool
}

func (s *failingSwitches) SetEnabled(ctx context.Context, principalID string, sw integration.Switch, enabled bool) error {
	if s.failSet.Load() {
		return errors.New("saving the switch failed")
	}
	return s.Switches.SetEnabled(ctx, principalID, sw, enabled)
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// exposure records what the broker marks after a third-party call.
type exposure struct {
	mu       sync.Mutex
	external []bool
	screened []string
}

func (e *exposure) MarkTurnExposed(_ context.Context, _, _ string, external bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.external = append(e.external, external)
}

func (e *exposure) Screen(_ context.Context, _, tool, _ string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.screened = append(e.screened, tool)
	return false, nil
}

type fixture struct {
	t           *testing.T
	ctx         context.Context
	store       *sqlite.Store
	switches    *integration.Switches
	failing     *failingSwitches
	repository  *failingStore
	refresh     time.Duration
	credentials *integration.Credentials
	logger      *slog.Logger
	clock       *clock
	exposure    *exposure
	service     *mcp.Service
	broker      *tools.Broker
}

func newFixture(t *testing.T, servers ...config.MCPServer) *fixture {
	t.Helper()
	return newFixtureWithRefresh(t, refreshBound, servers...)
}

func newFixtureWithRefresh(t *testing.T, refresh time.Duration, servers ...config.MCPServer) *fixture {
	t.Helper()
	httpretry.WithoutWaits(t)
	logs := &lockedBuffer{}
	// Registered first, so it runs last and sees every line the test logged.
	t.Cleanup(func() {
		text := logs.String()
		for _, key := range everyKey {
			if strings.Contains(text, key) {
				t.Errorf("a key reached the logs:\n%s", text)
			}
		}
		if strings.Contains(text, mcpsim.ServerErrorText) {
			t.Errorf("server text reached the logs:\n%s", text)
		}
	})
	ctx := t.Context()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	switches, err := integration.NewSwitches(store, logger)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := integration.NewCredentials(filepath.Join(t.TempDir(), "integrations"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t: t, ctx: ctx, store: store, switches: switches, credentials: credentials, logger: logger, refresh: refresh,
		failing: &failingSwitches{Switches: switches}, repository: &failingStore{Repository: store},
		clock:    &clock{now: time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)},
		exposure: &exposure{},
	}
	f.rebuild(servers...)
	return f
}

// rebuild replaces the service and broker over the same store, key files, clock, and logs, as a
// restart with a changed catalog does.
func (f *fixture) rebuild(servers ...config.MCPServer) {
	f.t.Helper()
	if f.service != nil {
		_ = f.service.Shutdown(context.Background())
	}
	service, err := mcp.NewService(mcp.Config{
		Servers: servers, Credentials: f.credentials, Repository: f.repository, Switches: f.failing,
		Logger: f.logger, Now: f.clock.Now, RefreshTimeout: f.refresh,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := service.Start(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	broker, err := tools.NewBroker(f.logger)
	if err != nil {
		f.t.Fatal(err)
	}
	broker.SetSwitches(f.switches)
	broker.SetTurnExposure(f.exposure)
	broker.SetScreener(f.exposure)
	for _, provider := range service.Providers() {
		if err := broker.Register(provider); err != nil {
			f.t.Fatal(err)
		}
	}
	f.service, f.broker = service, broker
}

func (f *fixture) enable(principalID, server string) {
	f.t.Helper()
	if _, err := f.service.SetEnabled(f.ctx, principalID, server, true); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) connect(principalID, server, key string) {
	f.t.Helper()
	if _, err := f.service.Connect(f.ctx, principalID, server, key); err != nil {
		f.t.Fatal(err)
	}
}

// turn collects the principal's tools and waits for any background refresh it started, so what
// the refresh did is settled before the test looks.
func (f *fixture) turn(principalID string) agentruntime.ToolSet {
	f.t.Helper()
	set, err := f.broker.ToolsFor(f.ctx, principalID)
	if err != nil {
		f.t.Fatalf("ToolsFor failed: %v", err)
	}
	mcp.WaitForRefreshes(f.service)
	return set
}

func (f *fixture) call(set agentruntime.ToolSet, name, arguments string) agentruntime.ToolResult {
	f.t.Helper()
	if set.Executor == nil {
		f.t.Fatalf("no tools offered to call %s", name)
	}
	result, err := set.Executor.Execute(f.ctx, agentruntime.ToolCall{ID: "call-1", Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		f.t.Fatal(err)
	}
	mcp.WaitForRefreshes(f.service)
	return result
}

func (f *fixture) provider(server string) tools.Provider {
	f.t.Helper()
	for _, provider := range f.service.Providers() {
		if provider.(integration.Switched).Switch().Integration == mcp.IntegrationName(server) {
			return provider
		}
	}
	f.t.Fatalf("no provider for %s", server)
	return nil
}

func (f *fixture) status(principalID, server string) mcp.ServerStatus {
	f.t.Helper()
	status, err := f.service.Status(f.ctx, principalID, server)
	if err != nil {
		f.t.Fatal(err)
	}
	return status
}

func names(set agentruntime.ToolSet) []string {
	var offered []string
	for _, definition := range set.Definitions {
		offered = append(offered, definition.Name)
	}
	return offered
}

func allow(tools ...string) []config.MCPTool {
	allowed := make([]config.MCPTool, 0, len(tools))
	for _, tool := range tools {
		allowed = append(allowed, config.MCPTool{Name: tool})
	}
	return allowed
}

func sharedServer(name string, sim *mcpsim.Simulator, tools ...string) config.MCPServer {
	return config.MCPServer{
		Name: name, Title: strings.ToUpper(name[:1]) + name[1:], Description: "A shared test service.",
		URL: sim.URL(), Auth: config.MCPAuthNone, Tools: allow(tools...),
	}
}

func keyedServer(name string, sim *mcpsim.Simulator, tools ...string) config.MCPServer {
	server := sharedServer(name, sim, tools...)
	server.Description = "A keyed test service."
	server.Auth = config.MCPAuthKey
	server.KeyHeader = "Authorization"
	server.KeyPrefix = "Bearer "
	return server
}

func searchTool(name string) mcpsim.Tool {
	return mcpsim.Tool{Name: name, Description: "Searches " + name + ".", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
	}}
}

func newSim(t *testing.T, tools ...string) *mcpsim.Simulator {
	sim := mcpsim.New(t)
	listed := make([]mcpsim.Tool, 0, len(tools))
	for _, tool := range tools {
		listed = append(listed, searchTool(tool))
		sim.SetResult(tool, mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text("result of " + tool)}})
	}
	sim.SetTools(listed, 0)
	return sim
}

func untrusted(text string) string {
	return strings.TrimPrefix(text, prompt.ToolResultUntrusted)
}

func TestKeysStayWithTheirOwnersAndSessionsAreNotShared(t *testing.T) {
	sim := newSim(t, "search")
	sim.SetStateful(true)
	sim.RequireHeader("Authorization", "Bearer "+aliceKey, "Bearer "+bobKey)
	f := newFixture(t, keyedServer("market", sim, "search"))

	usedBy := map[string]string{}
	acting := func(key string, act func()) {
		t.Helper()
		before := len(sim.Requests())
		act()
		requests := sim.Requests()[before:]
		if len(requests) == 0 {
			t.Fatal("nothing reached the server")
		}
		for _, request := range requests {
			if got := request.Header.Get("Authorization"); got != "Bearer "+key {
				t.Fatalf("a %s %s request carried another credential", request.HTTPMethod, request.RPCMethod)
			}
			if request.SessionID == "" {
				continue
			}
			if owner, seen := usedBy[request.SessionID]; seen && owner != key {
				t.Fatalf("session %s is shared between users", request.SessionID)
			}
			usedBy[request.SessionID] = key
		}
	}
	for _, user := range []struct{ principal, key string }{{"alice", aliceKey}, {"bob", bobKey}} {
		acting(user.key, func() {
			f.connect(user.principal, "market", user.key)
			set := f.turn(user.principal)
			if got := names(set); !slices.Equal(got, []string{"mcp_market_search"}) {
				t.Fatalf("%s was offered %v", user.principal, got)
			}
			if result := f.call(set, "mcp_market_search", `{"query":"lamp"}`); result.IsError || untrusted(result.Text) != "result of search" {
				t.Fatalf("%s call = %+v", user.principal, result)
			}
		})
	}
	// Each user's turn after the other's still carries only their own key.
	acting(aliceKey, func() { f.call(f.turn("alice"), "mcp_market_search", `{}`) })
	acting(bobKey, func() { f.call(f.turn("bob"), "mcp_market_search", `{}`) })

	owners := map[string]int{}
	for _, owner := range usedBy {
		owners[owner]++
	}
	if owners[aliceKey] == 0 || owners[bobKey] == 0 {
		t.Fatalf("sessions by key = %v, want each user to have their own", owners)
	}

	if err := f.service.Shutdown(f.ctx); err != nil {
		t.Fatal(err)
	}
	if open := sim.Sessions(); len(open) != 0 {
		t.Fatalf("sessions left open after shutdown: %v", open)
	}
}

func TestSwitchKeyAndPrincipalListGateTools(t *testing.T) {
	shared := newSim(t, "lookup")
	keyed := newSim(t, "search")
	private := newSim(t, "status")
	privateServer := sharedServer("private", private, "status")
	privateServer.Principals = []string{"alice"}
	f := newFixture(t, sharedServer("shared", shared, "lookup"), keyedServer("market", keyed, "search"), privateServer)

	if set := f.turn("alice"); !set.Empty() {
		t.Fatalf("servers are on by default: %v", names(set))
	}
	f.enable("alice", "shared")
	f.enable("alice", "market")
	if got := names(f.turn("alice")); !slices.Equal(got, []string{"mcp_shared_lookup"}) {
		t.Fatalf("offered %v, want only the shared server while the keyed one has no key", got)
	}
	if got := f.status("alice", "market").State; got != mcp.StateNeedsKey {
		t.Fatalf("keyed server without a key is %s", got)
	}
	if requests := keyed.Requests(); len(requests) != 0 {
		t.Fatalf("the keyed server was contacted without a key: %d requests", len(requests))
	}
	if result, _ := f.provider("market").Execute(f.ctx, "alice", tools.Call{Name: "mcp_market_search", Arguments: json.RawMessage(`{}`)}); !result.IsError {
		t.Fatalf("a keyed tool ran without a key: %+v", result)
	}

	// Bob is outside the private server's list: the switch cannot grant it, the settings refuse it,
	// and the provider refuses a call made past the broker.
	if err := f.switches.SetEnabled(f.ctx, "bob", f.provider("private").(integration.Switched).Switch(), true); err != nil {
		t.Fatal(err)
	}
	if set := f.turn("bob"); !set.Empty() {
		t.Fatalf("bob was offered %v", names(set))
	}
	if _, err := f.service.SetEnabled(f.ctx, "bob", "private", true); !errors.Is(err, mcp.ErrNotAllowed) {
		t.Fatalf("SetEnabled for a principal outside the list = %v", err)
	}
	result, err := f.provider("private").Execute(f.ctx, "bob", tools.Call{Name: "mcp_private_status", Arguments: json.RawMessage(`{}`)})
	if err != nil || !result.IsError || result.Text != prompt.ToolUnavailable {
		t.Fatalf("direct call outside the list = %+v, %v", result, err)
	}
	if calls := private.RequestsFor("tools/call"); len(calls) != 0 {
		t.Fatalf("the private server was called for bob: %d calls", len(calls))
	}
	servers, err := f.service.Servers(f.ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		if server.Name == "private" {
			t.Fatal("bob's settings list the private server")
		}
	}
}

func TestOnlyAllowlistedToolsAreOfferedInCatalogOrder(t *testing.T) {
	sim := newSim(t, "zeta", "Alpha.Find", "hidden", "beta")
	f := newFixture(t, sharedServer("shop", sim, "beta", "Alpha.Find", "zeta"))
	f.enable("alice", "shop")

	want := []string{"mcp_shop_beta", "mcp_shop_alpha_find", "mcp_shop_zeta"}
	set := f.turn("alice")
	if got := names(set); !slices.Equal(got, want) {
		t.Fatalf("offered %v, want %v", got, want)
	}
	readOnly, err := f.broker.ReadOnlyToolsFor(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(readOnly); !slices.Equal(got, want) {
		t.Fatalf("read-only turn offered %v, want %v", got, want)
	}
	for _, name := range want {
		if set.Activities[name] != tools.ActivityServices {
			t.Fatalf("%s carries activity %q", name, set.Activities[name])
		}
	}

	if result := f.call(set, "mcp_shop_alpha_find", `{}`); result.IsError || untrusted(result.Text) != "result of Alpha.Find" {
		t.Fatalf("normalized name did not reach the remote tool: %+v", result)
	}
	if result := f.call(set, "mcp_shop_hidden", `{}`); !result.IsError {
		t.Fatalf("an unlisted tool was callable through the broker: %+v", result)
	}
	result, err := f.provider("shop").Execute(f.ctx, "alice", tools.Call{Name: "mcp_shop_hidden", Arguments: json.RawMessage(`{}`)})
	if err != nil || !result.IsError || result.Text != prompt.ToolUnavailable {
		t.Fatalf("an unlisted tool was callable through the provider: %+v, %v", result, err)
	}
	for _, request := range sim.RequestsFor("tools/call") {
		if strings.Contains(string(request.Params), "hidden") {
			t.Fatal("the unlisted tool was called on the server")
		}
	}
}

func TestDefinitionsAreThirdPartyScreenedReads(t *testing.T) {
	sim := newSim(t, "search")
	server := sharedServer("shop", sim, "search")
	server.Guidance = "Use the shop for prices."
	f := newFixture(t, server)
	f.enable("alice", "shop")

	contribution, err := f.provider("shop").Contribute(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if contribution.Guidance != server.Guidance || contribution.WriteGuidance != "" {
		t.Fatalf("guidance = %q / %q", contribution.Guidance, contribution.WriteGuidance)
	}
	if len(contribution.Definitions) != 1 {
		t.Fatalf("definitions = %+v", contribution.Definitions)
	}
	definition := contribution.Definitions[0]
	if definition.Risk != tools.RiskRead || !definition.ThirdParty || !definition.Screen || definition.Household || definition.Private || definition.Confirmed {
		t.Fatalf("definition flags = %+v", definition)
	}
	if strings.Contains(definition.Description, mcpsim.Instructions) {
		t.Fatal("the server's instructions reached a description")
	}

	set := f.turn("alice")
	if !strings.Contains(set.Guidance, server.Guidance) || strings.Contains(set.Guidance, mcpsim.Instructions) {
		t.Fatalf("turn guidance = %q", set.Guidance)
	}
	f.call(set, "mcp_shop_search", `{"query":"chair"}`)
	f.exposure.mu.Lock()
	defer f.exposure.mu.Unlock()
	if !slices.Equal(f.exposure.external, []bool{true}) || !slices.Equal(f.exposure.screened, []string{"mcp_shop_search"}) {
		t.Fatalf("exposure = %v, screened = %v", f.exposure.external, f.exposure.screened)
	}
}

func TestMalformedDefinitionsAndFailingServersLeaveTheTurnWorking(t *testing.T) {
	sim := mcpsim.New(t)
	longName := "find_" + strings.Repeat("x", 52)
	huge := map[string]any{"type": "object", "description": strings.Repeat("d", mcp.MaximumSchemaBytes)}
	sim.SetTools([]mcpsim.Tool{
		searchTool("search"),
		{Name: "blank", Description: "  "},
		{Name: "pinned"},
		{Name: "stringschema", Description: "Bad schema.", RawInputSchema: json.RawMessage(`"object"`)},
		{Name: "arrayschema", Description: "Bad schema.", RawInputSchema: json.RawMessage(`[{"type":"object"}]`)},
		{Name: "typed", Description: "Not an object.", InputSchema: map[string]any{"type": "string"}},
		{Name: "huge", Description: "Too big.", InputSchema: huge},
		{Name: longName, Description: "Name too long."},
		{Name: "get-price", Description: "One."},
		{Name: "get_price", Description: "Two."},
		{Name: "web_search", Description: "Named like a built-in tool.", RawInputSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","properties":{}}`)},
	}, 0)
	server := sharedServer("shop", sim, "search", "blank", "pinned", "stringschema", "arrayschema", "typed", "huge", longName, "get-price", "get_price", "web_search")
	for i := range server.Tools {
		if server.Tools[i].Name == "pinned" {
			server.Tools[i].Description = "Pinned by the operator."
		}
	}

	down := newSim(t, "search")
	down.SetDown(true)
	hanging := newSim(t, "search")
	hanging.SetStream(true)
	hanging.HangOn("")
	hangingList := newSim(t, "search")
	hangingList.HangOn("tools/list")
	f := newFixture(t, server, sharedServer("down", down, "search"), sharedServer("hanging", hanging, "search"), sharedServer("slowlist", hangingList, "search"))
	for _, name := range []string{"shop", "down", "hanging", "slowlist"} {
		f.enable("alice", name)
	}

	set := f.turn("alice")
	want := []string{"mcp_shop_search", "mcp_shop_pinned", "mcp_shop_web_search"}
	if got := names(set); !slices.Equal(got, want) {
		t.Fatalf("offered %v, want %v", got, want)
	}
	for _, definition := range set.Definitions {
		switch definition.Name {
		case "mcp_shop_pinned":
			if definition.Description != "Shop: Pinned by the operator." {
				t.Fatalf("pinned description = %q", definition.Description)
			}
		case "mcp_shop_web_search":
			if strings.Contains(string(definition.InputSchema), "$schema") || !strings.Contains(string(definition.InputSchema), `"type":"object"`) {
				t.Fatalf("schema = %s", definition.InputSchema)
			}
		}
	}

	// The failed servers are in backoff: later turns skip them without contacting them, until the
	// backoff passes and they are retried in the background.
	failing := []*mcpsim.Simulator{down, hanging, hangingList}
	counts := func() []int {
		var seen []int
		for _, sim := range failing {
			seen = append(seen, len(sim.Requests()))
		}
		return seen
	}
	before := counts()
	for range 3 {
		if got := names(f.turn("alice")); !slices.Equal(got, want) {
			t.Fatalf("a later turn offered %v", got)
		}
	}
	if after := counts(); !slices.Equal(after, before) {
		t.Fatalf("turns in backoff contacted failing servers: %v, then %v", before, after)
	}
	f.clock.Advance(31 * time.Second)
	if got := names(f.turn("alice")); !slices.Equal(got, want) {
		t.Fatalf("the turn after the backoff offered %v", got)
	}
	for i, after := range counts() {
		if after == before[i] {
			t.Fatalf("failing server %d was not retried after its backoff", i)
		}
	}
}

func TestToolListIsCachedServedStaleAndBackedOff(t *testing.T) {
	sim := newSim(t, "search")
	// The list Start warms is held, so the first caller finds it never refreshed.
	arrived, release := sim.HoldOn("tools/list")
	defer release()
	f := newFixture(t, sharedServer("shop", sim, "search"))
	f.enable("alice", "shop")
	f.enable("bob", "shop")
	lists := func() int { return len(sim.RequestsFor("tools/list")) }

	// A caller that gives up gets nothing but does not stop the refresh, which the next turn uses.
	<-arrived
	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()
	if contribution, _ := f.provider("shop").Contribute(cancelled, "alice"); len(contribution.Definitions) != 0 {
		t.Fatalf("a cancelled caller was offered %v", contribution.Definitions)
	}
	release()
	if got := names(f.turn("alice")); len(got) != 1 {
		t.Fatalf("turn after a cancelled refresh offered %v", got)
	}
	if lists() != 1 {
		t.Fatalf("tools/list requests = %d, want 1", lists())
	}

	f.clock.Advance(4 * time.Minute)
	f.turn("alice")
	f.turn("bob")
	if lists() != 1 {
		t.Fatalf("a turn within five minutes listed again: %d", lists())
	}

	f.clock.Advance(2 * time.Minute)
	sim.SetDown(true)
	if got := names(f.turn("alice")); len(got) != 1 {
		t.Fatalf("stale list not served: %v", got)
	}
	attempts := len(sim.Requests())
	f.clock.Advance(10 * time.Second)
	if got := names(f.turn("alice")); len(got) != 1 {
		t.Fatalf("stale list not served in backoff: %v", got)
	}
	if len(sim.Requests()) != attempts {
		t.Fatal("a turn in backoff contacted the server")
	}
	f.clock.Advance(25 * time.Second)
	f.turn("alice")
	if len(sim.Requests()) == attempts {
		t.Fatal("no retry after the backoff passed")
	}
	attempts = len(sim.Requests())
	f.clock.Advance(40 * time.Second)
	f.turn("alice")
	if len(sim.Requests()) != attempts {
		t.Fatal("the backoff did not grow")
	}

	f.clock.Advance(time.Hour)
	if set := f.turn("alice"); !set.Empty() {
		t.Fatalf("a list over an hour old was served: %v", names(set))
	}

	// A server with nothing to serve is skipped at once and retried in the background.
	sim.SetDown(false)
	f.clock.Advance(10 * time.Minute)
	if set := f.turn("alice"); !set.Empty() {
		t.Fatalf("a server with no list made the turn wait: %v", names(set))
	}
	if got := names(f.turn("alice")); len(got) != 1 {
		t.Fatalf("a recovered server offered %v", got)
	}
}

func TestSessionExpiryIsRecovered(t *testing.T) {
	sim := newSim(t, "search")
	sim.SetStateful(true)
	f := newFixture(t, sharedServer("shop", sim, "search"))
	f.enable("alice", "shop")
	set := f.turn("alice")
	if result := f.call(set, "mcp_shop_search", `{}`); result.IsError {
		t.Fatalf("first call = %+v", result)
	}
	before := len(sim.RequestsFor("initialize"))
	sim.ForgetSessions(1)
	if result := f.call(set, "mcp_shop_search", `{}`); result.IsError || untrusted(result.Text) != "result of search" {
		t.Fatalf("call after session expiry = %+v", result)
	}
	if got := len(sim.RequestsFor("initialize")) - before; got != 1 {
		t.Fatalf("the call opened %d sessions, want 1", got)
	}
}

func TestResultsAreBoundedAndErrorsAdaptable(t *testing.T) {
	sim := newSim(t, "big", "pictures", "broken", "empty")
	sim.SetResult("big", mcpsim.Result{Content: []mcpsim.Content{
		mcpsim.Text(strings.Repeat("a", 30<<10)), mcpsim.Image("image/png"), mcpsim.Text(strings.Repeat("b", 30<<10)),
	}})
	sim.SetResult("pictures", mcpsim.Result{Content: []mcpsim.Content{
		mcpsim.Audio("audio/wav"), mcpsim.ResourceLink("https://example.com/item"), {"type": "made-up kind"},
	}})
	sim.SetResult("broken", mcpsim.Result{IsError: true, Content: []mcpsim.Content{mcpsim.Text("no such item")}})
	f := newFixture(t, sharedServer("shop", sim, "big", "pictures", "broken", "empty"))
	f.enable("alice", "shop")
	set := f.turn("alice")

	big := f.call(set, "mcp_shop_big", `{}`)
	if big.IsError || len(untrusted(big.Text)) > mcp.ResultBytes || !strings.Contains(big.Text, "[Not shown: image content.]") || !strings.Contains(big.Text, "clipped") {
		t.Fatalf("oversized result: error %v, %d bytes, tail %q", big.IsError, len(untrusted(big.Text)), big.Text[max(0, len(big.Text)-200):])
	}
	pictures := f.call(set, "mcp_shop_pictures", `{}`)
	if pictures.IsError || untrusted(pictures.Text) != "[Not shown: audio, resource_link, other content.]" {
		t.Fatalf("non-text result = %+v", pictures)
	}
	broken := f.call(set, "mcp_shop_broken", `{}`)
	if !broken.IsError || !strings.Contains(broken.Text, "no such item") {
		t.Fatalf("server error result = %+v", broken)
	}
	sim.SetResult("empty", mcpsim.Result{})
	if empty := f.call(set, "mcp_shop_empty", `{}`); empty.IsError || untrusted(empty.Text) != "The tool returned no content." {
		t.Fatalf("empty result = %+v", empty)
	}
	sim.SetDown(true)
	down := f.call(set, "mcp_shop_big", `{}`)
	if !down.IsError || strings.Contains(down.Text, mcpsim.ServerErrorText) {
		t.Fatalf("failed call = %+v", down)
	}
}

func TestRejectedCallIsAFixedText(t *testing.T) {
	sim := mcpsim.New(t)
	// A tool without a scripted result is answered with a JSON-RPC error carrying server text.
	sim.SetTools([]mcpsim.Tool{searchTool("search")}, 0)
	f := newFixture(t, sharedServer("shop", sim, "search"))
	f.enable("alice", "shop")
	result := f.call(f.turn("alice"), "mcp_shop_search", `{"query":"x"}`)
	if !result.IsError || strings.Contains(result.Text, mcpsim.ServerErrorText) || !strings.Contains(result.Text, "check the arguments") {
		t.Fatalf("rejected call = %+v", result)
	}
}

func TestConnectVerifyOriginProblemAndDisconnect(t *testing.T) {
	sim := newSim(t, "search")
	sim.RequireHeader("Authorization", "Bearer "+aliceKey)
	server := keyedServer("market", sim, "search")
	f := newFixture(t, server)
	name := mcp.IntegrationName("market")

	if _, err := f.service.Connect(f.ctx, "alice", "market", bobKey); !errors.Is(err, mcp.ErrKeyRefused) || strings.Contains(err.Error(), bobKey) {
		t.Fatalf("Connect with a refused key = %v", err)
	}
	if _, found, _ := f.credentials.Read("alice", name); found {
		t.Fatal("a refused key was stored")
	}
	if _, bound, _ := f.store.LoadIntegrationConnection(f.ctx, "alice", name); bound {
		t.Fatal("a refused key was recorded")
	}
	if _, err := f.service.Connect(f.ctx, "alice", "market", "short"); !errors.Is(err, mcp.ErrInvalidKey) {
		t.Fatalf("Connect with a malformed key = %v", err)
	}

	status, err := f.service.Connect(f.ctx, "alice", "market", aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != mcp.StateOn || status.Key != mcp.KeyConnected || !status.Enabled {
		t.Fatalf("status after Connect = %+v", status)
	}
	if key, found, err := f.credentials.Read("alice", name); err != nil || !found || key != aliceKey {
		t.Fatalf("stored key found=%v err=%v", found, err)
	}
	record, bound, err := f.store.LoadIntegrationConnection(f.ctx, "alice", name)
	if err != nil || !bound || record.TokenKind != integration.TokenKindPersonal || record.WorkspaceID != server.Origin() {
		t.Fatalf("connection record = %+v bound=%v err=%v", record, bound, err)
	}
	set := f.turn("alice")
	if result := f.call(set, "mcp_market_search", `{}`); result.IsError {
		t.Fatalf("call with a connected key = %+v", result)
	}

	// The server stops taking the key: the call fails and the connection shows a problem.
	sim.RequireHeader("Authorization", "Bearer rotated-key-0123456789")
	if result := f.call(set, "mcp_market_search", `{}`); !result.IsError || strings.Contains(result.Text, mcpsim.ServerErrorText) {
		t.Fatalf("refused call = %+v", result)
	}
	if got := f.status("alice", "market"); got.State != mcp.StateProblem || got.Key != mcp.KeyRefused {
		t.Fatalf("status after a refused call = %+v", got)
	}
	sim.RequireHeader("Authorization", "Bearer "+aliceKey)
	if result := f.call(set, "mcp_market_search", `{}`); result.IsError {
		t.Fatalf("call after the key was taken again = %+v", result)
	}
	if got := f.status("alice", "market").State; got != mcp.StateOn {
		t.Fatalf("a successful call left the state %s", got)
	}

	// The catalog moves the server to another origin: the key is never sent there.
	moved := server
	moved.URL = strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	if moved.Origin() == server.Origin() {
		t.Fatal("test setup: the origin did not change")
	}
	f.rebuild(moved)
	before := len(sim.Requests())
	if set := f.turn("alice"); !set.Empty() {
		t.Fatalf("a key for another origin offered %v", names(set))
	}
	result, err := f.provider("market").Execute(f.ctx, "alice", tools.Call{Name: "mcp_market_search", Arguments: json.RawMessage(`{}`)})
	if err != nil || !result.IsError {
		t.Fatalf("direct call with a stale key = %+v, %v", result, err)
	}
	if got := f.status("alice", "market"); got.State != mcp.StateNeedsReconnect || got.Key != mcp.KeyStale {
		t.Fatalf("status after the origin changed = %+v", got)
	}
	if len(sim.Requests()) != before {
		t.Fatal("the key was sent after the origin changed")
	}

	f.rebuild(server)
	f.turn("alice")
	removed, err := f.service.Disconnect(f.ctx, "alice", "market")
	if err != nil || !removed {
		t.Fatalf("Disconnect = %v, %v", removed, err)
	}
	if _, found, _ := f.credentials.Read("alice", name); found {
		t.Fatal("Disconnect left the key file")
	}
	if _, bound, _ := f.store.LoadIntegrationConnection(f.ctx, "alice", name); bound {
		t.Fatal("Disconnect left the connection record")
	}
	if got := f.status("alice", "market"); got.Enabled || got.State != mcp.StateOff || got.Key != mcp.KeyMissing {
		t.Fatalf("status after Disconnect = %+v", got)
	}
	if set := f.turn("alice"); !set.Empty() {
		t.Fatalf("tools offered after Disconnect: %v", names(set))
	}
}

func TestKeyIntake(t *testing.T) {
	sim := newSim(t, "search")
	sim.RequireHeader("Authorization", "Bearer "+aliceKey)
	other := newSim(t, "search")
	market := keyedServer("market", sim, "search")
	market.KeyPattern = regexp.MustCompile(`^(?:alice-key-[0-9a-f]+)$`)
	f := newFixture(t, market, keyedServer("other", other, "search"), sharedServer("shared", other, "search"))

	if result, err := f.service.OfferMessage(f.ctx, "alice", aliceKey); err != nil || result.Outcome != mcp.IntakeNone {
		t.Fatalf("offer with no intake = %+v, %v", result, err)
	}
	if _, err := f.service.BeginIntake("alice", "shared"); !errors.Is(err, mcp.ErrNoKeyNeeded) {
		t.Fatalf("intake for a server without keys = %v", err)
	}

	for _, message := range []string{
		"alice-key 0123456789abcdef",
		"alice-key-01",
		"bob-key-fedcba9876543210",
		"alice-key-0123456789abcdef\n",
		strings.Repeat("a", 513),
	} {
		if _, err := f.service.BeginIntake("alice", "market"); err != nil {
			t.Fatal(err)
		}
		result, err := f.service.OfferMessage(f.ctx, "alice", message)
		if err != nil || result.Outcome != mcp.IntakeNotKey || result.Server != "market" {
			t.Fatalf("offer %q = %+v, %v", message, result, err)
		}
		if _, pending := f.service.PendingIntake("alice"); pending {
			t.Fatalf("intake still pending after %q", message)
		}
	}
	if requests := sim.Requests(); len(requests) != 0 {
		t.Fatalf("a message that is not a key reached the server: %d requests", len(requests))
	}

	// One intake per principal: the later one replaces the earlier.
	if _, err := f.service.BeginIntake("alice", "market"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.BeginIntake("alice", "other"); err != nil {
		t.Fatal(err)
	}
	if server, pending := f.service.PendingIntake("alice"); !pending || server != "other" {
		t.Fatalf("pending intake = %q, %v", server, pending)
	}
	if !f.service.CancelIntake("alice") {
		t.Fatal("CancelIntake found nothing pending")
	}

	// An expired intake takes a late key, but lets an ordinary message through.
	expires, err := f.service.BeginIntake("alice", "market")
	if err != nil {
		t.Fatal(err)
	}
	if !expires.After(f.clock.Now()) {
		t.Fatalf("intake expires at %v", expires)
	}
	f.clock.Advance(11 * time.Minute)
	if _, pending := f.service.PendingIntake("alice"); pending {
		t.Fatal("an expired intake is pending")
	}
	if result, err := f.service.OfferMessage(f.ctx, "alice", aliceKey); err != nil || result.Outcome != mcp.IntakeExpired {
		t.Fatalf("offer of a key to an expired intake = %+v, %v", result, err)
	}
	if _, err := f.service.BeginIntake("alice", "market"); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(11 * time.Minute)
	if result, err := f.service.OfferMessage(f.ctx, "alice", "what is the weather tomorrow?"); err != nil || result.Outcome != mcp.IntakeNone {
		t.Fatalf("offer of an ordinary message to an expired intake = %+v, %v", result, err)
	}
	if result, _ := f.service.OfferMessage(f.ctx, "alice", aliceKey); result.Outcome != mcp.IntakeNone {
		t.Fatalf("the expired intake was not dropped: %+v", result)
	}
	if requests := sim.Requests(); len(requests) != 0 {
		t.Fatal("a key offered to an expired intake reached the server")
	}

	// A refused or unverified key leaves the intake pending, so the key pasted again is still taken.
	if _, err := f.service.BeginIntake("alice", "market"); err != nil {
		t.Fatal(err)
	}
	sim.RequireHeader("Authorization", "Bearer "+someoneElseKey)
	if result, err := f.service.OfferMessage(f.ctx, "alice", aliceNewKey); err != nil || result.Outcome != mcp.IntakeRefused {
		t.Fatalf("offer of a refused key = %+v, %v", result, err)
	}
	if server, pending := f.service.PendingIntake("alice"); !pending || server != "market" {
		t.Fatal("a refused key ended the intake")
	}
	sim.RequireHeader("Authorization", "Bearer "+aliceKey)
	sim.SetDown(true)
	if result, err := f.service.OfferMessage(f.ctx, "alice", aliceKey); err != nil || result.Outcome != mcp.IntakeUnverified {
		t.Fatalf("offer to a failing server = %+v, %v", result, err)
	}
	if _, pending := f.service.PendingIntake("alice"); !pending {
		t.Fatal("an unverified key ended the intake")
	}
	if _, found, _ := f.credentials.Read("alice", mcp.IntegrationName("market")); found {
		t.Fatal("a key that was not verified was stored")
	}
	sim.SetDown(false)
	result, err := f.service.OfferMessage(f.ctx, "alice", aliceKey)
	if err != nil || result.Outcome != mcp.IntakeConnected || result.Status.State != mcp.StateOn {
		t.Fatalf("offer of a valid key = %+v, %v", result, err)
	}
	if _, pending := f.service.PendingIntake("alice"); pending {
		t.Fatal("a connected key left the intake pending")
	}
	if got := names(f.turn("alice")); !slices.Equal(got, []string{"mcp_market_search"}) {
		t.Fatalf("offered after intake %v", got)
	}

	// Disconnecting one server leaves an intake for another pending.
	if _, err := f.service.BeginIntake("alice", "market"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Disconnect(f.ctx, "alice", "other"); err != nil {
		t.Fatal(err)
	}
	if server, pending := f.service.PendingIntake("alice"); !pending || server != "market" {
		t.Fatal("disconnecting another server cancelled the intake")
	}
	if _, err := f.service.Disconnect(f.ctx, "alice", "market"); err != nil {
		t.Fatal(err)
	}
	if _, pending := f.service.PendingIntake("alice"); pending {
		t.Fatal("disconnecting the intake's server left it pending")
	}
}

// A message that ended an intake is consumed again when it is offered once more while no intake
// is pending, as a retried update is, until the intake lifetime has passed; the memory is bounded.
func TestAConsumedMessageIsConsumedAgainWhenReplayed(t *testing.T) {
	sim := newSim(t, "search")
	sim.RequireHeader("Authorization", "Bearer "+aliceKey)
	f := newFixture(t, keyedServer("market", sim, "search"))

	if _, err := f.service.BeginIntake("alice", "market"); err != nil {
		t.Fatal(err)
	}
	first, err := f.service.OfferMessage(f.ctx, "alice", aliceKey)
	if err != nil || first.Outcome != mcp.IntakeConnected {
		t.Fatalf("offer of the key = %+v, %v", first, err)
	}
	requests := len(sim.Requests())
	if again, err := f.service.OfferMessage(f.ctx, "alice", aliceKey); err != nil || again != first {
		t.Fatalf("replayed key = %+v, %v; want %+v", again, err, first)
	}
	if len(sim.Requests()) != requests {
		t.Fatal("a replayed key was verified again")
	}
	if other, _ := f.service.OfferMessage(f.ctx, "alice", bobKey); other.Outcome != mcp.IntakeNone {
		t.Fatalf("another message after the intake ended = %+v", other)
	}
	if bobs, _ := f.service.OfferMessage(f.ctx, "bob", aliceKey); bobs.Outcome != mcp.IntakeNone {
		t.Fatalf("the same text from another principal = %+v", bobs)
	}

	if _, err := f.service.BeginIntake("alice", "market"); err != nil {
		t.Fatal(err)
	}
	if notKey, _ := f.service.OfferMessage(f.ctx, "alice", "where is my key?"); notKey.Outcome != mcp.IntakeNotKey {
		t.Fatalf("offer of a question = %+v", notKey)
	}
	if again, _ := f.service.OfferMessage(f.ctx, "alice", "where is my key?"); again.Outcome != mcp.IntakeNotKey || again.Server != "market" {
		t.Fatalf("replayed question = %+v", again)
	}

	f.clock.Advance(10 * time.Minute)
	if late, _ := f.service.OfferMessage(f.ctx, "alice", aliceKey); late.Outcome != mcp.IntakeNone {
		t.Fatalf("a replay after the intake lifetime = %+v", late)
	}

	// Only the latest messages are kept.
	for i := range 9 {
		if _, err := f.service.BeginIntake("alice", "market"); err != nil {
			t.Fatal(err)
		}
		if result, _ := f.service.OfferMessage(f.ctx, "alice", fmt.Sprintf("question %d", i)); result.Outcome != mcp.IntakeNotKey {
			t.Fatalf("question %d = %+v", i, result)
		}
	}
	if oldest, _ := f.service.OfferMessage(f.ctx, "alice", "question 0"); oldest.Outcome != mcp.IntakeNone {
		t.Fatalf("the oldest of nine consumed messages = %+v", oldest)
	}
	if newest, _ := f.service.OfferMessage(f.ctx, "alice", "question 8"); newest.Outcome != mcp.IntakeNotKey {
		t.Fatalf("the newest consumed message = %+v", newest)
	}
}

// An intake that expired longer ago than its lifetime takes nothing, so a token sent days after an
// abandoned Connect passes on as an ordinary message.
func TestAnAbandonedIntakeTakesNothing(t *testing.T) {
	sim := newSim(t, "search")
	f := newFixture(t, keyedServer("market", sim, "search"))
	if _, err := f.service.BeginIntake("alice", "market"); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(21 * time.Minute)
	if result, err := f.service.OfferMessage(f.ctx, "alice", aliceKey); err != nil || result.Outcome != mcp.IntakeNone {
		t.Fatalf("offer to an abandoned intake = %+v, %v", result, err)
	}
	if len(sim.Requests()) != 0 {
		t.Fatal("a message offered to an abandoned intake reached the server")
	}
}

// A key is never an absolute URL, and only a server's own key pattern recognises a key outside an
// intake.
func TestKeyShapes(t *testing.T) {
	sim := newSim(t, "search")
	market, other, private := keyedServer("market", sim, "search"), keyedServer("other", sim, "search"), keyedServer("private", sim, "search")
	market.KeyPattern = regexp.MustCompile(`^alice-key-[0-9a-f]+$`)
	private.KeyPattern = market.KeyPattern
	private.Principals = []string{"bob"}
	for _, text := range []string{"https://example.com/callback?code=0123456789", "custom://0123456789abcdef"} {
		if mcp.ValidKey(other, text) {
			t.Fatalf("%q is a valid key", text)
		}
	}
	f := newFixture(t, market, other, sharedServer("shared", sim, "search"), private)
	if got := f.service.MatchingKeyServers("alice", aliceKey); !slices.Equal(got, []string{"market"}) {
		t.Fatalf("servers matching alice's key = %v", got)
	}
	if got := f.service.MatchingKeyServers("bob", aliceKey); !slices.Equal(got, []string{"market", "private"}) {
		t.Fatalf("servers matching for bob = %v", got)
	}
	if got := f.service.MatchingKeyServers("alice", bobKey); len(got) != 0 {
		t.Fatalf("servers matching a key no pattern takes = %v", got)
	}
}

// After an idle hour a turn that must wait for its lists waits for all of them together, so several
// hanging servers cost about one refresh bound rather than one each; healthy ones are all offered.
func TestListsDueTogetherAreRefreshedTogether(t *testing.T) {
	var sims []*mcpsim.Simulator
	var servers []config.MCPServer
	for i := range 8 {
		sim := newSim(t, "search")
		sims = append(sims, sim)
		servers = append(servers, sharedServer(fmt.Sprintf("shop%d", i), sim, "search"))
	}
	f := newFixture(t, servers...)
	for _, server := range servers {
		f.enable("alice", server.Name)
	}
	mcp.WaitForRefreshes(f.service)
	if got := names(f.turn("alice")); len(got) != len(servers) {
		t.Fatalf("first turn offered %v", got)
	}

	f.clock.Advance(61 * time.Minute)
	if got := names(f.turn("alice")); len(got) != len(servers) {
		t.Fatalf("turn after an idle hour offered %v", got)
	}

	f.clock.Advance(61 * time.Minute)
	for _, sim := range sims {
		sim.HangOn("tools/list")
	}
	started := time.Now()
	set, err := f.broker.ToolsFor(f.ctx, "alice")
	elapsed := time.Since(started)
	if err != nil || !set.Empty() {
		t.Fatalf("turn with hanging servers = %v, %v", names(set), err)
	}
	// Waiting for the eight in turn takes eight refresh bounds.
	if elapsed > 5*refreshBound {
		t.Fatalf("the turn waited %v for %d hanging servers with a refresh bound of %v", elapsed, len(sims), refreshBound)
	}
	mcp.WaitForRefreshes(f.service)
	for i, sim := range sims {
		if got := len(sim.RequestsFor("tools/list")); got != 3 {
			t.Fatalf("server %d tools/list requests = %d, want 3", i, got)
		}
	}
}

func TestStaleListIsServedAtOnceAndRefreshedInTheBackground(t *testing.T) {
	sim := newSim(t, "search", "extra")
	sim.SetTools([]mcpsim.Tool{searchTool("search")}, 0)
	// A long refresh bound, so a turn that waited for the held refresh could not return before
	// the test releases it.
	f := newFixtureWithRefresh(t, time.Minute, sharedServer("shop", sim, "search", "extra"))
	f.enable("alice", "shop")

	// Start warmed the list, so the first turn lists nothing itself.
	mcp.WaitForRefreshes(f.service)
	if got := len(sim.RequestsFor("tools/list")); got != 1 {
		t.Fatalf("tools/list requests after Start = %d, want 1", got)
	}
	if got := names(f.turn("alice")); !slices.Equal(got, []string{"mcp_shop_search"}) {
		t.Fatalf("first turn offered %v", got)
	}
	if got := len(sim.RequestsFor("tools/list")); got != 1 {
		t.Fatalf("the first turn listed again: %d", got)
	}

	sim.SetTools([]mcpsim.Tool{searchTool("search"), searchTool("extra")}, 0)
	arrived, release := sim.HoldOn("tools/list")
	defer release()
	f.clock.Advance(6 * time.Minute)
	type collected struct {
		set agentruntime.ToolSet
		err error
	}
	done := make(chan collected, 1)
	go func() {
		set, err := f.broker.ToolsFor(f.ctx, "alice")
		done <- collected{set, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || !slices.Equal(names(got.set), []string{"mcp_shop_search"}) {
			t.Fatalf("turn with a due list = %v, %v", names(got.set), got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the turn waited for the background refresh")
	}
	<-arrived
	release()
	mcp.WaitForRefreshes(f.service)
	if got := names(f.turn("alice")); !slices.Equal(got, []string{"mcp_shop_search", "mcp_shop_extra"}) {
		t.Fatalf("turn after the background refresh offered %v", got)
	}
}

// A healthy server's list that aged past the hour because nobody asked, as before an hourly agent
// watch, is refreshed before the turn instead of leaving the turn without the server's tools.
func TestAListIdleForOverAnHourIsRefreshedBeforeTheTurn(t *testing.T) {
	sim := newSim(t, "search")
	f := newFixture(t, sharedServer("shop", sim, "search"))
	f.enable("alice", "shop")
	mcp.WaitForRefreshes(f.service)
	if got := names(f.turn("alice")); !slices.Equal(got, []string{"mcp_shop_search"}) {
		t.Fatalf("first turn offered %v", got)
	}

	f.clock.Advance(61 * time.Minute)
	if got := names(f.turn("alice")); !slices.Equal(got, []string{"mcp_shop_search"}) {
		t.Fatalf("turn after an idle hour offered %v", got)
	}
	if got := len(sim.RequestsFor("tools/list")); got != 2 {
		t.Fatalf("tools/list requests = %d, want 2", got)
	}
}

func TestReconnectRetiresTheOldSessionAndKeepsTheOldKeyOnFailure(t *testing.T) {
	sim := newSim(t, "search")
	sim.SetStateful(true)
	sim.RequireHeader("Authorization", "Bearer "+aliceKey, "Bearer "+aliceNewKey)
	f := newFixture(t, keyedServer("market", sim, "search"))
	name := mcp.IntegrationName("market")
	f.connect("alice", "market", aliceKey)
	f.call(f.turn("alice"), "mcp_market_search", `{}`)
	oldSessions := sim.Sessions()
	if len(oldSessions) == 0 {
		t.Fatal("no session opened")
	}

	// A record that cannot be saved leaves the working key in place.
	f.repository.failSave.Store(true)
	if _, err := f.service.Connect(f.ctx, "alice", "market", aliceNewKey); err == nil {
		t.Fatal("Connect succeeded without saving the record")
	}
	f.repository.failSave.Store(false)
	if key, found, _ := f.credentials.Read("alice", name); !found || key != aliceKey {
		t.Fatal("a failed reconnect did not keep the previous key")
	}
	if result := f.call(f.turn("alice"), "mcp_market_search", `{}`); result.IsError {
		t.Fatalf("call after a failed reconnect = %+v", result)
	}

	f.connect("alice", "market", aliceNewKey)
	for _, session := range oldSessions {
		if slices.Contains(sim.Sessions(), session) {
			t.Fatalf("the old key's session %s is still open", session)
		}
	}
	before := len(sim.Requests())
	if result := f.call(f.turn("alice"), "mcp_market_search", `{}`); result.IsError {
		t.Fatalf("call with the new key = %+v", result)
	}
	for _, request := range sim.Requests()[before:] {
		if request.Header.Get("Authorization") != "Bearer "+aliceNewKey {
			t.Fatal("a request after the reconnect carried the old key")
		}
	}

	// A key stored while the switch cannot be turned on is reported as stored, with the switch off.
	if _, err := f.service.SetEnabled(f.ctx, "alice", "market", false); err != nil {
		t.Fatal(err)
	}
	f.failing.failSet.Store(true)
	status, err := f.service.Connect(f.ctx, "alice", "market", aliceKey)
	f.failing.failSet.Store(false)
	if !errors.Is(err, mcp.ErrSwitchUnchanged) || status.Enabled || status.Key != mcp.KeyConnected || status.State != mcp.StateOff {
		t.Fatalf("Connect with a failing switch = %+v, %v", status, err)
	}
	if key, found, _ := f.credentials.Read("alice", name); !found || key != aliceKey {
		t.Fatal("Connect with a failing switch did not store the key")
	}
}

func TestFailedDisconnectLeavesNothingUsableInMemory(t *testing.T) {
	sim := newSim(t, "search")
	sim.SetStateful(true)
	sim.RequireHeader("Authorization", "Bearer "+aliceKey)
	f := newFixture(t, keyedServer("market", sim, "search"))
	f.connect("alice", "market", aliceKey)
	f.call(f.turn("alice"), "mcp_market_search", `{}`)
	if len(sim.Sessions()) == 0 {
		t.Fatal("no session opened")
	}
	f.repository.failDelete.Store(true)
	if _, err := f.service.Disconnect(f.ctx, "alice", "market"); err == nil {
		t.Fatal("Disconnect succeeded without deleting the record")
	}
	if open := sim.Sessions(); len(open) != 0 {
		t.Fatalf("a failed Disconnect left the session open: %v", open)
	}
	before := len(sim.Requests())
	if set := f.turn("alice"); !set.Empty() {
		t.Fatalf("a failed Disconnect left tools offered: %v", names(set))
	}
	if len(sim.Requests()) != before {
		t.Fatal("a failed Disconnect left the key in use")
	}
}

func TestAKeyReadBeforeItWasRemovedCannotOpenASession(t *testing.T) {
	sim := newSim(t, "search")
	sim.SetStateful(true)
	sim.RequireHeader("Authorization", "Bearer "+aliceKey)
	f := newFixture(t, keyedServer("market", sim, "search"))
	f.connect("alice", "market", aliceKey)
	err := mcp.OpenAfterReadingKey(f.service, "alice", "market", func() {
		if _, err := f.service.Disconnect(f.ctx, "alice", "market"); err != nil {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, mcp.ErrRetired) {
		t.Fatalf("a session opened for a removed key: %v", err)
	}

	f.connect("alice", "market", aliceKey)
	before := len(sim.Requests())
	err = mcp.OpenAfterReadingKey(f.service, "alice", "market", func() {
		if err := f.service.Shutdown(f.ctx); err != nil {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, mcp.ErrRetired) {
		t.Fatalf("a session opened after shutdown: %v", err)
	}
	if len(sim.Requests()) != before || len(sim.Sessions()) != 0 {
		t.Fatal("a retired key reached the server")
	}
}

func TestDisconnectWaitsForTheCallInFlight(t *testing.T) {
	sim := newSim(t, "search")
	sim.SetStateful(true)
	sim.RequireHeader("Authorization", "Bearer "+aliceKey)
	f := newFixture(t, keyedServer("market", sim, "search"))
	f.connect("alice", "market", aliceKey)
	f.turn("alice")
	arrived, release := sim.HoldOn("tools/call")
	defer release()

	called := make(chan tools.Result, 1)
	go func() {
		result, _ := f.provider("market").Execute(f.ctx, "alice", tools.Call{Name: "mcp_market_search", Arguments: json.RawMessage(`{}`)})
		called <- result
	}()
	<-arrived
	disconnected := make(chan error, 1)
	go func() {
		_, err := f.service.Disconnect(f.ctx, "alice", "market")
		disconnected <- err
	}()
	// Only a guard: Disconnect must not return while the call holds the session, and a correct
	// Disconnect never returns here however long this waits.
	select {
	case <-disconnected:
		t.Fatal("Disconnect returned while a call was in flight on the session")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	if result := <-called; result.IsError {
		t.Fatalf("the call in flight failed: %+v", result)
	}
	if err := <-disconnected; err != nil {
		t.Fatal(err)
	}
	if open := sim.Sessions(); len(open) != 0 {
		t.Fatalf("sessions left open after Disconnect: %v", open)
	}
}

func TestDisconnectRetiresTheKeyUnderConcurrentUse(t *testing.T) {
	sim := newSim(t, "search")
	sim.SetStateful(true)
	keys := []string{aliceKey, aliceNewKey}
	sim.RequireHeader("Authorization", "Bearer "+aliceKey, "Bearer "+aliceNewKey)
	f := newFixture(t, keyedServer("market", sim, "search"))
	provider := f.provider("market")

	for round := range 4 {
		key := keys[round%len(keys)]
		f.connect("alice", "market", key)
		stop := make(chan struct{})
		var workers sync.WaitGroup
		for range 4 {
			workers.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					_, _ = provider.Contribute(f.ctx, "alice")
					_, _ = provider.Execute(f.ctx, "alice", tools.Call{Name: "mcp_market_search", Arguments: json.RawMessage(`{}`)})
				}
			})
		}
		// Let the workers reach the server with the key before it is removed.
		for len(sim.RequestsFor("tools/call")) < 4*(round+1) {
			runtime.Gosched()
		}
		if _, err := f.service.Disconnect(f.ctx, "alice", "market"); err != nil {
			t.Fatal(err)
		}
		after := len(sim.Requests())
		close(stop)
		workers.Wait()
		mcp.WaitForRefreshes(f.service)
		for _, request := range sim.Requests()[after:] {
			if request.Header.Get("Authorization") == "Bearer "+key {
				t.Fatalf("round %d: a %s %s request carried the removed key", round, request.HTTPMethod, request.RPCMethod)
			}
		}
		if open := sim.Sessions(); len(open) != 0 {
			t.Fatalf("round %d: sessions left open after Disconnect: %v", round, open)
		}
	}
}

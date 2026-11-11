package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/mcp"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/testutil/mcpsim"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const (
	listingResult = "Bicycle, 120 euros, listed today."
	priceResult   = "Kettle: 35 euros."
	aliceMCPKey   = "alice-key-0123456789abcdef"
	bobMCPKey     = "bob-key-fedcba9876543210"
)

// newListingsServer is a shared MCP server whose search tool answers listingResult.
func newListingsServer(t *testing.T) *mcpsim.Simulator {
	t.Helper()
	sim := mcpsim.New(t)
	sim.SetTools([]mcpsim.Tool{{
		Name:        "search",
		Description: "Search the listings.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
	}}, 0)
	sim.SetResult("search", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text(listingResult)}})
	return sim
}

// sharedServer is a catalog entry for a server without keys, open to the principals given or, with
// none, to everyone.
func sharedServer(name string, sim *mcpsim.Simulator, principals ...string) config.MCPServer {
	server := config.MCPServer{
		Name: name, Title: "Listings " + name, Description: "Search classified listings.",
		URL: sim.URL(), Auth: config.MCPAuthNone, Tools: []config.MCPTool{{Name: "search"}},
	}
	if len(principals) > 0 {
		server.Principals = principals
	}
	return server
}

func (h *conversationHarness) setMCP(principal, server string, enabled bool) {
	h.t.Helper()
	if _, err := h.mcp.SetEnabled(h.ctx, principal, server, enabled); err != nil {
		h.t.Fatalf("turn %s on for %s: %v", server, principal, err)
	}
}

// mcpTurn starts a chat turn of the user and returns what the model receives.
func (h *conversationHarness) mcpTurn(runtime *controlledRuntime, updateID, userID int64, text string) runtimeCall {
	h.t.Helper()
	h.push(textUpdate(updateID, userID, text))
	return receive(h.t, h.ctx, runtime.calls)
}

// endTurn answers the turn and waits until its reply is out.
func (h *conversationHarness) endTurn(call runtimeCall, updateID, userID int64, reply string) {
	h.t.Helper()
	call.answer <- agentruntime.RunResult{Response: reply}
	h.wantReply(userID, updateID, reply)
	h.waitState(updateID, "delivered")
}

// mcpTools are the offered tool names from MCP servers.
func mcpTools(call runtimeCall) []string {
	var names []string
	for _, name := range toolNames(call.request.Tools) {
		if strings.HasPrefix(name, "mcp_") {
			names = append(names, name)
		}
	}
	return names
}

// A chat turn calls an allowlisted tool of a shared server; the server's text reaches the model,
// and the conversation that read it is marked as having read outside content.
func TestMCPToolRunsInAChatTurnAndExposesTheConversation(t *testing.T) {
	t.Parallel()
	sim := newListingsServer(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{sharedServer("shop", sim)}})
	h.provision("alice", "account-alice")
	h.start()
	h.setMCP("alice", "shop", true)

	call := h.mcpTurn(runtime, 1, 101, "find me a bicycle")
	if names := mcpTools(call); !slices.Equal(names, []string{"mcp_shop_search"}) {
		t.Fatalf("offered MCP tools = %v", names)
	}
	result := toolCaller(t, call)("mcp_shop_search", `{"query":"bicycle"}`)
	if result.IsError || !strings.Contains(result.Text, listingResult) {
		t.Fatalf("result = %+v", result)
	}
	calls := sim.RequestsFor("tools/call")
	if len(calls) != 1 {
		t.Fatalf("tools/call requests = %d", len(calls))
	}
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(calls[0].Params, &params); err != nil || params.Name != "search" || params.Arguments["query"] != "bicycle" {
		t.Fatalf("tools/call params = %s: %v", calls[0].Params, err)
	}
	h.endTurn(call, 1, 101, "There is a bicycle for 120 euros.")
	sessionID, _ := h.activeSession("alice")
	var exposed bool
	if err := h.db.QueryRowContext(h.ctx, `SELECT exposed_at IS NOT NULL FROM conversation_sessions WHERE id = ?`, sessionID).Scan(&exposed); err != nil || !exposed {
		t.Fatalf("the conversation that called an MCP tool is not marked exposed: %v %v", exposed, err)
	}
}

// Two live servers offer their tools in catalog order, not in the order of their names, and each
// call reaches its own server.
func TestMCPToolsAreOfferedInCatalogOrder(t *testing.T) {
	t.Parallel()
	shop, family := newListingsServer(t), newListingsServer(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{sharedServer("shop", shop), sharedServer("family", family)}})
	h.provision("alice", "account-alice")
	h.start()
	h.setMCP("alice", "family", true)
	h.setMCP("alice", "shop", true)

	call := h.mcpTurn(runtime, 1, 101, "find me a bicycle")
	if names := mcpTools(call); !slices.Equal(names, []string{"mcp_shop_search", "mcp_family_search"}) {
		t.Fatalf("offered MCP tools = %v", names)
	}
	caller := toolCaller(t, call)
	for _, name := range []string{"mcp_shop_search", "mcp_family_search"} {
		if result := caller(name, `{"query":"bicycle"}`); result.IsError || !strings.Contains(result.Text, listingResult) {
			t.Fatalf("%s result = %+v", name, result)
		}
	}
	if shopCalls, familyCalls := len(shop.RequestsFor("tools/call")), len(family.RequestsFor("tools/call")); shopCalls != 1 || familyCalls != 1 {
		t.Fatalf("tools/call requests: shop %d, family %d", shopCalls, familyCalls)
	}
	h.endTurn(call, 1, 101, "two bicycles")
}

// A scheduled run is read-only and still gets the server's read tool.
func TestMCPToolReachesAScheduledRun(t *testing.T) {
	t.Parallel()
	sim := newListingsServer(t)
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, mcp: []config.MCPServer{sharedServer("shop", sim)}})
	h.provision("alice", "account-alice")
	h.start()
	h.setMCP("alice", "shop", true)
	job := h.createJob("alice", "00000000000000d1", "Bicycles", "0 9 * * *", "Look for new bicycle listings.")

	clock.Set(t, "2026-09-28T09:00:05+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if names := mcpTools(run); !slices.Equal(names, []string{"mcp_shop_search"}) {
		t.Fatalf("scheduled run MCP tools = %v", names)
	}
	if result := toolCaller(t, run)("mcp_shop_search", `{"query":"bicycle"}`); result.IsError || !strings.Contains(result.Text, listingResult) {
		t.Fatalf("result = %+v", result)
	}
	run.answer <- agentruntime.RunResult{Response: "One new bicycle.", Final: "One new bicycle."}
	h.wantMessage(101, "<b>Scheduled: Bicycles</b>\n\nOne new bicycle.")
	h.waitRun(job.ID, schedule.StatusDelivered)
}

// An agent watch's check gets the server's read tool and alerts on what it returned.
func TestMCPToolReachesAnAgentWatchThatAlertsOnItsResult(t *testing.T) {
	t.Parallel()
	sim := newListingsServer(t)
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, watches: &stubEvaluator{}, mcp: []config.MCPServer{sharedServer("shop", sim)}})
	h.provision("alice", "account-alice")
	h.start()
	h.setMCP("alice", "shop", true)
	const id = "00000000000000c7"
	spec := agentWatch()
	spec.Target.Name = "Bicycle price"
	spec.Check = &watch.Check{Instruction: "Search the listings for a bicycle and read its price.", Condition: "a bicycle is listed below 150 euros", IntervalMinutes: 60}
	h.createWatch("alice", id, spec)

	if err := h.watches.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.watches.Shutdown(context.Background()) })
	check := receive(t, h.ctx, runtime.calls)
	if check.request.PrincipalID != "alice" || !strings.Contains(check.request.Message, `watch "Bicycle price"`) {
		t.Fatalf("check turn = %s %q", check.request.PrincipalID, check.request.Message)
	}
	if names := mcpTools(check); !slices.Equal(names, []string{"mcp_shop_search"}) {
		t.Fatalf("check MCP tools = %v", names)
	}
	result := toolCaller(t, check)("mcp_shop_search", `{"query":"bicycle"}`)
	if result.IsError || !strings.Contains(result.Text, listingResult) {
		t.Fatalf("result = %+v", result)
	}
	check.answer <- agentruntime.RunResult{Final: `{"met": true, "value": "120 euros", "headline": "A bicycle is listed at 120 euros.", "details": "Found in the listings."}`}
	alert := h.nextMessage()
	if alert.chatID != 101 || !strings.Contains(alert.text, "Bicycle price") || !strings.Contains(alert.text, "A bicycle is listed at 120 euros.") {
		t.Fatalf("alert = %+v", alert)
	}
	h.waitAlertKept(id)
}

// A server is off until its user turns it on, and a user outside its principal list gets none of
// its tools even with a switch that says on.
func TestMCPServerOffOrNotForTheUserOffersNothing(t *testing.T) {
	t.Parallel()
	shop, family := newListingsServer(t), newListingsServer(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{sharedServer("shop", shop), sharedServer("family", family, "alice")}})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	call := h.mcpTurn(runtime, 1, 101, "hello")
	if names := mcpTools(call); len(names) != 0 {
		t.Fatalf("tools of servers nobody turned on = %v", names)
	}
	h.endTurn(call, 1, 101, "hi")

	h.setMCP("alice", "family", true)
	if _, err := h.mcp.SetEnabled(h.ctx, "bob", "family", true); !errors.Is(err, mcp.ErrNotAllowed) {
		t.Fatalf("bob turning on a server not for him: %v", err)
	}
	var familySwitch integration.Switch
	for _, sw := range h.mcp.Switches() {
		if sw.Integration == mcp.IntegrationName("family") {
			familySwitch = sw
		}
	}
	if err := h.switches.SetEnabled(h.ctx, "bob", familySwitch, true); err != nil {
		t.Fatal(err)
	}

	call = h.mcpTurn(runtime, 2, 101, "anything new?")
	if names := mcpTools(call); !slices.Equal(names, []string{"mcp_family_search"}) {
		t.Fatalf("alice's MCP tools = %v", names)
	}
	h.endTurn(call, 2, 101, "nothing")

	call = h.mcpTurn(runtime, 3, 202, "anything new?")
	if names := mcpTools(call); len(names) != 0 {
		t.Fatalf("bob's MCP tools = %v", names)
	}
	// The tool is not bound in Bob's turn, so this call ends before the provider; that a call reaching
	// the provider rechecks the principal list is proven by the connector's own test.
	if result := toolCaller(t, call)("mcp_family_search", `{"query":"bicycle"}`); !result.IsError || strings.Contains(result.Text, listingResult) {
		t.Fatalf("bob's call = %+v", result)
	}
	h.endTurn(call, 3, 202, "nothing")
	if calls := family.RequestsFor("tools/call"); len(calls) != 0 {
		t.Fatalf("the family server was called %d times", len(calls))
	}
}

// Two users connected their own keys to one server: each turn's requests carry only its user's
// key, in the user's own session.
func TestMCPKeysStayWithTheirOwners(t *testing.T) {
	t.Parallel()
	sim := mcpsim.New(t)
	sim.SetStateful(true)
	sim.SetTools([]mcpsim.Tool{{Name: "lookup", Description: "Look up a price."}}, 0)
	sim.SetResult("lookup", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text(priceResult)}})
	sim.RequireHeader("Authorization", "Bearer "+aliceMCPKey, "Bearer "+bobMCPKey)
	server := config.MCPServer{
		Name: "prices", Title: "Prices", Description: "Look up prices.", URL: sim.URL(), Auth: config.MCPAuthKey,
		KeyHeader: "Authorization", KeyPrefix: "Bearer ", Tools: []config.MCPTool{{Name: "lookup"}},
	}
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{server}})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	for principal, key := range map[string]string{"alice": aliceMCPKey, "bob": bobMCPKey} {
		if status, err := h.mcp.Connect(h.ctx, principal, "prices", key); err != nil || status.State != mcp.StateOn {
			t.Fatalf("connect %s: %+v %v", principal, status, err)
		}
	}

	sessions := map[int64]string{}
	for index, user := range []struct {
		updateID, userID int64
		key              string
	}{{1, 101, aliceMCPKey}, {2, 202, bobMCPKey}} {
		before := len(sim.Requests())
		call := h.mcpTurn(runtime, user.updateID, user.userID, "what does a kettle cost?")
		if names := mcpTools(call); !slices.Equal(names, []string{"mcp_prices_lookup"}) {
			t.Fatalf("turn %d MCP tools = %v", index, names)
		}
		if result := toolCaller(t, call)("mcp_prices_lookup", `{}`); result.IsError || !strings.Contains(result.Text, priceResult) {
			t.Fatalf("turn %d result = %+v", index, result)
		}
		h.endTurn(call, user.updateID, user.userID, "35 euros")
		requests := sim.Requests()[before:]
		if !slices.ContainsFunc(requests, func(r mcpsim.Request) bool { return r.RPCMethod == "tools/call" }) {
			t.Fatalf("turn %d made no call", index)
		}
		for _, request := range requests {
			if got := request.Header.Get("Authorization"); got != "Bearer "+user.key {
				t.Fatalf("turn %d sent %s with %q", index, request.RPCMethod, got)
			}
			if request.RPCMethod == "tools/call" {
				sessions[user.userID] = request.SessionID
			}
		}
	}
	if sessions[101] == "" || sessions[101] == sessions[202] {
		t.Fatalf("sessions = %v", sessions)
	}
}

// An exported MCP switch restores and takes effect. A restored switch for a server the catalog no
// longer lists, or for one outside the user's principal list, grants nothing.
func TestMCPSwitchesRestoreAndGrantNothingBeyondTheCatalog(t *testing.T) {
	t.Parallel()
	shop, family, old := newListingsServer(t), newListingsServer(t), newListingsServer(t)
	source := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{mcp: []config.MCPServer{
		sharedServer("shop", shop), sharedServer("family", family), sharedServer("old", old),
	}})
	source.provision("alice", "account-alice")
	source.provision("bob", "account-bob")
	source.start()
	source.setMCP("alice", "shop", true)
	source.setMCP("bob", "family", true)
	source.setMCP("bob", "old", true)
	aliceData := source.takeExport(1, 101)
	bobData := source.takeExport(2, 202)

	runtime := newControlledRuntime()
	target := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{
		sharedServer("shop", shop), sharedServer("family", family, "alice"),
	}})
	target.provision("alice", "account-alice")
	target.provision("bob", "account-bob")
	target.start()
	copyKey(t, source, target)

	preview, outcome := target.restore(1, 101, aliceData)
	if !strings.Contains(preview, "mcp-shop on") || !strings.Contains(outcome, "Restored") {
		t.Fatalf("alice's restore:\n%s\noutcome: %s", preview, outcome)
	}
	call := target.mcpTurn(runtime, 3, 101, "find me a bicycle")
	if names := mcpTools(call); !slices.Equal(names, []string{"mcp_shop_search"}) {
		t.Fatalf("alice's MCP tools after the restore = %v", names)
	}
	target.endTurn(call, 3, 101, "found one")

	preview, outcome = target.restore(4, 202, bobData)
	if !strings.Contains(preview, "mcp-family on") || !strings.Contains(preview, "mcp-old on") || !strings.Contains(outcome, "Restored") {
		t.Fatalf("bob's restore:\n%s\noutcome: %s", preview, outcome)
	}
	if restored := target.count(`SELECT count(*) FROM integration_switches WHERE principal_id = 'bob' AND integration IN ('mcp-family', 'mcp-old') AND enabled = 1`); restored != 2 {
		t.Fatalf("bob's restored MCP switches = %d", restored)
	}
	call = target.mcpTurn(runtime, 6, 202, "find me a bicycle")
	if names := mcpTools(call); len(names) != 0 {
		t.Fatalf("bob's MCP tools after the restore = %v", names)
	}
	target.endTurn(call, 6, 202, "nothing")
	if listed, err := target.mcp.Servers(target.ctx, "bob"); err != nil || len(listed) != 1 || listed[0].Name != "shop" {
		t.Fatalf("bob's servers = %+v %v", listed, err)
	}
	for name, sim := range map[string]*mcpsim.Simulator{"family": family, "old": old} {
		if calls := sim.RequestsFor("tools/call"); len(calls) != 0 {
			t.Fatalf("the %s server was called %d times", name, len(calls))
		}
	}
}

// A server that is down when the application starts neither stops it nor a turn: the turn goes on
// with the tools of the servers that answer.
func TestMCPServerDownAtStartupLeavesTheTurnWorking(t *testing.T) {
	t.Parallel()
	down, shop := newListingsServer(t), newListingsServer(t)
	down.SetDown(true)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{sharedServer("outage", down), sharedServer("shop", shop)}})
	h.provision("alice", "account-alice")
	h.start()
	if err := h.mcp.Ready(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.setMCP("alice", "outage", true)
	h.setMCP("alice", "shop", true)

	call := h.mcpTurn(runtime, 1, 101, "find me a bicycle")
	if names := mcpTools(call); !slices.Equal(names, []string{"mcp_shop_search"}) {
		t.Fatalf("MCP tools with one server down = %v", names)
	}
	if result := toolCaller(t, call)("mcp_shop_search", `{"query":"bicycle"}`); result.IsError || !strings.Contains(result.Text, listingResult) {
		t.Fatalf("result = %+v", result)
	}
	h.endTurn(call, 1, 101, "found one")
	if logs := h.logs.String(); strings.Contains(logs, mcpsim.ServerErrorText) {
		t.Fatalf("a server's error text reached the logs: %s", logs)
	}
}

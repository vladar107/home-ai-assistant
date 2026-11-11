package telegram

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/mcp"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/mcpsim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
)

// keyedServer is a catalog entry for a server that takes each user's own key as a bearer token.
func keyedServer(name string, sim *mcpsim.Simulator, principals ...string) config.MCPServer {
	server := sharedServer(name, sim, principals...)
	server.Title, server.Description = "Prices "+name, "Look up prices."
	server.Auth, server.KeyHeader, server.KeyPrefix = config.MCPAuthKey, "Authorization", "Bearer "
	return server
}

// mcpKeyFile reads the principal's stored key for the server.
func (h *conversationHarness) mcpKeyFile(principal, server string) (string, bool) {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.credentialsDir, principal, mcp.IntegrationName(server)+".json"))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	var record struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		h.t.Fatal(err)
	}
	return record.Token, true
}

func (h *conversationHarness) mcpStatus(principal, server string) mcp.ServerStatus {
	h.t.Helper()
	status, err := h.mcp.Status(h.ctx, principal, server)
	if err != nil {
		h.t.Fatal(err)
	}
	return status
}

// assertMCPKeysHidden checks the captured logs, every message and edit sent to Telegram, and the
// connection records for the keys.
func (h *conversationHarness) assertMCPKeysHidden(keys ...string) {
	h.t.Helper()
	rows, err := h.db.QueryContext(h.ctx, `SELECT principal_id, integration, token_kind, workspace_id, workspace_name, bot_name FROM integration_connections`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var stored []string
	for rows.Next() {
		var columns [6]string
		if err := rows.Scan(&columns[0], &columns[1], &columns[2], &columns[3], &columns[4], &columns[5]); err != nil {
			h.t.Fatal(err)
		}
		stored = append(stored, columns[:]...)
	}
	for _, key := range keys {
		if strings.Contains(h.logs.String(), key) {
			h.t.Fatal("logs contain an MCP key")
		}
		for _, text := range h.sentTexts() {
			if strings.Contains(text, key) {
				h.t.Fatal("a Telegram message contains an MCP key")
			}
		}
		for _, value := range stored {
			if strings.Contains(value, key) {
				h.t.Fatal("a connection record contains an MCP key")
			}
		}
	}
}

// assertNothingReachedTheModel waits until the poller passed the given offset and checks that no
// turn started and no message was deleted beyond what the test collected.
func (h *conversationHarness) assertNothingReachedTheModel(runtime *controlledRuntime, offset int64) {
	h.t.Helper()
	h.waitOffset(offset)
	select {
	case call := <-runtime.calls:
		h.t.Fatalf("an intake message reached the model: %q", call.request.Message)
	default:
	}
}

// wantDeleted expects the user's message of the update to be deleted.
func (h *conversationHarness) wantDeleted(userID, updateID int64) {
	h.t.Helper()
	if deleted := h.nextDelete(); deleted.chatID != userID || deleted.messageID != requestMessageID(updateID) {
		h.t.Fatalf("deleted %+v, want message %d of chat %d", deleted, requestMessageID(updateID), userID)
	}
}

func servicesPage(lines ...string) string {
	return strings.Join(append([]string{crumbs("Services"), "", ServicesLine}, lines...), "\n")
}

// The page shows each server the user may use, in catalog order, in each of its five states, with
// buttons that fit the state; a server in trouble raises attention in the overview. A user no
// server is for, and an application without a catalog, have no Services group at all.
func TestServicesPageShowsEachServerInItsState(t *testing.T) {
	t.Parallel()
	shop, market, prices, moved, quotes := newListingsServer(t), newListingsServer(t), newListingsServer(t), newListingsServer(t), newListingsServer(t)
	quotes.RequireHeader("Authorization", "Bearer "+aliceMCPKey)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{
		sharedServer("shop", shop, "alice"), sharedServer("market", market, "alice"),
		keyedServer("prices", prices, "alice"), keyedServer("moved", moved, "alice"), keyedServer("quotes", quotes, "alice"),
	}})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	h.setMCP("alice", "shop", true)
	h.setMCP("alice", "prices", true)
	for _, server := range []string{"moved", "quotes"} {
		if _, err := h.mcp.Connect(h.ctx, "alice", server, aliceMCPKey); err != nil {
			t.Fatalf("connect %s: %v", server, err)
		}
	}
	// As if the key was connected while the catalog named the server at another origin.
	if _, err := h.db.ExecContext(h.ctx, `UPDATE integration_connections SET workspace_id = 'https://moved.example' WHERE principal_id = 'alice' AND integration = 'mcp-moved'`); err != nil {
		t.Fatal(err)
	}

	call := h.mcpTurn(runtime, 1, 101, "what does a kettle cost?")
	if names := mcpTools(call); !slices.Equal(names, []string{"mcp_shop_search", "mcp_quotes_search"}) {
		t.Fatalf("offered MCP tools = %v", names)
	}
	quotes.RequireHeader("Authorization", "Bearer rotated-key-0123456789")
	if result := toolCaller(t, call)("mcp_quotes_search", `{}`); !result.IsError {
		t.Fatalf("a refused call = %+v", result)
	}
	h.endTurn(call, 1, 101, "the price service refused")

	overview := h.requestSettings(2, 101)
	attention := attentionMark + " Connect your key again for Prices moved, Prices quotes in Services."
	if !strings.Contains(overview.text, "Services: 1 of 5 on") || !strings.Contains(overview.text, attention) ||
		!slices.Equal(buttonData(overview.keyboard), []string{"nav:model", "nav:tools", "nav:services", "nav:memory", "nav:confirmations"}) ||
		!slices.ContainsFunc(keyboardTexts(overview.keyboard), func(text string) bool { return text == "Services "+attentionMark }) {
		t.Fatalf("overview = %+v", overview)
	}
	page := h.openSettingsPage(3, 101, settingsMessageID(2), "services")
	want := servicesPage(
		"", "Listings shop: On", "Search classified listings.",
		"", "Listings market: Off", "Search classified listings.",
		"", "Prices prices: Needs a key", "Look up prices.",
		"", "Prices moved: Needs reconnection", "Look up prices.",
		"", "Prices quotes: Problem", "Look up prices.",
	)
	if page.text != want {
		t.Fatalf("services page =\n%s\nwant\n%s", page.text, want)
	}
	wantRows := [][]InlineButton{
		{{Text: "Turn off Listings shop", CallbackData: "mcp:off:shop"}},
		{{Text: "Turn on Listings market", CallbackData: "mcp:on:market"}},
		{{Text: "Turn off Prices prices", CallbackData: "mcp:off:prices"}, {Text: "Connect Prices prices", CallbackData: "mcp:connect:prices"}},
		{{Text: "Connect Prices moved", CallbackData: "mcp:connect:moved"}, {Text: "Disconnect Prices moved", CallbackData: "mcp:disconnect:moved"}},
		{{Text: "Connect Prices quotes", CallbackData: "mcp:connect:quotes"}, {Text: "Disconnect Prices quotes", CallbackData: "mcp:disconnect:quotes"}},
		{{Text: settingsBackText, CallbackData: "nav:root"}},
	}
	if !slices.EqualFunc(page.keyboard, wantRows, slices.Equal) {
		t.Fatalf("services keyboard = %+v", page.keyboard)
	}

	bob := h.requestSettings(4, 202)
	if strings.Contains(bob.text, "Services") || !slices.Equal(buttonData(bob.keyboard), []string{"nav:model", "nav:tools", "nav:memory", "nav:confirmations"}) {
		t.Fatalf("bob's overview = %+v", bob)
	}
	if edit := h.press(5, 202, settingsMessageID(4), "mcp:on:shop"); strings.Contains(edit.text, "Services") {
		t.Fatalf("bob's forged press = %+v", edit)
	}
	if switches := h.count(`SELECT count(*) FROM integration_switches WHERE principal_id = 'bob'`); switches != 0 {
		t.Fatalf("bob's forged press stored %d switches", switches)
	}

	plain := newConversationHarness(t, newControlledRuntime())
	plain.provision("alice", "account-alice")
	plain.start()
	if settings := plain.requestSettings(1, 101); strings.Contains(settings.text, "Services") || slices.Contains(buttonData(settings.keyboard), "nav:services") {
		t.Fatalf("overview without a catalog = %+v", settings)
	}
}

func keyboardTexts(keyboard InlineKeyboard) []string {
	var texts []string
	for _, row := range keyboard {
		for _, button := range row {
			texts = append(texts, button.Text)
		}
	}
	return texts
}

// Turning a server on and off with its button changes what the next chat turn is offered.
func TestServicesSwitchDecidesWhatTheNextTurnIsOffered(t *testing.T) {
	t.Parallel()
	shop := newListingsServer(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{sharedServer("shop", shop)}})
	h.provision("alice", "account-alice")
	h.start()

	h.requestSettings(1, 101)
	if page := h.openSettingsPage(2, 101, settingsMessageID(1), "services"); !strings.Contains(page.text, "Listings shop: Off") ||
		!slices.Equal(buttonData(page.keyboard), []string{"mcp:on:shop", "nav:root"}) {
		t.Fatalf("page before turning on = %+v", page)
	}
	if edit := h.press(3, 101, settingsMessageID(1), "mcp:on:shop"); !strings.Contains(edit.text, "Listings shop: On") ||
		!slices.Equal(buttonData(edit.keyboard), []string{"mcp:off:shop", "nav:root"}) {
		t.Fatalf("page after turning on = %+v", edit)
	}
	call := h.mcpTurn(runtime, 4, 101, "find me a bicycle")
	if names := mcpTools(call); !slices.Equal(names, []string{"mcp_shop_search"}) {
		t.Fatalf("offered MCP tools after turning on = %v", names)
	}
	h.endTurn(call, 4, 101, "found one")

	if edit := h.press(5, 101, settingsMessageID(1), "mcp:off:shop"); !strings.Contains(edit.text, "Listings shop: Off") {
		t.Fatalf("page after turning off = %+v", edit)
	}
	call = h.mcpTurn(runtime, 6, 101, "find me a bicycle")
	if names := mcpTools(call); len(names) != 0 {
		t.Fatalf("offered MCP tools after turning off = %v", names)
	}
	h.endTurn(call, 6, 101, "nothing")
	if logs := h.logs.String(); !strings.Contains(logs, "mcp.switch.changed") {
		t.Fatalf("logs lack the switch change: %s", logs)
	}
}

// Connect asks for the key; the pasted key is deleted, stored, verified, and used on the next
// turn, and it reaches no log line, reply, or model. Disconnect removes it again.
func TestServicesConnectByPastingAKeyAndDisconnect(t *testing.T) {
	t.Parallel()
	prices := newListingsServer(t)
	prices.RequireHeader("Authorization", "Bearer "+aliceMCPKey)
	clock := newTestClock(t, mondayMorning)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{clock: clock, mcp: []config.MCPServer{keyedServer("prices", prices)}})
	h.provision("alice", "account-alice")
	h.start()

	h.requestSettings(1, 101)
	if page := h.openSettingsPage(2, 101, settingsMessageID(1), "services"); !strings.Contains(page.text, "Prices prices: Off") ||
		!slices.Equal(buttonData(page.keyboard), []string{"mcp:on:prices", "mcp:connect:prices", "nav:root"}) {
		t.Fatalf("page before connecting = %+v", page)
	}
	h.press(3, 101, settingsMessageID(1), "mcp:connect:prices")
	h.wantMessage(101, ServicesConnectInstructions("Prices prices", clock.Now().Add(10*time.Minute).In(time.UTC)))
	if instructions := ServicesConnectInstructions("Prices prices", clock.Now().Add(10*time.Minute).In(time.UTC)); !strings.Contains(instructions, "by 06:10.") {
		t.Fatalf("instructions = %q", instructions)
	}

	h.push(textUpdate(4, 101, aliceMCPKey))
	h.wantDeleted(101, 4)
	h.wantMessage(101, ServicesConnectedReply("Prices prices"))
	if key, found := h.mcpKeyFile("alice", "prices"); !found || key != aliceMCPKey {
		t.Fatalf("stored key found=%v", found)
	}
	if status := h.mcpStatus("alice", "prices"); status.State != mcp.StateOn || !status.Enabled {
		t.Fatalf("status after connecting = %+v", status)
	}
	h.requestSettings(5, 101)
	if page := h.openSettingsPage(6, 101, settingsMessageID(5), "services"); !strings.Contains(page.text, "Prices prices: On") ||
		!slices.Equal(buttonData(page.keyboard), []string{"mcp:off:prices", "mcp:disconnect:prices", "nav:root"}) {
		t.Fatalf("page after connecting = %+v", page)
	}

	before := len(prices.Requests())
	call := h.mcpTurn(runtime, 7, 101, "what does a kettle cost?")
	if call.request.Message != "what does a kettle cost?" {
		t.Fatalf("the turn after the key carries %q", call.request.Message)
	}
	if names := mcpTools(call); !slices.Equal(names, []string{"mcp_prices_search"}) {
		t.Fatalf("offered MCP tools after connecting = %v", names)
	}
	if result := toolCaller(t, call)("mcp_prices_search", `{"query":"kettle"}`); result.IsError || !strings.Contains(result.Text, listingResult) {
		t.Fatalf("result = %+v", result)
	}
	requests := prices.Requests()[before:]
	if !slices.ContainsFunc(requests, func(r mcpsim.Request) bool { return r.RPCMethod == "tools/call" }) {
		t.Fatal("the turn made no call")
	}
	for _, request := range requests {
		if got := request.Header.Get("Authorization"); got != "Bearer "+aliceMCPKey {
			t.Fatalf("%s carried %q", request.RPCMethod, got)
		}
	}
	for _, text := range []string{call.request.Message, call.request.Memory, call.request.Jobs, call.request.Tools.Guidance} {
		if strings.Contains(text, aliceMCPKey) {
			t.Fatal("the key reached the model")
		}
	}
	h.endTurn(call, 7, 101, "35 euros")
	if requests := h.count(`SELECT count(*) FROM conversation_requests WHERE telegram_update_id = 4`); requests != 0 {
		t.Fatalf("the key message was queued for the model %d times", requests)
	}

	edit := h.press(8, 101, settingsMessageID(5), "mcp:disconnect:prices")
	if !strings.Contains(edit.text, "Prices prices: Off") || !strings.Contains(edit.text, ServicesDisconnectedNote("Prices prices")) ||
		!slices.Equal(buttonData(edit.keyboard), []string{"mcp:on:prices", "mcp:connect:prices", "nav:root"}) {
		t.Fatalf("page after disconnecting = %+v", edit)
	}
	if _, found := h.mcpKeyFile("alice", "prices"); found {
		t.Fatal("the key file survived disconnecting")
	}
	before = len(prices.Requests())
	call = h.mcpTurn(runtime, 9, 101, "and a toaster?")
	if names := mcpTools(call); len(names) != 0 {
		t.Fatalf("offered MCP tools after disconnecting = %v", names)
	}
	h.endTurn(call, 9, 101, "no prices")
	if after := len(prices.Requests()); after != before {
		t.Fatalf("the server was contacted %d times after disconnecting", after-before)
	}

	// Turned on again without a key, the server waits for one.
	if edit := h.press(10, 101, settingsMessageID(5), "mcp:on:prices"); !strings.Contains(edit.text, "Prices prices: Needs a key") {
		t.Fatalf("page after turning on without a key = %+v", edit)
	}
	h.assertMCPKeysHidden(aliceMCPKey)
}

// What the intake cannot use is deleted and sent nowhere: a message that is not a key ends it, a
// refused or unverified key leaves it waiting, and once it expired an ordinary message goes to the
// model again.
func TestServicesKeyIntakeTakesOnlyAKeyInTime(t *testing.T) {
	t.Parallel()
	prices, quotes := newListingsServer(t), newListingsServer(t)
	prices.RequireHeader("Authorization", "Bearer "+aliceMCPKey)
	quotes.SetDown(true)
	clock := newTestClock(t, mondayMorning)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{clock: clock, mcp: []config.MCPServer{keyedServer("prices", prices), keyedServer("quotes", quotes)}})
	h.provision("alice", "account-alice")
	h.start()
	h.requestSettings(1, 101)
	connect := func(updateID int64, server string) {
		t.Helper()
		h.press(updateID, 101, settingsMessageID(1), "mcp:connect:"+server)
		h.wantMessage(101, ServicesConnectInstructions("Prices "+server, clock.Now().Add(10*time.Minute).In(time.UTC)))
	}

	connect(2, "prices")
	h.push(textUpdate(3, 101, "where do I find my key?"))
	h.wantDeleted(101, 3)
	h.wantMessage(101, ServicesNotAKeyReply("Prices prices"))
	h.assertNothingReachedTheModel(runtime, 4)
	if requests := prices.Requests(); len(requests) != 0 {
		t.Fatalf("a message that is not a key reached the server: %d requests", len(requests))
	}
	if _, found := h.mcpKeyFile("alice", "prices"); found {
		t.Fatal("a message that is not a key was stored")
	}

	// A refused key stores nothing and the intake still takes the next paste.
	connect(4, "prices")
	h.push(textUpdate(5, 101, bobMCPKey))
	h.wantDeleted(101, 5)
	h.wantMessage(101, ServicesRefusedReply("Prices prices"))
	if _, found := h.mcpKeyFile("alice", "prices"); found {
		t.Fatal("a refused key was stored")
	}
	h.push(textUpdate(6, 101, aliceMCPKey))
	h.wantDeleted(101, 6)
	h.wantMessage(101, ServicesConnectedReply("Prices prices"))
	if key, found := h.mcpKeyFile("alice", "prices"); !found || key != aliceMCPKey {
		t.Fatalf("stored key found=%v", found)
	}

	// A server that cannot be reached leaves the intake waiting; a failed deletion is reported.
	connect(7, "quotes")
	h.mu.Lock()
	h.deleteStatus = 400
	h.mu.Unlock()
	h.push(textUpdate(8, 101, bobMCPKey))
	h.wantDeleted(101, 8)
	h.wantMessage(101, ServicesUnverifiedReply("Prices quotes")+" "+DeleteFailedLine)
	h.mu.Lock()
	h.deleteStatus = 0
	h.mu.Unlock()
	if server, pending := h.mcp.PendingIntake("alice"); !pending || server != "quotes" {
		t.Fatalf("pending intake after an unverified key = %q %v", server, pending)
	}

	// A key pasted after the intake expired is still deleted and sent nowhere.
	clock.Set(t, "2026-09-28T08:11:00+02:00")
	h.push(textUpdate(9, 101, bobMCPKey))
	h.wantDeleted(101, 9)
	h.wantMessage(101, ServicesExpiredReply("Prices quotes"))
	h.assertNothingReachedTheModel(runtime, 10)

	// After an expired intake an ordinary message is conversation again.
	connect(10, "quotes")
	clock.Set(t, "2026-09-28T08:22:00+02:00")
	requests := len(quotes.Requests())
	call := h.mcpTurn(runtime, 11, 101, "hello model")
	if call.request.Message != "hello model" {
		t.Fatalf("message after an expired intake = %q", call.request.Message)
	}
	h.endTurn(call, 11, 101, "hello")
	if after := len(quotes.Requests()); after != requests {
		t.Fatalf("an ordinary message reached the server: %d requests", after-requests)
	}
	h.assertMCPKeysHidden(aliceMCPKey, bobMCPKey)
	if logs := h.logs.String(); strings.Contains(logs, "where do I find my key") {
		t.Fatalf("logs contain an intake message: %s", logs)
	}
}

// A key pasted during an intake is taken when it matches the pattern of the server the intake waits
// for, even if it also looks like a Notion token. For a server without a pattern, a message another
// section's guard recognises, such as a Notion token or a Google sign-in address, ends the intake
// and is handled by that guard, and nothing reaches the server. With no intake pending the guards
// are unchanged, and a message shaped like one of the user's keys is kept from the model.
func TestServicesIntakeAndTheOtherGuards(t *testing.T) {
	t.Parallel()
	const notionLooking = "secret_0123456789abcdefKEY"
	prices, quotes := newListingsServer(t), newListingsServer(t)
	prices.RequireHeader("Authorization", "Bearer "+notionLooking)
	patterned := keyedServer("prices", prices)
	patterned.KeyPattern = regexp.MustCompile(`^secret_[A-Za-z0-9]{16,}$`)
	notionSim := notionsim.New(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: notionSim, google: googlesim.New(t), mcp: []config.MCPServer{patterned, keyedServer("quotes", quotes)}})
	h.provision("alice", "account-alice")
	h.start()
	h.requestSettings(1, 101)
	connect := func(updateID int64, server string) {
		t.Helper()
		h.press(updateID, 101, settingsMessageID(1), "mcp:connect:"+server)
		if instructions := h.nextMessage(); !strings.HasPrefix(instructions.text, "Send your key for Prices "+server+" as your next message by ") {
			t.Fatalf("instructions = %+v", instructions)
		}
	}

	connect(2, "prices")
	h.push(textUpdate(3, 101, notionLooking))
	h.wantDeleted(101, 3)
	h.wantMessage(101, ServicesConnectedReply("Prices prices"))
	if key, found := h.mcpKeyFile("alice", "prices"); !found || key != notionLooking {
		t.Fatalf("stored key found=%v", found)
	}

	connect(4, "quotes")
	h.push(textUpdate(5, 101, "secret_abcDEF123"))
	h.wantDeleted(101, 5)
	h.wantMessage(101, NotionTokenGuardReply)
	if _, pending := h.mcp.PendingIntake("alice"); pending {
		t.Fatal("a Notion token left the intake pending")
	}

	connect(6, "quotes")
	h.push(textUpdate(7, 101, "http://localhost/?code=4/0AbCdEfGhIjKlMnOp&scope=email"))
	h.wantDeleted(101, 7)
	h.wantMessage(101, GoogleNoSignInReply)
	if _, pending := h.mcp.PendingIntake("alice"); pending {
		t.Fatal("a sign-in address left the intake pending")
	}
	if requests := quotes.Requests(); len(requests) != 0 {
		t.Fatalf("another service's secret reached the server: %d requests", len(requests))
	}
	if _, found := h.mcpKeyFile("alice", "quotes"); found {
		t.Fatal("another service's secret was stored as a key")
	}

	h.push(textUpdate(8, 101, "secret_abcDEF123"))
	h.wantDeleted(101, 8)
	h.wantMessage(101, NotionTokenGuardReply)
	h.push(textUpdate(9, 101, "secret_fedcba9876543210XYZ"))
	h.wantDeleted(101, 9)
	h.wantMessage(101, ServicesKeyGuardReply)
	h.assertNothingReachedTheModel(runtime, 10)
	if requests := notionSim.Requests(); len(requests) != 0 {
		t.Fatalf("Notion was called %d times", len(requests))
	}
	h.assertMCPKeysHidden(notionLooking, "secret_abcDEF123", "secret_fedcba9876543210XYZ")
}

// A key or a message that is not one, consumed and deleted, is consumed again when its update is
// retried after the reply failed, instead of reaching the model.
func TestServicesRetriedIntakeUpdateNeverReachesTheModel(t *testing.T) {
	t.Parallel()
	prices := newListingsServer(t)
	prices.RequireHeader("Authorization", "Bearer "+aliceMCPKey)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{keyedServer("prices", prices)}})
	h.provision("alice", "account-alice")
	h.start()
	h.requestSettings(1, 101)

	for _, attempt := range []struct {
		updateID int64
		text     string
		reply    string
	}{
		{3, "where is my key?", ServicesNotAKeyReply("Prices prices")},
		{5, aliceMCPKey, ServicesConnectedReply("Prices prices")},
	} {
		h.press(attempt.updateID-1, 101, settingsMessageID(1), "mcp:connect:prices")
		h.nextMessage()
		h.failNextSend()
		h.push(textUpdate(attempt.updateID, 101, attempt.text))
		h.wantDeleted(101, attempt.updateID)
		if failed := h.nextMessage(); failed.status != http.StatusInternalServerError || failed.text != attempt.reply {
			t.Fatalf("first reply = %+v", failed)
		}
		// Telegram hands the update over again once the bot asks from the same offset.
		h.waitOffset(attempt.updateID)
		h.push(textUpdate(attempt.updateID, 101, attempt.text))
		h.wantDeleted(101, attempt.updateID)
		h.wantMessage(101, attempt.reply)
		h.assertNothingReachedTheModel(runtime, attempt.updateID+1)
	}
	if key, found := h.mcpKeyFile("alice", "prices"); !found || key != aliceMCPKey {
		t.Fatalf("stored key found=%v", found)
	}
	h.assertMCPKeysHidden(aliceMCPKey)
}

// A key sent in a burst with another message is offered alone, so it is consumed and deleted and
// the other message reaches the model as it was sent.
func TestServicesKeyInABurstIsOfferedAlone(t *testing.T) {
	t.Parallel()
	prices := newListingsServer(t)
	prices.RequireHeader("Authorization", "Bearer "+aliceMCPKey)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{keyedServer("prices", prices)}})
	h.provision("alice", "account-alice")
	h.start()
	h.requestSettings(1, 101)
	h.press(2, 101, settingsMessageID(1), "mcp:connect:prices")
	h.nextMessage()

	h.push(textUpdate(3, 101, aliceMCPKey), textUpdate(4, 101, "thanks, what does a kettle cost?"))
	h.wantDeleted(101, 3)
	h.wantMessage(101, ServicesConnectedReply("Prices prices"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.Message != "thanks, what does a kettle cost?" {
		t.Fatalf("the model received %q", call.request.Message)
	}
	h.endTurn(call, 4, 101, "35 euros")
	h.assertMCPKeysHidden(aliceMCPKey)
}

// During an intake a caption is offered like a text message, so a key sent as a photo's caption is
// consumed with its message; without an intake, a caption shaped like one of the user's keys is
// kept from the model.
func TestServicesKeyAsACaptionNeverReachesTheModel(t *testing.T) {
	t.Parallel()
	const patternedKey = "pk_0123456789abcdefghij"
	prices, tokens := newListingsServer(t), newListingsServer(t)
	prices.RequireHeader("Authorization", "Bearer "+aliceMCPKey)
	patterned := keyedServer("tokens", tokens)
	patterned.KeyPattern = regexp.MustCompile(`^pk_[a-z0-9]{20}$`)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{keyedServer("prices", prices), patterned}})
	h.provision("alice", "account-alice")
	h.start()
	h.requestSettings(1, 101)
	h.press(2, 101, settingsMessageID(1), "mcp:connect:prices")
	h.nextMessage()

	h.push(photoUpdate(3, 101, aliceMCPKey, "photo-1", 2048))
	h.wantDeleted(101, 3)
	h.wantMessage(101, ServicesConnectedReply("Prices prices"))
	if key, found := h.mcpKeyFile("alice", "prices"); !found || key != aliceMCPKey {
		t.Fatalf("stored key found=%v", found)
	}
	h.push(documentUpdate(4, 101, patternedKey, MediaFile{FileID: "doc-1", FileName: "notes.txt", MimeType: "text/plain", FileSize: 64}))
	h.wantDeleted(101, 4)
	h.wantMessage(101, ServicesKeyGuardReply)
	h.assertNothingReachedTheModel(runtime, 5)
	if requests := tokens.Requests(); len(requests) != 0 {
		t.Fatalf("a guarded caption reached the server: %d requests", len(requests))
	}
	h.assertMCPKeysHidden(aliceMCPKey, patternedKey)
}

// Forged presses change nothing: a server the catalog does not name, and Connect or Disconnect on
// a server that takes no key.
func TestServicesForgedPressesChangeNothing(t *testing.T) {
	t.Parallel()
	shop := newListingsServer(t)
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{mcp: []config.MCPServer{sharedServer("shop", shop)}})
	h.provision("alice", "account-alice")
	h.start()
	h.requestSettings(1, 101)
	h.setMCP("alice", "shop", true)
	for i, data := range []string{"mcp:on:nosuchserver", "mcp:connect:nosuchserver", "mcp:connect:shop", "mcp:disconnect:shop", "mcp:on", "mcp:bogus:shop"} {
		if edit := h.press(int64(2+i), 101, settingsMessageID(1), data); !strings.Contains(edit.text, "Listings shop: On") {
			t.Fatalf("page after %q = %+v", data, edit)
		}
	}
	if _, pending := h.mcp.PendingIntake("alice"); pending {
		t.Fatal("a forged Connect started an intake")
	}
	if status := h.mcpStatus("alice", "shop"); !status.Enabled {
		t.Fatal("a forged Disconnect turned the server off")
	}
	if rows := h.count(`SELECT count(*) FROM integration_switches WHERE integration <> 'mcp-shop'`); rows != 0 {
		t.Fatalf("forged presses stored %d switches", rows)
	}
	// Nothing was sent for the forged presses, so the next message is the answer to this one.
	h.push(textUpdate(8, 101, "/privacy"))
	h.wantReply(101, 8, privacyLines(AttachmentPrivacyLine, SchedulePrivacyLine, ServicePrivacyLine("Listings shop"), VoicePrivacyLine, MemoryPrivacyLine))
}

// A full catalog of twelve servers with the longest names, titles, and descriptions renders
// within Telegram's limits, and every button routes to its own server.
func TestServicesFullCatalogFitsAndRoutesEveryButton(t *testing.T) {
	t.Parallel()
	sim := newListingsServer(t)
	var catalog []config.MCPServer
	for i := range 12 {
		name := fmt.Sprintf("server%010d", i)
		server := sharedServer(name, sim)
		server.Title = fmt.Sprintf("Service %02d ", i) + strings.Repeat("🛒", 37)
		server.Description = strings.Repeat("d", 160)
		if i%2 == 1 {
			server = keyedServer(name, sim)
			server.Title = fmt.Sprintf("Service %02d ", i) + strings.Repeat("🛒", 37)
			server.Description = strings.Repeat("🔎", 160)
		}
		if len(server.Name) != 16 || len([]rune(server.Title)) != 48 {
			t.Fatalf("test setup: %q %q", server.Name, server.Title)
		}
		catalog = append(catalog, server)
	}
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: catalog})
	h.provision("alice", "account-alice")
	h.start()
	for i := 1; i < 12; i += 4 {
		if _, err := h.mcp.Connect(h.ctx, "alice", catalog[i].Name, aliceMCPKey); err != nil {
			t.Fatal(err)
		}
	}
	if longest := servicesCallback(mcpDisconnectAction, catalog[1].Name); len(longest) != 31 || len(longest) > maximumCallbackDataBytes {
		t.Fatalf("longest callback %q is %d bytes", longest, len(longest))
	}

	fits := func(text string, keyboard InlineKeyboard) {
		t.Helper()
		buttons := 0
		for _, row := range keyboard {
			if len(row) > 8 {
				t.Fatalf("a row of %d buttons", len(row))
			}
			for _, button := range row {
				buttons++
				if len(button.CallbackData) > maximumCallbackDataBytes {
					t.Fatalf("callback %q is %d bytes", button.CallbackData, len(button.CallbackData))
				}
			}
		}
		if units := utf16Length(text); units > messageLimit || buttons > 100 {
			t.Fatalf("page of %d UTF-16 units and %d buttons", units, buttons)
		}
	}
	h.requestSettings(1, 101)
	page := h.openSettingsPage(2, 101, settingsMessageID(1), "services")
	fits(page.text, page.keyboard)
	if len(page.keyboard) != 13 || !strings.Contains(page.text, strings.Repeat("🔎", 79)+"…") {
		t.Fatalf("page has %d rows:\n%s", len(page.keyboard), page.text)
	}

	enabled := func() []bool {
		t.Helper()
		states := make([]bool, 0, len(catalog))
		for _, server := range catalog {
			states = append(states, h.mcpStatus("alice", server.Name).Enabled)
		}
		return states
	}
	// changedOnly checks that, of all servers, only the i-th one's switch moved.
	changedOnly := func(before, after []bool, i int, data string) {
		t.Helper()
		for j := range catalog {
			if (j == i) != (before[j] != after[j]) {
				t.Fatalf("pressing %q moved %s from %v to %v", data, catalog[j].Name, before[j], after[j])
			}
		}
	}
	update := int64(3)
	press := func(data string) editedMessage {
		t.Helper()
		edit := h.press(update, 101, settingsMessageID(1), data)
		update++
		fits(edit.text, edit.keyboard)
		return edit
	}
	for i, server := range catalog {
		row := page.keyboard[i]
		for _, button := range row {
			if !strings.HasSuffix(button.CallbackData, ":"+server.Name) || !strings.HasSuffix(button.Text, server.Title) {
				t.Fatalf("row %d has %+v", i, button)
			}
		}
		before := enabled()
		press(row[0].CallbackData)
		changedOnly(before, enabled(), i, row[0].CallbackData)
		if len(row) < 2 {
			continue
		}
		before = enabled()
		switch second := row[1].CallbackData; {
		case strings.HasPrefix(second, "mcp:connect:"):
			press(second)
			if instructions := h.nextMessage(); !strings.HasPrefix(instructions.text, "Send your key for "+server.Title+" as your next message by ") {
				t.Fatalf("instructions for %s = %q", server.Name, instructions.text)
			}
			if pending, _ := h.mcp.PendingIntake("alice"); pending != server.Name {
				t.Fatalf("intake after pressing %q is for %q", second, pending)
			}
			h.mcp.CancelIntake("alice")
		case strings.HasPrefix(second, "mcp:disconnect:"):
			if edit := press(second); !strings.Contains(edit.text, ServicesDisconnectedNote(server.Title)) {
				t.Fatalf("note after pressing %q:\n%s", second, edit.text)
			}
			if _, found := h.mcpKeyFile("alice", server.Name); found {
				t.Fatalf("pressing %q left the key", second)
			}
		default:
			t.Fatalf("row %d ends with %q", i, second)
		}
		for j := range catalog {
			if j != i && before[j] != enabled()[j] {
				t.Fatalf("pressing %q moved %s", row[1].CallbackData, catalog[j].Name)
			}
		}
	}
}

// /privacy has a line for each server that is on and none for one that is off, and app_help on the
// page names its real buttons and explains how to turn a server on and connect a key.
func TestServicesPrivacyAndHelp(t *testing.T) {
	t.Parallel()
	shop, prices := newListingsServer(t), newListingsServer(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{mcp: []config.MCPServer{sharedServer("shop", shop), keyedServer("prices", prices)}})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "/privacy"))
	h.wantReply(101, 1, privacyLines(AttachmentPrivacyLine, SchedulePrivacyLine, VoicePrivacyLine, MemoryPrivacyLine))
	h.setMCP("alice", "shop", true)
	h.push(textUpdate(2, 101, "/privacy"))
	h.wantReply(101, 2, privacyLines(AttachmentPrivacyLine, SchedulePrivacyLine, ServicePrivacyLine("Listings shop"), VoicePrivacyLine, MemoryPrivacyLine))

	h.requestSettings(3, 101)
	page := h.openSettingsPage(4, 101, settingsMessageID(3), "services")
	call := h.mcpTurn(runtime, 5, 101, "how do I connect the price service?")
	help := h.callTool(call, "app_help", `{"topic":"services"}`)
	for _, want := range []string{
		`Where: send /settings, then press "Services".`, page.text,
		"Buttons on it: Turn off Listings shop | Turn on Prices prices | Connect Prices prices", ServicesHelp,
	} {
		if help.IsError || !strings.Contains(help.Text, want) {
			t.Fatalf("services help lacks %q:\n%s", want, help.Text)
		}
	}
	h.endTurn(call, 5, 101, "Press Connect Prices prices.")
}

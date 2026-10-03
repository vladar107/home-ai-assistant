package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
)

const (
	aliceNotionToken = "ntn_alicetoken123"
	bobNotionToken   = "ntn_bobpersonal456"
)

var (
	aliceWorkspace = notionsim.Token{Kind: "internal", BotName: "Home Agent", WorkspaceID: "ws-alice", WorkspaceName: "Alice Home"}
	bobWorkspace   = notionsim.Token{Kind: "personal", BotName: "Bob", WorkspaceID: "ws-bob"}
	aliceStatus    = notion.Status{Connected: true, TokenKind: "internal", WorkspaceName: "Alice Home", BotName: "Home Agent"}
	bobStatus      = notion.Status{Connected: true, TokenKind: "personal", BotName: "Bob"}
)

// installNotionToken places a token file the way the operator script does.
func (h *conversationHarness) installNotionToken(principal, token string) {
	h.t.Helper()
	directory := filepath.Join(h.credentialsDir, principal)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "notion.json"), []byte(`{"token":"`+token+`"}`), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *conversationHarness) notionTokenFile(principal string) (string, bool) {
	h.t.Helper()
	path := filepath.Join(h.credentialsDir, principal, "notion.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		h.t.Fatalf("token file permissions: info=%v err=%v", info, err)
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || directory.Mode().Perm() != 0o700 {
		h.t.Fatalf("token directory permissions: info=%v err=%v", directory, err)
	}
	var record struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		h.t.Fatal(err)
	}
	return record.Token, true
}

func (h *conversationHarness) nextDelete() deletedMessage { return receive(h.t, h.ctx, h.deletes) }

// assertNotionSecretHidden checks logs, every sent message, and the database for the tokens.
func (h *conversationHarness) assertNotionSecretHidden(tokens ...string) {
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
	for _, token := range tokens {
		if strings.Contains(h.logs.String(), token) {
			h.t.Fatal("logs contain a Notion token")
		}
		for _, text := range h.sentTexts() {
			if strings.Contains(text, token) {
				h.t.Fatal("a Telegram message contains a Notion token")
			}
		}
		for _, value := range stored {
			if strings.Contains(value, token) {
				h.t.Fatal("the database contains a Notion token")
			}
		}
	}
}

func usersMeRequests(sim *notionsim.Simulator) []notionsim.Request {
	var calls []notionsim.Request
	for _, request := range sim.Requests() {
		if request.Path == "/v1/users/me" {
			calls = append(calls, request)
		}
	}
	return calls
}

func TestNotionOperatorTokenIsAdoptedAndDisconnected(t *testing.T) {
	runtime := newControlledRuntime()
	sim := notionsim.New(t)
	sim.AddToken(aliceNotionToken, aliceWorkspace)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: sim})
	h.provision("alice", "acct-alice-1111")
	h.provision("bob", "acct-bob-2222")
	h.installNotionToken("alice", aliceNotionToken)
	h.installNotionToken("bob", "ntn_bogus999")
	h.start()

	overview := h.requestSettings(1, 101)
	if !strings.Contains(overview.text, "Accounts: Notion writes off") || !slices.Equal(buttonData(overview.keyboard), []string{"nav:model", "nav:accounts"}) {
		t.Fatalf("settings overview after adoption = %+v", overview)
	}
	settings := h.openSettingsPage(2, 101, settingsMessageID(1), "accounts")
	if !strings.Contains(settings.text, NotionConnectedLine(aliceStatus)) || !slices.Equal(buttonData(settings.keyboard), []string{"notion:writes-on", "notion:disconnect", "nav:root"}) {
		t.Fatalf("settings after adoption = %+v", settings)
	}
	record, found, err := h.store.LoadIntegrationConnection(h.ctx, "alice", "notion")
	if err != nil || !found || record.TokenKind != "internal" || record.WorkspaceID != "ws-alice" || record.WorkspaceName != "Alice Home" || record.BotName != "Home Agent" || record.WritesEnabled {
		t.Fatalf("connection row = %+v found=%v err=%v", record, found, err)
	}
	if calls := usersMeRequests(sim); len(calls) != 1 || calls[0].Token != aliceNotionToken || calls[0].Version != notionsim.Version {
		t.Fatalf("adoption requests = %+v", calls)
	}
	h.requestSettings(3, 101)
	if calls := usersMeRequests(sim); len(calls) != 1 {
		t.Fatalf("an adopted token was verified again: %+v", calls)
	}

	h.requestSettings(4, 202)
	bobSettings := h.openSettingsPage(5, 202, settingsMessageID(4), "accounts")
	if !strings.Contains(bobSettings.text, NotionInvalidTokenLine) || !slices.Equal(buttonData(bobSettings.keyboard), []string{"notion:disconnect", "nav:root"}) {
		t.Fatalf("settings with an invalid file = %+v", bobSettings)
	}
	if _, exists := h.notionTokenFile("bob"); !exists {
		t.Fatal("an invalid operator file was removed")
	}
	if _, found, err := h.store.LoadIntegrationConnection(h.ctx, "bob", "notion"); err != nil || found {
		t.Fatalf("invalid token created a row: found=%v err=%v", found, err)
	}

	h.push(textUpdate(6, 101, "secret_abcDEF123"))
	if deleted := h.nextDelete(); deleted.chatID != 101 || deleted.messageID != requestMessageID(6) {
		t.Fatalf("token guard deleted %+v", deleted)
	}
	h.wantMessage(101, NotionTokenGuardReply)
	h.push(textUpdate(7, 101, "/privacy"))
	h.wantReply(101, 7, ConversationPrivacyReply+" "+AttachmentPrivacyLine+" "+NotionPrivacyLine)
	select {
	case call := <-runtime.calls:
		t.Fatalf("a token-looking message reached the model: %+v", call.request)
	default:
	}

	edit := h.press(8, 101, settingsMessageID(1), "notion:disconnect")
	if !strings.Contains(edit.text, NotionOperatorLine) || !slices.Equal(buttonData(edit.keyboard), []string{"nav:root"}) {
		t.Fatalf("settings after disconnect = %+v", edit)
	}
	if _, exists := h.notionTokenFile("alice"); exists {
		t.Fatal("token file survived disconnect")
	}
	if _, found, err := h.store.LoadIntegrationConnection(h.ctx, "alice", "notion"); err != nil || found {
		t.Fatalf("connection row survived disconnect: found=%v err=%v", found, err)
	}
	h.assertNotionSecretHidden(aliceNotionToken, "ntn_bogus999", "secret_abcDEF123")
}

func TestNotionChatConnectIntake(t *testing.T) {
	runtime := newControlledRuntime()
	sim := notionsim.New(t)
	sim.AddToken(aliceNotionToken, aliceWorkspace)
	sim.AddToken(bobNotionToken, bobWorkspace)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: sim, notionChatConnect: true, notionIntakeTimeout: 300 * time.Millisecond})
	h.provision("alice", "acct-alice-1111")
	h.provision("bob", "acct-bob-2222")
	h.start()

	overview := h.requestSettings(1, 101)
	if !strings.Contains(overview.text, "Accounts: Notion not connected") || !slices.Equal(buttonData(overview.keyboard), []string{"nav:model", "nav:accounts"}) {
		t.Fatalf("settings overview before connect = %+v", overview)
	}
	settings := h.openSettingsPage(2, 101, settingsMessageID(1), "accounts")
	if !strings.Contains(settings.text, NotionNotConnectedLine) || !slices.Equal(buttonData(settings.keyboard), []string{"notion:connect", "nav:root"}) {
		t.Fatalf("settings before connect = %+v", settings)
	}
	if edit := h.press(3, 101, settingsMessageID(1), "notion:connect"); !strings.Contains(edit.text, NotionNotConnectedLine) {
		t.Fatalf("settings after pressing connect = %+v", edit)
	}
	h.wantMessage(101, NotionConnectInstructions)
	h.push(textUpdate(4, 101, aliceNotionToken))
	if deleted := h.nextDelete(); deleted.chatID != 101 || deleted.messageID != requestMessageID(4) {
		t.Fatalf("token message deletion = %+v", deleted)
	}
	h.wantMessage(101, NotionConnectedReply(aliceStatus))
	if token, exists := h.notionTokenFile("alice"); !exists || token != aliceNotionToken {
		t.Fatalf("token file after connect: exists=%v", exists)
	}
	if record, found, err := h.store.LoadIntegrationConnection(h.ctx, "alice", "notion"); err != nil || !found || record.TokenKind != "internal" {
		t.Fatalf("connection row = %+v found=%v err=%v", record, found, err)
	}
	h.requestSettings(5, 101)
	if after := h.openSettingsPage(6, 101, settingsMessageID(5), "accounts"); !strings.Contains(after.text, NotionConnectedLine(aliceStatus)) {
		t.Fatalf("settings after connect = %+v", after)
	}

	// A second press replaces the pending intake; a rejected token is deleted and nothing stored.
	h.requestSettings(7, 202)
	for _, id := range []int64{8, 9} {
		h.press(id, 202, settingsMessageID(7), "notion:connect")
		h.wantMessage(202, NotionConnectInstructions)
	}
	h.push(textUpdate(10, 202, "ntn_wrongtoken"))
	if deleted := h.nextDelete(); deleted.chatID != 202 || deleted.messageID != requestMessageID(10) {
		t.Fatalf("rejected token deletion = %+v", deleted)
	}
	h.wantMessage(202, NotionInvalidTokenReply)
	if _, exists := h.notionTokenFile("bob"); exists {
		t.Fatal("a rejected token was stored")
	}

	// Something that is not a token ends the intake without deletion or a model turn.
	h.press(11, 202, settingsMessageID(7), "notion:connect")
	h.wantMessage(202, NotionConnectInstructions)
	h.push(textUpdate(12, 202, "how do I get a token?"))
	h.wantMessage(202, NotionNotATokenReply)
	h.waitOffset(13)
	select {
	case deleted := <-h.deletes:
		t.Fatalf("a non-token message was deleted: %+v", deleted)
	case call := <-runtime.calls:
		t.Fatalf("an intake message reached the model: %+v", call.request)
	default:
	}

	// An expired intake treats the next message as conversation.
	h.press(13, 202, settingsMessageID(7), "notion:connect")
	h.wantMessage(202, NotionConnectInstructions)
	time.Sleep(400 * time.Millisecond)
	h.push(textUpdate(14, 202, "hello model"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.PrincipalID != "bob" || call.request.Message != "hello model" || !call.request.Tools.Empty() {
		t.Fatalf("expired intake request = %+v", call.request)
	}
	call.answer <- agentruntime.RunResult{Response: "hi bob"}
	h.wantReply(202, 14, "hi bob")
	h.waitState(14, "delivered")

	// A failed deletion still connects and asks the user to delete the message.
	h.mu.Lock()
	h.deleteStatus = 400
	h.mu.Unlock()
	h.press(15, 202, settingsMessageID(7), "notion:connect")
	h.wantMessage(202, NotionConnectInstructions)
	h.push(textUpdate(16, 202, bobNotionToken))
	h.nextDelete()
	h.wantMessage(202, NotionConnectedReply(bobStatus)+" "+DeleteFailedLine)
	h.requestSettings(17, 202)
	if after := h.openSettingsPage(18, 202, settingsMessageID(17), "accounts"); !strings.Contains(after.text, NotionConnectedLine(bobStatus)) || !strings.Contains(after.text, "personal access token") {
		t.Fatalf("settings after personal connect = %+v", after)
	}
	h.assertNotionSecretHidden(aliceNotionToken, bobNotionToken, "ntn_wrongtoken")
	if logs := h.logs.String(); strings.Contains(logs, "how do I get a token") || strings.Contains(logs, "synthetic-notion-error-text") {
		t.Fatalf("logs contain message or provider text: %s", logs)
	}
}

func TestNotionToolsAreScopedToTheConnectedPrincipal(t *testing.T) {
	runtime := newControlledRuntime()
	sim := notionsim.New(t)
	sim.AddToken(aliceNotionToken, aliceWorkspace)
	sim.AddToken(bobNotionToken, bobWorkspace)
	sim.ShareDataSource(aliceNotionToken, notionsim.DataSource{
		ID: "ds-tasks", Title: "Tasks", URL: "https://notion.so/ds-tasks",
		Properties: map[string]any{"Status": notionsim.SchemaProperty("status"), "Due": notionsim.SchemaProperty("date")},
	})
	sim.SharePage(aliceNotionToken, notionsim.Page{
		ID: "page-groceries", Title: "Groceries", URL: "https://notion.so/page-groceries", LastEdited: "2026-09-05T10:00:00.000Z", DataSourceID: "ds-tasks",
		Properties: map[string]any{"Status": notionsim.StatusProperty("Open"), "Due": notionsim.DateProperty("2026-09-10")},
		Blocks:     []map[string]any{notionsim.Heading(2, "Buy"), notionsim.Bullet("Milk"), notionsim.ToDo("Eggs", false)},
	})
	sim.SharePage(aliceNotionToken, notionsim.Page{
		ID: "page-done", Title: "Done task", URL: "https://notion.so/page-done", LastEdited: "2026-09-04T10:00:00.000Z", DataSourceID: "ds-tasks",
		Properties: map[string]any{"Status": notionsim.StatusProperty("Done")},
	})
	sim.SharePage(bobNotionToken, notionsim.Page{ID: "page-bob", Title: "Bob private", URL: "https://notion.so/page-bob", LastEdited: "2026-09-03T10:00:00.000Z"})
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: sim})
	h.provision("alice", "acct-alice-1111")
	h.provision("bob", "acct-bob-2222")
	h.installNotionToken("alice", aliceNotionToken)
	h.start()

	h.push(textUpdate(1, 101, "what is on my grocery list?"))
	alice := receive(t, h.ctx, runtime.calls)
	var names []string
	for _, definition := range alice.request.Tools.Definitions {
		names = append(names, definition.Name)
	}
	if !slices.Equal(names, []string{"notion_search", "notion_get_page", "notion_query_data_source", "notion_get_block_children", "movies_next"}) || !strings.Contains(alice.request.Tools.Guidance, "notion_search") {
		t.Fatalf("Alice's tools = %v guidance=%q", names, alice.request.Tools.Guidance)
	}
	execute := func(name, arguments string) agentruntime.ToolResult {
		t.Helper()
		result, err := alice.request.Tools.Executor.Execute(alice.ctx, agentruntime.ToolCall{Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := execute("notion_search", `{"query":"groc"}`); result.IsError || !strings.Contains(result.Text, "Groceries") ||
		!strings.Contains(result.Text, "page-groceries") || !strings.Contains(result.Text, "Status: Open") || strings.Contains(result.Text, "Bob private") {
		t.Fatalf("search result = %+v", result)
	}
	if result := execute("notion_search", `{"object_type":"data_source"}`); result.IsError || !strings.Contains(result.Text, "[data_source] Tasks") ||
		!strings.Contains(result.Text, "Due (date)") || strings.Contains(result.Text, "Groceries") {
		t.Fatalf("data source search = %+v", result)
	}
	if result := execute("notion_get_page", `{"page_id":"page-groceries"}`); result.IsError || !strings.Contains(result.Text, "## Buy") ||
		!strings.Contains(result.Text, "- Milk") || !strings.Contains(result.Text, "[ ] Eggs") || !strings.Contains(result.Text, "Due: 2026-09-10") ||
		!strings.Contains(result.Text, "parent: data source ds-tasks") {
		t.Fatalf("page result = %+v", result)
	}
	if result := execute("notion_query_data_source", `{"data_source_id":"ds-tasks","filter":{"property":"Status","status":{"equals":"Done"}}}`); result.IsError ||
		!strings.Contains(result.Text, "Done task") || strings.Contains(result.Text, "Groceries") {
		t.Fatalf("query result = %+v", result)
	}
	if result := execute("notion_query_data_source", `{"data_source_id":"ds-tasks","filter":{"bogus":1}}`); !result.IsError ||
		!strings.Contains(result.Text, "rejected the request") || strings.Contains(result.Text, "synthetic-notion-error-text") {
		t.Fatalf("malformed filter result = %+v", result)
	}
	if result := execute("notion_get_page", `{"page_id":"page-bob"}`); !result.IsError || !strings.Contains(result.Text, "no shared page") {
		t.Fatalf("unshared page result = %+v", result)
	}
	for _, request := range sim.Requests() {
		if request.Token != aliceNotionToken {
			t.Fatalf("Alice's turn used another token: %+v", request)
		}
	}
	alice.answer <- agentruntime.RunResult{Response: "milk and eggs"}
	h.wantReply(101, 1, "milk and eggs")
	h.waitState(1, "delivered")

	h.push(textUpdate(2, 202, "hi"))
	bob := receive(t, h.ctx, runtime.calls)
	if !bob.request.Tools.Empty() {
		t.Fatalf("Bob received tools without a connection: %+v", bob.request.Tools)
	}
	bob.answer <- agentruntime.RunResult{Response: "hello"}
	h.wantReply(202, 2, "hello")
	h.waitState(2, "delivered")

	h.installNotionToken("bob", bobNotionToken)
	before := len(sim.Requests())
	h.push(textUpdate(3, 202, "search my pages"))
	bob = receive(t, h.ctx, runtime.calls)
	if len(bob.request.Tools.Definitions) != 5 {
		t.Fatalf("Bob's tools after connecting = %+v", bob.request.Tools)
	}
	result, err := bob.request.Tools.Executor.Execute(bob.ctx, agentruntime.ToolCall{Name: "notion_search", Arguments: json.RawMessage(`{"query":"bob"}`)})
	if err != nil || result.IsError || !strings.Contains(result.Text, "Bob private") || strings.Contains(result.Text, "Groceries") {
		t.Fatalf("Bob's search = %+v %v", result, err)
	}
	for _, request := range sim.Requests()[before:] {
		if request.Token != bobNotionToken {
			t.Fatalf("Bob's turn used another token: %+v", request)
		}
	}
	bob.answer <- agentruntime.RunResult{Response: "found"}
	h.wantReply(202, 3, "found")
	h.waitState(3, "delivered")

	// Disconnecting during a turn makes the next execution fail closed without a request.
	h.push(textUpdate(4, 101, "again"))
	alice = receive(t, h.ctx, runtime.calls)
	if len(alice.request.Tools.Definitions) != 5 {
		t.Fatalf("Alice's tools = %+v", alice.request.Tools)
	}
	h.requestSettings(5, 101)
	if edit := h.press(6, 101, settingsMessageID(5), "notion:disconnect"); !strings.Contains(edit.text, NotionOperatorLine) {
		t.Fatalf("settings after disconnect = %+v", edit)
	}
	before = len(sim.Requests())
	result, err = alice.request.Tools.Executor.Execute(alice.ctx, agentruntime.ToolCall{Name: "notion_search", Arguments: json.RawMessage(`{"query":"groc"}`)})
	if err != nil || !result.IsError || !strings.Contains(result.Text, "no longer available") || len(sim.Requests()) != before {
		t.Fatalf("execution after disconnect = %+v %v (requests %d → %d)", result, err, before, len(sim.Requests()))
	}
	alice.answer <- agentruntime.RunResult{Response: "done"}
	h.wantReply(101, 4, "done")
	h.waitState(4, "delivered")
	h.assertNotionSecretHidden(aliceNotionToken, bobNotionToken)
	if logs := h.logs.String(); !strings.Contains(logs, "notion.connection.adopted") || strings.Contains(logs, "Groceries") || strings.Contains(logs, "Milk") {
		t.Fatalf("logs missing adoption or containing content: %s", logs)
	}
}

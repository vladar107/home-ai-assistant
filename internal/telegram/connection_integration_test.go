package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/codexauth"
)

var userCodePattern = regexp.MustCompile(`enter the code ([A-Z0-9-]+)\.`)

func buttonData(keyboard InlineKeyboard) []string {
	var data []string
	for _, row := range keyboard {
		for _, button := range row {
			data = append(data, button.CallbackData)
		}
	}
	return data
}

// requestSettings sends /settings and returns the reply, which must quote the request and carry the frame.
func (h *conversationHarness) requestSettings(updateID, userID int64) sentMessage {
	h.t.Helper()
	h.push(textUpdate(updateID, userID, "/settings"))
	message := h.nextMessage()
	if message.chatID != userID || message.replyTo != requestMessageID(updateID) ||
		!strings.HasPrefix(message.text, SettingsTitle+"\n") || !strings.HasSuffix(message.text, SettingsPrivacyLine) {
		h.t.Fatalf("settings reply = %+v", message)
	}
	return message
}

// press submits a button press and expects the answer followed by the refreshed settings page.
func (h *conversationHarness) press(updateID, userID, messageID int64, data string) editedMessage {
	h.t.Helper()
	h.push(callbackUpdate(updateID, userID, messageID, data))
	h.wantAnswer(updateID)
	edit := h.nextEdit()
	if edit.chatID != userID || edit.messageID != messageID || !strings.HasPrefix(edit.text, SettingsTitle) {
		h.t.Fatalf("settings edit = %+v", edit)
	}
	return edit
}

// openSettingsPage opens a settings page such as "tools" or "accounts.google" on an existing
// settings message.
func (h *conversationHarness) openSettingsPage(updateID, userID, messageID int64, page string) editedMessage {
	h.t.Helper()
	return h.press(updateID, userID, messageID, settingsNavPrefix+":"+page)
}

// requestConnect opens the settings and the model page, presses Connect, and returns the user code
// from the instructions. It consumes three update IDs starting at firstUpdateID.
func (h *conversationHarness) requestConnect(firstUpdateID, userID int64) string {
	h.t.Helper()
	settings := h.requestSettings(firstUpdateID, userID)
	if !strings.Contains(settings.text, "Model: not connected") || !slices.Equal(buttonData(settings.keyboard), []string{"nav:model", "nav:tools", "nav:memory"}) {
		h.t.Fatalf("settings before connect = %+v", settings)
	}
	page := h.openSettingsPage(firstUpdateID+1, userID, settingsMessageID(firstUpdateID), "model")
	if !strings.Contains(page.text, ModelNotConnectedLine) || !slices.Equal(buttonData(page.keyboard), []string{"model:connect", "nav:root"}) {
		h.t.Fatalf("model page before connect = %+v", page)
	}
	if edit := h.press(firstUpdateID+2, userID, settingsMessageID(firstUpdateID), "model:connect"); !strings.Contains(edit.text, ModelNotConnectedLine) {
		h.t.Fatalf("settings after pressing connect = %+v", edit)
	}
	message := h.nextMessage()
	match := userCodePattern.FindStringSubmatch(message.text)
	if message.chatID != userID || message.replyTo != 0 || match == nil || !strings.Contains(message.text, h.auth.URL()+"/codex/device") {
		h.t.Fatalf("connect instructions = %+v", message)
	}
	if match[1] != h.auth.LatestUserCode() {
		h.t.Fatalf("instructions code %q is not the simulator's latest %q", match[1], h.auth.LatestUserCode())
	}
	return match[1]
}

func (h *conversationHarness) credentialFile(principal string) (map[string]any, bool) {
	h.t.Helper()
	path := filepath.Join(h.profilesDir, principal, "auth.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		h.t.Fatalf("credential file permissions: info=%v err=%v", info, err)
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || directory.Mode().Perm() != 0o700 {
		h.t.Fatalf("profile directory permissions: info=%v err=%v", directory, err)
	}
	var stored map[string]map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		h.t.Fatal(err)
	}
	record, ok := stored["openai-codex"]
	if !ok {
		h.t.Fatalf("credential file has no provider record: %s", data)
	}
	return record, true
}

func (h *conversationHarness) assertNoSecretLeaked(accountID string) {
	h.t.Helper()
	for _, secret := range []string{codexauth.AccessToken(accountID), codexauth.RefreshToken(accountID)} {
		if strings.Contains(h.logs.String(), secret) {
			h.t.Fatal("logs contain a token")
		}
		for _, text := range h.sentTexts() {
			if strings.Contains(text, secret) {
				h.t.Fatal("a Telegram message contains a token")
			}
		}
	}
}

func TestConnectCeremonyBindsSubscription(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.start()
	for range 2 {
		registration := receive(t, h.ctx, h.registrations)
		var names []string
		for _, command := range registration.commands {
			names = append(names, command.Command)
		}
		if !slices.Contains(names, "settings") || slices.ContainsFunc(names, func(name string) bool {
			return name == "connect" || name == "connection" || name == "disconnect"
		}) {
			t.Fatalf("registered commands %v, want settings without the connection commands", names)
		}
	}
	code := h.requestConnect(1, 101)
	if _, exists := h.credentialFile("alice"); exists {
		t.Fatal("credential written before approval")
	}
	h.auth.Approve(code, "acct-alice-1234")
	h.wantMessage(101, connection.ConnectedReply)

	record, exists := h.credentialFile("alice")
	if !exists {
		t.Fatal("credential file missing after approval")
	}
	expires, _ := record["expires"].(float64)
	if record["type"] != "oauth" || record["access"] != codexauth.AccessToken("acct-alice-1234") ||
		record["refresh"] != codexauth.RefreshToken("acct-alice-1234") || record["accountId"] != "acct-alice-1234" ||
		int64(expires) <= time.Now().UnixMilli() {
		t.Fatalf("credential record = %v", record)
	}
	stored, found, err := h.store.LoadModelConnection(h.ctx, "alice")
	if err != nil || !found || stored.AccountID != "acct-alice-1234" || stored.Provider != "openai-codex" {
		t.Fatalf("connection row = %+v found=%v err=%v", stored, found, err)
	}
	var sawStart, sawExchange bool
	for _, request := range h.auth.Requests() {
		switch request.Path {
		case "/api/accounts/deviceauth/usercode":
			sawStart = request.Body["client_id"] == connection.DefaultClientID
		case "/oauth/token":
			sawExchange = request.Body["client_id"] == connection.DefaultClientID && request.Body["grant_type"] == "authorization_code"
		}
	}
	if !sawStart || !sawExchange {
		t.Fatalf("provider requests did not follow the protocol: %+v", h.auth.Requests())
	}

	settings := h.requestSettings(4, 101)
	if !strings.Contains(settings.text, "Model: connected") || !slices.Equal(buttonData(settings.keyboard), []string{"nav:model", "nav:tools", "nav:memory"}) {
		t.Fatalf("settings after connect = %+v", settings)
	}
	if page := h.openSettingsPage(5, 101, settingsMessageID(4), "model"); !strings.Contains(page.text, "ending in 1234") || !slices.Equal(buttonData(page.keyboard), []string{"model:disconnect", "nav:root"}) {
		t.Fatalf("model page after connect = %+v", page)
	}
	h.push(textUpdate(6, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.PrincipalID != "alice" {
		t.Fatalf("unexpected principal %+v", call.request)
	}
	call.answer <- agentruntimeResult("hi")
	h.wantReply(101, 6, "hi")
	h.waitState(6, "delivered")
	h.assertNoSecretLeaked("acct-alice-1234")
}

func TestConnectOutcomes(t *testing.T) {
	t.Run("denied", func(t *testing.T) {
		h := newConversationHarness(t, newControlledRuntime())
		h.start()
		code := h.requestConnect(1, 101)
		h.auth.Deny(code)
		h.wantMessage(101, connection.ConnectDeniedReply)
		if _, exists := h.credentialFile("alice"); exists {
			t.Fatal("denied ceremony wrote a credential")
		}
		settings := h.requestSettings(4, 101)
		if !strings.Contains(settings.text, "Model: not connected") || !slices.Equal(buttonData(settings.keyboard), []string{"nav:model", "nav:tools", "nav:memory"}) {
			t.Fatalf("settings after denial = %+v", settings)
		}
		if page := h.openSettingsPage(5, 101, settingsMessageID(4), "model"); !slices.Equal(buttonData(page.keyboard), []string{"model:connect", "nav:root"}) {
			t.Fatalf("model page after denial = %+v", page)
		}
	})
	t.Run("expired", func(t *testing.T) {
		h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{codeTimeout: 2 * time.Second})
		h.start()
		h.requestConnect(1, 101)
		h.wantMessage(101, connection.ConnectExpiredReply)
		if _, exists := h.credentialFile("alice"); exists {
			t.Fatal("expired ceremony wrote a credential")
		}
	})
	t.Run("replaced", func(t *testing.T) {
		h := newConversationHarness(t, newControlledRuntime())
		h.start()
		first := h.requestConnect(1, 101)
		second := h.requestConnect(4, 101)
		h.auth.Approve(first, "acct-old")
		h.auth.Approve(second, "acct-new")
		h.wantMessage(101, connection.ConnectedReply)
		record, exists := h.credentialFile("alice")
		if !exists || record["accountId"] != "acct-new" {
			t.Fatalf("credential = %v exists=%v", record, exists)
		}
		h.requestSettings(7, 101)
		if page := h.openSettingsPage(8, 101, settingsMessageID(7), "model"); !strings.Contains(page.text, "ending in -new") {
			t.Fatalf("after replacement got %+v", page)
		}
	})
	t.Run("pending as error then approved", func(t *testing.T) {
		h := newConversationHarness(t, newControlledRuntime())
		h.start()
		code := h.requestConnect(1, 101)
		h.auth.PendingAsError(code)
		h.auth.Approve(code, "acct-alice")
		h.wantMessage(101, connection.ConnectedReply)
	})
	t.Run("slow down", func(t *testing.T) {
		h := newConversationHarness(t, newControlledRuntime())
		h.start()
		code := h.requestConnect(1, 101)
		h.auth.SlowDownOnce(code)
		h.auth.Approve(code, "acct-alice")
		h.wantMessage(101, connection.ConnectedReply)
	})
}

func TestAccountBoundToAnotherPrincipalIsRejected(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-shared")
	h.start()
	code := h.requestConnect(1, 202)
	h.auth.Approve(code, "acct-shared")
	h.wantMessage(202, connection.AccountAlreadyBoundReply)
	if _, exists := h.credentialFile("bob"); exists {
		t.Fatal("rejected ceremony wrote a credential")
	}
	if _, found, err := h.store.LoadModelConnection(h.ctx, "bob"); err != nil || found {
		t.Fatalf("bob has a connection row: found=%v err=%v", found, err)
	}
	h.push(textUpdate(4, 202, "hello"))
	h.wantReply(202, 4, conversation.NotConnectedReply)
	h.waitState(4, "delivered")
	select {
	case call := <-runtime.calls:
		t.Fatalf("unconnected principal reached the runtime: %+v", call.request)
	default:
	}
}

func TestUnconnectedPrincipalGetsGuidance(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()
	h.push(textUpdate(1, 202, "hello"))
	h.wantReply(202, 1, conversation.NotConnectedReply)
	h.waitState(1, "delivered")
	if turn := receive(t, h.ctx, h.turns); turn.outcome != "error" || turn.errorKind != "not_connected" {
		t.Fatalf("telemetry turn = %+v", turn)
	}
	h.push(textUpdate(2, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.PrincipalID != "alice" {
		t.Fatalf("unexpected principal %+v", call.request)
	}
	call.answer <- agentruntimeResult("hi")
	h.wantReply(101, 2, "hi")
	h.waitState(2, "delivered")
	select {
	case call := <-runtime.calls:
		t.Fatalf("unconnected principal reached the runtime: %+v", call.request)
	default:
	}
}

func TestDisconnectCancelsRunningTurnAndBlocksQueuedWork(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()
	h.push(textUpdate(1, 101, "long request"))
	call := receive(t, h.ctx, runtime.calls)
	settings := h.requestSettings(2, 101)
	if !strings.Contains(settings.text, "Model: connected") || !slices.Equal(buttonData(settings.keyboard), []string{"nav:model", "nav:tools", "nav:memory"}) {
		t.Fatalf("settings while connected = %+v", settings)
	}
	if page := h.openSettingsPage(3, 101, settingsMessageID(2), "model"); !strings.Contains(page.text, "ending in lice") || !slices.Equal(buttonData(page.keyboard), []string{"model:disconnect", "nav:root"}) {
		t.Fatalf("model page while connected = %+v", page)
	}
	edit := h.press(4, 101, settingsMessageID(2), "model:disconnect")
	if !strings.Contains(edit.text, ModelNotConnectedLine) || !slices.Equal(buttonData(edit.keyboard), []string{"model:connect", "nav:root"}) {
		t.Fatalf("settings after disconnect = %+v", edit)
	}
	receive(t, h.ctx, call.ctx.Done())
	h.waitState(1, "cancelled")
	if _, exists := h.credentialFile("alice"); exists {
		t.Fatal("credential survived disconnect")
	}
	if _, found, err := h.store.LoadModelConnection(h.ctx, "alice"); err != nil || found {
		t.Fatalf("connection row survived disconnect: found=%v err=%v", found, err)
	}
	h.push(textUpdate(5, 101, "hello again"))
	h.wantReply(101, 5, conversation.NotConnectedReply)
	h.waitState(5, "delivered")
	// A stale Disconnect button on the old settings message only refreshes the view.
	if edit := h.press(6, 101, settingsMessageID(2), "model:disconnect"); !strings.Contains(edit.text, ModelNotConnectedLine) {
		t.Fatalf("stale disconnect rendered %+v", edit)
	}
	select {
	case call := <-runtime.calls:
		t.Fatalf("disconnected principal reached the runtime: %+v", call.request)
	default:
	}
}

func TestConnectionSurvivesRestartButPendingCeremonyDoesNot(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("bob", "acct-bob")
	h.start()
	code := h.requestConnect(1, 101)
	h.waitOffset(4)
	h.stop()
	h.start()
	h.auth.Approve(code, "acct-alice")
	if settings := h.requestSettings(4, 101); !strings.Contains(settings.text, "Model: not connected") {
		t.Fatalf("stale ceremony connected after restart: %+v", settings)
	}
	h.push(textUpdate(5, 202, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.PrincipalID != "bob" {
		t.Fatalf("unexpected principal %+v", call.request)
	}
	call.answer <- agentruntimeResult("hi bob")
	h.wantReply(202, 5, "hi bob")
	h.waitState(5, "delivered")
	fresh := h.requestConnect(6, 101)
	h.auth.Approve(fresh, "acct-alice")
	h.wantMessage(101, connection.ConnectedReply)
	if _, exists := h.credentialFile("alice"); !exists {
		t.Fatal("fresh ceremony after restart did not connect")
	}
}

func agentruntimeResult(text string) agentruntime.RunResult {
	return agentruntime.RunResult{Response: text}
}

package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/codexauth"
)

var userCodePattern = regexp.MustCompile(`enter the code ([A-Z0-9-]+)\.`)

func (h *conversationHarness) requestConnect(updateID, userID int64) string {
	h.t.Helper()
	h.push(textUpdate(updateID, userID, "/connect"))
	message := h.nextMessage()
	match := userCodePattern.FindStringSubmatch(message.text)
	if message.chatID != userID || match == nil || !strings.Contains(message.text, h.auth.URL()+"/codex/device") {
		h.t.Fatalf("connect reply = %+v", message)
	}
	if match[1] != h.auth.LatestUserCode() {
		h.t.Fatalf("reply code %q is not the simulator's latest %q", match[1], h.auth.LatestUserCode())
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
		for _, want := range []string{"connect", "connection", "disconnect"} {
			if !strings.Contains(strings.Join(names, ","), want) {
				t.Fatalf("registered commands %v lack %s", names, want)
			}
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

	h.push(textUpdate(2, 101, "/connection"))
	if message := h.nextMessage(); !strings.Contains(message.text, "ending in 1234") {
		t.Fatalf("connection status = %+v", message)
	}
	h.push(textUpdate(3, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.PrincipalID != "alice" {
		t.Fatalf("unexpected principal %+v", call.request)
	}
	call.answer <- agentruntimeResult("hi")
	h.wantMessage(101, "hi")
	h.waitState(3, "delivered")
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
		h.push(textUpdate(2, 101, "/connection"))
		h.wantMessage(101, NoConnectionReply)
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
		second := h.requestConnect(2, 101)
		h.auth.Approve(first, "acct-old")
		h.auth.Approve(second, "acct-new")
		h.wantMessage(101, connection.ConnectedReply)
		record, exists := h.credentialFile("alice")
		if !exists || record["accountId"] != "acct-new" {
			t.Fatalf("credential = %v exists=%v", record, exists)
		}
		h.push(textUpdate(3, 101, "/connection"))
		if message := h.nextMessage(); !strings.Contains(message.text, "ending in -new") {
			t.Fatalf("after replacement got %+v", message)
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
	h.push(textUpdate(2, 202, "hello"))
	h.wantMessage(202, conversation.NotConnectedReply)
	h.waitState(2, "delivered")
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
	h.wantMessage(202, conversation.NotConnectedReply)
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
	h.wantMessage(101, "hi")
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
	h.push(textUpdate(2, 101, "/disconnect"))
	h.wantMessage(101, DisconnectedReply)
	receive(t, h.ctx, call.ctx.Done())
	h.waitState(1, "cancelled")
	if _, exists := h.credentialFile("alice"); exists {
		t.Fatal("credential survived disconnect")
	}
	if _, found, err := h.store.LoadModelConnection(h.ctx, "alice"); err != nil || found {
		t.Fatalf("connection row survived disconnect: found=%v err=%v", found, err)
	}
	h.push(textUpdate(3, 101, "hello again"))
	h.wantMessage(101, conversation.NotConnectedReply)
	h.waitState(3, "delivered")
	h.push(textUpdate(4, 101, "/disconnect"))
	h.wantMessage(101, NoConnectionReply)
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
	h.waitOffset(2)
	h.stop()
	h.start()
	h.auth.Approve(code, "acct-alice")
	h.push(textUpdate(2, 101, "/connection"))
	h.wantMessage(101, NoConnectionReply)
	h.push(textUpdate(3, 202, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.PrincipalID != "bob" {
		t.Fatalf("unexpected principal %+v", call.request)
	}
	call.answer <- agentruntimeResult("hi bob")
	h.wantMessage(202, "hi bob")
	h.waitState(3, "delivered")
	fresh := h.requestConnect(4, 101)
	h.auth.Approve(fresh, "acct-alice")
	h.wantMessage(101, connection.ConnectedReply)
	if _, exists := h.credentialFile("alice"); !exists {
		t.Fatal("fresh ceremony after restart did not connect")
	}
}

func agentruntimeResult(text string) agentruntime.RunResult {
	return agentruntime.RunResult{Response: text}
}

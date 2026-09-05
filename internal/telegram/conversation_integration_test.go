package telegram

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil/codexauth"
)

const integrationTimeout = 15 * time.Second

const (
	recoverySessionID        = "11111111-1111-4111-8111-111111111111"
	recoveryRuntimeSessionID = "22222222-2222-4222-8222-222222222222"
	recoveryRequestID        = "33333333-3333-4333-8333-333333333333"
)

type runtimeCall struct {
	request agentruntime.RunRequest
	ctx     context.Context
	answer  chan agentruntime.RunResult
}

type controlledRuntime struct{ calls chan runtimeCall }

func newControlledRuntime() *controlledRuntime {
	return &controlledRuntime{calls: make(chan runtimeCall, 20)}
}

func (r *controlledRuntime) Run(ctx context.Context, request agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
	call := runtimeCall{request: request, ctx: ctx, answer: make(chan agentruntime.RunResult, 1)}
	r.calls <- call
	select {
	case result := <-call.answer:
		return result, nil
	case <-ctx.Done():
		return agentruntime.RunResult{Response: "late answer after cancellation"}, nil
	}
}

type sentMessage struct {
	chatID int64
	text   string
	status int
}

type commandRegistration struct {
	commands []BotCommand
	scope    []string
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

type harnessOptions struct {
	profilesDir string
	codeTimeout time.Duration
}

type conversationHarness struct {
	t             *testing.T
	ctx           context.Context
	store         *sqlite.Store
	db            *sql.DB
	client        *Client
	service       *Service
	conversation  *conversation.Service
	connections   *connection.Service
	auth          *codexauth.Simulator
	profilesDir   string
	sent          []string
	updates       chan []Update
	messages      chan sentMessage
	offsets       chan int64
	registrations chan commandRegistration
	turns         chan turnOutcome
	logs          *lockedBuffer
	mu            sync.Mutex
	sendStatuses  []int
	sendGate      <-chan struct{}
	commandStatus int
}

type turnOutcome struct{ outcome, errorKind string }

type integrationTelemetry struct{ turns chan turnOutcome }

func (integrationTelemetry) ContextLogger(_ context.Context, logger *slog.Logger) *slog.Logger {
	return logger
}
func (i integrationTelemetry) StartConversationTurn(ctx context.Context) (context.Context, func(string, string)) {
	return ctx, func(outcome, errorKind string) {
		select {
		case i.turns <- turnOutcome{outcome: outcome, errorKind: errorKind}:
		default:
		}
	}
}

func newConversationHarness(t *testing.T, runtime agentruntime.AgentRuntime) *conversationHarness {
	t.Helper()
	return newConversationHarnessWithOptions(t, runtime, harnessOptions{})
}

func newConversationHarnessWithOptions(t *testing.T, runtime agentruntime.AgentRuntime, options harnessOptions) *conversationHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	t.Cleanup(cancel)
	h := &conversationHarness{t: t, ctx: ctx, updates: make(chan []Update, 20), messages: make(chan sentMessage, 100), offsets: make(chan int64, 100), registrations: make(chan commandRegistration, 10), turns: make(chan turnOutcome, 20), logs: &lockedBuffer{}}
	server := httptest.NewServer(http.HandlerFunc(h.serveHTTP))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "state", "assistant.sqlite")
	var err error
	h.store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.store.Close(); err != nil {
			t.Error(err)
		}
	})
	h.db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.db.Close() })
	identities, err := identity.NewService(h.store, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 202},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := identities.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	h.client, err = NewClient(ClientConfig{BaseURL: server.URL, Token: "999:test-token", PollTimeout: time.Second, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(h.logs, nil))
	h.profilesDir = options.profilesDir
	if h.profilesDir == "" {
		h.profilesDir = filepath.Join(t.TempDir(), "profiles")
	}
	h.auth = codexauth.New(t)
	h.connections, err = connection.NewService(connection.Config{
		ProfilesDir: h.profilesDir, AuthBaseURL: h.auth.URL(), HTTPClient: h.auth.Client(),
		CodeTimeout: options.codeTimeout, Principals: []string{"alice", "bob"},
	}, h.store, h.client, logger)
	if err != nil {
		t.Fatal(err)
	}
	h.conversation, err = conversation.NewService(conversation.Config{TurnTimeout: 10 * time.Second}, h.store, runtime, h.client, conversation.NoTools{}, h.connections, logger, integrationTelemetry{turns: h.turns})
	if err != nil {
		t.Fatal(err)
	}
	h.service, err = NewService(h.client, identities, h.store, h.conversation, h.connections, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.stop)
	return h
}

func (h *conversationHarness) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.t.Error(err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var result any = struct{}{}
	switch filepath.Base(r.URL.Path) {
	case "getMe":
		result = Bot{ID: 999, Username: "assistant_test_bot"}
	case "getUpdates":
		offset, err := strconv.ParseInt(r.Form.Get("offset"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		select {
		case h.offsets <- offset:
		case <-r.Context().Done():
			return
		}
		select {
		case result = <-h.updates:
		case <-r.Context().Done():
			return
		}
	case "sendMessage":
		chatID, err := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		h.mu.Lock()
		status := http.StatusOK
		if len(h.sendStatuses) > 0 {
			status = h.sendStatuses[0]
			h.sendStatuses = h.sendStatuses[1:]
		}
		gate := h.sendGate
		h.sent = append(h.sent, r.Form.Get("text"))
		h.mu.Unlock()
		select {
		case h.messages <- sentMessage{chatID: chatID, text: r.Form.Get("text"), status: status}:
		case <-r.Context().Done():
			return
		}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status})
			return
		}
	case "sendChatAction":
		result = true
	case "setMyCommands":
		registration := commandRegistration{scope: r.Form["scope"]}
		if err := json.Unmarshal([]byte(r.Form.Get("commands")), &registration.commands); err != nil {
			h.t.Error(err)
			return
		}
		h.mu.Lock()
		status := h.commandStatus
		h.mu.Unlock()
		select {
		case h.registrations <- registration:
		case <-r.Context().Done():
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status})
			return
		}
		result = true
	default:
		h.t.Errorf("unexpected Telegram method: %s", r.URL.Path)
		w.WriteHeader(404)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result}); err != nil && r.Context().Err() == nil {
		h.t.Error(err)
	}
}

func (h *conversationHarness) start() {
	h.t.Helper()
	if err := h.connections.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if err := h.conversation.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if err := h.service.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *conversationHarness) stop() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.service.Shutdown(ctx); err != nil {
		h.t.Error(err)
	}
	if err := h.conversation.Shutdown(ctx); err != nil {
		h.t.Error(err)
	}
	if err := h.connections.Shutdown(ctx); err != nil {
		h.t.Error(err)
	}
}

// provision installs a credential file and account row directly, the way a completed ceremony does.
func (h *conversationHarness) provision(principal, accountID string) {
	h.t.Helper()
	directory := filepath.Join(h.profilesDir, principal)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		h.t.Fatal(err)
	}
	record := fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":%q,"refresh":%q,"expires":%d,"accountId":%q}}`,
		codexauth.AccessToken(accountID), codexauth.RefreshToken(accountID), time.Now().Add(time.Hour).UnixMilli(), accountID)
	if err := os.WriteFile(filepath.Join(directory, "auth.json"), []byte(record), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if bound, err := h.store.BindModelConnection(h.ctx, principal, "openai-codex", accountID); err != nil || !bound {
		h.t.Fatalf("bind %s: bound=%v err=%v", principal, bound, err)
	}
}

func (h *conversationHarness) sentTexts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sent...)
}

func textUpdate(id, userID int64, text string) Update {
	return Update{UpdateID: id, Message: &Message{From: &User{ID: userID, FirstName: "Test"}, Chat: Chat{ID: userID, Type: ChatTypePrivate}, Text: text}}
}

func (h *conversationHarness) push(updates ...Update) {
	h.t.Helper()
	select {
	case h.updates <- updates:
	case <-h.ctx.Done():
		h.t.Fatal("timed out submitting Telegram updates")
	}
}

func receive[T any](t *testing.T, ctx context.Context, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-ctx.Done():
		t.Fatal("timed out waiting for integration event")
		var zero T
		return zero
	}
}

func (h *conversationHarness) nextMessage() sentMessage { return receive(h.t, h.ctx, h.messages) }

func (h *conversationHarness) wantMessage(chatID int64, text string) {
	h.t.Helper()
	message := h.nextMessage()
	if message.chatID != chatID || message.text != text {
		h.t.Fatalf("message = %+v, want chat %d text %q", message, chatID, text)
	}
}

func (h *conversationHarness) waitState(updateID int64, state string) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var actual, message string
		var response sql.NullString
		err := h.db.QueryRowContext(h.ctx, `SELECT state, message, response_text FROM conversation_requests WHERE telegram_update_id = ?`, updateID).Scan(&actual, &message, &response)
		if err == nil && actual == state {
			if (state == "delivered" || state == "cancelled") && (message != "" || response.Valid) {
				h.t.Fatalf("terminal request retained content: message=%q response=%v", message, response)
			}
			return
		}
		if err != nil && err != sql.ErrNoRows {
			h.t.Fatal(err)
		}
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("request %d state = %q, want %q", updateID, actual, state)
		}
	}
}

func (h *conversationHarness) waitOffset(want int64) {
	h.t.Helper()
	for {
		if got := receive(h.t, h.ctx, h.offsets); got == want {
			return
		} else if got > want {
			h.t.Fatalf("poll offset %d passed wanted %d", got, want)
		}
	}
}

func TestConversationSessionsAndUserIsolation(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()
	h.push(textUpdate(1, 101, "remember amber"))
	alice := receive(t, h.ctx, runtime.calls)
	if alice.request.PrincipalID != "alice" || alice.request.Message != "remember amber" || len(alice.request.Tools) != 0 {
		t.Fatalf("unexpected runtime request: %+v", alice.request)
	}
	h.push(textUpdate(2, 202, "remember blue"))
	bob := receive(t, h.ctx, runtime.calls)
	if bob.request.PrincipalID != "bob" || bob.request.SessionID == alice.request.SessionID {
		t.Fatalf("principal sessions not isolated: %+v / %+v", alice.request, bob.request)
	}
	bob.answer <- agentruntime.RunResult{Response: "blue saved"}
	h.wantMessage(202, "blue saved")
	h.waitState(2, "delivered")
	alice.answer <- agentruntime.RunResult{Response: "amber saved"}
	h.wantMessage(101, "amber saved")
	h.waitState(1, "delivered")
	h.push(textUpdate(3, 101, "recall"))
	recall := receive(t, h.ctx, runtime.calls)
	if recall.request.SessionID != alice.request.SessionID {
		t.Fatal("conversation did not reuse Alice's session")
	}
	recall.answer <- agentruntime.RunResult{Response: "amber"}
	h.wantMessage(101, "amber")
	h.waitState(3, "delivered")
	h.push(textUpdate(4, 101, "/new"))
	h.wantMessage(101, NewSessionReply)
	h.push(textUpdate(5, 101, "recall"), textUpdate(6, 202, "recall"))
	for range 2 {
		call := receive(t, h.ctx, runtime.calls)
		if call.request.PrincipalID == "alice" && call.request.SessionID == alice.request.SessionID {
			t.Fatal("/new retained Alice's session")
		}
		if call.request.PrincipalID == "bob" && call.request.SessionID != bob.request.SessionID {
			t.Fatal("Alice's /new reset Bob's session")
		}
		call.answer <- agentruntime.RunResult{Response: "done"}
	}
	h.nextMessage()
	h.nextMessage()
	h.waitState(5, "delivered")
	h.waitState(6, "delivered")
}

func TestCommandsRemainResponsiveAndCancelSuppressesLateAnswer(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()
	h.push(textUpdate(1, 101, "long request"))
	call := receive(t, h.ctx, runtime.calls)
	h.push(textUpdate(2, 101, "second request"), textUpdate(3, 101, "/status"), textUpdate(4, 101, "/privacy"), textUpdate(5, 101, "/cancel"))
	h.wantMessage(101, conversation.BusyReply)
	h.wantMessage(101, ConversationStatusReply)
	h.wantMessage(101, ConversationPrivacyReply)
	h.wantMessage(101, CancelledReply)
	receive(t, h.ctx, call.ctx.Done())
	h.waitState(1, "cancelled")
	h.push(textUpdate(6, 101, "next request"))
	next := receive(t, h.ctx, runtime.calls)
	if next.request.Message != "next request" {
		t.Fatalf("busy request reached runtime: %+v", next.request)
	}
	next.answer <- agentruntime.RunResult{Response: "next answer"}
	h.wantMessage(101, "next answer")
	h.waitState(6, "delivered")
	h.stop()
	select {
	case message := <-h.messages:
		t.Fatalf("unexpected late reply: %+v", message)
	default:
	}
}

func TestUnauthorizedAndNonPrivateUpdatesAreIgnoredAndCursorSurvivesRestart(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()
	group := textUpdate(2, 101, "group request")
	group.Message.Chat.Type = "group"
	group.Message.Chat.ID = -500
	h.push(textUpdate(1, 303, "unauthorized"), group, Update{UpdateID: 3}, textUpdate(4, 101, ""), textUpdate(5, 101, "accepted"), textUpdate(5, 101, "accepted"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.Message != "accepted" {
		t.Fatalf("ignored input reached runtime: %+v", call.request)
	}
	call.answer <- agentruntime.RunResult{Response: "one answer"}
	h.wantMessage(101, "one answer")
	h.waitState(5, "delivered")
	h.waitOffset(6)
	h.stop()
	h.start()
	h.waitOffset(6)
	h.push(textUpdate(5, 101, "accepted"), textUpdate(6, 101, "/status"))
	h.wantMessage(101, ConversationStatusReply)
	h.waitOffset(7)
	h.stop()
	select {
	case call := <-runtime.calls:
		t.Fatalf("duplicate/ignored update ran model: %+v", call.request)
	default:
	}
	var count int
	if err := h.db.QueryRow(`SELECT count(*) FROM conversation_requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persisted %d requests, want 1", count)
	}
}

func TestConversationRestartRecovery(t *testing.T) {
	for _, state := range []string{"queued", "running", "ready", "delivering"} {
		t.Run(state, func(t *testing.T) {
			runtime := newControlledRuntime()
			h := newConversationHarness(t, runtime)
			h.provision("alice", "acct-alice")
			input := conversation.EnqueueInput{BotUserID: 999, TelegramUpdateID: 1, PrincipalID: "alice", ChatID: 101, Message: "original request"}
			_, err := h.store.EnqueueConversation(h.ctx, input, conversation.Session{ID: recoverySessionID, PrincipalID: "alice", RuntimeSessionID: recoveryRuntimeSessionID}, recoveryRequestID)
			if err != nil {
				t.Fatal(err)
			}
			if state != "queued" {
				if ok, err := h.store.ClaimConversationRequest(h.ctx, recoveryRequestID); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
			}
			if state == "ready" || state == "delivering" {
				if ok, err := h.store.SaveConversationResponse(h.ctx, recoveryRequestID, "saved answer", ""); err != nil || !ok {
					t.Fatalf("save: %v %v", ok, err)
				}
			}
			if state == "delivering" {
				if ok, err := h.store.ClaimConversationDelivery(h.ctx, recoveryRequestID); err != nil || !ok {
					t.Fatalf("delivery: %v %v", ok, err)
				}
			}
			h.start()
			want := "saved answer"
			if state == "queued" {
				call := receive(t, h.ctx, runtime.calls)
				if call.request.Message != input.Message {
					t.Fatal("queued request changed")
				}
				call.answer <- agentruntime.RunResult{Response: "queued answer"}
				want = "queued answer"
			}
			if state == "running" {
				want = conversation.InterruptedReply
			}
			h.wantMessage(101, want)
			h.waitState(1, "delivered")
			h.push(textUpdate(2, 101, "follow up"))
			call := receive(t, h.ctx, runtime.calls)
			if call.request.Message != "follow up" {
				t.Fatalf("replayed model work after restart: %+v", call.request)
			}
			if (call.request.SessionID != recoveryRuntimeSessionID) != (state == "running") {
				t.Fatalf("unexpected recovery session %q for %s", call.request.SessionID, state)
			}
			call.answer <- agentruntime.RunResult{Response: "follow up answer"}
			h.wantMessage(101, "follow up answer")
			h.waitState(2, "delivered")
		})
	}
}

func TestConversationDeliveryFailuresAndUnicodeChunks(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			runtime := newControlledRuntime()
			h := newConversationHarness(t, runtime)
			h.provision("alice", "acct-alice")
			h.sendStatuses = []int{status}
			h.start()
			h.push(textUpdate(1, 101, "answer"))
			call := receive(t, h.ctx, runtime.calls)
			call.answer <- agentruntime.RunResult{Response: "model answer"}
			if first := h.nextMessage(); first.status != status || first.text != "model answer" {
				t.Fatalf("first attempt = %+v", first)
			}
			if status == http.StatusForbidden {
				h.waitState(1, "cancelled")
			} else {
				h.wantMessage(101, "model answer")
				h.waitState(1, "delivered")
			}
			h.push(textUpdate(2, 101, "next"))
			call = receive(t, h.ctx, runtime.calls)
			if call.request.Message != "next" {
				t.Fatal("delivery retried the model request")
			}
			answer := strings.Repeat("😀", 8001)
			call.answer <- agentruntime.RunResult{Response: answer}
			var joined strings.Builder
			for range 3 {
				chunk := h.nextMessage()
				if chunk.chatID != 101 || !utf8.ValidString(chunk.text) || utf8.RuneCountInString(chunk.text) > 4000 {
					t.Fatalf("invalid Telegram chunk: chat=%d runes=%d", chunk.chatID, utf8.RuneCountInString(chunk.text))
				}
				joined.WriteString(chunk.text)
			}
			if joined.String() != answer {
				t.Fatal("chunking changed reply content")
			}
			h.waitState(2, "delivered")
		})
	}
}

func TestBusyWhileResponseIsBeingDelivered(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	gate := make(chan struct{})
	h.sendGate = gate
	h.provision("alice", "acct-alice")
	h.start()
	h.push(textUpdate(1, 101, "first"))
	call := receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "first answer"}
	h.wantMessage(101, "first answer")
	h.waitState(1, "delivering")
	h.push(textUpdate(2, 101, "second"))
	message := h.nextMessage()
	if message.text != conversation.BusyReply {
		t.Fatalf("during delivery got %q, want busy reply", message.text)
	}
	close(gate)
	h.waitState(1, "delivered")
	h.waitOffset(3)
	h.stop()
	select {
	case call := <-runtime.calls:
		t.Fatalf("message during delivery was enqueued: %s", fmt.Sprint(call.request))
	default:
	}
}

func TestBotCommandsRegisteredOnceAtStartup(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	h.start()
	want := []string{"status", "privacy", "cancel", "new", "connect", "connection", "disconnect"}
	for _, wantScope := range [][]string{nil, {`{"type":"all_private_chats"}`}} {
		registration := receive(t, h.ctx, h.registrations)
		if !slices.Equal(registration.scope, wantScope) {
			t.Fatalf("registration scope = %v, want %v", registration.scope, wantScope)
		}
		if len(registration.commands) != len(want) {
			t.Fatalf("registered %+v, want %v", registration.commands, want)
		}
		for index, command := range registration.commands {
			if command.Command != want[index] || command.Description == "" {
				t.Fatalf("command %d = %+v, want %q with a description", index, command, want[index])
			}
		}
	}
	h.push(textUpdate(1, 101, "/status"), textUpdate(2, 101, "/privacy"))
	h.wantMessage(101, ConversationStatusReply)
	h.wantMessage(101, ConversationPrivacyReply)
	h.waitOffset(3)
	select {
	case extra := <-h.registrations:
		t.Fatalf("registered again while polling: %+v", extra)
	default:
	}
}

func TestBotCommandRegistrationFailureKeepsPolling(t *testing.T) {
	h := newConversationHarness(t, newControlledRuntime())
	h.commandStatus = http.StatusInternalServerError
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	h.push(textUpdate(1, 101, "/status"))
	h.wantMessage(101, ConversationStatusReply)
	logs := h.logs.String()
	if !strings.Contains(logs, "telegram.commands.register_failed") {
		t.Fatalf("registration failure was not logged: %s", logs)
	}
	if strings.Contains(logs, "test-token") {
		t.Fatal("logs contain the Telegram token")
	}
}

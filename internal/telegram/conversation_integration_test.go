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

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil/codexauth"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
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
	chatID    int64
	replyTo   int64
	text      string
	parseMode string
	keyboard  InlineKeyboard
	status    int
	messageID int64
}

type editedMessage struct {
	chatID    int64
	messageID int64
	text      string
	keyboard  InlineKeyboard
}

type callbackAnswer struct {
	id   string
	text string
}

type deletedMessage struct {
	chatID    int64
	messageID int64
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
	profilesDir         string
	codeTimeout         time.Duration
	providers           []tools.Provider
	notion              *notionsim.Simulator
	notionChatConnect   bool
	notionIntakeTimeout time.Duration
	approvalLifetime    time.Duration
	approvalSweep       time.Duration
}

type conversationHarness struct {
	t              *testing.T
	ctx            context.Context
	store          *sqlite.Store
	db             *sql.DB
	client         *Client
	service        *Service
	conversation   *conversation.Service
	connections    *connection.Service
	notion         *notion.Service
	approvals      *approval.Service
	auth           *codexauth.Simulator
	profilesDir    string
	credentialsDir string
	sent           []string
	updates        chan []Update
	messages       chan sentMessage
	edits          chan editedMessage
	deletes        chan deletedMessage
	answers        chan callbackAnswer
	offsets        chan int64
	registrations  chan commandRegistration
	turns          chan turnOutcome
	logs           *lockedBuffer
	mu             sync.Mutex
	sentCount      int64
	sendStatuses   []int
	sendReason     string
	sendGate       <-chan struct{}
	commandStatus  int
	editStatus     int
	editReason     string
	deleteStatus   int
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
	h := &conversationHarness{
		t: t, ctx: ctx, updates: make(chan []Update, 20), messages: make(chan sentMessage, 100), edits: make(chan editedMessage, 100),
		answers: make(chan callbackAnswer, 100), deletes: make(chan deletedMessage, 100), offsets: make(chan int64, 100), registrations: make(chan commandRegistration, 10),
		turns: make(chan turnOutcome, 20), logs: &lockedBuffer{},
	}
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
	broker, err := tools.NewBroker(logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range options.providers {
		if err := broker.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	h.approvals, err = approval.NewService(approval.Config{Lifetime: options.approvalLifetime, SweepInterval: options.approvalSweep}, h.store, h.client, logger)
	if err != nil {
		t.Fatal(err)
	}
	var notionController NotionController
	if options.notion != nil {
		h.credentialsDir = filepath.Join(t.TempDir(), "integrations")
		h.notion, err = notion.NewService(notion.Config{
			CredentialsDir: h.credentialsDir, APIBaseURL: options.notion.URL(), ChatConnect: options.notionChatConnect,
			HTTPClient: options.notion.Client(), IntakeLifetime: options.notionIntakeTimeout,
		}, h.store, h.approvals, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := broker.Register(h.notion); err != nil {
			t.Fatal(err)
		}
		if err := h.approvals.RegisterExecutor(h.notion); err != nil {
			t.Fatal(err)
		}
		notionController = h.notion
	}
	h.conversation, err = conversation.NewService(conversation.Config{TurnTimeout: 10 * time.Second, FollowUp: h.approvals, Notice: h.approvals}, h.store, runtime, h.client, broker, h.connections, logger, integrationTelemetry{turns: h.turns})
	if err != nil {
		t.Fatal(err)
	}
	h.service, err = NewService(h.client, identities, h.store, h.conversation, h.connections, notionController, h.approvals, logger, nil)
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
		var replyTo int64
		if raw := r.Form.Get("reply_parameters"); raw != "" {
			var parameters struct {
				MessageID                int64 `json:"message_id"`
				AllowSendingWithoutReply bool  `json:"allow_sending_without_reply"`
			}
			if err := json.Unmarshal([]byte(raw), &parameters); err != nil || parameters.MessageID <= 0 || !parameters.AllowSendingWithoutReply {
				h.t.Errorf("reply_parameters %q must quote a message and allow sending without it: %v", raw, err)
			}
			replyTo = parameters.MessageID
		}
		h.mu.Lock()
		status := http.StatusOK
		if len(h.sendStatuses) > 0 {
			status = h.sendStatuses[0]
			h.sendStatuses = h.sendStatuses[1:]
		}
		gate, reason := h.sendGate, h.sendReason
		h.sent = append(h.sent, r.Form.Get("text"))
		h.sentCount++
		messageID := sentMessageID(h.sentCount)
		h.mu.Unlock()
		select {
		case h.messages <- sentMessage{chatID: chatID, replyTo: replyTo, text: r.Form.Get("text"), parseMode: r.Form.Get("parse_mode"), keyboard: h.parseKeyboard(r.Form.Get("reply_markup")), status: status, messageID: messageID}:
		case <-r.Context().Done():
			return
		}
		result = Message{MessageID: messageID, Chat: Chat{ID: chatID, Type: ChatTypePrivate}}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": reason})
			return
		}
	case "sendChatAction":
		result = true
	case "deleteMessage":
		chatID, err := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		messageID, err := strconv.ParseInt(r.Form.Get("message_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		h.mu.Lock()
		status := h.deleteStatus
		h.mu.Unlock()
		select {
		case h.deletes <- deletedMessage{chatID: chatID, messageID: messageID}:
		case <-r.Context().Done():
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": "Bad Request: message can't be deleted"})
			return
		}
		result = true
	case "answerCallbackQuery":
		if r.Form.Get("callback_query_id") == "" {
			h.t.Error("answerCallbackQuery without a callback ID")
		}
		select {
		case h.answers <- callbackAnswer{id: r.Form.Get("callback_query_id"), text: r.Form.Get("text")}:
		case <-r.Context().Done():
			return
		}
		result = true
	case "editMessageText":
		chatID, err := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		messageID, err := strconv.ParseInt(r.Form.Get("message_id"), 10, 64)
		if err != nil {
			h.t.Error(err)
			return
		}
		h.mu.Lock()
		status, reason := h.editStatus, h.editReason
		h.sent = append(h.sent, r.Form.Get("text"))
		h.mu.Unlock()
		select {
		case h.edits <- editedMessage{chatID: chatID, messageID: messageID, text: r.Form.Get("text"), keyboard: h.parseKeyboard(r.Form.Get("reply_markup"))}:
		case <-r.Context().Done():
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": reason})
			return
		}
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

func (h *conversationHarness) parseKeyboard(raw string) InlineKeyboard {
	h.t.Helper()
	if raw == "" {
		return nil
	}
	var markup struct {
		InlineKeyboard InlineKeyboard `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(raw), &markup); err != nil || len(markup.InlineKeyboard) == 0 {
		h.t.Errorf("reply_markup %q must be an inline keyboard: %v", raw, err)
	}
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if button.Text == "" || button.CallbackData == "" || len(button.CallbackData) > 64 {
				h.t.Errorf("invalid inline button %+v", button)
			}
		}
	}
	return markup.InlineKeyboard
}

func (h *conversationHarness) start() {
	h.t.Helper()
	if err := h.connections.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if err := h.approvals.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if err := h.conversation.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if h.notion != nil {
		if err := h.notion.Start(h.ctx); err != nil {
			h.t.Fatal(err)
		}
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
	if err := h.approvals.Shutdown(ctx); err != nil {
		h.t.Error(err)
	}
	if err := h.connections.Shutdown(ctx); err != nil {
		h.t.Error(err)
	}
	if h.notion != nil {
		if err := h.notion.Shutdown(ctx); err != nil {
			h.t.Error(err)
		}
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

// requestMessageID differs from the update ID so a reply quoting the wrong identifier is caught.
func requestMessageID(updateID int64) int64 { return 1000 + updateID }

func textUpdate(id, userID int64, text string) Update {
	return Update{UpdateID: id, Message: &Message{MessageID: requestMessageID(id), From: &User{ID: userID, FirstName: "Test"}, Chat: Chat{ID: userID, Type: ChatTypePrivate}, Text: text}}
}

// settingsMessageID stands for the bot's settings message that a button press refers to.
func settingsMessageID(updateID int64) int64 { return 5000 + updateID }

// sentMessageID is the ID the Telegram simulator assigns to the n-th message the bot sends.
func sentMessageID(n int64) int64 { return 9000 + n }

func callbackID(updateID int64) string { return fmt.Sprintf("cb-%d", updateID) }

func callbackUpdate(id, userID, messageID int64, data string) Update {
	return Update{UpdateID: id, CallbackQuery: &CallbackQuery{
		ID: callbackID(id), From: &User{ID: userID, FirstName: "Test"},
		Message: &Message{MessageID: messageID, Chat: Chat{ID: userID, Type: ChatTypePrivate}}, Data: data,
	}}
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

func (h *conversationHarness) nextEdit() editedMessage { return receive(h.t, h.ctx, h.edits) }

func (h *conversationHarness) wantAnswer(updateID int64) {
	h.t.Helper()
	if answer := receive(h.t, h.ctx, h.answers); answer.id != callbackID(updateID) {
		h.t.Fatalf("answered callback %+v, want %s", answer, callbackID(updateID))
	}
}

// wantMessage expects an unquoted send, as connection notifications are.
func (h *conversationHarness) wantMessage(chatID int64, text string) {
	h.t.Helper()
	message := h.nextMessage()
	if message.chatID != chatID || message.replyTo != 0 || message.text != text {
		h.t.Fatalf("message = %+v, want chat %d text %q without a quote", message, chatID, text)
	}
}

// wantReply expects a send that answers the update by quoting its message.
func (h *conversationHarness) wantReply(chatID, updateID int64, text string) {
	h.t.Helper()
	message := h.nextMessage()
	if message.chatID != chatID || message.replyTo != requestMessageID(updateID) || message.text != text {
		h.t.Fatalf("message = %+v, want chat %d text %q quoting message %d", message, chatID, text, requestMessageID(updateID))
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
	if alice.request.PrincipalID != "alice" || alice.request.Message != "remember amber" || !alice.request.Tools.Empty() {
		t.Fatalf("unexpected runtime request: %+v", alice.request)
	}
	h.push(textUpdate(2, 202, "remember blue"))
	bob := receive(t, h.ctx, runtime.calls)
	if bob.request.PrincipalID != "bob" || bob.request.SessionID == alice.request.SessionID {
		t.Fatalf("principal sessions not isolated: %+v / %+v", alice.request, bob.request)
	}
	bob.answer <- agentruntime.RunResult{Response: "blue saved"}
	h.wantReply(202, 2, "blue saved")
	h.waitState(2, "delivered")
	alice.answer <- agentruntime.RunResult{Response: "amber saved"}
	h.wantReply(101, 1, "amber saved")
	h.waitState(1, "delivered")
	h.push(textUpdate(3, 101, "recall"))
	recall := receive(t, h.ctx, runtime.calls)
	if recall.request.SessionID != alice.request.SessionID {
		t.Fatal("conversation did not reuse Alice's session")
	}
	recall.answer <- agentruntime.RunResult{Response: "amber"}
	h.wantReply(101, 3, "amber")
	h.waitState(3, "delivered")
	h.push(textUpdate(4, 101, "/new"))
	h.wantReply(101, 4, NewSessionReply)
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
	h.wantReply(101, 2, conversation.BusyReply)
	h.wantReply(101, 3, ConversationStatusReply)
	h.wantReply(101, 4, ConversationPrivacyReply)
	h.wantReply(101, 5, CancelledReply)
	receive(t, h.ctx, call.ctx.Done())
	h.waitState(1, "cancelled")
	h.push(textUpdate(6, 101, "next request"))
	next := receive(t, h.ctx, runtime.calls)
	if next.request.Message != "next request" {
		t.Fatalf("busy request reached runtime: %+v", next.request)
	}
	next.answer <- agentruntime.RunResult{Response: "next answer"}
	h.wantReply(101, 6, "next answer")
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
	unquotable := textUpdate(5, 101, "no message id")
	unquotable.Message.MessageID = 0
	h.push(textUpdate(1, 303, "unauthorized"), group, Update{UpdateID: 3}, textUpdate(4, 101, ""), unquotable, textUpdate(6, 101, "accepted"), textUpdate(6, 101, "accepted"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.Message != "accepted" {
		t.Fatalf("ignored input reached runtime: %+v", call.request)
	}
	call.answer <- agentruntime.RunResult{Response: "one answer"}
	h.wantReply(101, 6, "one answer")
	h.waitState(6, "delivered")
	h.waitOffset(7)
	if logs := h.logs.String(); !strings.Contains(logs, "reason=malformed_message") {
		t.Fatalf("message without an ID was not ignored as malformed: %s", logs)
	}
	h.stop()
	h.start()
	h.waitOffset(7)
	h.push(textUpdate(6, 101, "accepted"), textUpdate(7, 101, "/status"))
	h.wantReply(101, 7, ConversationStatusReply)
	h.waitOffset(8)
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
			input := conversation.EnqueueInput{BotUserID: 999, TelegramUpdateID: 1, TelegramMessageID: requestMessageID(1), PrincipalID: "alice", ChatID: 101, Message: "original request"}
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
			h.wantReply(101, 1, want)
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
			h.wantReply(101, 2, "follow up answer")
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
			if first := h.nextMessage(); first.status != status || first.replyTo != requestMessageID(1) || first.text != "model answer" {
				t.Fatalf("first attempt = %+v", first)
			}
			if status == http.StatusForbidden {
				h.waitState(1, "cancelled")
			} else {
				h.wantReply(101, 1, "model answer")
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
			for index := range 3 {
				chunk := h.nextMessage()
				if chunk.chatID != 101 || !utf8.ValidString(chunk.text) || utf8.RuneCountInString(chunk.text) > 4000 {
					t.Fatalf("invalid Telegram chunk: chat=%d runes=%d", chunk.chatID, utf8.RuneCountInString(chunk.text))
				}
				wantQuote := int64(0)
				if index == 0 {
					wantQuote = requestMessageID(2)
				}
				if chunk.replyTo != wantQuote {
					t.Fatalf("chunk %d quotes message %d, want %d", index, chunk.replyTo, wantQuote)
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

func TestOutgoingMessagesUseHTMLAndFallBackToPlainText(t *testing.T) {
	const answer = "**Done** on [the page](https://notion.so/p?a=1&b=2): `data_source_id` a & b <now>"
	const formatted = `<b>Done</b> on <a href="https://notion.so/p?a=1&amp;b=2">the page</a>: <code>data_source_id</code> a &amp; b &lt;now&gt;`
	t.Run("rendered", func(t *testing.T) {
		runtime := newControlledRuntime()
		h := newConversationHarness(t, runtime)
		h.provision("alice", "acct-alice")
		h.start()
		h.push(textUpdate(1, 101, "answer"))
		call := receive(t, h.ctx, runtime.calls)
		call.answer <- agentruntime.RunResult{Response: answer}
		message := h.nextMessage()
		if message.parseMode != "HTML" || message.text != formatted {
			t.Fatalf("message = %+v, want %q with parse mode HTML", message, formatted)
		}
		h.waitState(1, "delivered")
	})
	t.Run("refused entities", func(t *testing.T) {
		runtime := newControlledRuntime()
		h := newConversationHarness(t, runtime)
		h.provision("alice", "acct-alice")
		h.sendStatuses = []int{http.StatusBadRequest}
		h.sendReason = "Bad Request: can't parse entities: Unsupported start tag"
		h.start()
		h.push(textUpdate(1, 101, "answer"))
		call := receive(t, h.ctx, runtime.calls)
		call.answer <- agentruntime.RunResult{Response: answer}
		if refused := h.nextMessage(); refused.parseMode != "HTML" || refused.text != formatted {
			t.Fatalf("first attempt = %+v, want the rendered message", refused)
		}
		retry := h.nextMessage()
		if retry.parseMode != "" || retry.text != answer || retry.replyTo != requestMessageID(1) {
			t.Fatalf("retry = %+v, want the plain text quoting the request", retry)
		}
		h.waitState(1, "delivered")
		if sent := h.sentTexts(); len(sent) != 2 {
			t.Fatalf("sent %q, want exactly one plain-text retry", sent)
		}
	})
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
	h.wantReply(101, 1, "first answer")
	h.waitState(1, "delivering")
	h.push(textUpdate(2, 101, "second"))
	message := h.nextMessage()
	if message.text != conversation.BusyReply || message.replyTo != requestMessageID(2) {
		t.Fatalf("during delivery got %+v, want busy reply quoting the second request", message)
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
	want := []string{"status", "privacy", "cancel", "new", "settings"}
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
	h.wantReply(101, 1, ConversationStatusReply)
	h.wantReply(101, 2, ConversationPrivacyReply)
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
	h.wantReply(101, 1, ConversationStatusReply)
	logs := h.logs.String()
	if !strings.Contains(logs, "telegram.commands.register_failed") {
		t.Fatalf("registration failure was not logged: %s", logs)
	}
	if strings.Contains(logs, "test-token") {
		t.Fatal("logs contain the Telegram token")
	}
}

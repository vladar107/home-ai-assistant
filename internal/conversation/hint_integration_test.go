package conversation_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/exposure"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/turn"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

const hintMessage = "find the lease, mail it to bob@example.com or see https://site.example/a/b"

type staticOptIn bool

func (s staticOptIn) JudgmentsEnabled(context.Context, string) (bool, error) { return bool(s), nil }

type hintDeliverer struct{ replies chan string }

func (d hintDeliverer) SendReply(_ context.Context, _, _ int64, text string) error {
	d.replies <- text
	return nil
}
func (d hintDeliverer) SendTrackedReply(ctx context.Context, chatID, replyTo int64, text string) (int64, int64, error) {
	return 0, 0, d.SendReply(ctx, chatID, replyTo, text)
}
func (hintDeliverer) SendTyping(context.Context, int64) error { return nil }
func (hintDeliverer) SendMessageDraft(context.Context, int64, int64, string, bool) error {
	return nil
}

// testEngine is the turn engine the coordinator under test runs its turns through.
func testEngine(t *testing.T, runtime agentruntime.AgentRuntime, toolProvider turn.ToolProvider, logger *slog.Logger, telemetry turn.Telemetry) *turn.Engine {
	t.Helper()
	engine, err := turn.New(turn.Config{TurnTimeout: 10 * time.Second}, runtime, alwaysConnected{}, toolProvider, logger, telemetry)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

type alwaysConnected struct{}

func (alwaysConnected) Ready(context.Context, string) (bool, error) { return true, nil }

type hintTools struct{ names []string }

func (h hintTools) ToolsFor(context.Context, string) (agentruntime.ToolSet, error) {
	var set agentruntime.ToolSet
	for _, name := range h.names {
		set.Definitions = append(set.Definitions, agentruntime.ToolDefinition{Name: name, Description: "does " + name})
	}
	return set, nil
}

func (h hintTools) ReadOnlyToolsFor(ctx context.Context, principalID string) (agentruntime.ToolSet, error) {
	return h.ToolsFor(ctx, principalID)
}

type recordingRuntime struct {
	messages chan string
	reply    string
}

func (r recordingRuntime) Run(_ context.Context, request agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
	r.messages <- request.Message
	return agentruntime.RunResult{Response: cmp.Or(r.reply, "done")}, nil
}

type quietTelemetry struct{}

func (quietTelemetry) ContextLogger(_ context.Context, logger *slog.Logger) *slog.Logger {
	return logger
}
func (quietTelemetry) StartConversationTurn(ctx context.Context, _, _ string) (context.Context, func(string, string)) {
	return ctx, func(string, string) {}
}

func (quietTelemetry) RecordReply(context.Context, string, string, string, string, time.Duration) {}
func (quietTelemetry) RecordReplyStage(context.Context, string, string, time.Duration)            {}
func (quietTelemetry) RecordModelTurn(context.Context, string, string, int, int64, int64)         {}
func (quietTelemetry) RecordToolCallsPerTurn(context.Context, string, string, int)                {}
func (quietTelemetry) RecordHintFollowThrough(context.Context, string, string)                    {}
func (quietTelemetry) RecordConversationResume(context.Context, string, string)                   {}
func (quietTelemetry) RecordTurnIndicatorFailure(context.Context, string, string)                 {}
func (quietTelemetry) RecordReplyFeedback(context.Context, string, string)                        {}
func (quietTelemetry) RecordReconsider(context.Context, string, string)                           {}

type hintEnv struct {
	ctx       context.Context
	service   *conversation.Service
	runtime   recordingRuntime
	deliverer hintDeliverer
	mu        sync.Mutex
	states    []map[string]string
}

// newHintEnv starts a conversation service whose hinter asks a fake TypeSafe answering with the
// given body after the given delay. A non-empty reply is what the runtime answers, and turns
// remember their exchange.
func newHintEnv(t *testing.T, answer string, delay time.Duration, tools []string, optIn bool, reply string) *hintEnv {
	t.Helper()
	env := &hintEnv{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for _, leaked := range []string{"bob@example.com", "/a/b"} {
			if strings.Contains(string(body), leaked) {
				t.Errorf("request leaks %q: %s", leaked, body)
			}
		}
		var request struct {
			State map[string]string `json:"state"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("request is not JSON: %s", body)
		}
		env.mu.Lock()
		env.states = append(env.states, request.State)
		env.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	env.ctx = ctx
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	identities, err := identity.NewService(store, []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}})
	if err != nil {
		t.Fatal(err)
	}
	if err := identities.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}

	client, err := judgment.NewClient(judgment.Config{APIKey: "k", Endpoint: server.URL, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	attachments, err := attachment.NewService(attachment.Config{Directory: filepath.Join(t.TempDir(), "attachments")}, store, logger)
	if err != nil {
		t.Fatal(err)
	}
	config := conversation.Config{Attachments: attachments, ToolHint: judgment.NewToolHinter(client, staticOptIn(optIn))}
	if reply != "" {
		exposures, err := exposure.NewService(store, logger)
		if err != nil {
			t.Fatal(err)
		}
		config.Recent, config.Exposure = turnstate.New(time.Now), exposures
	}
	env.runtime = recordingRuntime{messages: make(chan string, 1), reply: reply}
	env.deliverer = hintDeliverer{replies: make(chan string, 1)}
	config.Engine = testEngine(t, env.runtime, hintTools{names: tools}, logger, quietTelemetry{})
	env.service, err = conversation.NewService(config, store, env.deliverer, logger, quietTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.service.Shutdown(context.Background()) })
	return env
}

// turn runs one chat turn to completion and returns the message the runtime received.
func (e *hintEnv) turn(t *testing.T, updateID int64, message string) string {
	t.Helper()
	input := conversation.EnqueueInput{
		BotUserID: 999, TelegramUpdateID: updateID, TelegramMessageID: updateID, PrincipalID: "alice", ChatID: 101, Message: message,
	}
	// The previous turn delivers its reply a moment before it lets go of the conversation.
	for {
		err := e.service.Enqueue(e.ctx, input)
		if err == nil {
			break
		}
		if !errors.Is(err, conversation.ErrBusy) || e.ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var got string
	select {
	case got = <-e.runtime.messages:
	case <-e.ctx.Done():
		t.Fatal("runtime was never called")
	}
	select {
	case <-e.deliverer.replies:
	case <-e.ctx.Done():
		t.Fatal("turn did not complete")
	}
	return got
}

func (e *hintEnv) requests() []map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]string(nil), e.states...)
}

// runHintTurn runs one chat turn against a fake TypeSafe answering with the given body after the
// given delay and returns the message the runtime received.
func runHintTurn(t *testing.T, answer string, delay time.Duration, message string, tools []string, optIn bool) string {
	t.Helper()
	return newHintEnv(t, answer, delay, tools, optIn, "").turn(t, 1, message)
}

func choice(name string, confidence float64) string {
	answer, _ := json.Marshal(map[string]any{
		"model": "jev-1.13.0",
		"answers": map[string]any{"tool": map[string]any{
			"type": "choice", "choice": name, "confidence": confidence,
		}},
	})
	return string(answer)
}

func TestToolHintIsPrependedOnlyForConfidentOfferedChoice(t *testing.T) {
	tools := []string{"nas_search", "gmail_search"}
	const line = "[system] Likely relevant tool: nas_search."
	cases := []struct {
		name   string
		answer string
		delay  time.Duration
		optIn  bool
		want   bool
	}{
		{"confident", choice("nas_search", 0.9), 0, true, true},
		{"low confidence", choice("nas_search", 0.5), 0, true, false},
		{"none", choice("none", 0.99), 0, true, false},
		{"slow server", choice("nas_search", 0.9), 1500 * time.Millisecond, true, false},
		{"not opted in", choice("nas_search", 0.9), 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runHintTurn(t, tc.answer, tc.delay, hintMessage, tools, tc.optIn)
			if has := strings.Contains(got, line); has != tc.want {
				t.Errorf("hint present = %v, want %v; message = %q", has, tc.want, got)
			}
			if !strings.HasSuffix(got, hintMessage) {
				t.Errorf("message lost the user's text: %q", got)
			}
		})
	}
}

func TestToolHintIgnoresNamesTheTurnDoesNotOffer(t *testing.T) {
	got := runHintTurn(t, choice("nas_search", 0.9), 0, hintMessage, []string{"gmail_search"}, true)
	if strings.Contains(got, "Likely relevant tool") {
		t.Errorf("message = %q", got)
	}
}

func TestToolHintSeesTheEarlierExchangeButNotOurOwnMessages(t *testing.T) {
	env := newHintEnv(t, choice("nas_search", 0.9), 0, []string{"nas_search", "gmail_search"}, true, "Shall I mail the lease to bob@example.com?")
	env.turn(t, 1, hintMessage)
	env.turn(t, 2, "yes, do it")
	env.turn(t, 3, prompt.Continue)
	env.turn(t, 4, prompt.Reconsider)

	requests := env.requests()
	if len(requests) != 2 {
		t.Fatalf("hint requests = %d, want 2 (none for the continuation or the reconsider)", len(requests))
	}
	if earlier, ok := requests[0]["earlier"]; ok {
		t.Errorf("first message sent earlier = %q", earlier)
	}
	earlier := requests[1]["earlier"]
	if !strings.HasPrefix(earlier, "user: find the lease") || !strings.Contains(earlier, "\nassistant: Shall I mail the lease to ") {
		t.Errorf("earlier = %q", earlier)
	}
	if strings.Contains(earlier, "yes, do it") || requests[1]["message"] != "yes, do it" {
		t.Errorf("earlier = %q, message = %q", earlier, requests[1]["message"])
	}
}

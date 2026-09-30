package conversation_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

const hintMessage = "find the lease, mail it to bob@example.com or see https://site.example/a/b"

type staticOptIn bool

func (s staticOptIn) JudgmentsEnabled(context.Context, string) (bool, error) { return bool(s), nil }

type hintDeliverer struct{ replies chan string }

func (d hintDeliverer) SendReply(_ context.Context, _, _ int64, text string) error {
	d.replies <- text
	return nil
}
func (hintDeliverer) SendTyping(context.Context, int64) error { return nil }

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

type recordingRuntime struct{ messages chan string }

func (r recordingRuntime) Run(_ context.Context, request agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
	r.messages <- request.Message
	return agentruntime.RunResult{Response: "done"}, nil
}

type quietTelemetry struct{}

func (quietTelemetry) ContextLogger(_ context.Context, logger *slog.Logger) *slog.Logger {
	return logger
}
func (quietTelemetry) StartConversationTurn(ctx context.Context) (context.Context, func(string, string)) {
	return ctx, func(string, string) {}
}

// runHintTurn runs one chat turn against a fake TypeSafe answering with the given body after the
// given delay and returns the message the runtime received.
func runHintTurn(t *testing.T, answer string, delay time.Duration, message string, tools []string, optIn bool) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for _, leaked := range []string{"bob@example.com", "/a/b"} {
			if strings.Contains(string(body), leaked) {
				t.Errorf("request leaks %q: %s", leaked, body)
			}
		}
		if !json.Valid(body) {
			t.Errorf("request is not JSON: %s", body)
		}
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
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
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
	runtime := recordingRuntime{messages: make(chan string, 1)}
	deliverer := hintDeliverer{replies: make(chan string, 1)}
	service, err := conversation.NewService(
		conversation.Config{TurnTimeout: 10 * time.Second, Attachments: attachments, ToolHint: judgment.NewToolHinter(client, staticOptIn(optIn))},
		store, runtime, deliverer, hintTools{names: tools}, conversation.AlwaysConnected{},
		logger, quietTelemetry{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	if err := service.Enqueue(ctx, conversation.EnqueueInput{
		BotUserID: 999, TelegramUpdateID: 1, TelegramMessageID: 1, PrincipalID: "alice", ChatID: 101, Message: message,
	}); err != nil {
		t.Fatal(err)
	}
	var got string
	select {
	case got = <-runtime.messages:
	case <-ctx.Done():
		t.Fatal("runtime was never called")
	}
	select {
	case <-deliverer.replies:
	case <-ctx.Done():
		t.Fatal("turn did not complete")
	}
	return got
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

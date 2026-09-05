package telegram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pifixture"
)

func TestMain(m *testing.M) {
	pifixture.RunIfRequested()
	os.Exit(m.Run())
}

func newFixtureRuntime(t *testing.T) *pi.Runtime {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := pi.New(t.Context(), pi.Config{
		Binary: binary, Provider: "fixture-provider", Model: "fixture-model:low",
		ExpectedVersion: pifixture.Version, SessionDir: filepath.Join(t.TempDir(), "sessions"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestProviderFailuresProduceActionableReplies(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		reply    string
		category agentruntime.FailureCategory
	}{
		{"quota", conversation.QuotaReply, agentruntime.FailureQuota},
		{"auth", conversation.ReconnectReply, agentruntime.FailureAuth},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			h := newConversationHarness(t, newFixtureRuntime(t))
			h.start()
			receive(t, h.ctx, h.registrations)
			if err := h.client.SendTyping(h.ctx, 101); err != nil {
				t.Fatalf("typing indicator failed against a True result: %v", err)
			}
			h.push(textUpdate(1, 101, "fixture:"+tc.scenario))
			h.wantMessage(101, tc.reply)
			h.waitState(1, "delivered")
			turn := receive(t, h.ctx, h.turns)
			if turn.outcome != "error" || turn.errorKind != string(tc.category) {
				t.Fatalf("telemetry turn = %+v, want error with %s", turn, tc.category)
			}
			var stored string
			if err := h.db.QueryRowContext(h.ctx, `SELECT COALESCE(error_kind, '') FROM conversation_requests WHERE telegram_update_id = 1`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != "runtime" {
				t.Fatalf("stored error_kind = %q, want runtime", stored)
			}
			h.push(textUpdate(2, 101, "hello"))
			h.wantMessage(101, "reply: hello")
			h.waitState(2, "delivered")
			if logs := h.logs.String(); strings.Contains(logs, "synthetic-private-text") {
				t.Fatal("logs contain provider text")
			}
		})
	}
}

package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/task"
)

type fakeUsage struct {
	read func(ctx context.Context) (chatgpt.Usage, error)
}

func (f fakeUsage) Usage(ctx context.Context, _ string) (chatgpt.Usage, error) { return f.read(ctx) }

func (fakeUsage) CachedUsage(string, time.Duration) (chatgpt.Usage, bool) {
	return chatgpt.Usage{}, false
}

type fakeTaskModels struct {
	pair     agentruntime.ModelEffort
	fallback string
}

func (f fakeTaskModels) EffectiveModel(context.Context, string) (agentruntime.ModelEffort, string) {
	return f.pair, f.fallback
}

func newStatusHarness(t *testing.T, usage chatgpt.UsageReader, tasks TaskModels) (*conversationHarness, *controlledRuntime) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{topics: true})
	h.provision("alice", "acct-alice")
	if err := os.WriteFile(filepath.Join(h.profilesDir, "alice", "models-store.json"), []byte(modelCatalogFile), 0o600); err != nil {
		t.Fatal(err)
	}
	h.service.model.Usage = usage
	h.service.model.Tasks = tasks
	h.service.model.Location = time.FixedZone("household", 2*3600)
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	return h, runtime
}

func (h *conversationHarness) status(updateID int64) string {
	h.t.Helper()
	h.push(textUpdate(updateID, 101, "/status"))
	return h.nextMessage().text
}

func successfulUsage() fakeUsage {
	return fakeUsage{read: func(context.Context) (chatgpt.Usage, error) {
		return chatgpt.Usage{
			PlanType: "plus", Allowed: true, LimitReached: false,
			Primary:   &chatgpt.UsageWindow{UsedPercent: 42.4, WindowSeconds: 18000, ResetAt: time.Date(2026, 10, 18, 19, 30, 0, 0, time.UTC)},
			Secondary: &chatgpt.UsageWindow{UsedPercent: 7, WindowSeconds: 604800, ResetAt: time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)},
		}, nil
	}}
}

func TestStatusShowsModelTaskModelLastTurnAndUsage(t *testing.T) {
	h, runtime := newStatusHarness(t, successfulUsage(), fakeTaskModels{pair: agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "medium"}})

	first := h.status(1)
	for _, want := range []string{
		ConversationStatusReply,
		"Model here: gpt-test, effort medium (the default).",
		"Background tasks: gpt-6.1-sol, effort medium.",
		"Last turn: none yet since the restart.",
		"Subscription usage (plus plan):",
		"5-hour: 42% used, resets Sun 18 Oct 21:30",
		"Weekly: 7% used, resets Thu 22 Oct 10:00",
		"Limit reached: no.",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("/status lacks %q:\n%s", want, first)
		}
	}

	h.push(textUpdate(2, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "hi", Model: "gpt-test", Tokens: agentruntime.TokenCounts{Input: 120, Output: 30, CacheRead: 80}}
	h.nextMessage()
	if after := h.status(3); !strings.Contains(after, "Last turn: answered by gpt-test, 120 input and 30 output tokens (80 read from cache).") {
		t.Fatalf("/status after a turn:\n%s", after)
	}
}

func TestStatusNotesAStaleChoiceAndATaskFallback(t *testing.T) {
	h, _ := newStatusHarness(t, successfulUsage(), fakeTaskModels{
		pair: agentruntime.ModelEffort{Model: "gpt-test", Effort: "medium"}, fallback: task.FallbackUsage,
	})
	if reply := h.modelCommand(1, "/model sol high"); !strings.Contains(reply.text, "now runs on gpt-6.1-sol") {
		t.Fatalf("choice reply = %s", reply.text)
	}
	if own := h.status(2); !strings.Contains(own, "Model here: gpt-6.1-sol, effort high (your choice, until /new).") ||
		!strings.Contains(own, "Background tasks: gpt-test, effort medium.\n"+statusTaskUsageLine) {
		t.Fatalf("/status with a choice:\n%s", own)
	}

	const withoutSol = `{"openai-codex":{"models":[{"id":"gpt-test","name":"Test","reasoning":true,"contextWindow":200000,"cost":{"Input":1,"Output":2}}]}}`
	if err := os.WriteFile(filepath.Join(h.profilesDir, "alice", "models-store.json"), []byte(withoutSol), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := h.status(3)
	if !strings.Contains(stale, "Model here: gpt-test, effort medium (the default).\n"+statusStaleChoiceLine) {
		t.Fatalf("/status with a stale choice:\n%s", stale)
	}
}

func TestStatusUsageFailuresShowNoProviderDetail(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not connected", connection.ErrNotConnected, statusNotConnectedLine},
		{"unavailable", chatgpt.ErrUsageUnavailable, statusUsageUnavailableLine},
		{"unauthorized", chatgpt.ErrUnauthorized, statusUsageUnavailableLine},
		{"provider text", errors.New("boom acct-secret-123 user@example.com"), statusUsageUnavailableLine},
		{"deadline", context.DeadlineExceeded, statusUsageUnavailableLine},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			usage := fakeUsage{read: func(context.Context) (chatgpt.Usage, error) { return chatgpt.Usage{}, c.err }}
			h, _ := newStatusHarness(t, usage, nil)
			reply := h.status(1)
			if !strings.HasSuffix(reply, c.want) {
				t.Fatalf("/status = %s, want it to end with %q", reply, c.want)
			}
			if strings.Contains(reply, "acct-secret-123") || strings.Contains(reply, "example.com") || strings.Contains(reply, "Background tasks") {
				t.Fatalf("/status leaks detail or shows an absent task model:\n%s", reply)
			}
		})
	}
}

func TestStatusGivesUpOnASlowUsageRead(t *testing.T) {
	usage := fakeUsage{read: func(ctx context.Context) (chatgpt.Usage, error) {
		<-ctx.Done()
		return chatgpt.Usage{}, ctx.Err()
	}}
	h, _ := newStatusHarness(t, usage, nil)
	if reply := h.status(1); !strings.HasSuffix(reply, statusUsageUnavailableLine) {
		t.Fatalf("/status = %s", reply)
	}
}

func TestPrivacyAndHelpMentionTheModelCommandAndUsageRead(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{})
	h.provision("alice", "acct-alice")
	h.start()
	h.push(textUpdate(1, 101, "/privacy"))
	if reply := h.nextMessage().text; !strings.Contains(reply, modelUsagePrivacyLine) {
		t.Fatalf("/privacy lacks the usage line:\n%s", reply)
	}

	h.push(textUpdate(2, 101, "how do I pick a model?"))
	call := receive(t, h.ctx, runtime.calls)
	if help := h.callTool(call, "app_help", `{"topic":"model"}`); help.IsError || !strings.Contains(help.Text, "/model") || !strings.Contains(help.Text, ModelCommandLine) {
		t.Fatalf("model help = %+v", help)
	}
	call.answer <- agentruntime.RunResult{Response: "Use /model."}
	h.wantReply(101, 2, "Use /model.")
}

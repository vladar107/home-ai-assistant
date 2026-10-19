package conversation_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// chatDeliverer records every text it is asked to send. A send it is given a hook for runs the
// hook, whose error becomes the send's.
type chatDeliverer struct {
	mu       sync.Mutex
	attempts []string
	hook     func(ctx context.Context, attempt int) error
	attempt  chan string
}

func newChatDeliverer(hook func(context.Context, int) error) *chatDeliverer {
	return &chatDeliverer{hook: hook, attempt: make(chan string, 16)}
}

func (d *chatDeliverer) SendReply(ctx context.Context, chatID, replyTo int64, text string) error {
	_, _, err := d.SendTrackedReply(ctx, chatID, replyTo, text)
	return err
}

func (d *chatDeliverer) SendTrackedReply(ctx context.Context, _, _ int64, text string) (int64, int64, error) {
	d.mu.Lock()
	d.attempts = append(d.attempts, text)
	attempt := len(d.attempts)
	d.mu.Unlock()
	d.attempt <- text
	var err error
	if d.hook != nil {
		err = d.hook(ctx, attempt)
	}
	return int64(1000 + attempt), 0, err
}

func (*chatDeliverer) SendTyping(context.Context, int64) error { return nil }
func (*chatDeliverer) SendMessageDraft(context.Context, int64, int64, string, bool) error {
	return nil
}

func (d *chatDeliverer) next(t *testing.T, ctx context.Context) string {
	t.Helper()
	select {
	case text := <-d.attempt:
		return text
	case <-ctx.Done():
		t.Fatal("no send was attempted")
		return ""
	}
}

func (e *metricsEnv) deliveryService(t *testing.T, runtime agentruntime.AgentRuntime, deliverer conversation.Deliverer) *conversation.Service {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := conversation.NewService(
		conversation.Config{TurnTimeout: 10 * time.Second, Engine: testEngine(t, runtime, hintTools{}, logger, e.telemetry)},
		e.store, deliverer, logger, e.telemetry,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return service
}

func TestRetriedDeliveryResumesWithTheChunkThatFailed(t *testing.T) {
	env := newMetricsEnv(t)
	answer := strings.Join([]string{strings.Repeat("a", 3000), strings.Repeat("b", 3000), strings.Repeat("c", 3000)}, "\n\n")
	chunks := conversation.SplitReply(answer)
	if len(chunks) < 3 {
		t.Fatalf("answer split into %d chunks, want at least 3", len(chunks))
	}
	failing := newChatDeliverer(func(_ context.Context, attempt int) error {
		if attempt == 2 {
			return errors.New("telegram unavailable")
		}
		return nil
	})
	first := env.deliveryService(t, answering(answer, agentruntime.Usage{}), failing)
	env.enqueue(t, first, 1)
	if sent := failing.next(t, env.ctx); sent != chunks[0] {
		t.Fatalf("first chunk = %.20q", sent)
	}
	if failed := failing.next(t, env.ctx); failed != chunks[1] {
		t.Fatalf("failed chunk = %.20q", failed)
	}
	// The retry waits several seconds, so a restart picks the delivery up instead.
	if err := first.Shutdown(env.ctx); err != nil {
		t.Fatal(err)
	}

	resumed := newChatDeliverer(nil)
	env.deliveryService(t, answering(answer, agentruntime.Usage{}), resumed)
	var got []string
	for range len(chunks) - 1 {
		got = append(got, resumed.next(t, env.ctx))
	}
	if !slices.Equal(got, chunks[1:]) {
		t.Fatalf("resumed delivery sent %d chunks starting with %.20q, want the %d from the failed one on", len(got), got[0], len(chunks)-1)
	}
}

func TestReplySentAsShutdownBeginsIsNotSentAgain(t *testing.T) {
	env := newMetricsEnv(t)
	var first *conversation.Service
	shutdown := make(chan error, 1)
	// The send succeeds only once shutdown has cancelled the worker, as with a signal arriving
	// while Telegram answers.
	interrupted := newChatDeliverer(func(ctx context.Context, _ int) error {
		go func() { shutdown <- first.Shutdown(context.Background()) }()
		<-ctx.Done()
		return nil
	})
	first = env.deliveryService(t, answering("first answer", agentruntime.Usage{}), interrupted)
	env.enqueue(t, first, 1)
	if sent := interrupted.next(t, env.ctx); sent != "first answer" {
		t.Fatalf("first send = %q", sent)
	}
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatal(err)
		}
	case <-env.ctx.Done():
		t.Fatal("shutdown did not finish")
	}

	restarted := newChatDeliverer(nil)
	second := env.deliveryService(t, answering("second answer", agentruntime.Usage{}), restarted)
	env.enqueue(t, second, 2)
	if sent := restarted.next(t, env.ctx); sent != "second answer" {
		t.Fatalf("after the restart the first send was %q, want only the new answer", sent)
	}
}

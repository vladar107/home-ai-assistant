package conversation_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// stallingDeliverer holds its first reply until the delivery is cancelled, and passes on the rest.
type stallingDeliverer struct {
	stalled chan struct{}
	replies chan string
}

func (d *stallingDeliverer) SendReply(ctx context.Context, chatID, replyTo int64, text string) error {
	_, _, err := d.SendTrackedReply(ctx, chatID, replyTo, text)
	return err
}

func (d *stallingDeliverer) SendTrackedReply(ctx context.Context, _, _ int64, text string) (int64, int64, error) {
	if d.stalled != nil {
		close(d.stalled)
		d.stalled = nil
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	d.replies <- text
	return 0, 0, nil
}

func (*stallingDeliverer) SendTyping(context.Context, int64) error { return nil }
func (*stallingDeliverer) SendMessageDraft(context.Context, int64, int64, string, bool) error {
	return nil
}

func TestCancelledDeliveryLetsTheNextMessageThroughAtOnce(t *testing.T) {
	env := newMetricsEnv(t)
	stalled := make(chan struct{})
	deliverer := &stallingDeliverer{stalled: stalled, replies: make(chan string, 1)}
	runtime := scriptedRuntime{run: func(context.Context, agentruntime.RunRequest, agentruntime.EventSink) (agentruntime.RunResult, error) {
		return agentruntime.RunResult{Response: "answer"}, nil
	}}
	service, err := conversation.NewService(
		conversation.Config{TurnTimeout: 10 * time.Second}, env.store, runtime, deliverer,
		hintTools{}, alwaysConnected{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), env.telemetry,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(env.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	env.enqueue(t, service, 1)
	waitFor(t, env, stalled)
	if cancelled, err := service.Cancel(env.ctx, conversation.Lane{PrincipalID: "alice"}); err != nil || !cancelled {
		t.Fatalf("cancel = %v, %v", cancelled, err)
	}
	stopped := time.Now()
	env.enqueue(t, service, 2)
	select {
	case <-deliverer.replies:
	case <-time.After(2 * time.Second):
		t.Fatal("the message after /cancel waited behind the cancelled delivery")
	}
	if waited := time.Since(stopped); waited > time.Second {
		t.Fatalf("the next reply took %v", waited)
	}
}

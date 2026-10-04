package exposure

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const requestID = "33333333-3333-4333-8333-333333333333"

type repository struct {
	external bool
	err      error
	marked   []bool
}

func (r *repository) MarkConversationExposed(_ context.Context, _, _ string, external bool) error {
	r.marked = append(r.marked, external)
	return nil
}

func (r *repository) ConversationExternal(context.Context, string, string) (bool, error) {
	return r.external, r.err
}

func newTestService(t *testing.T, repo *repository) *Service {
	t.Helper()
	service, err := NewService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestExternal(t *testing.T) {
	chat := tools.WithTurn(context.Background(), outbox.ChatTurn(requestID))
	task := tools.WithTurn(context.Background(), outbox.TaskTurn("task-1"))
	outsideRun := tools.NewRun("")
	outsideRun.MarkExternal("a page")
	for _, test := range []struct {
		name string
		ctx  context.Context
		repo repository
		want bool
	}{
		{name: "clean chat conversation", ctx: tools.WithRun(chat, tools.NewRun("")), want: false},
		{name: "external chat conversation", ctx: tools.WithRun(chat, tools.NewRun("")), repo: repository{external: true}, want: true},
		{name: "chat run that read outside content", ctx: tools.WithRun(chat, outsideRun), want: true},
		{name: "failed lookup", ctx: tools.WithRun(chat, tools.NewRun("")), repo: repository{err: errors.New("closed")}, want: true},
		{name: "clean task run", ctx: tools.WithRun(task, tools.NewRun("")), repo: repository{external: true}, want: false},
		{name: "task run that read outside content", ctx: tools.WithRun(task, outsideRun), want: true},
		{name: "call without a run", ctx: task, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := newTestService(t, &test.repo).External(test.ctx, "alice"); got != test.want {
				t.Fatalf("External = %v, want %v", got, test.want)
			}
		})
	}
}

func TestMarkTurnExposedRecordsChatTurnsOnly(t *testing.T) {
	repo := &repository{}
	service := newTestService(t, repo)
	service.MarkTurnExposed(context.Background(), "alice", outbox.TaskTurn("task-1"), true)
	service.MarkTurnExposed(context.Background(), "alice", outbox.ChatTurn(requestID), false)
	service.MarkTurnExposed(context.Background(), "alice", outbox.ChatTurn(requestID), true)
	if len(repo.marked) != 2 || repo.marked[0] || !repo.marked[1] {
		t.Fatalf("marks = %v, want the two chat marks with their level", repo.marked)
	}
}

package conversation_test

import (
	"context"
	"slices"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func TestClosingATabCancelsItsTurnAndKeepsItsConversation(t *testing.T) {
	env := newMetricsEnv(t)
	started, stopped := make(chan struct{}, 1), make(chan struct{}, 1)
	service := env.service(t, scriptedRuntime{run: func(ctx context.Context, request agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
		if request.Message != "hold" {
			return agentruntime.RunResult{Response: "answer"}, nil
		}
		started <- struct{}{}
		<-ctx.Done()
		stopped <- struct{}{}
		return agentruntime.RunResult{}, ctx.Err()
	}}, conversation.Config{})
	closedTab := conversation.Lane{PrincipalID: "alice", ThreadID: 7}
	goneTab := conversation.Lane{PrincipalID: "alice", ThreadID: 8}
	main := conversation.Lane{PrincipalID: "alice"}

	enqueueIn(t, env, service, goneTab, 1, "about the trip")
	env.awaitReply(t)
	enqueueIn(t, env, service, closedTab, 2, "hold")
	waitFor(t, env, started)

	closed, err := service.CloseLane(env.ctx, closedTab, conversation.CloseTabClosed)
	if err != nil || !closed {
		t.Fatalf("close tab = %v %v", closed, err)
	}
	waitFor(t, env, stopped)
	if _, pending, err := env.store.NextConversationRequest(env.ctx, closedTab); err != nil || pending {
		t.Fatalf("closed tab still has a turn: %v %v", pending, err)
	}
	if closed, err := service.CloseLane(env.ctx, goneTab, conversation.CloseTabGone); err != nil || !closed {
		t.Fatalf("close gone tab = %v %v", closed, err)
	}

	sessions, err := service.Sessions(env.ctx, main)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sessions = %+v %v", sessions, err)
	}
	if index := slices.IndexFunc(sessions, func(s conversation.SessionSummary) bool { return s.Active || s.Elsewhere }); index >= 0 {
		t.Fatalf("a closed tab's conversation is still open: %+v", sessions[index])
	}
	for _, session := range sessions {
		resumed, err := service.Resume(env.ctx, conversation.Lane{PrincipalID: "alice", ThreadID: 20 + session.ThreadID}, session.ID)
		if err != nil || !resumed.Found || resumed.Current || resumed.Elsewhere {
			t.Fatalf("resume of %+v = %+v %v", session, resumed, err)
		}
	}
}

func enqueueIn(t *testing.T, env *metricsEnv, service *conversation.Service, lane conversation.Lane, update int64, message string) {
	t.Helper()
	if err := service.Enqueue(env.ctx, conversation.EnqueueInput{
		BotUserID: 999, TelegramUpdateID: update, TelegramMessageID: update + 1, PrincipalID: lane.PrincipalID,
		ThreadID: lane.ThreadID, ChatID: 101, Message: message,
	}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, env *metricsEnv, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-env.ctx.Done():
		t.Fatal("timed out")
	}
}

package conversation_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/modelcatalog"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

var (
	chatDefault = agentruntime.ModelEffort{Model: "model-small", Effort: "medium"}
	strongHigh  = agentruntime.ModelEffort{Model: "model-strong", Effort: "high"}
)

type fixedCatalog struct {
	mu     sync.Mutex
	models []modelcatalog.Model
	err    error
}

func (c *fixedCatalog) Models(context.Context, string) ([]modelcatalog.Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.models, c.err
}

func (c *fixedCatalog) set(models []modelcatalog.Model, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models, c.err = models, err
}

func fullCatalog() []modelcatalog.Model {
	efforts := []string{"low", "medium", "high"}
	return []modelcatalog.Model{
		{ID: "model-small", Efforts: efforts},
		{ID: "model-strong", Efforts: efforts},
	}
}

type laneEnv struct {
	*metricsEnv
	service *conversation.Service
	catalog *fixedCatalog
	mu      sync.Mutex
	runs    []agentruntime.RunRequest
	update  int64
}

func newLaneEnv(t *testing.T) *laneEnv {
	t.Helper()
	env := &laneEnv{metricsEnv: newMetricsEnv(t), catalog: &fixedCatalog{models: fullCatalog()}}
	env.service = env.metricsEnv.service(t, scriptedRuntime{run: func(_ context.Context, request agentruntime.RunRequest, _ agentruntime.EventSink) (agentruntime.RunResult, error) {
		env.mu.Lock()
		env.runs = append(env.runs, request)
		env.mu.Unlock()
		return agentruntime.RunResult{Response: "answer", Final: "answer", Model: request.Model, Tokens: agentruntime.TokenCounts{Input: 10, Output: 5}}, nil
	}}, conversation.Config{ChatModel: chatDefault, TaskModel: strongHigh, Models: env.catalog})
	return env
}

// say sends a message in the lane and returns the model and effort the turn ran on.
func (e *laneEnv) say(t *testing.T, lane conversation.Lane, replyTo int64) agentruntime.ModelEffort {
	t.Helper()
	for {
		e.update++
		err := e.service.Enqueue(e.ctx, conversation.EnqueueInput{
			BotUserID: 999, TelegramUpdateID: e.update, TelegramMessageID: e.update + 1000, PrincipalID: lane.PrincipalID,
			ThreadID: lane.ThreadID, ChatID: 101, Message: "hello", ReplyToMessageID: replyTo,
		})
		if err == nil {
			break
		}
		if !errors.Is(err, conversation.ErrBusy) {
			t.Fatal(err)
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-e.ctx.Done():
			t.Fatal("the previous turn never finished")
		}
	}
	e.awaitReply(t)
	e.mu.Lock()
	defer e.mu.Unlock()
	last := e.runs[len(e.runs)-1]
	return agentruntime.ModelEffort{Model: last.Model, Effort: last.Effort}
}

// resume switches the lane to an earlier conversation, waiting out the previous turn's delivery,
// which is answered as busy until it is recorded.
func (e *laneEnv) resume(t *testing.T, lane conversation.Lane, sessionID string) {
	t.Helper()
	for {
		resumed, err := e.service.Resume(e.ctx, lane, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if resumed.Found {
			return
		}
		if !resumed.Busy {
			t.Fatalf("resume = %+v", resumed)
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-e.ctx.Done():
			t.Fatal("the previous turn never finished")
		}
	}
}

func (e *laneEnv) expect(t *testing.T, lane conversation.Lane, want agentruntime.ModelEffort) {
	t.Helper()
	if got := e.say(t, lane, 0); got != want {
		t.Fatalf("turn ran on %v, want %v", got, want)
	}
}

func TestLaneChoiceAppliesAfterNewAndHoldsAcrossSwitching(t *testing.T) {
	env := newLaneEnv(t)
	main := conversation.Lane{PrincipalID: "alice"}

	env.expect(t, main, chatDefault)
	if _, err := env.service.NewSession(env.ctx, main); err != nil {
		t.Fatal(err)
	}
	if err := env.service.SetChoice(env.ctx, main, strongHigh); err != nil {
		t.Fatal(err)
	}
	env.expect(t, main, strongHigh)

	sessions, err := env.service.Sessions(env.ctx, main)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sessions = %+v %v", sessions, err)
	}
	var older conversation.SessionSummary
	for _, session := range sessions {
		if !session.Active {
			older = session
		}
	}
	env.resume(t, main, older.ID)
	env.expect(t, main, strongHigh)

	current := sessions[0].ID
	if current == older.ID {
		current = sessions[1].ID
	}
	if err := env.store.RecordConversationReply(env.ctx, "alice", current, "", 5000); err != nil {
		t.Fatal(err)
	}
	if got := env.say(t, main, 5000); got != strongHigh {
		t.Fatalf("turn after replying to an old answer ran on %v, want %v", got, strongHigh)
	}

	turn := env.service.LaneTurn(main)
	if !turn.Ran || turn.Model != strongHigh.Model || turn.Tokens.Input != 10 || turn.Stale() {
		t.Fatalf("last turn = %+v", turn)
	}
}

func TestLaneChoiceNormalisesAliasesAndRefusesUnsupported(t *testing.T) {
	env := newLaneEnv(t)
	main := conversation.Lane{PrincipalID: "alice"}

	err := env.service.SetChoice(env.ctx, main, agentruntime.ModelEffort{Model: "model-unknown", Effort: "high"})
	if !errors.Is(err, conversation.ErrUnsupportedChoice) {
		t.Fatalf("unknown model error = %v", err)
	}
	err = env.service.SetChoice(env.ctx, main, agentruntime.ModelEffort{Model: "model-strong", Effort: "max"})
	if !errors.Is(err, conversation.ErrUnsupportedChoice) {
		t.Fatalf("unsupported effort error = %v", err)
	}
	if _, found, err := env.service.Choice(env.ctx, main); err != nil || found {
		t.Fatalf("refused choices left %v %v", found, err)
	}

	env.catalog.set(nil, modelcatalog.ErrUnavailable)
	for _, bad := range []agentruntime.ModelEffort{
		{Model: "model-unlisted", Effort: "low"},
		{Model: "model-strong", Effort: "low"},
		{Model: strings.Repeat("x", 300), Effort: "low"},
		{Model: "model with\nbreak", Effort: "low"},
	} {
		if err := env.service.SetChoice(env.ctx, main, bad); !errors.Is(err, conversation.ErrUnsupportedChoice) {
			t.Fatalf("set %q without a readable catalog = %v", bad.Model, err)
		}
	}
	if err := env.service.SetChoice(env.ctx, main, strongHigh); err != nil {
		t.Fatalf("set a default's pair without a readable catalog: %v", err)
	}
	env.expect(t, main, strongHigh)
}

func TestLaneChoiceClearedByNewAndTabClose(t *testing.T) {
	env := newLaneEnv(t)
	main := conversation.Lane{PrincipalID: "alice"}
	tab := conversation.Lane{PrincipalID: "alice", ThreadID: 7}

	env.expect(t, tab, chatDefault)
	if err := env.service.SetChoice(env.ctx, tab, strongHigh); err != nil {
		t.Fatal(err)
	}
	if err := env.service.SetChoice(env.ctx, main, strongHigh); err != nil {
		t.Fatal(err)
	}
	env.expect(t, tab, strongHigh)

	if _, err := env.service.NewSession(env.ctx, tab); err != nil {
		t.Fatal(err)
	}
	if _, found, err := env.service.Choice(env.ctx, tab); err != nil || found {
		t.Fatalf("choice after /new = %v %v", found, err)
	}
	env.expect(t, tab, chatDefault)
	if _, found, _ := env.service.Choice(env.ctx, main); !found {
		t.Fatal("/new in a tab cleared the main view's choice")
	}

	if err := env.service.SetChoice(env.ctx, tab, strongHigh); err != nil {
		t.Fatal(err)
	}
	if closed, err := env.service.CloseLane(env.ctx, tab, conversation.CloseTabClosed); err != nil || !closed {
		t.Fatalf("close tab = %v %v", closed, err)
	}
	if _, found, err := env.service.Choice(env.ctx, tab); err != nil || found {
		t.Fatalf("choice after closing the tab = %v %v", found, err)
	}
}

func TestLaneChoiceKeptWhenATabOnlyLooksGoneAndResumeStartsOnDefaults(t *testing.T) {
	env := newLaneEnv(t)
	tab := conversation.Lane{PrincipalID: "alice", ThreadID: 7}

	env.expect(t, tab, chatDefault)
	if err := env.service.SetChoice(env.ctx, tab, strongHigh); err != nil {
		t.Fatal(err)
	}
	if closed, err := env.service.CloseLane(env.ctx, tab, conversation.CloseTabGone); err != nil || !closed {
		t.Fatalf("close gone tab = %v %v", closed, err)
	}
	if choice, found, err := env.service.Choice(env.ctx, tab); err != nil || !found || choice != strongHigh {
		t.Fatalf("choice of a gone tab = %v %v %v", choice, found, err)
	}
	env.expect(t, tab, strongHigh)

	sessions, err := env.service.Sessions(env.ctx, conversation.Lane{PrincipalID: "alice"})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %+v %v", sessions, err)
	}
	if closed, err := env.service.CloseLane(env.ctx, tab, conversation.CloseTabClosed); err != nil || !closed {
		t.Fatalf("close tab = %v %v", closed, err)
	}
	fresh := conversation.Lane{PrincipalID: "alice", ThreadID: 9}
	env.resume(t, fresh, sessions[0].ID)
	env.expect(t, fresh, chatDefault)
}

func TestStaleLaneChoiceFallsBackAndIsReported(t *testing.T) {
	env := newLaneEnv(t)
	main := conversation.Lane{PrincipalID: "alice"}

	if err := env.service.SetChoice(env.ctx, main, strongHigh); err != nil {
		t.Fatal(err)
	}
	env.catalog.set([]modelcatalog.Model{{ID: "model-small", Efforts: []string{"low", "medium"}}}, nil)
	env.expect(t, main, chatDefault)
	if turn := env.service.LaneTurn(main); !turn.Stale() || !turn.Ran || turn.PassedOver != strongHigh {
		t.Fatalf("last turn = %+v, want it reported stale", turn)
	}
	if choice, found, _ := env.service.Choice(env.ctx, main); !found || choice != strongHigh {
		t.Fatalf("a stale choice was dropped: %v %v", choice, found)
	}
	if choice, stale, err := env.service.EffectiveChoice(env.ctx, main); err != nil || !stale || choice != chatDefault {
		t.Fatalf("effective choice = %v %v %v", choice, stale, err)
	}
	if err := env.service.SetChoice(env.ctx, main, agentruntime.ModelEffort{Model: "model-small", Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	if turn := env.service.LaneTurn(main); turn.Stale() {
		t.Fatalf("a valid new choice is still marked stale: %+v", turn)
	}
	if err := env.service.SetChoice(env.ctx, main, strongHigh); !errors.Is(err, conversation.ErrUnsupportedChoice) {
		t.Fatalf("set of a model the catalog dropped = %v", err)
	}
	if err := env.service.ClearChoice(env.ctx, main); err != nil {
		t.Fatal(err)
	}
	if err := env.service.SetChoice(env.ctx, main, strongHigh); err == nil {
		t.Fatal("catalog still lacks the model")
	}
	env.catalog.set(fullCatalog(), nil)
	if err := env.service.SetChoice(env.ctx, main, strongHigh); err != nil {
		t.Fatal(err)
	}
	env.catalog.set([]modelcatalog.Model{{ID: "model-small", Efforts: []string{"low", "medium"}}}, nil)

	env.catalog.set(fullCatalog(), nil)
	env.expect(t, main, strongHigh)
	if turn := env.service.LaneTurn(main); turn.Stale() {
		t.Fatalf("last turn = %+v, want it no longer stale", turn)
	}

	env.catalog.set(nil, modelcatalog.ErrUnavailable)
	env.expect(t, main, strongHigh)
}

func TestTaskRunPassesItsStoredPairToTheRuntime(t *testing.T) {
	env := newLaneEnv(t)
	for _, report := range []bool{false, true} {
		reply, err := env.service.RunTask(env.ctx, conversation.TaskRun{
			PrincipalID: "alice", Turn: "task:one", SessionID: "55555555-5555-4555-8555-555555555555",
			Message: "work", Report: report, Model: strongHigh.Model, Effort: strongHigh.Effort,
		})
		if err != nil || reply.ErrorKind != "" {
			t.Fatalf("reply = %+v %v", reply, err)
		}
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.runs) != 2 {
		t.Fatalf("runs = %d", len(env.runs))
	}
	for _, run := range env.runs {
		if run.Kind != agentruntime.RunTask || run.Model != strongHigh.Model || run.Effort != strongHigh.Effort {
			t.Fatalf("run = %+v", run)
		}
	}
}

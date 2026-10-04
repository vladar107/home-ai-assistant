package task_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/task"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type fixedExposure bool

func (e fixedExposure) External(context.Context, string) bool { return bool(e) }

type recordingProposer struct{ proposals []approval.Proposal }

func (p *recordingProposer) Propose(_ context.Context, principalID string, proposal approval.Proposal) (approval.Record, error) {
	p.proposals = append(p.proposals, proposal)
	return approval.Record{PrincipalID: principalID, Tool: proposal.Tool, Arguments: proposal.Arguments}, nil
}

type statusMessenger struct{}

func (statusMessenger) SendStatus(context.Context, int64, int64, string, string) (int64, error) {
	return 1, nil
}

func (statusMessenger) EditStatus(context.Context, int64, int64, string, string, bool) error {
	return nil
}

type taskFixture struct {
	service   *task.Service
	store     *sqlite.Store
	proposer  *recordingProposer
	requestID string
	ctx       context.Context
}

func newTaskFixture(t *testing.T, exposed bool) *taskFixture {
	t.Helper()
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	requestID := "33333333-3333-4333-8333-333333333333"
	session := conversation.Session{ID: "11111111-1111-4111-8111-111111111111", PrincipalID: "alice", RuntimeSessionID: "22222222-2222-4222-8222-222222222222"}
	if _, err := store.EnqueueConversation(ctx, conversation.EnqueueInput{
		BotUserID: 999, TelegramUpdateID: 7, TelegramMessageID: 1007, PrincipalID: "alice", ChatID: 101, Message: "research it",
	}, session, requestID, nil); err != nil {
		t.Fatal(err)
	}
	fixture := &taskFixture{store: store, proposer: &recordingProposer{}, requestID: requestID}
	service, err := task.NewService(task.Config{}, store, store, statusMessenger{}, struct{ conversation.Deliverer }{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	service.SetProposer(fixture.proposer)
	service.SetExposure(fixedExposure(exposed))
	fixture.service = service
	fixture.ctx = tools.WithTurn(ctx, outbox.ChatTurn(requestID))
	return fixture
}

func (f *taskFixture) start(t *testing.T) tools.Result {
	t.Helper()
	result, err := f.service.Execute(f.ctx, "alice", tools.Call{Name: "task_start", Arguments: json.RawMessage(`{"title":"Compare phones","brief":"Compare three phones."}`)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *taskFixture) openTasks(t *testing.T) int {
	t.Helper()
	tasks, err := f.service.Tasks(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	return len(tasks)
}

func TestTaskStartsAtOnceWithoutOutsideContent(t *testing.T) {
	fixture := newTaskFixture(t, false)

	result := fixture.start(t)
	if result.IsError || !strings.HasPrefix(result.Text, "Started background task") {
		t.Fatalf("result = %+v", result)
	}
	if fixture.openTasks(t) != 1 || len(fixture.proposer.proposals) != 0 {
		t.Fatalf("tasks = %d, proposals = %d", fixture.openTasks(t), len(fixture.proposer.proposals))
	}
}

func TestTaskAfterOutsideContentIsProposedAndConfirmedLater(t *testing.T) {
	fixture := newTaskFixture(t, true)

	result := fixture.start(t)
	if result.IsError || !strings.Contains(result.Text, "waiting for the user's confirmation") {
		t.Fatalf("result = %+v", result)
	}
	if fixture.openTasks(t) != 0 || len(fixture.proposer.proposals) != 1 {
		t.Fatalf("tasks = %d, proposals = %d", fixture.openTasks(t), len(fixture.proposer.proposals))
	}
	proposal := fixture.proposer.proposals[0]
	want := `{"title":"Compare phones","brief":"Compare three phones.","request_id":"` + fixture.requestID + `"}`
	if proposal.Tool != "task_start" || string(proposal.Arguments) != want {
		t.Fatalf("proposal = %s %s", proposal.Tool, proposal.Arguments)
	}

	outcome, err := fixture.service.ExecuteApproved(t.Context(), approval.Record{ID: "0123456789abcdef", PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
	if err != nil || !strings.Contains(outcome.Text, "Compare phones") {
		t.Fatalf("outcome = %+v %v", outcome, err)
	}
	if fixture.openTasks(t) != 1 {
		t.Fatalf("tasks = %d, want the confirmed task", fixture.openTasks(t))
	}
}

func TestTaskProposalRefusesInvalidInputBeforeProposing(t *testing.T) {
	fixture := newTaskFixture(t, true)

	result, err := fixture.service.Execute(fixture.ctx, "alice", tools.Call{Name: "task_start", Arguments: json.RawMessage(`{"title":"","brief":"x"}`)})
	if err != nil || !result.IsError || len(fixture.proposer.proposals) != 0 {
		t.Fatalf("result = %+v %v, proposals = %d", result, err, len(fixture.proposer.proposals))
	}
}

func TestTaskConfirmationFailsWhenTheQueueIsFull(t *testing.T) {
	fixture := newTaskFixture(t, false)
	for range 4 {
		if result := fixture.start(t); result.IsError {
			t.Fatalf("filler = %+v", result)
		}
	}

	arguments := json.RawMessage(`{"title":"Compare phones","brief":"Compare three phones.","request_id":"` + fixture.requestID + `"}`)
	_, err := fixture.service.ExecuteApproved(t.Context(), approval.Record{ID: "fedcba9876543210", PrincipalID: "alice", Tool: "task_start", Arguments: arguments})
	var actionErr *approval.ActionError
	if !errors.As(err, &actionErr) || actionErr.Reason != "too_many" {
		t.Fatalf("err = %v", err)
	}
}

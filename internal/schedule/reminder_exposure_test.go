package schedule_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type fixedExposure bool

func (e fixedExposure) External(context.Context, string) bool { return bool(e) }

type recordingProposer struct{ proposals []approval.Proposal }

func (p *recordingProposer) Propose(_ context.Context, principalID string, proposal approval.Proposal) (approval.Record, error) {
	p.proposals = append(p.proposals, proposal)
	return approval.Record{PrincipalID: principalID, Tool: proposal.Tool, Arguments: proposal.Arguments}, nil
}

type reminderFixture struct {
	service  *schedule.Service
	store    *sqlite.Store
	proposer *recordingProposer
	now      time.Time
}

func newReminderFixture(t *testing.T, exposed bool) *reminderFixture {
	t.Helper()
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	fixture := &reminderFixture{store: store, proposer: &recordingProposer{}, now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	service, err := schedule.NewService(
		schedule.Config{Zone: "Europe/Amsterdam", Now: func() time.Time { return fixture.now }},
		store, store, struct{ conversation.Deliverer }{}, fixture.proposer, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Register(schedule.Reminder(func() time.Time { return fixture.now })); err != nil {
		t.Fatal(err)
	}
	service.SetExposure(fixedExposure(exposed))
	fixture.service = service
	return fixture
}

func (f *reminderFixture) setReminder(t *testing.T, arguments string) tools.Result {
	t.Helper()
	result, err := f.service.Execute(t.Context(), "alice", tools.Call{Name: "reminder_set", Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *reminderFixture) pending(t *testing.T) int {
	t.Helper()
	jobs, err := f.store.ListScheduledJobs(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	return len(jobs)
}

const reminderArguments = `{"text":"Call mom","at":"2026-10-04 17:00"}`

func TestReminderIsSetAtOnceWithoutOutsideContent(t *testing.T) {
	fixture := newReminderFixture(t, false)

	result := fixture.setReminder(t, reminderArguments)
	if result.IsError || !strings.HasPrefix(result.Text, "Reminder set for ") {
		t.Fatalf("result = %+v", result)
	}
	if fixture.pending(t) != 1 || len(fixture.proposer.proposals) != 0 {
		t.Fatalf("jobs = %d, proposals = %d", fixture.pending(t), len(fixture.proposer.proposals))
	}
}

func TestReminderAfterOutsideContentIsProposedAndConfirmedLater(t *testing.T) {
	fixture := newReminderFixture(t, true)

	result := fixture.setReminder(t, reminderArguments)
	if result.IsError || result.Text != approval.ProposedText {
		t.Fatalf("result = %+v", result)
	}
	if fixture.pending(t) != 0 || len(fixture.proposer.proposals) != 1 {
		t.Fatalf("jobs = %d, proposals = %d", fixture.pending(t), len(fixture.proposer.proposals))
	}
	proposal := fixture.proposer.proposals[0]
	if proposal.Tool != "reminder_set" || string(proposal.Arguments) != `{"text":"Call mom","at":"2026-10-04 17:00","zone":"Europe/Amsterdam"}` {
		t.Fatalf("proposal = %s %s", proposal.Tool, proposal.Arguments)
	}
	if !strings.Contains(proposal.Summary, "Call mom") || !strings.Contains(proposal.Summary, "17:00") ||
		!strings.Contains(proposal.Summary, "I read outside content in this conversation, so I need your confirmation.") {
		t.Fatalf("summary = %q", proposal.Summary)
	}

	outcome, err := fixture.service.ExecuteApproved(t.Context(), approval.Record{ID: "0123456789abcdef", PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
	if err != nil || !strings.HasPrefix(outcome.Text, "Reminder set for ") {
		t.Fatalf("outcome = %+v %v", outcome, err)
	}
	if fixture.pending(t) != 1 {
		t.Fatalf("jobs = %d, want the confirmed reminder", fixture.pending(t))
	}
}

func TestReminderConfirmationFailsWhenTheTimeHasPassed(t *testing.T) {
	fixture := newReminderFixture(t, true)
	fixture.setReminder(t, reminderArguments)
	proposal := fixture.proposer.proposals[0]

	fixture.now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	_, err := fixture.service.ExecuteApproved(t.Context(), approval.Record{ID: "0123456789abcdef", PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
	var actionErr *approval.ActionError
	if !errors.As(err, &actionErr) || !strings.Contains(actionErr.Text, "already passed") || fixture.pending(t) != 0 {
		t.Fatalf("err = %v, jobs = %d", err, fixture.pending(t))
	}
}

func TestReminderConfirmationFailsWhenTheQueueIsFull(t *testing.T) {
	fixture := newReminderFixture(t, false)
	for index := range schedule.MaximumOneTimeJobsPerPrincipal {
		at := time.Date(2026, 10, 4, 8, index, 0, 0, time.UTC).Format("2006-01-02 15:04")
		if result := fixture.setReminder(t, `{"text":"Filler","at":"`+at+`","timezone":"UTC"}`); result.IsError {
			t.Fatalf("filler %d = %+v", index, result)
		}
	}

	arguments := json.RawMessage(`{"text":"Call mom","at":"2026-10-04 17:00","zone":"Europe/Amsterdam"}`)
	_, err := fixture.service.ExecuteApproved(t.Context(), approval.Record{ID: "fedcba9876543210", PrincipalID: "alice", Tool: "reminder_set", Arguments: arguments})
	var actionErr *approval.ActionError
	if !errors.As(err, &actionErr) || actionErr.Reason != "too_many" {
		t.Fatalf("err = %v", err)
	}
}

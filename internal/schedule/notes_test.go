package schedule_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// actingProposer stands in for the approval service at its boundary: it makes a change at once
// through the scheduler's executor, as a clean turn does, or only records it as a card after
// outside content.
type actingProposer struct {
	service   *schedule.Service
	external  bool
	proposals []approval.Proposal
	acted     []approval.Proposal
	outcomes  []approval.Outcome
}

func (p *actingProposer) Propose(_ context.Context, principalID string, proposal approval.Proposal) (approval.Record, error) {
	p.proposals = append(p.proposals, proposal)
	return approval.Record{PrincipalID: principalID, Tool: proposal.Tool, Arguments: proposal.Arguments}, nil
}

func (p *actingProposer) Act(ctx context.Context, principalID string, proposal approval.Proposal) (string, error) {
	if p.external {
		_, err := p.Propose(ctx, principalID, proposal)
		return approval.ProposedText, err
	}
	outcome, err := p.service.ExecuteApproved(ctx, approval.Record{ID: "00000000000000d1", PrincipalID: principalID, Tool: proposal.Tool, Arguments: proposal.Arguments})
	if err != nil {
		return "", err
	}
	p.acted = append(p.acted, proposal)
	p.outcomes = append(p.outcomes, outcome)
	return outcome.Text, nil
}

type notesHandler struct{}

func (notesHandler) Validate(config json.RawMessage) error {
	return schedule.ValidateNotesConfig(config)
}
func (notesHandler) Run(context.Context, schedule.Occurrence) (schedule.Result, error) {
	return schedule.Result{Quiet: true}, nil
}

const builtInCheckType = "test.check"

type notesFixture struct {
	t        *testing.T
	service  *schedule.Service
	store    *sqlite.Store
	proposer *actingProposer
	mu       sync.Mutex
	now      time.Time
}

func newNotesFixture(t *testing.T) *notesFixture {
	t.Helper()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	f := &notesFixture{t: t, store: store, proposer: &actingProposer{}, now: utc(5, 9, 0)}
	service, err := schedule.NewService(
		schedule.Config{Zone: "UTC", TickInterval: time.Hour, Now: f.clock},
		store, store, discardingDeliverer{}, f.proposer, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, jobType := range []schedule.JobType{
		{Name: schedule.TypeModelTurn, Handler: quietHandler{}, CatchUp: time.Hour, MinimumInterval: time.Hour},
		schedule.Reminder(f.clock),
		{Name: schedule.InboxScanType, Handler: notesHandler{}, CatchUp: time.Hour, MinimumInterval: time.Hour, EditableNotes: true},
		{Name: builtInCheckType, Handler: quietHandler{}, CatchUp: time.Hour, MinimumInterval: time.Hour},
	} {
		if err := service.Register(jobType); err != nil {
			t.Fatal(err)
		}
	}
	f.proposer.service = service
	f.service = service
	return f
}

func (f *notesFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *notesFixture) set(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = at
}

func (f *notesFixture) create(id, name, jobType, cron string, config json.RawMessage) schedule.Job {
	f.t.Helper()
	prepared, err := f.service.Prepare("alice", name, jobType, schedule.Timing{Cron: cron}, config)
	if err != nil {
		f.t.Fatal(err)
	}
	if created, err := f.service.Create(f.t.Context(), id, prepared); err != nil || !created {
		f.t.Fatalf("create %s: %v %v", name, created, err)
	}
	return f.job(id)
}

// inbox creates the principal's inbox job with notes on the hourly schedule.
func (f *notesFixture) inbox(notes string) schedule.Job {
	f.t.Helper()
	config, err := schedule.WithNotes(json.RawMessage(`{}`), notes)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.create(schedule.InboxJobID("alice"), schedule.InboxName, schedule.InboxScanType, schedule.InboxCron, config)
}

func (f *notesFixture) job(id string) schedule.Job {
	f.t.Helper()
	job, found, err := f.store.LoadScheduledJob(f.t.Context(), "alice", id)
	if err != nil || !found {
		f.t.Fatalf("load %s: %v %v", id, found, err)
	}
	return job
}

// tick runs the job's due occurrence through the scheduler and waits until its next run has moved
// past it.
func (f *notesFixture) tick(at time.Time, jobID string) {
	f.t.Helper()
	due := f.job(jobID).NextRunAt
	f.set(at)
	f.service.Wake()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.job(jobID).NextRunAt.After(due) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("job %s did not advance past %v", jobID, due)
}

func (f *notesFixture) call(name string, arguments any) tools.Result {
	f.t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		f.t.Fatal(err)
	}
	result, err := f.service.Execute(f.t.Context(), "alice", tools.Call{Name: name, Arguments: encoded})
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *notesFixture) confirm(proposal approval.Proposal) (approval.Outcome, error) {
	return f.service.ExecuteApproved(f.t.Context(), approval.Record{ID: "00000000000000c1", PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
}

const startingNotes = "Report bills to pay.\nSkip newsletters."

func edit(find, replace string) map[string]string {
	return map[string]string{"find": find, "replace": replace}
}

func TestNotesEditIsMadeAtOnceAndUndoRestoresTheOldNotes(t *testing.T) {
	f := newNotesFixture(t)
	before := f.inbox(startingNotes)
	f.set(utc(5, 9, 5))

	result := f.call("schedule_update", map[string]any{"job_id": before.ID, "prompt_edits": []any{edit("Skip newsletters.", "Skip newsletters and receipts.")}})
	if result.IsError || len(f.proposer.acted) != 1 || len(f.proposer.proposals) != 0 {
		t.Fatalf("schedule_update = %+v, acted %d, cards %d", result, len(f.proposer.acted), len(f.proposer.proposals))
	}
	if !strings.Contains(result.Text, `Changed the notes of "Inbox intake".`) {
		t.Fatalf("result = %q", result.Text)
	}
	if summary := f.proposer.acted[0].Summary; !strings.Contains(summary, "Was: Skip newsletters.\nNow: Skip newsletters and receipts.") || strings.Contains(summary, "Report bills") {
		t.Fatalf("summary = %q, want only the passage that changes", summary)
	}
	after := f.job(before.ID)
	if got := schedule.Notes(after.Config); got != "Report bills to pay.\nSkip newsletters and receipts." {
		t.Fatalf("notes = %q", got)
	}
	if after.Cron != schedule.InboxCron || !after.NextRunAt.Equal(before.NextRunAt) || after.Name != schedule.InboxName {
		t.Fatalf("the edit changed more than the notes: %+v", after)
	}

	if _, err := f.service.Undo(t.Context(), approval.Record{PrincipalID: "alice", Tool: f.proposer.acted[0].Tool}, f.proposer.outcomes[0].Undo); err != nil {
		t.Fatal(err)
	}
	if got := schedule.Notes(f.job(before.ID).Config); got != startingNotes {
		t.Fatalf("notes after Undo = %q", got)
	}
}

func TestNotesEditAfterOutsideContentIsACardThatConfirmApplies(t *testing.T) {
	f := newNotesFixture(t)
	job := f.inbox(startingNotes)
	f.proposer.external = true

	result := f.call("schedule_update", map[string]any{"job_id": job.ID, "prompt": "Only mail that needs me."})
	if result.IsError || result.Text != approval.ProposedText || len(f.proposer.proposals) != 1 || len(f.proposer.acted) != 0 {
		t.Fatalf("schedule_update = %+v", result)
	}
	if got := schedule.Notes(f.job(job.ID).Config); got != startingNotes {
		t.Fatalf("a card changed the notes before Confirm: %q", got)
	}
	card := f.proposer.proposals[0]
	if !strings.Contains(card.Summary, "Change the notes of \"Inbox intake\" to:\nOnly mail that needs me.") {
		t.Fatalf("summary = %q", card.Summary)
	}

	f.set(utc(5, 9, 5))
	outcome, err := f.confirm(card)
	if err != nil || len(outcome.Undo) == 0 {
		t.Fatalf("confirm = %+v %v", outcome, err)
	}
	if got := schedule.Notes(f.job(job.ID).Config); got != "Only mail that needs me." {
		t.Fatalf("notes after Confirm = %q", got)
	}
}

func TestAStaleNotesEditIsRefused(t *testing.T) {
	f := newNotesFixture(t)
	job := f.inbox(startingNotes)
	f.proposer.external = true
	for _, arguments := range []map[string]any{
		{"job_id": job.ID, "prompt_edits": []any{edit("bills", "bills and fines")}},
		{"job_id": job.ID, "prompt_edits": []any{edit("Skip newsletters.", "Skip adverts.")}},
	} {
		if result := f.call("schedule_update", arguments); result.IsError {
			t.Fatalf("schedule_update = %+v", result)
		}
	}

	f.set(utc(5, 9, 5))
	if _, err := f.confirm(f.proposer.proposals[0]); err != nil {
		t.Fatal(err)
	}
	_, err := f.confirm(f.proposer.proposals[1])
	var refusal *approval.ActionError
	if !errors.As(err, &refusal) || refusal.Reason != "stale" {
		t.Fatalf("stale confirm = %v", err)
	}
	if got := schedule.Notes(f.job(job.ID).Config); got != "Report bills and fines to pay.\nSkip newsletters." {
		t.Fatalf("notes = %q, want only the first edit", got)
	}
}

func TestARunBetweenTheProposalAndConfirmLeavesTheCardGood(t *testing.T) {
	f := newNotesFixture(t)
	job := f.inbox(startingNotes)
	f.proposer.external = true
	if result := f.call("schedule_update", map[string]any{"job_id": job.ID, "prompt_edits": []any{edit("bills", "bills and fines")}}); result.IsError {
		t.Fatalf("schedule_update = %+v", result)
	}
	if err := f.service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.service.Shutdown(context.Background()) })
	f.tick(utc(5, 10, 1), job.ID)
	if ran := f.job(job.ID); ran.UpdatedAt.Equal(job.UpdatedAt) {
		t.Fatal("the run did not move the job's last change, so it proves nothing")
	}

	if _, err := f.confirm(f.proposer.proposals[0]); err != nil {
		t.Fatalf("confirm after a run = %v", err)
	}
	if got := schedule.Notes(f.job(job.ID).Config); got != "Report bills and fines to pay.\nSkip newsletters." {
		t.Fatalf("notes = %q", got)
	}
}

func TestNotesJobAcceptsNullForOtherFields(t *testing.T) {
	f := newNotesFixture(t)
	job := f.inbox(startingNotes)
	result := f.call("schedule_update", map[string]any{"job_id": job.ID, "cron": nil, "prompt": "Only mail that needs me."})
	if result.IsError || schedule.Notes(f.job(job.ID).Config) != "Only mail that needs me." {
		t.Fatalf("schedule_update with cron null = %+v", result)
	}
}

func TestNotesJobRefusesEveryOtherFieldByName(t *testing.T) {
	f := newNotesFixture(t)
	job := f.inbox(startingNotes)
	for field, value := range map[string]any{"cron": "0 9 * * *", "name": "Mail", "model": "gpt-6.1-sol:high"} {
		result := f.call("schedule_update", map[string]any{"job_id": job.ID, field: value, "prompt": "Only mail that needs me."})
		if !result.IsError || !strings.Contains(result.Text, "leave out "+field+";") || !strings.Contains(result.Text, "/settings") {
			t.Fatalf("%s = %+v", field, result)
		}
	}
	if result := f.call("schedule_update", map[string]any{"job_id": job.ID, "prompt_edits": []any{edit("bills", "bills")}}); !result.IsError || !strings.Contains(result.Text, "changes nothing") {
		t.Fatalf("an edit that changes nothing = %+v", result)
	}
	if len(f.proposer.acted) != 0 || len(f.proposer.proposals) != 0 {
		t.Fatalf("a refused change was made: acted %d, cards %d", len(f.proposer.acted), len(f.proposer.proposals))
	}
	if got := f.job(job.ID); schedule.Notes(got.Config) != startingNotes || got.Cron != schedule.InboxCron || got.Name != schedule.InboxName {
		t.Fatalf("job after refusals = %+v", got)
	}
}

func TestOtherBuiltInJobsAndRemindersGetTheirOwnRefusal(t *testing.T) {
	f := newNotesFixture(t)
	check := f.create("0000000000000001", "Trips", builtInCheckType, "30 7 * * *", json.RawMessage(`{}`))
	result := f.call("schedule_update", map[string]any{"job_id": check.ID, "prompt": "Check more often."})
	if !result.IsError || !strings.Contains(result.Text, "built-in check") || !strings.Contains(result.Text, "/settings") || strings.Contains(strings.ToLower(result.Text), "reminder") {
		t.Fatalf("built-in check = %+v", result)
	}

	if set := f.call("reminder_set", map[string]any{"text": "Call mom", "at": "2026-10-06 17:00"}); set.IsError {
		t.Fatalf("reminder_set = %+v", set)
	}
	jobs, err := f.store.ListScheduledJobs(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	var reminder string
	for _, job := range jobs {
		if job.Type == schedule.TypeReminder {
			reminder = job.ID
		}
	}
	if result := f.call("schedule_update", map[string]any{"job_id": reminder, "prompt": "Call dad"}); !result.IsError || !strings.Contains(result.Text, "Reminders cannot be changed") {
		t.Fatalf("reminder = %+v", result)
	}
}

func TestNotesAreShownUnderTheirLabel(t *testing.T) {
	f := newNotesFixture(t)
	job := f.inbox(startingNotes)
	label := "Notes (the user's preferences; how Inbox intake checks mail is built in): "

	listed := f.call("schedule_list", map[string]any{})
	if !strings.Contains(listed.Text, label+"Report bills to pay.") || strings.Contains(listed.Text, "Model:") {
		t.Fatalf("schedule_list = %q", listed.Text)
	}
	got := f.call("schedule_get", map[string]any{"job_id": job.ID})
	if !strings.Contains(got.Text, label+startingNotes) {
		t.Fatalf("schedule_get = %q", got.Text)
	}
	if section := f.service.PromptSection(t.Context(), "alice"); !strings.Contains(section, label+"Report bills to pay. Skip newsletters.") {
		t.Fatalf("prompt section = %q", section)
	}
}

func TestMigrateInboxMovesOnlyTheInboxJobOnTheLegacySchedule(t *testing.T) {
	f := newNotesFixture(t)
	if schedule.InboxCron != "0 8-22 * * *" {
		t.Fatalf("inbox cron = %q", schedule.InboxCron)
	}
	const legacy = "0 8-22/3 * * *"
	notes, err := schedule.WithNotes(json.RawMessage(`{}`), startingNotes)
	if err != nil {
		t.Fatal(err)
	}
	inbox := f.create(schedule.InboxJobID("alice"), schedule.InboxName, schedule.InboxScanType, legacy, notes)
	other := f.create("0000000000000001", schedule.InboxName, schedule.InboxScanType, legacy, json.RawMessage(`{}`))
	config, err := schedule.ModelTurnConfig("Check my mail.")
	if err != nil {
		t.Fatal(err)
	}
	own := f.create("0000000000000002", "Mail", schedule.TypeModelTurn, legacy, config)

	f.set(utc(5, 9, 30))
	if err := f.service.MigrateInbox(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := f.job(inbox.ID)
	if got.Cron != schedule.InboxCron || !got.NextRunAt.Equal(utc(5, 10, 0)) || schedule.Notes(got.Config) != startingNotes {
		t.Fatalf("migrated inbox = %s next %v config %s", got.Cron, got.NextRunAt, got.Config)
	}
	for _, id := range []string{other.ID, own.ID} {
		if untouched := f.job(id); untouched.Cron != legacy {
			t.Fatalf("job %s that is not the built-in inbox job was moved: %s", id, untouched.Cron)
		}
	}

	if err := f.service.MigrateInbox(t.Context()); err != nil {
		t.Fatal(err)
	}
	if again := f.job(inbox.ID); !again.UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatal("a second migration changed the job again")
	}
}

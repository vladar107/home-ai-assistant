package approval_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

const chooseTool = "choose_write"

type chooseExecutor struct{}

func (chooseExecutor) ActionTools() []string { return []string{chooseTool} }

func (chooseExecutor) ExecuteApproved(context.Context, approval.Record) (approval.Outcome, error) {
	return approval.Outcome{Text: "Saved."}, nil
}

type quietDeliverer struct{}

func (quietDeliverer) SendText(context.Context, int64, string) (int64, error) { return 1, nil }
func (quietDeliverer) SendProposal(context.Context, int64, approval.Shown, approval.Buttons) (int64, approval.WidgetKind, error) {
	return 1, approval.NotWidget, nil
}
func (quietDeliverer) EditText(context.Context, int64, int64, approval.Shown) error { return nil }
func (quietDeliverer) EditProposal(context.Context, int64, int64, approval.Shown, approval.Buttons) error {
	return nil
}
func (quietDeliverer) SendPreview(context.Context, int64, int64, string) error { return nil }
func (quietDeliverer) SendDirectActions(context.Context, int64, approval.DirectActions) (int64, error) {
	return 1, nil
}
func (quietDeliverer) EditDirectActions(context.Context, int64, int64, approval.DirectActions) error {
	return nil
}

// racingRepository has the assistant withdraw a change just before the user's choice reaches the
// store.
type racingRepository struct {
	approval.Repository
	before func()
}

func (r racingRepository) ConfirmChosen(ctx context.Context, bundleID, principalID string, kept []string, now time.Time) ([]string, []string, error) {
	if r.before != nil {
		r.before()
	}
	return r.Repository.ConfirmChosen(ctx, bundleID, principalID, kept, now)
}

func newChooseService(t *testing.T, before func(store *sqlite.Store)) (*approval.Service, *sqlite.Store) {
	t.Helper()
	ctx := t.Context()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	var repository approval.Repository = store
	if before != nil {
		repository = racingRepository{Repository: store, before: func() { before(store) }}
	}
	service, err := approval.NewService(approval.Config{}, repository, quietDeliverer{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterExecutor(chooseExecutor{}); err != nil {
		t.Fatal(err)
	}
	return service, store
}

func chooseProposal(summary string) approval.Proposal {
	arguments, _ := json.Marshal(map[string]string{"summary": summary})
	return approval.Proposal{Tool: chooseTool, Arguments: arguments, Summary: summary}
}

// A change the assistant withdraws between the press loading the proposal and the store's choice
// shows as withdrawn in the answer, with the tally and the caption of the stored state.
func TestChosenConfirmShowsAChangeWithdrawnBeforeTheStoreCall(t *testing.T) {
	var withdraw func()
	service, store := newChooseService(t, func(*sqlite.Store) { withdraw() })
	ctx := t.Context()
	records, err := service.ProposeAll(ctx, "alice", []approval.Proposal{chooseProposal("Change A"), chooseProposal("Change B"), chooseProposal("Change C")})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AfterDelivery(ctx, "alice", 101); err != nil {
		t.Fatal(err)
	}
	withdraw = func() {
		if finished, err := store.FinishApproval(ctx, records[1].ID, approval.StatusPending, approval.StatusRejected, approval.ReasonWithdrawn, ""); err != nil || !finished {
			t.Errorf("withdraw = %v %v", finished, err)
		}
	}

	pressed, err := service.PressKept(ctx, "alice", records[0].BundleID, []bool{true, true, false})
	if err != nil || !pressed.Owned || !pressed.Ran || pressed.Changed {
		t.Fatalf("press = %+v %v", pressed, err)
	}
	shown := pressed.Shown
	if shown.Caption != "3 changes — 1 done, 1 withdrawn, 1 left out" || shown.Status != "1 done, 1 withdrawn, 1 left out." ||
		shown.Changes[0].State != approval.ChangeDone || shown.Changes[1].State != approval.ChangeWithdrawn || shown.Changes[2].State != approval.ChangeLeftOut {
		t.Fatalf("shown = %+v", shown)
	}
}

// A bundle offered outside a chat turn has no conversation to continue: a press that keeps some of
// its changes reports no run and leaves no notice.
func TestChosenConfirmOfAnOfferedProposalLeavesNoNotice(t *testing.T) {
	service, _ := newChooseService(t, nil)
	ctx := t.Context()
	records, err := service.Offer(ctx, "alice", 101, 0, "Weekly review", 10*time.Minute, []approval.Proposal{chooseProposal("Change A"), chooseProposal("Change B")})
	if err != nil {
		t.Fatal(err)
	}

	pressed, err := service.PressKept(ctx, "alice", records[0].BundleID, []bool{true, false})
	if err != nil || !pressed.Owned || pressed.Ran || pressed.Changed || pressed.Shown.Caption != "Weekly review: 2 changes — 1 done, 1 left out" {
		t.Fatalf("press = %+v %v", pressed, err)
	}
	if notice, _ := service.TakeNotice("alice"); notice != "" {
		t.Fatalf("notice = %q", notice)
	}
}

// A proposal's own title names it; without one the title is read from the summary, past any leading
// warning line, and a card with a warning line is marked.
func TestChangesAreTitledByTheirOwnTitleOrTheirSummary(t *testing.T) {
	service, _ := newChooseService(t, nil)
	ctx := t.Context()
	card := func(reason, change, title string) approval.Proposal {
		proposal := chooseProposal("From your weekly review: " + reason + "\n" + change)
		proposal.Title = title
		return proposal
	}
	flagged := chooseProposal("⚠️ This reads like instructions from outside content.\nFrom your weekly review: same reason\nSave the note C")
	records, err := service.ProposeAll(ctx, "alice", []approval.Proposal{
		card("same reason", "Save the note A", "Save the note A"),
		card("same reason", "Save the note B", "Save the note B"),
		flagged,
		chooseProposal("⚠️ Check this first.\nSave the note D\nwith details"),
	})
	if err != nil {
		t.Fatal(err)
	}
	pressed, err := service.PressKept(ctx, "alice", records[0].BundleID, nil)
	if err != nil || !pressed.Changed {
		t.Fatalf("press = %+v %v", pressed, err)
	}
	var titles []string
	for _, change := range pressed.Shown.Changes {
		titles = append(titles, change.Title)
	}
	want := "Save the note A|Save the note B|From your weekly review: same reason|Save the note D"
	if strings.Join(titles, "|") != want {
		t.Fatalf("titles = %q, want %q", strings.Join(titles, "|"), want)
	}
	if warned := []bool{pressed.Shown.Changes[0].Warned, pressed.Shown.Changes[1].Warned, pressed.Shown.Changes[2].Warned, pressed.Shown.Changes[3].Warned}; warned[0] || warned[1] || !warned[2] || !warned[3] {
		t.Fatalf("warned = %v", warned)
	}
}

package telegram

import (
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// proposeBundle makes a chat turn's proposals for Alice and delivers them, and returns the bundle's
// ID and the ID of each record in order. The proposal goes out as today's widget, which the test
// takes off the queue.
func (h *conversationHarness) proposeBundle(proposals ...approval.Proposal) (string, []string) {
	h.t.Helper()
	records, err := h.approvals.ProposeAll(h.ctx, "alice", proposals)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.approvals.AfterDelivery(h.ctx, "alice", 101); err != nil {
		h.t.Fatal(err)
	}
	h.nextWidget()
	ids := make([]string, len(records))
	for index, record := range records {
		ids[index] = record.ID
	}
	return records[0].BundleID, ids
}

// withdraw has the assistant withdraw the numbered change of the waiting proposal.
func (h *conversationHarness) withdraw(number int) {
	h.t.Helper()
	if _, refused, err := h.approvals.Withdraw(h.ctx, "alice", []int{number}, false); err != nil || refused {
		h.t.Fatalf("withdraw %d = %v %v", number, refused, err)
	}
}

func (h *conversationHarness) wantStatuses(ids []string, want ...string) {
	h.t.Helper()
	for index, id := range ids {
		status, reason := h.approvalStatus(id)
		if got := strings.TrimSuffix(status+"/"+reason, "/"); got != want[index] {
			h.t.Fatalf("approval %d = %q, want %q", index+1, got, want[index])
		}
	}
}

// A Confirm that keeps the first of two runs it and ends the second as left out; the message shows
// both, the caption and the tally count both, the assistant's notice names the one left out, and the
// press reports that the model has an outcome to continue from.
func TestChosenConfirmRunsTheKeptChangesAndLeavesOutTheRest(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	bundle, ids := h.proposeBundle(
		outcomeProposal(t, "Save the note A", "Saved A.", false),
		outcomeProposal(t, "Save the note B", "Saved B.", false),
	)

	pressed, err := h.approvals.PressKept(h.ctx, "alice", bundle, []bool{true, false})
	if err != nil || !pressed.Owned || !pressed.Ran || pressed.Changed {
		t.Fatalf("press = %+v %v", pressed, err)
	}
	h.wantStatuses(ids, "executed", "rejected/left_out")
	shown := pressed.Shown
	if shown.Caption != "2 changes — 1 done, 1 left out" || shown.Status != "1 done, 1 left out." || shown.ID != bundle ||
		len(shown.Changes) != 2 || shown.Changes[0].State != approval.ChangeDone || shown.Changes[1].State != approval.ChangeLeftOut ||
		shown.Changes[1].Outcome != "Left out; nothing was written." || !strings.Contains(shown.Text, "2. Save the note B\nLeft out; nothing was written.") {
		t.Fatalf("shown after the press = %+v", shown)
	}
	notice, _ := h.approvals.TakeNotice("alice")
	if !strings.Contains(notice, "The user left out your "+outcomeTool+" proposal; nothing was written, and it is no longer waiting.") ||
		!strings.Contains(notice, "confirmed your "+outcomeTool+" proposal and it was executed. Saved A.") {
		t.Fatalf("notice = %q", notice)
	}
	if again, err := h.approvals.PressKept(h.ctx, "alice", bundle, []bool{true, false}); err != nil || again.Ran || again.Changed || again.Shown.Caption != shown.Caption {
		t.Fatalf("replayed press = %+v %v", again, err)
	}
}

// A Confirm that names another number of changes than the proposal shows, or keeps none that still
// waits, resolves nothing and reports the proposal as changed, with its stored state.
func TestChosenConfirmThatDoesNotMatchTheProposalResolvesNothing(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	bundle, ids := h.proposeBundle(
		outcomeProposal(t, "Save the note A", "Saved A.", false),
		outcomeProposal(t, "Save the note B", "Saved B.", false),
		outcomeProposal(t, "Save the note C", "Saved C.", false),
	)
	for _, keep := range [][]bool{{true, true}, {true, true, true, true}, nil} {
		pressed, err := h.approvals.PressKept(h.ctx, "alice", bundle, keep)
		if err != nil || !pressed.Owned || !pressed.Changed || pressed.Ran || pressed.Shown.Caption != "3 changes" {
			t.Fatalf("press naming %d changes = %+v %v", len(keep), pressed, err)
		}
	}
	h.wantStatuses(ids, "pending", "pending", "pending")

	h.withdraw(2)
	pressed, err := h.approvals.PressKept(h.ctx, "alice", bundle, []bool{false, true, false})
	if err != nil || !pressed.Changed || pressed.Ran || pressed.Shown.Caption != "3 changes — 2 waiting, 1 withdrawn" {
		t.Fatalf("press keeping only a withdrawn change = %+v %v", pressed, err)
	}
	h.wantStatuses(ids, "pending", "rejected/withdrawn", "pending")
}

// A kept change the assistant withdrew meanwhile is passed over, and the rest of the kept ones run.
func TestChosenConfirmPassesOverAWithdrawnChange(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	bundle, ids := h.proposeBundle(
		outcomeProposal(t, "Save the note A", "Saved A.", false),
		outcomeProposal(t, "Save the note B", "Saved B.", false),
		outcomeProposal(t, "Save the note C", "Saved C.", false),
	)
	h.withdraw(2)

	pressed, err := h.approvals.PressKept(h.ctx, "alice", bundle, []bool{true, true, false})
	if err != nil || !pressed.Ran || pressed.Changed {
		t.Fatalf("press = %+v %v", pressed, err)
	}
	h.wantStatuses(ids, "executed", "rejected/withdrawn", "rejected/left_out")
	if pressed.Shown.Caption != "3 changes — 1 done, 1 withdrawn, 1 left out" {
		t.Fatalf("caption = %q", pressed.Shown.Caption)
	}
}

// A Confirm after the deadline expires everything and leaves nothing out.
func TestChosenConfirmAfterTheDeadlineExpiresEverything(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	bundle, ids := h.proposeBundle(
		outcomeProposal(t, "Save the note A", "Saved A.", false),
		outcomeProposal(t, "Save the note B", "Saved B.", false),
	)
	h.exec(`UPDATE approvals SET created_at = '1999-01-01T00:00:00.000Z', expires_at = '2000-01-01T00:00:00.000Z' WHERE bundle_id = ?`, bundle)

	pressed, err := h.approvals.PressKept(h.ctx, "alice", bundle, []bool{true, false})
	if err != nil || !pressed.Owned || pressed.Ran || pressed.Changed || pressed.Shown.Caption != "2 changes — expired" {
		t.Fatalf("press = %+v %v", pressed, err)
	}
	h.wantStatuses(ids, "expired/timeout", "expired/timeout")
}

// Another principal's proposal is neither pressed nor read.
func TestChosenConfirmAndReadingRefuseAnotherPrincipal(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	bundle, ids := h.proposeBundle(
		outcomeProposal(t, "Save the note A", "Saved A.", false),
		outcomeProposal(t, "Save the note B", "Saved B.", false),
	)
	if pressed, err := h.approvals.PressKept(h.ctx, "bob", bundle, []bool{true, true}); err != nil || pressed.Owned || pressed.Ran {
		t.Fatalf("press by another principal = %+v %v", pressed, err)
	}
	if text, found, err := h.approvals.ChangeSummary(h.ctx, "bob", bundle, 1); err != nil || found || text != "" {
		t.Fatalf("summary read by another principal = %q %v %v", text, found, err)
	}
	h.wantStatuses(ids, "pending", "pending")
}

// One change is read in full by its number, whatever became of it, and a number the proposal does
// not have reads nothing.
func TestChangeSummaryIsReadByNumber(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	bundle, _ := h.proposeBundle(
		outcomeProposal(t, "Save the note A\nWith every line of the list.", "Saved A.", false),
		outcomeProposal(t, "Save the note B", "Saved B.", false),
	)
	for number, want := range map[int]string{1: "Save the note A\nWith every line of the list.", 2: "Save the note B"} {
		if text, found, err := h.approvals.ChangeSummary(h.ctx, "alice", bundle, number); err != nil || !found || text != want {
			t.Fatalf("change %d = %q %v %v", number, text, found, err)
		}
	}
	for _, number := range []int{0, 3, -1} {
		if _, found, err := h.approvals.ChangeSummary(h.ctx, "alice", bundle, number); err != nil || found {
			t.Fatalf("change %d found = %v %v", number, found, err)
		}
	}
	if _, err := h.approvals.PressKept(h.ctx, "alice", bundle, []bool{false, true}); err != nil {
		t.Fatal(err)
	}
	if text, found, err := h.approvals.ChangeSummary(h.ctx, "alice", bundle, 1); err != nil || !found || !strings.HasPrefix(text, "Save the note A") {
		t.Fatalf("a change left out = %q %v %v", text, found, err)
	}
}

// A title is the first line of the summary after any intent warning, joined with the next line when
// the first ends with a colon; a record of the changes kind shows its numbered titles with what
// became of each, not the full summaries.
func TestChangesAreTitledAndShownAsAListForTheChangesKind(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	bundle, ids := h.proposeBundle(
		outcomeProposal(t, "Add to the list:\nmilk, eggs\nand bread", "Added.", false),
		outcomeProposal(t, approval.IntentWarning+"\n\nRename the note\nto Groceries", "Renamed.", false),
	)
	h.exec(`UPDATE approvals SET widget_kind = 'changes' WHERE bundle_id = ?`, bundle)
	h.exec(`UPDATE approvals SET warned = 1 WHERE id = ?`, ids[1])

	pressed, err := h.approvals.PressKept(h.ctx, "alice", bundle, nil)
	if err != nil || !pressed.Changed {
		t.Fatalf("press = %+v %v", pressed, err)
	}
	shown := pressed.Shown
	want := "1. Add to the list: milk, eggs\n2. Rename the note"
	if shown.Widget != approval.ChangesWidget || shown.Text != want || shown.Status != "Expires in 10 minutes." || shown.Caption != "⚠️ 2 changes" ||
		len(shown.Changes) != 2 || shown.Changes[0].Title != "Add to the list: milk, eggs" || shown.Changes[0].Warned ||
		shown.Changes[1].Title != "Rename the note" || !shown.Changes[1].Warned || shown.Changes[1].State != approval.ChangeWaiting {
		t.Fatalf("changes shown = %+v", shown)
	}

	if _, err := h.approvals.PressKept(h.ctx, "alice", bundle, []bool{true, false}); err != nil {
		t.Fatal(err)
	}
	h.wantStatuses(ids, "executed", "rejected/left_out")
	pressed, err = h.approvals.PressKept(h.ctx, "alice", bundle, []bool{true})
	if err != nil || !pressed.Changed || pressed.Shown.Text != want[:len("1. Add to the list: milk, eggs")]+"\nDone. Added.\n2. Rename the note\nLeft out; nothing was written." {
		t.Fatalf("changes shown once resolved = %+v %v", pressed.Shown, err)
	}
}

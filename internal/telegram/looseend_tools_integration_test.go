package telegram

import (
	"slices"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/looseend"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const looseEndPromise = `{"kind":"promise","title":"Send the forms to the accountant","counterpart":"the accountant","due":"2026-10-02"}`

func newLooseEndHarness(t *testing.T) (*conversationHarness, *controlledRuntime) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{clock: newTestClock(t, mondayMorning), providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	return h, runtime
}

func (h *conversationHarness) switchLooseEnds(principalID string, on bool) {
	h.t.Helper()
	if err := h.switches.SetEnabled(h.ctx, principalID, looseend.Switch, on); err != nil {
		h.t.Fatal(err)
	}
}

// looseEndRow is one of alice's loose ends as stored.
type looseEndRow struct {
	id, kind, title, counterpart, source, status string
	outside, allDay                              bool
}

func (h *conversationHarness) looseEnds(principalID string) []looseEndRow {
	h.t.Helper()
	rows, err := h.db.QueryContext(h.ctx, `SELECT id, kind, title, counterpart, source, status, outside, due_all_day FROM loose_ends WHERE principal_id = ? ORDER BY rowid`, principalID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var items []looseEndRow
	for rows.Next() {
		var row looseEndRow
		if err := rows.Scan(&row.id, &row.kind, &row.title, &row.counterpart, &row.source, &row.status, &row.outside, &row.allDay); err != nil {
			h.t.Fatal(err)
		}
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return items
}

// looseEndProposal runs one of alice's chat turns that reads outside content and then tracks the
// promise, and returns the proposal that follows the reply.
func (h *conversationHarness) looseEndProposal(runtime *controlledRuntime, updateID int64, arguments string) sentMessage {
	h.t.Helper()
	h.push(textUpdate(updateID, 101, "about the forwarded letter"))
	call := receive(h.t, h.ctx, runtime.calls)
	readOutside(h.t, call)
	if result := toolCaller(h.t, call)(looseend.ToolTrack, arguments); result.IsError || !strings.HasSuffix(result.Text, approval.ProposedText) {
		h.t.Fatalf("track after outside content = %+v, want a proposal", result)
	}
	h.finishTurn(call, updateID, "ok")
	return h.nextMessage()
}

// The tools are offered only while the user has Loose ends on. A promise in a clean conversation
// is tracked at once, as the user's own text, and Undo removes it.
func TestLooseEndToolsFollowTheSwitchAndAPromiseIsTrackedWithUndo(t *testing.T) {
	t.Parallel()
	h, runtime := newLooseEndHarness(t)
	looseEndTools := []string{looseend.ToolList, looseend.ToolTrack, looseend.ToolClose}

	off, _ := h.memoryTurn(runtime, 1, 101, "ok")
	if names := toolNames(off.Tools); slices.ContainsFunc(looseEndTools, func(name string) bool { return slices.Contains(names, name) }) ||
		strings.Contains(off.Tools.Guidance, looseend.ToolTrack) {
		t.Fatalf("tools with Loose ends off = %v, guidance %q", names, off.Tools.Guidance)
	}

	h.switchLooseEnds("alice", true)
	on, _ := h.memoryTurn(runtime, 2, 101, "ok")
	if names := toolNames(on.Tools); !slices.Contains(names, looseend.ToolTrack) ||
		!slices.Contains(names, looseend.ToolList) || !slices.Contains(names, looseend.ToolClose) {
		t.Fatalf("tools with Loose ends on = %v", names)
	}
	for _, want := range []string{"call loose_end_track with kind promise", "loose_end_track, instead of a reminder, for a cancellation, return, renewal, trial, or expiry date"} {
		if !strings.Contains(on.Tools.Guidance, want) {
			t.Fatalf("guidance lacks %q:\n%s", want, on.Tools.Guidance)
		}
	}
	if bobs, _ := h.memoryTurn(runtime, 3, 202, "ok"); slices.Contains(toolNames(bobs.Tools), looseend.ToolTrack) {
		t.Fatalf("bob's tools with only alice's Loose ends on = %v", toolNames(bobs.Tools))
	}

	tracked := h.wantDirect(h.ownDataTurn(runtime, 4, false, looseend.ToolTrack, looseEndPromise), looseend.ToolTrack)
	items := h.looseEnds("alice")
	if len(items) != 1 || items[0].kind != "promise" || items[0].title != "Send the forms to the accountant" || items[0].counterpart != "the accountant" ||
		items[0].source != "chat" || items[0].status != "open" || items[0].outside || !items[0].allDay {
		t.Fatalf("tracked = %+v, want one open chat promise that is not outside", items)
	}
	if answer := h.undoDirect(tracked); answer != "Undone." {
		t.Fatalf("undo = %q", answer)
	}
	if items := h.looseEnds("alice"); len(items) != 0 {
		t.Fatalf("loose ends after Undo = %+v", items)
	}

	refused := h.ownDataTurn(runtime, 5, false, looseend.ToolTrack, `{"kind":"promise","title":"Pay the invoice","due":"2026-09-01"}`)
	if !refused.IsError || !strings.Contains(refused.Text, "due must be in the future. Nothing was tracked.") {
		t.Fatalf("a past due date = %+v", refused)
	}
}

// After outside content a track is a card: Confirm tracks the loose end as outside, and Reject
// tracks nothing. Another user sees none of it.
func TestLooseEndTrackedAfterOutsideContentIsConfirmedAsOutside(t *testing.T) {
	t.Parallel()
	h, runtime := newLooseEndHarness(t)
	h.switchLooseEnds("alice", true)

	proposal := h.looseEndProposal(runtime, 1, looseEndPromise)
	if !strings.Contains(proposal.text, "Track Send the forms to the accountant (promise, the accountant), due Fri 2 Oct 2026.") {
		t.Fatalf("proposal = %q", proposal.text)
	}
	if items := h.looseEnds("alice"); len(items) != 0 {
		t.Fatalf("a proposal tracked %+v", items)
	}
	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	h.nextEdit()
	h.continueTurn(runtime, 2)
	items := h.looseEnds("alice")
	if len(items) != 1 || items[0].status != "open" || !items[0].outside {
		t.Fatalf("confirmed = %+v, want one open item marked outside", items)
	}

	rejected := h.looseEndProposal(runtime, 3, `{"kind":"return_window","title":"Return the kettle","counterpart":"shop.example","due":"2026-10-20"}`)
	_, reject := approvalButtons(t, rejected.keyboard)
	h.push(callbackUpdate(4, 101, rejected.messageID, reject))
	h.wantAnswer(4)
	h.nextEdit()
	if items := h.looseEnds("alice"); len(items) != 1 {
		t.Fatalf("loose ends after Reject = %+v", items)
	}

	h.switchLooseEnds("bob", true)
	_, results := h.memoryTurn(runtime, 5, 202, "ok",
		memoryCall(looseend.ToolList, `{}`), memoryCall(looseend.ToolClose, `{"id":"`+items[0].id+`","status":"done"}`))
	if results[0].IsError || !strings.Contains(results[0].Text, "No open loose ends.") {
		t.Fatalf("bob's list = %+v", results[0])
	}
	if !results[1].IsError || !strings.Contains(results[1].Text, "No loose end has that id") {
		t.Fatalf("bob closing alice's loose end = %+v", results[1])
	}
	if kept := h.looseEnds("alice"); kept[0].status != "open" {
		t.Fatalf("alice's loose end after bob's close = %+v", kept)
	}
}

// Closing done or dropped is made at once in a clean conversation, and Undo reopens. Listing reads
// outside content, so a track later in the same turn is only proposed.
func TestLooseEndIsClosedWithUndoAndListingExposesTheTurn(t *testing.T) {
	t.Parallel()
	h, runtime := newLooseEndHarness(t)
	h.switchLooseEnds("alice", true)
	h.wantDirect(h.ownDataTurn(runtime, 1, false, looseend.ToolTrack, looseEndPromise), looseend.ToolTrack)
	id := h.looseEnds("alice")[0].id

	for index, step := range []struct{ status, stored string }{{"done", "done"}, {"drop", "dropped"}} {
		closed := h.wantDirect(h.ownDataTurn(runtime, int64(2+index), false, looseend.ToolClose, `{"id":"`+id+`","status":"`+step.status+`"}`), looseend.ToolClose)
		if items := h.looseEnds("alice"); items[0].status != step.stored {
			t.Fatalf("after %s = %+v", step.status, items)
		}
		if answer := h.undoDirect(closed); answer != "Undone." {
			t.Fatalf("undo of %s = %q", step.status, answer)
		}
		if items := h.looseEnds("alice"); items[0].status != "open" {
			t.Fatalf("after undoing %s = %+v", step.status, items)
		}
	}

	_, results := h.memoryTurn(runtime, 4, 101, "ok",
		memoryCall(looseend.ToolList, `{"kind":"promise"}`), memoryCall(looseend.ToolTrack, `{"kind":"trial_end","title":"Video trial","due":"2026-10-10"}`))
	if results[0].IsError || !strings.Contains(results[0].Text, "["+id+"] promise: Send the forms to the accountant, with the accountant; due 2026-10-02 (Fri)") {
		t.Fatalf("list = %+v", results[0])
	}
	if results[1].IsError || !strings.HasSuffix(results[1].Text, approval.ProposedText) {
		t.Fatalf("track after listing = %+v, want a proposal", results[1])
	}
	h.nextMessage()
	if items := h.looseEnds("alice"); len(items) != 1 {
		t.Fatalf("a proposal tracked %+v", items)
	}
}

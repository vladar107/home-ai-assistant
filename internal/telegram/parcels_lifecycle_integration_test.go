package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
)

func (w *parcelWiring) cycle() {
	w.t.Helper()
	w.assembly.ParcelLoop.Cycle(w.ctx)
	w.assembly.ParcelLoop.Wait()
}

func (w *parcelWiring) proposals() parcel.Proposals {
	return parcel.Proposals{Repository: w.store, Approvals: w.store, Carriers: parcel.DefaultCarriers()}
}

func (w *parcelWiring) parcelOf(principalID, number string) (parcel.Parcel, bool) {
	w.t.Helper()
	held, err := w.store.ListParcels(w.ctx, principalID)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, item := range held {
		if item.Number == number {
			return item, true
		}
	}
	return parcel.Parcel{}, false
}

// A proposal made in chat is recorded with its approval, so while its card waits neither the mail
// job nor the inbox check offers the number again.
func TestAChatProposalWaitsOnItsApprovalAndBlocksAnotherOffer(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	call := w.startTurn(1, 101)
	executeTool(t, w.ctx, call.request.Tools, "outside_read", `{}`)
	if text := w.track(call, "1234567895"); !strings.Contains(text, approval.ProposedText) {
		t.Fatalf("result = %q", text)
	}
	proposed, found, err := w.store.LoadProposal(w.ctx, "alice", "1234567895")
	if err != nil || !found || proposed.ApprovalID == "" || proposed.Outcome != parcel.OutcomeProposed {
		t.Fatalf("proposal = %+v %v %v", proposed, found, err)
	}
	check := w.proposals()
	check.HasEntry = func(context.Context, string, string) bool { return true }
	if _, err := check.Proposal(w.ctx, "alice", "1234567895", "dhl"); !errors.Is(err, parcel.ErrNotProposed) {
		t.Fatalf("a number whose card waits was offered again: %v", err)
	}
	w.finishTurn(call, 1, "proposed")
}

// A user without an assigned entry of the carrier is never proposed a number, even after outside
// content: it is followed through mail.
func TestWithoutAnAssignedEntryEvenAForwardedNumberIsFollowedByMail(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	call := w.startTurn(1, 202)
	executeTool(t, w.ctx, call.request.Tools, "outside_read", `{}`)
	if text := w.track(call, "1234567892"); !strings.Contains(text, "followed through their mail") || strings.Contains(text, approval.ProposedText) {
		t.Fatalf("result = %q", text)
	}
	if held, ok := w.parcelOf("bob", "1234567892"); !ok || held.Source != parcel.SourceMail {
		t.Fatalf("bob's parcel = %+v %v", held, ok)
	}
	w.finish(call, 1, 202, "followed")
}

// A number asked for again after the user stopped following it is not added, and the model is told.
func TestAStoppedNumberAskedAgainIsNotFollowedAgain(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	call := w.startTurn(1, 202)
	if text := w.track(call, "1234567892"); !strings.Contains(text, "followed through their mail") {
		t.Fatalf("result = %q", text)
	}
	if text := w.track(call, "1234567892"); !strings.Contains(text, "already one of the user's parcels") {
		t.Fatalf("a second ask = %q", text)
	}
	held, _ := w.parcelOf("bob", "1234567892")
	if _, found, err := w.store.StopParcel(w.ctx, "bob", held.ID, time.Now()); err != nil || !found {
		t.Fatalf("stop = %v %v", found, err)
	}
	if text := w.track(call, "1234567892"); !strings.Contains(text, "stopped following") {
		t.Fatalf("a stopped number asked again = %q", text)
	}
	w.finish(call, 1, 202, "done")
}

// A forwarded Telegram message is outside content, so a number in it is proposed, not added.
func TestANumberInAForwardedMessageIsProposed(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	update := textUpdate(1, 101, "Your parcel 1234567896 is on its way")
	update.Message.ForwardOrigin = &MessageOrigin{Type: "user"}
	w.push(update)
	call := receive(t, w.ctx, w.runtime.calls)
	if text := w.track(call, "1234567896"); !strings.Contains(text, approval.ProposedText) {
		t.Fatalf("result = %q", text)
	}
	if calls := w.serviceCalls(); len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
	w.finishTurn(call, 1, "proposed")
}

// A number added through Home Assistant becomes a parcel with a card; Undo removes it from Home
// Assistant, leaves the card in its final state and the parcel gone, and the number can be proposed
// again.
func TestUndoLeavesNoFrozenParcelAndTheNumberCanBeProposedAgain(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	w.cycle()
	for len(w.messages) > 0 {
		<-w.messages
	}
	if tracking, err := w.store.LoadTracking(w.ctx, "alice"); err != nil || tracking.BaselinedAt.IsZero() {
		t.Fatalf("tracking after the baseline = %+v %v", tracking, err)
	}

	call := w.startTurn(1, 101)
	if text := w.track(call, "1234567892"); !strings.Contains(text, "followed in Home Assistant") {
		t.Fatalf("result = %q", text)
	}
	w.finishTurn(call, 1, "following it")
	w.cycle()
	held, ok := w.parcelOf("alice", "1234567892")
	if !ok || held.Source != parcel.SourceHomeAssistant || held.CardMessage == 0 {
		t.Fatalf("parcel Home Assistant lists = %+v %v", held, ok)
	}

	var id string
	if err := w.db.QueryRowContext(w.ctx, `SELECT id FROM approvals WHERE tool = ? AND mode = 'direct'`, parcel.ToolTrack).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if undid, err := w.approvals.Undo(w.ctx, "alice", id); err != nil || undid.Answer != "Undone." {
		t.Fatalf("undo = %+v %v", undid, err)
	}
	if _, ok := w.parcelOf("alice", "1234567892"); ok {
		t.Fatal("the undone number is still a parcel")
	}
	for {
		edit := w.nextEdit()
		if edit.messageID == held.CardMessage {
			if !strings.Contains(edit.text, "Stopped") {
				t.Fatalf("card after undo = %q", edit.text)
			}
			break
		}
	}
	w.assembly.ParcelLoop.Poke("alice")
	w.cycle()
	if _, ok := w.parcelOf("alice", "1234567892"); ok {
		t.Fatal("the undone number came back from Home Assistant")
	}

	for len(w.messages) > 0 {
		<-w.messages
	}
	second := w.startTurn(3, 101)
	executeTool(t, w.ctx, second.request.Tools, "outside_read", `{}`)
	if text := w.track(second, "1234567892"); !strings.Contains(text, approval.ProposedText) {
		t.Fatalf("the number after an undo = %q, want a proposal", text)
	}
	w.finishTurn(second, 3, "proposed")
}

// After a restore the loop's baseline sends the restored parcel a new card without a notification.
func TestARestoredParcelGetsASilentCardFromTheBaseline(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	now := time.Now().UTC().Truncate(time.Second)
	if _, _, err := w.store.UpsertParcel(w.ctx, parcel.Parcel{
		PrincipalID: "alice", Carrier: "postnl", Number: "3SABCD1234567", Title: "Garden chair", Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	w.cycle()
	file, err := w.assembly.Exports.Export(w.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.ExecContext(w.ctx, `DELETE FROM parcels WHERE principal_id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	for len(w.messages) > 0 {
		<-w.messages
	}
	if _, err := w.assembly.Exports.Restore(w.ctx, "alice", file.Data); err != nil {
		t.Fatal(err)
	}
	w.cycle()
	restored, ok := w.parcelOf("alice", "3SABCD1234567")
	if !ok || restored.CardMessage == 0 {
		t.Fatalf("restored parcel = %+v %v", restored, ok)
	}
	var cards []sentMessage
	for len(w.messages) > 0 {
		cards = append(cards, <-w.messages)
	}
	silentCard := false
	for _, message := range cards {
		if message.messageID == restored.CardMessage && message.silent {
			silentCard = true
		}
	}
	if !silentCard {
		t.Fatalf("the restored card was not sent silently: %+v", cards)
	}
}

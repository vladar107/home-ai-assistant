package sqlite

import (
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/parcel"
)

func orderOnly(principal string) parcel.Parcel {
	return parcel.Parcel{
		PrincipalID: principal, Carrier: parcel.ShopID, OrderRef: "123-4567890-1234567", Sender: "Example Shop", Source: parcel.SourceMail,
		Status: parcel.StatusRegistered, CreatedAt: parcelNow, UpdatedAt: parcelNow,
	}
}

// An order-only mail after a hand-off finds the parcel that has a number by now, instead of making
// a second one, and does not turn its carrier or link back into the shop's.
func TestOrderMailAfterAHandOffUpdatesTheSameParcel(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	first, _, err := store.UpsertParcel(ctx, orderOnly("alice"))
	if err != nil {
		t.Fatal(err)
	}
	handOff := orderOnly("alice")
	handOff.Carrier, handOff.Number, handOff.Link, handOff.Status, handOff.UpdatedAt = "dhl_nl", "EX987654321", "https://example.test/track", parcel.StatusInTransit, parcelNow.Add(time.Hour)
	if _, isNew, err := store.UpsertParcel(ctx, handOff); err != nil || isNew {
		t.Fatalf("hand-off = %v %v", isNew, err)
	}
	again := orderOnly("alice")
	again.Link, again.UpdatedAt = "https://example.test/order", parcelNow.Add(2*time.Hour)
	merged, isNew, err := store.UpsertParcel(ctx, again)
	if err != nil || isNew || merged.ID != first.ID || merged.Carrier != "dhl_nl" || merged.Number != "EX987654321" || merged.Link != "https://example.test/track" {
		t.Fatalf("order mail = %+v %v %v", merged, isNew, err)
	}
	if merged.Status != parcel.StatusInTransit {
		t.Fatalf("an earlier status replaced a later one: %s", merged.Status)
	}
	if parcels, _ := store.ListParcels(ctx, "alice"); len(parcels) != 1 {
		t.Fatalf("parcels = %d, want one", len(parcels))
	}

	sourced := parcel.Parcel{
		PrincipalID: "alice", Carrier: "postnl", Source: parcel.SourceHomeAssistant, SourceKey: "package-1", Status: parcel.StatusInTransit,
		Title: "Example", CreatedAt: parcelNow, UpdatedAt: parcelNow,
	}
	created, _, err := store.UpsertParcel(ctx, sourced)
	if err != nil {
		t.Fatal(err)
	}
	numbered := sourced
	numbered.Number, numbered.UpdatedAt = "EX555555555", parcelNow.Add(time.Hour)
	if got, isNew, err := store.UpsertParcel(ctx, numbered); err != nil || isNew || got.ID != created.ID {
		t.Fatalf("source key gained a number = %+v %v %v", got, isNew, err)
	}
	if got, isNew, err := store.UpsertParcel(ctx, sourced); err != nil || isNew || got.ID != created.ID || got.Number != "EX555555555" {
		t.Fatalf("source key listing after the number = %+v %v %v", got, isNew, err)
	}
}

// A hand-off that reaches the numbered parcel first leaves the order's number-less parcel behind;
// the next reading with both folds it in, keeping the card the user sees and leaving no tombstone.
func TestHandOffFoldsTheNumberlessParcelOfTheOrder(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	orphan, _, err := store.UpsertParcel(ctx, orderOnly("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCard(ctx, "alice", orphan.ID, 41, parcel.CardText); err != nil {
		t.Fatal(err)
	}
	numbered := parcel.Parcel{
		PrincipalID: "alice", Carrier: "dhl_nl", Number: "EX987654321", Source: parcel.SourceHomeAssistant, SourceKey: "package-2",
		Status: parcel.StatusInTransit, CreatedAt: parcelNow, UpdatedAt: parcelNow,
	}
	survivor, _, err := store.UpsertParcel(ctx, numbered)
	if err != nil {
		t.Fatal(err)
	}
	numbered.OrderRef, numbered.Sender, numbered.UpdatedAt = "123-4567890-1234567", "Example Shop", parcelNow.Add(time.Hour)
	merged, isNew, err := store.UpsertParcel(ctx, numbered)
	if err != nil || isNew || merged.ID != survivor.ID || merged.CardMessage != 41 || merged.CardKind != parcel.CardText {
		t.Fatalf("fold = %+v %v %v", merged, isNew, err)
	}
	parcels, err := store.ListParcels(ctx, "alice")
	if err != nil || len(parcels) != 1 || parcels[0].ID != survivor.ID || parcels[0].CardMessage != 41 {
		t.Fatalf("parcels = %+v %v", parcels, err)
	}
	if count := countRows(t, store, `SELECT count(*) FROM parcel_tombstones`); count != 0 {
		t.Fatalf("the folded parcel left %d tombstones", count)
	}
}

// A parcel found by its source key that later gained a number and was stopped is recognised by
// each of its identities: its number, its source key alone, and an order-only mail.
func TestStoppedParcelIsTombstonedUnderEveryKey(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	listed := parcel.Parcel{
		PrincipalID: "alice", Carrier: "postnl", Source: parcel.SourceHomeAssistant, SourceKey: "package-3", Sender: "Example Shop",
		OrderRef: "123-4567890-1234567", Status: parcel.StatusInTransit, CreatedAt: parcelNow, UpdatedAt: parcelNow,
	}
	created, _, err := store.UpsertParcel(ctx, listed)
	if err != nil {
		t.Fatal(err)
	}
	listed.Number, listed.UpdatedAt = "EX555555555", parcelNow.Add(time.Hour)
	if _, _, err := store.UpsertParcel(ctx, listed); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.StopParcel(ctx, "alice", created.ID, parcelNow.Add(2*time.Hour)); err != nil || !found {
		t.Fatalf("stop = %v %v", found, err)
	}
	for name, probe := range map[string]parcel.Parcel{
		"source key only": {Carrier: "postnl", Source: parcel.SourceHomeAssistant, SourceKey: "package-3"},
		"order mail":      {Carrier: parcel.ShopID, Sender: "Example Shop", OrderRef: "123-4567890-1234567"},
		"number":          {Carrier: "postnl", Number: "EX555555555"},
	} {
		if seen, err := store.TombstoneSeenAny(ctx, "alice", probe.Keys(), parcelNow.Add(3*time.Hour)); err != nil || !seen {
			t.Errorf("%s: tombstoned = %v %v", name, seen, err)
		}
	}
	if seen, _ := store.TombstoneSeenAny(ctx, "alice", parcel.Parcel{Carrier: "postnl", Number: "EX000000000"}.Keys(), parcelNow); seen {
		t.Error("an unrelated parcel is tombstoned")
	}
}

// A parcel that is delivered without a delivered time is stamped with the first time it was seen
// delivered, keeps that time when the source lists it again, and so expires.
func TestDeliveredWithoutATimeExpiresFromTheFirstSighting(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	delivered := shippedParcel("alice", "EX123456789NL")
	delivered.Status = parcel.StatusDelivered
	delivered.CreatedAt, delivered.UpdatedAt = parcelNow.Add(-20*24*time.Hour), parcelNow.Add(-20*24*time.Hour)
	stored, _, err := store.UpsertParcel(ctx, delivered)
	if err != nil || !stored.DeliveredAt.Equal(delivered.UpdatedAt) {
		t.Fatalf("stored = %+v %v", stored, err)
	}
	for day := 19; day >= 0; day-- {
		delivered.UpdatedAt = parcelNow.Add(-time.Duration(day) * 24 * time.Hour)
		if stored, _, err = store.UpsertParcel(ctx, delivered); err != nil {
			t.Fatal(err)
		}
	}
	if !stored.DeliveredAt.Equal(parcelNow.Add(-20 * 24 * time.Hour)) {
		t.Fatalf("delivered time moved to %s", stored.DeliveredAt)
	}
	if deleted, err := store.DeleteExpired(ctx, parcelNow.Add(-parcel.Retention), parcelNow); err != nil || deleted != 1 {
		t.Fatalf("deleted = %d %v", deleted, err)
	}
}

func TestStatusOnlyMovesForwardExceptForProblems(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	steps := []struct {
		status parcel.Status
		want   parcel.Status
	}{
		{parcel.StatusOutForDelivery, parcel.StatusOutForDelivery},
		{parcel.StatusInTransit, parcel.StatusOutForDelivery},
		{parcel.StatusUnknown, parcel.StatusOutForDelivery},
		{parcel.StatusProblem, parcel.StatusProblem},
		{parcel.StatusInTransit, parcel.StatusInTransit},
		{parcel.StatusDelivered, parcel.StatusDelivered},
		{parcel.StatusAtPickupPoint, parcel.StatusDelivered},
		{parcel.StatusRegistered, parcel.StatusDelivered},
		{parcel.StatusProblem, parcel.StatusProblem},
	}
	for index, step := range steps {
		item := shippedParcel("alice", "EX123456789NL")
		item.Status, item.UpdatedAt = step.status, parcelNow.Add(time.Duration(index)*time.Minute)
		got, _, err := store.UpsertParcel(ctx, item)
		if err != nil || got.Status != step.want {
			t.Fatalf("step %d: %s over the stored one = %s %v, want %s", index, step.status, got.Status, err, step.want)
		}
	}
}

// A proposed number remembers the approval of its latest offer and how often it was offered; a
// decision clears the approval.
func TestProposedNumberKeepsItsApprovalAndOfferCount(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	for index, approval := range []string{"approval-1", "approval-2"} {
		if err := store.RecordProposal(ctx, parcel.ProposedNumber{PrincipalID: "alice", Number: "EX555555555", ApprovalID: approval, ProposedAt: parcelNow.Add(time.Duration(index) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	proposal, _, err := store.LoadProposal(ctx, "alice", "EX555555555")
	if err != nil || proposal.ApprovalID != "approval-2" || proposal.Offers != 2 || proposal.Outcome != parcel.OutcomeProposed {
		t.Fatalf("proposal = %+v %v", proposal, err)
	}
	if err := store.SetProposalOutcome(ctx, "alice", "EX555555555", parcel.OutcomeDeclined, parcelNow.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if proposal, _, _ = store.LoadProposal(ctx, "alice", "EX555555555"); proposal.ApprovalID != "" || proposal.Offers != 2 || proposal.Outcome != parcel.OutcomeDeclined {
		t.Fatalf("declined proposal = %+v", proposal)
	}
}

// Mail can only guess between carriers whose numbers share a pattern, so a number read under either
// is the same parcel: Home Assistant, which knows its carrier, corrects mail's guess, and mail never
// changes the carrier Home Assistant gave it.
func TestANumberSharedByTwoCarriersIsOneParcel(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	const number = "3SEXA12345678"
	guessed := parcel.Parcel{
		PrincipalID: "alice", Carrier: "dhl_nl", Number: number, Source: parcel.SourceMail, Status: parcel.StatusInTransit,
		Link: "https://example.test/dhl", CreatedAt: parcelNow, UpdatedAt: parcelNow,
	}
	first, _, err := store.UpsertParcel(ctx, guessed)
	if err != nil {
		t.Fatal(err)
	}
	account := parcel.Parcel{
		PrincipalID: "alice", Carrier: "postnl", Number: number, Source: parcel.SourceHomeAssistant, SourceKey: "entry:" + number + "-NL-1234AB",
		Status: parcel.StatusOutForDelivery, Link: "https://example.test/postnl", CreatedAt: parcelNow, UpdatedAt: parcelNow.Add(time.Hour),
	}
	merged, isNew, err := store.UpsertParcel(ctx, account)
	if err != nil || isNew || merged.ID != first.ID || merged.Carrier != "postnl" || merged.Link != "https://example.test/postnl" {
		t.Fatalf("the account's reading = %+v %v %v", merged, isNew, err)
	}
	guessed.Status, guessed.UpdatedAt = parcel.StatusDelivered, parcelNow.Add(2*time.Hour)
	if again, isNew, err := store.UpsertParcel(ctx, guessed); err != nil || isNew || again.ID != first.ID || again.Carrier != "postnl" ||
		again.Link != "https://example.test/postnl" || again.Status != parcel.StatusDelivered {
		t.Fatalf("mail after the account = %+v %v %v", again, isNew, err)
	}
	if parcels, _ := store.ListParcels(ctx, "alice"); len(parcels) != 1 {
		t.Fatalf("parcels = %+v", parcels)
	}
	if _, _, err := store.StopParcel(ctx, "alice", first.ID, parcelNow.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"postnl|" + number, "dhl_nl|" + number} {
		if seen, err := store.TombstoneSeenAny(ctx, "alice", []string{key}, parcelNow); err != nil || !seen {
			t.Fatalf("no tombstone under %s: %v %v", key, seen, err)
		}
	}

	// A number several carriers share is never kept as "other", which a source naming one of the
	// carriers would not find.
	for shared, settled := range map[string]string{"3SEXA12345679": "postnl", "123456789012": "gls"} {
		typed := parcel.Parcel{PrincipalID: "alice", Carrier: "other", Number: shared, Source: parcel.SourceMail, Status: parcel.StatusUnknown, CreatedAt: parcelNow, UpdatedAt: parcelNow}
		stored, _, err := store.UpsertParcel(ctx, typed)
		if err != nil || stored.Carrier != settled {
			t.Fatalf("%s typed without a carrier = %+v %v", shared, stored, err)
		}
		for _, carrier := range parcel.DefaultCarriers().Twins(settled, shared) {
			listed := parcel.Parcel{
				PrincipalID: "alice", Carrier: carrier, Number: shared, Source: parcel.SourceHomeAssistant, SourceKey: "entry:" + shared,
				Status: parcel.StatusInTransit, CreatedAt: parcelNow, UpdatedAt: parcelNow.Add(time.Hour),
			}
			if got, isNew, err := store.UpsertParcel(ctx, listed); err != nil || isNew || got.ID != stored.ID {
				t.Fatalf("%s listed under %s = %+v %v %v", shared, carrier, got, isNew, err)
			}
		}
	}
}

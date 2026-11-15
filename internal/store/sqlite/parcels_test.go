package sqlite

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/delivery"
	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
)

var parcelNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func shippedParcel(principal, number string) parcel.Parcel {
	return parcel.Parcel{
		PrincipalID: principal, Carrier: "postnl", Number: number, Title: "Example kettle", Sender: "Example Shop",
		Source: parcel.SourceHomeAssistant, SourceKey: "key-" + number, Status: parcel.StatusInTransit,
		WindowFrom: parcelNow.Add(2 * time.Hour), WindowTo: parcelNow.Add(4 * time.Hour), Link: "https://example.test/track/" + number,
		CreatedAt: parcelNow, UpdatedAt: parcelNow,
	}
}

func TestParcelUpsertInsertsUpdatesAndMergesByEitherKey(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	created, isNew, err := store.UpsertParcel(ctx, shippedParcel("alice", "EX123456789NL"))
	if err != nil || !isNew || created.ID == "" {
		t.Fatalf("insert = %+v %v %v", created, isNew, err)
	}
	loaded, found, err := store.LoadParcel(ctx, "alice", created.ID)
	if err != nil || !found || loaded.Number != "EX123456789NL" || loaded.Status != parcel.StatusInTransit || !loaded.WindowFrom.Equal(parcelNow.Add(2*time.Hour)) || loaded.Title != "Example kettle" {
		t.Fatalf("loaded = %+v %v %v", loaded, found, err)
	}

	update := shippedParcel("alice", "EX123456789NL")
	update.Status, update.Title, update.PickupPoint, update.UpdatedAt = parcel.StatusAtPickupPoint, "", "Example Point", parcelNow.Add(time.Hour)
	merged, isNew, err := store.UpsertParcel(ctx, update)
	if err != nil || isNew || merged.ID != created.ID || merged.Status != parcel.StatusAtPickupPoint || merged.Title != "Example kettle" || merged.PickupPoint != "Example Point" {
		t.Fatalf("update = %+v %v %v", merged, isNew, err)
	}
	unknown := shippedParcel("alice", "EX123456789NL")
	unknown.Status, unknown.UpdatedAt = parcel.StatusUnknown, parcelNow.Add(2*time.Hour)
	if merged, _, err = store.UpsertParcel(ctx, unknown); err != nil || merged.Status != parcel.StatusAtPickupPoint {
		t.Fatalf("an unknown status replaced a known one: %+v %v", merged, err)
	}
	if err := store.SaveCard(ctx, "alice", created.ID, 77, parcel.CardWidget); err != nil {
		t.Fatal(err)
	}
	if merged, _, err = store.UpsertParcel(ctx, update); err != nil || merged.CardMessage != 77 || merged.CardKind != parcel.CardWidget {
		t.Fatalf("an update lost the card: %+v %v", merged, err)
	}

	order := parcel.Parcel{
		PrincipalID: "alice", Carrier: parcel.ShopID, OrderRef: "123-4567890-1234567", Sender: "Example Shop", Source: parcel.SourceMail,
		Status: parcel.StatusRegistered, CreatedAt: parcelNow, UpdatedAt: parcelNow,
	}
	byOrder, isNew, err := store.UpsertParcel(ctx, order)
	if err != nil || !isNew {
		t.Fatalf("order insert = %+v %v %v", byOrder, isNew, err)
	}
	order.Status, order.UpdatedAt = parcel.StatusInTransit, parcelNow.Add(time.Hour)
	if again, isNew, err := store.UpsertParcel(ctx, order); err != nil || isNew || again.ID != byOrder.ID || again.Status != parcel.StatusInTransit {
		t.Fatalf("order merge = %+v %v %v", again, isNew, err)
	}
	handOff := order
	handOff.Carrier, handOff.Number, handOff.UpdatedAt = "dhl", "EX987654321", parcelNow.Add(2*time.Hour)
	adopted, isNew, err := store.UpsertParcel(ctx, handOff)
	if err != nil || isNew || adopted.ID != byOrder.ID || adopted.Carrier != "dhl" || adopted.Number != "EX987654321" {
		t.Fatalf("hand-off = %+v %v %v", adopted, isNew, err)
	}
	if parcels, err := store.ListParcels(ctx, "alice"); err != nil || len(parcels) != 2 {
		t.Fatalf("parcels = %+v %v", parcels, err)
	}

	bad := shippedParcel("alice", "short")
	if _, _, err := store.UpsertParcel(ctx, bad); err == nil {
		t.Fatal("a number that is not normalised was stored")
	}
}

func TestParcelsAreScopedToTheirOwner(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	alices, _, err := store.UpsertParcel(ctx, shippedParcel("alice", "EX123456789NL"))
	if err != nil {
		t.Fatal(err)
	}
	bobs, isNew, err := store.UpsertParcel(ctx, shippedParcel("bob", "EX123456789NL"))
	if err != nil || !isNew || bobs.ID == alices.ID {
		t.Fatalf("the same number for another principal = %+v %v %v", bobs, isNew, err)
	}
	if _, found, err := store.LoadParcel(ctx, "bob", alices.ID); err != nil || found {
		t.Fatalf("bob loaded alice's parcel: %v %v", found, err)
	}
	if err := store.SaveCard(ctx, "bob", alices.ID, 5, parcel.CardText); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := store.LoadParcel(ctx, "alice", alices.ID); again.CardMessage != 0 {
		t.Fatal("bob set the card of alice's parcel")
	}
	if _, found, err := store.StopParcel(ctx, "bob", alices.ID, parcelNow); err != nil || found {
		t.Fatalf("bob stopped alice's parcel: %v %v", found, err)
	}
	if recorded, err := store.RecordNotice(ctx, "bob", alices.ID, parcel.StatusDelivered, parcelNow); err != nil || recorded {
		t.Fatalf("bob recorded a notice for alice's parcel: %v %v", recorded, err)
	}
	if parcels, err := store.ListParcels(ctx, "alice"); err != nil || len(parcels) != 1 || parcels[0].ID != alices.ID {
		t.Fatalf("alice's parcels = %+v %v", parcels, err)
	}
	if err := store.SaveSettings(ctx, parcel.Settings{PrincipalID: "alice", Postcode: "1234AB"}); err != nil {
		t.Fatal(err)
	}
	if settings, err := store.LoadSettings(ctx, "bob"); err != nil || settings.Postcode != "" {
		t.Fatalf("bob's settings = %+v %v", settings, err)
	}
}

func TestStoppingAParcelLeavesATombstoneAndFreezesIt(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	created, _, err := store.UpsertParcel(ctx, shippedParcel("alice", "EX123456789NL"))
	if err != nil {
		t.Fatal(err)
	}
	stopped, found, err := store.StopParcel(ctx, "alice", created.ID, parcelNow.Add(time.Hour))
	if err != nil || !found || !stopped.Stopped() {
		t.Fatalf("stop = %+v %v %v", stopped, found, err)
	}
	if seen, err := store.TombstoneSeenAny(ctx, "alice", created.Keys(), parcelNow.Add(2*time.Hour)); err != nil || !seen {
		t.Fatalf("tombstone = %v %v", seen, err)
	}
	if seen, err := store.TombstoneSeenAny(ctx, "bob", created.Keys(), parcelNow); err != nil || seen {
		t.Fatalf("bob sees alice's tombstone: %v %v", seen, err)
	}
	listed := shippedParcel("alice", "EX123456789NL")
	listed.Status, listed.UpdatedAt = parcel.StatusDelivered, parcelNow.Add(3*time.Hour)
	again, isNew, err := store.UpsertParcel(ctx, listed)
	if err != nil || isNew || again.Status != parcel.StatusInTransit || !again.Stopped() {
		t.Fatalf("a source changed a stopped parcel: %+v %v %v", again, isNew, err)
	}
}

func TestRetentionDeletesOldParcelsLeavingTombstonesThatArePruned(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	delivered := shippedParcel("alice", "EX111111111")
	delivered.Status, delivered.DeliveredAt = parcel.StatusDelivered, parcelNow.Add(-15*24*time.Hour)
	recent := shippedParcel("alice", "EX222222222")
	recent.Status, recent.DeliveredAt = parcel.StatusDelivered, parcelNow.Add(-13*24*time.Hour)
	moving := shippedParcel("alice", "EX333333333")
	stopped := shippedParcel("alice", "EX444444444")
	stoppedStored := parcel.Parcel{}
	for _, item := range []parcel.Parcel{delivered, recent, moving, stopped} {
		saved, _, err := store.UpsertParcel(ctx, item)
		if err != nil {
			t.Fatal(err)
		}
		if item.Number == stopped.Number {
			stoppedStored = saved
		}
	}
	if _, _, err := store.StopParcel(ctx, "alice", stoppedStored.ID, parcelNow.Add(-20*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	deleted, err := store.DeleteExpired(ctx, parcelNow.Add(-parcel.Retention), parcelNow)
	if err != nil || deleted != 2 {
		t.Fatalf("deleted = %d %v", deleted, err)
	}
	parcels, err := store.ListParcels(ctx, "alice")
	if err != nil || len(parcels) != 2 {
		t.Fatalf("parcels left = %+v %v", parcels, err)
	}
	for _, number := range []string{"EX111111111", "EX444444444"} {
		if seen, err := store.TombstoneSeenAny(ctx, "alice", []string{"postnl|" + number}, parcelNow); err != nil || !seen {
			t.Fatalf("no tombstone for %s: %v %v", number, seen, err)
		}
	}
	if seen, _ := store.TombstoneSeenAny(ctx, "alice", []string{"postnl|EX222222222"}, parcelNow); seen {
		t.Fatal("a parcel kept by retention has a tombstone")
	}

	later := parcelNow.Add(29 * 24 * time.Hour)
	if _, err := store.TombstoneSeenAny(ctx, "alice", []string{"postnl|EX111111111", "source|ha|key-EX111111111"}, later); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.PruneTombstones(ctx, parcelNow.Add(31*24*time.Hour).Add(-parcel.TombstoneLifetime))
	if err != nil || pruned != 2 {
		t.Fatalf("pruned = %d %v, want only the tombstone no source listed", pruned, err)
	}
	if seen, _ := store.TombstoneSeenAny(ctx, "alice", []string{"postnl|EX111111111"}, later); !seen {
		t.Fatal("a tombstone a source listed was pruned")
	}
	if seen, _ := store.TombstoneSeenAny(ctx, "alice", []string{"postnl|EX444444444"}, later); seen {
		t.Fatal("a tombstone nobody listed survived pruning")
	}
}

func TestParcelsNoSourceUpdatedAreDeletedWithoutTombstones(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	idle := shippedParcel("alice", "EX111111111")
	fresh := shippedParcel("alice", "EX222222222")
	fresh.UpdatedAt = parcelNow.Add(20 * 24 * time.Hour)
	delivered := shippedParcel("alice", "EX333333333")
	delivered.Status = parcel.StatusDelivered
	stopped := shippedParcel("alice", "EX555555555")
	bobs := shippedParcel("bob", "EX444444444")
	for _, item := range []parcel.Parcel{idle, fresh, delivered, stopped, bobs} {
		saved, _, err := store.UpsertParcel(ctx, item)
		if err != nil {
			t.Fatal(err)
		}
		if item.Number == stopped.Number {
			if _, _, err := store.StopParcel(ctx, "alice", saved.ID, parcelNow); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := parcelNow.Add(31 * 24 * time.Hour)
	dropped, err := store.DeleteIdle(ctx, "alice", now.Add(-parcel.IdleLifetime))
	if err != nil || len(dropped) != 1 || dropped[0].Number != "EX111111111" {
		t.Fatalf("dropped = %+v %v", dropped, err)
	}
	left := map[string]bool{}
	for _, principal := range []string{"alice", "bob"} {
		parcels, err := store.ListParcels(ctx, principal)
		if err != nil {
			t.Fatal(err)
		}
		for _, held := range parcels {
			left[held.Number] = true
		}
	}
	if left["EX111111111"] || !left["EX222222222"] || !left["EX333333333"] || !left["EX555555555"] || !left["EX444444444"] {
		t.Fatalf("parcels left = %v", left)
	}
	if seen, err := store.TombstoneSeenAny(ctx, "alice", []string{"postnl|EX111111111", "source|ha|key-EX111111111"}, now); err != nil || seen {
		t.Fatalf("an idle parcel left a tombstone: %v %v", seen, err)
	}
	if again, isNew, err := store.UpsertParcel(ctx, idle); err != nil || !isNew || again.Stopped() {
		t.Fatalf("news of an idle parcel = %+v %v %v", again, isNew, err)
	}
}

func TestAddedAndProposedNumbers(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	for _, principal := range []string{"alice", "bob"} {
		if err := store.SaveAddedNumber(ctx, parcel.AddedNumber{PrincipalID: principal, Carrier: "dhl", Number: "EX123456789", Domain: "example_domain", EntryID: "entry-" + principal, AddedAt: parcelNow}); err != nil {
			t.Fatal(err)
		}
	}
	added, found, err := store.LoadAddedNumber(ctx, "alice", "dhl", "EX123456789")
	if err != nil || !found || added.EntryID != "entry-alice" || !added.AddedAt.Equal(parcelNow) {
		t.Fatalf("added = %+v %v %v", added, found, err)
	}
	if owners, err := store.AddedNumberOwners(ctx, "dhl", "EX123456789"); err != nil || !slices.Equal(owners, []string{"alice", "bob"}) {
		t.Fatalf("owners = %v %v", owners, err)
	}
	if numbers, err := store.ListAddedNumbers(ctx, "bob"); err != nil || len(numbers) != 1 || numbers[0].EntryID != "entry-bob" {
		t.Fatalf("bob's numbers = %+v %v", numbers, err)
	}
	if err := store.DeleteAddedNumber(ctx, "alice", "dhl", "EX123456789"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.LoadAddedNumber(ctx, "alice", "dhl", "EX123456789"); found {
		t.Fatal("the removed number is still there")
	}
	if _, found, _ := store.LoadAddedNumber(ctx, "bob", "dhl", "EX123456789"); !found {
		t.Fatal("alice removed bob's number")
	}

	if err := store.RecordProposal(ctx, parcel.ProposedNumber{PrincipalID: "alice", Number: "EX555555555", Carrier: "ups", ProposedAt: parcelNow}); err != nil {
		t.Fatal(err)
	}
	proposal, found, err := store.LoadProposal(ctx, "alice", "EX555555555")
	if err != nil || !found || proposal.Outcome != parcel.OutcomeProposed || proposal.Carrier != "ups" {
		t.Fatalf("proposal = %+v %v %v", proposal, found, err)
	}
	if err := store.RecordProposal(ctx, parcel.ProposedNumber{PrincipalID: "alice", Number: "EX555555555", Carrier: "ups", ProposedAt: parcelNow.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if proposal, _, _ = store.LoadProposal(ctx, "alice", "EX555555555"); !proposal.ProposedAt.Equal(parcelNow.Add(time.Hour)) {
		t.Fatalf("an offered-again number kept its old time: %+v", proposal)
	}
	if err := store.SetProposalOutcome(ctx, "alice", "EX555555555", parcel.OutcomeDeclined, parcelNow.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordProposal(ctx, parcel.ProposedNumber{PrincipalID: "alice", Number: "EX555555555", ProposedAt: parcelNow.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if proposal, _, _ = store.LoadProposal(ctx, "alice", "EX555555555"); proposal.Outcome != parcel.OutcomeDeclined || !proposal.UpdatedAt.Equal(parcelNow.Add(2*time.Hour)) {
		t.Fatalf("a declined number was offered again: %+v", proposal)
	}
	if _, found, _ := store.LoadProposal(ctx, "bob", "EX555555555"); found {
		t.Fatal("bob sees alice's proposal")
	}
	if err := store.SetProposalOutcome(ctx, "alice", "EX666666666", parcel.OutcomeAdded, parcelNow); err != nil {
		t.Fatal(err)
	}
	if proposal, found, _ := store.LoadProposal(ctx, "alice", "EX666666666"); !found || proposal.Outcome != parcel.OutcomeAdded {
		t.Fatalf("a typed number's outcome = %+v %v", proposal, found)
	}
}

func TestNoticesAreSentOncePerParcelAndStatus(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	delivered := shippedParcel("alice", "EX123456789NL")
	delivered.Status, delivered.DeliveredAt = parcel.StatusDelivered, parcelNow.Add(-time.Hour)
	created, _, err := store.UpsertParcel(ctx, delivered)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status parcel.Status
		want   bool
	}{
		{parcel.StatusOutForDelivery, true}, {parcel.StatusOutForDelivery, false}, {parcel.StatusDelivered, true}, {parcel.StatusDelivered, false},
	} {
		if recorded, err := store.RecordNotice(ctx, "alice", created.ID, tc.status, parcelNow); err != nil || recorded != tc.want {
			t.Fatalf("notice %s = %v %v, want %v", tc.status, recorded, err, tc.want)
		}
	}
	if recorded, err := store.RecordNotice(ctx, "alice", "missing", parcel.StatusDelivered, parcelNow); err != nil || recorded {
		t.Fatalf("a notice for no parcel = %v %v", recorded, err)
	}
	if _, err := store.DeleteExpired(ctx, parcelNow.Add(time.Hour), parcelNow); err != nil {
		t.Fatal(err)
	}
	if count := countRows(t, store, `SELECT count(*) FROM parcel_notices_sent`); count != 0 {
		t.Fatalf("notices left after the parcel was deleted = %d", count)
	}
}

func TestMailCursorAndSettings(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	if _, found, err := store.LoadMailCursor(ctx, "alice"); err != nil || found {
		t.Fatalf("a cursor before any run = %v %v", found, err)
	}
	if err := store.SaveMailCursor(ctx, "alice", parcel.MailCursor{AfterEpoch: 1790000000, Processed: []string{"m1", "m2"}}, parcelNow); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMailCursor(ctx, "alice", parcel.MailCursor{AfterEpoch: 1790000500}, parcelNow); err != nil {
		t.Fatal(err)
	}
	cursor, found, err := store.LoadMailCursor(ctx, "alice")
	if err != nil || !found || cursor.AfterEpoch != 1790000500 || len(cursor.Processed) != 0 {
		t.Fatalf("cursor = %+v %v %v", cursor, found, err)
	}
	if err := store.SaveMailCursor(ctx, "bob", parcel.MailCursor{AfterEpoch: 5, Processed: []string{"m9"}}, parcelNow); err != nil {
		t.Fatal(err)
	}
	if cursor, _, _ = store.LoadMailCursor(ctx, "alice"); cursor.AfterEpoch != 1790000500 {
		t.Fatalf("bob's cursor changed alice's: %+v", cursor)
	}

	if settings, err := store.LoadSettings(ctx, "alice"); err != nil || settings.Postcode != "" {
		t.Fatalf("settings before saving = %+v %v", settings, err)
	}
	for _, postcode := range []string{"1234AB", "5678CD"} {
		if err := store.SaveSettings(ctx, parcel.Settings{PrincipalID: "alice", Postcode: postcode}); err != nil {
			t.Fatal(err)
		}
	}
	if settings, err := store.LoadSettings(ctx, "alice"); err != nil || settings.Postcode != "5678CD" {
		t.Fatalf("settings = %+v %v", settings, err)
	}
}

type parcelFiles struct{}

func (parcelFiles) SendFile(context.Context, string, delivery.File) error { return nil }

type parcelOffers struct{}

func (parcelOffers) Offer(context.Context, string, int64, int64, string, time.Duration, []approval.Proposal) ([]approval.Record, error) {
	return nil, nil
}

// The parcel section carries a user's parcels, numbers, tombstones, and postcode to a fresh
// database without the card messages and stopped parcels, and restoring it twice adds nothing.
func TestParcelExportRestoresIntoAnotherDatabase(t *testing.T) {
	t.Parallel()
	source, ctx := openTestStore(t)
	target, _ := openTestStore(t)

	open, _, err := source.UpsertParcel(ctx, shippedParcel("alice", "EX123456789NL"))
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SaveCard(ctx, "alice", open.ID, 99, parcel.CardWidget); err != nil {
		t.Fatal(err)
	}
	stopping, _, err := source.UpsertParcel(ctx, shippedParcel("alice", "EX222222222"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.StopParcel(ctx, "alice", stopping.ID, parcelNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.UpsertParcel(ctx, parcel.Parcel{
		PrincipalID: "alice", Carrier: parcel.ShopID, OrderRef: "123-4567890-1234567", Sender: "Example Shop", Source: parcel.SourceMail,
		Status: parcel.StatusRegistered, CreatedAt: parcelNow, UpdatedAt: parcelNow,
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.SaveAddedNumber(ctx, parcel.AddedNumber{PrincipalID: "alice", Carrier: "dhl", Number: "EX333333333", Domain: "example_domain", EntryID: "entry-1", AddedAt: parcelNow}); err != nil {
		t.Fatal(err)
	}
	if err := source.SetProposalOutcome(ctx, "alice", "EX444444444", parcel.OutcomeDeclined, parcelNow); err != nil {
		t.Fatal(err)
	}
	if err := source.SaveSettings(ctx, parcel.Settings{PrincipalID: "alice", Postcode: "1234AB"}); err != nil {
		t.Fatal(err)
	}
	if err := source.SaveMailCursor(ctx, "alice", parcel.MailCursor{AfterEpoch: 1790000000, Processed: []string{"m1"}}, parcelNow); err != nil {
		t.Fatal(err)
	}
	if _, err := source.RecordNotice(ctx, "alice", open.ID, parcel.StatusOutForDelivery, parcelNow); err != nil {
		t.Fatal(err)
	}

	keyPath := filepath.Join(t.TempDir(), "export.key")
	build := func(store *Store) *export.Service {
		service, err := export.NewService(export.Config{KeyPath: keyPath, Now: func() time.Time { return parcelNow }}, store, parcelFiles{}, parcelOffers{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Register(parcel.Exporter()); err != nil {
			t.Fatal(err)
		}
		return service
	}
	file, err := build(source).Export(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	restoring := build(target)
	report, err := restoring.Restore(ctx, "alice", file.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range report.Sections {
		if section.Failed || section.Refused != "" || len(section.Skipped) != 0 {
			t.Fatalf("section = %+v", section)
		}
	}

	parcels, err := target.ListParcels(ctx, "alice")
	if err != nil || len(parcels) != 2 {
		t.Fatalf("restored parcels = %+v %v, want the two that were not stopped", parcels, err)
	}
	for _, item := range parcels {
		if item.CardMessage != 0 || item.CardKind != "" || item.Stopped() {
			t.Fatalf("restored parcel keeps a card or a stop: %+v", item)
		}
	}
	slices.SortFunc(parcels, func(a, b parcel.Parcel) int { return strings.Compare(a.Number, b.Number) })
	if parcels[1].Number != "EX123456789NL" || parcels[1].Status != parcel.StatusInTransit || !parcels[1].WindowTo.Equal(parcelNow.Add(4*time.Hour)) || parcels[0].OrderRef != "123-4567890-1234567" {
		t.Fatalf("restored parcels = %+v", parcels)
	}
	if seen, _ := target.TombstoneSeenAny(ctx, "alice", []string{"postnl|EX222222222"}, parcelNow); !seen {
		t.Fatal("the tombstone was not restored")
	}
	if _, found, _ := target.LoadAddedNumber(ctx, "alice", "dhl", "EX333333333"); !found {
		t.Fatal("the added number was not restored")
	}
	if proposal, found, _ := target.LoadProposal(ctx, "alice", "EX444444444"); !found || proposal.Outcome != parcel.OutcomeDeclined {
		t.Fatalf("the proposal = %+v %v", proposal, found)
	}
	if settings, _ := target.LoadSettings(ctx, "alice"); settings.Postcode != "1234AB" {
		t.Fatalf("settings = %+v", settings)
	}
	if _, found, _ := target.LoadMailCursor(ctx, "alice"); found {
		t.Fatal("the mail cursor was exported")
	}
	if count := countRows(t, target, `SELECT count(*) FROM parcel_notices_sent`); count != 0 {
		t.Fatal("the notices sent were exported")
	}

	again, err := restoring.Restore(ctx, "alice", file.Data)
	if err != nil {
		t.Fatal(err)
	}
	if added := again.Additions(); added != 0 {
		t.Fatalf("a second restore added %d records", added)
	}
	if parcels, _ := target.ListParcels(ctx, "alice"); len(parcels) != 2 {
		t.Fatalf("parcels after the second restore = %d", len(parcels))
	}
}

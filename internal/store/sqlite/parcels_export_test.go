package sqlite

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
)

// A restore never brings back a parcel the user stopped: the file's parcel is skipped when the
// account holds a tombstone for any of its keys.
func TestParcelRestoreSkipsAStoppedParcel(t *testing.T) {
	t.Parallel()
	source, ctx := openTestStore(t)
	target, _ := openTestStore(t)
	if _, _, err := source.UpsertParcel(ctx, shippedParcel("alice", "EX123456789NL")); err != nil {
		t.Fatal(err)
	}
	stopped, _, err := target.UpsertParcel(ctx, shippedParcel("alice", "EX123456789NL"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := target.StopParcel(ctx, "alice", stopped.ID, parcelNow); err != nil {
		t.Fatal(err)
	}
	if err := source.RecordProposal(ctx, parcel.ProposedNumber{PrincipalID: "alice", Number: "EX555555555", ApprovalID: "approval-1", ProposedAt: parcelNow}); err != nil {
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
	report, err := build(target).Restore(ctx, "alice", file.Data)
	if err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, section := range report.Sections {
		for _, item := range section.Skipped {
			if item.Reason == "stopped" {
				skipped++
			}
		}
	}
	if skipped != 1 {
		t.Fatalf("report = %+v, want the stopped parcel skipped", report.Sections)
	}
	if parcels, _ := target.ListParcels(ctx, "alice"); len(parcels) != 1 || !parcels[0].Stopped() {
		t.Fatalf("parcels = %+v, want only the stopped one", parcels)
	}
	proposal, found, err := target.LoadProposal(ctx, "alice", "EX555555555")
	if err != nil || !found || proposal.ApprovalID != "" || proposal.Outcome != parcel.OutcomeProposed || proposal.Offers != 1 {
		t.Fatalf("restored proposal = %+v %v %v, want one without an approval", proposal, found, err)
	}
}

package sqlite

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/intake"
)

var intakeNow = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func intakeRecord(id, sourceRef, priority, update string) intake.Record {
	return intake.Record{
		ID: id, PrincipalID: "alice", Source: intake.SourceMail, SourceRef: sourceRef, Kind: "newsletter", Title: "Street works",
		Facts:    intake.Facts{Dates: []intake.Date{}, Amounts: []intake.Amount{}, References: []intake.Reference{}},
		Priority: priority, Update: update, CreatedAt: intakeNow, UpdatedAt: intakeNow,
	}
}

// A row written before the priority columns existed reads as an action with no update.
func TestIntakeRecordWithoutPriorityReadsAsAction(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO intake_records (id, principal_id, source, source_ref, kind, title, sender, facts, action_required, created_at, updated_at)
		VALUES ('aaaaaaaaaaaaaaa1', 'alice', 'file', 'att-1', 'invoice', 'Water bill', '', '{"dates":[],"amounts":[],"references":[]}', 'Pay it', ?, ?)`,
		formatTime(intakeNow), formatTime(intakeNow)); err != nil {
		t.Fatal(err)
	}
	record, found, err := store.LoadIntakeRecord(ctx, "alice", "aaaaaaaaaaaaaaa1")
	if err != nil || !found || record.Priority != intake.PriorityAction || record.Update != "" {
		t.Fatalf("record = %+v %v %v, want an action without an update", record, found, err)
	}
}

func TestIntakePriorityAndUpdateRoundTrip(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	for _, record := range []intake.Record{
		intakeRecord("aaaaaaaaaaaaaaa2", "a/1", intake.PriorityUpdate, "Road closed until Friday."),
		intakeRecord("aaaaaaaaaaaaaaa3", "a/2", "", ""),
	} {
		if created, err := store.CreateIntakeRecord(ctx, record); err != nil || !created {
			t.Fatalf("create = %v %v", created, err)
		}
	}
	update, _, _ := store.LoadIntakeRecord(ctx, "alice", "aaaaaaaaaaaaaaa2")
	if update.Priority != intake.PriorityUpdate || update.Update != "Road closed until Friday." {
		t.Fatalf("update record = %+v", update)
	}
	plain, _, _ := store.LoadIntakeRecordBySource(ctx, "alice", intake.SourceMail, "a/2")
	if plain.Priority != intake.PriorityAction {
		t.Fatalf("an empty priority = %q, want action", plain.Priority)
	}
}

// intakeWithoutPriority exports as an earlier image did, before the priority columns.
type intakeWithoutPriority struct{ export.Exporter }

func (e intakeWithoutPriority) Section() export.Section {
	section := e.Exporter.Section()
	section.Tables = []export.Table{{Name: section.Tables[0].Name, Omit: []string{"pending_actions", "priority", "update_text"}}}
	return section
}

func TestIntakeExportCarriesPriorityAndUpdate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		exporter     export.Exporter
		wantPriority string
		wantUpdate   string
	}{
		{"current", intake.Exporter(), intake.PriorityUpdate, "Road closed until Friday."},
		{"from before the fields", intakeWithoutPriority{intake.Exporter()}, intake.PriorityAction, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source, ctx := openTestStore(t)
			target, _ := openTestStore(t)
			if _, err := source.CreateIntakeRecord(ctx, intakeRecord("aaaaaaaaaaaaaaa2", "a/1", intake.PriorityUpdate, "Road closed until Friday.")); err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(t.TempDir(), "export.key")
			build := func(store *Store) *export.Service {
				service, err := export.NewService(export.Config{KeyPath: keyPath, Now: func() time.Time { return intakeNow }}, store, parcelFiles{}, parcelOffers{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatal(err)
				}
				if err := service.Register(test.exporter); err != nil {
					t.Fatal(err)
				}
				return service
			}
			file, err := build(source).Export(ctx, "alice")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := build(target).Restore(ctx, "alice", file.Data); err != nil {
				t.Fatal(err)
			}
			restored, found, err := target.LoadIntakeRecordBySource(ctx, "alice", intake.SourceMail, "a/1")
			if err != nil || !found || restored.Priority != test.wantPriority || restored.Update != test.wantUpdate {
				t.Fatalf("restored = %+v %v %v, want priority %q and update %q", restored, found, err, test.wantPriority, test.wantUpdate)
			}
		})
	}
}

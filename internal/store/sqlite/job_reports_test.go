package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

// Reports are told once, found by their messages only by their owner, outlive their job, and are
// pruned by age and by count.
func TestJobReportsAreKeptPerPrincipal(t *testing.T) {
	store, ctx := openTestStore(t)
	at := time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC)
	job := schedule.Job{
		ID: "0123456789abcdef", PrincipalID: "alice", Name: "Digest", Type: schedule.TypeModelTurn,
		Cron: "0 9 * * *", Zone: "Europe/Amsterdam", Config: json.RawMessage(`{"prompt":"x"}`), Enabled: true,
		NextRunAt: at, CreatedAt: at, UpdatedAt: at,
	}
	if created, err := store.CreateScheduledJob(ctx, job, 5); err != nil || !created {
		t.Fatalf("create job = %v %v", created, err)
	}
	report := func(id string, delivered time.Time, messages ...int64) schedule.Report {
		sent := make([]schedule.Sent, 0, len(messages))
		for _, messageID := range messages {
			sent = append(sent, schedule.Sent{MessageID: messageID})
		}
		return schedule.Report{ID: id, PrincipalID: "alice", JobID: job.ID, JobName: job.Name, JobType: job.Type, Text: "report " + id, DeliveredAt: delivered, Messages: sent}
	}
	if err := store.SaveJobReport(ctx, report("aaaaaaaaaaaaaaa1", at, 11, 12), 200); err != nil {
		t.Fatal(err)
	}
	// Saving again, as a retried delivery does, adds only the new message.
	if err := store.SaveJobReport(ctx, report("aaaaaaaaaaaaaaa1", at.Add(time.Minute), 13), 200); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJobReport(ctx, report("aaaaaaaaaaaaaaa2", at.Add(time.Hour), 21), 200); err != nil {
		t.Fatal(err)
	}

	taken, err := store.TakeJobReports(ctx, "alice")
	if err != nil || len(taken) != 2 || taken[0].ID != "aaaaaaaaaaaaaaa1" || !taken[0].DeliveredAt.Equal(at) || taken[1].ID != "aaaaaaaaaaaaaaa2" {
		t.Fatalf("taken = %+v %v", taken, err)
	}
	if again, err := store.TakeJobReports(ctx, "alice"); err != nil || len(again) != 0 {
		t.Fatalf("taken again = %+v %v", again, err)
	}

	for _, message := range []int64{11, 13} {
		if found, ok, err := store.JobReportByMessage(ctx, "alice", message); err != nil || !ok || found.ID != "aaaaaaaaaaaaaaa1" {
			t.Fatalf("report of message %d = %+v %v %v", message, found, ok, err)
		}
	}
	if _, ok, err := store.JobReportByMessage(ctx, "bob", 11); err != nil || ok {
		t.Fatalf("bob found alice's report by message: %v %v", ok, err)
	}
	if _, ok, err := store.LoadJobReport(ctx, "bob", "aaaaaaaaaaaaaaa1"); err != nil || ok {
		t.Fatalf("bob loaded alice's report: %v %v", ok, err)
	}
	if has, err := store.HasJobReports(ctx, "bob"); err != nil || has {
		t.Fatalf("bob has reports = %v %v", has, err)
	}

	if deleted, err := store.DeleteScheduledJob(ctx, "alice", job.ID); err != nil || !deleted {
		t.Fatalf("delete job = %v %v", deleted, err)
	}
	if kept, ok, err := store.LoadJobReport(ctx, "alice", "aaaaaaaaaaaaaaa2"); err != nil || !ok || kept.Text != "report aaaaaaaaaaaaaaa2" {
		t.Fatalf("report after its job was deleted = %+v %v %v", kept, ok, err)
	}

	if err := store.PruneJobReports(ctx, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if recent, err := store.RecentJobReports(ctx, "alice", time.Time{}); err != nil || len(recent) != 1 || recent[0].ID != "aaaaaaaaaaaaaaa2" {
		t.Fatalf("after pruning by age = %+v %v", recent, err)
	}
	if _, ok, err := store.JobReportByMessage(ctx, "alice", 11); err != nil || ok {
		t.Fatalf("a pruned report's message still finds it: %v %v", ok, err)
	}

	for i := range 3 {
		if err := store.SaveJobReport(ctx, report(fmt.Sprintf("bbbbbbbbbbbbbbb%d", i), at.Add(time.Duration(2+i)*time.Hour)), 2); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := store.RecentJobReports(ctx, "alice", time.Time{})
	if err != nil || len(recent) != 2 || recent[0].ID != "bbbbbbbbbbbbbbb2" || recent[1].ID != "bbbbbbbbbbbbbbb1" {
		t.Fatalf("after pruning by count = %+v %v", recent, err)
	}
}

// Migration 028 adds external to reports kept before it, which stay external because their runs
// are unknown.
func TestMigrationJobReportExternalKeepsExistingReportsExternal(t *testing.T) {
	ctx := t.Context()
	path, err := filepath.Abs(filepath.Join(t.TempDir(), "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.name >= "028" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-09-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO job_reports (id, principal_id, job_id, job_name, job_type, text, delivered_at)
		 VALUES ('aaaaaaaaaaaaaaa1', 'alice', 'a1b2c3d4e5f60718', 'Digest', 'model_turn', 'old report', '2026-09-01T10:00:00.000Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	old, ok, err := store.LoadJobReport(ctx, "alice", "aaaaaaaaaaaaaaa1")
	if err != nil || !ok || !old.External {
		t.Fatalf("old report = %+v %v %v, want external", old, ok, err)
	}
	clean := schedule.Report{ID: "aaaaaaaaaaaaaaa2", PrincipalID: "alice", JobID: "a1b2c3d4e5f60718", JobName: "Digest", JobType: "model_turn", Text: "clean", DeliveredAt: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}
	if err := store.SaveJobReport(ctx, clean, 200); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.LoadJobReport(ctx, "alice", clean.ID); err != nil || !ok || got.External {
		t.Fatalf("clean report = %+v %v %v, want not external", got, ok, err)
	}
}

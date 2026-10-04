package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

// Two ticks that read the same due job must queue its occurrence once, and a job's owner is the
// only principal who can see or change it.
func TestScheduledOccurrencesAreClaimedOnce(t *testing.T) {
	store, ctx := openTestStore(t)
	due := time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC)
	job := schedule.Job{
		ID: "0123456789abcdef", PrincipalID: "alice", Name: "Digest", Type: schedule.TypeModelTurn,
		Cron: "0 9 * * *", Zone: "Europe/Amsterdam", Config: json.RawMessage(`{"prompt":"x"}`), Enabled: true,
		NextRunAt: due, CreatedAt: due.Add(-time.Hour), UpdatedAt: due.Add(-time.Hour),
	}
	if created, err := store.CreateScheduledJob(ctx, job, 1); err != nil || !created {
		t.Fatalf("create = %v %v", created, err)
	}
	if created, err := store.CreateScheduledJob(ctx, job, 5); err != nil || created {
		t.Fatalf("creating the same job again = %v %v", created, err)
	}
	second := job
	second.ID = "fedcba9876543210"
	if _, err := store.CreateScheduledJob(ctx, second, 1); !errors.Is(err, schedule.ErrTooManyJobs) {
		t.Fatalf("a job over the limit = %v", err)
	}

	next := due.Add(24 * time.Hour)
	first := &schedule.Run{ID: "1111111111111111", JobID: job.ID, PrincipalID: "alice", ScheduledFor: due}
	again := &schedule.Run{ID: "2222222222222222", JobID: job.ID, PrincipalID: "alice", ScheduledFor: due}
	if advanced, err := store.AdvanceScheduledJob(ctx, job.ID, due, next, first); err != nil || !advanced {
		t.Fatalf("first claim = %v %v", advanced, err)
	}
	if advanced, err := store.AdvanceScheduledJob(ctx, job.ID, due, next, again); err != nil || advanced {
		t.Fatalf("second claim of the same occurrence = %v %v", advanced, err)
	}
	if err := store.QueueScheduledRun(ctx, *again); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ScheduledRuns(ctx, schedule.StatusQueued)
	if err != nil || len(runs) != 1 || runs[0].ID != first.ID || runs[0].JobName != "Digest" {
		t.Fatalf("queued runs = %+v %v", runs, err)
	}

	if _, found, err := store.LoadScheduledJob(ctx, "bob", job.ID); err != nil || found {
		t.Fatalf("bob loaded alice's job: %v %v", found, err)
	}
	if changed, err := store.SetScheduledJobEnabled(ctx, "bob", job.ID, false, next, due); err != nil || changed {
		t.Fatalf("bob paused alice's job: %v %v", changed, err)
	}
	if deleted, err := store.DeleteScheduledJob(ctx, "bob", job.ID); err != nil || deleted {
		t.Fatalf("bob deleted alice's job: %v %v", deleted, err)
	}
}

func oneTimeJob(id, principalID string, runAt time.Time) schedule.Job {
	return schedule.Job{
		ID: id, PrincipalID: principalID, Name: "Call mom", Type: schedule.TypeReminder,
		RunAt: runAt, Zone: "Europe/Amsterdam", Config: json.RawMessage(`{"text":"Call mom"}`), Enabled: true,
		NextRunAt: runAt, CreatedAt: runAt.Add(-time.Hour), UpdatedAt: runAt.Add(-time.Hour),
	}
}

// A one-time job queues its single run and finishes in the same transaction, and a finished job
// is neither due, listed, nor counted toward the limit, yet still loads so it can be snoozed.
func TestOneTimeJobQueuesOneRunAndFinishesAtomically(t *testing.T) {
	store, ctx := openTestStore(t)
	due := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	job := oneTimeJob("0123456789abcdef", "alice", due)
	if created, err := store.CreateScheduledJob(ctx, job, 1); err != nil || !created {
		t.Fatalf("create = %v %v", created, err)
	}
	if jobs, err := store.ListScheduledJobs(ctx, "alice"); err != nil || len(jobs) != 1 || !jobs[0].OneTime() || !jobs[0].RunAt.Equal(due) {
		t.Fatalf("listed = %+v %v", jobs, err)
	}
	if jobs, err := store.DueScheduledJobs(ctx, due); err != nil || len(jobs) != 1 {
		t.Fatalf("due = %+v %v", jobs, err)
	}

	first := &schedule.Run{ID: "1111111111111111", JobID: job.ID, PrincipalID: "alice", ScheduledFor: due}
	again := &schedule.Run{ID: "2222222222222222", JobID: job.ID, PrincipalID: "alice", ScheduledFor: due}
	if advanced, err := store.AdvanceScheduledJob(ctx, job.ID, due, time.Time{}, first); err != nil || !advanced {
		t.Fatalf("finishing claim = %v %v", advanced, err)
	}
	if advanced, err := store.AdvanceScheduledJob(ctx, job.ID, due, time.Time{}, again); err != nil || advanced {
		t.Fatalf("second claim = %v %v", advanced, err)
	}
	runs, err := store.ScheduledRuns(ctx, schedule.StatusQueued)
	if err != nil || len(runs) != 1 || runs[0].ID != first.ID || !runs[0].JobOneTime || runs[0].JobType != schedule.TypeReminder {
		t.Fatalf("queued runs = %+v %v", runs, err)
	}

	if jobs, err := store.DueScheduledJobs(ctx, due.Add(time.Hour)); err != nil || len(jobs) != 0 {
		t.Fatalf("a finished job is due: %+v %v", jobs, err)
	}
	if jobs, err := store.ListScheduledJobs(ctx, "alice"); err != nil || len(jobs) != 0 {
		t.Fatalf("a finished job is listed: %+v %v", jobs, err)
	}
	loaded, found, err := store.LoadScheduledJob(ctx, "alice", job.ID)
	if err != nil || !found || loaded.FinishedAt.IsZero() {
		t.Fatalf("finished job = %+v %v %v", loaded, found, err)
	}

	// With a limit of one, the finished job leaves room for another.
	next := oneTimeJob("fedcba9876543210", "alice", due.Add(time.Hour))
	if created, err := store.CreateScheduledJob(ctx, next, 1); err != nil || !created {
		t.Fatalf("a job after the finished one = %v %v", created, err)
	}
	if _, err := store.CreateScheduledJob(ctx, oneTimeJob("00000000000000aa", "alice", due.Add(2*time.Hour)), 1); !errors.Is(err, schedule.ErrTooManyJobs) {
		t.Fatalf("a second pending job over the limit = %v", err)
	}
}

// Recurring and one-time jobs are limited separately, so a full set of one kind never blocks the
// other.
func TestRecurringAndOneTimeLimitsAreCountedSeparately(t *testing.T) {
	store, ctx := openTestStore(t)
	at := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	recurring := func(n int) schedule.Job {
		return schedule.Job{
			ID: fmt.Sprintf("%016x", n), PrincipalID: "alice", Name: "Digest", Type: schedule.TypeModelTurn,
			Cron: "0 9 * * *", Zone: "Europe/Amsterdam", Config: json.RawMessage(`{"prompt":"x"}`), Enabled: true,
			NextRunAt: at, CreatedAt: at, UpdatedAt: at,
		}
	}
	for n := 1; n <= schedule.MaximumJobsPerPrincipal; n++ {
		if created, err := store.CreateScheduledJob(ctx, recurring(n), schedule.MaximumJobsPerPrincipal); err != nil || !created {
			t.Fatalf("recurring %d = %v %v", n, created, err)
		}
	}
	if _, err := store.CreateScheduledJob(ctx, recurring(1000), schedule.MaximumJobsPerPrincipal); !errors.Is(err, schedule.ErrTooManyJobs) {
		t.Fatalf("a 51st recurring job = %v", err)
	}
	for n := 1; n <= schedule.MaximumOneTimeJobsPerPrincipal; n++ {
		job := oneTimeJob(fmt.Sprintf("%016x", 2000+n), "alice", at.Add(time.Duration(n)*time.Minute))
		if created, err := store.CreateScheduledJob(ctx, job, schedule.MaximumOneTimeJobsPerPrincipal); err != nil || !created {
			t.Fatalf("one-time %d = %v %v", n, created, err)
		}
	}
	over := oneTimeJob(fmt.Sprintf("%016x", 3000), "alice", at.Add(time.Hour))
	if _, err := store.CreateScheduledJob(ctx, over, schedule.MaximumOneTimeJobsPerPrincipal); !errors.Is(err, schedule.ErrTooManyJobs) {
		t.Fatalf("a 51st one-time job = %v", err)
	}
	if created, err := store.CreateScheduledJob(ctx, oneTimeJob(fmt.Sprintf("%016x", 3001), "bob", at), schedule.MaximumOneTimeJobsPerPrincipal); err != nil || !created {
		t.Fatalf("bob's first one-time job = %v %v", created, err)
	}
}

// Only the owner can bring a finished reminder back, and only a finished one of the named type.
func TestReviveScheduledJobIsOwnerOnly(t *testing.T) {
	store, ctx := openTestStore(t)
	due := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	job := oneTimeJob("0123456789abcdef", "alice", due)
	if _, err := store.CreateScheduledJob(ctx, job, 5); err != nil {
		t.Fatal(err)
	}
	later := due.Add(10 * time.Minute)
	if revived, err := store.ReviveScheduledJob(ctx, "alice", job.ID, schedule.TypeReminder, later, due); err != nil || revived {
		t.Fatalf("reviving a pending reminder = %v %v", revived, err)
	}
	run := &schedule.Run{ID: "1111111111111111", JobID: job.ID, PrincipalID: "alice", ScheduledFor: due}
	if advanced, err := store.AdvanceScheduledJob(ctx, job.ID, due, time.Time{}, run); err != nil || !advanced {
		t.Fatalf("finish = %v %v", advanced, err)
	}

	if revived, err := store.ReviveScheduledJob(ctx, "bob", job.ID, schedule.TypeReminder, later, due); err != nil || revived {
		t.Fatalf("bob revived alice's reminder: %v %v", revived, err)
	}
	if revived, err := store.ReviveScheduledJob(ctx, "alice", job.ID, schedule.TypeModelTurn, later, due); err != nil || revived {
		t.Fatalf("a reminder revived as another type: %v %v", revived, err)
	}
	if jobs, err := store.ListScheduledJobs(ctx, "alice"); err != nil || len(jobs) != 0 {
		t.Fatalf("the reminder came back for the wrong caller: %+v %v", jobs, err)
	}

	if revived, err := store.ReviveScheduledJob(ctx, "alice", job.ID, schedule.TypeReminder, later, due); err != nil || !revived {
		t.Fatalf("alice's revive = %v %v", revived, err)
	}
	jobs, err := store.ListScheduledJobs(ctx, "alice")
	if err != nil || len(jobs) != 1 || !jobs[0].RunAt.Equal(later) || !jobs[0].NextRunAt.Equal(later) || !jobs[0].FinishedAt.IsZero() {
		t.Fatalf("revived = %+v %v", jobs, err)
	}
	if revived, err := store.ReviveScheduledJob(ctx, "alice", job.ID, schedule.TypeReminder, later, due); err != nil || revived {
		t.Fatalf("a second revive = %v %v", revived, err)
	}
}

// Pruning removes a finished one-time job only after its runs are done, and never a recurring one.
func TestPruneRemovesFinishedOneTimeJobsOnly(t *testing.T) {
	store, ctx := openTestStore(t)
	due := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	done := oneTimeJob("0123456789abcdef", "alice", due)
	open := oneTimeJob("fedcba9876543210", "alice", due)
	recurring := schedule.Job{
		ID: "00000000000000cc", PrincipalID: "alice", Name: "Digest", Type: schedule.TypeModelTurn,
		Cron: "0 9 * * *", Zone: "Europe/Amsterdam", Config: json.RawMessage(`{"prompt":"x"}`), Enabled: true,
		NextRunAt: due, CreatedAt: due, UpdatedAt: due,
	}
	for _, job := range []schedule.Job{done, open, recurring} {
		if _, err := store.CreateScheduledJob(ctx, job, 5); err != nil {
			t.Fatal(err)
		}
	}
	for index, job := range []schedule.Job{done, open} {
		run := &schedule.Run{ID: fmt.Sprintf("%016d", index+1), JobID: job.ID, PrincipalID: "alice", ScheduledFor: due}
		if advanced, err := store.AdvanceScheduledJob(ctx, job.ID, due, time.Time{}, run); err != nil || !advanced {
			t.Fatalf("finish %s = %v %v", job.ID, advanced, err)
		}
	}
	if ok, err := store.TransitionScheduledRun(ctx, "0000000000000001", schedule.StatusQueued, schedule.StatusSkipped, "", "stale", time.Now()); err != nil || !ok {
		t.Fatalf("finish run = %v %v", ok, err)
	}

	if err := store.PruneScheduledRuns(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.LoadScheduledJob(ctx, "alice", done.ID); found {
		t.Fatal("a finished job with finished runs was kept")
	}
	if _, found, _ := store.LoadScheduledJob(ctx, "alice", open.ID); !found {
		t.Fatal("a finished job with an open run was pruned")
	}
	if _, found, _ := store.LoadScheduledJob(ctx, "alice", recurring.ID); !found {
		t.Fatal("a recurring job was pruned")
	}
}

// Migration 011 rebuilds both schedule tables; the recurring jobs and runs stored before it must
// survive with their values, and the rebuilt tables must accept one-time jobs.
func TestMigrationOneTimeJobsKeepsExistingRows(t *testing.T) {
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
		if m.name >= "011" {
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
		`INSERT INTO scheduled_jobs (id, principal_id, name, job_type, cron, zone, config, enabled, next_run_at, created_at, updated_at)
		 VALUES ('0123456789abcdef', 'alice', 'Digest', 'model.turn', '0 9 * * *', 'Europe/Amsterdam', '{"prompt":"x"}', 0, '2026-10-05T07:00:00.000Z', '2026-09-01T00:00:00.000Z', '2026-09-02T00:00:00.000Z')`,
		`INSERT INTO schedule_runs (id, job_id, principal_id, scheduled_for, status, reply, created_at, updated_at)
		 VALUES ('1111111111111111', '0123456789abcdef', 'alice', '2026-10-04T07:00:00.000Z', 'ready', 'report', '2026-10-04T07:00:00.000Z', '2026-10-04T07:00:05.000Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}

	job, found, err := store.LoadScheduledJob(ctx, "alice", "0123456789abcdef")
	if err != nil || !found {
		t.Fatalf("job after migration = %v %v", found, err)
	}
	if job.Name != "Digest" || job.Type != schedule.TypeModelTurn || job.Cron != "0 9 * * *" || job.OneTime() || job.Enabled || !job.FinishedAt.IsZero() ||
		string(job.Config) != `{"prompt":"x"}` || !job.NextRunAt.Equal(time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("job after migration = %+v", job)
	}
	runs, err := store.ScheduledRuns(ctx, schedule.StatusReady)
	if err != nil || len(runs) != 1 || runs[0].ID != "1111111111111111" || runs[0].Reply != "report" || runs[0].JobName != "Digest" {
		t.Fatalf("runs after migration = %+v %v", runs, err)
	}

	due := time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)
	if created, err := store.CreateScheduledJob(ctx, oneTimeJob("fedcba9876543210", "alice", due), 5); err != nil || !created {
		t.Fatalf("one-time job after migration = %v %v", created, err)
	}
	var foreignKeyProblems int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyProblems); err != nil || foreignKeyProblems != 0 {
		t.Fatalf("foreign key check = %d %v", foreignKeyProblems, err)
	}
}

// An update applies only to the owner's unfinished job as it was read, and keeps the pause state.
func TestUpdateScheduledJobRefusesAStaleRead(t *testing.T) {
	store, ctx := openTestStore(t)
	read := time.Date(2026, 9, 28, 6, 0, 0, 0, time.UTC)
	job := schedule.Job{
		ID: "0123456789abcdef", PrincipalID: "alice", Name: "Digest", Type: schedule.TypeModelTurn,
		Cron: "0 9 * * *", Zone: "Europe/Amsterdam", Config: json.RawMessage(`{"prompt":"x"}`), Enabled: false,
		NextRunAt: read.Add(time.Hour), CreatedAt: read, UpdatedAt: read,
	}
	if created, err := store.CreateScheduledJob(ctx, job, 1); err != nil || !created {
		t.Fatalf("create = %v %v", created, err)
	}
	changed := job
	changed.Name, changed.Cron, changed.Config = "Evening digest", "0 18 * * *", json.RawMessage(`{"prompt":"y"}`)
	changed.NextRunAt, changed.UpdatedAt = read.Add(10*time.Hour), read.Add(time.Minute)

	stranger := changed
	stranger.PrincipalID = "bob"
	if updated, err := store.UpdateScheduledJob(ctx, stranger, read); err != nil || updated {
		t.Fatalf("bob updated alice's job: %v %v", updated, err)
	}
	if updated, err := store.UpdateScheduledJob(ctx, changed, read.Add(time.Second)); err != nil || updated {
		t.Fatalf("an update from a stale read = %v %v", updated, err)
	}
	if updated, err := store.UpdateScheduledJob(ctx, changed, read); err != nil || !updated {
		t.Fatalf("update = %v %v", updated, err)
	}
	if updated, err := store.UpdateScheduledJob(ctx, changed, read); err != nil || updated {
		t.Fatalf("the same update twice = %v %v", updated, err)
	}
	stored, _, err := store.LoadScheduledJob(ctx, "alice", job.ID)
	if err != nil || stored.Name != "Evening digest" || stored.Cron != "0 18 * * *" || string(stored.Config) != `{"prompt":"y"}` ||
		stored.Enabled || !stored.NextRunAt.Equal(changed.NextRunAt) || !stored.CreatedAt.Equal(read) {
		t.Fatalf("stored = %+v %v", stored, err)
	}
}

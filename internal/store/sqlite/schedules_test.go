package sqlite

import (
	"encoding/json"
	"errors"
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

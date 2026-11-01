package telegram

import (
	"strings"
	"testing"
)

// /jobs shows a job anchored to a place it waits to arrive at, rather than its far-off next run.
func TestJobsCommandShowsAPlaceJobWaitingForArrival(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO scheduled_jobs (id, principal_id, name, job_type, anchor, zone, config, enabled, next_run_at, created_at, updated_at)
		VALUES ('a1b2c3d4e5f60718', 'alice', 'Water the plants', 'reminder.text', 'repeating', 'Europe/Amsterdam',
		'{"text":"Water the plants","anchor":{"kind":"place","place_id":"place-1","edge":"arrive","offset_minutes":0,"label":"Home"}}', 1,
		'2126-09-28T06:00:00.000Z', '2026-09-28T06:00:00.000Z', '2026-09-28T06:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	list := h.requestJobs(1)
	if !strings.Contains(list.text, "1. ⏰ Water the plants\non arriving at Home, next waits for arrival") {
		t.Fatalf("jobs = %q", list.text)
	}
}

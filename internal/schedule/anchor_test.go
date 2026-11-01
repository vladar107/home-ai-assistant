package schedule_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// A job anchored to arriving at a place waits for an arrival, runs when one sets its next run, and
// with once finishes after that run.
func TestAPlaceJobWithOnceFinishesAfterItsRun(t *testing.T) {
	f := newTravelFixture(t)
	config, err := schedule.ReminderConfig("Water the plants")
	if err != nil {
		t.Fatal(err)
	}
	config, err = schedule.JoinConfig(config, &schedule.Anchor{Kind: schedule.AnchorPlace, PlaceID: "place-1", Edge: schedule.EdgeArrive, Once: true, Label: "Home"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := f.clock()
	waiting := now.AddDate(100, 0, 0)
	job := schedule.Job{
		ID: "0000000000000001", PrincipalID: "alice", Name: "Water the plants", Type: schedule.TypeReminder, Zone: "UTC",
		Config: config, Enabled: true, NextRunAt: waiting, CreatedAt: now, UpdatedAt: now, AnchorRepeats: true,
	}
	if created, err := f.store.CreateScheduledJob(t.Context(), job, schedule.MaximumJobsPerPrincipal); err != nil || !created {
		t.Fatalf("create = %v %v", created, err)
	}
	if got := schedule.NextRunText(f.job(job.ID), "UTC"); got != "waits for arrival" {
		t.Fatalf("next run = %q", got)
	}
	if listed, err := f.service.Execute(t.Context(), "alice", tools.Call{Name: "schedule_list", Arguments: json.RawMessage(`{}`)}); err != nil ||
		!strings.Contains(listed.Text, "on arriving at Home, once") {
		t.Fatalf("schedule_list = %+v %v", listed, err)
	}
	if changed, err := f.service.SetEnabled(t.Context(), "alice", job.ID, false); err != nil || !changed {
		t.Fatalf("pause = %v %v", changed, err)
	}
	if changed, err := f.service.SetEnabled(t.Context(), "alice", job.ID, true); err != nil || !changed || !f.job(job.ID).NextRunAt.After(now.AddDate(99, 0, 0)) {
		t.Fatalf("resume = %v %v, next %v", changed, err, f.job(job.ID).NextRunAt)
	}
	f.start()

	arrived := now.Add(5 * time.Minute)
	if moved, err := f.store.MoveScheduledJobOccurrence(t.Context(), job.ID, f.job(job.ID).NextRunAt, time.Time{}, arrived, "place-1:arrive"); err != nil || !moved {
		t.Fatalf("arrival = %v %v", moved, err)
	}
	f.set(arrived.Add(time.Minute))
	f.service.Wake()
	f.waitRuns(job.ID, 1)
	deadline := time.Now().Add(5 * time.Second)
	for f.job(job.ID).FinishedAt.IsZero() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := f.job(job.ID); got.FinishedAt.IsZero() || f.runs(job.ID) != 1 {
		t.Fatalf("place job after its run = %+v, runs %d", got, f.runs(job.ID))
	}
}

// A job anchored to a trip's start runs when the trip starts, and a cancelled trip finishes it.
func TestATripAnchoredJobFollowsTheTrip(t *testing.T) {
	f := newTravelFixture(t)
	start := f.clock().Add(48 * time.Hour)
	tripID := "trip0001"
	if err := f.store.AddTrip(t.Context(), trip.Trip{
		ID: tripID, PrincipalID: "alice", Title: "Example city", Destination: "Example city", StartsAt: start, EndsAt: start.Add(72 * time.Hour),
		Zone: "UTC", Status: trip.StatusPlanned, Source: trip.SourceChat, CreatedAt: f.clock(), UpdatedAt: f.clock(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RegisterAnchor(schedule.AnchorTrip, trip.NewAnchors(f.store)); err != nil {
		t.Fatal(err)
	}
	result, err := f.service.Execute(t.Context(), "alice", tools.Call{Name: "reminder_set", Arguments: json.RawMessage(
		`{"text":"Check the weather there","anchor":{"kind":"trip","trip_id":"` + tripID + `","edge":"start","offset_minutes":0}}`)})
	if err != nil || result.IsError || !strings.Contains(result.Text, `at the start of the trip "Example city"`) {
		t.Fatalf("reminder_set = %+v %v", result, err)
	}
	var id string
	if err := f.db.QueryRowContext(t.Context(), `SELECT id FROM scheduled_jobs`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := f.job(id); !got.RunAt.Equal(start) || got.OccurrenceKey != tripID+":start" {
		t.Fatalf("trip reminder = %+v", got)
	}
	cancelled, found, err := f.store.LoadTrip(t.Context(), "alice", tripID)
	if err != nil || !found {
		t.Fatalf("load trip = %v %v", found, err)
	}
	cancelled.Status = trip.StatusCancelled
	if updated, err := f.store.UpdateTrip(t.Context(), cancelled); err != nil || !updated {
		t.Fatalf("cancel trip = %v %v", updated, err)
	}
	f.start()
	f.service.WakeAnchors()
	deadline := time.Now().Add(5 * time.Second)
	for f.job(id).FinishedAt.IsZero() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	var reply sql.NullString
	if err := f.db.QueryRowContext(t.Context(), `SELECT error_kind FROM schedule_runs WHERE job_id = ?`, id).Scan(&reply); err != nil || reply.String != "anchor_gone" || f.job(id).FinishedAt.IsZero() {
		t.Fatalf("cancelled trip's reminder: run %q %v, job %+v", reply.String, err, f.job(id))
	}
}

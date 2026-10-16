package schedule_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type quietHandler struct{}

func (quietHandler) Validate(json.RawMessage) error { return nil }
func (quietHandler) Run(context.Context, schedule.Occurrence) (schedule.Result, error) {
	return schedule.Result{Quiet: true}, nil
}

type discardingDeliverer struct{}

func (discardingDeliverer) SendReply(context.Context, int64, int64, string) error { return nil }
func (discardingDeliverer) SendTrackedReply(context.Context, int64, int64, string) (int64, int64, error) {
	return 1, 0, nil
}
func (discardingDeliverer) SendTyping(context.Context, int64) error { return nil }
func (discardingDeliverer) SendMessageDraft(context.Context, int64, int64, string, bool) error {
	return nil
}

// tripWindow is traveling for every instant inside [from, to).
type tripWindow struct {
	mu       sync.Mutex
	from, to time.Time
	failure  error
	asked    []time.Time
}

func (w *tripWindow) Traveling(_ context.Context, _ string, at time.Time) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, at)
	if w.failure != nil {
		return false, w.failure
	}
	return !at.Before(w.from) && at.Before(w.to), nil
}

type travelFixture struct {
	t        *testing.T
	service  *schedule.Service
	store    *sqlite.Store
	proposer *recordingProposer
	trip     *tripWindow
	mu       sync.Mutex
	now      time.Time
	db       *sql.DB
}

func utc(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC)
}

func newTravelFixture(t *testing.T) *travelFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "assistant.sqlite")
	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &travelFixture{t: t, store: store, proposer: &recordingProposer{}, now: utc(5, 8, 0), db: db,
		trip: &tripWindow{from: utc(5, 0, 0), to: utc(7, 0, 0)}}
	service, err := schedule.NewService(
		schedule.Config{Zone: "UTC", TickInterval: time.Hour, Now: f.clock},
		store, store, discardingDeliverer{}, f.proposer, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Register(schedule.JobType{Name: schedule.TypeModelTurn, Handler: quietHandler{}, CatchUp: 48 * time.Hour, MinimumInterval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := service.Register(schedule.Reminder(f.clock)); err != nil {
		t.Fatal(err)
	}
	service.SetTravel(f.trip)
	f.service = service
	return f
}

func (f *travelFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *travelFixture) set(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = at
}

func (f *travelFixture) start() {
	f.t.Helper()
	if err := f.service.Start(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = f.service.Shutdown(context.Background()) })
}

func (f *travelFixture) create(id, name string, timing schedule.Timing, flagged bool) schedule.Job {
	f.t.Helper()
	config, err := schedule.ModelTurnConfig("Tell me about the day.")
	if err != nil {
		f.t.Fatal(err)
	}
	prepared, err := f.service.Prepare("alice", name, schedule.TypeModelTurn, timing, config)
	if err != nil {
		f.t.Fatal(err)
	}
	prepared.Job.TravelPause = flagged
	if created, err := f.service.Create(f.t.Context(), id, prepared); err != nil || !created {
		f.t.Fatalf("create %s: %v %v", name, created, err)
	}
	job := prepared.Job
	job.ID = id
	return job
}

func (f *travelFixture) runs(jobID string) int {
	f.t.Helper()
	var count int
	if err := f.db.QueryRowContext(f.t.Context(), `SELECT count(*) FROM schedule_runs WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		f.t.Fatal(err)
	}
	return count
}

func (f *travelFixture) waitRuns(jobID string, want int) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.runs(jobID) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *travelFixture) job(id string) schedule.Job {
	f.t.Helper()
	job, found, err := f.store.LoadScheduledJob(f.t.Context(), "alice", id)
	if err != nil || !found {
		f.t.Fatalf("load %s: %v %v", id, found, err)
	}
	return job
}

// tick moves the clock and waits until the job's next run has moved past the given instant, which
// is how a finished tick shows.
func (f *travelFixture) tick(at time.Time, jobID string, advancedPast time.Time) {
	f.t.Helper()
	f.set(at)
	f.service.Wake()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.job(jobID).NextRunAt.After(advancedPast) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("job %s did not advance past %v", jobID, advancedPast)
}

func TestFlaggedRecurringJobAdvancesSilentlyInsideATripAndFiresAfterIt(t *testing.T) {
	f := newTravelFixture(t)
	daily := f.create("0000000000000001", "Daily digest", schedule.Timing{Cron: "0 9 * * *"}, true)
	f.start()

	f.tick(utc(5, 9, 1), daily.ID, utc(5, 9, 0))
	f.tick(utc(6, 9, 1), daily.ID, utc(6, 9, 0))
	if got := f.job(daily.ID); !got.NextRunAt.Equal(utc(7, 9, 0)) || !got.Enabled || !got.TravelPause {
		t.Fatalf("job inside the trip = %+v", got)
	}
	if runs := f.runs(daily.ID); runs != 0 {
		t.Fatalf("runs queued inside the trip = %d", runs)
	}

	f.tick(utc(7, 9, 1), daily.ID, utc(7, 9, 0))
	if runs := f.runs(daily.ID); runs != 1 {
		t.Fatalf("runs after the trip = %d, want the first occurrence after it", runs)
	}
}

func TestTheOccurrenceDecidesNotTheMomentTheTickRuns(t *testing.T) {
	f := newTravelFixture(t)
	flagged := f.create("0000000000000001", "Flagged", schedule.Timing{Cron: "0 9 * * *"}, true)
	plain := f.create("0000000000000002", "Plain", schedule.Timing{Cron: "0 9 * * *"}, false)
	f.start()
	// The tick runs after the trip has ended, over the 09:00 occurrence inside it.
	f.tick(utc(7, 1, 0), flagged.ID, utc(6, 9, 0))
	f.waitRuns(plain.ID, 1)
	if f.runs(flagged.ID) != 0 || f.runs(plain.ID) != 1 {
		t.Fatalf("runs: flagged %d, unflagged %d", f.runs(flagged.ID), f.runs(plain.ID))
	}
	if !slices.ContainsFunc(f.trip.asked, func(at time.Time) bool { return at.Equal(utc(6, 9, 0)) }) {
		t.Fatalf("the trip was asked about %v, want the occurrence", f.trip.asked)
	}
}

func TestOneTimeJobsRunNowAndAFailedLookupAreNotGated(t *testing.T) {
	f := newTravelFixture(t)
	once := f.create("0000000000000001", "Once", schedule.Timing{At: "2026-10-05 12:00"}, true)
	flagged := f.create("0000000000000002", "Flagged", schedule.Timing{Cron: "0 9 * * *"}, true)
	failing := f.create("0000000000000003", "Failing lookup", schedule.Timing{Cron: "0 14 * * *"}, true)
	f.start()

	if queued, err := f.service.RunNow(t.Context(), "alice", flagged.ID, "press"); err != nil || !queued {
		t.Fatalf("run now = %v %v", queued, err)
	}
	if runs := f.runs(flagged.ID); runs != 1 {
		t.Fatalf("run now inside a trip queued %d runs", runs)
	}

	f.set(utc(5, 12, 1))
	f.service.Wake()
	f.waitRuns(once.ID, 1)
	f.trip.mu.Lock()
	f.trip.failure = errors.New("trips are unavailable")
	f.trip.mu.Unlock()
	f.tick(utc(5, 14, 1), failing.ID, utc(5, 14, 0))
	f.waitRuns(failing.ID, 1)
	if f.runs(once.ID) != 1 {
		t.Fatalf("a one-time job inside a trip queued %d runs", f.runs(once.ID))
	}
	if f.runs(failing.ID) != 1 {
		t.Fatalf("a job whose trip lookup failed queued %d runs, want it to run", f.runs(failing.ID))
	}
}

func TestScheduleToolsSetAndKeepTheTravelFlag(t *testing.T) {
	f := newTravelFixture(t)
	call := func(name, arguments string) tools.Result {
		t.Helper()
		result, err := f.service.Execute(t.Context(), "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	confirm := func(id string) {
		t.Helper()
		proposal := f.proposer.proposals[len(f.proposer.proposals)-1]
		if _, err := f.service.ExecuteApproved(t.Context(), approval.Record{ID: id, PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments}); err != nil {
			t.Fatal(err)
		}
	}

	if result := call("schedule_propose", `{"name":"Bins","cron":"0 18 * * 0","prompt":"Remind me about the bins.","pause_while_traveling":true}`); result.IsError {
		t.Fatalf("propose = %+v", result)
	}
	if !strings.Contains(f.proposer.proposals[0].Summary, "Pauses while you are traveling") {
		t.Fatalf("summary = %q", f.proposer.proposals[0].Summary)
	}
	confirm("0000000000000001")
	if !f.job("0000000000000001").TravelPause {
		t.Fatal("the confirmed job lost the flag")
	}

	if result := call("schedule_update", `{"job_id":"0000000000000001","name":"Bins out"}`); result.IsError {
		t.Fatalf("rename = %+v", result)
	}
	confirm("0000000000000001")
	if got := f.job("0000000000000001"); got.Name != "Bins out" || !got.TravelPause {
		t.Fatalf("job after a rename = %+v", got)
	}
	if listed := call("schedule_list", `{}`); !strings.Contains(listed.Text, "pauses while traveling") {
		t.Fatalf("list = %q", listed.Text)
	}
	if names, err := f.service.TravelPausedNames(t.Context(), "alice"); err != nil || !slices.Equal(names, []string{"Bins out"}) {
		t.Fatalf("paused names = %v %v", names, err)
	}

	if result := call("schedule_update", `{"job_id":"0000000000000001","pause_while_traveling":false}`); result.IsError {
		t.Fatalf("clear = %+v", result)
	}
	confirm("0000000000000001")
	if f.job("0000000000000001").TravelPause {
		t.Fatal("pause_while_traveling false did not clear the flag")
	}

	if result := call("schedule_propose", `{"name":"Once","at":"2026-10-06 09:00","prompt":"Check the train.","pause_while_traveling":true}`); !result.IsError {
		t.Fatalf("a one-time job was allowed to pause while traveling: %+v", result)
	}
	if changed, err := f.service.SetTravelPause(t.Context(), "alice", "0000000000000001", true); err != nil || !changed || !f.job("0000000000000001").TravelPause {
		t.Fatalf("toggle = %v %v", changed, err)
	}
}

func TestHeadsUpUsesTheNewScheduleAndOldOnesMoveOnce(t *testing.T) {
	f := newTravelFixture(t)
	if schedule.HeadsUpCron != "30 7,18 * * *" {
		t.Fatalf("heads-up cron = %q", schedule.HeadsUpCron)
	}
	id := schedule.HeadsUpJobID("alice")
	old := f.create(id, schedule.HeadsUpName, schedule.Timing{Cron: "0 8,19 * * *"}, false)
	other := f.create("0000000000000001", schedule.HeadsUpName, schedule.Timing{Cron: "0 8,19 * * *"}, false)

	f.set(utc(5, 9, 0))
	f.start()
	got := f.job(id)
	if got.Cron != schedule.HeadsUpCron || !got.NextRunAt.Equal(utc(5, 18, 30)) {
		t.Fatalf("migrated heads-up = %s next %v", got.Cron, got.NextRunAt)
	}
	if got.UpdatedAt.Equal(old.UpdatedAt) {
		t.Fatal("the migrated job was not written")
	}
	if untouched := f.job(other.ID); untouched.Cron != "0 8,19 * * *" {
		t.Fatalf("a job that is not the built-in Heads-up was moved: %s", untouched.Cron)
	}

	if err := f.service.MigrateHeadsUp(t.Context()); err != nil {
		t.Fatal(err)
	}
	if again := f.job(id); !again.UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatal("a second migration changed the job again")
	}
}

func TestHeadsUpTheUserChangedIsLeftAlone(t *testing.T) {
	f := newTravelFixture(t)
	id := schedule.HeadsUpJobID("alice")
	changed := f.create(id, schedule.HeadsUpName, schedule.Timing{Cron: "0 9 * * *"}, false)
	f.start()
	if got := f.job(id); got.Cron != "0 9 * * *" || !got.UpdatedAt.Equal(changed.UpdatedAt) {
		t.Fatalf("a changed heads-up = %+v", got)
	}
}

func TestAFlagToggledAfterAProposalSurvivesConfirmAndUndo(t *testing.T) {
	f := newTravelFixture(t)
	job := f.create("0000000000000001", "Bins", schedule.Timing{Cron: "0 18 * * 0"}, true)
	call := func(arguments string) {
		t.Helper()
		result, err := f.service.Execute(t.Context(), "alice", tools.Call{Name: "schedule_update", Arguments: json.RawMessage(arguments)})
		if err != nil || result.IsError {
			t.Fatalf("schedule_update = %+v %v", result, err)
		}
	}
	confirm := func() approval.Outcome {
		t.Helper()
		proposal := f.proposer.proposals[len(f.proposer.proposals)-1]
		outcome, err := f.service.ExecuteApproved(t.Context(), approval.Record{ID: "0000000000000009", PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}

	call(`{"job_id":"` + job.ID + `","name":"Bins out"}`)
	if changed, err := f.service.SetTravelPause(t.Context(), "alice", job.ID, false); err != nil || !changed {
		t.Fatalf("toggle = %v %v", changed, err)
	}
	outcome := confirm()
	if got := f.job(job.ID); got.Name != "Bins out" || got.TravelPause {
		t.Fatalf("job after Confirm = %+v, want the flag the user set in /jobs", got)
	}

	if changed, err := f.service.SetTravelPause(t.Context(), "alice", job.ID, true); err != nil || !changed {
		t.Fatalf("toggle = %v %v", changed, err)
	}
	if _, err := f.service.Undo(t.Context(), approval.Record{PrincipalID: "alice", Tool: "schedule_update"}, outcome.Undo); err != nil {
		t.Fatal(err)
	}
	if got := f.job(job.ID); got.Name != "Bins" || !got.TravelPause {
		t.Fatalf("job after Undo = %+v, want the old name and the flag toggled since", got)
	}

	call(`{"job_id":"` + job.ID + `","pause_while_traveling":false}`)
	outcome = confirm()
	if f.job(job.ID).TravelPause {
		t.Fatal("the update did not clear the flag")
	}
	if _, err := f.service.Undo(t.Context(), approval.Record{PrincipalID: "alice", Tool: "schedule_update"}, outcome.Undo); err != nil {
		t.Fatal(err)
	}
	if !f.job(job.ID).TravelPause {
		t.Fatal("Undo did not restore the flag its update changed")
	}
}

func TestBuiltInAndOneTimeJobsCannotPauseWhileTraveling(t *testing.T) {
	f := newTravelFixture(t)
	headsUp := f.create(schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, schedule.Timing{Cron: schedule.HeadsUpCron}, false)
	once := f.create("0000000000000001", "Once", schedule.Timing{At: "2026-10-06 09:00"}, false)

	result, err := f.service.Execute(t.Context(), "alice", tools.Call{Name: "schedule_update", Arguments: json.RawMessage(`{"job_id":"` + headsUp.ID + `","pause_while_traveling":true}`)})
	if err != nil || !result.IsError {
		t.Fatalf("flagging Heads-up through the tool = %+v %v", result, err)
	}
	for _, id := range []string{headsUp.ID, once.ID} {
		if changed, err := f.service.SetTravelPause(t.Context(), "alice", id, true); err != nil || changed {
			t.Fatalf("flagging %s = %v %v", id, changed, err)
		}
	}
}

func TestARenamedHeadsUpOnTheOldScheduleIsMoved(t *testing.T) {
	f := newTravelFixture(t)
	id := schedule.HeadsUpJobID("alice")
	f.create(id, "Morning brief", schedule.Timing{Cron: "0 8,19 * * *"}, false)
	f.set(utc(5, 9, 0))
	f.start()
	if got := f.job(id); got.Cron != schedule.HeadsUpCron || got.Name != "Morning brief" {
		t.Fatalf("renamed heads-up = %+v", got)
	}
}

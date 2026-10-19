package telegram

import (
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// readOutside makes the chat turn read outside content, after which its conversation's writes are
// proposed instead of made.
func readOutside(t *testing.T, chat runtimeCall) {
	t.Helper()
	if result := toolCaller(t, chat)("read_outside", `{}`); result.IsError {
		t.Fatalf("read_outside = %+v", result)
	}
}

// runExposedTool runs one tool in a chat turn of the given user that read outside content first.
func (h *conversationHarness) runExposedTool(runtime *controlledRuntime, updateID, userID int64, name, arguments string) agentruntime.ToolResult {
	h.t.Helper()
	h.push(textUpdate(updateID, userID, "about my routes"))
	call := receive(h.t, h.ctx, runtime.calls)
	readOutside(h.t, call)
	result := toolCaller(h.t, call)(name, arguments)
	call.answer <- agentruntime.RunResult{Response: "done"}
	h.wantReply(userID, updateID, "done")
	h.waitState(updateID, "delivered")
	return result
}

// ownDataTurn runs one of alice's chat turns that calls the tool, reading outside content first when
// exposed, and waits for the reply and the message that follows it: the "Done" message of a direct
// action, recorded so an Undo edits it, or the proposal's bundle.
func (h *conversationHarness) ownDataTurn(runtime *controlledRuntime, updateID int64, exposed bool, name, arguments string) agentruntime.ToolResult {
	h.t.Helper()
	h.push(textUpdate(updateID, 101, "change my things"))
	call := receive(h.t, h.ctx, runtime.calls)
	if exposed {
		readOutside(h.t, call)
	}
	result := toolCaller(h.t, call)(name, arguments)
	h.finishTurn(call, updateID, "ok")
	if result.IsError {
		return result
	}
	following := h.nextMessage()
	h.waitMessageRecorded(following.messageID)
	return result
}

// wantDirect asserts that a tool made its change at once, and returns its approval record's ID.
func (h *conversationHarness) wantDirect(result agentruntime.ToolResult, tool string) string {
	h.t.Helper()
	if result.IsError || !strings.Contains(result.Text, "\nDone: the change is made") || !strings.Contains(result.Text, "Undo button") {
		h.t.Fatalf("%s = %+v, want it done at once with an Undo", tool, result)
	}
	var id, status, mode string
	if err := h.db.QueryRowContext(h.ctx,
		`SELECT id, status, mode FROM approvals WHERE principal_id = 'alice' AND tool = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, tool,
	).Scan(&id, &status, &mode); err != nil {
		h.t.Fatal(err)
	}
	if status != approval.StatusDone || mode != approval.ModeDirect {
		h.t.Fatalf("%s record = %s %s, want a done direct action", tool, status, mode)
	}
	return id
}

// undoDirect presses Undo on alice's direct action and waits for its "Done" message to be edited.
func (h *conversationHarness) undoDirect(id string) string {
	h.t.Helper()
	undid, err := h.approvals.Undo(h.ctx, "alice", id)
	if err != nil || !undid.Owned {
		h.t.Fatalf("undo = %+v %v", undid, err)
	}
	h.nextEdit()
	return undid.Answer
}

func (h *conversationHarness) approvalRecordStatus(id string) (string, string) {
	h.t.Helper()
	var status, reason string
	if err := h.db.QueryRowContext(h.ctx, `SELECT status, COALESCE(reason, '') FROM approvals WHERE id = ?`, id).Scan(&status, &reason); err != nil {
		h.t.Fatal(err)
	}
	return status, reason
}

// wantProposedOnly asserts that a tool proposed its change and wrote nothing.
func (h *conversationHarness) wantProposedOnly(result agentruntime.ToolResult, tool string) {
	h.t.Helper()
	if result.IsError || !strings.HasSuffix(result.Text, approval.ProposedText) {
		h.t.Fatalf("%s after outside content = %+v, want a proposal", tool, result)
	}
	if count := h.countRows(`SELECT count(*) FROM approvals WHERE tool = ? AND mode = 'proposed' AND status = 'pending'`, tool); count != 1 {
		h.t.Fatalf("%s pending proposals = %d, want 1", tool, count)
	}
}

func (h *conversationHarness) storedJob(id string) (schedule.Job, bool) {
	h.t.Helper()
	job, found, err := h.store.LoadScheduledJob(h.ctx, "alice", id)
	if err != nil {
		h.t.Fatal(err)
	}
	return job, found
}

// Jobs: a clean turn schedules, changes, and deletes at once, and Undo reverses each, even after a
// run moved the job; a job changed since and a one-time job whose time passed are refused. After
// outside content the same call is only proposed.
func TestJobsAreChangedAtOnceAndUndone(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()

	created := h.wantDirect(h.ownDataTurn(runtime, 1, false, "schedule_propose", `{"name":"Weekly tasks","cron":"0 9 * * 1","prompt":"List my overdue Notion tasks."}`), "schedule_propose")
	if job, found := h.storedJob(created); !found || job.Name != "Weekly tasks" || job.Cron != "0 9 * * 1" {
		t.Fatalf("created job = %+v %v, want it stored under the record's id", job, found)
	}
	if answer := h.undoDirect(created); answer != "Undone." {
		t.Fatalf("undo of a created job = %q", answer)
	}
	if _, found := h.storedJob(created); found {
		t.Fatal("the undone job is still there")
	}
	if status, _ := h.approvalRecordStatus(created); status != approval.StatusUndone {
		t.Fatalf("record after undo = %s", status)
	}

	job := h.createJob("alice", "a1b2c3d4e5f60718", "Daily digest", "30 7 * * *", "Summarise my day.")
	updated := h.wantDirect(h.ownDataTurn(runtime, 2, false, "schedule_update", `{"job_id":"`+job.ID+`","name":"Morning digest","cron":"0 8 * * *"}`), "schedule_update")
	if changed, _ := h.storedJob(job.ID); changed.Name != "Morning digest" || changed.Cron != "0 8 * * *" {
		t.Fatalf("changed job = %+v", changed)
	}
	// A run moves the job's next run and its change time; that is no change the user made.
	if _, err := h.db.ExecContext(h.ctx, `UPDATE scheduled_jobs SET updated_at = '2026-09-28T06:30:00.000Z' WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if answer := h.undoDirect(updated); answer != "Undone." {
		t.Fatalf("undo of a change = %q", answer)
	}
	if restored, _ := h.storedJob(job.ID); restored.Name != "Daily digest" || restored.Cron != "30 7 * * *" ||
		schedule.ModelTurnPrompt(restored.Config) != "Summarise my day." || !restored.Enabled {
		t.Fatalf("restored job = %+v", restored)
	}

	stale := h.wantDirect(h.ownDataTurn(runtime, 3, false, "schedule_update", `{"job_id":"`+job.ID+`","name":"Evening digest"}`), "schedule_update")
	if changed, err := h.scheduler.SetEnabled(h.ctx, "alice", job.ID, false); err != nil || !changed {
		t.Fatalf("pause = %v %v", changed, err)
	}
	if answer := h.undoDirect(stale); !strings.HasPrefix(answer, "Not undone: the job was changed since") {
		t.Fatalf("undo of a job changed since = %q", answer)
	}
	if status, reason := h.approvalRecordStatus(stale); status != approval.StatusDone || reason != approval.ReasonUndoRefused {
		t.Fatalf("refused record = %s %s", status, reason)
	}
	if kept, _ := h.storedJob(job.ID); kept.Name != "Evening digest" {
		t.Fatalf("a refused undo changed the job: %+v", kept)
	}

	removed := h.wantDirect(h.ownDataTurn(runtime, 4, false, "schedule_remove", `{"job_id":"`+job.ID+`"}`), "schedule_remove")
	if _, found := h.storedJob(job.ID); found {
		t.Fatal("the removed job is still there")
	}
	if answer := h.undoDirect(removed); answer != "Undone." {
		t.Fatalf("undo of a removal = %q", answer)
	}
	back, found := h.storedJob(job.ID)
	if !found || back.Name != "Evening digest" || back.Cron != "30 7 * * *" || back.Enabled || !back.NextRunAt.After(clock.Now()) {
		t.Fatalf("recreated job = %+v %v, want it back under its id, still paused, with a next run", back, found)
	}

	once := h.wantDirect(h.ownDataTurn(runtime, 5, false, "schedule_propose", `{"name":"Train check","at":"2026-09-28 09:00","prompt":"Is my 9:30 train delayed?"}`), "schedule_propose")
	gone := h.wantDirect(h.ownDataTurn(runtime, 6, false, "schedule_remove", `{"job_id":"`+once+`"}`), "schedule_remove")
	clock.Set(t, "2026-09-28T09:30:00+02:00")
	if answer := h.undoDirect(gone); !strings.Contains(answer, "has passed, so it cannot be restored") {
		t.Fatalf("undo of a passed one-time job's removal = %q", answer)
	}
	if _, found := h.storedJob(once); found {
		t.Fatal("a passed one-time job came back")
	}

	before := h.countRows(`SELECT count(*) FROM scheduled_jobs`)
	h.wantProposedOnly(h.ownDataTurn(runtime, 7, true, "schedule_propose", `{"name":"Again","cron":"0 10 * * 1","prompt":"Summarise the week."}`), "schedule_propose")
	if after := h.countRows(`SELECT count(*) FROM scheduled_jobs`); after != before {
		t.Fatalf("a proposal changed the jobs: %d to %d", before, after)
	}
}

// Watches: a clean turn sets up, changes, and removes at once, and Undo reverses each; a removed
// watch comes back with its pause. A watch changed since is refused, and after outside content the
// same call is only proposed.
func TestWatchesAreChangedAtOnceAndUndone(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, watches: &stubEvaluator{}, providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()
	watches := func() int { return h.countRows(`SELECT count(*) FROM watches WHERE principal_id = 'alice'`) }
	window := func(id string) string {
		t.Helper()
		var config string
		if err := h.db.QueryRowContext(h.ctx, `SELECT config FROM watches WHERE id = ?`, id).Scan(&config); err != nil {
			t.Fatal(err)
		}
		return config
	}

	created := h.wantDirect(h.ownDataTurn(runtime, 1, false, "watch_propose",
		`{"kind":"departure","mode":"train","from":"Utrecht Centraal","to":"Amsterdam Centraal","departure":"08:30","days":["mon","tue","wed","thu","fri"]}`), "watch_propose")
	if watches() != 1 || h.countRows(`SELECT count(*) FROM watches WHERE id = ?`, created) != 1 {
		t.Fatalf("watches after a direct watch_propose = %d", watches())
	}
	if answer := h.undoDirect(created); answer != "Undone." || watches() != 0 {
		t.Fatalf("undo of a created watch = %q, %d left", answer, watches())
	}

	h.createWatch("alice", "b1b2c3d4e5f60718", trainWatch())
	updated := h.wantDirect(h.ownDataTurn(runtime, 2, false, "watch_update", `{"watch_id":"b1b2c3d4e5f60718","departure":"09:00","minutes_before":30}`), "watch_update")
	if config := window("b1b2c3d4e5f60718"); !strings.Contains(config, `"start":"08:30","end":"09:00"`) {
		t.Fatalf("changed watch = %s", config)
	}
	if answer := h.undoDirect(updated); answer != "Undone." {
		t.Fatalf("undo of a change = %q", answer)
	}
	if config := window("b1b2c3d4e5f60718"); !strings.Contains(config, `"start":"07:00","end":"08:30"`) {
		t.Fatalf("restored watch = %s", config)
	}

	stale := h.wantDirect(h.ownDataTurn(runtime, 3, false, "watch_update", `{"watch_id":"b1b2c3d4e5f60718","delay_minutes":10}`), "watch_update")
	clock.Set(t, "2026-09-28T08:05:00+02:00")
	if paused, err := h.watches.SetEnabled(h.ctx, "alice", "b1b2c3d4e5f60718", false); err != nil || !paused {
		t.Fatalf("pause = %v %v", paused, err)
	}
	if answer := h.undoDirect(stale); !strings.HasPrefix(answer, "Not undone: the watch was changed since") {
		t.Fatalf("undo of a watch changed since = %q", answer)
	}
	if config := window("b1b2c3d4e5f60718"); !strings.Contains(config, `"delay_minutes":10`) {
		t.Fatalf("a refused undo changed the watch: %s", config)
	}

	removed := h.wantDirect(h.ownDataTurn(runtime, 4, false, "watch_remove", `{"watch_id":"b1b2c3d4e5f60718"}`), "watch_remove")
	if watches() != 0 {
		t.Fatal("the removed watch is still there")
	}
	if answer := h.undoDirect(removed); answer != "Undone." {
		t.Fatalf("undo of a removal = %q", answer)
	}
	if enabled, _ := h.watchState("b1b2c3d4e5f60718"); enabled || !strings.Contains(window("b1b2c3d4e5f60718"), `"delay_minutes":10`) {
		t.Fatalf("recreated watch enabled = %v, want it back paused as it was", enabled)
	}

	h.wantProposedOnly(h.ownDataTurn(runtime, 5, true, "watch_remove", `{"watch_id":"b1b2c3d4e5f60718"}`), "watch_remove")
	if watches() != 1 {
		t.Fatal("a proposal removed the watch")
	}
}

// Routes: a clean turn saves, changes, and deletes at once, and Undo reverses each; a route
// changed since is refused, and after outside content the same call is only proposed.
func TestRoutesAreChangedAtOnceAndUndone(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: newTestClock(t, mondayMorning), ns: stationsGateway(t), providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()
	legs := func() string {
		t.Helper()
		var stored string
		if err := h.db.QueryRowContext(h.ctx, `SELECT legs FROM routes WHERE principal_id = 'alice'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		return stored
	}
	routeCount := func() int { return h.countRows(`SELECT count(*) FROM routes`) }
	save := `{"name":"work","days":["mon","tue","wed","thu","fri"],"legs":[{"mode":"bike","minutes":6},{"mode":"train","from":"amfs","to":"Utrecht","departures":["07:56"]}]}`

	saved := h.wantDirect(h.ownDataTurn(runtime, 1, false, "route_save", save), "route_save")
	if routeCount() != 1 || h.countRows(`SELECT count(*) FROM routes WHERE id = ?`, saved) != 1 {
		t.Fatalf("routes after a direct route_save = %d", routeCount())
	}
	if answer := h.undoDirect(saved); answer != "Undone." || routeCount() != 0 {
		t.Fatalf("undo of a save = %q, %d left", answer, routeCount())
	}

	resaved := h.wantDirect(h.ownDataTurn(runtime, 2, false, "route_save", save), "route_save")
	original := legs()
	updated := h.wantDirect(h.ownDataTurn(runtime, 3, false, "route_update", `{"name":"work","leg_index":1,"leg":{"minutes":8}}`), "route_update")
	if changed := legs(); !strings.Contains(changed, `{"mode":"bike","minutes":8}`) {
		t.Fatalf("changed legs = %s", changed)
	}
	if answer := h.undoDirect(updated); answer != "Undone." || legs() != original {
		t.Fatalf("undo of a change = %q, legs %s, want %s", answer, legs(), original)
	}

	stale := h.wantDirect(h.ownDataTurn(runtime, 4, false, "route_update", `{"name":"work","arrive_by":"08:45"}`), "route_update")
	if _, err := h.db.ExecContext(h.ctx, `UPDATE routes SET days = '["mon"]', updated_at = '2026-09-28T06:30:00.000Z' WHERE id = ?`, resaved); err != nil {
		t.Fatal(err)
	}
	if answer := h.undoDirect(stale); !strings.HasPrefix(answer, "Not undone: the route was changed since") {
		t.Fatalf("undo of a route changed since = %q", answer)
	}

	removed := h.wantDirect(h.ownDataTurn(runtime, 5, false, "route_remove", `{"name":"work"}`), "route_remove")
	if routeCount() != 0 {
		t.Fatal("the removed route is still there")
	}
	if answer := h.undoDirect(removed); answer != "Undone." {
		t.Fatalf("undo of a removal = %q", answer)
	}
	if h.countRows(`SELECT count(*) FROM routes WHERE id = ? AND arrive_by = '08:45' AND days = '["mon"]'`, resaved) != 1 || legs() != original {
		t.Fatalf("recreated route legs = %s, want it back under its id as it was", legs())
	}

	h.wantProposedOnly(h.ownDataTurn(runtime, 6, true, "route_remove", `{"name":"work"}`), "route_remove")
	if routeCount() != 1 {
		t.Fatal("a proposal removed the route")
	}
}

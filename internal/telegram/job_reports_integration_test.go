package telegram

import (
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/turn"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

func (h *conversationHarness) reportID(text string) string {
	h.t.Helper()
	var id string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM job_reports WHERE text = ?`, text).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func replyTo(update Update, messageID int64) Update {
	update.Message.ReplyToMessage = &Message{MessageID: messageID}
	return update
}

// A delivered report is kept: the next chat turn is told of it once, by excerpt, even after a
// restart; a reply to its message quotes it in full; job_reports reads it; and a new conversation
// starts with the last day's reports. Nothing of it reaches another user.
func TestJobReportsReachLaterTurns(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	h.push(textUpdate(1, 101, "hello"))
	opening := receive(t, h.ctx, runtime.calls)
	if strings.Contains(opening.request.Message, "Reports the user's scheduled jobs") || slices.Contains(toolNames(opening.request.Tools), "job_reports") {
		t.Fatalf("a turn without reports was told of them: %q %v", opening.request.Message, toolNames(opening.request.Tools))
	}
	opening.answer <- agentruntime.RunResult{Response: "hi"}
	h.wantReply(101, 1, "hi")
	h.waitState(1, "delivered")

	job := h.createJob("alice", "a1b2c3d4e5f60718", "Commute", "0 9 * * *", "Check my trains.")
	long := "The 08:12 train is on time. " + strings.Repeat("Platform details follow. ", 12) + "The second train is cancelled."
	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	tools.RunFrom(run.ctx).MarkExternal("")
	run.answer <- agentruntime.RunResult{Response: long, Final: long}
	first := h.nextMessage()
	h.waitRun(job.ID, schedule.StatusDelivered)
	firstID := h.reportID(long)

	// A new scheduler over the same database stands for a restart: the report is still unheard of.
	restarted, err := schedule.NewService(schedule.Config{Zone: "Europe/Amsterdam", Now: clock.Now}, h.store, h.store, h.client, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	notice, outside, private := restarted.TurnReports(h.ctx, "alice", conversation.ReportTurn{})
	if !outside || private || !strings.Contains(notice, `job "Commute" (job id a1b2c3d4e5f60718), Mon 28 Sep 09:00 CEST, report id `+firstID+": The 08:12 train is on time.") ||
		!strings.HasSuffix(notice, "…") || strings.Contains(notice, "second train") {
		t.Fatalf("notice after restart = %q private=%v", notice, private)
	}

	if queued, err := h.scheduler.RunNow(h.ctx, "alice", job.ID, ""); err != nil || !queued {
		t.Fatalf("run now = %v %v", queued, err)
	}
	h.scheduler.Wake()
	run = receive(t, h.ctx, runtime.calls)
	run.answer <- agentruntime.RunResult{Response: "All trains run.", Final: "All trains run."}
	second := h.nextMessage()
	if second.text != "<b>Scheduled: Commute</b>\n\nAll trains run." {
		t.Fatalf("second report = %+v", second)
	}
	secondID := ""
	for secondID == "" {
		secondID = h.optionalReportID("All trains run.")
	}

	h.push(textUpdate(2, 101, "and now?"))
	next := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(next.request.Message, "report id "+secondID+": All trains run.") || strings.Contains(next.request.Message, firstID) {
		t.Fatalf("next turn message = %q", next.request.Message)
	}
	if !slices.Contains(toolNames(next.request.Tools), "job_reports") {
		t.Fatalf("chat tools = %v", toolNames(next.request.Tools))
	}
	next.answer <- agentruntime.RunResult{Response: "fine"}
	h.wantReply(101, 2, "fine")
	h.waitState(2, "delivered")

	h.push(textUpdate(3, 101, "thanks"))
	after := receive(t, h.ctx, runtime.calls)
	if strings.Contains(after.request.Message, "report id") {
		t.Fatalf("a report was told twice: %q", after.request.Message)
	}
	after.answer <- agentruntime.RunResult{Response: "welcome"}
	h.wantReply(101, 3, "welcome")
	h.waitState(3, "delivered")

	h.push(replyTo(textUpdate(4, 101, "what about the second train?"), first.messageID))
	reply := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(reply.request.Message, "The user is replying to this report (report id "+firstID+`) from job "Commute"`) ||
		!strings.Contains(reply.request.Message, "The second train is cancelled.") {
		t.Fatalf("reply turn message = %q", reply.request.Message)
	}
	listed := h.callTool(reply, "job_reports", `{"job":"commute"}`)
	if listed.IsError || !strings.Contains(listed.Text, "report id "+firstID) || !strings.Contains(listed.Text, "report id "+secondID) {
		t.Fatalf("job_reports list = %+v", listed)
	}
	full := h.callTool(reply, "job_reports", `{"report_id":"`+firstID+`"}`)
	if full.IsError || !strings.Contains(full.Text, long) {
		t.Fatalf("job_reports read = %+v", full)
	}
	reply.answer <- agentruntime.RunResult{Response: "It is cancelled."}
	h.wantReply(101, 4, "It is cancelled.")
	h.waitState(4, "delivered")
	if count := h.countRows(`SELECT count(*) FROM conversation_sessions WHERE principal_id = 'alice' AND external_at IS NOT NULL`); count != 1 {
		t.Fatalf("external sessions = %d, want 1", count)
	}

	h.push(textUpdate(5, 101, "/new"))
	h.wantReply(101, 5, localetest.English(NewSessionReply))
	h.push(textUpdate(6, 101, "the report this morning"))
	fresh := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(fresh.request.Message, "report id "+firstID) || !strings.Contains(fresh.request.Message, "report id "+secondID) ||
		strings.Contains(fresh.request.Message, "All trains run.") {
		t.Fatalf("new conversation message = %q", fresh.request.Message)
	}
	fresh.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 6, "ok")
	h.waitState(6, "delivered")
	// The digest names reports without their text, so the new conversation has read nothing from
	// outside.
	if count := h.countRows(`SELECT count(*) FROM conversation_sessions WHERE principal_id = 'alice' AND external_at IS NOT NULL`); count != 1 {
		t.Fatalf("external sessions after the digest = %d, want 1", count)
	}

	h.push(replyTo(textUpdate(7, 202, "what is this?"), first.messageID))
	foreign := receive(t, h.ctx, runtime.calls)
	if strings.Contains(foreign.request.Message, "report") || slices.Contains(toolNames(foreign.request.Tools), "job_reports") {
		t.Fatalf("another user's turn saw the report: %q %v", foreign.request.Message, toolNames(foreign.request.Tools))
	}
	if result, err := h.scheduler.Execute(h.ctx, "bob", tools.Call{Name: "job_reports", Arguments: json.RawMessage(`{"report_id":"` + firstID + `"}`)}); err != nil || !result.IsError || strings.Contains(result.Text, "train") {
		t.Fatalf("bob read alice's report: %+v %v", result, err)
	}
	foreign.answer <- agentruntime.RunResult{Response: "no idea"}
	h.wantReply(202, 7, "no idea")
}

// A report marks the conversation it is told in as having read outside content only when its run
// read some: a run that only read household data leaves the conversation clean, a run that read a
// web page marks it.
func TestJobReportMarksConversationOnlyWhenItsRunWasExternal(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()

	job := h.createJob("alice", "a1b2c3d4e5f60718", "Commute", "0 9 * * *", "Check my trains.")
	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	run.answer <- agentruntime.RunResult{Response: "Trains run.", Final: "Trains run."}
	h.nextMessage()
	h.waitRun(job.ID, schedule.StatusDelivered)
	if got := h.countRows(`SELECT external FROM job_reports WHERE text = 'Trains run.'`); got != 0 {
		t.Fatalf("clean report external = %d, want 0", got)
	}

	h.push(textUpdate(1, 101, "and now?"))
	clean := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(clean.request.Message, "Trains run.") {
		t.Fatalf("clean turn message = %q", clean.request.Message)
	}
	clean.answer <- agentruntime.RunResult{Response: "fine"}
	h.wantReply(101, 1, "fine")
	h.waitState(1, "delivered")
	if count := h.countRows(`SELECT count(*) FROM conversation_sessions WHERE principal_id = 'alice' AND external_at IS NOT NULL`); count != 0 {
		t.Fatalf("external sessions after a household-only report = %d, want 0", count)
	}

	if queued, err := h.scheduler.RunNow(h.ctx, "alice", job.ID, ""); err != nil || !queued {
		t.Fatalf("run now = %v %v", queued, err)
	}
	h.scheduler.Wake()
	run = receive(t, h.ctx, runtime.calls)
	tools.RunFrom(run.ctx).MarkExternal("")
	run.answer <- agentruntime.RunResult{Response: "Strike announced.", Final: "Strike announced."}
	h.nextMessage()
	for h.optionalReportID("Strike announced.") == "" {
	}
	if got := h.countRows(`SELECT external FROM job_reports WHERE text = 'Strike announced.'`); got != 1 {
		t.Fatalf("web report external = %d, want 1", got)
	}

	h.push(textUpdate(2, 101, "anything new?"))
	marked := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(marked.request.Message, "Strike announced.") {
		t.Fatalf("external turn message = %q", marked.request.Message)
	}
	marked.answer <- agentruntime.RunResult{Response: "yes"}
	h.wantReply(101, 2, "yes")
	h.waitState(2, "delivered")
	if count := h.countRows(`SELECT count(*) FROM conversation_sessions WHERE principal_id = 'alice' AND external_at IS NOT NULL`); count != 1 {
		t.Fatalf("external sessions after a web report = %d, want 1", count)
	}
}

// A reminder's message is kept as its report too, so a reply to it is understood.
func TestReplyToReminderQuotesIt(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()
	h.createReminder("alice", "b1b2c3d4e5f60718", "Call the plumber", "2026-09-28 17:00")

	clock.Set(t, "2026-09-28T17:00:20+02:00")
	h.scheduler.Wake()
	reminder := h.nextMessage()
	if !strings.Contains(reminder.text, "Call the plumber") || len(reminder.keyboard) == 0 {
		t.Fatalf("reminder = %+v", reminder)
	}
	h.waitRun("b1b2c3d4e5f60718", schedule.StatusDelivered)

	h.push(replyTo(textUpdate(1, 101, "move it to tomorrow"), reminder.messageID))
	turn := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(turn.request.Message, `The user is replying to this report (report id `) ||
		!strings.Contains(turn.request.Message, `from reminder "Call the plumber" (job id b1b2c3d4e5f60718)`) {
		t.Fatalf("reply turn message = %q", turn.request.Message)
	}
	turn.answer <- agentruntime.RunResult{Response: "moved"}
	h.wantReply(101, 1, "moved")
}

func (h *conversationHarness) optionalReportID(text string) string {
	h.t.Helper()
	var id string
	_ = h.db.QueryRowContext(h.ctx, `SELECT id FROM job_reports WHERE text = ?`, text).Scan(&id)
	return id
}

// Chat turns carry the user's own jobs, with the start of each prompt, in the system prompt; a
// change to a job changes it, and a scheduled turn, which has its own prompt, goes without.
func TestChatTurnsKnowTheUsersJobs(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	delays := strings.Repeat("Mention delays over five minutes. ", 12)
	prompt := "Check the 08:12 from Central to Riverside and the bus from Riverside to the office. " + delays + "Come home on the 17:10 from Riverside. " + delays + "Skip the weather."
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Commute", "0 9 * * 1-5", prompt)
	h.createJob("bob", "c1b2c3d4e5f60718", "Garden", "0 18 * * *", "Check the soil moisture.")
	clock.Set(t, "2026-09-28T08:00:05+02:00")
	h.createReminder("alice", "b1b2c3d4e5f60718", "Call the plumber", "2026-09-29 17:00")

	h.push(textUpdate(1, 101, "is my train on time?"))
	chat := receive(t, h.ctx, runtime.calls)
	jobs := chat.request.Jobs
	for _, want := range []string{
		`id a1b2c3d4e5f60718: "Commute", 0 9 * * 1-5 (Europe/Amsterdam): Check the 08:12 from Central to Riverside`,
		"Come home on the 17:10 from Riverside.",
		"id b1b2c3d4e5f60718: reminder, once, Tue 29 Sep 17:00 CEST: Call the plumber",
		"read the job that covers them with schedule_get",
	} {
		if !strings.Contains(jobs, want) {
			t.Fatalf("jobs section lacks %q: %q", want, jobs)
		}
	}
	if strings.Contains(jobs, "Skip the weather.") || !strings.Contains(jobs, "…") || strings.Contains(jobs, "Garden") {
		t.Fatalf("jobs section = %q", jobs)
	}
	if strings.Index(jobs, "b1b2c3d4e5f60718") > strings.Index(jobs, "a1b2c3d4e5f60718") {
		t.Fatalf("the newer reminder is not listed first: %q", jobs)
	}
	chat.answer <- agentruntime.RunResult{Response: "yes"}
	h.wantReply(101, 1, "yes")
	h.waitState(1, "delivered")

	if changed, err := h.scheduler.SetEnabled(h.ctx, "alice", job.ID, false); err != nil || !changed {
		t.Fatalf("pause = %v %v", changed, err)
	}
	h.push(textUpdate(2, 101, "and tomorrow?"))
	chat = receive(t, h.ctx, runtime.calls)
	if strings.Contains(chat.request.Jobs, "Commute") || !strings.Contains(chat.request.Jobs, "Call the plumber") {
		t.Fatalf("jobs section after pausing = %q", chat.request.Jobs)
	}
	chat.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 2, "ok")
	h.waitState(2, "delivered")

	clock.Set(t, "2026-09-28T18:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if run.request.PrincipalID != "bob" || run.request.Jobs != "" {
		t.Fatalf("scheduled turn = %s with jobs %q", run.request.PrincipalID, run.request.Jobs)
	}
	run.answer <- agentruntime.RunResult{Response: "Moist.", Final: "Moist."}
	h.wantMessage(202, "<b>Scheduled: Garden</b>\n\nMoist.")
}

func (h *conversationHarness) createRoute(principalID, id string, spec routes.Spec) routes.Route {
	h.t.Helper()
	now := h.scheduler.Now()
	route := routes.Route{ID: id, PrincipalID: principalID, Spec: spec, CreatedAt: now, UpdatedAt: now}
	if created, err := h.store.CreateRoute(h.ctx, route, routes.MaximumRoutesPerPrincipal); err != nil || !created {
		h.t.Fatalf("create route: %v %v", created, err)
	}
	return route
}

func eastbrookDeparture() watch.Spec {
	return watch.Spec{
		Kind:         watch.KindDeparture,
		Target:       watch.Target{Mode: watch.ModeTrain, From: "Eastbrook", To: "Westfield"},
		Triggers:     []string{watch.TriggerCancelled, watch.TriggerDelay},
		DelayMinutes: 5,
		Days:         []string{"mon", "tue", "wed", "thu", "fri"},
		Start:        "07:30",
		End:          "08:12",
		Zone:         "Europe/Amsterdam",
	}
}

// Chat turns, background tasks, and scheduled turns carry the user's saved routes and enabled
// watches after their jobs, in creation order, so an edit keeps a route in place and an unchanged
// store gives the same text. A scheduled turn has no jobs section, a paused watch is left out, and
// nothing of another user's appears.
func TestTurnsKnowTheUsersRoutesAndWatches(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	h.createJob("alice", "a1b2c3d4e5f60718", "Commute", "0 9 * * 1-5", "Check the 08:12 from Eastbrook.")
	work := h.createRoute("alice", "00000000000000c1", routes.Spec{
		Name: "work", Days: []string{"mon", "tue", "wed", "thu", "fri"}, ArriveBy: "09:00",
		Legs: []routes.Leg{
			{Mode: routes.ModeWalk, Minutes: 5},
			{Mode: routes.ModeTrain, From: "Eastbrook", To: "Westfield", Departures: []string{"08:12"}},
			{Mode: routes.ModeWalk, Minutes: 10},
		},
	})
	clock.Set(t, "2026-09-28T08:00:05+02:00")
	h.createRoute("alice", "00000000000000c2", routes.Spec{
		Name: "evening class", Days: []string{"tue", "thu"},
		Legs: []routes.Leg{{Mode: routes.ModeBus, Stop: "Westfield, Market", Line: "12", Towards: "Eastbrook"}},
	})
	h.createRoute("bob", "00000000000000c3", routes.Spec{
		Name: "gym", Days: []string{"sat"},
		Legs: []routes.Leg{{Mode: routes.ModeTrain, From: "Westfield", To: "Northgate"}},
	})
	h.createWatch("alice", "00000000000000a1", eastbrookDeparture())
	paused := eastbrookDeparture()
	paused.Target.To, paused.End = "Southport", "17:40"
	h.createWatch("alice", "00000000000000a2", paused)
	if changed, err := h.store.SetWatchEnabled(h.ctx, "alice", "00000000000000a2", false, clock.Now()); err != nil || !changed {
		t.Fatalf("pause watch = %v %v", changed, err)
	}

	chatSections := func(update, chatID int64) string {
		t.Helper()
		h.push(textUpdate(update, chatID, "how do I get to work tomorrow?"))
		chat := receive(t, h.ctx, runtime.calls)
		chat.answer <- agentruntime.RunResult{Response: "ok"}
		h.wantReply(chatID, update, "ok")
		h.waitState(update, "delivered")
		return chat.request.Jobs
	}
	first := chatSections(1, 101)
	jobs := strings.Index(first, "Scheduled jobs: the user's enabled")
	saved := strings.Index(first, "\n\nSaved routes: the user's saved journeys, oldest first.")
	watches := strings.Index(first, "\n\nWatches: the user's enabled watches, oldest first.")
	if jobs != 0 || saved < jobs || watches < saved {
		t.Fatalf("sections out of order: %q", first)
	}
	for _, want := range []string{
		"call route_check with the route's name",
		"\n- work: Mon–Fri, arrive by 09:00. Legs: walk 5 min; train Eastbrook → Westfield, departures 08:12; walk 10 min\n- evening class: Tue, Thu. Legs: bus 12 from Westfield, Market towards Eastbrook, no fixed departures\n\n",
		"\n- id 00000000000000a1: 08:12 train Eastbrook → Westfield; ",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("sections lack %q: %q", want, first)
		}
	}
	if strings.Contains(first, "Southport") || strings.Contains(first, "gym") || strings.Contains(first, "Northgate") {
		t.Fatalf("sections list a paused watch or bob's route: %q", first)
	}
	if again := chatSections(2, 101); again != first {
		t.Fatalf("unchanged stores changed the sections:\n%q\n%q", first, again)
	}

	edited := work
	edited.ArriveBy, edited.UpdatedAt = "09:15", clock.Now().Add(time.Minute)
	if updated, err := h.store.UpdateRoute(h.ctx, edited, work.UpdatedAt); err != nil || !updated {
		t.Fatalf("edit route = %v %v", updated, err)
	}
	third := chatSections(3, 101)
	if !strings.Contains(third, "\n- work: Mon–Fri, arrive by 09:15. Legs: walk 5 min;") || strings.Index(third, "- work:") > strings.Index(third, "- evening class:") {
		t.Fatalf("the edited route moved or kept its old target: %q", third)
	}

	task := make(chan turn.TaskReply, 1)
	go func() {
		reply, err := h.engine.RunTask(h.ctx, turn.TaskRun{
			PrincipalID: "alice", Turn: outbox.TaskTurn("eeee"), SessionID: "55555555-5555-4555-8555-555555555555",
			Message: "plan the week's trips", CallBudget: 4,
		})
		if err != nil {
			t.Error(err)
		}
		task <- reply
	}()
	call := receive(t, h.ctx, runtime.calls)
	if call.request.Jobs != third {
		t.Fatalf("task sections = %q, want the chat turn's %q", call.request.Jobs, third)
	}
	call.answer <- agentruntime.RunResult{Response: "planned", Final: "planned"}
	if reply := receive(t, h.ctx, task); reply.Text != "planned" {
		t.Fatalf("task reply = %+v", reply)
	}

	scheduled := make(chan turn.ScheduledReply, 1)
	go func() {
		reply, err := h.engine.RunScheduled(h.ctx, "alice", "schedule/aaaa", "check the commute", agentruntime.ModelEffort{})
		if err != nil {
			t.Error(err)
		}
		scheduled <- reply
	}()
	call = receive(t, h.ctx, runtime.calls)
	if want := third[strings.Index(third, "Saved routes:"):]; call.request.Jobs != want {
		t.Fatalf("scheduled sections = %q, want routes and watches alone: %q", call.request.Jobs, want)
	}
	call.answer <- agentruntime.RunResult{Response: "on time", Final: "on time"}
	if reply := receive(t, h.ctx, scheduled); reply.Text != "on time" {
		t.Fatalf("scheduled reply = %+v", reply)
	}

	bob := chatSections(4, 202)
	if !strings.HasPrefix(bob, "Saved routes:") || !strings.Contains(bob, "\n- gym: Sat. Legs: train Westfield → Northgate") ||
		strings.Contains(bob, "Watches:") || strings.Contains(bob, "Eastbrook") {
		t.Fatalf("bob's sections = %q", bob)
	}
}

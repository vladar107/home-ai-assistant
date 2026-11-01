package telegram

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/project"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// projectIconID is the memo among the icons the simulator offers in these tests.
const projectIconID = "5000000000000000005"

const dentistProject = `{"title":"Find a dentist","goal":"A dentist near home taking new patients",` +
	`"items":[{"text":"Ask neighbours for names"},{"text":"Call the clinic","due_on":"2026-10-01"}]}`

type projectTabHarness struct {
	*conversationHarness
	runtime *controlledRuntime
	clock   *testClock
}

// newProjectTabHarness runs Alice's and Bob's chats with the scheduler and projects whose tabs the
// Telegram service keeps, with tabs on or off.
func newProjectTabHarness(t *testing.T, topics bool) *projectTabHarness {
	t.Helper()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, topics: topics, providers: []tools.Provider{outsideProvider{}}})
	h.tabs.icons = append(slices.Clone(testTopicIcons), TopicIcon{Emoji: "📝", CustomEmojiID: projectIconID})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	return &projectTabHarness{conversationHarness: h, runtime: runtime, clock: clock}
}

// startTurn sends Alice's message in the tab, zero for the main view, and returns the turn.
func (h *projectTabHarness) startTurn(updateID, threadID int64, text string) runtimeCall {
	h.t.Helper()
	update := textUpdate(updateID, 101, text)
	if threadID > 0 {
		update = tabMessage(updateID, threadID, text)
	}
	h.push(update)
	return receive(h.t, h.ctx, h.runtime.calls)
}

// endTurn answers the turn and takes its reply and, for a turn that acted, the "Done" message after
// it, both in the tab.
func (h *projectTabHarness) endTurn(call runtimeCall, updateID, threadID int64, acted bool) {
	h.t.Helper()
	call.answer <- agentruntime.RunResult{Response: "ok"}
	if reply := h.nextMessage(); reply.threadID != threadID || reply.text != "ok" {
		h.t.Fatalf("reply = %+v, want it in tab %d", reply, threadID)
	}
	h.waitState(updateID, "delivered")
	if acted {
		done := h.nextMessage()
		if done.threadID != threadID {
			h.t.Fatalf("done message = %+v, want it in tab %d", done, threadID)
		}
		h.waitMessageRecorded(done.messageID)
	}
}

func (h *projectTabHarness) projectID(principalID string) string {
	h.t.Helper()
	var id string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM projects WHERE principal_id = ? ORDER BY created_at DESC LIMIT 1`, principalID).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *projectTabHarness) itemIDs(projectID string) []string {
	h.t.Helper()
	rows, err := h.db.QueryContext(h.ctx, `SELECT id FROM project_items WHERE project_id = ? ORDER BY position`, projectID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (h *projectTabHarness) projectSummaryMessage(projectID string) int64 {
	h.t.Helper()
	var messageID *int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT summary_message_id FROM projects WHERE id = ?`, projectID).Scan(&messageID); err != nil {
		h.t.Fatal(err)
	}
	if messageID == nil {
		return 0
	}
	return *messageID
}

// wantProjectTab takes the next tab the bot made and checks it is the project's, named after it,
// recorded as fixed, and given the memo icon.
func (h *projectTabHarness) wantProjectTab(name, projectID string) int64 {
	h.t.Helper()
	created := receive(h.t, h.ctx, h.tabs.created)
	if created.name != name || created.chatID != 101 {
		h.t.Fatalf("made tab %+v, want %q", created, name)
	}
	if edit := receive(h.t, h.ctx, h.tabs.topicEdits); edit != (editedTopic{threadID: created.threadID, iconID: projectIconID, iconed: true}) {
		h.t.Fatalf("project tab edit = %+v, want only its icon", edit)
	}
	threadID, found, err := h.store.SubjectTab(h.ctx, "alice", thread.ProjectDestination(projectID))
	if err != nil || !found || threadID != created.threadID {
		h.t.Fatalf("registry has the project at %d %v %v, want %d", threadID, found, err, created.threadID)
	}
	if tab, _ := h.registryTab(created.threadID); tab.NamedBy != thread.NamedByFixed || tab.Name != name {
		h.t.Fatalf("project tab in the registry = %+v", tab)
	}
	return created.threadID
}

// wantProjectSummary takes the next message and checks it is the project's summary in its tab,
// plain and without link previews, pinned silently and recorded.
func (h *projectTabHarness) wantProjectSummary(threadID int64, projectID, text string) sentMessage {
	h.t.Helper()
	summary := h.nextMessage()
	if summary.threadID != threadID || summary.text != text || summary.parseMode != "" || summary.preview != `{"is_disabled":true}` {
		h.t.Fatalf("summary = %+v, want in tab %d as plain text:\n%s", summary, threadID, text)
	}
	if pin := receive(h.t, h.ctx, h.tabs.pins); pin != (pinnedMessage{chatID: 101, messageID: summary.messageID, silent: true}) {
		h.t.Fatalf("pin = %+v, want the summary pinned silently", pin)
	}
	if recorded := h.projectSummaryMessage(projectID); recorded != summary.messageID {
		h.t.Fatalf("recorded summary %d, want %d", recorded, summary.messageID)
	}
	return summary
}

func (h *projectTabHarness) expectNoNewTab() {
	h.t.Helper()
	select {
	case created := <-h.tabs.created:
		h.t.Fatalf("unexpected tab %+v", created)
	default:
	}
}

// A project started by conversation gets its own tab with its summary pinned. Talking there is
// about the project, with its open items on every turn; checking one off edits the summary, a
// reminder for the project arrives in the tab, and a nudge set for it is named when the project is
// done. Deleting the tab forgets the summary, and the done project's messages then go to the
// reminders tab. /projects lists it, and nothing about it reaches the logs.
func TestAProjectHasATabWithItsSummaryPinned(t *testing.T) {
	t.Parallel()
	h := newProjectTabHarness(t, true)

	call := h.startTurn(1, 0, "let's make a project for finding a dentist")
	created := toolCaller(t, call)("project_create", dentistProject)
	id := h.projectID("alice")
	h.wantDirect(created, "project_create")
	if !strings.Contains(created.Text, "(id "+id+")") {
		t.Fatalf("project_create = %+v, want the project's id", created)
	}
	tab := h.wantProjectTab("Find a dentist", id)
	summary := h.wantProjectSummary(tab, id, strings.Join([]string{
		"Find a dentist",
		"Goal: A dentist near home taking new patients",
		"",
		"Open items: 2 of 2",
		"- Ask neighbours for names",
		"- Call the clinic (due Thu 1 Oct)",
		"",
		"Next due: Thu 1 Oct, Call the clinic",
	}, "\n"))
	h.endTurn(call, 1, 0, true)
	items := h.itemIDs(id)
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}

	call = h.startTurn(2, tab, "I asked the neighbours")
	line := `[system] This conversation is about the user's project "Find a dentist" (id ` + id + `): status active; goal "A dentist near home taking new patients"; ` +
		`open items (2 of 2): [` + items[0] + `] "Ask neighbours for names"; [` + items[1] + `] "Call the clinic" due 2026-10-01; next due 2026-10-01. project_get shows it in full.`
	if !strings.Contains(call.request.Message, line) {
		t.Fatalf("turn in the project's tab:\n%s\nwant the line:\n%s", call.request.Message, line)
	}
	checked := toolCaller(t, call)("project_item_check", `{"project_id":"`+id+`","item_id":"`+items[0]+`"}`)
	h.wantDirect(checked, "project_item_check")
	if edit := h.nextEdit(); edit.messageID != summary.messageID || !strings.Contains(edit.text, "Open items: 1 of 2\n- Call the clinic (due Thu 1 Oct)") {
		t.Fatalf("summary after a check = %+v", edit)
	}
	set := toolCaller(t, call)("reminder_set", `{"text":"Call the clinic","at":"2026-09-28 08:05","project_id":"`+id+`"}`)
	nudge := toolCaller(t, call)("schedule_propose", `{"name":"Dentist nudge","cron":"0 9 * * 0","prompt":"Say what is next on finding a dentist.","project_id":"`+id+`"}`)
	if set.IsError || nudge.IsError {
		t.Fatalf("reminder = %+v, nudge = %+v", set, nudge)
	}
	h.endTurn(call, 2, tab, true)
	if _, subject := h.sessionOf(2); subject != thread.ProjectDestination(id) {
		t.Fatalf("the tab's conversation is about %q", subject)
	}

	var reminderID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM scheduled_jobs WHERE job_type = ? AND destination = ?`, schedule.TypeReminder, thread.ProjectDestination(id)).Scan(&reminderID); err != nil {
		t.Fatal(err)
	}
	h.clock.Set(t, "2026-09-28T08:05:10+02:00")
	h.scheduler.Wake()
	if reminder := h.nextMessage(); reminder.threadID != tab || !strings.Contains(reminder.text, "Call the clinic") {
		t.Fatalf("project reminder = %+v, want it in the project's tab %d", reminder, tab)
	}
	h.waitRun(reminderID, schedule.StatusDelivered)
	if destination := h.landedJobDestination(reminderID); destination != thread.ProjectDestination(id) {
		t.Fatalf("report destination = %q", destination)
	}

	call = h.startTurn(3, tab, "we're done, I found one")
	finished := toolCaller(t, call)("project_update", `{"project_id":"`+id+`","status":"done"}`)
	h.wantDirect(finished, "project_update")
	if !strings.Contains(finished.Text, `Jobs still routed to this project: "Dentist nudge"`) || !strings.Contains(finished.Text, "schedule_remove") {
		t.Fatalf("project_update to done = %+v, want the nudge offered for deletion", finished)
	}
	if edit := h.nextEdit(); edit.messageID != summary.messageID || !strings.HasPrefix(edit.text, project.SummaryDone+"\nFind a dentist") {
		t.Fatalf("summary of the done project = %+v", edit)
	}
	h.endTurn(call, 3, tab, true)

	if err := h.store.DeleteTab(h.ctx, "alice", tab); err != nil {
		t.Fatal(err)
	}
	if h.projectSummaryMessage(id) != 0 {
		t.Fatal("the summary outlived its tab")
	}
	h.createReminder("alice", "00000000000000d1", "About the dentist", "2026-09-28 08:10")
	h.write(`UPDATE scheduled_jobs SET destination = ? WHERE id = ?`, thread.ProjectDestination(id), "00000000000000d1")
	h.clock.Set(t, "2026-09-28T08:10:10+02:00")
	h.scheduler.Wake()
	reminders := h.wantFeedTab("Reminders", thread.FeedReminders)
	receive(t, h.ctx, h.tabs.topicEdits)
	if message := h.nextMessage(); message.threadID != reminders || !strings.Contains(message.text, "About the dentist") {
		t.Fatalf("reminder of the done project = %+v, want it in the reminders tab %d", message, reminders)
	}
	h.expectNoNewTab()

	h.push(textUpdate(4, 101, "/projects"))
	if list := h.nextMessage(); list.text != "Projects\nNo open projects.\n\n"+ClosedProjectsOf+"\n- Find a dentist: done, 1 of 2 items open, next due 2026-10-01" {
		t.Fatalf("/projects = %q", list.text)
	}
	for _, private := range []string{"dentist", "neighbours", "clinic"} {
		if strings.Contains(strings.ToLower(h.logs.String()), private) {
			t.Fatalf("%q reached the logs:\n%s", private, h.logs.String())
		}
	}
}

func (h *projectTabHarness) sessionOf(updateID int64) (id, subject string) {
	h.t.Helper()
	var value *string
	if err := h.db.QueryRowContext(h.ctx,
		`SELECT session.id, session.subject FROM conversation_requests AS request
		 JOIN conversation_sessions AS session ON session.id = request.session_id
		 WHERE request.telegram_update_id = ?`, updateID,
	).Scan(&id, &value); err != nil {
		h.t.Fatal(err)
	}
	if value != nil {
		subject = *value
	}
	return id, subject
}

func (h *projectTabHarness) landedJobDestination(jobID string) string {
	h.t.Helper()
	var destination *string
	if err := h.db.QueryRowContext(h.ctx, `SELECT destination FROM job_reports WHERE job_id = ? ORDER BY delivered_at DESC LIMIT 1`, jobID).Scan(&destination); err != nil {
		h.t.Fatal(err)
	}
	if destination == nil {
		return ""
	}
	return *destination
}

// seedProject stores an open project of the principal without telling its tabs.
func (h *projectTabHarness) seedProject(principalID, id, title string) {
	h.t.Helper()
	now := h.clock.Now().UTC().Truncate(time.Millisecond)
	if _, err := h.store.CreateProject(h.ctx, project.Project{
		ID: id, PrincipalID: principalID, Title: title, Status: project.StatusActive, CreatedAt: now, UpdatedAt: now,
	}, project.MaximumOpenProjects); err != nil {
		h.t.Fatal(err)
	}
}

// A job can be routed only to one of the user's own open projects, never to another user's, and a
// job a trip routes keeps its trip. Another user's project is never shown, and a job of one user
// that names another's project falls back to its feed.
func TestProjectsStayWithTheirOwner(t *testing.T) {
	t.Parallel()
	h := newProjectTabHarness(t, true)
	const alices, bobs = "aaaaaaaaaaaaaa01", "bbbbbbbbbbbbbb01"
	h.seedProject("alice", alices, "Paint the shed")
	h.seedProject("bob", bobs, "Plan a party")
	h.createJob("alice", "00000000000000e1", "Packing list", "0 9 * * 1", "List what to pack.")
	h.write(`UPDATE scheduled_jobs SET destination = ? WHERE id = ?`, thread.TripDestination("0000beef"), "00000000000000e1")

	call := h.startTurn(1, 0, "remind me about the party")
	foreign := toolCaller(t, call)("reminder_set", `{"text":"Buy balloons","at":"2026-09-28 09:00","project_id":"`+bobs+`"}`)
	if !foreign.IsError || !strings.Contains(foreign.Text, "no open project has the id") {
		t.Fatalf("reminder_set for another user's project = %+v", foreign)
	}
	shown := toolCaller(t, call)("project_get", `{"project_id":"`+bobs+`"}`)
	if !shown.IsError || strings.Contains(shown.Text, "party") {
		t.Fatalf("project_get of another user's project = %+v", shown)
	}
	moved := toolCaller(t, call)("schedule_update", `{"job_id":"00000000000000e1","project_id":"`+alices+`"}`)
	if !moved.IsError || !strings.Contains(moved.Text, "cannot be moved to a project") {
		t.Fatalf("schedule_update of a trip's job = %+v", moved)
	}
	listed := toolCaller(t, call)("project_list", `{"include_closed":true}`)
	if listed.IsError || !strings.Contains(listed.Text, "Paint the shed") || strings.Contains(listed.Text, "party") {
		t.Fatalf("project_list = %+v", listed)
	}
	h.endTurn(call, 1, 0, false)
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE destination LIKE 'project:%'`); count != 0 {
		t.Fatalf("%d jobs were routed to a project", count)
	}
	if job, _ := h.storedJob("00000000000000e1"); job.Destination != thread.TripDestination("0000beef") {
		t.Fatalf("the trip's job now goes to %q", job.Destination)
	}

	h.createReminder("bob", "00000000000000e2", "Party things", "2026-09-28 08:05")
	h.write(`UPDATE scheduled_jobs SET destination = ? WHERE id = ?`, thread.ProjectDestination(alices), "00000000000000e2")
	h.clock.Set(t, "2026-09-28T08:05:10+02:00")
	h.scheduler.Wake()
	created := receive(t, h.ctx, h.tabs.created)
	if created.chatID != 202 || created.name != "Reminders" {
		t.Fatalf("made tab %+v, want Bob's reminders tab", created)
	}
	if message := h.nextMessage(); message.chatID != 202 || message.threadID != created.threadID || !strings.Contains(message.text, "Party things") {
		t.Fatalf("Bob's reminder = %+v, want it in his reminders tab %d", message, created.threadID)
	}
	if h.projectSummaryMessage(alices) != 0 {
		t.Fatal("Bob's reminder gave Alice's project a summary")
	}
	select {
	case extra := <-h.tabs.created:
		t.Fatalf("unexpected tab %+v", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

// With tabs off, a project gets no tab or summary, and its reminders reach the main view.
func TestProjectsWithTabsOffStayInTheMainView(t *testing.T) {
	t.Parallel()
	h := newProjectTabHarness(t, false)
	call := h.startTurn(1, 0, "let's make a project for finding a dentist")
	h.wantDirect(toolCaller(t, call)("project_create", dentistProject), "project_create")
	id := h.projectID("alice")
	if set := toolCaller(t, call)("reminder_set", `{"text":"Call the clinic","at":"2026-09-28 08:05","project_id":"`+id+`"}`); set.IsError {
		t.Fatalf("reminder_set = %+v", set)
	}
	h.endTurn(call, 1, 0, true)
	h.clock.Set(t, "2026-09-28T08:05:10+02:00")
	h.scheduler.Wake()
	if reminder := h.nextMessage(); reminder.threadID != 0 || !strings.Contains(reminder.text, "Call the clinic") {
		t.Fatalf("project reminder with tabs off = %+v", reminder)
	}
	h.expectNoNewTab()
	if h.projectSummaryMessage(id) != 0 {
		t.Fatal("a summary was recorded with tabs off")
	}
}

// After outside content, a project change is only proposed. A direct change's Undo puts the project
// back, and one changed since refuses it.
func TestProjectChangesAreProposedAfterOutsideContentAndUndone(t *testing.T) {
	t.Parallel()
	h := newProjectTabHarness(t, false)
	const id = "aaaaaaaaaaaaaa02"
	h.seedProject("alice", id, "Paint the shed")

	call := h.startTurn(1, 0, "add primer to the shed project")
	added := toolCaller(t, call)("project_item_add", `{"project_id":"`+id+`","items":[{"text":"Buy primer"}]}`)
	undo := h.wantDirect(added, "project_item_add")
	h.endTurn(call, 1, 0, true)
	if count := h.countRows(`SELECT count(*) FROM project_items WHERE project_id = ?`, id); count != 1 {
		t.Fatalf("items after the add = %d", count)
	}
	h.undoDirect(undo)
	if count := h.countRows(`SELECT count(*) FROM project_items WHERE project_id = ?`, id); count != 0 {
		t.Fatalf("items after Undo = %d", count)
	}

	call = h.startTurn(2, 0, "rename it")
	renamed := toolCaller(t, call)("project_update", `{"project_id":"`+id+`","title":"Paint the fence"}`)
	again := h.wantDirect(renamed, "project_update")
	h.endTurn(call, 2, 0, true)
	call = h.startTurn(3, 0, "and pause it")
	h.wantDirect(toolCaller(t, call)("project_update", `{"project_id":"`+id+`","status":"paused"}`), "project_update")
	h.endTurn(call, 3, 0, true)
	if answer := h.undoDirect(again); !strings.Contains(answer, "changed since") {
		t.Fatalf("Undo of a project changed since = %q", answer)
	}

	// Undoing a move to the project puts the job back where it went before.
	h.createJob("alice", "00000000000000f1", "Weekly check", "0 9 * * 1", "Say what is left.")
	call = h.startTurn(4, 0, "put my weekly check in the shed project")
	moved := h.wantDirect(toolCaller(t, call)("schedule_update", `{"job_id":"00000000000000f1","project_id":"`+id+`"}`), "schedule_update")
	h.endTurn(call, 4, 0, true)
	if job, _ := h.storedJob("00000000000000f1"); job.Destination != thread.ProjectDestination(id) {
		t.Fatalf("moved job goes to %q", job.Destination)
	}
	h.undoDirect(moved)
	if job, _ := h.storedJob("00000000000000f1"); job.Destination != "" {
		t.Fatalf("after Undo the job goes to %q", job.Destination)
	}

	call = h.startTurn(5, 0, "about the shed")
	readOutside(t, call)
	h.wantProposedOnly(toolCaller(t, call)("project_item_add", `{"project_id":"`+id+`","items":[{"text":"Buy brushes"}]}`), "project_item_add")
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, 5, "ok")
	h.waitState(5, "delivered")
	if count := h.countRows(`SELECT count(*) FROM project_items WHERE project_id = ?`, id); count != 0 {
		t.Fatalf("an add after outside content stored %d items", count)
	}
}

// directJobTurn runs one of Alice's clean turns that changes a job and returns its approval ID.
func (h *projectTabHarness) directJobTurn(updateID int64, tool, arguments string) string {
	h.t.Helper()
	call := h.startTurn(updateID, 0, "change my job")
	id := h.wantDirect(toolCaller(h.t, call)(tool, arguments), tool)
	h.endTurn(call, updateID, 0, true)
	return id
}

// exposedProposal runs one of Alice's turns that reads outside content and then calls the tool,
// which only proposes, and returns the proposal's message.
func (h *projectTabHarness) exposedProposal(updateID int64, tool, arguments string) sentMessage {
	h.t.Helper()
	call := h.startTurn(updateID, 0, "about my project")
	readOutside(h.t, call)
	if result := toolCaller(h.t, call)(tool, arguments); result.IsError || !strings.HasSuffix(result.Text, approval.ProposedText) {
		h.t.Fatalf("%s after outside content = %+v, want a proposal", tool, result)
	}
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(101, updateID, "ok")
	h.waitState(updateID, "delivered")
	proposal := h.nextMessage()
	h.waitMessageRecorded(proposal.messageID)
	return proposal
}

// confirm presses Confirm on the proposal and returns the proposal's message as it then reads.
func (h *projectTabHarness) confirm(updateID int64, proposal sentMessage) editedMessage {
	h.t.Helper()
	accept, _ := approvalButtons(h.t, proposal.keyboard)
	h.push(callbackUpdate(updateID, 101, proposal.messageID, accept))
	h.wantAnswer(updateID)
	edit := h.nextEdit()
	h.continueTurn(h.runtime, updateID)
	return edit
}

// A nudge routed to a project reads the project's open items on its run. Undoing an older change
// to a job leaves a later move to a project alone, an Undo of a move the job has since left is
// refused, and project_id none takes a job off its project. A project is checked again on Confirm:
// a job confirmed after outside content goes to the project, and one confirmed after the project
// closed, or for a job a trip has taken since, is refused.
func TestProjectJobsFollowTheirProject(t *testing.T) {
	t.Parallel()
	h := newProjectTabHarness(t, false)
	call := h.startTurn(1, 0, "let's make a project for finding a dentist")
	h.wantDirect(toolCaller(t, call)("project_create", dentistProject), "project_create")
	h.endTurn(call, 1, 0, true)
	id := h.projectID("alice")
	destination := thread.ProjectDestination(id)

	h.directJobTurn(2, "schedule_propose", `{"name":"Dentist nudge","cron":"0 9 * * 1","prompt":"Say what is next on finding a dentist.","project_id":"`+id+`"}`)
	var nudgeID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM scheduled_jobs WHERE name = 'Dentist nudge' AND destination = ?`, destination).Scan(&nudgeID); err != nil {
		t.Fatal(err)
	}
	h.clock.Set(t, "2026-09-28T09:00:10+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, h.runtime.calls)
	if !strings.Contains(run.request.Message, `[system] This conversation is about the user's project "Find a dentist" (id `+id+`)`) ||
		!strings.Contains(run.request.Message, `"Ask neighbours for names"`) || !strings.Contains(run.request.Message, `"Call the clinic" due 2026-10-01`) {
		t.Fatalf("the nudge's run does not read the project:\n%s", run.request.Message)
	}
	run.answer <- agentruntime.RunResult{Response: "Next: ask the neighbours.", Final: "Next: ask the neighbours."}
	if report := h.nextMessage(); !strings.Contains(report.text, "Next: ask the neighbours.") {
		t.Fatalf("nudge report = %+v", report)
	}
	h.waitRun(nudgeID, schedule.StatusDelivered)

	const jobID = "00000000000000f2"
	h.createJob("alice", jobID, "Weekly check", "0 9 * * 2", "Say what is left.")
	stored := func() schedule.Job {
		t.Helper()
		job, _ := h.storedJob(jobID)
		return job
	}
	retimed := h.directJobTurn(3, "schedule_update", `{"job_id":"`+jobID+`","cron":"0 10 * * 2"}`)
	h.directJobTurn(4, "schedule_update", `{"job_id":"`+jobID+`","project_id":"`+id+`"}`)
	h.undoDirect(retimed)
	if job := stored(); job.Cron != "0 9 * * 2" || job.Destination != destination {
		t.Fatalf("after undoing the new time the job runs %q to %q, want its old time and the project", job.Cron, job.Destination)
	}
	h.directJobTurn(5, "schedule_update", `{"job_id":"`+jobID+`","project_id":"none"}`)
	if job := stored(); job.Destination != "" {
		t.Fatalf("taken off its project, the job goes to %q", job.Destination)
	}
	moved := h.directJobTurn(6, "schedule_update", `{"job_id":"`+jobID+`","project_id":"`+id+`"}`)
	h.directJobTurn(7, "schedule_update", `{"job_id":"`+jobID+`","project_id":"none"}`)
	if answer := h.undoDirect(moved); !strings.Contains(answer, "moved since") {
		t.Fatalf("Undo of a move the job has left = %q", answer)
	}
	if job := stored(); job.Destination != "" {
		t.Fatalf("after the refused Undo the job goes to %q", job.Destination)
	}

	proposed := h.exposedProposal(8, "schedule_propose", `{"name":"Clinic follow-up","cron":"0 9 * * 3","prompt":"Ask about the clinic.","project_id":"`+id+`"}`)
	h.confirm(9, proposed)
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE name = 'Clinic follow-up' AND destination = ?`, destination); count != 1 {
		t.Fatalf("confirmed jobs routed to the project = %d", count)
	}

	reminder := h.exposedProposal(10, "reminder_set", `{"text":"Book the check-up","at":"2026-09-29 08:00","project_id":"`+id+`"}`)
	h.write(`UPDATE projects SET status = 'abandoned' WHERE id = ?`, id)
	if refused := h.confirm(11, reminder); !strings.Contains(refused.text, "can no longer take jobs") {
		t.Fatalf("reminder confirmed for a closed project = %+v", refused)
	}
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE job_type = ?`, schedule.TypeReminder); count != 0 {
		t.Fatalf("reminders after the refused Confirm = %d", count)
	}

	h.write(`UPDATE projects SET status = 'active' WHERE id = ?`, id)
	update := h.exposedProposal(12, "schedule_update", `{"job_id":"`+jobID+`","project_id":"`+id+`"}`)
	h.write(`UPDATE scheduled_jobs SET destination = ? WHERE id = ?`, thread.TripDestination("0000beef"), jobID)
	if refused := h.confirm(13, update); !strings.Contains(refused.text, "cannot be moved to a project") {
		t.Fatalf("move confirmed for a trip's job = %+v", refused)
	}
	if job := stored(); job.Destination != thread.TripDestination("0000beef") {
		t.Fatalf("the trip's job now goes to %q", job.Destination)
	}
}

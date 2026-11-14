package telegram

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// userTab and otherUserTab are tabs Alice opened and named herself; tabMessage names them "Trip
// notes".
const (
	userTab      = 50
	otherUserTab = 51

	// tabUsedText is how a trip_add result says the trip took the tab it was added in.
	tabUsedText = "It uses this tab: its summary is pinned here"
)

// newTripTakeHarness is newTripTabHarness with a tool that reads outside content, after which trip
// changes are only proposed.
func newTripTakeHarness(t *testing.T) *tripTabHarness {
	t.Helper()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, topics: true, providers: []tools.Provider{outsideProvider{}}})
	h.tabs.icons = append(slices.Clone(testTopicIcons), TopicIcon{Emoji: "✈️", CustomEmojiID: tripIconID})
	h.provision("alice", "account-alice")
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	return &tripTabHarness{conversationHarness: h, runtime: runtime, clock: clock, trips: h.trips, notifier: h.tripNotifier}
}

// tripTurn sends Alice's message in the tab, zero for the main view, and returns the turn.
func (h *tripTabHarness) tripTurn(updateID, threadID int64, text string) runtimeCall {
	h.t.Helper()
	update := textUpdate(updateID, 101, text)
	if threadID > 0 {
		update = tabMessage(updateID, threadID, text)
	}
	h.push(update)
	return receive(h.t, h.ctx, h.runtime.calls)
}

// endTripTurn answers the turn and takes its reply and, for a turn that acted, the "Done" message
// after it, both in the tab.
func (h *tripTabHarness) endTripTurn(call runtimeCall, updateID, threadID int64, acted bool) {
	h.t.Helper()
	call.answer <- agentruntime.RunResult{Response: "ok"}
	if reply := h.nextMessage(); reply.threadID != threadID || reply.text != "ok" {
		h.t.Fatalf("reply = %+v, want it in tab %d", reply, threadID)
	}
	h.waitState(updateID, "delivered")
	if acted {
		if done := h.nextMessage(); done.threadID != threadID || !strings.HasPrefix(done.text, "Done:") {
			h.t.Fatalf("done message = %+v, want it in tab %d", done, threadID)
		}
	}
}

// moveTrip runs Alice's turn in the tab that says it is the trip's, and returns the tool's result
// and the trip's summary, which must be sent and pinned there.
func (h *tripTabHarness) moveTrip(updateID, threadID int64, tripID string) (agentruntime.ToolResult, sentMessage) {
	h.t.Helper()
	call := h.tripTurn(updateID, threadID, "this is the tab for my trip")
	result := toolCaller(h.t, call)("trip_update", `{"id":"`+tripID+`","use_this_tab":true}`)
	if result.IsError || !strings.Contains(result.Text, "Done:") || !strings.Contains(result.Text, "to this tab: its summary is pinned here") {
		h.t.Fatalf("move = %+v, want it done", result)
	}
	summary := h.wantSummary(threadID, tripID, h.summaryOf(tripID))
	h.endTripTurn(call, updateID, threadID, true)
	return result, summary
}

// refusedIn runs Alice's turn in the tab with the tool call, which must be refused with the text.
func (h *tripTabHarness) refusedIn(updateID, threadID int64, tool, arguments, want string) {
	h.t.Helper()
	call := h.tripTurn(updateID, threadID, "use this tab for my trip")
	if result := toolCaller(h.t, call)(tool, arguments); !result.IsError || !strings.Contains(result.Text, want) {
		h.t.Fatalf("%s in tab %d = %+v, want refused with %q", tool, threadID, result, want)
	}
	h.endTripTurn(call, updateID, threadID, false)
}

func (h *tripTabHarness) wantMovedNote(summary sentMessage, title string) {
	h.t.Helper()
	if edit := h.nextEdit(); edit.messageID != summary.messageID || edit.text != localetest.English(TripMovedNote)+"\n"+title {
		h.t.Fatalf("summary in the tab the trip left = %+v, want the moved note", edit)
	}
}

func (h *tripTabHarness) expectNoTopicEdit() {
	h.t.Helper()
	select {
	case edit := <-h.tabs.topicEdits:
		h.t.Fatalf("unexpected tab edit %+v", edit)
	default:
	}
}

func (h *tripTabHarness) tripTab(tripID string) int64 {
	h.t.Helper()
	threadID, found, err := h.store.SubjectTab(h.ctx, "alice", thread.TripDestination(tripID))
	if err != nil {
		h.t.Fatal(err)
	}
	if !found {
		return 0
	}
	return threadID
}

func (h *tripTabHarness) tripIDTitled(title string) string {
	h.t.Helper()
	var id string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM trips WHERE principal_id = 'alice' AND title = ?`, title).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func tripAddArguments(title string) string {
	return `{"title":"` + title + `","destination":"Example City","starts_at":"2026-10-01 09:00","ends_at":"2026-10-03","timezone":"Europe/Lisbon"}`
}

// Saying in a tab of the user's own that it is the trip's moves the trip there: its summary is
// pinned there, the summary in the tab it left says it moved, the tab keeps its name, and the
// trip's conversation, reminders, notices, and news continue there.
func TestSayingItInAUserTabMovesTheTripThere(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, true)
	planned := h.addTrip()
	old := h.wantTripTab("Example trip", planned.ID)
	first := h.wantSummary(old, planned.ID, h.summaryOf(planned.ID))

	result, _ := h.moveTrip(1, userTab, planned.ID)
	if !strings.Contains(result.Text, "The tab it had stays as an ordinary tab with its messages, which the user can delete.") {
		t.Fatalf("move = %q, want the left tab named", result.Text)
	}
	h.wantMovedNote(first, "Example trip")
	if tab := h.tripTab(planned.ID); tab != userTab {
		t.Fatalf("the trip's tab = %d, want %d", tab, userTab)
	}
	if taken, _ := h.registryTab(userTab); taken.Name != "Trip notes" || taken.NamedBy != thread.NamedByUser || taken.IconCustomEmojiID != "" {
		t.Fatalf("taken tab in the registry = %+v", taken)
	}
	if left, _ := h.registryTab(old); left.Subject != "" || left.NamedBy != thread.NamedByUser || left.Name != "Example trip" {
		t.Fatalf("left tab in the registry = %+v", left)
	}
	h.expectNoNewTab()

	line := "[system] This conversation is about the user's trip \"Example trip\" (id " + planned.ID + "); trip_list with its id shows it in full."
	call := h.tripTurn(2, userTab, "what should I pack?")
	if !strings.Contains(call.request.Message, line) {
		t.Fatalf("the next turn in the taken tab is not about the trip:\n%s", call.request.Message)
	}
	h.endTripTurn(call, 2, userTab, false)
	conversation, subject := h.requestSession(2)
	if moved, _ := h.requestSession(1); moved != conversation || subject != thread.TripDestination(planned.ID) {
		t.Fatalf("the next turn joined %s about %q, want the tab's conversation %s about the trip", conversation, subject, moved)
	}

	reminderID := trip.ReminderID(planned.ID, planned.Bookings[0].ID)
	h.clock.Set(t, "2026-09-30T08:00:10+02:00")
	h.scheduler.Wake()
	reminder := h.nextMessage()
	if reminder.threadID != userTab || !strings.Contains(reminder.text, "Check-in opens: Flight to Example City (ABC123)") {
		t.Fatalf("check-in reminder = %+v, want it in the taken tab", reminder)
	}
	h.waitRun(reminderID, schedule.StatusDelivered)
	reply := tabMessage(3, userTab, "is check-in open yet?")
	reply.Message.ReplyToMessage = &Message{MessageID: reminder.messageID, MessageThreadID: userTab}
	h.push(reply)
	call = receive(t, h.ctx, h.runtime.calls)
	if !strings.Contains(call.request.Message, line) {
		t.Fatalf("the reply to the reminder is not about the trip:\n%s", call.request.Message)
	}
	h.endTripTurn(call, 3, userTab, false)
	if session, _ := h.requestSession(3); session != conversation {
		t.Fatalf("the reply to the reminder joined %s, want %s", session, conversation)
	}

	loop, err := trip.NewLoop(trip.LoopConfig{Now: h.clock.Now}, h.trips, nil, h.notifier, slog.New(slog.NewTextHandler(h.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Set(t, "2026-10-01T08:01:00+02:00")
	loop.Cycle(h.ctx)
	if started := h.nextMessage(); started.threadID != userTab || !strings.Contains(started.text, "has started") {
		t.Fatalf("start notice = %+v, want it in the taken tab", started)
	}
	if err := h.notifier.Send(h.ctx, "alice", planned.ID, "The flight leaves from another gate."); err != nil {
		t.Fatal(err)
	}
	if update := h.nextMessage(); update.threadID != userTab || update.text != "The flight leaves from another gate." {
		t.Fatalf("trip update = %+v, want it in the taken tab", update)
	}
	// Each delivery resolved the taken tab, which gives a tab the bot made its name and icon.
	h.expectNoTopicEdit()
	h.expectNoNewTab()

	call = h.tripTurn(4, old, "anything left here?")
	if strings.Contains(call.request.Message, "about the user's trip") {
		t.Fatalf("a turn in the tab the trip left is about the trip:\n%s", call.request.Message)
	}
	h.endTripTurn(call, 4, old, false)
	if _, subject := h.requestSession(4); subject != "" {
		t.Fatalf("a new conversation in the tab the trip left is about %q", subject)
	}
}

// A taken tab keeps the name and icon the user gave it when the trip's title changes and after a
// restart, and a rename the user makes is kept.
func TestATakenTabKeepsTheUsersNameAndIcon(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, true)
	planned := h.addTrip()
	old := h.wantTripTab("Example trip", planned.ID)
	first := h.wantSummary(old, planned.ID, h.summaryOf(planned.ID))
	_, summary := h.moveTrip(1, userTab, planned.ID)
	h.wantMovedNote(first, "Example trip")

	renamed := h.update(planned, func(draft *trip.Draft) { draft.Title = "Example weekend" })
	if edit := h.nextEdit(); edit.messageID != summary.messageID || !strings.HasPrefix(edit.text, "Example weekend (Example City)") {
		t.Fatalf("summary after a new title = %+v", edit)
	}
	h.expectNoTopicEdit()
	if taken, _ := h.registryTab(userTab); taken.Name != "Trip notes" || taken.IconCustomEmojiID != "" {
		t.Fatalf("taken tab after a new title = %+v", taken)
	}

	h.push(userRenamedTab(2, userTab, "Our weekend"))
	h.waitOffset(3)
	h.update(renamed, func(draft *trip.Draft) { draft.Destination = "Example Town" })
	h.nextEdit()
	h.expectNoTopicEdit()

	// At start the trips are settled, and a new process has no record of which tabs it iconed.
	if err := h.trips.Settle(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.nextEdit()
	restarted := NewTabs(h.client, h.store, h.trips, nil, nil, nil, slog.New(slog.NewTextHandler(h.logs, nil)), nil)
	restarted.refreshTopicIcons(h.ctx)
	if _, offered := restarted.IconFor(tripTabIcon); !offered {
		t.Fatal("the restarted tabs were not offered the trip icon")
	}
	restarted.Summarize(h.ctx, "alice", 101, planned.ID)
	if edit := h.nextEdit(); edit.messageID != summary.messageID {
		t.Fatalf("summary after the restart = %+v", edit)
	}
	h.expectNoTopicEdit()
	if taken, _ := h.registryTab(userTab); taken.Name != "Our weekend" || taken.NamedBy != thread.NamedByUser || taken.IconCustomEmojiID != "" ||
		taken.Subject != thread.TripDestination(planned.ID) {
		t.Fatalf("taken tab after the user's rename and a restart = %+v", taken)
	}
	h.expectNoNewTab()
}

// The main view, a feed's tab, an open trip's tab, a project's tab, and a watch's tab cannot be a
// trip's, and a suggested or ended trip does not move; each is refused with the reason and changes
// nothing.
func TestATripRefusesTabsItCannotUse(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, true)
	planned := h.addTrip()
	tab := h.wantTripTab("Example trip", planned.ID)
	h.wantSummary(tab, planned.ID, h.summaryOf(planned.ID))
	second := h.exampleDraft("")
	second.Title, second.Bookings = "Second trip", nil
	other := h.addTripFrom(second)
	otherTab := h.wantTripTab("Second trip", other.ID)
	h.wantSummary(otherTab, other.ID, h.summaryOf(other.ID))
	if err := h.notifier.Send(h.ctx, "alice", "", "a trip notice"); err != nil {
		t.Fatal(err)
	}
	reminders := h.wantFeedTab("Reminders", thread.FeedReminders)
	receive(t, h.ctx, h.tabs.topicEdits)
	h.nextMessage()
	const projectTab, watchTab = 70, 72
	if err := h.store.AddSubjectTab(h.ctx, "alice", projectTab, "Example project", thread.ProjectDestination("0000000000000001")); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddSubjectTab(h.ctx, "alice", watchTab, "Example watch", thread.WatchDestination("0000000000000002")); err != nil {
		t.Fatal(err)
	}

	move := `{"id":"` + planned.ID + `","use_this_tab":true}`
	h.refusedIn(1, 0, "trip_update", move, "This is the main view, which cannot be a trip's tab.")
	h.refusedIn(2, 0, "trip_add", strings.TrimSuffix(tripAddArguments("Third trip"), "}")+`,"use_this_tab":true}`, "This is the main view")
	h.refusedIn(3, reminders, "trip_update", move, "This is the Reminders tab, which keeps its own kind of messages, so it cannot be a trip's tab.")
	h.refusedIn(4, otherTab, "trip_update", move, `This tab is the trip "Second trip"'s, which has not ended, so it cannot be another trip's tab.`)
	h.refusedIn(5, projectTab, "trip_update", move, "This tab is a project's, so it cannot be a trip's tab.")
	h.refusedIn(6, userTab, "trip_update", `{"id":"`+planned.ID+`","use_this_tab":true,"cancel":true}`, "use_this_tab does not go with cancel or end_now.")
	const fixedTab, unknownTab = 71, 77
	if _, err := h.store.AddTab(h.ctx, "alice", fixedTab, "Example", thread.NamedByFixed); err != nil {
		t.Fatal(err)
	}
	h.refusedIn(7, fixedTab, "trip_update", move, "This tab cannot be a trip's tab.")
	// A message in a tab that does not reply to the tab's creation leaves the registry without it.
	unknown := textUpdate(8, 101, "use this tab for my trip")
	unknown.Message.MessageThreadID = unknownTab
	h.push(unknown)
	call := receive(t, h.ctx, h.runtime.calls)
	if result := toolCaller(t, call)("trip_update", move); !result.IsError || !strings.Contains(result.Text, "This tab is not in my records yet, so a trip cannot use it.") {
		t.Fatalf("trip_update in a tab the registry does not know = %+v", result)
	}
	h.endTripTurn(call, 8, unknownTab, false)

	found := h.exampleDraft(trip.SourceMail)
	found.Title = "Found trip"
	for index := range found.Bookings {
		found.Bookings[index].Reference += "-9"
	}
	suggested, err := h.trips.Suggest(h.ctx, "alice", found)
	if err != nil || suggested.Outcome != trip.OutcomeNew {
		t.Fatalf("suggest: %v %+v", err, suggested)
	}
	h.refusedIn(9, userTab, "trip_update", `{"id":"`+suggested.Trip.ID+`","use_this_tab":true}`, "That trip is not tracked yet, so it has no tab to move")
	past := h.exampleDraft("")
	past.Title, past.Bookings = "Past trip", nil
	past.StartsAt, past.EndsAt = past.StartsAt.Add(-10*24*time.Hour), past.EndsAt.Add(-10*24*time.Hour)
	ended := h.addTripFrom(past)
	h.refusedIn(10, userTab, "trip_update", `{"id":"`+ended.ID+`","use_this_tab":true}`, "That trip has ended, so it does not move to another tab.")
	h.refusedIn(11, watchTab, "trip_update", move, "This tab is a watch's, so it cannot be a trip's tab.")

	if h.tripTab(planned.ID) != tab || h.tripTab(other.ID) != otherTab || h.tripTab(ended.ID) != 0 {
		t.Fatalf("a refused move changed the trips' tabs")
	}
	if registered, _ := h.registryTab(userTab); registered.Subject != "" {
		t.Fatalf("the user's tab serves %q after refused moves", registered.Subject)
	}
	h.expectNoNewTab()
	h.expectNoTopicEdit()
}

// With tabs off, the trip tools refuse to use a tab.
func TestTripsWithTabsOffRefuseToUseATab(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, false)
	planned := h.addTrip()
	h.refusedIn(1, 0, "trip_update", `{"id":"`+planned.ID+`","use_this_tab":true}`, trip.TabsOffRefusal)
	h.refusedIn(2, 0, "trip_add", strings.TrimSuffix(tripAddArguments("Second trip"), "}")+`,"use_this_tab":true}`, trip.TabsOffRefusal)
}

// The tab of a trip that has ended can be taken. The bot made it for that trip, so it takes the new
// trip's name and keeps the trip icon, and the ended trip's summary there says the tab passed on.
// The ended trip is left without a tab, so its messages go to the reminders tab.
func TestATabOfAnEndedTripCanBeTaken(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, true)
	now := h.clock.Now()
	visit := h.exampleDraft("")
	visit.Title, visit.Bookings = "Example visit", nil
	visit.StartsAt, visit.EndsAt = now.Add(time.Hour), now.Add(2*time.Hour)
	ended := h.addTripFrom(visit)
	endedTab := h.wantTripTab("Example visit", ended.ID)
	visitSummary := h.wantSummary(endedTab, ended.ID, h.summaryOf(ended.ID))
	planned := h.addTrip()
	plannedTab := h.wantTripTab("Example trip", planned.ID)
	first := h.wantSummary(plannedTab, planned.ID, h.summaryOf(planned.ID))
	h.clock.Set(t, "2026-09-28T11:00:00+02:00")

	h.moveTrip(1, endedTab, planned.ID)
	h.wantMovedNote(first, "Example trip")
	if edit := h.nextEdit(); edit.messageID != visitSummary.messageID || edit.text != localetest.English(TripTabPassedNote)+"\nExample visit" {
		t.Fatalf("the ended trip's summary after the take = %+v", edit)
	}
	if edit := receive(t, h.ctx, h.tabs.topicEdits); edit != (editedTopic{threadID: endedTab, name: "Example trip", named: true}) {
		t.Fatalf("taken tab edit = %+v, want it named after the new trip", edit)
	}
	h.expectNoTopicEdit()
	if h.tripTab(planned.ID) != endedTab || h.tripTab(ended.ID) != 0 || h.summaryMessage(ended.ID) != 0 {
		t.Fatalf("after the take the trip is at %d and the ended trip at %d with summary %d",
			h.tripTab(planned.ID), h.tripTab(ended.ID), h.summaryMessage(ended.ID))
	}
	if taken, _ := h.registryTab(endedTab); taken.Name != "Example trip" || taken.NamedBy != thread.NamedByFixed || taken.IconCustomEmojiID != tripIconID {
		t.Fatalf("taken tab in the registry = %+v", taken)
	}
	if err := h.notifier.Send(h.ctx, "alice", ended.ID, "about the ended trip"); err != nil {
		t.Fatal(err)
	}
	reminders := h.wantFeedTab("Reminders", thread.FeedReminders)
	receive(t, h.ctx, h.tabs.topicEdits)
	if message := h.nextMessage(); message.threadID != reminders {
		t.Fatalf("notice of the ended trip = %+v, want it in the reminders tab", message)
	}
	h.expectNoNewTab()
}

// A trip added in a tab the user named takes it and no tab is made; one added in a tab the client
// opened for a message, or in the main view, gets a tab of its own. Confirmed after outside
// content, an add in a tab the user named takes it too, and its card says so.
func TestATripAddedInATabTheUserNamedTakesIt(t *testing.T) {
	t.Parallel()
	h := newTripTakeHarness(t)
	call := h.tripTurn(1, userTab, "I'm going to Example City")
	if result := toolCaller(t, call)("trip_add", tripAddArguments("Example trip")); result.IsError || !strings.Contains(result.Text, tabUsedText) {
		t.Fatalf("add in the user's tab = %+v", result)
	}
	id := h.tripIDTitled("Example trip")
	h.wantSummary(userTab, id, h.summaryOf(id))
	h.endTripTurn(call, 1, userTab, true)
	if h.tripTab(id) != userTab {
		t.Fatalf("the trip added in the user's tab is at %d", h.tripTab(id))
	}
	if taken, _ := h.registryTab(userTab); taken.Name != "Trip notes" || taken.NamedBy != thread.NamedByUser {
		t.Fatalf("taken tab in the registry = %+v", taken)
	}
	h.expectNoNewTab()
	h.expectNoTopicEdit()

	const openedTab = 52
	h.push(implicitTab(2, openedTab))
	h.waitOffset(3)
	call = h.tripTurn(3, openedTab, "and to Example Town")
	if result := toolCaller(t, call)("trip_add", tripAddArguments("Second trip")); result.IsError || strings.Contains(result.Text, tabUsedText) {
		t.Fatalf("add in a tab the client opened = %+v", result)
	}
	second := h.tripIDTitled("Second trip")
	secondTab := h.wantTripTab("Second trip", second)
	h.wantSummary(secondTab, second, h.summaryOf(second))
	h.endTripTurn(call, 3, openedTab, true)
	naming := receive(t, h.ctx, h.runtime.calls)
	if !strings.HasPrefix(naming.request.Message, "[system] Name the chat tab") {
		t.Fatalf("model call = %q, want the naming of the opened tab", naming.request.Message)
	}
	naming.answer <- agentruntime.RunResult{Final: `{"name":"Travel plans"}`, Response: `{"name":"Travel plans"}`}
	receive(t, h.ctx, h.tabs.topicEdits)
	if opened, _ := h.registryTab(openedTab); opened.Subject != "" || opened.NamedBy != thread.NamedByBot {
		t.Fatalf("the tab the client opened = %+v, want it left alone", opened)
	}

	call = h.tripTurn(4, 0, "and to Example Village")
	if result := toolCaller(t, call)("trip_add", tripAddArguments("Third trip")); result.IsError || strings.Contains(result.Text, tabUsedText) {
		t.Fatalf("add in the main view = %+v", result)
	}
	third := h.tripIDTitled("Third trip")
	h.wantSummary(h.wantTripTab("Third trip", third), third, h.summaryOf(third))
	h.endTripTurn(call, 4, 0, true)

	call = h.tripTurn(5, otherUserTab, "here is the booking mail")
	readOutside(t, call)
	if result := toolCaller(t, call)("trip_add", tripAddArguments("Fourth trip")); result.IsError || !strings.Contains(result.Text, approval.ProposedText) {
		t.Fatalf("add after outside content = %+v, want a proposal", result)
	}
	call.answer <- agentruntime.RunResult{Response: "ok"}
	if reply := h.nextMessage(); reply.threadID != otherUserTab {
		t.Fatalf("reply = %+v", reply)
	}
	h.waitState(5, "delivered")
	proposal := h.nextMessage()
	if proposal.threadID != otherUserTab || !strings.Contains(proposal.text, "Fourth trip") || !strings.Contains(proposal.text, "The trip takes this tab.") {
		t.Fatalf("proposal = %+v, want its card to say the trip takes the tab", proposal)
	}
	h.waitMessageRecorded(proposal.messageID)
	accept, _ := approvalButtons(t, proposal.keyboard)
	h.push(tabPress(6, otherUserTab, proposal.messageID, accept))
	h.wantAnswer(6)
	if outcome := h.nextEdit(); outcome.messageID != proposal.messageID || !strings.Contains(outcome.text, "Saved the trip") {
		t.Fatalf("proposal after Confirm = %+v", outcome)
	}
	fourth := h.tripIDTitled("Fourth trip")
	h.wantSummary(otherUserTab, fourth, h.summaryOf(fourth))
	h.continueTurn(h.runtime, 6)
	if h.tripTab(fourth) != otherUserTab {
		t.Fatalf("the confirmed trip is at %d, want the tab it was added in", h.tripTab(fourth))
	}
	h.expectNoNewTab()
}

// A trip can move on and back again, and moving it to the tab it has changes nothing: it always has
// one tab.
func TestMovingATripAgainAndBackLeavesItOneTab(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, true)
	planned := h.addTrip()
	old := h.wantTripTab("Example trip", planned.ID)
	first := h.wantSummary(old, planned.ID, h.summaryOf(planned.ID))

	_, inUserTab := h.moveTrip(1, userTab, planned.ID)
	h.wantMovedNote(first, "Example trip")
	_, inOtherTab := h.moveTrip(2, otherUserTab, planned.ID)
	h.wantMovedNote(inUserTab, "Example trip")
	h.moveTrip(3, old, planned.ID)
	h.wantMovedNote(inOtherTab, "Example trip")

	call := h.tripTurn(4, old, "this is the trip's tab")
	if result := toolCaller(t, call)("trip_update", `{"id":"`+planned.ID+`","use_this_tab":true}`); result.IsError ||
		!strings.Contains(result.Text, `The trip "Example trip" already uses this tab; nothing changed.`) {
		t.Fatalf("move to the trip's own tab = %+v", result)
	}
	h.endTripTurn(call, 4, old, false)

	if count := h.countRows(`SELECT count(*) FROM telegram_tabs WHERE principal_id = 'alice' AND subject = ?`, thread.TripDestination(planned.ID)); count != 1 || h.tripTab(planned.ID) != old {
		t.Fatalf("the trip has %d tabs, at %d", count, h.tripTab(planned.ID))
	}
	for _, threadID := range []int64{userTab, otherUserTab} {
		if left, _ := h.registryTab(threadID); left.Subject != "" || left.Name != "Trip notes" {
			t.Fatalf("tab %d after the trip left = %+v", threadID, left)
		}
	}
	h.expectNoNewTab()
	h.expectNoTopicEdit()
}

// A taken tab the user deleted is replaced on the trip's next message, as any gone trip tab is.
func TestADeletedTakenTabIsReplaced(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, true)
	planned := h.addTrip()
	old := h.wantTripTab("Example trip", planned.ID)
	first := h.wantSummary(old, planned.ID, h.summaryOf(planned.ID))
	_, taken := h.moveTrip(1, userTab, planned.ID)
	h.wantMovedNote(first, "Example trip")

	h.goneTab(userTab)
	h.ageTab(userTab)
	if err := h.notifier.Send(h.ctx, "alice", planned.ID, "first"); err != nil {
		t.Fatal(err)
	}
	h.wantRefused("sendMessage", userTab)
	if message := h.nextMessage(); message.threadID != 0 || message.text != "first" {
		t.Fatalf("notice to the gone tab = %+v, want it in the main view", message)
	}
	if _, kept := h.registryTab(userTab); kept || h.summaryMessage(planned.ID) != 0 {
		t.Fatalf("the gone tab was kept (%v) with summary %d", kept, h.summaryMessage(planned.ID))
	}
	if err := h.notifier.Send(h.ctx, "alice", planned.ID, "second"); err != nil {
		t.Fatal(err)
	}
	again := h.wantTripTab("Example trip", planned.ID)
	summary := h.wantSummary(again, planned.ID, h.summaryOf(planned.ID))
	if again == userTab || again == old || summary.messageID == taken.messageID {
		t.Fatalf("the trip kept tab %d or summary %d", again, summary.messageID)
	}
	if message := h.nextMessage(); message.threadID != again || message.text != "second" {
		t.Fatalf("notice after the tab was gone = %+v, want it in the new tab %d", message, again)
	}
}

// A trip_add asking for the tab takes a tab the client opened for a message too, and a trip that
// has already ended is given no tab: asked for, it is refused, and otherwise its card does not say
// it takes the tab.
func TestATripAddedWithTheFlagTakesATabTheClientOpened(t *testing.T) {
	t.Parallel()
	h := newTripTakeHarness(t)
	const openedTab = 53
	h.push(implicitTab(1, openedTab))
	h.waitOffset(2)
	call := h.tripTurn(2, openedTab, "use this tab for my trip to Example City")
	if result := toolCaller(t, call)("trip_add", strings.TrimSuffix(tripAddArguments("Example trip"), "}")+`,"use_this_tab":true}`); result.IsError || !strings.Contains(result.Text, tabUsedText) {
		t.Fatalf("add with the flag in a tab the client opened = %+v", result)
	}
	id := h.tripIDTitled("Example trip")
	h.wantSummary(openedTab, id, h.summaryOf(id))
	h.endTripTurn(call, 2, openedTab, true)
	if taken, _ := h.registryTab(openedTab); taken.Subject != thread.TripDestination(id) || taken.NamedBy != thread.NamedByUser {
		t.Fatalf("the opened tab after the take = %+v", taken)
	}
	h.expectNoNewTab()

	past := `{"title":"Past trip","destination":"Example City","starts_at":"2026-09-01","ends_at":"2026-09-03","timezone":"Europe/Lisbon"}`
	h.refusedIn(3, userTab, "trip_add", strings.TrimSuffix(past, "}")+`,"use_this_tab":true}`, "That trip has already ended, so it cannot use a tab")
	call = h.tripTurn(4, userTab, "I was in Example City last month")
	if result := toolCaller(t, call)("trip_add", past); result.IsError || strings.Contains(result.Text, tabUsedText) {
		t.Fatalf("add of an ended trip in the user's tab = %+v", result)
	}
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.nextMessage()
	h.waitState(4, "delivered")
	if done := h.nextMessage(); !strings.Contains(done.text, "Past trip") || strings.Contains(done.text, "The trip takes this tab.") {
		t.Fatalf("done message for an ended trip = %+v", done)
	}
	if registered, _ := h.registryTab(userTab); registered.Subject != "" {
		t.Fatalf("an ended trip took the user's tab: %+v", registered)
	}
	h.expectNoNewTab()
}

// A trip added after outside content in a tab the user named takes it only if it still can when
// confirmed; otherwise it gets a tab of its own and the outcome says why.
func TestAConfirmedAddWhoseTabWasTakenMeanwhileSaysSo(t *testing.T) {
	t.Parallel()
	h := newTripTakeHarness(t)
	call := h.tripTurn(1, userTab, "here is the booking mail")
	readOutside(t, call)
	if result := toolCaller(t, call)("trip_add", tripAddArguments("Example trip")); result.IsError || !strings.Contains(result.Text, approval.ProposedText) {
		t.Fatalf("add after outside content = %+v, want a proposal", result)
	}
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.nextMessage()
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	if !strings.Contains(proposal.text, "The trip takes this tab.") {
		t.Fatalf("proposal = %+v, want its card to say the trip takes the tab", proposal)
	}
	h.waitMessageRecorded(proposal.messageID)

	other := h.exampleDraft("")
	other.Title, other.Bookings = "Second trip", nil
	second := h.addTripFrom(other)
	secondTab := h.wantTripTab("Second trip", second.ID)
	secondSummary := h.wantSummary(secondTab, second.ID, h.summaryOf(second.ID))
	if moved, err := h.trips.UseTab(h.ctx, "alice", second.ID, userTab); err != nil || moved.Rejection != "" {
		t.Fatalf("move of the second trip = %+v %v", moved, err)
	}
	h.wantMovedNote(secondSummary, "Second trip")
	h.wantSummary(userTab, second.ID, h.summaryOf(second.ID))

	accept, _ := approvalButtons(t, proposal.keyboard)
	h.push(tabPress(2, userTab, proposal.messageID, accept))
	h.wantAnswer(2)
	outcome := h.nextEdit()
	if outcome.messageID != proposal.messageID || !strings.Contains(outcome.text, "Saved the trip") ||
		!strings.Contains(outcome.text, `It does not use this tab: This tab is the trip "Second trip"'s`) {
		t.Fatalf("proposal after Confirm = %+v, want the trip saved without the tab", outcome)
	}
	id := h.tripIDTitled("Example trip")
	h.wantSummary(h.wantTripTab("Example trip", id), id, h.summaryOf(id))
	h.continueTurn(h.runtime, 2)
	if h.tripTab(second.ID) != userTab {
		t.Fatalf("the second trip lost the user's tab")
	}
}

// A change with a move moves the trip first, so the change's summary lands in the taken tab, also
// for a trip whose tab is gone, for which no tab is made.
func TestAChangeWithAMoveLandsInTheTakenTab(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, true)
	planned := h.addTrip()
	old := h.wantTripTab("Example trip", planned.ID)
	first := h.wantSummary(old, planned.ID, h.summaryOf(planned.ID))

	before := h.summaryOf(planned.ID)
	call := h.tripTurn(1, userTab, "call it a weekend and keep it here")
	result := toolCaller(t, call)("trip_update", `{"id":"`+planned.ID+`","title":"Example weekend","use_this_tab":true}`)
	if result.IsError || !strings.Contains(result.Text, "Updated the trip") || !strings.Contains(result.Text, `Moved the trip "Example trip" to this tab`) {
		t.Fatalf("change with a move = %+v", result)
	}
	summary := h.wantSummary(userTab, planned.ID, before)
	h.wantMovedNote(first, "Example trip")
	if edit := h.nextEdit(); edit.messageID != summary.messageID || !strings.HasPrefix(edit.text, "Example weekend (Example City)") {
		t.Fatalf("summary after the change = %+v, want the one in the taken tab", edit)
	}
	h.endTripTurn(call, 1, userTab, true)

	second := h.exampleDraft("")
	second.Title, second.Bookings = "Second trip", nil
	gone := h.addTripFrom(second)
	goneTab := h.wantTripTab("Second trip", gone.ID)
	h.wantSummary(goneTab, gone.ID, h.summaryOf(gone.ID))
	// A tab taken for gone is dropped from the registry, with its summary.
	if err := h.store.DeleteTab(h.ctx, "alice", goneTab); err != nil {
		t.Fatal(err)
	}
	before = h.summaryOf(gone.ID)
	call = h.tripTurn(2, otherUserTab, "it is in Example Town now, and this is its tab")
	result = toolCaller(t, call)("trip_update", `{"id":"`+gone.ID+`","destination":"Example Town","use_this_tab":true}`)
	if result.IsError || !strings.Contains(result.Text, "Updated the trip") || strings.Contains(result.Text, "The tab it had") {
		t.Fatalf("change with a move of a trip without a tab = %+v", result)
	}
	summary = h.wantSummary(otherUserTab, gone.ID, before)
	if edit := h.nextEdit(); edit.messageID != summary.messageID || !strings.Contains(edit.text, "(Example Town)") {
		t.Fatalf("summary after the change = %+v", edit)
	}
	h.endTripTurn(call, 2, otherUserTab, true)
	h.expectNoNewTab()
	h.expectNoTopicEdit()
}

// After outside content a move is only proposed, its card says so, and a tab that another trip
// took before the Confirm is refused with nothing changed.
func TestAProposedMoveIsCheckedAgainOnConfirm(t *testing.T) {
	t.Parallel()
	h := newTripTakeHarness(t)
	planned := h.addTrip()
	old := h.wantTripTab("Example trip", planned.ID)
	h.wantSummary(old, planned.ID, h.summaryOf(planned.ID))

	call := h.tripTurn(1, userTab, "the mail says this is the tab for it")
	readOutside(t, call)
	if result := toolCaller(t, call)("trip_update", `{"id":"`+planned.ID+`","use_this_tab":true}`); result.IsError || !strings.Contains(result.Text, approval.ProposedText) {
		t.Fatalf("move after outside content = %+v, want a proposal", result)
	}
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.nextMessage()
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	if proposal.threadID != userTab || !strings.Contains(proposal.text, `Move the trip "Example trip" to this tab.`) {
		t.Fatalf("proposal = %+v, want its card to say the trip moves here", proposal)
	}
	h.waitMessageRecorded(proposal.messageID)

	other := h.exampleDraft("")
	other.Title, other.Bookings = "Second trip", nil
	second := h.addTripFrom(other)
	secondTab := h.wantTripTab("Second trip", second.ID)
	secondSummary := h.wantSummary(secondTab, second.ID, h.summaryOf(second.ID))
	if moved, err := h.trips.UseTab(h.ctx, "alice", second.ID, userTab); err != nil || moved.Rejection != "" {
		t.Fatalf("move of the second trip = %+v %v", moved, err)
	}
	h.wantMovedNote(secondSummary, "Second trip")
	h.wantSummary(userTab, second.ID, h.summaryOf(second.ID))

	accept, _ := approvalButtons(t, proposal.keyboard)
	h.push(tabPress(2, userTab, proposal.messageID, accept))
	h.wantAnswer(2)
	if outcome := h.nextEdit(); outcome.messageID != proposal.messageID ||
		!strings.Contains(outcome.text, `This tab is the trip "Second trip"'s, which has not ended, so it cannot be another trip's tab. Nothing was changed.`) {
		t.Fatalf("proposal after Confirm = %+v, want the move refused", outcome)
	}
	h.continueTurn(h.runtime, 2)
	if h.tripTab(planned.ID) != old || h.tripTab(second.ID) != userTab {
		t.Fatalf("after the refused Confirm the trips are at %d and %d", h.tripTab(planned.ID), h.tripTab(second.ID))
	}
	h.expectNoNewTab()
}

// /new in a taken tab starts another conversation about the trip, and /close removes the tab, which
// the trip then gets again as a tab of its own.
func TestNewAndCloseInATakenTab(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, true)
	planned := h.addTrip()
	old := h.wantTripTab("Example trip", planned.ID)
	first := h.wantSummary(old, planned.ID, h.summaryOf(planned.ID))
	h.moveTrip(1, userTab, planned.ID)
	h.wantMovedNote(first, "Example trip")
	moved, _ := h.requestSession(1)

	h.push(tabMessage(2, userTab, "/new"))
	if reset := h.nextMessage(); reset.threadID != userTab || reset.text != localetest.English(NewSessionReply) {
		t.Fatalf("/new = %+v", reset)
	}
	call := h.tripTurn(3, userTab, "and the hotel?")
	if !strings.Contains(call.request.Message, "about the user's trip \"Example trip\"") {
		t.Fatalf("the turn after /new is not about the trip:\n%s", call.request.Message)
	}
	h.endTripTurn(call, 3, userTab, false)
	if session, subject := h.requestSession(3); session == moved || subject != thread.TripDestination(planned.ID) {
		t.Fatalf("after /new the tab holds %s about %q, want a new conversation about the trip", session, subject)
	}

	h.push(tabMessage(4, userTab, "/close"))
	prompt := h.nextMessage()
	if prompt.threadID != userTab || prompt.text != localetest.English(CloseTabPrompt) {
		t.Fatalf("/close = %+v", prompt)
	}
	h.push(tabPress(5, userTab, prompt.messageID, "tabs:close:50"))
	h.wantAnswer(5)
	if deleted := receive(t, h.ctx, h.tabs.deleted); deleted != userTab {
		t.Fatalf("deleted tab %d, want %d", deleted, userTab)
	}
	if outcome := h.nextMessage(); outcome.threadID != 0 || outcome.text != localetest.English(TabClosedReply) {
		t.Fatalf("close outcome = %+v", outcome)
	}
	if _, kept := h.registryTab(userTab); kept || h.summaryMessage(planned.ID) != 0 {
		t.Fatalf("the closed tab was kept (%v) with summary %d", kept, h.summaryMessage(planned.ID))
	}
	if err := h.notifier.Send(h.ctx, "alice", planned.ID, "news"); err != nil {
		t.Fatal(err)
	}
	again := h.wantTripTab("Example trip", planned.ID)
	h.wantSummary(again, planned.ID, h.summaryOf(planned.ID))
	if message := h.nextMessage(); message.threadID != again || message.text != "news" {
		t.Fatalf("news after the close = %+v, want it in the trip's new tab %d", message, again)
	}
}

// A summary Telegram did not take when the trip moved is sent and pinned with the trip's next
// message.
func TestASummaryLostInTheMoveComesWithTheNextMessage(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, true)
	planned := h.addTrip()
	old := h.wantTripTab("Example trip", planned.ID)
	first := h.wantSummary(old, planned.ID, h.summaryOf(planned.ID))

	call := h.tripTurn(1, userTab, "this is the tab for my trip")
	h.mu.Lock()
	h.sendStatuses = []int{http.StatusInternalServerError}
	h.mu.Unlock()
	if result := toolCaller(t, call)("trip_update", `{"id":"`+planned.ID+`","use_this_tab":true}`); result.IsError {
		t.Fatalf("move = %+v", result)
	}
	if lost := h.nextMessage(); lost.threadID != userTab || lost.status != http.StatusInternalServerError || lost.text != h.summaryOf(planned.ID) {
		t.Fatalf("summary send = %+v, want it refused", lost)
	}
	h.wantMovedNote(first, "Example trip")
	h.endTripTurn(call, 1, userTab, true)
	if h.tripTab(planned.ID) != userTab || h.summaryMessage(planned.ID) != 0 {
		t.Fatalf("after the lost summary the trip is at %d with summary %d", h.tripTab(planned.ID), h.summaryMessage(planned.ID))
	}

	if err := h.notifier.Send(h.ctx, "alice", planned.ID, "news"); err != nil {
		t.Fatal(err)
	}
	h.wantSummary(userTab, planned.ID, h.summaryOf(planned.ID))
	if message := h.nextMessage(); message.threadID != userTab || message.text != "news" {
		t.Fatalf("news = %+v, want it in the taken tab after the summary", message)
	}
	h.expectNoNewTab()
}

// Taking the tab of a trip that is over holds that trip's lock as well as the moving trip's, so
// nothing of the trip that is over is written to the tab meanwhile.
func TestTakingAnEndedTripsTabHoldsBothTrips(t *testing.T) {
	t.Parallel()
	h := newTripTabHarness(t, true)
	now := h.clock.Now()
	visit := h.exampleDraft("")
	visit.Title, visit.Bookings = "Example visit", nil
	visit.StartsAt, visit.EndsAt = now.Add(time.Hour), now.Add(2*time.Hour)
	ended := h.addTripFrom(visit)
	endedTab := h.wantTripTab("Example visit", ended.ID)
	h.wantSummary(endedTab, ended.ID, h.summaryOf(ended.ID))
	planned := h.addTrip()
	h.wantSummary(h.wantTripTab("Example trip", planned.ID), planned.ID, h.summaryOf(planned.ID))
	h.clock.Set(t, "2026-09-28T11:00:00+02:00")

	gate := make(chan struct{})
	h.mu.Lock()
	h.sendGate = gate
	h.mu.Unlock()
	taken := make(chan error, 1)
	go func() {
		move, err := h.tabSet.TakeTripTab(h.ctx, "alice", 101, planned.ID, endedTab, false)
		if err == nil && move.Refusal != "" {
			err = errors.New(move.Refusal)
		}
		taken <- err
	}()
	// The summary is sent in the taken tab while the take holds its locks.
	if summary := h.nextMessage(); summary.threadID != endedTab {
		t.Fatalf("summary = %+v, want it in the taken tab", summary)
	}
	lock, _ := h.tabSet.feedLocks.LoadOrStore("alice\x00"+thread.TripDestination(ended.ID), &sync.Mutex{})
	if lock.(*sync.Mutex).TryLock() {
		lock.(*sync.Mutex).Unlock()
		t.Fatal("the ended trip's tab work could run while its tab was being taken")
	}
	h.mu.Lock()
	h.sendGate = nil
	h.mu.Unlock()
	close(gate)
	if err := <-taken; err != nil {
		t.Fatal(err)
	}
	if !lock.(*sync.Mutex).TryLock() {
		t.Fatal("the ended trip's lock was kept after the take")
	}
	lock.(*sync.Mutex).Unlock()
}

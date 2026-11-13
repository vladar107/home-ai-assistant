package telegram

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// watchIconID is the magnifying glass among the icons the simulator offers in these tests.
const watchIconID = "5000000000000000007"

const (
	kettleCheck     = "Look up the price of the blue kettle at the example shop."
	kettleCondition = "it costs less than 30 euros"
)

// watchChecks answers each watch's check with the observation set for it, or fails it, and answers
// a watch nothing was set for with a state that triggers nothing.
type watchChecks struct {
	mu           sync.Mutex
	observations map[string]watch.Observation
	failing      map[string]bool
}

func (c *watchChecks) set(watchID string, observation watch.Observation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observations[watchID] = observation
	delete(c.failing, watchID)
}

func (c *watchChecks) fail(watchID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failing[watchID] = true
}

func (c *watchChecks) Evaluate(_ context.Context, _ string, checked watch.Watch, _ watch.Occurrence) (watch.Observation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failing[checked.ID] {
		return watch.Observation{}, errors.New("the check failed")
	}
	if observation, set := c.observations[checked.ID]; set {
		return observation, nil
	}
	return watch.Observation{State: "normal"}, nil
}

// triggered is a check that found what its watch waits for, in the given state.
func triggered(state, headline string) watch.Observation {
	return watch.Observation{State: state, Triggered: true, Headline: headline}
}

type watchTabHarness struct {
	*projectTabHarness
	checks *watchChecks
}

// newWatchTabHarness runs Alice's and Bob's chats with watches whose checks the test answers and
// whose cycles it runs itself, with tabs on or off.
func newWatchTabHarness(t *testing.T, topics bool) *watchTabHarness {
	t.Helper()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	checks := &watchChecks{observations: make(map[string]watch.Observation), failing: make(map[string]bool)}
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, watches: checks, agentWatches: checks, topics: topics})
	h.tabs.icons = append(slices.Clone(testTopicIcons), TopicIcon{Emoji: watchTabIcon, CustomEmojiID: watchIconID})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	return &watchTabHarness{projectTabHarness: &projectTabHarness{conversationHarness: h, runtime: runtime, clock: clock}, checks: checks}
}

// kettleWatch is an agent watch that checks all day, every day, and tells of each trigger given.
func kettleWatch(name string, triggers ...string) watch.Spec {
	spec := agentWatch()
	spec.Target.Name = name
	spec.Check = &watch.Check{Instruction: kettleCheck, Condition: kettleCondition, IntervalMinutes: 60}
	spec.Triggers = triggers
	return spec
}

// proposeArguments are watch_propose's arguments for kettleWatch.
func proposeArguments(name string, triggers ...string) string {
	return `{"kind":"agent","name":"` + name + `","check":{"instruction":"` + kettleCheck + `","condition":"` + kettleCondition +
		`","interval_minutes":60},"triggers":["` + strings.Join(triggers, `","`) + `"]}`
}

func (h *watchTabHarness) storedWatch(id string) watch.Watch {
	h.t.Helper()
	stored, found, err := h.store.LoadWatch(h.ctx, "alice", id)
	if err != nil || !found {
		h.t.Fatalf("watch %s = %v %v", id, found, err)
	}
	return stored
}

// wantWatchTab takes the next tab the bot made and checks it is the watch's, named after it,
// recorded as fixed, and given the magnifying glass icon.
func (h *watchTabHarness) wantWatchTab(name, watchID string) int64 {
	h.t.Helper()
	created := receive(h.t, h.ctx, h.tabs.created)
	if created.name != name || created.chatID != 101 {
		h.t.Fatalf("made tab %+v, want %q", created, name)
	}
	if edit := receive(h.t, h.ctx, h.tabs.topicEdits); edit != (editedTopic{threadID: created.threadID, iconID: watchIconID, iconed: true}) {
		h.t.Fatalf("watch tab edit = %+v, want only its icon", edit)
	}
	threadID, found, err := h.store.SubjectTab(h.ctx, "alice", thread.WatchDestination(watchID))
	if err != nil || !found || threadID != created.threadID {
		h.t.Fatalf("registry has the watch at %d %v %v, want %d", threadID, found, err, created.threadID)
	}
	if tab, _ := h.registryTab(created.threadID); tab.NamedBy != thread.NamedByFixed || tab.Name != name {
		h.t.Fatalf("watch tab in the registry = %+v", tab)
	}
	return created.threadID
}

// wantWatchSummary takes the next message and checks it is the watch's summary as the watch reads
// now, in its tab, plain and without link previews, pinned silently and recorded.
func (h *watchTabHarness) wantWatchSummary(threadID int64, watchID string) sentMessage {
	h.t.Helper()
	summary := h.nextMessage()
	if pin := receive(h.t, h.ctx, h.tabs.pins); pin != (pinnedMessage{chatID: 101, messageID: summary.messageID, silent: true}) {
		h.t.Fatalf("pin = %+v, want the summary pinned silently", pin)
	}
	stored := h.storedWatch(watchID)
	if text := watch.Summary(stored); summary.threadID != threadID || summary.text != text || summary.parseMode != "" || summary.preview != `{"is_disabled":true}` || len(summary.keyboard) != 0 {
		h.t.Fatalf("summary = %+v, want in tab %d as plain text:\n%s", summary, threadID, text)
	}
	if stored.SummaryMessageID != summary.messageID {
		h.t.Fatalf("recorded summary %d, want %d", stored.SummaryMessageID, summary.messageID)
	}
	return summary
}

// wantSummaryEdit takes the next edit and checks it brings the summary up to what the watch reads
// now.
func (h *watchTabHarness) wantSummaryEdit(summary sentMessage, watchID string) string {
	h.t.Helper()
	edit, text := h.nextEdit(), watch.Summary(h.storedWatch(watchID))
	if edit.messageID != summary.messageID || edit.text != text {
		h.t.Fatalf("edit = %+v, want the summary %d to read:\n%s", edit, summary.messageID, text)
	}
	return text
}

// watchByTool has Alice ask for an agent watch in the main view, which is set up at once, and
// returns its ID with the tab and the summary it got.
func (h *watchTabHarness) watchByTool(updateID int64, name string, triggers ...string) (string, int64, sentMessage) {
	h.t.Helper()
	call := h.startTurn(updateID, 0, "tell me when it gets cheaper")
	id := h.wantDirect(toolCaller(h.t, call)("watch_propose", proposeArguments(name, triggers...)), "watch_propose")
	tab := h.wantWatchTab(name, id)
	summary := h.wantWatchSummary(tab, id)
	h.endTurn(call, updateID, 0, true)
	return id, tab, summary
}

// cycleAt moves the clock to the local time of the test's Monday and runs one watch cycle.
func (h *watchTabHarness) cycleAt(local string) {
	h.t.Helper()
	h.clock.Set(h.t, "2026-09-28T"+local+":00+02:00")
	h.watches.Cycle(h.ctx)
}

// nextAlert takes the next message, which must be a message of the watch with its buttons, and
// waits until the watch service has kept it, as it does just after sending.
func (h *watchTabHarness) nextAlert(watchID string) sentMessage {
	h.t.Helper()
	return h.nextAlerts(watchID)[watchID]
}

// nextAlerts takes one message of each watch, in whatever order one cycle sent them, and waits
// until the watch service has kept each.
func (h *watchTabHarness) nextAlerts(watchIDs ...string) map[string]sentMessage {
	h.t.Helper()
	alerts := make(map[string]sentMessage)
	for range watchIDs {
		alert := h.nextMessage()
		buttons := buttonData(alert.keyboard)
		watchID, isAlert := "", len(buttons) == 2
		if isAlert {
			watchID, isAlert = strings.CutPrefix(buttons[0], "watch:mute:")
		}
		if _, taken := alerts[watchID]; !isAlert || taken || !slices.Contains(watchIDs, watchID) || buttons[1] != "watch:pause:"+watchID {
			h.t.Fatalf("message = %+v, want one of each of the watches %v", alert, watchIDs)
		}
		h.waitKept(watchID, alert)
		alerts[watchID] = alert
	}
	return alerts
}

func (h *watchTabHarness) waitKept(watchID string, alert sentMessage) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for h.countRows(`SELECT count(*) FROM watch_alert_messages WHERE watch_id = ? AND telegram_message_id = ?`, watchID, alert.messageID) == 0 {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("the message %d of watch %s was never kept", alert.messageID, watchID)
		}
	}
	if landed := h.landedThread(`SELECT thread_id FROM watch_alert_messages WHERE telegram_message_id = ?`, alert.messageID); landed != alert.threadID {
		h.t.Fatalf("message %d was kept in tab %d, sent to %d", alert.messageID, landed, alert.threadID)
	}
}

func (h *watchTabHarness) noWatchTab(watchID string) {
	h.t.Helper()
	if tab, found, err := h.store.SubjectTab(h.ctx, "alice", thread.WatchDestination(watchID)); err != nil || found {
		h.t.Fatalf("watch %s has tab %d %v %v, want none", watchID, tab, found, err)
	}
	h.expectNoNewTab()
}

// An agent watch gets its own tab when it is set up, with its summary pinned. Every message of the
// watch lands there: its alert, the notice that the condition ended, and the notice that it cannot
// be checked. Another agent watch has another tab, a train watch keeps the Watches tab, and none of
// what is watched reaches the logs.
func TestEachAgentWatchHasATabOfItsOwn(t *testing.T) {
	t.Parallel()
	h := newWatchTabHarness(t, true)
	kettle, kettleTab, summary := h.watchByTool(1, "Kettle price", watch.TriggerEnter, watch.TriggerClear)
	if want := strings.Join([]string{
		"Kettle price",
		"Checks: " + kettleCheck,
		"Condition: " + kettleCondition,
		"Days: every day",
		"Window: all day (Europe/Amsterdam), checked every 60 minutes, each check a model turn on your own subscription",
		"Tells you about: the condition starting to hold, the condition ending",
	}, "\n"); summary.text != want {
		t.Fatalf("summary:\n%s\nwant:\n%s", summary.text, want)
	}
	release, releaseTab, _ := h.watchByTool(2, "Release date", watch.TriggerEnter)
	if releaseTab == kettleTab {
		t.Fatalf("both watches share tab %d", kettleTab)
	}
	const train = "00000000000000a1"
	h.createWatch("alice", train, trainWatch())

	h.checks.set(kettle, triggered("25", "The kettle costs 25 euros."))
	h.checks.set(release, triggered("1 December", "The next release is on 1 December."))
	h.checks.set(train, triggered("cancelled", "It is cancelled."))
	h.watches.Cycle(h.ctx)
	alerts := h.nextAlerts(kettle, release, train)
	if alert := alerts[kettle]; alert.threadID != kettleTab || !strings.Contains(alert.text, "The kettle costs 25 euros.") {
		t.Fatalf("kettle alert = %+v, want it in its tab %d", alert, kettleTab)
	}
	if alert := alerts[release]; alert.threadID != releaseTab || !strings.Contains(alert.text, "The next release is on 1 December.") {
		t.Fatalf("release alert = %+v, want it in its tab %d", alert, releaseTab)
	}
	watches := h.wantFeedTab("Watches", thread.FeedWatches)
	if alert := alerts[train]; alert.threadID != watches || !strings.Contains(alert.text, "It is cancelled.") {
		t.Fatalf("train alert = %+v, want it in the Watches tab %d", alert, watches)
	}
	h.noWatchTab(train)

	h.checks.set(kettle, watch.Observation{State: "normal"})
	h.cycleAt("08:05")
	if ended := h.nextAlert(kettle); ended.threadID != kettleTab || !strings.Contains(ended.text, "No longer the case.") {
		t.Fatalf("notice that the condition ended = %+v, want it in the kettle's tab %d", ended, kettleTab)
	}

	h.checks.fail(release)
	for _, local := range []string{"08:10", "08:15", "08:20"} {
		h.cycleAt(local)
	}
	if failure := h.nextAlert(release); failure.threadID != releaseTab || !strings.Contains(failure.text, "I can't check this right now.") {
		t.Fatalf("notice of the failing check = %+v, want it in the release's tab %d", failure, releaseTab)
	}
	h.expectNoNewTab()
	if h.storedWatch(kettle).SummaryMessageID != summary.messageID {
		t.Fatal("the kettle's summary was sent again")
	}
	for _, private := range []string{"kettle", "release", "euros"} {
		if strings.Contains(strings.ToLower(h.logs.String()), private) {
			t.Fatalf("%q reached the logs:\n%s", private, h.logs.String())
		}
	}
}

// The summary and the tab's name follow every change to the watch: a new name renames the tab and
// edits the summary, an Undo puts both back, a pause and a travel pause are said in the summary,
// and a mute for today, which ends without a write, leaves it alone. A paused watch keeps its tab.
func TestAWatchsSummaryAndTabNameFollowTheWatch(t *testing.T) {
	t.Parallel()
	h := newWatchTabHarness(t, true)
	id, tab, summary := h.watchByTool(1, "Kettle price", watch.TriggerEnter, watch.TriggerChange)

	call := h.startTurn(2, 0, "it is a teapot, and I travel a lot")
	renamed := h.wantDirect(toolCaller(t, call)("watch_update", `{"watch_id":"`+id+`","name":"Teapot price","pause_while_traveling":true}`), "watch_update")
	if edit := receive(t, h.ctx, h.tabs.topicEdits); edit != (editedTopic{threadID: tab, name: "Teapot price", named: true}) {
		t.Fatalf("tab edit after the new name = %+v", edit)
	}
	if text := h.wantSummaryEdit(summary, id); !strings.HasPrefix(text, "Teapot price\n") || !strings.HasSuffix(text, "\nPauses while you travel.") {
		t.Fatalf("summary after the change:\n%s", text)
	}
	h.endTurn(call, 2, 0, true)
	if registered, _ := h.registryTab(tab); registered.Name != "Teapot price" || registered.NamedBy != thread.NamedByFixed {
		t.Fatalf("renamed tab in the registry = %+v", registered)
	}

	undid, err := h.approvals.Undo(h.ctx, "alice", renamed)
	if err != nil || !undid.Owned {
		t.Fatalf("undo = %+v %v", undid, err)
	}
	if edit := receive(t, h.ctx, h.tabs.topicEdits); edit != (editedTopic{threadID: tab, name: "Kettle price", named: true}) {
		t.Fatalf("tab edit after Undo = %+v", edit)
	}
	if text := h.wantSummaryEdit(summary, id); !strings.HasPrefix(text, "Kettle price\n") || strings.Contains(text, "travel") {
		t.Fatalf("summary after Undo:\n%s", text)
	}
	// The "Done" message of the change loses its Undo button.
	h.nextEdit()

	h.checks.set(id, triggered("25", "The kettle costs 25 euros."))
	h.watches.Cycle(h.ctx)
	alert := h.nextAlert(id)
	if alert.threadID != tab {
		t.Fatalf("alert = %+v, want it in the watch's tab %d", alert, tab)
	}
	h.push(tabPress(3, tab, alert.messageID, "watch:mute:"+id))
	h.wantAnswer(3)
	// A summary edit would come before the alert's own, since the watch's change makes it.
	if edit := h.nextEdit(); edit.messageID != alert.messageID || !strings.Contains(edit.text, WatchMutedReply) {
		t.Fatalf("edit after Mute today = %+v, want only the alert's", edit)
	}

	h.write(`UPDATE watches SET muted_on = NULL WHERE id = ?`, id)
	h.checks.set(id, triggered("24", "The kettle costs 24 euros."))
	h.cycleAt("08:05")
	second := h.nextAlert(id)
	h.push(tabPress(4, tab, second.messageID, "watch:pause:"+id))
	h.wantAnswer(4)
	if text := h.wantSummaryEdit(summary, id); !strings.HasSuffix(text, "\nPaused; resume it in /jobs.") {
		t.Fatalf("summary of the paused watch:\n%s", text)
	}
	if edit := h.nextEdit(); edit.messageID != second.messageID || !strings.Contains(edit.text, WatchPausedReply) {
		t.Fatalf("edit after Pause = %+v", edit)
	}
	h.checks.set(id, triggered("23", "The kettle costs 23 euros."))
	h.cycleAt("08:10")

	if resumed, err := h.watches.SetEnabled(h.ctx, "alice", id, true); err != nil || !resumed {
		t.Fatalf("resume = %v %v", resumed, err)
	}
	if text := h.wantSummaryEdit(summary, id); strings.Contains(text, "Paused") {
		t.Fatalf("summary of the resumed watch:\n%s", text)
	}
	h.cycleAt("08:15")
	if alert := h.nextAlert(id); alert.threadID != tab || !strings.Contains(alert.text, "The kettle costs 23 euros.") {
		t.Fatalf("alert after the pause = %+v, want the one found after resuming, in tab %d", alert, tab)
	}
	if h.storedWatch(id).SummaryMessageID != summary.messageID {
		t.Fatal("the summary was sent again")
	}
	h.expectNoNewTab()
}

// A removed watch's tab stays with its messages. Nothing is sent or edited there, and a
// conversation started there is about nothing in particular. Undoing the removal brings the watch
// back into the same tab with a fresh summary.
func TestARemovedWatchKeepsItsTab(t *testing.T) {
	t.Parallel()
	h := newWatchTabHarness(t, true)
	id, tab, summary := h.watchByTool(1, "Kettle price", watch.TriggerEnter)

	call := h.startTurn(2, 0, "stop watching the kettle")
	removed := h.wantDirect(toolCaller(t, call)("watch_remove", `{"watch_id":"`+id+`"}`), "watch_remove")
	h.endTurn(call, 2, 0, true)
	if registered, found := h.registryTab(tab); !found || registered.Subject != thread.WatchDestination(id) {
		t.Fatalf("the removed watch's tab = %+v %v, want it kept", registered, found)
	}

	call = h.startTurn(3, tab, "what was this about?")
	if strings.Contains(call.request.Message, "This conversation is about") {
		t.Fatalf("a turn in a removed watch's tab was given a subject:\n%s", call.request.Message)
	}
	h.endTurn(call, 3, tab, false)
	if _, subject := h.sessionOf(3); subject != "" {
		t.Fatalf("the conversation in a removed watch's tab is about %q", subject)
	}

	undid, err := h.approvals.Undo(h.ctx, "alice", removed)
	if err != nil || !undid.Owned {
		t.Fatalf("undo = %+v %v", undid, err)
	}
	if again := h.wantWatchSummary(tab, id); again.messageID == summary.messageID {
		t.Fatal("the watch that came back kept the summary of the removed one")
	}
	h.nextEdit()
	h.expectNoNewTab()
	select {
	case edit := <-h.edits:
		t.Fatalf("unexpected edit %+v", edit)
	default:
	}
}

// A message that follows an alert still in a removed watch's tab is about that alert's watch, as a
// reply to the alert of a removed watch is in the Watches tab.
func TestAMessageAfterARemovedWatchsAlertIsAboutThatWatch(t *testing.T) {
	t.Parallel()
	h := newWatchTabHarness(t, true)
	id, tab, _ := h.watchByTool(1, "Kettle price", watch.TriggerEnter)
	h.checks.set(id, triggered("25", "The kettle costs 25 euros."))
	h.watches.Cycle(h.ctx)
	if alert := h.nextAlert(id); alert.threadID != tab {
		t.Fatalf("alert = %+v, want it in the watch's tab %d", alert, tab)
	}
	if deleted, err := h.watches.Delete(h.ctx, "alice", id); err != nil || !deleted {
		t.Fatalf("delete = %v %v", deleted, err)
	}

	call := h.startTurn(3, tab, "where was that?")
	if line := `[system] This conversation is about the user's watch "Kettle price" (id ` + id + `); watch_list shows it in full.`; !strings.Contains(call.request.Message, line) {
		t.Fatalf("turn after the removed watch's alert:\n%s\nwant the line:\n%s", call.request.Message, line)
	}
	h.endTurn(call, 3, tab, false)
	if _, subject := h.sessionOf(3); subject != thread.WatchDestination(id) {
		t.Fatalf("the conversation after the removed watch's alert is about %q", subject)
	}
}

// A watch that exists already gets its tab with its next message. While the tab cannot be made the
// message goes to the Watches tab; once Telegram no longer has the tab the client leaves the
// message in the main view, and the next one makes the tab again with a fresh summary.
func TestAWatchWhoseTabCannotBeHadFallsBackAndGetsItAgain(t *testing.T) {
	t.Parallel()
	h := newWatchTabHarness(t, true)
	const train, id = "00000000000000a1", "00000000000000c1"
	h.createWatch("alice", train, trainWatch())
	h.checks.set(train, triggered("cancelled", "It is cancelled."))
	h.watches.Cycle(h.ctx)
	watches := h.wantFeedTab("Watches", thread.FeedWatches)
	h.nextAlert(train)

	h.createWatch("alice", id, kettleWatch("Kettle price", watch.TriggerEnter, watch.TriggerChange))
	h.mu.Lock()
	h.tabs.createStatus = 429
	h.mu.Unlock()
	h.checks.set(id, triggered("25", "The kettle costs 25 euros."))
	h.cycleAt("08:05")
	if alert := h.nextAlert(id); alert.threadID != watches {
		t.Fatalf("alert of a watch whose tab cannot be made = %+v, want it in the Watches tab %d", alert, watches)
	}
	if !strings.Contains(h.logs.String(), "msg=telegram.watch_tab.failed principal_id=alice watch_id="+id+" stage=create") {
		t.Fatalf("failed creation not logged:\n%s", h.logs.String())
	}
	h.mu.Lock()
	h.tabs.createStatus = 0
	h.mu.Unlock()

	h.checks.set(id, triggered("24", "The kettle costs 24 euros."))
	h.cycleAt("08:10")
	tab := h.wantWatchTab("Kettle price", id)
	first := h.wantWatchSummary(tab, id)
	if alert := h.nextAlert(id); alert.threadID != tab {
		t.Fatalf("alert = %+v, want it in the watch's new tab %d", alert, tab)
	}

	h.goneTab(tab)
	h.ageTab(tab)
	h.checks.set(id, triggered("23", "The kettle costs 23 euros."))
	h.cycleAt("08:15")
	h.wantRefused("sendMessage", tab)
	if alert := h.nextAlert(id); alert.threadID != 0 {
		t.Fatalf("alert to the gone tab = %+v, want it in the main view", alert)
	}
	if h.storedWatch(id).SummaryMessageID != 0 {
		t.Fatal("the summary outlived its tab")
	}

	h.checks.set(id, triggered("22", "The kettle costs 22 euros."))
	h.cycleAt("08:20")
	again := h.wantWatchTab("Kettle price", id)
	summary := h.wantWatchSummary(again, id)
	if again == tab || summary.messageID == first.messageID {
		t.Fatalf("the watch kept tab %d or summary %d", again, summary.messageID)
	}
	if alert := h.nextAlert(id); alert.threadID != again {
		t.Fatalf("alert after the tab was gone = %+v, want it in the new tab %d", alert, again)
	}
}

// With tabs off, an agent watch gets no tab or summary, and its alerts reach the main view.
func TestAgentWatchesWithTabsOffStayInTheMainView(t *testing.T) {
	t.Parallel()
	h := newWatchTabHarness(t, false)
	call := h.startTurn(1, 0, "tell me when it gets cheaper")
	id := h.wantDirect(toolCaller(t, call)("watch_propose", proposeArguments("Kettle price", watch.TriggerEnter)), "watch_propose")
	h.endTurn(call, 1, 0, true)
	h.checks.set(id, triggered("25", "The kettle costs 25 euros."))
	h.watches.Cycle(h.ctx)
	if alert := h.nextAlert(id); alert.threadID != 0 {
		t.Fatalf("alert with tabs off = %+v", alert)
	}
	if paused, err := h.watches.SetEnabled(h.ctx, "alice", id, false); err != nil || !paused {
		t.Fatalf("pause = %v %v", paused, err)
	}
	h.noWatchTab(id)
	if h.storedWatch(id).SummaryMessageID != 0 {
		t.Fatal("a summary was recorded with tabs off")
	}
}

// Writing in a watch's tab is a conversation about the watch, named by its ID before its first
// alert and by its name after it, with no "About" line for the tab's own watch.
func TestAWatchsTabTalksAboutTheWatch(t *testing.T) {
	t.Parallel()
	h := newWatchTabHarness(t, true)
	id, tab, _ := h.watchByTool(1, "Kettle price", watch.TriggerEnter)

	call := h.startTurn(2, tab, "how often do you look?")
	if line := "[system] This conversation is about the user's watch (id " + id + "); watch_list shows it in full."; !strings.Contains(call.request.Message, line) {
		t.Fatalf("turn in the watch's tab:\n%s\nwant the line:\n%s", call.request.Message, line)
	}
	h.endTurn(call, 2, tab, false)
	session, subject := h.sessionOf(2)
	if subject != thread.WatchDestination(id) {
		t.Fatalf("the tab's conversation is about %q", subject)
	}

	h.checks.set(id, triggered("25", "The kettle costs 25 euros."))
	h.watches.Cycle(h.ctx)
	if alert := h.nextAlert(id); alert.threadID != tab {
		t.Fatalf("alert = %+v, want it in the watch's tab %d", alert, tab)
	}
	call = h.startTurn(3, tab, "is that a good price?")
	if line := `[system] This conversation is about the user's watch "Kettle price" (id ` + id + `); watch_list shows it in full.`; !strings.Contains(call.request.Message, line) ||
		!strings.Contains(call.request.Message, "The kettle costs 25 euros.") {
		t.Fatalf("turn after the alert:\n%s\nwant the line:\n%s", call.request.Message, line)
	}
	h.endTurn(call, 3, tab, false)
	if again, subject := h.sessionOf(3); again != session || subject != thread.WatchDestination(id) {
		t.Fatalf("the turn after the alert ran in conversation %s about %q, want %s about the watch", again, subject, session)
	}
}

// A conversation about the watch that is open in the Watches tab, started by replying to an alert
// sent there before the watch had a tab, keeps the watch. A conversation that then starts in the
// watch's new tab is about nothing in particular, making the tab moved nothing, and a reply to an
// alert in the watch's tab is refused, naming the Watches tab. Once that conversation has ended,
// the same reply is about the watch.
func TestAWatchsTabLeavesAConversationOpenInTheWatchesTab(t *testing.T) {
	t.Parallel()
	h := newWatchTabHarness(t, true)
	const train, id = "00000000000000a1", "00000000000000c1"
	h.createWatch("alice", train, trainWatch())
	h.checks.set(train, triggered("cancelled", "It is cancelled."))
	h.watches.Cycle(h.ctx)
	watches := h.wantFeedTab("Watches", thread.FeedWatches)
	h.nextAlert(train)

	h.createWatch("alice", id, kettleWatch("Kettle price", watch.TriggerEnter))
	if err := h.store.SaveWatchAlertMessage(h.ctx, watch.AlertMessage{
		PrincipalID: "alice", MessageID: 9200, ThreadID: watches, WatchID: id, WatchLabel: "Kettle price",
		Text: "The kettle costs 29 euros.", External: true,
	}, 200); err != nil {
		t.Fatal(err)
	}
	h.push(replyInTab(1, watches, "where did you see that?", 9200))
	line := `[system] This conversation is about the user's watch "Kettle price" (id ` + id + `); watch_list shows it in full.`
	if message, answer := h.answerTurn(h.runtime, 1, "On the shop's page."); !strings.Contains(message, line) || answer.threadID != watches {
		t.Fatalf("reply to the earlier alert = %q, answered %+v", message, answer)
	}
	earlier, subject := h.sessionOf(1)
	if subject != thread.WatchDestination(id) {
		t.Fatalf("the reply's conversation is about %q", subject)
	}

	h.checks.set(id, triggered("25", "The kettle costs 25 euros."))
	h.cycleAt("08:05")
	tab := h.wantWatchTab("Kettle price", id)
	h.wantWatchSummary(tab, id)
	alert := h.nextAlert(id)
	if alert.threadID != tab {
		t.Fatalf("alert = %+v, want it in the watch's new tab %d", alert, tab)
	}

	call := h.startTurn(2, tab, "and now?")
	if strings.Contains(call.request.Message, "This conversation is about") {
		t.Fatalf("a turn in the watch's tab took the watch from the conversation that holds it:\n%s", call.request.Message)
	}
	h.endTurn(call, 2, tab, false)
	if started, subject := h.sessionOf(2); started == earlier || subject != "" {
		t.Fatalf("the turn in the watch's tab ran in conversation %s about %q", started, subject)
	}
	if count := h.countRows(`SELECT count(*) FROM conversation_sessions WHERE id = ? AND status = 'active' AND thread_id = ? AND subject = ?`, earlier, watches, thread.WatchDestination(id)); count != 1 {
		t.Fatal("the conversation in the Watches tab lost the watch, moved, or ended")
	}

	h.push(replyInTab(3, tab, "is that the lowest?", alert.messageID))
	if refusal := h.nextMessage(); refusal.threadID != tab || refusal.text != SessionOpenIn(watches, "Watches") {
		t.Fatalf("reply to the alert while the Watches tab holds the watch = %+v, want %q", refusal, SessionOpenIn(watches, "Watches"))
	}
	h.waitOffset(4)
	h.push(tabMessage(4, watches, "/new"))
	if reply := h.nextMessage(); reply.threadID != watches || reply.text != NewSessionReply {
		t.Fatalf("/new in the Watches tab = %+v", reply)
	}
	h.push(replyInTab(5, tab, "is that the lowest?", alert.messageID))
	if message, answer := h.answerTurn(h.runtime, 5, "So far."); !strings.Contains(message, line) || answer.threadID != tab {
		t.Fatalf("reply to the alert once the Watches tab let the watch go = %q, answered %+v", message, answer)
	}
	if _, subject := h.sessionOf(5); subject != thread.WatchDestination(id) {
		t.Fatalf("the reply's conversation is about %q", subject)
	}
}

// A reply to the alert of another kind of watch works as before: its conversation is about that
// watch, in the Watches tab, and neither the reply nor a pause gives the watch a tab or a summary.
func TestAReplyToAnotherKindOfWatchStaysInTheWatchesTab(t *testing.T) {
	t.Parallel()
	h := newWatchTabHarness(t, true)
	const train = "00000000000000a1"
	h.createWatch("alice", train, trainWatch())
	h.checks.set(train, triggered("cancelled", "It is cancelled."))
	h.watches.Cycle(h.ctx)
	watches := h.wantFeedTab("Watches", thread.FeedWatches)
	alert := h.nextAlert(train)
	if alert.threadID != watches {
		t.Fatalf("train alert = %+v, want it in the Watches tab %d", alert, watches)
	}

	h.push(replyInTab(1, watches, "is there another train?", alert.messageID))
	message, answer := h.answerTurn(h.runtime, 1, "The next one runs.")
	if !strings.Contains(message, `[system] This conversation is about the user's watch "08:30 train Utrecht Centraal → Amsterdam Centraal" (id `+train+`); watch_list shows it in full.`) ||
		strings.Count(message, "It is cancelled.") != 1 {
		t.Fatalf("turn replying to the train alert = %q", message)
	}
	if answer.threadID != watches || answer.text != "About “08:30 train Utrecht Centraal → Amsterdam Centraal”:\n\nThe next one runs." {
		t.Fatalf("answer = %+v, want it in the Watches tab %d", answer, watches)
	}
	if _, subject := h.sessionOf(1); subject != thread.WatchDestination(train) {
		t.Fatalf("the reply's conversation is about %q", subject)
	}
	h.push(tabMessage(2, watches, "and the one after?"))
	if message, answer := h.answerTurn(h.runtime, 2, "That one too."); !strings.Contains(message, "(id "+train+")") || answer.text != "That one too." {
		t.Fatalf("follow-up = %q, answered %+v", message, answer)
	}

	h.push(tabPress(3, watches, alert.messageID, "watch:pause:"+train))
	h.wantAnswer(3)
	if edit := h.nextEdit(); edit.messageID != alert.messageID || !strings.Contains(edit.text, WatchPausedReply) {
		t.Fatalf("edit after Pause = %+v, want only the alert's", edit)
	}
	h.noWatchTab(train)
	if h.storedWatch(train).SummaryMessageID != 0 {
		t.Fatal("a train watch was given a summary")
	}
}

// A restored agent watch gets a tab and a summary of its own at once, whatever the summary message
// of the watch it was exported from.
func TestARestoredAgentWatchGetsAFreshTab(t *testing.T) {
	t.Parallel()
	source := newExportHarness(t)
	source.seedExport("alice")
	data := source.takeExport(1, 101)

	h := newWatchTabHarness(t, true)
	copyKey(t, source, h.conversationHarness)
	if _, outcome := h.restore(1, 101, data); !strings.Contains(outcome, "Restored") {
		t.Fatalf("restore outcome: %s", outcome)
	}
	tab := h.wantWatchTab("Kettle price", exportAgentWatch)
	if summary := h.wantWatchSummary(tab, exportAgentWatch); summary.messageID == exportWatchSummary {
		t.Fatalf("the restored watch took the exported summary message %d", summary.messageID)
	}
}

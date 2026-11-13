package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// newTripSettingsHarness is newTripTabHarness with Google configured, which the Trips page needs,
// and no account connected.
func newTripSettingsHarness(t *testing.T) (*tripTabHarness, *googlesim.Simulator) {
	t.Helper()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	sim := googleSimulator(t)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, topics: true, google: sim, calendar: calendarSimulator(t, sim)})
	h.tabs.icons = append(slices.Clone(testTopicIcons), TopicIcon{Emoji: "✈️", CustomEmojiID: tripIconID})
	h.provision("alice", "account-alice")
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	return &tripTabHarness{conversationHarness: h, runtime: runtime, clock: clock, trips: h.trips, notifier: h.tripNotifier}, sim
}

// addTripTitled plans a trip without bookings that starts the given time after the example trip
// does, and takes the tab and the summary it gets.
func (h *tripTabHarness) addTripTitled(title string, later time.Duration) (trip.Trip, sentMessage) {
	h.t.Helper()
	draft := h.exampleDraft("")
	draft.Title, draft.Bookings = title, nil
	draft.StartsAt, draft.EndsAt = draft.StartsAt.Add(later), draft.EndsAt.Add(later)
	planned := h.addTripFrom(draft)
	tab := h.wantTripTab(title, planned.ID)
	return planned, h.wantSummary(tab, planned.ID, h.summaryOf(planned.ID))
}

// tripsPageIn sends /settings in the tab, zero for the main view, opens the page that holds the
// Trips section there, and returns the page with the settings message's ID. It consumes two update
// IDs.
func (h *tripTabHarness) tripsPageIn(firstUpdateID, threadID int64) (editedMessage, int64) {
	h.t.Helper()
	request := textUpdate(firstUpdateID, 101, "/settings")
	// A tab the registry knows is addressed as it is; any other is the tab Alice named herself.
	if _, known := h.registryTab(threadID); known {
		request.Message.MessageThreadID = threadID
	} else if threadID > 0 {
		request = tabMessage(firstUpdateID, threadID, "/settings")
	}
	h.push(request)
	overview := h.nextMessage()
	if overview.threadID != threadID || !strings.HasPrefix(overview.text, SettingsTitle+"\n") {
		h.t.Fatalf("settings = %+v, want them in tab %d", overview, threadID)
	}
	return h.pressIn(firstUpdateID+1, threadID, overview.messageID, settingsNavPrefix+":tools"), overview.messageID
}

// pressIn presses a button of the settings message in the tab and returns the refreshed page.
func (h *tripTabHarness) pressIn(updateID, threadID, messageID int64, data string) editedMessage {
	h.t.Helper()
	h.push(tabPress(updateID, threadID, messageID, data))
	h.wantAnswer(updateID)
	return h.settingsEdit(messageID)
}

func (h *tripTabHarness) settingsEdit(messageID int64) editedMessage {
	h.t.Helper()
	edit := h.nextEdit()
	if edit.messageID != messageID || !strings.HasPrefix(edit.text, SettingsTitle) {
		h.t.Fatalf("settings edit = %+v, want message %d", edit, messageID)
	}
	return edit
}

func tripTabButton(tripID string) string { return "trips:tab:" + tripID }

// tripMoveButtons is the buttons of the page that move a trip, in order.
func tripMoveButtons(page editedMessage) []string {
	return slices.DeleteFunc(buttonData(page.keyboard), func(data string) bool { return !strings.HasPrefix(data, "trips:tab:") })
}

func buttonTitled(keyboard InlineKeyboard, data string) string {
	for _, row := range keyboard {
		for _, button := range row {
			if button.CallbackData == data {
				return button.Text
			}
		}
	}
	return ""
}

// The Trips page opened in a tab Alice named offers the tab to her planned trip, although no mail
// can be read and the scan is off. The press moves the trip there with its summary, says so on the
// page, and a later notice of the trip arrives there; a second press changes nothing.
func TestTheTripsPageMovesATripToTheTabItIsOpenedIn(t *testing.T) {
	t.Parallel()
	h, _ := newTripSettingsHarness(t)
	planned := h.addTrip()
	old := h.wantTripTab("Example trip", planned.ID)
	first := h.wantSummary(old, planned.ID, h.summaryOf(planned.ID))

	page, messageID := h.tripsPageIn(1, userTab)
	button := tripTabButton(planned.ID)
	if !strings.Contains(page.text, TripsNeedsGoogleLine) || strings.Contains(page.text, TripsTabLine) ||
		!slices.Equal(tripMoveButtons(page), []string{button}) || buttonTitled(page.keyboard, button) != "Use this tab for Example trip" {
		t.Fatalf("page in Alice's tab = %+v", page)
	}

	h.push(tabPress(3, userTab, messageID, button))
	h.wantAnswer(3)
	h.wantMovedNote(first, "Example trip")
	h.wantSummary(userTab, planned.ID, h.summaryOf(planned.ID))
	page = h.settingsEdit(messageID)
	moved := `Moved "Example trip" to this tab: its summary is pinned here, and its reminders, notices, and news arrive here. The tab it had stays with its messages; you can delete it.`
	if !strings.HasSuffix(page.text, moved) || !strings.Contains(page.text, `"Example trip" uses this tab.`) || len(tripMoveButtons(page)) != 0 {
		t.Fatalf("page after the move = %+v", page)
	}
	if tab := h.tripTab(planned.ID); tab != userTab {
		t.Fatalf("the trip's tab = %d, want %d", tab, userTab)
	}
	if taken, _ := h.registryTab(userTab); taken.Name != "Trip notes" || taken.NamedBy != thread.NamedByUser {
		t.Fatalf("taken tab in the registry = %+v", taken)
	}
	if left, _ := h.registryTab(old); left.Subject != "" {
		t.Fatalf("left tab in the registry = %+v", left)
	}

	if err := h.notifier.Send(h.ctx, "alice", planned.ID, "The flight leaves from another gate."); err != nil {
		t.Fatal(err)
	}
	if notice := h.nextMessage(); notice.threadID != userTab || notice.text != "The flight leaves from another gate." {
		t.Fatalf("trip notice = %+v, want it in the taken tab", notice)
	}

	if page = h.pressIn(4, userTab, messageID, button); !strings.HasSuffix(page.text, `"Example trip" already uses this tab.`) {
		t.Fatalf("page after a second press = %+v", page)
	}
	if recorded := h.summaryMessage(planned.ID); recorded == first.messageID || h.tripTab(planned.ID) != userTab {
		t.Fatalf("a second press moved the trip: summary %d, tab %d", recorded, h.tripTab(planned.ID))
	}
	// The first press is recorded by the time the second is answered, under one command whatever
	// the trip.
	if logs := h.logs.String(); !strings.Contains(logs, "command=trips.tab") || strings.Contains(logs, "command=trips.tab:") {
		t.Fatalf("the press is not recorded as trips.tab:\n%s", logs)
	}
	h.expectNoNewTab()
	h.expectNoTopicEdit()
}

// In the main view and in a feed's tab the page has no button and says where to open the settings
// instead, and a forged press there moves nothing.
func TestTheTripsPageOffersNoTabWhereATripCannotGo(t *testing.T) {
	t.Parallel()
	h, _ := newTripSettingsHarness(t)
	planned := h.addTrip()
	tab := h.wantTripTab("Example trip", planned.ID)
	h.wantSummary(tab, planned.ID, h.summaryOf(planned.ID))
	if err := h.notifier.Send(h.ctx, "alice", "", "a trip notice"); err != nil {
		t.Fatal(err)
	}
	reminders := h.wantFeedTab("Reminders", thread.FeedReminders)
	receive(t, h.ctx, h.tabs.topicEdits)
	h.nextMessage()
	button := tripTabButton(planned.ID)

	page, messageID := h.tripsPageIn(1, 0)
	if !strings.Contains(page.text, TripsTabLine) || len(tripMoveButtons(page)) != 0 {
		t.Fatalf("page in the main view = %+v", page)
	}
	if page = h.pressIn(3, 0, messageID, button); !strings.HasSuffix(page.text, TripsTabLine) {
		t.Fatalf("forged press in the main view = %+v", page)
	}

	page, messageID = h.tripsPageIn(4, reminders)
	if !strings.Contains(page.text, TripsTabLine) || len(tripMoveButtons(page)) != 0 {
		t.Fatalf("page in the Reminders tab = %+v", page)
	}
	refusal := "This is the Reminders tab, which keeps its own kind of messages, so it cannot be a trip's tab."
	if page = h.pressIn(6, reminders, messageID, button); !strings.HasSuffix(page.text, refusal) {
		t.Fatalf("forged press in the Reminders tab = %+v", page)
	}

	if page, _ = h.tripsPageIn(7, tab); strings.Contains(page.text, TripsTabLine) || !strings.Contains(page.text, `"Example trip" uses this tab.`) || len(tripMoveButtons(page)) != 0 {
		t.Fatalf("page in the trip's own tab = %+v", page)
	}
	if h.tripTab(planned.ID) != tab {
		t.Fatalf("a refused press moved the trip to %d", h.tripTab(planned.ID))
	}
	h.expectNoNewTab()
	h.expectNoTopicEdit()
}

// A button pressed after another trip took the tab is refused with the reason, one for a trip that
// is no longer planned says so, and one whose ID no trip could have is no button of the page;
// none moves anything.
func TestAPressAfterTheTabStoppedBeingUsableIsRefused(t *testing.T) {
	t.Parallel()
	h, _ := newTripSettingsHarness(t)
	planned, _ := h.addTripTitled("Example trip", 0)
	tab := h.tripTab(planned.ID)
	other, otherSummary := h.addTripTitled("Second trip", time.Hour)

	page, messageID := h.tripsPageIn(1, userTab)
	if want := []string{tripTabButton(planned.ID), tripTabButton(other.ID)}; !slices.Equal(tripMoveButtons(page), want) {
		t.Fatalf("buttons = %v, want %v", tripMoveButtons(page), want)
	}

	if result, err := h.trips.UseTab(h.ctx, "alice", other.ID, userTab); err != nil || result.Rejection != "" {
		t.Fatalf("the second trip's move: %v %q", err, result.Rejection)
	}
	h.wantMovedNote(otherSummary, "Second trip")
	h.wantSummary(userTab, other.ID, h.summaryOf(other.ID))

	page = h.pressIn(3, userTab, messageID, tripTabButton(planned.ID))
	refusal := `This tab is the trip "Second trip"'s, which has not ended, so it cannot be another trip's tab.`
	if !strings.HasSuffix(page.text, refusal) || !strings.Contains(page.text, `"Second trip" uses this tab.`) || len(tripMoveButtons(page)) != 0 {
		t.Fatalf("page after the refused press = %+v", page)
	}
	if h.tripTab(planned.ID) != tab || h.tripTab(other.ID) != userTab {
		t.Fatalf("the refused press moved a trip: %d and %d", h.tripTab(planned.ID), h.tripTab(other.ID))
	}

	page = h.pressIn(4, userTab, messageID, tripTabButton(strings.ToUpper(planned.ID)+"-x"))
	if strings.Contains(page.text, TripsTabGoneNote) || strings.Contains(page.text, "Moved") || strings.Contains(page.text, refusal) || !strings.Contains(page.text, `"Second trip" uses this tab.`) {
		t.Fatalf("press with a malformed trip ID = %+v", page)
	}
	if page = h.pressIn(5, userTab, messageID, tripTabButton("ffffffff")); !strings.HasSuffix(page.text, TripsTabGoneNote) {
		t.Fatalf("press for a trip that is not planned = %+v", page)
	}
	// The malformed press is recorded by the time the next one is answered.
	if logs := h.logs.String(); !strings.Contains(logs, "reason=unknown_action") || strings.Contains(logs, "-x") {
		t.Fatalf("the malformed press is not recorded as an unknown action:\n%s", logs)
	}
	h.expectNoNewTab()
	h.expectNoTopicEdit()
}

// The five nearest planned trips get a button, nearest first, and a sixth gets none; the buttons
// are there with mail readable and the scan off too.
func TestTheTripsPageOffersItsTabToFiveTrips(t *testing.T) {
	t.Parallel()
	h, sim := newTripSettingsHarness(t)
	var want []string
	for index := 6; index >= 1; index-- {
		planned, _ := h.addTripTitled(fmt.Sprintf("Trip %d", index), time.Duration(index)*time.Hour)
		if index <= tripTabButtons {
			want = append([]string{tripTabButton(planned.ID)}, want...)
		}
	}

	page, _ := h.tripsPageIn(1, userTab)
	if got := tripMoveButtons(page); !slices.Equal(got, want) {
		t.Fatalf("buttons = %v, want the five nearest trips %v", got, want)
	}
	if strings.Contains(page.text, TripsTabLine) || buttonTitled(page.keyboard, want[0]) != "Use this tab for Trip 1" {
		t.Fatalf("page = %+v", page)
	}

	if reply := h.connectGoogle(sim, 10, 101, aliceMailbox, bothProducts); !strings.Contains(reply, "is connected read-only") {
		t.Fatalf("connected reply = %q", reply)
	}
	page, _ = h.tripsPageIn(20, userTab)
	if got := tripMoveButtons(page); !strings.Contains(page.text, TripsOffLine) || !slices.Equal(got, want) {
		t.Fatalf("page with mail readable = %+v", page)
	}
}

// changingTrips is the trips of a principal whose one planned trip stops being planned the moment
// it is asked to move, and is refused in the words the trip tools give the model.
type changingTrips struct {
	planned []trip.Trip
}

func (c *changingTrips) Trips(context.Context, string) ([]trip.Trip, error) {
	return slices.Clone(c.planned), nil
}

func (c *changingTrips) CheckTab(context.Context, string, string, int64, bool) (trip.TabMove, error) {
	return trip.TabMove{}, nil
}

func (c *changingTrips) UseTab(context.Context, string, string, int64) (trip.Result, error) {
	c.planned = nil
	return trip.Result{Rejection: "That trip is cancelled and cannot be changed."}, nil
}

// A trip cancelled between the page and the press is answered with the page's own note, not with
// the refusal written for the model.
func TestAPressForATripCancelledMeanwhileSaysItIsNoLongerPlanned(t *testing.T) {
	t.Parallel()
	trips := &changingTrips{planned: []trip.Trip{{ID: "0123abcd", Title: "Example trip", Status: trip.StatusPlanned}}}
	section := NewTripsSection(nil, nil, trips, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	outcome, err := section.act(thread.With(context.Background(), userTab), "alice", 101, "tab:0123abcd")
	if err != nil || outcome.note != TripsTabGoneNote {
		t.Fatalf("outcome = %+v %v, want the note for a trip no longer planned", outcome, err)
	}
}

package telegram

import (
	"slices"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const locationClockStart = "2026-09-30T12:05:00Z"

func locationHarness(t *testing.T, mutate func(*harnessOptions)) (*conversationHarness, *controlledRuntime, *testClock) {
	t.Helper()
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	runtime := newControlledRuntime()
	clock := newTestClock(t, locationClockStart)
	options := harnessOptions{location: amsterdam, clock: clock, locations: true}
	if mutate != nil {
		mutate(&options)
	}
	h := newConversationHarnessWithOptions(t, runtime, options)
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	return h, runtime, clock
}

func pinUpdate(id, userID int64, at time.Time, latitude, longitude float64) Update {
	update := textUpdate(id, userID, "")
	update.Message.Date = at.Unix()
	update.Message.Location = &Location{Latitude: latitude, Longitude: longitude, HorizontalAccuracy: 15}
	return update
}

func liveUpdate(id, userID int64, at time.Time, period int64) Update {
	update := pinUpdate(id, userID, at, 52.16612, 4.48105)
	update.Message.Location.LivePeriod = period
	return update
}

func liveEdit(userID, messageID int64, updateID int64, started, edited time.Time, latitude, longitude float64) Update {
	return Update{UpdateID: updateID, EditedMessage: &Message{
		MessageID: messageID, From: &User{ID: userID, FirstName: "Test"}, Chat: Chat{ID: userID, Type: ChatTypePrivate},
		Date: started.Unix(), EditDate: edited.Unix(),
		Location: &Location{Latitude: latitude, Longitude: longitude, HorizontalAccuracy: 10, LivePeriod: 900},
	}}
}

// askTurn sends a text message and returns the message the model receives, answering the turn.
func (h *conversationHarness) askTurn(runtime *controlledRuntime, updateID, userID int64, text string) string {
	h.t.Helper()
	h.push(textUpdate(updateID, userID, text))
	call := receive(h.t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "ok"}
	h.wantReply(userID, updateID, "ok")
	h.waitState(updateID, "delivered")
	return call.request.Message
}

func (h *conversationHarness) wantQuiet(runtime *controlledRuntime) {
	h.t.Helper()
	select {
	case message := <-h.messages:
		h.t.Fatalf("unexpected message: %+v", message)
	case call := <-runtime.calls:
		h.t.Fatalf("unexpected model call: %+v", call.request.Message)
	case edit := <-h.edits:
		h.t.Fatalf("unexpected edit: %+v", edit)
	default:
	}
}

func TestSharedPinIsAcknowledgedWithoutATurnAndInformsTheNextOne(t *testing.T) {
	h, runtime, clock := locationHarness(t, nil)
	h.start()

	h.push(pinUpdate(1, 101, clock.Now(), 52.16612, 4.48105))
	h.wantReply(101, 1, "📍 Got it, near Stationsplein 1, Leiden. Ask me anything about it.")
	h.waitOffset(2)
	h.wantQuiet(runtime)

	clock.Set(t, "2026-09-30T12:07:00Z")
	message := h.askTurn(runtime, 2, 101, "pharmacy nearby?")
	for _, want := range []string{
		"[system] User's location: 52.16612, 4.48105 (±15 m; shared 2 min ago), near Stationsplein 1, Leiden.",
		"[system] Current local time:", "pharmacy nearby?",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("turn message lacks %q: %q", want, message)
		}
	}

	clock.Set(t, "2026-09-30T13:05:01Z")
	if message := h.askTurn(runtime, 3, 101, "and now?"); strings.Contains(message, "User's location") {
		t.Fatalf("an expired pin reached the model: %q", message)
	}
	if _, found, _ := h.store.LoadLocation(h.ctx, "alice"); !found {
		t.Fatal("the row should wait for the prune, not vanish on read")
	}
}

func TestOtherPrincipalsNeverSeeALocation(t *testing.T) {
	h, runtime, clock := locationHarness(t, nil)
	h.start()
	h.push(pinUpdate(1, 101, clock.Now(), 52.16612, 4.48105))
	h.wantReply(101, 1, "📍 Got it, near Stationsplein 1, Leiden. Ask me anything about it.")

	if message := h.askTurn(runtime, 2, 202, "where am I?"); strings.Contains(message, "location") && strings.Contains(message, "52.16612") {
		t.Fatalf("bob's turn saw alice's location: %q", message)
	}
	if message := h.askTurn(runtime, 3, 101, "where am I?"); !strings.Contains(message, "52.16612, 4.48105") {
		t.Fatalf("alice's turn lacks her location: %q", message)
	}
}

func TestVenueShareNamesTheVenue(t *testing.T) {
	h, runtime, clock := locationHarness(t, nil)
	h.start()
	update := pinUpdate(1, 101, clock.Now(), 52.1, 4.4)
	update.Message.Venue = &Venue{Location: *update.Message.Location, Title: "Cafe Zuid", Address: "Kerkstraat 2, Leiden"}
	update.Message.Location = nil
	h.push(update)
	h.wantReply(101, 1, "📍 Got it, near Kerkstraat 2, Leiden. Ask me anything about it.")
	if message := h.askTurn(runtime, 2, 101, "hi"); !strings.Contains(message, ", at Cafe Zuid, near Kerkstraat 2, Leiden.") {
		t.Fatalf("turn message = %q", message)
	}
}

func TestLiveLocationFollowsEditsSilently(t *testing.T) {
	h, runtime, clock := locationHarness(t, nil)
	h.start()
	start := clock.Now()

	h.push(liveUpdate(1, 101, start, 900))
	h.wantReply(101, 1, "📍 Following your live location until 14:20, near Stationsplein 1, Leiden.")
	h.waitOffset(2)

	messageID := requestMessageID(1)
	h.push(
		liveEdit(101, messageID, 2, start, start.Add(10*time.Second), 52.2, 4.5),
		liveEdit(101, messageID, 3, start, start.Add(20*time.Second), 52.3, 4.6),
		liveEdit(101, messageID, 4, start, start.Add(30*time.Second), 52.1, 5.4),
	)
	h.waitOffset(5)
	h.wantQuiet(runtime)
	stored, found, err := h.store.LoadLocation(h.ctx, "alice")
	if err != nil || !found || stored.Latitude != 52.1 || stored.Longitude != 5.4 || !stored.UpdatedAt.Equal(start.Add(30*time.Second)) {
		t.Fatalf("stored = %+v %v %v", stored, found, err)
	}

	// Out-of-order delivery of an older edit, and an edited text message, change nothing.
	edited := textUpdate(6, 101, "edited words")
	edited.EditedMessage, edited.Message = edited.Message, nil
	edited.EditedMessage.EditDate = start.Add(time.Minute).Unix()
	h.push(liveEdit(101, messageID, 5, start, start.Add(15*time.Second), 10, 10), edited)
	h.waitOffset(7)
	h.wantQuiet(runtime)
	if stored, _, _ := h.store.LoadLocation(h.ctx, "alice"); stored.Latitude != 52.1 {
		t.Fatalf("an older edit moved the position back: %+v", stored)
	}

	clock.Set(t, "2026-09-30T12:06:00Z")
	if message := h.askTurn(runtime, 7, 101, "where to?"); !strings.Contains(message, "52.10000, 5.40000 (±10 m; live until 14:20, updated 30 s ago)") {
		t.Fatalf("turn message = %q", message)
	}
}

func TestStoppingALiveShareKeepsItBrieflyAsEnded(t *testing.T) {
	h, runtime, clock := locationHarness(t, nil)
	h.start()
	start := clock.Now()
	h.push(liveUpdate(1, 101, start, 900))
	h.wantReply(101, 1, "📍 Following your live location until 14:20, near Stationsplein 1, Leiden.")

	stop := liveEdit(101, requestMessageID(1), 2, start, start.Add(time.Minute), 52.16612, 4.48105)
	stop.EditedMessage.Location.LivePeriod = 0
	h.push(stop)
	h.waitOffset(3)
	h.wantQuiet(runtime)

	clock.Set(t, "2026-09-30T12:10:00Z")
	if message := h.askTurn(runtime, 3, 101, "how far?"); !strings.Contains(message, "live share ended, last updated") {
		t.Fatalf("turn message = %q", message)
	}
	clock.Set(t, "2026-09-30T12:21:01Z")
	if message := h.askTurn(runtime, 4, 101, "and?"); strings.Contains(message, "User's location") {
		t.Fatalf("an ended share outlived its grace: %q", message)
	}
}

func TestForgettingDuringALiveShareStopsLaterEdits(t *testing.T) {
	h, runtime, clock := locationHarness(t, nil)
	h.start()
	start := clock.Now()
	h.push(liveUpdate(1, 101, start, 900))
	h.wantReply(101, 1, "📍 Following your live location until 14:20, near Stationsplein 1, Leiden.")

	overview := h.requestSettings(2, 101)
	if !strings.Contains(overview.text, "Location: shared") || !slices.Contains(buttonData(overview.keyboard), "nav:location") {
		t.Fatalf("settings overview = %+v", overview)
	}
	page := h.openSettingsPage(3, 101, settingsMessageID(2), "location")
	if !slices.Contains(buttonData(page.keyboard), "location:forget") {
		t.Fatalf("location page = %+v", page)
	}
	edit := h.press(4, 101, settingsMessageID(2), "location:forget")
	if !strings.Contains(edit.text, LocationForgottenNote) || !strings.Contains(edit.text, "Location: not shared") {
		t.Fatalf("settings after forgetting = %+v", edit)
	}
	if _, found, _ := h.store.LoadLocation(h.ctx, "alice"); found {
		t.Fatal("the position survived forgetting")
	}

	h.push(liveEdit(101, requestMessageID(1), 5, start, start.Add(time.Minute), 52.5, 4.9))
	h.waitOffset(6)
	h.wantQuiet(runtime)
	if _, found, _ := h.store.LoadLocation(h.ctx, "alice"); found {
		t.Fatal("an edit after forgetting stored the position again")
	}

	h.push(pinUpdate(6, 101, clock.Now(), 52.0, 4.0))
	h.wantReply(101, 6, "📍 Got it, near Stationsplein 1, Leiden. Ask me anything about it.")
	if _, found, _ := h.store.LoadLocation(h.ctx, "alice"); !found {
		t.Fatal("a new share was not stored")
	}
	h.push(liveEdit(101, requestMessageID(1), 7, start, start.Add(2*time.Minute), 52.6, 4.9))
	h.waitOffset(8)
	h.wantQuiet(runtime)
	if stored, _, _ := h.store.LoadLocation(h.ctx, "alice"); stored.Latitude != 52.0 {
		t.Fatalf("the forgotten live message replaced the later pin: %+v", stored)
	}
}

func TestUnreadableLocationIsRefusedAndNotKept(t *testing.T) {
	h, runtime, clock := locationHarness(t, nil)
	h.start()
	h.push(pinUpdate(1, 101, clock.Now(), 91, 4.4))
	h.wantReply(101, 1, "I could not read that location, so I did not keep it.")
	h.waitOffset(2)
	h.wantQuiet(runtime)
	if _, found, _ := h.store.LoadLocation(h.ctx, "alice"); found {
		t.Fatal("an invalid position was stored")
	}
}

func TestLocationSharingDisabledTurnsPinsAway(t *testing.T) {
	h, runtime, clock := locationHarness(t, func(o *harnessOptions) { o.locations = false })
	h.start()
	h.push(pinUpdate(1, 101, clock.Now(), 52.1, 4.4))
	h.wantReply(101, 1, "Location sharing is not enabled on this assistant.")
	h.push(textUpdate(2, 101, "/privacy"))
	if reply := h.nextMessage(); strings.Contains(reply.text, "location you share") {
		t.Fatalf("privacy mentions locations while disabled: %q", reply.text)
	}
	h.wantQuiet(runtime)
}

func TestPrivacyMentionsLocationsWhenEnabled(t *testing.T) {
	h, _, _ := locationHarness(t, nil)
	h.start()
	h.push(textUpdate(1, 101, "/privacy"))
	if reply := h.nextMessage(); !strings.Contains(reply.text, privacyItem+LocationPrivacyLine) {
		t.Fatalf("privacy = %q", reply.text)
	}
}

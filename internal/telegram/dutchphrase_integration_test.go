package telegram

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/dutchphrase"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

const (
	phraseTurnMarker = "This is the Dutch phrase of the day"
	firstPhrase      = `{"candidates":[{"phrase":"Mag ik even langs?","usage":"Je wilt iemand in de trein passeren.","translation":"Can I get past?","usage_translation":"You want to get past someone on the train."}]}`
	headsUpHeading   = "<b>Scheduled: Heads-up</b>\n\n"
	firstBlock       = "🇳🇱 <b>Dutch phrase of the day</b>\n<b>Mag ik even langs?</b>\nJe wilt iemand in de trein passeren.\n<tg-spoiler>Can I get past? — You want to get past someone on the train.</tg-spoiler>"
)

// setPhrase turns alice's Dutch phrase on or off.
func (h *conversationHarness) setPhrase(on bool) {
	h.t.Helper()
	if err := h.switches.SetEnabled(h.ctx, "alice", dutchphrase.Switch, on); err != nil {
		h.t.Fatal(err)
	}
}

// answerTurn answers the next turn, which must not be the Dutch phrase's.
func answerTurn(t *testing.T, h *conversationHarness, runtime *controlledRuntime, reply string) {
	t.Helper()
	call := receive(t, h.ctx, runtime.calls)
	if strings.Contains(call.request.Message, phraseTurnMarker) {
		t.Fatalf("the Dutch phrase's turn ran before the Heads-up turn: %q", call.request.Message)
	}
	call.answer <- agentruntime.RunResult{Response: reply, Final: reply}
}

// answerPhrase answers the next turn, which must be the Dutch phrase's, and returns its message.
func answerPhrase(t *testing.T, h *conversationHarness, runtime *controlledRuntime, reply string) string {
	t.Helper()
	call := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(call.request.Message, phraseTurnMarker) || len(call.request.Tools.Definitions) != 0 {
		t.Fatalf("turn = %q with %d tools, want the Dutch phrase's", call.request.Message, len(call.request.Tools.Definitions))
	}
	call.answer <- agentruntime.RunResult{Response: reply, Final: reply}
	return call.request.Message
}

// messageWithoutTurn is the next message sent, failing when another turn starts first.
func messageWithoutTurn(t *testing.T, h *conversationHarness, runtime *controlledRuntime) sentMessage {
	t.Helper()
	select {
	case call := <-runtime.calls:
		t.Fatalf("an unexpected turn started: %q", call.request.Message)
	case message := <-h.messages:
		return message
	case <-h.ctx.Done():
		t.Fatal("timed out waiting for a message")
	}
	return sentMessage{}
}

func (h *conversationHarness) waitRuns(jobID, status string, want int) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for h.countRows(`SELECT count(*) FROM schedule_runs WHERE job_id = ? AND status = ?`, jobID, status) < want {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("%s runs of %s did not reach %d", status, jobID, want)
		}
	}
}

func (h *conversationHarness) runHeadsUpNow(jobID string) {
	h.t.Helper()
	if queued, err := h.scheduler.RunNow(h.ctx, "alice", jobID, ""); err != nil || !queued {
		h.t.Fatalf("run now = %v %v", queued, err)
	}
	h.scheduler.Wake()
}

// The first Heads-up of a day ends with the phrase, sent alone on a quiet morning with its
// translation behind a spoiler. A later scheduled run the same day adds none, a manual run shows the
// day's phrase again, and the next day's phrase is never one sent before.
func TestHeadsUpEndsWithTheDutchPhraseOncePerDay(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()
	h.setPhrase(true)
	notes := schedule.HeadsUpNotes + " Prefer phrases for work."
	headsUp := h.createJob("alice", schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, "0 9,18 * * *", notes)

	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	answerTurn(t, h, runtime, prompt.NothingToAdd)
	if message := answerPhrase(t, h, runtime, firstPhrase); !strings.Contains(message, "Prefer phrases for work.") {
		t.Fatalf("the phrase's turn lacks the Heads-up notes: %q", message)
	}
	if message := h.nextMessage(); message.text != headsUpHeading+firstBlock || message.parseMode != "HTML" {
		t.Fatalf("quiet morning sent %q (%s), want the phrase alone", message.text, message.parseMode)
	}
	h.waitRuns(headsUp.ID, schedule.StatusDelivered, 1)
	if got := h.countRows(`SELECT count(*) FROM dutch_phrases WHERE principal_id = 'alice' AND local_date = '2026-09-28' AND phrase = 'Mag ik even langs?'`); got != 1 {
		t.Fatalf("stored phrases = %d", got)
	}
	if got := h.countRows(`SELECT count(*) FROM job_reports WHERE text LIKE '%||Can I get past?%'`); got != 1 {
		t.Fatalf("reports holding the phrase = %d", got)
	}

	clock.Set(t, "2026-09-28T18:00:20+02:00")
	h.scheduler.Wake()
	answerTurn(t, h, runtime, "The bin goes out tonight.")
	if message := messageWithoutTurn(t, h, runtime); message.text != headsUpHeading+"The bin goes out tonight." {
		t.Fatalf("the second run of the day sent %q", message.text)
	}
	h.waitRuns(headsUp.ID, schedule.StatusDelivered, 2)

	h.runHeadsUpNow(headsUp.ID)
	answerTurn(t, h, runtime, "Nothing new since this morning.")
	if message := messageWithoutTurn(t, h, runtime); message.text != headsUpHeading+"Nothing new since this morning.\n\n"+firstBlock {
		t.Fatalf("a manual run sent %q", message.text)
	}
	h.waitRuns(headsUp.ID, schedule.StatusDelivered, 3)

	clock.Set(t, "2026-09-29T09:00:20+02:00")
	h.scheduler.Wake()
	answerTurn(t, h, runtime, "The 08:17 is cancelled; take the 08:23.")
	next := `{"candidates":[{"phrase":"mag ik even langs","usage":"a","translation":"b","usage_translation":"c"},` +
		`{"phrase":"Tot zo!","usage":"Je ziet iemand straks weer.","translation":"See you soon!","usage_translation":"You will see someone again shortly.","note":"zo means soon here."}]}`
	if message := answerPhrase(t, h, runtime, next); !strings.Contains(message, "> Mag ik even langs?") {
		t.Fatalf("the phrase's turn does not list the phrase sent before: %q", message)
	}
	want := headsUpHeading + "The 08:17 is cancelled; take the 08:23.\n\n🇳🇱 <b>Dutch phrase of the day</b>\n<b>Tot zo!</b>\nJe ziet iemand straks weer.\n" +
		"<tg-spoiler>See you soon! — You will see someone again shortly.</tg-spoiler>\n<tg-spoiler>zo means soon here.</tg-spoiler>"
	if message := h.nextMessage(); message.text != want {
		t.Fatalf("the next morning sent %q", message.text)
	}
	if got := h.countRows(`SELECT count(*) FROM dutch_phrases WHERE principal_id = 'alice'`); got != 2 {
		t.Fatalf("stored phrases = %d", got)
	}
}

// The phrase never changes a run beyond adding itself: with the switch off, after a failed turn, and
// when the phrase's own turn fails or cannot be read, the run is what its turn made it.
func TestHeadsUpWithoutAPhraseIsAsTheTurnMadeIt(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	h.start()
	headsUp := h.createJob("alice", schedule.HeadsUpJobID("alice"), schedule.HeadsUpName, "0 9 * * *", schedule.HeadsUpNotes)
	delivered := 0
	// Each manual run needs its own minute, which the run is keyed by.
	runNow := func() {
		t.Helper()
		clock.Set(t, fmt.Sprintf("2026-09-28T08:%02d:00+02:00", delivered+1))
		h.runHeadsUpNow(headsUp.ID)
	}
	report := func(reply, want string) {
		t.Helper()
		runNow()
		answerTurn(t, h, runtime, reply)
		if message := messageWithoutTurn(t, h, runtime); message.text != headsUpHeading+want {
			t.Fatalf("run sent %q, want %q", message.text, want)
		}
		delivered++
		h.waitRuns(headsUp.ID, schedule.StatusDelivered, delivered)
	}

	report("Rain from noon.", "Rain from noon.")

	h.setPhrase(true)
	report("", localetest.English(turn.UnavailableReply))

	for _, phraseReply := range []string{"Here is a phrase: Tot zo!", ""} {
		runNow()
		answerTurn(t, h, runtime, "Rain from noon.")
		answerPhrase(t, h, runtime, phraseReply)
		if message := messageWithoutTurn(t, h, runtime); message.text != headsUpHeading+"Rain from noon." {
			t.Fatalf("with the phrase reply %q the run sent %q", phraseReply, message.text)
		}
		delivered++
		h.waitRuns(headsUp.ID, schedule.StatusDelivered, delivered)
	}

	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	answerTurn(t, h, runtime, prompt.NothingToAdd)
	answerPhrase(t, h, runtime, "{}")
	h.waitRuns(headsUp.ID, schedule.StatusSkipped, 1)
	if got := h.countRows(`SELECT count(*) FROM dutch_phrases`); got != 0 {
		t.Fatalf("stored phrases = %d", got)
	}
	if len(h.messages) != 0 {
		t.Fatalf("a quiet run without a phrase sent %+v", <-h.messages)
	}
}

// The phrase's toggle shows on the Heads-up page only while Heads-up is on, and its switch outlives
// Heads-up being turned off and on again. /privacy names the kept phrases while the phrase is on or
// any phrase was sent.
func TestDutchPhraseToggleFollowsHeadsUp(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()
	enabled := func() bool {
		t.Helper()
		on, err := h.switches.Enabled(h.ctx, "alice", dutchphrase.Switch)
		if err != nil {
			t.Fatal(err)
		}
		return on
	}
	hasPhrase := func(page editedMessage) bool {
		return strings.Contains(page.text, "Dutch phrase:") || slices.ContainsFunc(buttonData(page.keyboard), func(data string) bool {
			return strings.HasPrefix(data, "headsup:phrase")
		})
	}

	h.requestSettings(1, 101)
	messageID := settingsMessageID(1)
	if page := h.openSettingsPage(2, 101, messageID, "tools"); hasPhrase(page) {
		t.Fatalf("the phrase shows while Heads-up is off: %+v", page)
	}
	page := h.press(3, 101, messageID, "headsup:on")
	if !strings.Contains(page.text, localetest.English(PhraseOffLine)) || !slices.Contains(buttonData(page.keyboard), "headsup:phraseon") {
		t.Fatalf("page after Heads-up on = %+v", page)
	}
	page = h.press(4, 101, messageID, "headsup:phraseon")
	if !strings.Contains(page.text, localetest.English(PhraseOnLine)) || !slices.Contains(buttonData(page.keyboard), "headsup:phraseoff") || !enabled() {
		t.Fatalf("page after phrase on = %+v", page)
	}
	h.push(textUpdate(5, 101, "/privacy"))
	if privacy := h.nextMessage(); !strings.Contains(privacy.text, localetest.English(PhrasePrivacyLine)) {
		t.Fatalf("/privacy with the phrase on = %q", privacy.text)
	}

	if page = h.press(6, 101, messageID, "headsup:off"); hasPhrase(page) || !enabled() {
		t.Fatalf("page after Heads-up off = %+v, phrase on = %v", page, enabled())
	}
	page = h.press(7, 101, messageID, "headsup:on")
	if !strings.Contains(page.text, localetest.English(PhraseOnLine)) || !slices.Contains(buttonData(page.keyboard), "headsup:phraseoff") {
		t.Fatalf("page after Heads-up on again = %+v", page)
	}
	page = h.press(8, 101, messageID, "headsup:phraseoff")
	if !strings.Contains(page.text, localetest.English(PhraseOffLine)) || enabled() {
		t.Fatalf("page after phrase off = %+v", page)
	}
	h.push(textUpdate(9, 101, "/privacy"))
	if privacy := h.nextMessage(); strings.Contains(privacy.text, localetest.English(PhrasePrivacyLine)) {
		t.Fatalf("/privacy with the phrase off and no phrase sent = %q", privacy.text)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO dutch_phrases (principal_id, local_date, phrase, normalized, usage, translation, usage_translation, created_at)
		VALUES ('alice', '2026-09-28', 'Tot zo!', 'tot zo', 'Bij het afscheid.', 'See you soon!', 'When saying goodbye.', '2026-09-28T07:30:00Z')`); err != nil {
		t.Fatal(err)
	}
	h.push(textUpdate(10, 101, "/privacy"))
	if privacy := h.nextMessage(); !strings.Contains(privacy.text, localetest.English(PhrasePrivacyLine)) {
		t.Fatalf("/privacy with the phrase off and phrases kept = %q", privacy.text)
	}

	h.press(11, 101, messageID, "headsup:off")
	h.press(12, 101, messageID, "headsup:phraseon")
	if enabled() {
		t.Fatal("a stale button turned the phrase on while Heads-up is off")
	}
}

package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/turn"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const unattendedRussianLine = "[system] Interface language: Russian. Write anything meant for the user in Russian."

// A scheduled turn names the language a Russian user chose, a chat turn of theirs does not, and a
// user who never chose reads a scheduled turn as before. The report header and the fixed notice
// of a run read in Russian for the one who chose it and in English for the other.
func TestScheduledTurnIsToldTheChosenLanguageAndItsNoticesReadInIt(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, languages: true, location: amsterdam, clock: clock})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}

	h.push(textUpdate(1, 101, "hello"))
	chat := receive(t, h.ctx, runtime.calls)
	if strings.Contains(chat.request.Message, unattendedRussianLine) || strings.Contains(chat.request.Message, "Interface language") {
		t.Fatalf("a chat turn was told the interface language: %q", chat.request.Message)
	}
	chat.answer <- agentruntime.RunResult{Response: "hi"}
	h.wantReply(101, 1, "hi")
	h.waitState(1, "delivered")

	alice := h.createJob("alice", "a1b2c3d4e5f60718", "Commute", "0 9 * * *", "Check my trains.")
	h.createJob("bob", "b1b2c3d4e5f60718", "Commute", "0 9 * * *", "Check my trains.")
	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	messages := map[string]string{}
	for range 2 {
		run := receive(t, h.ctx, runtime.calls)
		messages[run.request.PrincipalID] = run.request.Message
		run.answer <- agentruntime.RunResult{Response: "All trains run.", Final: "All trains run."}
	}
	reports := map[int64]string{}
	for range 2 {
		report := h.nextMessage()
		reports[report.chatID] = report.text
	}
	if !strings.HasPrefix(messages["alice"], prompt.Marker+" Current local time:") || !strings.Contains(messages["alice"], unattendedRussianLine+"\n\n"+prompt.Marker+" This is the user's scheduled job") {
		t.Fatalf("alice's scheduled turn = %q, want the language line beside the time", messages["alice"])
	}
	if strings.Contains(messages["bob"], "Interface language") {
		t.Fatalf("bob's scheduled turn = %q, want no language line", messages["bob"])
	}
	if want := strings.Replace(messages["alice"], unattendedRussianLine+"\n\n", "", 1); strings.ReplaceAll(want, "Alice", "Bob") != messages["bob"] {
		t.Fatalf("the two scheduled turns differ beyond the line and the name:\n%q\n%q", messages["alice"], messages["bob"])
	}
	if reports[101] != "<b>По расписанию: Commute</b>\n\nAll trains run." || reports[202] != "<b>Scheduled: Commute</b>\n\nAll trains run." {
		t.Fatalf("reports = %v", reports)
	}
	h.waitRun(alice.ID, schedule.StatusDelivered)

	if queued, err := h.scheduler.RunNow(h.ctx, "alice", alice.ID, ""); err != nil || !queued {
		t.Fatalf("run now = %v %v", queued, err)
	}
	h.scheduler.Wake()
	quiet := receive(t, h.ctx, runtime.calls)
	quiet.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd, Final: prompt.NothingToAdd}
	if report := h.nextMessage(); report.text != "<b>По расписанию: Commute</b>\n\nСейчас сообщить нечего." || len(localetest.Latin(report.text, "Commute", "b")) != 0 {
		t.Fatalf("quiet report = %q", report.text)
	}
}

// A background task's message names the Russian a user chose, and the system prompt does not
// change with the language.
func TestBackgroundTaskMessageIsToldTheChosenLanguage(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: testTasks(), languages: true})
	h.provision("alice", "account-alice")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}

	chat, status, _ := h.startTask(runtime, 1, "Mail", "Go through my mail and list what needs action.")
	run := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(run.request.Message, unattendedRussianLine) || strings.Contains(chat.request.Message, "Interface language") {
		t.Fatalf("task message = %q, chat message = %q", run.request.Message, chat.request.Message)
	}
	if strings.Contains(prompt.System(true, "", false), "Interface language") {
		t.Fatal("the system prompt names the interface language")
	}
	h.wantStatusEdit(status, "Работаю над: Mail", true)
}

// A watch with no language of its own sends a Russian user's alert through a rewrite into Russian,
// and an English user's alert as it was written.
func TestWatchAlertOfARussianUserIsRewrittenIntoRussian(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	evaluator := &stubEvaluator{}
	evaluator.set(watch.Observation{State: "cancelled", Triggered: true, Headline: "It is cancelled."})
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, watches: evaluator, languages: true})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.createWatch("bob", "00000000000000b1", trainWatch())
	h.watches.Cycle(h.ctx)
	if alert := h.nextMessage(); alert.chatID != 202 || !strings.Contains(alert.text, "It is cancelled.") {
		t.Fatalf("bob's alert = %+v", alert)
	}

	h.createWatch("alice", "00000000000000a1", trainWatch())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.watches.Cycle(h.ctx)
	}()
	rewrite := receive(t, h.ctx, runtime.calls)
	original := rewrite.request.Message
	if !strings.HasPrefix(original, "[system] Rewrite the message below in Russian for the user.") || !strings.Contains(original, "It is cancelled.") {
		t.Fatalf("rewrite run = %q", original)
	}
	rewrite.answer <- agentruntime.RunResult{Response: "x", Final: "**⚠️ 08:30 поезд Utrecht Centraal → Amsterdam Centraal**\n\nПоезд отменён."}
	alert := h.nextMessage()
	<-done
	if alert.chatID != 101 || !strings.Contains(alert.text, "Поезд отменён.") {
		t.Fatalf("alice's alert = %+v", alert)
	}
}

// A Russian user's scheduled run that cannot reach their model reads in Russian, while the report it
// keeps, which later turns read, stays English.
func TestRussianUsersFailedRunReadsInRussianAndStoresEnglish(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, languages: true})
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	job := h.createJob("alice", "0000000000000001", "Morning", "0 9 * * *", "Anything new?")

	clock.Set(t, "2026-09-28T09:00:05+02:00")
	h.scheduler.Wake()
	h.wantMessage(101, "<b>По расписанию: Morning</b>\n\n"+turn.NotConnectedReply.In(locale.Russian))
	h.waitRun(job.ID, schedule.StatusDelivered)
	var reply string
	if err := h.db.QueryRowContext(h.ctx, `SELECT text FROM job_reports WHERE job_id = ?`, job.ID).Scan(&reply); err != nil {
		t.Fatal(err)
	}
	if reply != turn.NotConnectedReply.In(locale.English) {
		t.Fatalf("stored report = %q, want English", reply)
	}
}

package telegram

import (
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// scheduleLanguageRow reads what an approval record stores: the summary the user confirmed, the
// English outcome the model reads and the outcome in the user's language.
func scheduleLanguageRow(h *conversationHarness, id string) (summary, outcome, userOutcome string) {
	h.t.Helper()
	if err := h.db.QueryRowContext(h.ctx, `SELECT summary, COALESCE(outcome, ''), user_outcome FROM approvals WHERE id = ?`, id).Scan(&summary, &outcome, &userOutcome); err != nil {
		h.t.Fatal(err)
	}
	return summary, outcome, userOutcome
}

func scheduleLanguageWant(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("%s lacks %q: %s", what, want, text)
		}
	}
}

// A Russian user's direct job actions are stored and shown in Russian, and each Undo answers in
// Russian, while the model's tool results and the English outcome stay English.
func TestRussianUserSchedulesChangesAndUndoesJobs(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{languages: true, schedules: clock, providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	allowed := []string{"Weekly", "tasks", "List", "my", "overdue", "Europe", "Amsterdam", "CEST", "CET", "Daily", "digest", "Morning", "Evening", "Summarise", "day", "jobs"}

	result := h.ownDataTurn(runtime, 1, false, "schedule_propose", `{"name":"Weekly tasks","cron":"0 9 * * 1","prompt":"List my overdue tasks."}`)
	created := h.wantDirect(result, "schedule_propose")
	if cyrillic.MatchString(result.Text) {
		t.Fatalf("the model's tool result carries Russian: %q", result.Text)
	}
	summary, outcome, userOutcome := scheduleLanguageRow(h, created)
	scheduleLanguageWant(t, "created summary", summary, "Запланировать «Weekly tasks»", "Запуск: 0 9 * * 1 (Europe/Amsterdam)", "Следующий запуск: пн 28 сен 09:00 CEST; пн 5 окт 09:00 CEST; пн 12 окт 09:00 CEST",
		"Каждый запуск спрашивает твою модель, только с инструментами чтения, и присылает тебе ответ:\nList my overdue tasks.")
	if !strings.HasPrefix(outcome, "Scheduled. First run: Mon ") || !strings.HasPrefix(userOutcome, "Запланировано. Первый запуск: пн ") {
		t.Fatalf("created outcomes = %q, %q", outcome, userOutcome)
	}
	if latin := localetest.Latin(summary+userOutcome, allowed...); len(latin) != 0 {
		t.Fatalf("created texts keep Latin words %v: %s %s", latin, summary, userOutcome)
	}
	if answer := h.undoDirect(created); answer != "Отменено." {
		t.Fatalf("undo of a created job = %q", answer)
	}
	_, outcome, userOutcome = scheduleLanguageRow(h, created)
	if outcome != `Deleted the scheduled job "Weekly tasks".` || userOutcome != "Задание «Weekly tasks» удалено." {
		t.Fatalf("undone outcomes = %q, %q", outcome, userOutcome)
	}

	job := h.createJob("alice", "a1b2c3d4e5f60718", "Daily digest", "30 7 * * *", "Summarise my day.")
	updated := h.wantDirect(h.ownDataTurn(runtime, 2, false, "schedule_update", `{"job_id":"`+job.ID+`","name":"Morning digest","cron":"0 8 * * *"}`), "schedule_update")
	summary, outcome, userOutcome = scheduleLanguageRow(h, updated)
	scheduleLanguageWant(t, "changed summary", summary, "Изменить «Daily digest», новое название «Morning digest»", "Запуск: 0 8 * * * (Europe/Amsterdam)", "Инструкции остаются прежними.")
	if !strings.HasPrefix(outcome, "Changed. Next run: ") || !strings.HasPrefix(userOutcome, "Изменено. Следующий запуск: ") {
		t.Fatalf("changed outcomes = %q, %q", outcome, userOutcome)
	}
	if latin := localetest.Latin(summary+userOutcome, allowed...); len(latin) != 0 {
		t.Fatalf("changed texts keep Latin words %v: %s %s", latin, summary, userOutcome)
	}
	if _, err := h.db.ExecContext(h.ctx, `UPDATE scheduled_jobs SET updated_at = '2026-09-28T06:30:00.000Z' WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if answer := h.undoDirect(updated); answer != "Отменено." {
		t.Fatalf("undo of a changed job = %q", answer)
	}
	if _, outcome, userOutcome = scheduleLanguageRow(h, updated); outcome != `Restored the scheduled job "Daily digest" as it was.` || userOutcome != "Задание «Daily digest» возвращено как было." {
		t.Fatalf("restored outcomes = %q, %q", outcome, userOutcome)
	}

	stale := h.wantDirect(h.ownDataTurn(runtime, 3, false, "schedule_update", `{"job_id":"`+job.ID+`","name":"Evening digest"}`), "schedule_update")
	if changed, err := h.scheduler.SetEnabled(h.ctx, "alice", job.ID, false); err != nil || !changed {
		t.Fatalf("pause = %v %v", changed, err)
	}
	answer := h.undoDirect(stale)
	scheduleLanguageWant(t, "refused undo", answer, "задание с тех пор изменили; измени его в /jobs.")
	if _, outcome, userOutcome = scheduleLanguageRow(h, stale); outcome != "the job was changed since; change it in /jobs instead." || !strings.Contains(userOutcome, "задание с тех пор изменили") {
		t.Fatalf("refused outcomes = %q, %q", outcome, userOutcome)
	}

	removed := h.wantDirect(h.ownDataTurn(runtime, 4, false, "schedule_remove", `{"job_id":"`+job.ID+`"}`), "schedule_remove")
	summary, outcome, userOutcome = scheduleLanguageRow(h, removed)
	if summary != "Удалить задание «Evening digest» (30 7 * * * (Europe/Amsterdam)). Оно больше не запустится." || outcome != "Deleted." || userOutcome != "Удалено." {
		t.Fatalf("removed row = %q, %q, %q", summary, outcome, userOutcome)
	}
	if answer := h.undoDirect(removed); answer != "Отменено." {
		t.Fatalf("undo of a removal = %q", answer)
	}
	if _, outcome, userOutcome = scheduleLanguageRow(h, removed); outcome != `Restored the scheduled job "Evening digest"; its earlier runs are not.` || userOutcome != "Задание «Evening digest» восстановлено; прошлые запуски не вернутся." {
		t.Fatalf("recreated outcomes = %q, %q", outcome, userOutcome)
	}

	once := h.wantDirect(h.ownDataTurn(runtime, 5, false, "schedule_propose", `{"name":"Train check","at":"2026-09-28 09:00","prompt":"Is my train delayed?"}`), "schedule_propose")
	gone := h.wantDirect(h.ownDataTurn(runtime, 6, false, "schedule_remove", `{"job_id":"`+once+`"}`), "schedule_remove")
	clock.Set(t, "2026-09-28T09:30:00+02:00")
	answer = h.undoDirect(gone)
	scheduleLanguageWant(t, "refused one-time undo", answer, "его время, пн 28 сен 09:00 CEST, уже прошло, поэтому восстановить нельзя; запланируй заново.")
	if _, outcome, _ = scheduleLanguageRow(h, gone); !strings.HasPrefix(outcome, "its time, Mon 28 Sep 09:00 CEST, has passed, so it cannot be restored") {
		t.Fatalf("refused one-time outcome = %q", outcome)
	}
}

// After outside content a Russian user's proposals read in Russian, confirming stores the Russian
// outcome, and the notice the model reads stays English.
func TestRussianUserConfirmsScheduleProposals(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{languages: true, schedules: clock, providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	allowed := []string{"Weekly", "tasks", "List", "my", "overdue", "Europe", "Amsterdam", "CEST", "CET", "Daily", "digest", "Summarise", "day", "Call", "the", "dentist", "jobs", "Was", "Now", "briefly"}
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Daily digest", "30 7 * * *", "Summarise my day.")

	h.push(textUpdate(1, 101, "set things up"))
	chat := receive(t, h.ctx, runtime.calls)
	readOutside(t, chat)
	call := toolCaller(t, chat)
	proposed := call("schedule_propose", `{"name":"Weekly tasks","cron":"0 9 * * 1","prompt":"List my overdue tasks."}`)
	if proposed.IsError || cyrillic.MatchString(proposed.Text) || !strings.HasSuffix(proposed.Text, approval.ProposedText) {
		t.Fatalf("schedule_propose = %+v", proposed)
	}
	h.finishTurn(chat, 1, "proposed")
	proposal := h.nextMessage()
	scheduleLanguageWant(t, "new job proposal", proposal.text, "Запланировать «Weekly tasks»", "Запуск: 0 9 * * 1 (Europe/Amsterdam)", "Следующий запуск: пн 28 сен 09:00 CEST")
	if latin := localetest.Latin(proposal.text, allowed...); len(latin) != 0 {
		t.Fatalf("the proposal keeps Latin words %v: %s", latin, proposal.text)
	}
	h.waitMessageRecorded(proposal.messageID)
	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	edit := h.nextEdit()
	scheduleLanguageWant(t, "message after confirm", edit.text, "Готово. Запланировано. Первый запуск: пн ")
	continued := h.continueTurn(runtime, 2)
	if cyrillic.MatchString(continued.request.Message) {
		t.Fatalf("the model's notice carries Russian: %q", continued.request.Message)
	}

	h.push(textUpdate(3, 101, "change the digest and add a reminder"))
	chat = receive(t, h.ctx, runtime.calls)
	readOutside(t, chat)
	call = toolCaller(t, chat)
	if changed := call("schedule_update", `{"job_id":"`+job.ID+`","prompt_edits":[{"find":"Summarise my day.","replace":"Summarise my day briefly."}],"cron":"0 8 * * *"}`); changed.IsError {
		t.Fatalf("schedule_update = %+v", changed)
	}
	if reminder := call("reminder_set", `{"text":"Call the dentist","at":"2026-09-28 17:00"}`); reminder.IsError {
		t.Fatalf("reminder_set = %+v", reminder)
	}
	h.finishTurn(chat, 3, "proposed")
	changes := h.nextMessage()
	scheduleLanguageWant(t, "change proposals", changes.text, "Изменить «Daily digest»", "Запуск: 0 8 * * * (Europe/Amsterdam)", "Инструкции меняются только здесь:",
		"Было: Summarise my day.\nСтало: Summarise my day briefly.", "Поставить напоминание на пн 28 сен 17:00 CEST:\nCall the dentist",
		"В этом разговоре был прочитан внешний контент, поэтому нужно твоё подтверждение.")
	if latin := localetest.Latin(changes.text, allowed...); len(latin) != 0 {
		t.Fatalf("the change proposals keep Latin words %v: %s", latin, changes.text)
	}
}

// A user who never chose a language reads the stored outcome of a job action unchanged, and the
// outcome shown to them is the English one.
func TestEnglishUserKeepsScheduleOutcomes(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{languages: true, schedules: clock, providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()

	created := h.wantDirect(h.ownDataTurn(runtime, 1, false, "schedule_propose", `{"name":"Weekly tasks","cron":"0 9 * * 1","prompt":"List my overdue tasks."}`), "schedule_propose")
	summary, outcome, userOutcome := scheduleLanguageRow(h, created)
	scheduleLanguageWant(t, "summary", summary, `Schedule "Weekly tasks"`, "Runs: 0 9 * * 1 (Europe/Amsterdam)", "Next: Mon 28 Sep 09:00 CEST; Mon 5 Oct 09:00 CEST; Mon 12 Oct 09:00 CEST")
	if outcome != userOutcome || !strings.HasPrefix(outcome, "Scheduled. First run: Mon ") {
		t.Fatalf("outcomes = %q, %q", outcome, userOutcome)
	}
	if answer := h.undoDirect(created); answer != "Undone." {
		t.Fatalf("undo = %q", answer)
	}
	if _, outcome, userOutcome = scheduleLanguageRow(h, created); outcome != userOutcome || outcome != `Deleted the scheduled job "Weekly tasks".` {
		t.Fatalf("undone outcomes = %q, %q", outcome, userOutcome)
	}
}

// What /jobs shows of an anchored or recurring job reads in Russian, with the English text kept
// for the model.
func TestScheduleDescriptionsReadInRussian(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		anchor    schedule.Anchor
		english   string
		russian   string
		wantLatin []string
	}{
		{schedule.Anchor{Kind: schedule.AnchorCalendarEvent, Label: "Dentist", OffsetMinutes: -30}, "30 min before Dentist", "за 30 мин до «Dentist»", []string{"Dentist"}},
		{schedule.Anchor{Kind: schedule.AnchorCalendarEvent}, "at the start of the event", "в начале события", nil},
		{schedule.Anchor{Kind: schedule.AnchorCalendarMatch, Title: "Standup", OffsetMinutes: 1500}, `1 day 1 h after each event titled "Standup"`, "через 1 день 1 ч после каждого события «Standup»", []string{"Standup"}},
		{schedule.Anchor{Kind: schedule.AnchorTrip, Edge: schedule.EdgeEnd, Label: "Lisbon", OffsetMinutes: 60}, `1 h after the end of the trip "Lisbon"`, "через 1 ч после конца поездки «Lisbon»", []string{"Lisbon"}},
		{schedule.Anchor{Kind: schedule.AnchorTrip, Edge: schedule.EdgeStart}, "at the start of the trip", "в начале поездки", nil},
		{schedule.Anchor{Kind: schedule.AnchorPlace, Edge: schedule.EdgeArrive, Label: "Office", Once: true}, "on arriving at Office, once", "при прибытии в «Office», один раз", []string{"Office"}},
		{schedule.Anchor{Kind: schedule.AnchorPlace, Edge: schedule.EdgeLeave, OffsetMinutes: 15}, "15 min after leaving the place", "через 15 мин после выхода из этого места", nil},
	} {
		if got := test.anchor.Describe(); got != test.english {
			t.Errorf("Describe() = %q, want %q", got, test.english)
		}
		got := test.anchor.DescribeIn(locale.Russian)
		if got != test.russian || len(localetest.Latin(got, test.wantLatin...)) != 0 {
			t.Errorf("DescribeIn(ru) = %q, want %q", got, test.russian)
		}
	}
}

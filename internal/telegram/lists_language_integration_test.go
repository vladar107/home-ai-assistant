package telegram

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
)

// wantRussianList fails when a text the user reads holds a word written only in Latin letters, other
// than the allowed ones: commands, and values that come from the test's own data.
func wantRussianList(t *testing.T, text string, allowed ...string) {
	t.Helper()
	if found := localetest.Latin(text, allowed...); len(found) != 0 {
		t.Fatalf("text holds Latin words %v: %q", found, text)
	}
}

func wantRussianListButtons(t *testing.T, keyboard InlineKeyboard, allowed ...string) {
	t.Helper()
	for _, label := range buttonLabels(keyboard) {
		wantRussianList(t, label, allowed...)
	}
}

func newRussianListsHarness(t *testing.T) (*conversationHarness, *controlledRuntime) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{languages: true, schedules: newTestClock(t, mondayMorning), tasks: testTasks()})
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()
	return h, runtime
}

// /collections writes its title, counts, and buttons in Russian, a count takes the form its number
// needs, and the delete question and its buttons follow. Bob, who never chose, reads English.
func TestCollectionsReadInRussian(t *testing.T) {
	t.Parallel()
	h, _ := newRussianListsHarness(t)

	h.push(textUpdate(1, 101, "/collections"))
	if empty := h.nextMessage(); empty.text != NoCollectionsReply.In(locale.Russian) {
		t.Fatalf("empty /collections = %q", empty.text)
	} else {
		wantRussianList(t, empty.text)
	}

	const one, three, five = "aaaaaaaaaaaaaa00", "bbbbbbbbbbbbbb00", "cccccccccccccc00"
	h.seedCollection("alice", one, "Книги", 1)
	h.seedCollection("alice", three, "Инструменты", 3)
	h.seedCollection("alice", five, "Фильмы", 5)
	h.seedCollection("bob", "dddddddddddddd00", "Tools", 2)

	h.push(textUpdate(2, 101, "/collections"))
	list := h.nextMessage()
	if want := "Коллекции\n1. Книги: 1 запись\n2. Инструменты: 3 записи\n3. Фильмы: 5 записей"; list.text != want {
		t.Fatalf("/collections = %q, want %q", list.text, want)
	}
	if labels := buttonLabels(list.keyboard); !slices.Equal(labels, []string{"Открыть 1", "Открыть 2", "Открыть 3"}) {
		t.Fatalf("/collections buttons = %v", labels)
	}
	wantRussianList(t, list.text)

	h.push(callbackUpdate(3, 101, list.messageID, "col:open:"+three+":1"))
	h.wantAnswer(3)
	opened := h.nextEdit()
	if !strings.HasPrefix(opened.text, "Инструменты\n\n1. ") {
		t.Fatalf("opened collection = %q", opened.text)
	}
	if labels := buttonLabels(opened.keyboard); !slices.Equal(labels, []string{"Удалить 1", "Удалить 2", "Удалить 3", "Удалить коллекцию", "‹ Все коллекции"}) {
		t.Fatalf("collection buttons = %v", labels)
	}
	wantRussianList(t, opened.text, "title")
	wantRussianListButtons(t, opened.keyboard)

	h.push(callbackUpdate(4, 101, list.messageID, "col:ask:"+three+":1"))
	h.wantAnswer(4)
	ask := h.nextEdit()
	if want := "Удалить коллекцию Инструменты и все её записи (3)? Это нельзя отменить."; ask.text != want {
		t.Fatalf("delete question = %q, want %q", ask.text, want)
	}
	if labels := buttonLabels(ask.keyboard); !slices.Equal(labels, []string{"Да, удалить", "Закрыть"}) {
		t.Fatalf("delete question buttons = %v", labels)
	}

	h.push(callbackUpdate(5, 101, list.messageID, "col:ask:"+five+":1"))
	h.wantAnswer(5)
	if ask := h.nextEdit(); ask.text != "Удалить коллекцию Фильмы и все её записи (5)? Это нельзя отменить." {
		t.Fatalf("delete question = %q", ask.text)
	}

	h.push(textUpdate(6, 202, "/collections"))
	if list := h.nextMessage(); list.text != "Collections\n1. Tools: 2 records" {
		t.Fatalf("bob's /collections = %q, want English", list.text)
	}
}

// /backlog writes its title, ages, hint, and buttons in Russian, with each age in the form its
// number needs and the page buttons and title on a second page. The export stays English.
func TestBacklogReadsInRussian(t *testing.T) {
	t.Parallel()
	h, _ := newRussianListsHarness(t)

	h.push(textUpdate(1, 101, "/backlog"))
	if empty := h.nextMessage(); empty.text != NoBacklogReply.In(locale.Russian) {
		t.Fatalf("empty /backlog = %q", empty.text)
	}

	now := h.backlog.Now()
	for index, days := range []int{0, 1, 3, 5, 11, 21} {
		created := now.Add(-time.Duration(days)*24*time.Hour - 2*time.Hour)
		h.seedBacklog("alice", fmt.Sprintf("aaaaaaa%d", index+1), fmt.Sprintf("Идея %d", index+1), "", created)
	}
	h.push(textUpdate(2, 101, "/backlog"))
	list := h.nextMessage()
	want := "Бэклог\n\n1. Идея 6 (21 день назад)\n2. Идея 5 (11 дней назад)\n3. Идея 4 (5 дней назад)\n4. Идея 3 (3 дня назад)\n5. Идея 2 (1 день назад)\n6. Идея 1 (сегодня)\n\nОтправьте /backlog export, чтобы получить полный текст."
	if list.text != want {
		t.Fatalf("/backlog = %q, want %q", list.text, want)
	}
	wantRussianList(t, list.text, "backlog", "export")
	labels := buttonLabels(list.keyboard)
	if len(labels) != 12 || labels[0] != "Готово 1" || labels[1] != "Удалить 1" {
		t.Fatalf("/backlog buttons = %v", labels)
	}
	wantRussianListButtons(t, list.keyboard)

	for index := 7; index <= 9; index++ {
		h.seedBacklog("alice", fmt.Sprintf("bbbbbbb%d", index), fmt.Sprintf("Заметка %d", index), "", now.Add(-time.Hour))
	}
	h.push(textUpdate(3, 101, "/backlog"))
	paged := h.nextMessage()
	if !strings.HasPrefix(paged.text, "Бэклог (страница 1 из 2)\n\n") {
		t.Fatalf("paged /backlog = %q", paged.text)
	}
	if last := buttonLabels(paged.keyboard); last[len(last)-1] != "Далее ›" {
		t.Fatalf("paged buttons = %v", last)
	}
}

// /jobs writes its title, the state it adds to a job's description, its buttons, and its page
// line in Russian; what a job's description and its next run say is the scheduler's own text.
func TestJobsReadInRussian(t *testing.T) {
	t.Parallel()
	h, _ := newRussianListsHarness(t)

	h.push(textUpdate(1, 101, "/jobs"))
	if empty := h.nextMessage(); empty.text != NoJobsReply.In(locale.Russian) {
		t.Fatalf("empty /jobs = %q", empty.text)
	}

	for index := range 9 {
		h.createJob("alice", fmt.Sprintf("a1b2c3d4e5f6071%d", index), fmt.Sprintf("Сводка %d", index+1), "30 7 * * *", "Summarise my day.")
	}
	h.push(textUpdate(2, 101, "/jobs"))
	list := h.nextMessage()
	if !strings.HasPrefix(list.text, "Задания по расписанию (страница 1 из 2)\n\n1. Сводка 1\n") || !strings.Contains(list.text, ", следующий запуск: ") {
		t.Fatalf("/jobs = %q", list.text)
	}
	labels := buttonLabels(list.keyboard)
	if labels[0] != "Пауза 1" || !slices.Contains(labels, "Запустить 1 сейчас") || !slices.Contains(labels, "Удалить 1") || labels[len(labels)-1] != "Далее ›" {
		t.Fatalf("/jobs buttons = %v", labels)
	}
	wantRussianListButtons(t, list.keyboard)
	wantRussianList(t, strings.SplitN(list.text, "\n", 2)[0])

	edit := h.pressJob(3, list.messageID, "jobs:pause:a1b2c3d4e5f60710:1")
	if !strings.Contains(edit.text, "1. Сводка 1\n") || !strings.Contains(edit.text, ", приостановлено") {
		t.Fatalf("/jobs after pause = %q", edit.text)
	}
	if labels := buttonLabels(edit.keyboard); labels[0] != "Возобновить 1" {
		t.Fatalf("/jobs buttons after pause = %v", labels)
	}

	h.push(textUpdate(4, 202, "/jobs"))
	if bob := h.nextMessage(); bob.text != NoJobsReply.In(locale.English) {
		t.Fatalf("bob's /jobs = %q, want English", bob.text)
	}
}

// A reminder arrives with its snooze buttons in the language of the context it is sent in.
func TestReminderSnoozeButtonsReadInRussian(t *testing.T) {
	t.Parallel()
	h, _ := newRussianListsHarness(t)
	if _, _, err := (ReminderSender{Client: h.client}).SendReminder(locale.With(h.ctx, locale.Russian), 101, "a1b2c3d4e5f60718", "Позвонить маме"); err != nil {
		t.Fatal(err)
	}
	message := h.nextMessage()
	if labels := buttonLabels(message.keyboard); !slices.Equal(labels, []string{"Отложить на 10 мин", "Отложить на 1 час", "Завтра"}) {
		t.Fatalf("snooze buttons = %v", labels)
	}
}

// /tasks writes its title and Cancel buttons in Russian; the line under each task is the task
// package's own text.
func TestTasksReadInRussian(t *testing.T) {
	t.Parallel()
	h, runtime := newRussianListsHarness(t)

	h.push(textUpdate(1, 101, "/tasks"))
	if empty := h.nextMessage(); empty.text != NoTasksReply.In(locale.Russian) {
		t.Fatalf("empty /tasks = %q", empty.text)
	} else {
		wantRussianList(t, empty.text)
	}

	_, _, id := h.startTask(runtime, 2, "Сравнение посудомоек", "Compare current dishwashers under 800 euros.")
	receive(t, h.ctx, runtime.calls)
	h.push(textUpdate(3, 101, "/tasks"))
	var list sentMessage
	for {
		list = h.nextMessage()
		if strings.HasPrefix(list.text, "Фоновые задачи") {
			break
		}
	}
	if !strings.Contains(list.text, "1. Сравнение посудомоек\n") || !slices.Equal(buttonData(list.keyboard), []string{"tasks:cancel:" + id}) {
		t.Fatalf("/tasks = %+v", list)
	}
	if labels := buttonLabels(list.keyboard); !slices.Equal(labels, []string{"Отменить 1"}) {
		t.Fatalf("/tasks buttons = %v", labels)
	}
}

var (
	russianDay      = regexp.MustCompile(`\d{1,2} (янв|фев|мар|апр|мая|июн|июл|авг|сен|окт|ноя|дек) \d{4}`)
	russianLongDate = regexp.MustCompile(`\d{1,2} (января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря) \d{4}`)
)

// /sessions writes its title, the current mark, the date, and its buttons in Russian, and a resume
// answers in Russian with its date.
func TestSessionsReadInRussian(t *testing.T) {
	t.Parallel()
	profiles := filepath.Join(t.TempDir(), "profiles")
	h := newConversationHarnessWithOptions(t, newFixtureRuntime(t, profiles), harnessOptions{profilesDir: profiles, languages: true})
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.provision("alice", "acct-alice")
	h.start()

	h.push(textUpdate(1, 101, "/sessions"))
	if empty := h.nextMessage(); empty.text != NoSessionsReply.In(locale.Russian) {
		t.Fatalf("empty /sessions = %q", empty.text)
	}

	h.talk(2, 101, "планирование отопления")
	earlierID, _ := h.activeSession("alice")
	h.push(textUpdate(3, 101, "/new"))
	h.nextMessage()
	h.talk(4, 101, "список покупок")

	h.push(textUpdate(5, 101, "/sessions"))
	list := h.nextMessage()
	if !strings.HasPrefix(list.text, "Недавние разговоры\n\n") || !strings.Contains(list.text, "список покупок (текущий)") || !strings.Contains(list.text, "планирование отопления") || !russianDay.MatchString(list.text) {
		t.Fatalf("/sessions = %q", list.text)
	}
	if labels := buttonLabels(list.keyboard); !slices.Equal(labels, []string{"Продолжить 2"}) {
		t.Fatalf("/sessions buttons = %v", labels)
	}
	wantRussianList(t, list.text)

	h.push(callbackUpdate(6, 101, list.messageID, "sessions:resume:"+earlierID))
	h.wantAnswer(6)
	edit := h.nextEdit()
	if !strings.Contains(edit.text, "планирование отопления (текущий)") {
		t.Fatalf("/sessions after resume = %q", edit.text)
	}
	followUp := h.nextMessage()
	if !strings.HasPrefix(followUp.text, "Вы вернулись к разговору от ") || !russianLongDate.MatchString(followUp.text) || !strings.Contains(followUp.text, "Ваше следующее сообщение продолжит его.") {
		t.Fatalf("resume follow-up = %q", followUp.text)
	}
	wantRussianList(t, followUp.text)
}

// A trip card draws its labels, dates, and buttons in Russian for a context in Russian, and the
// Track press, which reads the user's own choice, answers in Russian with its reminders.
func TestTripCardReadsInRussian(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, mondayMorning)
	sim := googleSimulator(t)
	reply := &scanReply{}
	base := newConversationHarnessWithOptions(t, reply, harnessOptions{schedules: clock, google: sim, calendar: calendarSimulator(t, sim), languages: true})
	if err := base.store.SaveLanguage(base.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	base.provision("alice", "account-alice")
	base.start()
	h := &tripHarness{conversationHarness: base, clock: clock, trips: base.trips, scan: reply}
	found := h.suggest("ABC123")

	if err := (TripNotifier{Client: h.client, Chats: h.store}).SendTripCard(locale.With(h.ctx, locale.Russian), found); err != nil {
		t.Fatal(err)
	}
	card := h.nextMessage()
	for _, part := range []string{
		"Найдена поездка в почте:", "Example trip (Example City)", "с чт 1 окт 2026 по сб 3 окт 2026, время указано в часовом поясе Europe/Lisbon",
		"- Flight to Example City, чт 1 окт 07:00 (ABC123): регистрация открывается ср 30 сен 07:00 (найдено на сайте поставщика)",
		"предположительно, за 24 ч до начала", "Отслеживайте поездку, чтобы получать напоминания о регистрации",
	} {
		if !strings.Contains(card.text, part) {
			t.Fatalf("card lacks %q: %q", part, card.text)
		}
	}
	if labels := buttonLabels(card.keyboard); !slices.Equal(labels, []string{"Следить", "Игнорировать"}) {
		t.Fatalf("card buttons = %v", labels)
	}
	wantRussianList(t, card.text, "Example", "trip", "City", "Flight", "to", "Hotel", "ABC", "Europe", "Lisbon")

	tracked := h.pressCard(1, 101, card.messageID, "trip:track:"+found.ID)
	for _, part := range []string{"Отслеживается: Example trip (Example City), с чт 1 окт 2026 по сб 3 окт 2026.", "Напоминание для Flight to Example City: ср 30 сен 07:00.", "Для Example Hotel напоминания нет: регистрация уже открыта.", "Совет: попросите меня приостановить"} {
		if !strings.Contains(tracked.text, part) {
			t.Fatalf("tracked card lacks %q: %q", part, tracked.text)
		}
	}
}

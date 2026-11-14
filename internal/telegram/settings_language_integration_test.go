package telegram

import (
	"html"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/memory"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
)

// settingsLatinWords are the Latin words every Russian settings page may keep: product names and
// commands. A word that only one page's own data carries is allowed on that page alone, through
// settingsPageWords.
var settingsLatinWords = []string{
	"Notion", "NAS", "Google", "Home", "Assistant", "Grafana", "TypeSafe", "ChatGPT", "Telegram",
	"settings", "new", "memory", "privacy", "status", "model", "jobs", "events", "inbox",
}

var settingsPageWords = map[string][]string{
	"model":    {"lice"},
	"tools":    {"NS", "AeroDataBox", "aerodatabox", "com", "OVapi", "Transitous", "transitous", "org", "sources", "OpenStreetMap"},
	"events":   {"POST", "api"},
	"home":     {"Assist"},
	"services": {"Prices", "prices", "Look", "up", "Listings", "shop", "Search", "classified", "listings"},
	"root":     {"strict", "local"},
}

func wantRussianPage(t *testing.T, what, text string, allowed ...string) {
	t.Helper()
	text = html.UnescapeString(text)
	if !cyrillic.MatchString(text) {
		t.Fatalf("%s is not Russian: %q", what, text)
	}
	if latin := localetest.Latin(text, append(allowed, settingsLatinWords...)...); len(latin) != 0 {
		t.Fatalf("%s has English words %v: %q", what, latin, text)
	}
}

func withoutMonitoring(data []string) []string {
	return slices.DeleteFunc(slices.Clone(data), func(item string) bool { return strings.HasPrefix(item, "nav:monitoring") })
}

func wantRussianPageButtons(t *testing.T, what string, keyboard InlineKeyboard, allowed ...string) {
	t.Helper()
	for _, label := range buttonLabels(keyboard) {
		if label == "" || len(localetest.Latin(html.UnescapeString(label), append(allowed, settingsLatinWords...)...)) != 0 {
			t.Fatalf("%s has a button label %q with English words", what, label)
		}
	}
}

func (h *conversationHarness) ruSettings(updateID, userID int64) sentMessage {
	h.t.Helper()
	h.push(textUpdate(updateID, userID, "/settings"))
	message := h.nextMessage()
	if message.chatID != userID || !strings.HasPrefix(message.text, "Настройки\n") {
		h.t.Fatalf("settings reply = %+v", message)
	}
	return message
}

func (h *conversationHarness) ruPress(updateID, userID, messageID int64, data string) editedMessage {
	h.t.Helper()
	h.push(callbackUpdate(updateID, userID, messageID, data))
	h.wantAnswer(updateID)
	edit := h.nextEdit()
	if edit.chatID != userID || edit.messageID != messageID || !strings.HasPrefix(edit.text, "Настройки") {
		h.t.Fatalf("settings edit = %+v", edit)
	}
	return edit
}

func (h *conversationHarness) chooseRussian(principal string) {
	h.t.Helper()
	if err := h.store.SaveLanguage(h.ctx, principal, locale.Russian); err != nil {
		h.t.Fatal(err)
	}
}

type settingsPage struct {
	text string
	data []string
}

// walkSettings opens every page of /settings that the overview links to, and the page of every
// section a group links to, and returns each by its page ID.
func (h *conversationHarness) walkSettings(userID int64, update *int64, title string) map[string]settingsPage {
	h.t.Helper()
	press := func(data string, messageID int64) editedMessage {
		*update++
		h.push(callbackUpdate(*update, userID, messageID, data))
		h.wantAnswer(*update)
		edit := h.nextEdit()
		if edit.chatID != userID || !strings.HasPrefix(edit.text, title) {
			h.t.Fatalf("settings edit = %+v", edit)
		}
		return edit
	}
	*update++
	h.push(textUpdate(*update, userID, "/settings"))
	overview := h.nextMessage()
	messageID := settingsMessageID(*update)
	pages := map[string]settingsPage{"root": {text: overview.text, data: buttonData(overview.keyboard)}}
	pending := slices.Clone(pages["root"].data)
	for len(pending) > 0 {
		data := pending[0]
		pending = pending[1:]
		page, found := strings.CutPrefix(data, settingsNavPrefix+":")
		if !found || page == settingsRootPage {
			continue
		}
		if _, seen := pages[page]; seen {
			continue
		}
		edit := press(data, messageID)
		pages[page] = settingsPage{text: edit.text, data: buttonData(edit.keyboard)}
		pending = append(pending, pages[page].data...)
	}
	return pages
}

// Every settings page reads in Russian for a user who chose it, with the same buttons and
// callback data as for one who did not, and an English-speaking user in the same chat sees English.
func TestEverySettingsPageReadsInRussianWithTheSameButtons(t *testing.T) {
	t.Parallel()
	notion := notionsim.New(t)
	h, _ := newEventsHarness(t, newControlledRuntime(), harnessOptions{
		languages: true, schedules: newTestClock(t, mondayMorning), notion: notion, notionChatConnect: true, google: googleSimulator(t),
		nasRoots: nasRootsFor(t, true), locations: true, web: true, ns: nsGateway(t), flights: flightGateway(t, &atomic.Bool{}),
		homeAssistant: homeAssistantGateway(t), grafana: grafanaGateway(t), bus: nsGateway(t), busContact: "ops@example.org", typeSafe: true,
		mcp: []config.MCPServer{keyedServer("prices", newListingsServer(t)), sharedServer("shop", newListingsServer(t))},
	})
	h.chooseRussian("alice")
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()

	update := int64(0)
	russian := h.walkSettings(101, &update, "Настройки")
	english := h.walkSettings(202, &update, "Settings")
	if len(russian) < 10 {
		t.Fatalf("the walk found only %d pages: %v", len(russian), russian)
	}
	for id, page := range russian {
		other, found := english[id]
		if !found && id != "memory" && id != "monitoring" {
			t.Fatalf("page %q is shown only to the Russian user", id)
		}
		if found && id != "home" && !slices.Equal(withoutMonitoring(page.data), withoutMonitoring(other.data)) {
			t.Fatalf("page %q: callback data %v in Russian, %v in English", id, page.data, other.data)
		}
		wantRussianPage(t, "page "+id, page.text, settingsPageWords[id]...)
	}
	if !strings.Contains(russian["root"].text, "Язык: русский") || !strings.Contains(russian["tools"].text, "Разбор почты: выключен") ||
		!strings.Contains(russian["tools"].text, "Оповещения: выключены") || !strings.Contains(russian["events"].text, "Источники событий позволяют") ||
		!strings.Contains(russian["safety"].text, "Проверки безопасности (TypeSafe): выключены") || !strings.Contains(russian["widgets"].text, "Виджеты: выключены") ||
		!strings.Contains(russian["services"].text, "Сервисы вне дома") || !strings.Contains(russian["memory"].text, "Память: включена") {
		t.Fatalf("a distinctive phrase is missing from the Russian pages: %v", russian)
	}
	if !strings.HasPrefix(english["tools"].text, "Settings › Everyday tools") || strings.Contains(english["root"].text, "Язык: русский") {
		t.Fatalf("the English user's pages = %v", english)
	}
}

func TestRussianSettingsPagesKeepTheirButtonsInRussian(t *testing.T) {
	t.Parallel()
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{
		languages: true, schedules: newTestClock(t, mondayMorning), nasRoots: nasRootsFor(t, true), web: true, ns: nsGateway(t),
	})
	h.chooseRussian("alice")
	h.provision("alice", "acct-alice")
	h.start()
	overview := h.ruSettings(1, 101)
	wantRussianPageButtons(t, "overview", overview.keyboard)
	for i, page := range []string{"model", "home", "tools", "memory", "widgets"} {
		edit := h.ruPress(int64(2+i), 101, settingsMessageID(1), settingsNavPrefix+":"+page)
		wantRussianPageButtons(t, "page "+page, edit.keyboard)
		if !slices.Contains(buttonLabels(edit.keyboard), "‹ Назад") {
			t.Fatalf("page %s has no Russian Back button: %v", page, buttonLabels(edit.keyboard))
		}
	}
}

// Notion, Google, and the NAS: the connection steps, their outcomes, and the switches.
func TestAccountsAndFoldersReadInRussian(t *testing.T) {
	t.Parallel()
	notion := notionsim.New(t)
	notion.AddToken(aliceNotionToken, aliceWorkspace)
	sim := googleSimulator(t)
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{
		languages: true, schedules: newTestClock(t, mondayMorning), notion: notion, notionChatConnect: true, google: sim, nasRoots: nasRootsFor(t, true),
	})
	h.chooseRussian("alice")
	h.provision("alice", "acct-alice")
	h.start()

	overview := h.ruSettings(1, 101)
	if !strings.Contains(overview.text, "Аккаунты: Google не подключён, Notion не подключён") || !strings.Contains(overview.text, "Дом и файлы: NAS сохранение выключено") {
		t.Fatalf("overview = %q", overview.text)
	}
	messageID := settingsMessageID(1)
	accounts := h.ruPress(2, 101, messageID, "nav:accounts")
	if !strings.HasPrefix(accounts.text, "Настройки › Аккаунты\n\nGoogle: не подключён\nNotion: не подключён") || !slices.Equal(buttonData(accounts.keyboard), []string{"nav:accounts.google", "nav:accounts.notion", "nav:root"}) {
		t.Fatalf("accounts = %+v", accounts)
	}

	notionPage := h.ruPress(3, 101, messageID, "nav:accounts.notion")
	if !strings.Contains(notionPage.text, "Notion: не подключён.") || !slices.Equal(buttonData(notionPage.keyboard), []string{"notion:connect", "nav:accounts"}) || buttonLabels(notionPage.keyboard)[0] != "Подключить Notion" {
		t.Fatalf("notion page = %+v", notionPage)
	}
	h.ruPress(4, 101, messageID, "notion:connect")
	instructions := h.nextMessage()
	if !strings.HasPrefix(instructions.text, "Отправь токен Notion следующим сообщением") {
		t.Fatalf("notion instructions = %q", instructions.text)
	}
	h.push(textUpdate(5, 101, "not a token"))
	if reply := h.nextMessage(); !strings.HasPrefix(reply.text, "Это не похоже на токен Notion") {
		t.Fatalf("not a token reply = %q", reply.text)
	}
	h.ruPress(6, 101, messageID, "notion:connect")
	h.nextMessage()
	h.push(textUpdate(7, 101, aliceNotionToken))
	h.nextDelete()
	if reply := h.nextMessage(); !strings.Contains(reply.text, "Notion подключён к рабочему пространству \"Alice Home\" как \"Home Agent\" (внутреннее подключение).") {
		t.Fatalf("connected reply = %q", reply.text)
	}
	connected := h.ruPress(8, 101, messageID, "nav:accounts.notion")
	if !strings.Contains(connected.text, "Notion: подключён к рабочему пространству") || !strings.Contains(connected.text, "запись выключена.") ||
		!slices.Equal(buttonData(connected.keyboard), []string{"notion:writes-on", "notion:disconnect", "nav:accounts"}) ||
		!slices.Equal(buttonLabels(connected.keyboard)[:2], []string{"Включить запись в Notion", "Отключить Notion"}) {
		t.Fatalf("connected page = %+v", connected)
	}
	enabled := h.ruPress(9, 101, messageID, "notion:writes-on")
	if !strings.Contains(enabled.text, "Запись в Notion включена.") || buttonLabels(enabled.keyboard)[0] != "Выключить запись в Notion" {
		t.Fatalf("writes on = %+v", enabled)
	}
	if overview := h.ruPress(10, 101, messageID, "nav:root"); !strings.Contains(overview.text, "Notion запись включена") {
		t.Fatalf("overview after writes on = %q", overview.text)
	}

	home := h.ruPress(11, 101, messageID, "nav:home")
	if !strings.Contains(home.text, "NAS: home, inbox. Сохранение: выключено, в inbox.") || buttonLabels(home.keyboard)[0] != "Включить сохранение на NAS" {
		t.Fatalf("home page = %+v", home)
	}
	if on := h.ruPress(12, 101, messageID, "nas:writes-on"); !strings.Contains(on.text, "Сохранение на NAS включено.") || !slices.Contains(buttonData(on.keyboard), "nas:writes-off") {
		t.Fatalf("nas on = %+v", on)
	}

	link := h.russianGoogleSignIn(13, 101)
	reply := h.pasteRedirect(15, 101, sim.Consent(link, aliceMailbox, googlesim.Grant{Mail: true, Calendar: true}))
	if reply != "Аккаунт alice@example.com подключён только для чтения: почта: включено, календарь: включено." {
		t.Fatalf("sign-in reply = %q", reply)
	}
	again := h.pasteRedirect(16, 101, sim.Consent(link, aliceMailbox, googlesim.Grant{Mail: true}))
	if !strings.HasPrefix(again, "Этот адрес входа не соответствует ожидающему входу") {
		t.Fatalf("second sign-in reply = %q", again)
	}
	google := h.ruPress(17, 101, messageID, "nav:accounts.google")
	if !strings.HasPrefix(google.text, "Настройки › Аккаунты › Google\n\nАккаунты Google:\nalice@example.com: почта: включено, календарь: включено.") {
		t.Fatalf("google page = %q", google.text)
	}
	labels := buttonLabels(google.keyboard)
	for _, want := range []string{"Подключить аккаунт Google", "Выключить почту", "Выключить календарь", "Отключить"} {
		if !slices.Contains(labels, want) {
			t.Fatalf("google buttons %v lack %q", labels, want)
		}
	}
	mailOff := ""
	for _, data := range buttonData(google.keyboard) {
		if strings.HasPrefix(data, "google:mail-off:") {
			mailOff = data
		}
	}
	if mailOff == "" {
		t.Fatalf("no mail switch in %v", buttonData(google.keyboard))
	}
	if off := h.ruPress(18, 101, messageID, mailOff); !strings.Contains(off.text, "почта: выключено, календарь: включено.") || !slices.Contains(buttonLabels(off.keyboard), "Включить почту") {
		t.Fatalf("mail off = %+v", off)
	}
	if overview := h.ruPress(19, 101, messageID, "nav:root"); !strings.Contains(overview.text, "Google 1 аккаунт") {
		t.Fatalf("overview with an account = %q", overview.text)
	}
	tools := h.ruPress(20, 101, messageID, "nav:tools")
	if !strings.Contains(tools.text, "Разбор почты: выключен. Он читает твою почту, поэтому сначала включи «Почту»") {
		t.Fatalf("tools without mail = %q", tools.text)
	}
	wantRussianPage(t, "tools", tools.text)
}

func (h *conversationHarness) russianGoogleSignIn(updateID, userID int64) string {
	h.t.Helper()
	settings := h.ruSettings(updateID, userID)
	h.ruPress(updateID+1, userID, settingsMessageID(updateID), "google:connect")
	instructions := h.nextMessage()
	if instructions.chatID != userID || !strings.HasPrefix(instructions.text, "Чтобы подключить аккаунт Google только для чтения:") || len(settings.text) == 0 {
		h.t.Fatalf("sign-in instructions = %+v", instructions)
	}
	return html.UnescapeString(instructions.text[strings.LastIndex(instructions.text, "\n")+1:])
}

// The Services page: the state of each service, the key steps, and the notes after each press.
func TestServicesPageAndKeyStepsReadInRussian(t *testing.T) {
	t.Parallel()
	prices := newListingsServer(t)
	prices.RequireHeader("Authorization", "Bearer "+aliceMCPKey)
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{
		languages: true, clock: clock, mcp: []config.MCPServer{keyedServer("prices", prices)},
	})
	h.chooseRussian("alice")
	h.provision("alice", "account-alice")
	h.start()

	h.ruSettings(1, 101)
	messageID := settingsMessageID(1)
	page := h.ruPress(2, 101, messageID, "nav:services")
	if !strings.Contains(page.text, "Prices prices: Выключен") || !slices.Equal(buttonData(page.keyboard), []string{"mcp:on:prices", "mcp:connect:prices", "nav:root"}) ||
		!slices.Equal(buttonLabels(page.keyboard)[:2], []string{"Включить Prices prices", "Подключить Prices prices"}) {
		t.Fatalf("services page = %+v", page)
	}
	h.ruPress(3, 101, messageID, "mcp:connect:prices")
	instructions := h.nextMessage()
	if !strings.HasPrefix(instructions.text, "Отправь твой ключ для Prices prices следующим сообщением до 06:10.") {
		t.Fatalf("instructions = %q", instructions.text)
	}
	h.push(textUpdate(4, 101, "where is my key?"))
	h.wantDeleted(101, 4)
	if reply := h.nextMessage(); !strings.HasPrefix(reply.text, "Это не похоже на ключ для Prices prices") {
		t.Fatalf("not a key reply = %q", reply.text)
	}
	h.ruPress(5, 101, messageID, "mcp:connect:prices")
	h.nextMessage()
	h.push(textUpdate(6, 101, aliceMCPKey))
	h.wantDeleted(101, 6)
	if reply := h.nextMessage(); reply.text != "Сервис Prices prices подключён с твоим ключом и включён." {
		t.Fatalf("connected reply = %q", reply.text)
	}
	h.ruSettings(7, 101)
	connected := h.ruPress(8, 101, settingsMessageID(7), "nav:services")
	if !strings.Contains(connected.text, "Prices prices: Включён") || !slices.Equal(buttonLabels(connected.keyboard)[:2], []string{"Выключить Prices prices", "Отключить Prices prices"}) ||
		!slices.Equal(buttonData(connected.keyboard), []string{"mcp:off:prices", "mcp:disconnect:prices", "nav:root"}) {
		t.Fatalf("connected page = %+v", connected)
	}
	privacy := h.ruPrivacy(9, 101)
	if !strings.Contains(privacy, "Prices prices: то, что ты спрашиваешь у сервиса, уходит ему") {
		t.Fatalf("privacy = %q", privacy)
	}
	gone := h.ruPress(10, 101, settingsMessageID(7), "mcp:disconnect:prices")
	if !strings.Contains(gone.text, "Твой ключ для Prices prices удалён, сервис выключен.") || !strings.Contains(gone.text, "Prices prices: Выключен") {
		t.Fatalf("after disconnecting = %q", gone.text)
	}
}

func (h *conversationHarness) ruPrivacy(updateID, userID int64) string {
	h.t.Helper()
	h.push(textUpdate(updateID, userID, "/privacy"))
	return h.nextMessage().text
}

var eventsLastUsed = regexp.MustCompile(`последнее событие \d{1,2} [а-я]{3} \d\d:\d\d\.`)

// Event sources: adding one, its token message, the page with its buttons and last event, and the
// note after each button.
func TestEventSourcesReadInRussian(t *testing.T) {
	t.Parallel()
	h, url := newEventsHarness(t, newControlledRuntime(), harnessOptions{languages: true})
	h.chooseRussian("alice")
	h.provision("alice", "acct-alice")
	h.start()

	h.ruSettings(1, 101)
	messageID := settingsMessageID(1)
	empty := h.ruPress(2, 101, messageID, "nav:events")
	if !strings.Contains(empty.text, "Источников событий пока нет.") || buttonLabels(empty.keyboard)[0] != "Добавить источник событий" || buttonData(empty.keyboard)[0] != "events:add" {
		t.Fatalf("empty page = %+v", empty)
	}
	h.ruPress(3, 101, messageID, "events:add")
	if prompt := h.nextMessage(); !strings.HasPrefix(prompt.text, "Отправь название нового источника событий") {
		t.Fatalf("prompt = %q", prompt.text)
	}
	h.push(textUpdate(4, 101, strings.Repeat("x", 41)))
	if reply := h.nextMessage(); !strings.HasPrefix(reply.text, "Такое название не подходит") {
		t.Fatalf("bad name reply = %q", reply.text)
	}
	h.ruPress(5, 101, messageID, "events:add")
	h.nextMessage()
	h.push(textUpdate(6, 101, "Backups"))
	token := h.nextMessage()
	if !strings.HasPrefix(token.text, "Токен источника событий <b>Backups</b>:\n<code>hae_") || !strings.Contains(token.text, "Отправляй его как <code>Authorization: Bearer</code>") {
		t.Fatalf("token message = %q", token.text)
	}
	if created := h.nextMessage(); !strings.HasPrefix(created.text, "Источник событий \"Backups\" настроен и пересылает события как есть.") {
		t.Fatalf("created reply = %q", created.text)
	}
	secret := eventTokenPattern.FindString(token.text)

	id := h.eventSourceID("alice", "Backups")
	page := h.ruPress(7, 101, messageID, "nav:events")
	if !strings.Contains(page.text, "• Backups: пересылка, включён.") ||
		!slices.Equal(buttonData(page.keyboard), []string{"events:add", "label", "events:mode:" + id, "events:pause:" + id, "events:token:" + id, "events:delete:" + id, "nav:root"}) ||
		!slices.Equal(buttonLabels(page.keyboard), []string{"Добавить источник событий", "Backups", "Оценивать", "Пауза", "Новый токен", "Удалить", "‹ Назад"}) {
		t.Fatalf("page with a source = %+v", page)
	}
	if status, _ := postEvent(t, url, "Bearer "+secret, eventBody("Backup done", "", "info", "")); status != http.StatusAccepted {
		t.Fatalf("event post = %d", status)
	}
	h.nextMessage()
	if used := h.ruPress(8, 101, messageID, "nav:events"); !eventsLastUsed.MatchString(used.text) {
		t.Fatalf("page after an event = %q", used.text)
	}
	if assessed := h.ruPress(9, 101, messageID, "events:mode:"+id); !strings.Contains(assessed.text, "теперь поручает мне оценивать каждое событие") ||
		!strings.Contains(assessed.text, "оценка, включён") || buttonLabels(assessed.keyboard)[2] != "Пересылать" {
		t.Fatalf("after assess = %+v", assessed)
	}
	if paused := h.ruPress(10, 101, messageID, "events:pause:"+id); !strings.Contains(paused.text, "на паузе: его события отклоняются, пока ты не продолжишь.") || buttonLabels(paused.keyboard)[3] != "Продолжить" {
		t.Fatalf("after pause = %+v", paused)
	}
	if resumed := h.ruPress(11, 101, messageID, "events:resume:"+id); !strings.Contains(resumed.text, "снова принимает события.") {
		t.Fatalf("after resume = %q", resumed.text)
	}
	h.push(textUpdate(12, 101, secret))
	h.wantDeleted(101, 12)
	if reply := h.nextMessage(); !strings.HasPrefix(reply.text, "Это было похоже на токен источника событий") {
		t.Fatalf("token guard reply = %q", reply.text)
	}
	if deleted := h.ruPress(13, 101, messageID, "events:delete:"+id); !strings.Contains(deleted.text, "удалён; его токен больше не работает.") {
		t.Fatalf("after delete = %q", deleted.text)
	}
	if gone := h.ruPress(14, 101, messageID, "events:delete:"+id); !strings.Contains(gone.text, "Этого источника событий больше нет.") {
		t.Fatalf("after deleting again = %q", gone.text)
	}
}

// /memory and its Memory page: the list with its headings and page titles, the buttons, the
// confirmation with the count in the right form, and the switches.
func TestMemoryPagesReadInRussian(t *testing.T) {
	t.Parallel()
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{languages: true, schedules: newTestClock(t, mondayMorning)})
	h.chooseRussian("alice")
	h.provision("alice", "acct-alice")
	h.start()

	h.push(textUpdate(1, 101, "/memory"))
	if empty := h.nextMessage(); !strings.HasPrefix(empty.text, "Я ничего о тебе не помню.") {
		t.Fatalf("empty /memory = %q", empty.text)
	}
	h.seedMemory("alice", "aaaaaaa1", memory.KindInstruction, "answer briefly")
	for n := 2; n <= 10; n++ {
		h.seedMemory("alice", "aaaaaaa"+string(rune('a'+n)), memory.KindFact, "fact number "+string(rune('0'+n%10)))
	}
	if err := h.store.AddMemory(h.ctx, memory.Entry{ID: "learned1", PrincipalID: "alice", Kind: memory.KindFact, Text: "likes tea", Source: memory.SourceLearned, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	h.push(textUpdate(2, 101, "/memory"))
	list := h.nextMessage()
	for _, want := range []string{"Память (страница 1 из 2)\n", "Инструкции\n1. answer briefly", "Факты\n2. "} {
		if !strings.Contains(list.text, want) {
			t.Fatalf("/memory lacks %q: %s", want, list.text)
		}
	}
	data := buttonData(list.keyboard)
	for _, want := range []string{"mem:del:aaaaaaa1:1", "mem:page:2", "mem:ask:1"} {
		if !slices.Contains(data, want) {
			t.Fatalf("buttons %v lack %s", data, want)
		}
	}
	labels := buttonLabels(list.keyboard)
	for _, want := range []string{"Удалить 1", "Далее ›", "Забыть всё"} {
		if !slices.Contains(labels, want) {
			t.Fatalf("labels %v lack %q", labels, want)
		}
	}
	h.push(callbackUpdate(3, 101, settingsMessageID(2), "mem:page:2"))
	h.wantAnswer(3)
	second := h.nextEdit()
	if !strings.Contains(second.text, "Память (страница 2 из 2)") || !strings.Contains(second.text, "(из опыта)") || !slices.Contains(buttonLabels(second.keyboard), "‹ Назад") {
		t.Fatalf("page 2 = %+v", second)
	}
	wantRussianPage(t, "page 2", second.text, "fact", "number", "likes", "tea", "answer", "briefly")

	h.push(callbackUpdate(4, 101, settingsMessageID(2), "mem:ask:1"))
	h.wantAnswer(4)
	ask := h.nextEdit()
	if !strings.HasPrefix(ask.text, "Забыть всё (11 записей)?") || !slices.Equal(buttonData(ask.keyboard), []string{"mem:clear:1", "mem:back:1"}) ||
		!slices.Equal(buttonLabels(ask.keyboard), []string{"Да, забыть всё", "Закрыть"}) {
		t.Fatalf("confirmation = %+v", ask)
	}
	h.push(callbackUpdate(5, 101, settingsMessageID(2), "mem:del:nobody:1"))
	h.wantAnswer(5)
	h.nextEdit()
	if gone := h.nextMessage(); gone.text != "Этой записи больше нет." {
		t.Fatalf("gone reply = %q", gone.text)
	}
	h.push(callbackUpdate(6, 101, settingsMessageID(2), "mem:clear:1"))
	h.wantAnswer(6)
	if cleared := h.nextEdit(); !strings.HasPrefix(cleared.text, "Я ничего о тебе не помню.") {
		t.Fatalf("after clearing = %q", cleared.text)
	}
	if note := h.nextMessage(); note.text != "Всё забыто." {
		t.Fatalf("cleared note = %q", note.text)
	}

	h.ruSettings(7, 101)
	page := h.ruPress(8, 101, settingsMessageID(7), "nav:memory")
	for _, want := range []string{"Память: включена (по умолчанию).", "Учиться на 👎: включено", "Запоминать процедуры: включено", "Еженедельный обзор: выключен"} {
		if !strings.Contains(page.text, want) {
			t.Fatalf("memory page lacks %q: %s", want, page.text)
		}
	}
	for _, want := range []string{"Выключить память", "Не учиться на 👎", "Не запоминать процедуры", "Включить еженедельный обзор"} {
		if !slices.Contains(buttonLabels(page.keyboard), want) {
			t.Fatalf("memory labels %v lack %q", buttonLabels(page.keyboard), want)
		}
	}
	if off := h.ruPress(9, 101, settingsMessageID(7), "memory:off"); !strings.Contains(off.text, "Память: выключена.") || buttonLabels(off.keyboard)[0] != "Включить память" {
		t.Fatalf("memory off = %+v", off)
	}
	h.push(textUpdate(10, 101, "/memory"))
	if empty := h.nextMessage(); !strings.Contains(empty.text, "Память выключена: ничего отсюда не используется") {
		t.Fatalf("/memory while off = %q", empty.text)
	}
}

// Heads-up and Inbox intake: the pages, their buttons, and the notes.
func TestHeadsUpAndInboxPagesReadInRussian(t *testing.T) {
	t.Parallel()
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{languages: true, schedules: newTestClock(t, mondayMorning), google: googleSimulator(t)})
	h.chooseRussian("alice")
	h.provision("alice", "acct-alice")
	h.start()

	h.ruSettings(1, 101)
	messageID := settingsMessageID(1)
	page := h.ruPress(2, 101, messageID, "nav:tools")
	if !strings.Contains(page.text, "Оповещения: выключены.") || !slices.Contains(buttonLabels(page.keyboard), "Включить оповещения") || !slices.Contains(buttonData(page.keyboard), "headsup:on") {
		t.Fatalf("tools page = %+v", page)
	}
	on := h.ruPress(3, 101, messageID, "headsup:on")
	if !strings.Contains(on.text, "Оповещения: включены.") || !slices.Contains(buttonLabels(on.keyboard), "Выключить оповещения") || !slices.Contains(buttonData(on.keyboard), "headsup:off") {
		t.Fatalf("after on = %+v", on)
	}
	if overview := h.ruPress(4, 101, messageID, "nav:root"); !strings.Contains(overview.text, "Оповещения включено") {
		t.Fatalf("overview = %q", overview.text)
	}
	if off := h.ruPress(5, 101, messageID, "headsup:off"); !strings.Contains(off.text, "Оповещения: выключены.") {
		t.Fatalf("after off = %q", off.text)
	}
	wantRussianPage(t, "tools page", page.text)
}

// Shared locations: the page with and without a location, the forget note, and the refusal when
// the assistant has no location support.
func TestLocationPageReadsInRussian(t *testing.T) {
	t.Parallel()
	h, _, clock := locationHarness(t, func(options *harnessOptions) { options.languages = true })
	h.chooseRussian("alice")
	h.start()

	h.ruSettings(1, 101)
	messageID := settingsMessageID(1)
	page := h.ruPress(2, 101, messageID, "nav:location")
	if !strings.Contains(page.text, "Местоположение: не передано.\nПередай местоположение или трансляцию местоположения в этом чате") || slices.Contains(buttonData(page.keyboard), "location:forget") {
		t.Fatalf("page before sharing = %+v", page)
	}
	h.push(pinUpdate(3, 101, clock.Now(), 52.16612, 4.48105))
	h.nextMessage()
	shared := h.ruPress(4, 101, messageID, "nav:location")
	if !strings.Contains(shared.text, "Местоположение: ") || !strings.Contains(shared.text, "Твоё последнее переданное местоположение используется в твоём разговоре") ||
		!slices.Equal(buttonData(shared.keyboard), []string{"location:forget", "nav:root"}) || buttonLabels(shared.keyboard)[0] != "Забыть моё местоположение" {
		t.Fatalf("page after sharing = %+v", shared)
	}
	if forgotten := h.ruPress(5, 101, messageID, "location:forget"); !strings.Contains(forgotten.text, "Местоположение забыто.") {
		t.Fatalf("after forgetting = %q", forgotten.text)
	}
	if overview := h.ruPress(6, 101, messageID, "nav:root"); !strings.Contains(overview.text, "Местоположение: не передано") {
		t.Fatalf("overview = %q", overview.text)
	}
}

func TestLocationSharingRefusalsReadInRussian(t *testing.T) {
	t.Parallel()
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{languages: true})
	h.chooseRussian("alice")
	h.provision("alice", "acct-alice")
	h.start()
	h.push(pinUpdate(1, 101, time.Now(), 52.16612, 4.48105))
	h.wantReply(101, 1, "Передача местоположения на этом ассистенте не включена.")
}

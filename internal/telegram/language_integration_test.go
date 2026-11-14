package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// A user who never chose has English, and the Language page is the first thing /settings shows.
// Choosing Русский redraws the page in Russian at once, sets that chat's slash menu, and changes
// the replies after it for that user alone.
func TestLanguagePageSwitchesTheInterfaceToRussian(t *testing.T) {
	t.Parallel()
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{languages: true})
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()
	for _, scope := range []string{"", `{"type":"all_private_chats"}`} {
		registration := receive(t, h.ctx, h.registrations)
		if got := strings.Join(registration.scope, ""); got != scope || registration.description("new") != "Start a fresh conversation" {
			t.Fatalf("shared registration = %+v, want scope %q in English", registration, scope)
		}
	}

	settings := h.requestSettings(1, 101)
	if !strings.HasPrefix(settings.text, localetest.English(SettingsTitle)+"\n\nLanguage: English\n") {
		t.Fatalf("overview = %q, want the Language line first", settings.text)
	}
	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, localetest.English(NoSessionReply))

	h.push(callbackUpdate(3, 101, settings.messageID, "nav:language"))
	h.wantAnswer(3)
	page := h.nextEdit()
	if !strings.HasPrefix(page.text, "Settings › Language\n\nInterface language: English.") || strings.Join(buttonData(page.keyboard), " ") != "language:en language:ru nav:root" {
		t.Fatalf("language page = %+v", page)
	}
	if labels := buttonLabels(page.keyboard); len(labels) < 2 || labels[0] != "English" || labels[1] != "Русский" {
		t.Fatalf("language buttons = %v, want each written in its own language", labels)
	}

	h.push(callbackUpdate(4, 101, settings.messageID, "language:ru"))
	h.wantAnswer(4)
	chosen := h.nextEdit()
	if !strings.HasPrefix(chosen.text, "Настройки › Язык\n\nЯзык интерфейса: русский.") || len(localetest.Latin(chosen.text)) != 0 {
		t.Fatalf("page after choosing Russian = %q", chosen.text)
	}
	if language, found, err := h.store.LoadLanguage(h.ctx, "alice"); err != nil || !found || language != locale.Russian {
		t.Fatalf("stored language = %v %v %v", language, found, err)
	}
	menu := receive(t, h.ctx, h.registrations)
	if got := strings.Join(menu.scope, ""); got != `{"type":"chat","chat_id":101}` || menu.description("new") != "Начать новый разговор" {
		t.Fatalf("chat menu = %+v, want the Russian list in alice's chat", menu)
	}

	h.push(callbackUpdate(5, 101, settings.messageID, "nav:root"))
	h.wantAnswer(5)
	if overview := h.nextEdit(); !strings.HasPrefix(overview.text, "Настройки\n\nЯзык: русский\n") {
		t.Fatalf("overview in Russian = %q", overview.text)
	}
	h.push(textUpdate(6, 101, "/new"), textUpdate(7, 101, "/help"))
	h.wantReply(101, 6, "Пока нет разговора, который можно сбросить.")
	help := h.nextMessage()
	if !strings.HasPrefix(help.text, "Доступные команды:\n/backlog: ") || !strings.Contains(help.text, "/new: Начать новый разговор") {
		t.Fatalf("help in Russian = %q", help.text)
	}

	h.push(textUpdate(8, 202, "/new"))
	h.wantReply(202, 8, localetest.English(NoSessionReply))
	if language, found, err := h.store.LoadLanguage(h.ctx, "bob"); err != nil || found || language != locale.English {
		t.Fatalf("bob's language = %v %v %v, want none", language, found, err)
	}

	h.push(callbackUpdate(9, 101, settings.messageID, "language:en"))
	h.wantAnswer(9)
	if back := h.nextEdit(); !strings.HasPrefix(back.text, "Settings › Language\n\nInterface language: English.") {
		t.Fatalf("page after choosing English = %q", back.text)
	}
	if menu := receive(t, h.ctx, h.registrations); !menu.deleted || strings.Join(menu.scope, "") != `{"type":"chat","chat_id":101}` {
		t.Fatalf("chat menu after choosing English = %+v", menu)
	}
	h.push(textUpdate(10, 101, "/new"))
	h.wantReply(101, 10, localetest.English(NoSessionReply))
}

// A choice made earlier survives a restart: the service sets the Russian list in that user's chat
// when it starts, and leaves the shared scopes and everyone else on English.
func TestStartSetsTheMenuOfEachUserWhoChoseRussian(t *testing.T) {
	t.Parallel()
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{languages: true})
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()
	var shared, chat []commandRegistration
	for range 3 {
		registration := receive(t, h.ctx, h.registrations)
		if strings.Contains(strings.Join(registration.scope, ""), `"chat"`) {
			chat = append(chat, registration)
		} else {
			shared = append(shared, registration)
		}
	}
	if len(shared) != 2 || len(chat) != 1 {
		t.Fatalf("registrations: %d shared, %d per chat, want 2 and 1", len(shared), len(chat))
	}
	for _, registration := range shared {
		if registration.description("new") != "Start a fresh conversation" {
			t.Fatalf("shared registration = %+v, want English", registration)
		}
	}
	if chat[0].description("new") != "Начать новый разговор" || !strings.Contains(strings.Join(chat[0].scope, ""), "101") {
		t.Fatalf("chat registration = %+v, want the Russian list in alice's chat", chat[0])
	}
	h.push(textUpdate(1, 101, "/new"), textUpdate(2, 202, "/new"))
	h.wantReply(101, 1, "Пока нет разговора, который можно сбросить.")
	h.wantReply(202, 2, localetest.English(NoSessionReply))
}

// The export carries the language, a restore brings it back, and one already chosen where the file
// is restored is left as it is.
func TestExportCarriesTheLanguageAndRestoreKeepsAChoiceAlreadyMade(t *testing.T) {
	t.Parallel()
	source := newExportHarness(t)
	if err := source.store.SaveLanguage(source.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	data := source.takeExport(1, 101)
	if !slices.ContainsFunc(readZip(t, data), func(entry zipEntry) bool {
		return entry.name == "language.json" && strings.Contains(string(entry.body), `"principal_languages"`)
	}) {
		t.Fatal("the export has no language section")
	}

	target := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{watches: &stubEvaluator{}, locations: true, languages: true})
	target.provision("alice", "acct-alice")
	target.start()
	copyKey(t, source, target)
	for range 2 {
		receive(t, target.ctx, target.registrations)
	}
	preview, outcome := target.restore(1, 101, data)
	if !strings.Contains(preview, "interface language: Русский") || !strings.Contains(outcome, "Restored") {
		t.Fatalf("preview:\n%s\noutcome: %s", preview, outcome)
	}
	if language, found, err := target.store.LoadLanguage(target.ctx, "alice"); err != nil || !found || language != locale.Russian {
		t.Fatalf("language after the restore = %v %v %v", language, found, err)
	}

	if menu := receive(t, target.ctx, target.registrations); menu.description("new") != "Начать новый разговор" || strings.Join(menu.scope, "") != `{"type":"chat","chat_id":101}` {
		t.Fatalf("chat menu after the restore = %+v", menu)
	}

	chosen := newExportHarness(t)
	copyKey(t, source, chosen)
	if err := chosen.store.SaveLanguage(chosen.ctx, "alice", locale.English); err != nil {
		t.Fatal(err)
	}
	if again := chosen.restorePreview(1, 101, data); !strings.Contains(again, "Everything in this file is already here") {
		t.Fatalf("preview with a language already chosen:\n%s", again)
	}
	if language, found, err := chosen.store.LoadLanguage(chosen.ctx, "alice"); err != nil || !found || language != locale.English {
		t.Fatalf("language after the restore = %v %v %v, want the choice kept", language, found, err)
	}
}

func (r commandRegistration) description(command string) string {
	for _, registered := range r.commands {
		if registered.Command == command {
			return registered.Description
		}
	}
	return ""
}

func buttonLabels(keyboard InlineKeyboard) []string {
	var labels []string
	for _, row := range keyboard {
		for _, button := range row {
			labels = append(labels, button.Text)
		}
	}
	return labels
}

var cyrillic = regexp.MustCompile(`\p{Cyrillic}`)

// What the model reads stays English for a Russian user: a chat turn reaches the runtime with
// nothing written in Russian by the assistant, and a failed restore's outcome, which the model is
// told of, is the English text.
func TestRussianUserSeesRussianWhileTheModelReadsEnglish(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{languages: true, watches: &stubEvaluator{}, locations: true})
	h.provision("alice", "acct-alice")
	h.start()
	for range 2 {
		receive(t, h.ctx, h.registrations)
	}
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}

	h.push(textUpdate(1, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	request := call.request
	if text := fmt.Sprint(request.Message, request.Memory, request.Jobs, request.Tools.Guidance); cyrillic.MatchString(text) {
		t.Fatalf("the turn carries Russian: %q", text)
	}
	for _, definition := range request.Tools.Definitions {
		if text := fmt.Sprint(definition); cyrillic.MatchString(text) {
			t.Fatalf("tool %s carries Russian: %q", definition.Name, text)
		}
	}
	call.answer <- agentruntime.RunResult{Response: "hi"}
	h.wantReply(101, 1, "hi")

	_, err := h.assembly.Exports.ExecuteApproved(locale.With(h.ctx, locale.Russian), approval.Record{
		Tool: export.RestoreTool, PrincipalID: "alice", Arguments: json.RawMessage(`{"digest":"0123"}`),
	})
	var failed *approval.ActionError
	if !errors.As(err, &failed) || failed.Text != localetest.English(export.RestoreGoneText) {
		t.Fatalf("restore outcome for a Russian user = %v, want the English text", err)
	}
}

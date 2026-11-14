package telegram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/task"
)

// russianCommandsAllowed are the Latin words a Russian command reply may keep: product names,
// commands, model names and effort levels, which are values rather than words of the interface.
var russianCommandsAllowed = []string{
	"Home", "Agent", "Telegram", "ChatGPT", "new", "model", "default", "settings", "status",
	"gpt", "test", "sol", "plus", "Sol", "Test", "Terra", "Code", "terra", "codex",
	"minimal", "low", "medium", "high", "xhigh", "off", "jobs", "memory", "lt", "gt", "nope", "sideways", "max",
}

func newRussianCommandsHarness(t *testing.T) (*conversationHarness, *controlledRuntime) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{topics: true, languages: true})
	h.provision("alice", "acct-alice")
	if err := os.WriteFile(filepath.Join(h.profilesDir, "alice", "models-store.json"), []byte(modelCatalogFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.service.model.Usage = successfulUsage()
	h.service.model.Tasks = fakeTaskModels{pair: agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "medium"}, fallback: task.FallbackUsage}
	h.service.model.Location = time.FixedZone("household", 2*3600)
	h.start()
	for range 3 {
		receive(t, h.ctx, h.registrations)
	}
	return h, runtime
}

func wantRussian(t *testing.T, text string, phrases ...string) {
	t.Helper()
	for _, phrase := range phrases {
		if !strings.Contains(text, phrase) {
			t.Fatalf("reply lacks %q:\n%s", phrase, text)
		}
	}
	if latin := localetest.Latin(text, russianCommandsAllowed...); len(latin) != 0 {
		t.Fatalf("reply keeps words written only in Latin letters %v:\n%s", latin, text)
	}
}

// /status and /usage read in Russian: the lines, the usage windows and the reset times in the
// Russian date style, and the last turn once one ran.
func TestStatusAndUsageReadInRussian(t *testing.T) {
	t.Parallel()
	h, runtime := newRussianCommandsHarness(t)

	h.push(textUpdate(1, 101, "/status"))
	first := h.nextMessage().text
	wantRussian(t, first,
		"Home Agent на связи. Доступ к Telegram активен.",
		"Модель здесь: gpt-test, глубина рассуждений medium (по умолчанию).",
		"Фоновые задачи: gpt-6.1-sol, глубина рассуждений medium.\nМодель для задач сейчас недоступна на вашем тарифе",
		"Последний ответ: после перезапуска ответов ещё не было.",
		"Использование подписки (тариф plus):",
		"За 5 часов: использовано 42%, сброс вс 18 окт 21:30",
		"За неделю: использовано 7%, сброс чт 22 окт 10:00",
		"Лимит исчерпан: нет.",
	)

	h.push(textUpdate(2, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "hi", Model: "gpt-test", Tokens: agentruntime.TokenCounts{Input: 120, Output: 30, CacheRead: 80}}
	h.nextMessage()
	h.push(textUpdate(3, 101, "/status"))
	wantRussian(t, h.nextMessage().text, "Последний ответ: модель — gpt-test, токенов на входе — 120, на выходе — 30 (из кэша прочитано 80).")

	h.push(textUpdate(4, 101, "/usage"))
	wantRussian(t, h.nextMessage().text, "Использование подписки (тариф plus):", "Лимит исчерпан: нет.")
}

// /privacy, /new and /cancel answer in Russian.
func TestPrivacyNewAndCancelReadInRussian(t *testing.T) {
	t.Parallel()
	h, runtime := newRussianCommandsHarness(t)

	h.push(textUpdate(1, 101, "/privacy"))
	privacy := h.nextMessage().text
	wantRussian(t, privacy, "Ваши сообщения уходят в вашу собственную подписку на модель.", "Голос: расшифровывается")

	h.push(textUpdate(2, 101, "/cancel"))
	wantRussian(t, h.nextMessage().text, "Нет ответа, который можно остановить.")

	h.push(textUpdate(3, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "hi"}
	h.nextMessage()
	h.push(textUpdate(4, 101, "/new"))
	wantRussian(t, h.nextMessage().text, "Начат новый разговор.")
	h.push(textUpdate(5, 101, "/new"))
	wantRussian(t, h.nextMessage().text, "Пока нет разговора, который можно сбросить.")
}

// /model lists the catalog, its buttons and every refusal in Russian, while callback data and the
// values the user types stay as they are.
func TestModelReadsInRussian(t *testing.T) {
	t.Parallel()
	h, _ := newRussianCommandsHarness(t)
	h.service.model.Usage = cachedUsage{unavailable: map[string]bool{"gpt-7-terra": true}}

	h.push(textUpdate(1, 101, "/model"))
	list := h.nextMessage()
	wantRussian(t, list.text,
		"Модель и глубина рассуждений",
		"Здесь: gpt-test, глубина рассуждений medium (по умолчанию).",
		"Фоновые задачи: gpt-6.1-sol, глубина рассуждений medium. Это задаёт оператор, а не /model.",
		"gpt-test: Test, контекст 200k, самая низкая цена. Глубина рассуждений: minimal, low, medium, high (текущая модель)",
		"(модель сейчас недоступна на вашем тарифе)",
		"Измените это кнопками или введите /model &lt;модель&gt; [глубина], /model &lt;глубина&gt; или /model default.",
	)
	if labels := buttonLabels(list.keyboard); labels[len(labels)-1] != "К значениям по умолчанию" {
		t.Fatalf("/model buttons = %v", labels)
	}

	h.push(callbackUpdate(2, 101, list.messageID, "mdl:m:gpt-6.1-sol"))
	h.wantAnswer(2)
	levels := h.nextEdit()
	wantRussian(t, levels.text, "Выберите глубину рассуждений для модели gpt-6.1-sol.")
	if labels := buttonLabels(levels.keyboard); labels[len(labels)-2] != "Другие модели" || labels[len(labels)-1] != "К значениям по умолчанию" {
		t.Fatalf("level buttons = %v", labels)
	}
	if data := buttonData(levels.keyboard); data[len(data)-1] != "mdl:d" || data[len(data)-2] != "mdl:l" {
		t.Fatalf("level callback data = %v", data)
	}

	h.push(callbackUpdate(3, 101, list.messageID, "mdl:e:gpt-6.1-sol:xhigh"))
	h.wantAnswer(3)
	saved := h.nextEdit()
	wantRussian(t, saved.text, "Сохранено: gpt-6.1-sol, глубина рассуждений xhigh, в этом разговоре до /new.")
	if labels := buttonLabels(saved.keyboard); len(labels) != 1 || labels[0] != "Сменить модель" {
		t.Fatalf("saved buttons = %v", labels)
	}

	h.push(textUpdate(4, 101, "/model sol low"))
	wantRussian(t, h.nextMessage().text, "Теперь этот разговор идёт на модели gpt-6.1-sol с глубиной рассуждений low, до /new.")
	h.push(textUpdate(5, 101, "/model nope"))
	wantRussian(t, h.nextMessage().text, `Модель "nope" мне неизвестна. Доступные модели:`)
	h.push(textUpdate(6, 101, "/model sol sideways"))
	wantRussian(t, h.nextMessage().text, `"sideways" не является глубиной рассуждений. Модель gpt-6.1-sol поддерживает:`)
	h.push(textUpdate(7, 101, "/model a b c"))
	wantRussian(t, h.nextMessage().text, "Используйте /model &lt;модель&gt; [глубина], /model &lt;глубина&gt; или /model default. Модели:")
	h.push(textUpdate(8, 101, "/model default"))
	wantRussian(t, h.nextMessage().text, "Этот разговор снова идёт на значениях по умолчанию: gpt-test, глубина рассуждений medium.")
}

// A sticker, a recording, a file past the size limit and an over-long message are refused in
// Russian.
func TestRefusedMessagesReadInRussian(t *testing.T) {
	t.Parallel()
	h, _ := newRussianCommandsHarness(t)

	sticker := textUpdate(1, 101, "")
	sticker.Message.Sticker = &MediaFile{FileID: "sticker-1"}
	h.push(sticker)
	h.wantReply(101, 1, "Стикеры я прочитать не могу. Отправьте текст, фото или файл, и я с этим поработаю.")

	recording := textUpdate(2, 101, "")
	recording.Message.Voice = &MediaFile{FileID: "voice-1", FileSize: 30 << 20}
	h.push(recording)
	h.wantReply(101, 2, "Эта запись слишком большая, я не могу её прослушать; записи должны быть меньше 20 МБ.")

	document := textUpdate(3, 101, "")
	document.Message.Document = &MediaFile{FileID: "doc-1", FileName: "report.pdf", MimeType: "application/pdf", FileSize: 30 << 20}
	h.push(document)
	h.wantReply(101, 3, "Этот файл слишком большой, я не могу его сохранить; файлы должны быть меньше 20 МБ.")

	h.push(textUpdate(4, 101, strings.Repeat("a", 4097)))
	invalid := h.nextMessage().text
	wantRussian(t, invalid, "Не удалось принять это сообщение. В сообщении может быть не больше 4096 символов")
}

package telegram

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// A turn that times out reads in Russian for a Russian user, and in English for one who never chose.
// Serial: it has a 300 ms turn timeout.
func TestTimedOutTurnRepliesInTheUsersLanguage(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{languages: true, turnTimeout: 300 * time.Millisecond})
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}

	h.push(textUpdate(1, 101, "plan my day"))
	receive(t, h.ctx, runtime.calls)
	h.wantReply(101, 1, "Ответ занял слишком много времени. Попробуй ещё раз с более коротким запросом.")
	h.waitState(1, "delivered")

	h.push(textUpdate(2, 202, "plan my day"))
	receive(t, h.ctx, runtime.calls)
	h.wantReply(202, 2, localetest.English(conversation.TimeoutReply))
}

// A provider's usage limit reads in Russian for a Russian user.
func TestUsageLimitTurnRepliesInRussian(t *testing.T) {
	t.Parallel()
	profiles := filepath.Join(t.TempDir(), "profiles")
	h := newConversationHarnessWithOptions(t, newFixtureRuntime(t, profiles), harnessOptions{profilesDir: profiles, languages: true})
	h.provision("alice", "acct-alice")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}

	h.push(textUpdate(1, 101, "fixture:quota"))
	h.wantReply(101, 1, "Твоя подписка на модель достигла лимита использования. Попробуй позже.")
	h.waitState(1, "delivered")
}

// A Russian user's background task card, progress and report read in Russian, while the notice the
// model is told of the report stays English.
func TestBackgroundTaskCardReadsInRussian(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: testTasks(), languages: true})
	h.provision("alice", "account-alice")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}

	chat, status, id := h.startTask(runtime, 1, "Mail", "Go through my mail and list what needs action.")
	if status.text != "Запускаю: Mail" {
		t.Fatalf("status = %q", status.text)
	}
	run := receive(t, h.ctx, runtime.calls)
	if cyrillic.MatchString(run.request.Message) {
		t.Fatalf("the task's message to the model carries Russian: %q", run.request.Message)
	}
	h.wantStatusEdit(status, "Работаю над: Mail\nПрошло меньше минуты, 0 запросов.", true)

	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")
	run.answer <- agentruntime.RunResult{Response: "Two mails need action.", Final: "Two mails need action."}
	report := h.nextMessage()
	if report.replyTo != requestMessageID(1) || !strings.HasPrefix(report.text, "<b>Фоновая задача: Mail</b>\n\nTwo mails need action.") {
		t.Fatalf("report = %+v", report)
	}
	h.wantStatusEdit(status, "Готово: Mail\nЗаняло меньше минуты, 0 запросов. Отчёт ниже.", false)

	h.push(textUpdate(2, 101, "what did you find?"))
	followUp := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(followUp.request.Message, fmt.Sprintf(`Background task "Mail" (id %s) finished`, id)) || cyrillic.MatchString(followUp.request.Message) {
		t.Fatalf("follow-up message = %q, want the English notice", followUp.request.Message)
	}
	followUp.answer <- agentruntime.RunResult{Response: "Two mails."}
	h.wantReply(101, 2, "Two mails.")
	h.waitState(2, "delivered")
}

// A failed background task of a Russian user is reported in Russian, while the notice and the
// task_result the model reads keep the English text.
func TestFailedBackgroundTaskReachesTheModelInEnglish(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: testTasks(), languages: true})
	h.provision("alice", "account-alice")
	h.start()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}

	chat, status, id := h.startTask(runtime, 1, "Mail", "Go through my mail and list what needs action.")
	run := receive(t, h.ctx, runtime.calls)
	h.wantStatusEdit(status, "Работаю над: Mail", true)
	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")
	run.answer <- agentruntime.RunResult{}
	report := h.nextMessage()
	if !strings.Contains(report.text, "Не удалось завершить ответ.") {
		t.Fatalf("report = %q, want the Russian failure text", report.text)
	}
	if result := h.taskColumn(id, "result"); cyrillic.MatchString(result) {
		t.Fatalf("the stored result carries Russian: %q", result)
	}

	h.push(textUpdate(2, 101, "what happened?"))
	followUp := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(followUp.request.Message, "I could not complete that response.") || cyrillic.MatchString(followUp.request.Message) {
		t.Fatalf("follow-up message = %q, want the English notice", followUp.request.Message)
	}
	if result := h.callTool(followUp, "task_result", fmt.Sprintf(`{"task_id":%q}`, id)); result.IsError || cyrillic.MatchString(result.Text) {
		t.Fatalf("task_result = %+v, want English", result)
	}
	followUp.answer <- agentruntime.RunResult{Response: "It failed."}
	h.wantReply(101, 2, "It failed.")
	h.waitState(2, "delivered")
}

// A Russian user's restore preview and its confirmation read in Russian.
func TestRestorePreviewReadsInRussian(t *testing.T) {
	t.Parallel()
	source := newExportHarness(t)
	source.seedExport("alice")
	data := source.takeExport(1, 101)

	target := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{languages: true})
	target.provision("alice", "acct-alice")
	target.start()
	if err := target.store.SaveLanguage(target.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	copyKey(t, source, target)

	preview := target.restorePreview(1, 101, data)
	for _, want := range []string{
		"Предпросмотр восстановления из твоего экспорта от ",
		"Пока ты не подтвердишь, ничего не изменится.",
		"\nПамять: ",
		"\nСохранённые места: 1 к добавлению.",
		"Добавляется только то, что отмечено знаком +; уже имеющееся не меняется.",
	} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview lacks %q:\n%s", want, preview)
		}
	}
	for _, unwanted := range []string{"Restore preview", "to add", "Saved places"} {
		if strings.Contains(preview, unwanted) {
			t.Fatalf("preview holds %q:\n%s", unwanted, preview)
		}
	}
	card := target.nextMessage()
	if !strings.HasPrefix(card.text, "Восстановление из твоего экспорта\n") || !strings.Contains(card.text, "Восстановить ") || !strings.Contains(card.text, "Всё, что добавляется, перечислено выше.") {
		t.Fatalf("restore card = %q", card.text)
	}
}

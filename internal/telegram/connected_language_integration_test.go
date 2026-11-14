package telegram

import (
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// The task list and the privacy page of a Russian user read in Russian.
func TestTaskListAndPrivacyReadInRussian(t *testing.T) {
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
	h.wantStatusEdit(status, "Работаю над: Mail", true)
	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")
	run.answer <- agentruntime.RunResult{}
	h.nextMessage()
	h.wantStatusEdit(status, "Не удалось: Mail", false)

	h.push(textUpdate(2, 101, "/tasks"))
	if list := h.nextMessage().text; !strings.Contains(list, "ошибка меньше минуты назад") {
		t.Fatalf("/tasks = %q", list)
	}
	h.push(textUpdate(3, 101, "/privacy"))
	if privacy := h.nextMessage().text; !strings.Contains(privacy, "Отчёты фоновых задач хранятся 30 дней.") {
		t.Fatalf("/privacy = %q", privacy)
	}
}

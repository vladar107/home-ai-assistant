package telegram

import (
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// /jobs describes a Russian user's job and watch, their schedule, window and state, in Russian, and
// a watch tab's summary follows the same language.
func TestJobsDescribeJobsAndWatchesInRussian(t *testing.T) {
	t.Parallel()
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{languages: true, schedules: newTestClock(t, mondayMorning), watches: &stubEvaluator{}})
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.provision("alice", "acct-alice")
	h.start()
	h.createJob("alice", "a1b2c3d4e5f60711", "Сводка", "30 7 * * *", "Summarise my day.")
	h.createWatch("alice", "00000000000000a1", trainWatch())

	h.push(textUpdate(1, 101, "/jobs"))
	list := h.nextMessage()
	if !strings.Contains(list.text, "следующий запуск: вт 29 сен 07:30") || !strings.Contains(list.text, "2. 👁 Поезд Utrecht Centraal → Amsterdam Centraal, отправление 08:30\nпн–пт, 07:00–08:30") {
		t.Fatalf("/jobs = %q", list.text)
	}
	if found := localetest.Latin(list.text, "Utrecht", "Centraal", "Amsterdam", "CET", "CEST", "Europe"); len(found) != 0 {
		t.Fatalf("/jobs has English words %v: %q", found, list.text)
	}

	summary := watch.SummaryIn(locale.Russian, watch.Watch{Spec: trainWatch(), TravelPause: true})
	if !strings.Contains(summary, "Дни: пн–пт") || !strings.Contains(summary, "Сообщает о: отмена, частичная отмена, задержка от 5 мин, смена платформы") || !strings.Contains(summary, "Приостанавливается, пока ты в поездке.") {
		t.Fatalf("watch summary = %q", summary)
	}
	if found := localetest.Latin(summary, "Utrecht", "Centraal", "Amsterdam", "jobs", "Europe"); len(found) != 0 {
		t.Fatalf("watch summary has English words %v: %q", found, summary)
	}
}

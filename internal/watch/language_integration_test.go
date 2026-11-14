package watch_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

type languageWatch struct {
	*engine
	proposer *fakeProposer
	service  *watch.Service
	russian  context.Context
}

func newLanguageWatch(t *testing.T) *languageWatch {
	t.Helper()
	e := newEngine(t)
	proposer := &fakeProposer{}
	service, err := watch.NewService(watch.Config{Zone: "Europe/Amsterdam", Now: e.clock.Now}, e.store, e.store, e.sender, proposer, slog.New(slog.NewTextHandler(e.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Register(watch.KindDeparture, e.evaluator); err != nil {
		t.Fatal(err)
	}
	return &languageWatch{engine: e, proposer: proposer, service: service, russian: locale.With(e.ctx, locale.Russian)}
}

func (l *languageWatch) call(ctx context.Context, tool, arguments string) tools.Result {
	l.t.Helper()
	result, err := l.service.Execute(ctx, "alice", tools.Call{Name: tool, Arguments: json.RawMessage(arguments)})
	if err != nil || result.IsError {
		l.t.Fatalf("%s = %+v %v", tool, result, err)
	}
	return result
}

func (l *languageWatch) lastProposal() approval.Proposal {
	l.t.Helper()
	l.proposer.mu.Lock()
	defer l.proposer.mu.Unlock()
	return l.proposer.proposals[len(l.proposer.proposals)-1].proposal
}

func (l *languageWatch) confirm(ctx context.Context, id string, proposal approval.Proposal) approval.Outcome {
	l.t.Helper()
	outcome, err := l.service.ExecuteApproved(ctx, approval.Record{ID: id, PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
	if err != nil {
		l.t.Fatal(err)
	}
	return outcome
}

func languageWatchWantRussian(t *testing.T, text string, phrases ...string) {
	t.Helper()
	for _, phrase := range phrases {
		if !strings.Contains(text, phrase) {
			t.Fatalf("%q does not contain %q", text, phrase)
		}
	}
	if latin := localetest.Latin(text, "Zwolle", "Utrecht", "Europe", "Amsterdam", "jobs", "CEST", "Den", "Haag", "Centraal", "Amersfoort"); len(latin) > 0 {
		t.Fatalf("%q holds Latin words %v", text, latin)
	}
}

const languageWatchNew = `{"kind":"departure","mode":"train","from":"Utrecht","to":"Zwolle","departure":"07:42","days":["mon","tue","wed","thu","fri"],"pause_while_traveling":true}`

func TestWatchProposeConfirmAndUndoReadInRussian(t *testing.T) {
	l := newLanguageWatch(t)

	result := l.call(l.russian, "watch_propose", languageWatchNew)
	if result.Text != approval.ProposedText {
		t.Fatalf("the model's result = %q, want the English %q", result.Text, approval.ProposedText)
	}
	proposal := l.lastProposal()
	languageWatchWantRussian(t, proposal.Summary, "Наблюдение: Поезд Utrecht", "Дни: пн–пт", "Сообщает о: отмена, частичная отмена, задержка от", "В поездках оно молчит.")
	if proposal.Intent.Verb != "watch" || proposal.Intent.TargetKind != "departure watch" || strings.Contains(proposal.Intent.TargetName, "Поезд") {
		t.Fatalf("the intent is not English: %+v", proposal.Intent)
	}

	created := l.confirm(l.russian, "00000000000000d1", proposal)
	languageWatchWantRussian(t, created.UserText, "Наблюдение включено.", "Первая проверка:")
	if !strings.HasPrefix(created.Text, "Watching. First check: Mon ") {
		t.Fatalf("the model's outcome = %q, want English", created.Text)
	}

	l.call(l.russian, "watch_update", `{"watch_id":"00000000000000d1","departure":"07:50"}`)
	change := l.lastProposal()
	languageWatchWantRussian(t, change.Summary, "Изменить наблюдение Поезд Utrecht → Zwolle, отправление 07:42 на:\nПоезд Utrecht → Zwolle, отправление 07:50")
	if strings.Contains(change.Summary, "Change the watch") || change.Intent.Verb != "change" {
		t.Fatalf("change proposal = %+v", change)
	}
	changed := l.confirm(l.russian, "00000000000000d2", change)
	languageWatchWantRussian(t, changed.UserText, "Изменено.")
	if !strings.HasPrefix(changed.Text, "Changed. ") {
		t.Fatalf("the model's outcome = %q", changed.Text)
	}

	restored, err := l.service.Undo(l.russian, approval.Record{PrincipalID: "alice", Tool: "watch_update"}, changed.Undo)
	if err != nil {
		t.Fatal(err)
	}
	languageWatchWantRussian(t, restored.UserText, "Наблюдение Поезд Utrecht → Zwolle, отправление 07:42 восстановлено как было.")
	if !strings.HasPrefix(restored.Text, "Restored the watch 07:42 train Utrecht") {
		t.Fatalf("the Undo notice for the model = %q", restored.Text)
	}

	l.call(l.russian, "watch_remove", `{"watch_id":"00000000000000d1"}`)
	removal := l.lastProposal()
	languageWatchWantRussian(t, removal.Summary, "Остановить наблюдение Поезд", "пн–пт")
	deleted := l.confirm(l.russian, "00000000000000d3", removal)
	languageWatchWantRussian(t, deleted.UserText, "Удалено.")
	if deleted.Text != "Deleted." {
		t.Fatalf("the model's outcome = %q", deleted.Text)
	}
	back, err := l.service.Undo(l.russian, approval.Record{PrincipalID: "alice", Tool: "watch_remove"}, deleted.Undo)
	if err != nil {
		t.Fatal(err)
	}
	languageWatchWantRussian(t, back.UserText, "снова включено;", "прежние уведомления не сохраняются.")
	if !strings.HasPrefix(back.Text, "Watching 07:42 train Utrecht") {
		t.Fatalf("the Undo notice for the model = %q", back.Text)
	}
}

func TestWatchUndoRefusalAndActionErrorsReadInRussian(t *testing.T) {
	l := newLanguageWatch(t)
	l.call(l.russian, "watch_propose", languageWatchNew)
	created := l.confirm(l.russian, "00000000000000e1", l.lastProposal())

	if _, err := l.service.Undo(l.russian, approval.Record{PrincipalID: "alice", Tool: "watch_propose"}, created.Undo); err != nil {
		t.Fatal(err)
	}
	_, err := l.service.Undo(l.russian, approval.Record{PrincipalID: "alice", Tool: "watch_propose"}, created.Undo)
	var refusal *approval.UndoRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("second Undo = %v, want a refusal", err)
	}
	if refusal.Reason != "the watch no longer exists." {
		t.Fatalf("reason for the model = %q", refusal.Reason)
	}
	languageWatchWantRussian(t, refusal.UserReason, "наблюдения больше нет.")

	_, err = l.service.ExecuteApproved(l.russian, approval.Record{ID: "00000000000000e2", PrincipalID: "alice", Tool: "watch_remove", Arguments: json.RawMessage(`{"watch_id":"00000000000000e1"}`)})
	var failed *approval.ActionError
	if !errors.As(err, &failed) {
		t.Fatalf("removing a deleted watch = %v, want an action error", err)
	}
	if failed.Text != "That watch no longer exists." {
		t.Fatalf("text for the model = %q", failed.Text)
	}
	languageWatchWantRussian(t, failed.UserText, "Этого наблюдения больше нет.")
}

func TestWatchDescriptionReadsInRussian(t *testing.T) {
	spec := departureSpec()
	spec.Target.Via = "Amersfoort"
	spec.Triggers = []string{watch.TriggerDelay, watch.TriggerPlatform}
	described := spec.DescribeIn(locale.Russian)
	languageWatchWantRussian(t, described+"\n"+spec.LabelIn(locale.Russian), "Дни: пн–пт", "Время: 06:12–07:42 (Europe/Amsterdam), проверяется раз в", "Сообщает о: задержка от 5 мин, смена платформы", "через Amersfoort")
	if strings.Contains(described, "Days:") || spec.Describe() == described {
		t.Fatalf("the English description changed with the language: %q", spec.Describe())
	}
	if english := spec.Describe(); !strings.Contains(english, "Days: Mon–Fri") || !strings.Contains(english, "Tells you about: a delay of 5 minutes or more, a platform change") {
		t.Fatalf("the English description = %q", english)
	}
}

func TestWatchProposalStaysEnglishForAnEnglishUser(t *testing.T) {
	l := newLanguageWatch(t)
	l.call(l.ctx, "watch_propose", languageWatchNew)
	summary := l.lastProposal().Summary
	for _, want := range []string{"Watch 07:42 train Utrecht → Zwolle\n", "Days: Mon–Fri", "You get a message only when something changes, and once more when it is back to normal.", "It stays silent while you are traveling."} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q does not contain %q", summary, want)
		}
	}
}

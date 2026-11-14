package intake_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/intake"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// intakeLangAllowed are the Latin-letter words the Russian texts may hold: the values the test
// supplies and the job's own names.
var intakeLangAllowed = []string{"Electricity", "for", "September", "Invoice", "Example", "Energy", "billing", "example", "com", "payment", "due", "total", "EUR", "invoice", "number", "INV", "Pay", "by", "October", "Paid", "already", "Water", "bill", "parcel", "notice", "Parcel", "on", "its", "way", "Be", "home", "R", "m1", "m2", "Example", "Sender", "id", "Act", "reference", "15", "Inbox", "the"}

// intakeLangProposer records what the service proposes and then makes it at once, as Confirm would.
type intakeLangProposer struct {
	service   *intake.Service
	proposals []approval.Proposal
	outcome   approval.Outcome
	failure   error
}

func (p *intakeLangProposer) Act(ctx context.Context, principalID string, proposal approval.Proposal) (string, error) {
	p.proposals = append(p.proposals, proposal)
	p.outcome, p.failure = p.service.ExecuteApproved(ctx, approval.Record{ID: "0000000000000001", PrincipalID: principalID, Tool: proposal.Tool, Arguments: proposal.Arguments})
	if p.failure != nil {
		return "", &approval.FailedError{Text: p.failure.Error()}
	}
	return "Done: " + p.outcome.Text, nil
}

func newIntakeLangService(t *testing.T) (*intake.Service, *intakeLangProposer) {
	t.Helper()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	proposer := &intakeLangProposer{}
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	service, err := intake.NewService(intake.Config{Location: time.UTC, Now: func() time.Time {
		now = now.Add(time.Minute)
		return now
	}}, store, proposer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	proposer.service = service
	return service, proposer
}

func intakeLangRequire(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, phrase := range want {
		if !strings.Contains(text, phrase) {
			t.Fatalf("text lacks %q:\n%s", phrase, text)
		}
	}
	if latin := localetest.Latin(text, intakeLangAllowed...); len(latin) != 0 {
		t.Fatalf("text holds Latin words %q:\n%s", latin, text)
	}
}

// A Russian user reads the proposals to change and to delete a record, and their outcomes, in
// Russian, while the model's result and the outcome texts it keeps stay English.
func TestIntakeRecordProposalsAndOutcomesReadInRussian(t *testing.T) {
	t.Parallel()
	service, proposer := newIntakeLangService(t)
	ctx := locale.With(t.Context(), locale.Russian)
	const draft = `{"source":"file","source_ref":"a1b2c3d4","kind":"Invoice","title":"Electricity for September","sender":"Example Energy (billing@example.com)",` +
		`"dates":[{"label":"payment due","date":"2026-10-15"}],"amounts":[{"label":"total","value":"84.20","currency":"eur"}],` +
		`"references":[{"label":"invoice number","value":"INV-2026-0917"}],"action_required":"Pay 84.20 EUR by 15 October."}`
	call := func(name, arguments string) tools.Result {
		t.Helper()
		result, err := service.Execute(ctx, "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	created := call("intake_record", draft)
	id := strings.TrimSuffix(strings.TrimPrefix(created.Text, "Kept as intake record "), ".")
	if created.IsError || strings.ContainsAny(created.Text, "абвгдеёжзийклмнопрстуфхцчшщъыьэюя") {
		t.Fatalf("the model's result = %+v", created)
	}

	changed := call("intake_record", strings.Replace(draft, "Pay 84.20 EUR by 15 October.", "Paid already.", 1))
	if changed.IsError || !strings.HasPrefix(changed.Text, "Done: Updated the intake record "+id+".") {
		t.Fatalf("the model's result after the change = %+v", changed)
	}
	summary := proposer.proposals[0].Summary
	intakeLangRequire(t, summary, "Изменить запись входящих Electricity for September (Invoice) так:",
		"Electricity for September (Invoice), от Example Energy", "Что сделать: Paid already.")
	if proposer.outcome.Text != "Updated the intake record "+id+"." || proposer.outcome.UserText != "Запись входящих "+id+" обновлена." {
		t.Fatalf("update outcome = %+v", proposer.outcome)
	}

	call("intake_delete", `{"id":"`+id+`"}`)
	intakeLangRequire(t, proposer.proposals[1].Summary, "Удалить запись входящих Electricity for September (Invoice).")
	if proposer.outcome.Text != "Deleted the intake record." || proposer.outcome.UserText != "Запись входящих удалена." {
		t.Fatalf("delete outcome = %+v", proposer.outcome)
	}

	_, err := service.ExecuteApproved(ctx, approval.Record{PrincipalID: "alice", Tool: "intake_delete", Arguments: json.RawMessage(`{"id":"` + id + `"}`)})
	var failed *approval.ActionError
	if !errorsAsAction(err, &failed) || failed.Text != "That record no longer exists." || failed.UserText != "Такой записи больше нет." {
		t.Fatalf("delete of a gone record = %#v", err)
	}
}

func errorsAsAction(err error, target **approval.ActionError) bool {
	failed, ok := err.(*approval.ActionError)
	if ok {
		*target = failed
	}
	return ok
}

type intakeLangChecker struct{ reply string }

func (c intakeLangChecker) RunCheck(context.Context, string, string) (string, string, error) {
	return c.reply, "", nil
}

type intakeLangMail struct{}

func (intakeLangMail) MailReadable(context.Context, string) (bool, error) { return true, nil }

type intakeLangApprovals struct{}

func (intakeLangApprovals) LoadApproval(context.Context, string) (approval.Record, bool, error) {
	return approval.Record{}, false, nil
}

type intakeLangChats struct{}

func (intakeLangChats) ResolveActiveChat(context.Context, string) (int64, bool, error) {
	return 101, true, nil
}

type intakeLangReminders struct{}

func (intakeLangReminders) ReminderProposal(_ context.Context, _, text, _ string) (approval.Proposal, error) {
	return approval.Proposal{Tool: "reminder_create", Arguments: json.RawMessage(`{}`), Summary: "Напомнить: " + text, Intent: approval.Intent{Verb: "set", TargetKind: "reminder", TargetName: text}}, nil
}

type intakeLangOffers struct {
	heading   string
	proposals []approval.Proposal
}

func (o *intakeLangOffers) OfferIfIdle(_ context.Context, _ string, _, _ int64, heading string, _ time.Duration, proposals []approval.Proposal) ([]approval.Record, error) {
	o.heading, o.proposals = heading, proposals
	records := make([]approval.Record, len(proposals))
	for index := range records {
		records[index].ID = strings.Repeat("a", index+1)
	}
	return records, nil
}

// The inbox check's card heading, the warning and "About" line on each follow-up, and the report
// with its notes read in Russian for a Russian user; the check's prompt stays English.
func TestInboxCheckCardAndReportReadInRussian(t *testing.T) {
	t.Parallel()
	service, _ := newIntakeLangService(t)
	reply := `{"messages":[{"account":"alice@example.com","message_id":"m1","kind":"invoice","title":"Water bill","sender":"Example Sender",` +
		`"dates":[{"label":"due","date":"2026-10-15"}],"amounts":[],"references":[{"label":"reference","value":"R-m1"}],"action_required":"Act by 15 October.",` +
		`"actions":[{"type":"reminder","text":"Be home for the parcel","at":"2026-10-14 09:00"}]}],"left":3,"through":""}`
	offers := &intakeLangOffers{}
	jobType, err := service.ScanJobType(intake.ScanConfig{
		Checker: intakeLangChecker{reply: reply}, Mail: intakeLangMail{}, Approvals: intakeLangApprovals{}, Offers: offers,
		Chats: intakeLangChats{}, Reminders: intakeLangReminders{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := locale.With(t.Context(), locale.Russian)
	result, err := jobType.Handler.Run(ctx, schedule.Occurrence{PrincipalID: "alice", JobID: "job", JobName: "Inbox", ScheduledFor: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if offers.heading != "Из твоей почты" || len(offers.proposals) != 1 {
		t.Fatalf("offered %q %+v", offers.heading, offers.proposals)
	}
	intakeLangRequire(t, offers.proposals[0].Summary, "⚠️ Это взято из почты, то есть из внешнего контента.", "О письме: Water bill, от Example Sender", "Напомнить: Be home for the parcel")
	intakeLangRequire(t, result.Text, "Новые письма, которым что-то нужно от тебя:", "Water bill (invoice), от Example Sender", "- reference: R-m1", "Что сделать: Act by 15 October.",
		"Предлагаемые действия — на карточке выше; ничего не произойдёт, пока ты не подтвердишь.", "Ещё 3 письма ждут следующей проверки.")
	if strings.Contains(result.Text, "Предлагаемые действия — на карточке выше; ничего не произойдёт, пока ты не подтвердишь. Ещё") == false {
		t.Fatalf("the notes are not one paragraph:\n%s", result.Text)
	}
	if english := intake.InboxHeading; english != "From your inbox" {
		t.Fatalf("InboxHeading = %q", english)
	}
}

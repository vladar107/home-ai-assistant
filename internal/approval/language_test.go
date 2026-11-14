package approval_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

const languageTool = "language_write"

// languageExecutor writes its outcome in English for the model and, for a user who chose Russian,
// in Russian for the user. Its Undo is refused when the arguments say the note changed.
type languageExecutor struct{}

func (languageExecutor) ActionTools() []string { return []string{languageTool} }

func (languageExecutor) ExecuteApproved(ctx context.Context, _ approval.Record) (approval.Outcome, error) {
	outcome := approval.Outcome{Text: "Saved the note.", Undo: json.RawMessage(`{"note":1}`)}
	if locale.From(ctx) == locale.Russian {
		outcome.UserText = "Заметка сохранена."
	}
	return outcome, nil
}

func (languageExecutor) Undo(ctx context.Context, record approval.Record, _ json.RawMessage) (approval.Outcome, error) {
	if strings.Contains(string(record.Arguments), "changed") {
		refusal := &approval.UndoRefusal{Reason: "The note changed since."}
		if locale.From(ctx) == locale.Russian {
			refusal.UserReason = "Заметка с тех пор изменилась."
		}
		return approval.Outcome{}, refusal
	}
	outcome := approval.Outcome{Text: "Removed the note."}
	if locale.From(ctx) == locale.Russian {
		outcome.UserText = "Заметка удалена."
	}
	return outcome, nil
}

// recordingDeliverer keeps what the service sent and edited, in order.
type recordingDeliverer struct {
	mu       sync.Mutex
	proposal []approval.Shown
	buttons  []approval.Buttons
	texts    chan approval.Shown
	direct   []approval.DirectActions
	edited   []approval.DirectActions
}

func newRecordingDeliverer() *recordingDeliverer {
	return &recordingDeliverer{texts: make(chan approval.Shown, 8)}
}

func (d *recordingDeliverer) SendText(context.Context, int64, string) (int64, error) { return 1, nil }

func (d *recordingDeliverer) SendProposal(_ context.Context, _ int64, shown approval.Shown, buttons approval.Buttons) (int64, approval.WidgetKind, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.proposal, d.buttons = append(d.proposal, shown), append(d.buttons, buttons)
	return int64(len(d.proposal)), approval.NotWidget, nil
}

func (d *recordingDeliverer) EditText(_ context.Context, _, _ int64, shown approval.Shown) error {
	d.texts <- shown
	return nil
}

func (d *recordingDeliverer) EditProposal(context.Context, int64, int64, approval.Shown, approval.Buttons) error {
	return nil
}

func (d *recordingDeliverer) SendPreview(context.Context, int64, int64, string) error { return nil }

func (d *recordingDeliverer) SendDirectActions(_ context.Context, _ int64, actions approval.DirectActions) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.direct = append(d.direct, actions)
	return int64(len(d.direct)), nil
}

func (d *recordingDeliverer) EditDirectActions(_ context.Context, _, _ int64, actions approval.DirectActions) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.edited = append(d.edited, actions)
	return nil
}

func (d *recordingDeliverer) lastProposal() (approval.Shown, approval.Buttons) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.proposal[len(d.proposal)-1], d.buttons[len(d.buttons)-1]
}

// directExposure lets every call act directly, in the chat request the test names.
type directExposure struct{ request *string }

func (e directExposure) UserChatRequest(context.Context) (string, bool) { return *e.request, true }
func (directExposure) External(context.Context, string) bool            { return false }
func (directExposure) Exposed(context.Context, string) bool             { return false }

// flagEverything warns about every proposal.
type flagEverything struct{}

func (flagEverything) Mismatch(context.Context, string, string, approval.Intent) (bool, error) {
	return true, nil
}

type languageFixture struct {
	service   *approval.Service
	store     *sqlite.Store
	deliverer *recordingDeliverer
	request   *string
}

// newLanguageFixture opens a real database with alice, who chose Russian, and bob, who did not.
func newLanguageFixture(t *testing.T, cfg approval.Config) *languageFixture {
	t.Helper()
	ctx := t.Context()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 202},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveLanguage(ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	deliverer := newRecordingDeliverer()
	service, err := approval.NewService(cfg, store, deliverer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	service.SetLanguages(store)
	if err := service.RegisterExecutor(languageExecutor{}); err != nil {
		t.Fatal(err)
	}
	request := "request-1"
	service.SetExposure(directExposure{request: &request})
	return &languageFixture{service: service, store: store, deliverer: deliverer, request: &request}
}

func languageProposal(summary string) approval.Proposal {
	arguments, _ := json.Marshal(map[string]string{"summary": summary})
	return approval.Proposal{Tool: languageTool, Arguments: arguments, Summary: summary}
}

// A proposal for a user who chose Russian has Russian buttons, and a Russian status line once it is
// rejected or confirmed, while the model's notice for the same record stays the English one of
// today. Another user's proposal in the same service is unchanged.
func TestProposalAndItsOutcomeAreShownInTheUsersLanguage(t *testing.T) {
	t.Parallel()
	f := newLanguageFixture(t, approval.Config{})
	ctx := t.Context()

	for _, tc := range []struct {
		principal                                   string
		language                                    locale.Language
		waiting, rejected, confirmed, notice, thing string
	}{
		{"alice", locale.Russian, "Истекает через 10 минут.", "Отклонено, ничего не записано.", "Готово. Заметка сохранена.", "", ""},
		{"bob", locale.English, "Expires in 10 minutes.", "Rejected; nothing was written.", "Done. Saved the note.", "", ""},
	} {
		record, err := f.service.Propose(ctx, tc.principal, languageProposal("Save the note"))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.AfterDelivery(ctx, tc.principal, 101); err != nil {
			t.Fatal(err)
		}
		shown, buttons := f.deliverer.lastProposal()
		if shown.Language != tc.language || buttons.Language != tc.language || shown.Status != tc.waiting {
			t.Fatalf("%s: proposal = %+v, buttons = %+v", tc.principal, shown, buttons)
		}
		pressed, err := f.service.Press(ctx, tc.principal, approval.ActionReject, record.ID)
		if err != nil || pressed.Shown.Status != tc.rejected || pressed.Shown.Language != tc.language {
			t.Fatalf("%s: rejected = %+v %v", tc.principal, pressed.Shown, err)
		}
		if tc.language == locale.Russian && !strings.HasSuffix(pressed.Shown.Caption, "— отклонено") {
			t.Fatalf("caption = %q, want the Russian tally", pressed.Shown.Caption)
		}
		if got, want := f.service.PendingNotice(tc.principal), "The user rejected your language_write proposal; nothing was written, and it is no longer waiting."; got != want {
			t.Fatalf("%s: notice = %q, want %q", tc.principal, got, want)
		}

		record, err = f.service.Propose(ctx, tc.principal, languageProposal("Save the note again"))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.AfterDelivery(ctx, tc.principal, 101); err != nil {
			t.Fatal(err)
		}
		pressed, err = f.service.Press(ctx, tc.principal, approval.ActionConfirm, record.ID)
		if err != nil || pressed.Shown.Status != tc.confirmed {
			t.Fatalf("%s: confirmed = %+v %v", tc.principal, pressed.Shown, err)
		}
		stored, _, err := f.store.LoadApproval(ctx, record.ID)
		if err != nil || stored.Outcome != "Saved the note." {
			t.Fatalf("%s: stored outcome = %q %v, want the English line for the model", tc.principal, stored.Outcome, err)
		}
		if got, want := f.service.PendingNotice(tc.principal), "The user confirmed your language_write proposal and it was executed. Saved the note."; got != want {
			t.Fatalf("%s: notice = %q, want %q", tc.principal, got, want)
		}
	}
}

// The sweep that expires a proposal edits its message in the language of the record's owner, read
// from the store: the sweep's own context names none.
func TestSweepEditsAnExpiredProposalInItsOwnersLanguage(t *testing.T) {
	t.Parallel()
	f := newLanguageFixture(t, approval.Config{Lifetime: 400 * time.Millisecond, SweepInterval: 20 * time.Millisecond})
	ctx := t.Context()
	if err := f.service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, principal := range []string{"alice", "bob"} {
		if _, err := f.service.Propose(ctx, principal, languageProposal("Save the note for "+principal)); err != nil {
			t.Fatal(err)
		}
		if err := f.service.AfterDelivery(ctx, principal, 101); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = f.service.Shutdown(context.Background()) })
	for range 2 {
		select {
		case shown := <-f.deliverer.texts:
			want := "Expired; nothing was written."
			if shown.Language == locale.Russian {
				want = "Истекло, ничего не записано."
			}
			if shown.Status != want || !strings.Contains(shown.Text, want) || (shown.Language == locale.Russian) != strings.Contains(shown.Body, "alice") {
				t.Fatalf("expiry edit = %+v, want %q", shown, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the sweep did not edit an expired proposal")
		}
	}
}

// A direct action's result for the model and the notice of its Undo are the English text of today
// whatever language the user has, while the user's message, buttons and answers are in theirs.
func TestDirectActionAndUndoKeepTheModelsTextEnglish(t *testing.T) {
	t.Parallel()
	f := newLanguageFixture(t, approval.Config{})
	ctx := t.Context()

	type result struct{ act, undone, refused, refusedNotice, undoneNotice string }
	run := func(principal string) (result, []approval.DirectActions, []string) {
		var out result
		var err error
		*f.request = "request-" + principal
		out.act, err = f.service.Act(ctx, principal, languageProposal("Save the note "+principal))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.AfterDelivery(ctx, principal, 101); err != nil {
			t.Fatal(err)
		}
		f.deliverer.mu.Lock()
		sent := f.deliverer.direct[len(f.deliverer.direct)-1]
		f.deliverer.mu.Unlock()
		records, err := f.store.DirectApprovals(ctx, principal, *f.request)
		if err != nil || len(records) != 1 {
			t.Fatalf("direct records = %+v %v", records, err)
		}
		var answers []string
		undid, err := f.service.Undo(ctx, principal, records[0].ID)
		if err != nil || !undid.Owned {
			t.Fatalf("undo = %+v %v", undid, err)
		}
		answers = append(answers, undid.Answer)
		out.undone, _ = f.service.TakeLaneNotice(principal, 0)

		*f.request = "request-changed-" + principal
		if _, err := f.service.Act(ctx, principal, languageProposal("The note changed "+principal)); err != nil {
			t.Fatal(err)
		}
		changed, err := f.store.DirectApprovals(ctx, principal, *f.request)
		if err != nil || len(changed) != 1 {
			t.Fatalf("changed records = %+v %v", changed, err)
		}
		undid, err = f.service.Undo(ctx, principal, changed[0].ID)
		if err != nil || !undid.Owned {
			t.Fatalf("refused undo = %+v %v", undid, err)
		}
		answers = append(answers, undid.Answer)
		out.refused, _ = f.service.TakeLaneNotice(principal, 0)
		return out, []approval.DirectActions{sent}, answers
	}

	alice, aliceSent, aliceAnswers := run("alice")
	bob, bobSent, bobAnswers := run("bob")

	if alice.act != bob.act || !strings.Contains(alice.act, "Saved the note.") {
		t.Fatalf("act result = %q, want the English text of today, equal to bob's %q", alice.act, bob.act)
	}
	if want := "The user pressed Undo on your language_write action; it was undone and the change no longer stands. Removed the note."; alice.undone != want || bob.undone != want {
		t.Fatalf("undo notices = %q and %q, want %q", alice.undone, bob.undone, want)
	}
	if want := "The user pressed Undo on your language_write action, but it was not undone: The note changed since."; alice.refused != want || bob.refused != want {
		t.Fatalf("refusal notices = %q and %q, want %q", alice.refused, bob.refused, want)
	}

	if text := aliceSent[0].Text; !strings.HasPrefix(text, "Готово:") || !strings.HasSuffix(text, "Отменить можно в течение 24 часов.") || aliceSent[0].Undo[0].Label != "Отменить" {
		t.Fatalf("alice's Done message = %+v", aliceSent[0])
	}
	if text := bobSent[0].Text; !strings.HasPrefix(text, "Done:") || !strings.HasSuffix(text, "Undo works for 24 hours.") || bobSent[0].Undo[0].Label != "Undo" {
		t.Fatalf("bob's Done message = %+v", bobSent[0])
	}
	if aliceAnswers[0] != "Отменено." || aliceAnswers[1] != "Отменить не удалось: Заметка с тех пор изменилась." {
		t.Fatalf("alice's answers = %q", aliceAnswers)
	}
	if bobAnswers[0] != "Undone." || bobAnswers[1] != "Not undone: The note changed since." {
		t.Fatalf("bob's answers = %q", bobAnswers)
	}
	f.deliverer.mu.Lock()
	defer f.deliverer.mu.Unlock()
	if len(f.deliverer.edited) == 0 || !strings.Contains(f.deliverer.edited[0].Text, "Отменено.") {
		t.Fatalf("edits of the Done message = %+v, want the Russian status", f.deliverer.edited)
	}
}

// The warning is written in the language of the moment, and the record's flag, not that wording,
// says it is there: after the user switches language the proposal still shows its title and its
// warning, in both directions.
func TestWarnedProposalKeepsItsTitleAndWarningAfterALanguageSwitch(t *testing.T) {
	t.Parallel()
	f := newLanguageFixture(t, approval.Config{})
	ctx := t.Context()
	f.service.SetIntentChecker(flagEverything{})
	intent := approval.Intent{Verb: "rename", TargetKind: "note", TargetName: "Groceries"}
	english, russian := approval.IntentWarning, "⚠️ Похоже, вы об этом не просили."

	for _, tc := range []struct {
		principal, from, to string
		warning             string
		then                locale.Language
		caption             string
	}{
		{"alice", "ru", "en", russian, locale.English, "⚠️ 2 changes — rejected"},
		{"bob", "en", "ru", english, locale.Russian, "⚠️ 2 изменения — отклонено"},
	} {
		language, _ := locale.Parse(tc.from)
		if err := f.store.SaveLanguage(ctx, tc.principal, language); err != nil {
			t.Fatal(err)
		}
		first := languageProposal("Rename the note\nto Groceries")
		first.Intent = intent
		second := languageProposal("Delete the note Chores")
		second.Intent = intent
		records, err := f.service.ProposeAll(ctx, tc.principal, []approval.Proposal{first, second})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.AfterDelivery(ctx, tc.principal, 101); err != nil {
			t.Fatal(err)
		}
		stored, _, err := f.store.LoadApproval(ctx, records[0].ID)
		if err != nil || !stored.Warned || !strings.HasPrefix(stored.Summary, tc.warning) {
			t.Fatalf("%s: stored = %+v %v, want a warned summary starting with %q", tc.principal, stored, err, tc.warning)
		}

		if err := f.store.SaveLanguage(ctx, tc.principal, tc.then); err != nil {
			t.Fatal(err)
		}
		pressed, err := f.service.Press(ctx, tc.principal, approval.ActionReject, records[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		shown := pressed.Shown
		if shown.Caption != tc.caption || shown.Changes[0].Title != "Rename the note" || !shown.Changes[0].Warned || !strings.Contains(shown.Body, tc.warning) {
			t.Fatalf("%s: after the switch = %+v", tc.principal, shown)
		}
		if title := approval.TitleOf(stored.Summary); title != "Rename the note" {
			t.Fatalf("%s: title = %q", tc.principal, title)
		}
	}
}

// The room a summary needs for the warning does not depend on the language the warning is in.
func TestSummaryRoomCoversTheLongestWarning(t *testing.T) {
	t.Parallel()
	if approval.SummaryFits(strings.Repeat("a", 2500)) {
		t.Fatal("a summary that leaves no room for the warning fits")
	}
	if !approval.SummaryFits(strings.Repeat("a", 2500-len([]rune(approval.IntentWarning))-2)) {
		t.Fatal("a summary that leaves room for the English warning does not fit")
	}
	if approval.ProposalChanged.In(locale.English) != approval.ProposalChangedText {
		t.Fatal("the English answer of a changed proposal differs from its constant")
	}
}

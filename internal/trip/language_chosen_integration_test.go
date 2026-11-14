package trip_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// chosenRewriter rewrites into a named language as well as after a sample, and keeps what it was
// asked.
type chosenRewriter struct {
	fakeRewriter
	languages []string
}

func (r *chosenRewriter) RewriteAlert(_ context.Context, _, language, text string) (string, error) {
	r.languages, r.texts = append(r.languages, language), append(r.texts, text)
	return r.answer, r.err
}

// A user who never wrote has a trip's messages rewritten into the language they chose when that is
// Russian, a user who wrote keeps the language of what they wrote, and a user who never chose
// keeps the English text.
func TestATripMessageFollowsTheChosenLanguageWhenThereIsNoSample(t *testing.T) {
	t.Parallel()
	original := "Trip Example trip to Example City has started (until Fri 2026-10-09 15:00). Nothing is paused."
	russian := `Поездка Example trip в Example City началась (до пт 2026-10-09 15:00). Ничего не приостановлено.`
	for _, test := range []struct {
		name    string
		chosen  *locale.Language
		writing fakeWriting
		want    string
		asked   []string
		samples []string
	}{
		{"never wrote, chose Russian", ptr(locale.Russian), fakeWriting{tab: 7}, russian, []string{"Russian"}, nil},
		{"never wrote, never chose", nil, fakeWriting{tab: 7}, original, nil, nil},
		{"wrote, chose Russian", ptr(locale.Russian), fakeWriting{tab: 7, messages: map[int64]string{7: "hola"}}, russian, nil, []string{"hola"}},
	} {
		f := newFixture(t)
		planned := f.add(draft(1, 3))
		rewriter := &chosenRewriter{fakeRewriter: fakeRewriter{answer: russian}}
		sender := &recordingSender{}
		language, err := trip.NewLanguage(test.writing, test.writing, rewriter, f.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		language.SetLanguages(f.store)
		if test.chosen != nil {
			if err := f.store.SaveLanguage(t.Context(), "alice", *test.chosen); err != nil {
				t.Fatal(err)
			}
		}
		if err := language.Sender(sender).Send(t.Context(), "alice", planned.ID, original); err != nil {
			t.Fatal(err)
		}
		want := original
		if test.asked != nil || test.samples != nil {
			want = test.want
		}
		if got := sender.texts(); len(got) != 1 || got[0] != want {
			t.Fatalf("%s: sent %q, want %q", test.name, got, want)
		}
		if len(rewriter.languages) != len(test.asked) || len(rewriter.samples) != len(test.samples) {
			t.Fatalf("%s: asked for %q after samples %q", test.name, rewriter.languages, rewriter.samples)
		}
	}
}

func ptr(language locale.Language) *locale.Language { return &language }

// A change proposed earlier that no longer applies is refused in words that read fully in Russian
// for the user and stay English for the model.
func TestAStaleTripChangeReadsInRussianForTheUserAndEnglishForTheModel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	planned := f.add(draft(1, 3))

	result, err := f.trips.Update(t.Context(), "alice", planned.ID, planned.UpdatedAt.Add(-time.Hour), draft(1, 4))
	if err != nil || result.Rejection != "The trip changed since this was proposed; propose the change again." {
		t.Fatalf("stale update = %+v %v", result, err)
	}
	if text := result.RejectionIn(locale.Russian); text != "Поездка изменилась с тех пор, как это предложили. Попроси изменить её заново." || len(localetest.Latin(text)) != 0 {
		t.Fatalf("russian rejection = %q", text)
	}
	if text := result.RejectionIn(locale.English); text != result.Rejection {
		t.Fatalf("english rejection = %q", text)
	}

	cancelled, err := f.trips.Cancel(t.Context(), "alice", planned.ID, time.Time{})
	if err != nil || cancelled.Rejection != "" {
		t.Fatalf("cancel = %+v %v", cancelled, err)
	}
	result, err = f.trips.Update(t.Context(), "alice", planned.ID, time.Time{}, draft(1, 4))
	if err != nil || result.Rejection != "That trip is cancelled and cannot be changed." {
		t.Fatalf("update of a cancelled trip = %+v %v", result, err)
	}
	if text := result.RejectionIn(locale.Russian); text != "Поездка отменена, изменить её нельзя." {
		t.Fatalf("russian rejection = %q", text)
	}
}

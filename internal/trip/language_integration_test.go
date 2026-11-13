package trip_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// fakeWriting is what the user wrote, by tab, and the tab the trip's messages go to.
type fakeWriting struct {
	tab      int64
	messages map[int64]string
	latest   string
}

func (w fakeWriting) TripTab(context.Context, string, string) (int64, error) { return w.tab, nil }

func (w fakeWriting) LatestMessage(_ context.Context, _ string, threadID int64, anywhere bool) (string, bool, error) {
	if anywhere {
		return w.latest, w.latest != "", nil
	}
	text, found := w.messages[threadID]
	return text, found, nil
}

// fakeRewriter answers every rewrite with the same text and keeps what it was asked.
type fakeRewriter struct {
	answer  string
	err     error
	samples []string
	texts   []string
}

func (r *fakeRewriter) RewriteAlertLike(_ context.Context, _, sample, text string) (string, error) {
	r.samples, r.texts = append(r.samples, sample), append(r.texts, text)
	return r.answer, r.err
}

// A rewrite stands in for the message only when it keeps every number, every title the message
// quotes, and every booking reference of the trip the message holds, and fits one message.
func TestATripMessageIsRewrittenOnlyWhenTheRewriteKeepsWhatMatters(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(1, 3, booking(trip.KindFlight, "Flight to Example City", "ABC123", days(1), time.Time{})))
	original := `Updated trip "Example trip": now 16 Oct 2026 to 18 Oct 2026. Booking ABC123 stays.`
	for _, test := range []struct {
		name    string
		answer  string
		err     error
		rewrite bool
	}{
		{"a faithful rewrite", `Viaje "Example trip" actualizado: ahora del 16 oct 2026 al 18 oct 2026. La reserva ABC123 sigue.`, nil, true},
		{"a changed number", `Viaje "Example trip" actualizado: ahora del 17 oct 2026 al 18 oct 2026. La reserva ABC123 sigue.`, nil, false},
		{"a dropped number", `Viaje "Example trip" actualizado: ahora del 16 oct al 18 oct 2026. La reserva ABC123 sigue.`, nil, false},
		{"a changed quoted title", `Viaje "Viaje de ejemplo" actualizado: ahora del 16 oct 2026 al 18 oct 2026. La reserva ABC123 sigue.`, nil, false},
		{"a changed booking reference", `Viaje "Example trip" actualizado: ahora del 16 oct 2026 al 18 oct 2026. La reserva ABD123 sigue.`, nil, false},
		{"more than one message", `Viaje "Example trip" 16 oct 2026 18 oct 2026 ABC123 ` + strings.Repeat("\U0001F600", 2100), nil, false},
		{"an empty rewrite", " ", nil, false},
		{"a failed rewrite", "", errors.New("the model is not connected"), false},
	} {
		rewriter := &fakeRewriter{answer: test.answer, err: test.err}
		sender := &recordingSender{}
		language, err := trip.NewLanguage(fakeWriting{tab: 7, messages: map[int64]string{7: "hola"}, latest: "hello"}, fakeWriting{tab: 7, messages: map[int64]string{7: "hola"}, latest: "hello"},
			rewriter, f.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := language.Sender(sender).Send(t.Context(), "alice", planned.ID, original); err != nil {
			t.Fatal(err)
		}
		want := original
		if test.rewrite {
			want = test.answer
		}
		if got := sender.texts(); len(got) != 1 || got[0] != want {
			t.Fatalf("%s: sent %q, want %q", test.name, got, want)
		}
		if len(rewriter.samples) != 1 || rewriter.samples[0] != "hola" || rewriter.texts[0] != original {
			t.Fatalf("%s: rewrite asked with %q %q", test.name, rewriter.samples, rewriter.texts)
		}
	}
}

// The sample is the latest message in the tab the trip's messages go to, then the latest anywhere,
// and without either, or without the history to read them from, no rewrite is asked for.
func TestATripMessagesLanguageSampleComesFromItsTabFirst(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(1, 3))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, test := range []struct {
		name    string
		writing fakeWriting
		sample  string
	}{
		{"the tab's", fakeWriting{tab: 7, messages: map[int64]string{7: "hola", 0: "hello"}, latest: "hello"}, "hola"},
		{"anywhere", fakeWriting{tab: 7, messages: map[int64]string{0: "hello"}, latest: "hello"}, "hello"},
		{"a blank message in the tab", fakeWriting{tab: 7, messages: map[int64]string{7: "  "}, latest: "hello"}, "hello"},
		{"never wrote", fakeWriting{tab: 7}, ""},
	} {
		rewriter := &fakeRewriter{answer: "rewritten"}
		sender := &recordingSender{}
		language, err := trip.NewLanguage(test.writing, test.writing, rewriter, f.store, logger)
		if err != nil {
			t.Fatal(err)
		}
		if got := language.Sample(t.Context(), "alice", planned.ID); got != test.sample {
			t.Fatalf("%s: sample = %q, want %q", test.name, got, test.sample)
		}
		if err := language.Sender(sender).Send(t.Context(), "alice", planned.ID, "Trip Example trip has ended."); err != nil {
			t.Fatal(err)
		}
		want, rewrites := "rewritten", 1
		if test.sample == "" {
			want, rewrites = "Trip Example trip has ended.", 0
		}
		if got := sender.texts(); len(got) != 1 || got[0] != want || len(rewriter.samples) != rewrites {
			t.Fatalf("%s: sent %q after %d rewrites", test.name, got, len(rewriter.samples))
		}
	}

	rewriter := &fakeRewriter{answer: "rewritten"}
	sender := &recordingSender{}
	language, err := trip.NewLanguage(fakeWriting{tab: 7}, nil, rewriter, f.store, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := language.Sender(sender).Send(t.Context(), "alice", planned.ID, "Trip Example trip has ended."); err != nil {
		t.Fatal(err)
	}
	if got := sender.texts(); len(got) != 1 || got[0] != "Trip Example trip has ended." || len(rewriter.samples) != 0 {
		t.Fatalf("without a history: sent %q after %d rewrites", got, len(rewriter.samples))
	}
}

// A rewrite is held to what it adds as well as to what it keeps: the original's lines, each with
// its numbers in order, nothing Telegram would make tappable that the original lacks, and no more
// than about twice its length. The log says why one was refused, never what it said.
func TestATripMessagesRewriteMayAddNothing(t *testing.T) {
	f := newFixture(t)
	planned := f.add(draft(1, 3))
	original := "Example trip: calendar changes" + bullet + "Museum visit: Sat 17 Oct 13:00\u201314:00 (was 14:00\u201315:00)"
	header := "Example trip: cambios en el calendario"
	for _, test := range []struct {
		name   string
		answer string
		reason string
	}{
		{"a faithful rewrite", header + bullet + "Museum visit: s\u00e1b 17 oct 13:00\u201314:00 (antes 14:00\u201315:00)", ""},
		{"digits of another script", header + bullet + "Museum visit: \u0661\u0667 oct \u0661\u0663:\u0660\u0660\u2013\u0661\u0664:\u0660\u0660 (\u0661\u0664:\u0660\u0660\u2013\u0661\u0665:\u0660\u0660)", ""},
		{"swapped times", header + bullet + "Museum visit: s\u00e1b 17 oct 14:00\u201315:00 (antes 13:00\u201314:00)", "numbers"},
		{"a number moved to another line", header + " 17" + bullet + "Museum visit: s\u00e1b oct 13:00\u201314:00 (antes 14:00\u201315:00)", "numbers"},
		{"an added line", header + bullet + "Museum visit: s\u00e1b 17 oct 13:00\u201314:00 (antes 14:00\u201315:00)" + bullet + "Reserve ahora.", "lines"},
		{"an added address", header + bullet + "Museum visit: s\u00e1b 17 oct 13:00\u201314:00 (antes 14:00\u201315:00) example.com", "address"},
		{"an added handle", header + bullet + "Museum visit: s\u00e1b 17 oct 13:00\u201314:00 (antes 14:00\u201315:00) (@example_bot)", "address"},
		{"an added command", header + " /start" + bullet + "Museum visit: s\u00e1b 17 oct 13:00\u201314:00 (antes 14:00\u201315:00)", "address"},
		{"an appended sentence", header + bullet + "Museum visit: s\u00e1b 17 oct 13:00\u201314:00 (antes 14:00\u201315:00). " + strings.Repeat("Reserve ahora mismo. ", 10), "length"},
	} {
		var logs strings.Builder
		writing := fakeWriting{tab: 7, messages: map[int64]string{7: "hola"}}
		sender := &recordingSender{}
		language, err := trip.NewLanguage(writing, writing, &fakeRewriter{answer: test.answer}, f.store, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := language.Sender(sender).Send(t.Context(), "alice", planned.ID, original); err != nil {
			t.Fatal(err)
		}
		want := original
		if test.reason == "" {
			want = test.answer
		}
		if got := sender.texts(); len(got) != 1 || got[0] != want {
			t.Fatalf("%s: sent %q, want %q", test.name, got, want)
		}
		if test.reason != "" && (!strings.Contains(logs.String(), "outcome=rejected reason="+test.reason) || strings.Contains(logs.String(), "Museum")) {
			t.Fatalf("%s: log = %q, want the reason %q and no text", test.name, logs.String(), test.reason)
		}
	}
}

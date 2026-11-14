package watch_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// noticesEngine is an engine whose service knows the owner's chosen language and whose rewrite
// hands the text back, so what the code wrote is what is read.
func noticesEngine(t *testing.T, chosen *locale.Language) (*engine, *fakeRewriter) {
	t.Helper()
	e := newEngine(t)
	if chosen != nil {
		if err := e.store.SaveLanguage(e.ctx, "alice", *chosen); err != nil {
			t.Fatal(err)
		}
	}
	rewriter := &fakeRewriter{}
	rewriter.answer(func(text string) (string, error) { return text, nil })
	service, err := watch.NewService(watch.Config{Zone: "Europe/Amsterdam", Now: e.clock.Now, Rewriter: rewriter, Languages: e.store},
		e.store, e.store, e.sender, nil, slog.New(slog.NewTextHandler(e.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Register(watch.KindDeparture, e.evaluator); err != nil {
		t.Fatal(err)
	}
	e.service = service
	return e, rewriter
}

// A Russian user's watch with no language of its own is rewritten into Russian, and its fixed
// recovery and failure lines are written in Russian before that; an English user's watch never
// reaches the model and keeps its English lines.
func TestRussianUsersWatchNoticesAreWrittenAndRewrittenInRussian(t *testing.T) {
	russian := locale.Russian
	e, rewriter := noticesEngine(t, &russian)
	e.create("alice", watchID, departureSpec())

	e.evaluator.set(late, nil)
	e.at("2026-09-28 06:25")
	if sent := e.wantSent(1, "It is 8 minutes late."); !strings.Contains(sent[0].text, "Поезд") {
		t.Fatalf("alert = %q, want the label in Russian", sent[0].text)
	}
	e.evaluator.set(onTime, nil)
	e.at("2026-09-28 06:40")
	recovered := e.wantSent(1, "Снова всё в порядке.")
	if strings.Contains(recovered[0].text, "Back to normal.") {
		t.Fatalf("recovery = %q, want Russian", recovered[0].text)
	}
	if len(rewriter.calls) != 2 || rewriter.calls[0] != "alice Russian" {
		t.Fatalf("rewrites = %v, want each alert asked for in Russian", rewriter.calls)
	}

	e.evaluator.set(watch.Observation{}, &watch.UpstreamError{Service: "ns", Err: errors.New("503 Service Unavailable")})
	e.at("2026-09-29 06:15")
	e.at("2026-09-29 06:20")
	e.at("2026-09-29 06:25")
	failure := e.wantSent(1, "Сейчас не могу это проверить.")
	if strings.Contains(failure[0].text, "can't check") {
		t.Fatalf("failure = %q", failure[0].text)
	}
}

func TestEnglishUsersWatchNeverReachesTheRewriteAndKeepsItsEnglishLines(t *testing.T) {
	e, rewriter := noticesEngine(t, nil)
	e.create("alice", watchID, departureSpec())

	e.evaluator.set(late, nil)
	e.at("2026-09-28 06:25")
	e.wantSent(1, "It is 8 minutes late.")
	e.evaluator.set(onTime, nil)
	e.at("2026-09-28 06:40")
	e.wantSent(1, "Back to normal.")
	if len(rewriter.calls) != 0 {
		t.Fatalf("rewrites = %v, want none", rewriter.calls)
	}
}

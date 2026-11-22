package eval_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/eval"
)

func replayOf(t *testing.T, f *runFixture, source *eval.Result) *eval.Result {
	t.Helper()
	replayed, err := eval.Replay(t.Context(), eval.ReplayOptions{ResultsDir: f.options.ResultsDir, SourceRunID: source.RunID, CasesDir: f.cases})
	if err != nil {
		t.Fatal(err)
	}
	return replayed
}

func TestReplayWithAChangedCheckChangesVerdictsWithoutAModel(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, 10)
	f.addCase("greets", "happy", 2, echoes("reply:"))
	f.addCase("too-strict", "wrong-date", 1, echoes("never said"))
	f.addCase("too-loose", "wrong-date", 1, echoes("reply:"))

	source, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if got := source.Summary.Overall; got.Passed != 3 || got.Ran != 4 {
		t.Fatalf("source overall = %+v, want 3 of 4", got)
	}
	review, err := json.Marshal(eval.ReviewFile{RunID: source.RunID, Reviews: []eval.ReviewEntry{
		{Case: "too-loose", Trial: 1, Verdict: eval.VerdictBad, ReviewedAt: time.Now()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Dir, "review.json"), review, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := f.runs.Load()

	// Fix the strict check and tighten the loose one.
	f.addCase("too-strict", "wrong-date", 1, echoes("reply:"))
	f.addCase("too-loose", "wrong-date", 1, echoes("never said"))

	replayed := replayOf(t, f, source)
	if f.runs.Load() != calls {
		t.Fatalf("replay started the model: %d runs, was %d", f.runs.Load(), calls)
	}
	if !replayed.Replayed || replayed.ReplayOf != source.RunID || replayed.RunID == source.RunID {
		t.Fatalf("replayed = %v of %q as %q", replayed.Replayed, replayed.ReplayOf, replayed.RunID)
	}
	if got := replayed.Summary.Overall; got.Passed != 3 || got.Ran != 4 {
		t.Fatalf("replayed overall = %+v, want 3 of 4 with the verdicts swapped", got)
	}
	status := map[string]string{}
	for _, c := range replayed.Cases {
		status[c.ID] = c.Trials[0].Status
	}
	if status["too-strict"] != eval.TrialPassed || status["too-loose"] != eval.TrialFailed || status["greets"] != eval.TrialPassed {
		t.Fatalf("statuses = %v", status)
	}
	if got := strings.Join(replayed.ReplayChanges, "; "); got != "too-loose#1: passed -> failed; too-strict#1: failed -> passed" &&
		got != "too-strict#1: failed -> passed; too-loose#1: passed -> failed" {
		t.Fatalf("changes = %q", got)
	}

	report, err := os.ReadFile(filepath.Join(replayed.Dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"replayed from the cassettes of run " + source.RunID, "no model ran", "too-strict#1: failed -> passed", "Missed failures"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("replay report lacks %q:\n%s", want, report)
		}
	}
	if stored, err := eval.LoadResult(replayed.Dir); err != nil || !stored.Replayed || stored.ReplayOf != source.RunID || stored.ModelCalls != source.ModelCalls {
		t.Fatalf("stored replay = %+v, %v", stored, err)
	}

	// The source run is untouched, and the replay itself can be replayed.
	if again, err := eval.LoadResult(source.Dir); err != nil || again.Replayed || again.Summary.Overall != source.Summary.Overall {
		t.Fatalf("source changed: %+v, %v", again, err)
	}
	if second := replayOf(t, f, replayed); second.Summary.Overall != replayed.Summary.Overall || second.ReplayOf != replayed.RunID || len(second.ReplayChanges) != 0 {
		t.Fatalf("replaying a replay: %+v", second)
	}

	if err := eval.ImportRun(t.Context(), f.options.ResultsDir, replayed.RunID); err != nil {
		t.Fatal(err)
	}
}

func TestReplayReportsACaseThatNoLongerMatchesItsCassette(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, 10)
	f.addCase("shifts", "happy", 1, echoes("reply:"))
	f.addCase("vanishes", "happy", 1, echoes("reply:"))
	source, err := f.run()
	if err != nil {
		t.Fatal(err)
	}

	f.addCase("shifts", "happy", 1, echoes("reply:"), echoes("reply:"))
	if err := os.Remove(filepath.Join(f.cases, "vanishes.json")); err != nil {
		t.Fatal(err)
	}
	replayed := replayOf(t, f, source)
	for _, c := range replayed.Cases {
		trial := c.Trials[0]
		if trial.Status != eval.TrialError {
			t.Errorf("%s: status %s, want error", c.ID, trial.Status)
		}
		if c.ID == "shifts" && !strings.Contains(trial.Error, "2 step(s) now and its cassette 1") {
			t.Errorf("shifts error = %q", trial.Error)
		}
		if c.ID == "vanishes" && !strings.Contains(trial.Error, "no longer exists") {
			t.Errorf("vanishes error = %q", trial.Error)
		}
	}
}

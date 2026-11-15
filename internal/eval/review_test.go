package eval_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/eval"
)

func reviewRun(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "20260101-000000")
	if err := os.MkdirAll(filepath.Join(dir, "cassettes"), 0o700); err != nil {
		t.Fatal(err)
	}
	trial := func(caseID string, n int, status string) eval.TrialResult {
		result := eval.TrialResult{N: n, Status: status}
		if status == eval.TrialFailed {
			result.Failures = []eval.Failure{{Step: 1, Check: "calls", Message: "weather was never called"}}
		}
		if n == 1 {
			result.Cassette = filepath.Join("cassettes", caseID+"-1.json")
			cassette := eval.Cassette{Case: caseID, Trial: 1, Steps: []eval.CassetteStep{{
				Step: eval.Step{Say: "what is the weather tomorrow?", Expect: &eval.Expect{
					Reply: &eval.ReplyExpect{Contains: []string{"sunny"}},
				}},
				ToolCalls: []eval.ToolCall{{Name: "weather", Arguments: json.RawMessage(`{"day":"tomorrow"}`), Result: "No place called\nhome was found", IsError: true}},
				Reply:     "It will be sunny tomorrow with a light breeze.",
			}}}
			encoded, err := json.Marshal(cassette)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, result.Cassette), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	result := &eval.Result{
		RunID: "20260101-000000", Dir: dir, PiVersion: "1", Budget: 10,
		Cases: []eval.CaseResult{
			{ID: "weather", Title: "Asks for the weather", Kind: "wrong-date", Tags: []string{"daily"},
				Trials: []eval.TrialResult{trial("weather", 1, eval.TrialPassed), trial("weather", 2, eval.TrialFailed)}},
			{ID: "alarm", Kind: "happy", Trials: []eval.TrialResult{trial("alarm", 1, eval.TrialFailed), trial("alarm", 2, eval.TrialPassed)}},
		},
	}
	result.Summarize()
	if err := result.WriteReport(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func review(t *testing.T, dir string, all bool, input string) string {
	t.Helper()
	var out bytes.Buffer
	if err := eval.Review(eval.ReviewOptions{Dir: dir, All: all, In: strings.NewReader(input), Out: &out}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestReviewStoresVerdictsAndResumesAtTheFirstUnreviewedTrial(t *testing.T) {
	dir := reviewRun(t)

	shown := review(t, dir, false, "g\nlooks right\nq\n")
	for _, want := range []string{
		"alarm trial 1", "[1/4]", "what is the weather tomorrow?", `tool weather({"day":"tomorrow"}) -> ERROR`,
		"= No place called home was found", `expects: {"reply":{"contains":["sunny"]}}`,
		"It will be sunny tomorrow", "checks: FAILED", "step 1, calls: weather was never called",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("view lacks %q:\n%s", want, shown)
		}
	}
	stored, found, err := eval.LoadReview(dir)
	if err != nil || !found || len(stored.Reviews) != 1 {
		t.Fatalf("review = %+v, found %v, err %v", stored, found, err)
	}
	if entry := stored.Reviews[0]; entry.Case != "alarm" || entry.Trial != 1 || entry.Verdict != "good" || entry.Note != "looks right" || entry.ReviewedAt.IsZero() {
		t.Fatalf("entry = %+v", entry)
	}

	resumed := review(t, dir, false, "s\nb\n\nu\nunclear\n")
	if !strings.Contains(resumed, "alarm trial 2") || strings.Contains(resumed, "[1/4]") || !strings.Contains(resumed, "[1/3]") {
		t.Fatalf("resumed session did not start at the first unreviewed trial:\n%s", resumed)
	}
	stored, _, _ = eval.LoadReview(dir)
	got := map[string]string{}
	for _, entry := range stored.Reviews {
		got[entry.Key()] = entry.Verdict
	}
	want := map[string]string{"alarm#1": "good", "weather#1": "bad", "weather#2": "unsure"}
	if len(got) != len(want) || got["alarm#1"] != want["alarm#1"] || got["weather#1"] != want["weather#1"] || got["weather#2"] != want["weather#2"] {
		t.Fatalf("verdicts = %v, want %v", got, want)
	}

	review(t, dir, false, "g\n\n")
	if again := review(t, dir, false, ""); !strings.Contains(again, "nothing to review") {
		t.Fatalf("a fully reviewed run should have nothing to review:\n%s", again)
	}
	if all := review(t, dir, true, "q\n"); !strings.Contains(all, "[1/4]") || !strings.Contains(all, "previous verdict: good (looks right)") {
		t.Fatalf("--all should show reviewed trials again:\n%s", all)
	}
}

func TestReviewLimitedToCasesShowsOnlyTheirTrials(t *testing.T) {
	dir := reviewRun(t)

	shown := review(t, dir, false, "q\n")
	if !strings.Contains(shown, "[1/4]") {
		t.Fatalf("an unfiltered review should queue every trial:\n%s", shown)
	}
	var out bytes.Buffer
	if err := eval.Review(eval.ReviewOptions{Dir: dir, Cases: []string{"weather"}, In: strings.NewReader("q\n"), Out: &out}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "weather trial 1") || !strings.Contains(got, "[1/2]") || strings.Contains(got, "alarm trial") {
		t.Fatalf("review limited to weather showed:\n%s", got)
	}
	err := eval.Review(eval.ReviewOptions{Dir: dir, Cases: []string{"wether"}, In: strings.NewReader(""), Out: &out})
	if err == nil || !strings.Contains(err.Error(), `"wether"`) {
		t.Fatalf("an unknown case ID should be refused, got %v", err)
	}
}

func TestReviewReportCountsAgreementAndCallsOutMissedFailuresAndFalseAlarms(t *testing.T) {
	dir := reviewRun(t)
	// alarm#1 failed, operator good: false alarm. alarm#2 passed, operator good: agree.
	// weather#1 passed, operator bad: missed failure. weather#2 failed, operator unsure: excluded.
	review(t, dir, false, "g\nacceptable wording\ng\n\nb\nreply is wrong\nu\n\n")

	report, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Operator review", "Reviewed trials: 4 (unsure, excluded from agreement: 1)",
		"Agreement between the checks and the operator: 33.3% (1/3)",
		"Missed failures (operator bad, checks passed): 1", "False alarms (operator good, checks failed): 1",
		"| alarm | 50.0% (1/2) |", "| weather | 0.0% (0/1) |",
		"### Missed failures (operator bad, checks passed)\n\n- weather, trial 1: reply is wrong",
		"### False alarms (operator good, checks failed)\n\n- alarm, trial 1: acceptable wording",
	} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}

	if err := os.Remove(filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
	if err := eval.RewriteReport(dir); err != nil {
		t.Fatal(err)
	}
	if rewritten, err := os.ReadFile(filepath.Join(dir, "report.md")); err != nil || !bytes.Equal(rewritten, report) {
		t.Fatalf("rewritten report differs: %v", err)
	}
}

func TestLatestRunPicksTheNewestRunDirectory(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"20260101-000000", "20260301-120000", "20260201-000000"} {
		if err := os.MkdirAll(filepath.Join(root, id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, id, "result.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if latest, err := eval.LatestRun(root); err != nil || latest != "20260301-120000" {
		t.Fatalf("latest = %q, %v", latest, err)
	}
}

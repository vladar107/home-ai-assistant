package eval

import (
	"strings"
	"testing"
)

func TestReportShowsTheHappyLinePerKindRatesAndTypeSafeTrials(t *testing.T) {
	trial := func(n int, status string, typeSafe bool) TrialResult {
		return TrialResult{N: n, Status: status, TypeSafe: typeSafe}
	}
	result := &Result{
		RunID: "r1", PiVersion: "1.1.0", Provider: "openai-codex", Model: "gpt-test", Effort: "low", Commit: "abc", Budget: 300,
		Cases: []CaseResult{
			{ID: "greet", Kind: KindHappy, Trials: []TrialResult{trial(1, TrialPassed, true), trial(2, TrialFailed, true)}},
			{ID: "date", Kind: "wrong-date", Tags: []string{KindHappy}, Trials: []TrialResult{trial(1, TrialPassed, false), trial(2, TrialPassed, false)}},
			{ID: "remind", Kind: "wrong-date", Trials: []TrialResult{trial(1, TrialFailed, true), trial(2, TrialAborted, true)}},
		},
	}
	result.Summarize()
	report := result.Markdown()

	for _, want := range []string{
		"Pass rate: 60.0% (3/5)", "Happy-path pass rate: 75.0% (3/4)", "pass^k (k=2, every trial of a case passes): 33.3%",
		"| happy | 50.0% (1/2) |", "| wrong-date | 66.7% (2/3) |", "3 trial(s) ran with the safety checks", "Off in: date#1, date#2.",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

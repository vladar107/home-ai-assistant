package eval

import (
	"bytes"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func near(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func TestPairedDifferencesOnAHandComputedExample(t *testing.T) {
	t.Parallel()
	// Differences 1, 0, -1, 1, 1: mean 0.4; squared deviations 0.36+0.16+1.96+0.36+0.36 = 3.2;
	// variance 3.2/4 = 0.8; standard error sqrt(0.8/5) = 0.4; interval 0.4 ± 1.96*0.4.
	p := PairedDifferences([]float64{0, 1, 1, 0, 0}, []float64{1, 1, 0, 1, 1})
	if p.N != 5 || !near(p.Mean, 0.4) || !near(p.SE, 0.4) {
		t.Fatalf("paired = %+v", p)
	}
	if !near(p.Low, 0.4-1.96*0.4) || !near(p.High, 0.4+1.96*0.4) || !near(p.MDD, (1.96+0.8416)*0.4) {
		t.Errorf("interval = [%v, %v], mdd %v", p.Low, p.High, p.MDD)
	}
	if p.Better != 3 || p.Worse != 1 || p.Same != 1 {
		t.Errorf("better %d worse %d same %d", p.Better, p.Worse, p.Same)
	}
	if p.Detected() {
		t.Error("an interval that includes zero was reported as detected")
	}
}

func TestPairedDifferencesDetectsAConsistentGain(t *testing.T) {
	t.Parallel()
	// Differences 0.5, 0.5, 0.25, 0.75: mean 0.5, squared deviations 0+0+0.0625+0.0625, sd sqrt(0.125/3).
	p := PairedDifferences([]float64{0, 0.5, 0.75, 0}, []float64{0.5, 1, 1, 0.75})
	wantSE := math.Sqrt(0.125/3) / 2
	if !near(p.Mean, 0.5) || !near(p.SE, wantSE) || !p.Detected() || p.Low <= 0 {
		t.Fatalf("paired = %+v, want mean 0.5, se %v and the interval above zero", p, wantSE)
	}
}

func TestPairedDifferencesOfIdenticalRatesFindsNothing(t *testing.T) {
	t.Parallel()
	rates := []float64{0, 0.3333333333333333, 1, 0.6666666666666666}
	p := PairedDifferences(rates, rates)
	if p.Mean != 0 || p.SE != 0 || p.Detected() || p.Better != 0 || p.Worse != 0 || p.Same != 4 {
		t.Fatalf("paired = %+v", p)
	}
}

func TestPairedDifferencesWithOneCaseHasNoInterval(t *testing.T) {
	t.Parallel()
	p := PairedDifferences([]float64{0}, []float64{1})
	if p.N != 1 || p.Mean != 1 || p.SE != 0 || p.Detected() {
		t.Fatalf("paired = %+v", p)
	}
	if got := PairedDifferences(nil, nil); got.N != 0 {
		t.Fatalf("empty = %+v", got)
	}
}

// storedRun builds a run whose cases each have the given trial outcomes, p for passed and f for failed.
func storedRun(id, variant, fingerprint string, started time.Time, cases map[string]string) *Result {
	r := &Result{
		RunID: id, StartedAt: started, FinishedAt: started.Add(time.Minute), Commit: "c1", PiVersion: "1.0", Provider: "p",
		Model: "m", Effort: "low", PromptFingerprint: fingerprint, Dir: filepath.Join("results", id),
	}
	if variant != "" {
		r.Variant = &VariantInfo{Name: variant, SHA256: strings.Repeat("a", 64)}
	}
	for _, name := range slices.Sorted(maps.Keys(cases)) {
		c := CaseResult{ID: name, Title: name, Kind: "wrong-time", Tags: nil}
		if strings.HasPrefix(name, "happy-") {
			c.Kind = KindHappy
		}
		for i, outcome := range cases[name] {
			trial := TrialResult{
				N: i + 1, Status: TrialPassed, Model: "m", Effort: "low", ModelCalls: 2,
				Tokens: agentruntime.TokenCounts{Input: 100, Output: 10},
			}
			if outcome == 'f' {
				trial.Status = TrialFailed
				trial.Failures = []Failure{{Step: 1, Check: "reply.contains", Message: "missing"}}
			}
			c.Trials = append(c.Trials, trial)
		}
		r.Cases = append(r.Cases, c)
		r.ModelCalls += 2 * len(c.Trials)
	}
	r.Summarize()
	return r
}

func importAll(t *testing.T, store *ResultsStore, runs ...*Result) {
	t.Helper()
	for _, r := range runs {
		if err := store.Import(t.Context(), r, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func openTestStore(t *testing.T) *ResultsStore {
	t.Helper()
	store, err := OpenResultsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestComparingARunWithItselfReportsNoDifference(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	run := storedRun("run-a", "", "fp-a", start, map[string]string{"one": "ppp", "two": "pfp", "three": "fff", "happy-four": "ppp"})
	importAll(t, store, run)

	c, err := Compare(t.Context(), store, "run-a", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if c.Paired.Mean != 0 || c.Paired.Detected() || c.Paired.Better != 0 || c.Paired.Worse != 0 || c.Paired.Same != 4 {
		t.Fatalf("paired = %+v", c.Paired)
	}
	if !strings.Contains(c.Verdict(), "No difference was detected") {
		t.Errorf("verdict = %q", c.Verdict())
	}
}

func TestCompareIsPairedAndUsesOnlySharedCases(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	base := storedRun("run-a", "", "fp-a", start, map[string]string{"one": "fff", "two": "ppf", "three": "ppp", "only-a": "ppp"})
	variant := storedRun("run-b", "better", "fp-b", start.Add(time.Hour), map[string]string{"one": "ppp", "two": "ppp", "three": "ppf", "only-b": "fff"})
	importAll(t, store, base, variant)

	c, err := Compare(t.Context(), store, "run-a", "run-b")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Shared, ","); got != "one,three,two" {
		t.Errorf("shared = %s", got)
	}
	if strings.Join(c.OnlyFirst, ",") != "only-a" || strings.Join(c.OnlySecond, ",") != "only-b" {
		t.Errorf("only first %v, only second %v", c.OnlyFirst, c.OnlySecond)
	}
	// Differences: one +1, two +1/3, three -1/3; mean 1/3; the standard error follows from them.
	diffs := []float64{1, 1.0 / 3, -1.0 / 3}
	mean := (diffs[0] + diffs[1] + diffs[2]) / 3
	var squares float64
	for _, d := range diffs {
		squares += (d - mean) * (d - mean)
	}
	wantSE := math.Sqrt(squares/2) / math.Sqrt(3)
	if !near(c.Paired.Mean, mean) || !near(c.Paired.SE, wantSE) || c.Paired.Better != 2 || c.Paired.Worse != 1 {
		t.Errorf("paired = %+v, want mean %v se %v", c.Paired, mean, wantSE)
	}
	if c.FirstRate.Passed != 5 || c.FirstRate.Ran != 9 || c.SecondRate.Passed != 8 || c.SecondRate.Ran != 9 {
		t.Errorf("shared pass rates %v and %v", c.FirstRate, c.SecondRate)
	}
	if !near(c.FirstPassK, 1.0/3) || !near(c.SecondPassK, 2.0/3) {
		t.Errorf("pass^k %v and %v", c.FirstPassK, c.SecondPassK)
	}

	markdown := c.Markdown()
	for _, want := range []string{
		"Compared on 3 shared case(s)", "only in the first run (only-a)", "only in the second (only-b)",
		"Standard error", "95% interval", "Minimum detectable difference", "Cases better: 2, worse: 1",
		"Model calls per trial | 2.0 | 2.0", "Input tokens per trial | 100.0 | 100.0", "variant `better`",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("comparison lacks %q:\n%s", want, markdown)
		}
	}
	path, err := WriteComparison(t.TempDir(), c)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "compare.md" {
		t.Errorf("path = %s", path)
	}
	if written, err := os.ReadFile(path); err != nil || string(written) != markdown {
		t.Errorf("written comparison differs: %v", err)
	}
}

func TestCompareOnAWeakerModelListsCasesThatDidNotDrop(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	base := storedRun("run-a", "", "fp-a", start, map[string]string{"steady": "ppp", "dropped": "ppp", "failing": "pfp", "short": "pp", "pinned": "ppp"})
	weak := storedRun("run-b", "weak-model", "fp-a", start.Add(time.Hour), map[string]string{"steady": "ppp", "dropped": "pfp", "failing": "ppp", "short": "pp", "pinned": "ppp"})
	weak.Model = "m-small"
	for _, run := range []*Result{base, weak} {
		for i := range run.Cases {
			for j := range run.Cases[i].Trials {
				if run.Cases[i].ID == "pinned" {
					run.Cases[i].Trials[j].Model = "m-pinned"
				} else {
					run.Cases[i].Trials[j].Model = run.Model
				}
			}
		}
	}
	weak.Summarize()
	importAll(t, store, base, weak)

	c, err := Compare(t.Context(), store, "run-a", "run-b")
	if err != nil {
		t.Fatal(err)
	}
	if !c.ModelChanged || !slices.Equal(c.NotSensitive, []string{"steady"}) {
		t.Fatalf("model changed %v, not sensitive %v; want only steady", c.ModelChanged, c.NotSensitive)
	}
	markdown := c.Markdown()
	section := markdown[strings.Index(markdown, "## Not sensitive to the model"):]
	if !strings.Contains(markdown, "different model, m-small instead of m") || !strings.Contains(section, "- steady\n") {
		t.Errorf("comparison lacks the sensitivity mark or section:\n%s", markdown)
	}
	for _, unlisted := range []string{"- dropped", "- failing", "- short", "- pinned"} {
		if strings.Contains(section, unlisted) {
			t.Errorf("section lists %q:\n%s", unlisted, section)
		}
	}
	if !strings.Contains(c.Summary(), "1 case(s) not sensitive to the model") {
		t.Errorf("summary = %q", c.Summary())
	}

	same, err := Compare(t.Context(), store, "run-a", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if same.ModelChanged || same.NotSensitive != nil || strings.Contains(same.Markdown(), "Not sensitive") || strings.Contains(same.Summary(), "sensitive") {
		t.Errorf("a comparison on one model shows a sensitivity section:\n%s", same.Markdown())
	}
}

func TestCompareRefusesRunsWithoutSharedCases(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	importAll(t, store, storedRun("run-a", "", "x", start, map[string]string{"one": "p"}), storedRun("run-b", "", "x", start, map[string]string{"two": "p"}))
	if _, err := Compare(t.Context(), store, "run-a", "run-b"); err == nil || !strings.Contains(err.Error(), "share no case") {
		t.Fatalf("got %v", err)
	}
	if _, err := Compare(t.Context(), store, "run-a", "missing"); err == nil {
		t.Fatal("an unknown run was accepted")
	}
}

func TestImportReplacesARunAndKeepsTheVerdicts(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	run := storedRun("run-a", "", "x", start, map[string]string{"one": "pf"})
	review := &ReviewFile{RunID: "run-a", Reviews: []ReviewEntry{{Case: "one", Trial: 2, Verdict: VerdictBad, Note: "wrong", ReviewedAt: start}}}
	for range 2 {
		if err := store.Import(t.Context(), run, review); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := store.Runs(t.Context())
	if err != nil || len(runs) != 1 || runs[0].Reviewed != 1 || runs[0].Overall.Passed != 1 || runs[0].Overall.Ran != 2 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	var checks, verdicts int
	if err := store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM checks), (SELECT COUNT(*) FROM verdicts)`).Scan(&checks, &verdicts); err != nil || checks != 1 || verdicts != 1 {
		t.Fatalf("checks %d verdicts %d: %v", checks, verdicts, err)
	}
}

func TestImportRunDirReadsResultAndReview(t *testing.T) {
	t.Parallel()
	results := t.TempDir()
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	run := storedRun("20260301-090000", "", "x", start, map[string]string{"one": "pp"})
	run.Dir = filepath.Join(results, run.RunID)
	if err := run.WriteReport(); err != nil {
		t.Fatal(err)
	}
	if err := saveReview(run.Dir, &ReviewFile{RunID: run.RunID, Reviews: []ReviewEntry{{Case: "one", Trial: 1, Verdict: VerdictGood, ReviewedAt: start}}}); err != nil {
		t.Fatal(err)
	}
	if err := ImportRun(t.Context(), results, run.RunID); err != nil {
		t.Fatal(err)
	}
	store, err := OpenResultsStore(results)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runs, err := store.Runs(t.Context())
	if err != nil || len(runs) != 1 || runs[0].Reviewed != 1 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	ids, err := store.ImportAll(t.Context(), results)
	if err != nil || len(ids) != 1 {
		t.Fatalf("import all = %v, %v", ids, err)
	}
}

func TestTrendSplitsRunsWherePromptModelOrPiChanged(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	cases := map[string]string{"one": "ppp", "happy-two": "ppf"}
	later := storedRun("run-3", "", "fp-a", start.Add(2*time.Hour), cases)
	later.Model = "m2"
	later.Summarize()
	importAll(t, store,
		storedRun("run-1", "", "fp-a", start, cases),
		storedRun("run-2", "tighter", "fp-b", start.Add(time.Hour), cases),
		later,
		storedRun("run-4", "", "fp-a", start.Add(3*time.Hour), cases),
	)
	runs, err := store.Runs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	WriteTrend(&out, runs)
	text := out.String()
	for _, want := range []string{"prompt fp-a, model m (low), Pi 1.0", "prompt fp-b, model m (low), Pi 1.0", "prompt fp-a, model m2 (low), Pi 1.0", "83.3% (5/6)", "66.7%", "tighter"} {
		if !strings.Contains(text, want) {
			t.Errorf("trend lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "prompt fp-a, model m (low)") != 1 || strings.Index(text, "run-1") > strings.Index(text, "run-4") {
		t.Errorf("run-4 must follow run-1 in the first group:\n%s", text)
	}
	var empty bytes.Buffer
	WriteTrend(&empty, nil)
	if !strings.Contains(empty.String(), "no runs") {
		t.Errorf("empty trend = %q", empty.String())
	}
}

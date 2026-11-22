package eval

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

type boardRun struct {
	RunID, StartedAt, Commit, Model, Effort, Fingerprint, PiVersion, Variant string
	Time                                                                     int64
	Cases, TrialsRan, TrialsPassed, K, FullSuite, Regression                 int
	PassRate, PassK, TokensPerTrial, ModelCallsPerTrial                      float64
	HappyRate                                                                sql.NullFloat64
}

func boardRuns(t *testing.T, store *ResultsStore) []boardRun {
	t.Helper()
	rows, err := store.db.Query(`
SELECT run_id, time, started_at, commit_name, model, effort, fingerprint, pi_version, variant, cases, trials_ran, trials_passed,
       pass_rate, pass_k, k, happy_rate, tokens_per_trial, model_calls_per_trial, full_suite, regression
FROM eval_runs ORDER BY run_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var runs []boardRun
	for rows.Next() {
		var r boardRun
		if err := rows.Scan(&r.RunID, &r.Time, &r.StartedAt, &r.Commit, &r.Model, &r.Effort, &r.Fingerprint, &r.PiVersion, &r.Variant,
			&r.Cases, &r.TrialsRan, &r.TrialsPassed, &r.PassRate, &r.PassK, &r.K, &r.HappyRate, &r.TokensPerTrial, &r.ModelCallsPerTrial,
			&r.FullSuite, &r.Regression); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return runs
}

// fullRun is a hand-built run that played the whole suite.
func fullRun(id string, started time.Time, cases map[string]string) *Result {
	r := storedRun(id, "", strings.Repeat("0123456789abcdef", 4), started, cases)
	r.Selection = &Selection{FullSuite: true}
	return r
}

func TestViewsGiveTheBoardExactRunCaseAndCheckRows(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	full := fullRun("run-a", start, map[string]string{"happy-one": "pp", "two": "pf"})
	two := &full.Cases[1]
	two.Trials = append(two.Trials, TrialResult{N: 3, Status: TrialAborted, Model: "m", Effort: "low"})
	full.Tokens = agentruntime.TokenCounts{Input: 400, Output: 40, CacheRead: 1000}
	full.Summarize()
	selected := storedRun("run-b", "", "short", start.Add(time.Hour), map[string]string{"two": "f"})
	selected.Selection = &Selection{Tags: []string{"dates"}}
	importAll(t, store, full, selected)

	got := boardRuns(t, store)
	want := []boardRun{
		{
			RunID: "run-a", Time: start.Unix(), StartedAt: "2026-03-01T09:00:00Z", Commit: "c1", Model: "m", Effort: "low",
			Fingerprint: "0123456789ab", PiVersion: "1.0", Cases: 2, TrialsRan: 4, TrialsPassed: 3, PassRate: 0.75,
			PassK: 0.5, K: 2, HappyRate: sql.NullFloat64{Float64: 1, Valid: true}, TokensPerTrial: 110, ModelCallsPerTrial: 2,
			FullSuite: 1, Regression: 1,
		},
		{
			RunID: "run-b", Time: start.Add(time.Hour).Unix(), StartedAt: "2026-03-01T10:00:00Z", Commit: "c1", Model: "m",
			Effort: "low", Fingerprint: "short", PiVersion: "1.0", Cases: 1, TrialsRan: 1, K: 1, ModelCallsPerTrial: 2,
		},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("eval_runs =\n%+v\nwant\n%+v", got, want)
	}

	rows, err := store.db.Query(`SELECT run_id, time, regression, case_id, kind, happy, trials, passed, pass_rate FROM eval_case_rates ORDER BY run_id, case_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var rates []string
	for rows.Next() {
		var runID, caseID, kind string
		var at int64
		var regression, happy, trials, passed int
		var rate float64
		if err := rows.Scan(&runID, &at, &regression, &caseID, &kind, &happy, &trials, &passed, &rate); err != nil {
			t.Fatal(err)
		}
		rates = append(rates, fmt.Sprintf("%s %d %d %s %s %d %d %d %.2f", runID, at, regression, caseID, kind, happy, trials, passed, rate))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	a, b := start.Unix(), start.Add(time.Hour).Unix()
	wantRates := []string{
		fmt.Sprintf("run-a %d 1 happy-one happy 1 2 2 1.00", a),
		fmt.Sprintf("run-a %d 1 two wrong-time 0 2 1 0.50", a),
		fmt.Sprintf("run-b %d 0 two wrong-time 0 1 0 0.00", b),
	}
	if !slices.Equal(rates, wantRates) {
		t.Fatalf("eval_case_rates =\n%s\nwant\n%s", strings.Join(rates, "\n"), strings.Join(wantRates, "\n"))
	}

	checks, err := store.db.Query(`SELECT run_id, time, case_id, trial, step, check_name, message FROM eval_failed_checks ORDER BY run_id, case_id, trial`)
	if err != nil {
		t.Fatal(err)
	}
	defer checks.Close()
	var failed []string
	for checks.Next() {
		var runID, caseID, check, message string
		var at int64
		var trial, step int
		if err := checks.Scan(&runID, &at, &caseID, &trial, &step, &check, &message); err != nil {
			t.Fatal(err)
		}
		failed = append(failed, fmt.Sprintf("%s %d %s#%d step %d %s: %s", runID, at, caseID, trial, step, check, message))
	}
	if err := checks.Err(); err != nil {
		t.Fatal(err)
	}
	wantFailed := []string{
		fmt.Sprintf("run-a %d two#2 step 1 reply.contains: missing", a),
		fmt.Sprintf("run-b %d two#1 step 1 reply.contains: missing", b),
	}
	if !slices.Equal(failed, wantFailed) {
		t.Fatalf("eval_failed_checks =\n%s\nwant\n%s", strings.Join(failed, "\n"), strings.Join(wantFailed, "\n"))
	}
}

func TestViewsExposeNoFilePath(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	for _, view := range []string{"eval_runs", "eval_case_rates", "eval_failed_checks"} {
		var columns []string
		rows, err := store.db.Query(`SELECT name FROM pragma_table_info(?)`, view)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			columns = append(columns, name)
		}
		rows.Close()
		if len(columns) == 0 || slices.Contains(columns, "dir") {
			t.Errorf("%s columns = %v", view, columns)
		}
	}
}

func passingCases(n int) map[string]string {
	all := map[string]string{}
	for i := range n {
		all[fmt.Sprintf("case-%02d", i+1)] = "p"
	}
	return all
}

func TestOnlyAFullSuiteRunOfTheProductionPromptIsARegressionRun(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	byCase := fullRun("by-case", start, passingCases(3))
	byCase.Selection = &Selection{CaseIDs: []string{"case-01", "case-02", "case-03"}}
	byTag := fullRun("by-tag", start, passingCases(3))
	byTag.Selection = &Selection{Tags: []string{"dates"}}
	legacyFull := storedRun("legacy-45", "", "x", start, passingCases(45))
	legacyPart := storedRun("legacy-13", "", "x", start, passingCases(13))
	variant := fullRun("variant", start, passingCases(3))
	variant.Variant = &VariantInfo{Name: "tighter", SHA256: strings.Repeat("a", 64)}
	replayed := fullRun("replayed", start, passingCases(3))
	replayed.Replayed, replayed.ReplayOf = true, "full"
	stopped := fullRun("stopped", start, passingCases(3))
	stopped.Stopped = StopQuota
	importAll(t, store, fullRun("full", start, passingCases(3)), byCase, byTag, legacyFull, legacyPart, variant, replayed, stopped)

	got := map[string][2]int{}
	for _, run := range boardRuns(t, store) {
		got[run.RunID] = [2]int{run.FullSuite, run.Regression}
	}
	want := map[string][2]int{
		"full": {1, 1}, "by-case": {0, 0}, "by-tag": {0, 0}, "legacy-45": {1, 1}, "legacy-13": {0, 0},
		"variant": {1, 0}, "replayed": {1, 0}, "stopped": {1, 0},
	}
	for id, flags := range want {
		if got[id] != flags {
			t.Errorf("%s: full_suite, regression = %v, want %v", id, got[id], flags)
		}
	}
	var regressionCases int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM eval_case_rates WHERE regression = 1`).Scan(&regressionCases); err != nil || regressionCases != 3+45 {
		t.Fatalf("regression case rows = %d, %v; want 48", regressionCases, err)
	}
}

// previousRuns is the runs table as the store created it before full_suite existed.
const previousRuns = `
CREATE TABLE runs (
	run_id TEXT PRIMARY KEY,
	started_at TEXT NOT NULL,
	finished_at TEXT NOT NULL,
	commit_name TEXT NOT NULL,
	pi_version TEXT NOT NULL,
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	effort TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	variant TEXT NOT NULL,
	variant_sha256 TEXT NOT NULL,
	replayed INTEGER NOT NULL,
	replay_of TEXT NOT NULL,
	stopped TEXT NOT NULL,
	model_calls INTEGER NOT NULL,
	tokens_input INTEGER NOT NULL,
	tokens_output INTEGER NOT NULL,
	tokens_cache_read INTEGER NOT NULL,
	tokens_cache_write INTEGER NOT NULL,
	trials_ran INTEGER NOT NULL,
	trials_passed INTEGER NOT NULL,
	pass_k REAL NOT NULL,
	k INTEGER NOT NULL,
	happy_ran INTEGER NOT NULL,
	happy_passed INTEGER NOT NULL,
	dir TEXT NOT NULL
);`

func TestAStoreOfThePreviousSchemaGainsFullSuiteAndKeepsItsRowsAndVerdicts(t *testing.T) {
	t.Parallel()
	results := t.TempDir()
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	reviewedAt := start.Add(time.Hour)
	runs := []*Result{storedRun("20260301-090000", "", "x", start, passingCases(45)), storedRun("20260302-090000", "", "x", start.Add(24*time.Hour), passingCases(13))}

	old, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(results, ResultsStoreFile))+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(previousRuns); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(resultsSchema); err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		r.Dir = filepath.Join(results, r.RunID)
		s := r.Summary
		if _, err := old.Exec(`INSERT INTO runs VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.RunID, r.StartedAt.Format(time.RFC3339), r.FinishedAt.Format(time.RFC3339), r.Commit, r.PiVersion, r.Provider,
			r.Model, r.Effort, r.PromptFingerprint, "", "", 0, "", "", r.ModelCalls, 0, 0, 0, 0,
			s.Overall.Ran, s.Overall.Passed, s.PassK, s.K, s.Happy.Ran, s.Happy.Passed, r.Dir); err != nil {
			t.Fatal(err)
		}
		for _, c := range r.Cases {
			if _, err := old.Exec(`INSERT INTO trials VALUES (?,?,1,?,0,'passed',0,'m','low','',2,100,10,0,0,0,'')`, r.RunID, c.ID, c.Kind); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := old.Exec(`INSERT INTO verdicts VALUES (?,'case-01',1,?,'fine',?)`, r.RunID, VerdictGood, reviewedAt.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		if err := r.WriteReport(); err != nil {
			t.Fatal(err)
		}
		if err := saveReview(r.Dir, &ReviewFile{RunID: r.RunID, Reviews: []ReviewEntry{{Case: "case-01", Trial: 1, Verdict: VerdictGood, Note: "fine", ReviewedAt: reviewedAt}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenResultsStore(results)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	check := func(when string) {
		t.Helper()
		got := boardRuns(t, store)
		if len(got) != 2 || got[0].FullSuite != 1 || got[0].Regression != 1 || got[0].Cases != 45 || got[1].FullSuite != 0 || got[1].Cases != 13 {
			t.Fatalf("%s: eval_runs = %+v", when, got)
		}
		var verdicts int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM verdicts WHERE verdict = ? AND note = 'fine'`, VerdictGood).Scan(&verdicts); err != nil || verdicts != 2 {
			t.Fatalf("%s: verdicts = %d, %v", when, verdicts, err)
		}
	}
	check("opened")

	ids, err := store.ImportAll(t.Context(), results)
	if err != nil || len(ids) != 2 {
		t.Fatalf("import all = %v, %v", ids, err)
	}
	check("imported again")
}

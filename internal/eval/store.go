package eval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ResultsStoreFile is the local results database beside the run directories.
const ResultsStoreFile = "results.sqlite"

const resultsSchema = `
CREATE TABLE IF NOT EXISTS runs (
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
	dir TEXT NOT NULL,
	full_suite INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS trials (
	run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
	case_id TEXT NOT NULL,
	trial INTEGER NOT NULL,
	kind TEXT NOT NULL,
	happy INTEGER NOT NULL,
	status TEXT NOT NULL,
	typesafe INTEGER NOT NULL,
	model TEXT NOT NULL,
	effort TEXT NOT NULL,
	reported_model TEXT NOT NULL,
	model_calls INTEGER NOT NULL,
	tokens_input INTEGER NOT NULL,
	tokens_output INTEGER NOT NULL,
	tokens_cache_read INTEGER NOT NULL,
	tokens_cache_write INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	error TEXT NOT NULL,
	PRIMARY KEY (run_id, case_id, trial)
);
CREATE TABLE IF NOT EXISTS checks (
	run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
	case_id TEXT NOT NULL,
	trial INTEGER NOT NULL,
	step INTEGER NOT NULL,
	check_name TEXT NOT NULL,
	message TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS verdicts (
	run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
	case_id TEXT NOT NULL,
	trial INTEGER NOT NULL,
	verdict TEXT NOT NULL,
	note TEXT NOT NULL,
	reviewed_at TEXT NOT NULL,
	PRIMARY KEY (run_id, case_id, trial)
);
`

// resultsViews are the Grafana board's contract, so the tables can change without breaking it.
// They are replaced on every open and expose no file path. A trial ran unless it was aborted and
// passed when its status is passed, as in Summarize.
const resultsViews = `
DROP VIEW IF EXISTS eval_failed_checks;
DROP VIEW IF EXISTS eval_case_rates;
DROP VIEW IF EXISTS eval_runs;
CREATE VIEW eval_runs AS
SELECT r.run_id,
       CAST(strftime('%s', r.started_at) AS INTEGER) AS time,
       r.started_at,
       r.commit_name,
       r.model,
       r.effort,
       substr(r.fingerprint, 1, 12) AS fingerprint,
       r.pi_version,
       r.variant,
       (SELECT COUNT(DISTINCT t.case_id) FROM trials t WHERE t.run_id = r.run_id) AS cases,
       r.trials_ran,
       r.trials_passed,
       CASE WHEN r.trials_ran > 0 THEN CAST(r.trials_passed AS REAL) / r.trials_ran END AS pass_rate,
       r.pass_k,
       r.k,
       CASE WHEN r.happy_ran > 0 THEN CAST(r.happy_passed AS REAL) / r.happy_ran END AS happy_rate,
       CASE WHEN r.trials_ran > 0 THEN CAST(r.tokens_input + r.tokens_output AS REAL) / r.trials_ran END AS tokens_per_trial,
       CASE WHEN r.trials_ran > 0 THEN CAST(r.model_calls AS REAL) / r.trials_ran END AS model_calls_per_trial,
       r.full_suite,
       CASE WHEN r.full_suite = 1 AND r.variant = '' AND r.replayed = 0 AND r.stopped = '' THEN 1 ELSE 0 END AS regression
FROM runs r;
CREATE VIEW eval_case_rates AS
SELECT t.run_id,
       r.time,
       r.regression,
       t.case_id,
       t.kind,
       t.happy,
       COUNT(*) AS trials,
       SUM(t.status = 'passed') AS passed,
       CAST(SUM(t.status = 'passed') AS REAL) / COUNT(*) AS pass_rate
FROM trials t JOIN eval_runs r ON r.run_id = t.run_id
WHERE t.status != 'aborted'
GROUP BY t.run_id, t.case_id, t.kind, t.happy;
CREATE VIEW eval_failed_checks AS
SELECT c.run_id,
       r.time,
       c.case_id,
       c.trial,
       c.step,
       c.check_name,
       c.message
FROM checks c JOIN eval_runs r ON r.run_id = c.run_id;
`

// ResultsStore is the local SQLite file of imported runs, their trials, failed checks and the
// operator's verdicts. The run directories stay the source of truth; the store makes trends and
// comparisons a query.
type ResultsStore struct {
	db *sql.DB
}

// OpenResultsStore opens or creates the store in the results directory.
func OpenResultsStore(resultsDir string) (*ResultsStore, error) {
	if err := os.MkdirAll(resultsDir, 0o700); err != nil {
		return nil, fmt.Errorf("open the results store: %w", err)
	}
	path := filepath.Join(resultsDir, ResultsStoreFile)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open the results store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(resultsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create the results schema: %w", err)
	}
	if err := addFullSuite(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("add the full-suite column: %w", err)
	}
	if err := replaceViews(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create the results views: %w", err)
	}
	return &ResultsStore{db: db}, nil
}

// replaceViews recreates the views in one transaction, so a snapshot taken by another process
// while a command opens the store never lacks one.
func replaceViews(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(resultsViews); err != nil {
		return err
	}
	return tx.Commit()
}

// addFullSuite adds runs.full_suite to a store created before it existed, deciding each stored
// run by the rule Result.FullSuite applies to a result without a selection.
func addFullSuite(db *sql.DB) error {
	var present int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'full_suite'`).Scan(&present); err != nil {
		return err
	}
	if present > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE runs ADD COLUMN full_suite INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE runs SET full_suite = (SELECT COUNT(DISTINCT t.case_id) FROM trials t WHERE t.run_id = runs.run_id) >= ?`,
		legacyFullSuiteCases); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *ResultsStore) Close() error { return s.db.Close() }

func flag(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Import stores a run and its review, replacing what the store held for the same run ID, so
// importing again after a review picks up the new verdicts.
func (s *ResultsStore) Import(ctx context.Context, r *Result, review *ReviewFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE run_id = ?`, r.RunID); err != nil {
		return err
	}
	summary := r.Summary
	variant, variantSHA := "", ""
	if r.Variant != nil {
		variant, variantSHA = r.Variant.Name, r.Variant.SHA256
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO runs (run_id, started_at, finished_at, commit_name, pi_version, provider,
	model, effort, fingerprint, variant, variant_sha256, replayed, replay_of, stopped,
	model_calls, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write,
	trials_ran, trials_passed, pass_k, k, happy_ran, happy_passed, dir, full_suite)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.RunID, r.StartedAt.UTC().Format(time.RFC3339), r.FinishedAt.UTC().Format(time.RFC3339), r.Commit, r.PiVersion, r.Provider,
		r.Model, r.Effort, r.PromptFingerprint, variant, variantSHA, flag(r.Replayed), r.ReplayOf, r.Stopped,
		r.ModelCalls, r.Tokens.Input, r.Tokens.Output, r.Tokens.CacheRead, r.Tokens.CacheWrite,
		summary.Overall.Ran, summary.Overall.Passed, summary.PassK, summary.K, summary.Happy.Ran, summary.Happy.Passed, r.Dir, flag(r.FullSuite())); err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	for _, c := range r.Cases {
		for _, t := range c.Trials {
			if _, err := tx.ExecContext(ctx, `INSERT INTO trials VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				r.RunID, c.ID, t.N, c.Kind, flag(c.Happy()), t.Status, flag(t.TypeSafe), t.Model, t.Effort, t.ReportedModel,
				t.ModelCalls, t.Tokens.Input, t.Tokens.Output, t.Tokens.CacheRead, t.Tokens.CacheWrite, t.DurationMS, t.Error); err != nil {
				return fmt.Errorf("insert trial %s#%d: %w", c.ID, t.N, err)
			}
			for _, f := range t.Failures {
				if _, err := tx.ExecContext(ctx, `INSERT INTO checks VALUES (?,?,?,?,?,?)`, r.RunID, c.ID, t.N, f.Step, f.Check, f.Message); err != nil {
					return fmt.Errorf("insert check: %w", err)
				}
			}
		}
	}
	if review != nil {
		for _, e := range review.Reviews {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO verdicts VALUES (?,?,?,?,?,?)`,
				r.RunID, e.Case, e.Trial, e.Verdict, e.Note, e.ReviewedAt.UTC().Format(time.RFC3339)); err != nil {
				return fmt.Errorf("insert verdict: %w", err)
			}
		}
	}
	return tx.Commit()
}

// ImportRunDir loads a run directory's result.json and review.json into the store.
func (s *ResultsStore) ImportRunDir(ctx context.Context, dir string) (*Result, error) {
	result, err := LoadResult(dir)
	if err != nil {
		return nil, err
	}
	review, _, err := LoadReview(dir)
	if err != nil {
		return nil, err
	}
	return result, s.Import(ctx, result, review)
}

// ImportAll imports every run directory under the results directory and returns their IDs.
func (s *ResultsStore) ImportAll(ctx context.Context, resultsDir string) ([]string, error) {
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return nil, fmt.Errorf("import runs: %w", err)
	}
	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(resultsDir, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, resultFile)); err != nil {
			continue
		}
		if _, err := s.ImportRunDir(ctx, dir); err != nil {
			return ids, fmt.Errorf("import %s: %w", entry.Name(), err)
		}
		ids = append(ids, entry.Name())
	}
	return ids, nil
}

// ImportRun imports one run directory into the results store beside it; the CLI calls it after a
// run, a review and a replay.
func ImportRun(ctx context.Context, resultsDir, runID string) error {
	store, err := OpenResultsStore(resultsDir)
	if err != nil {
		return err
	}
	defer store.Close()
	_, err = store.ImportRunDir(ctx, filepath.Join(resultsDir, runID))
	return err
}

// StoredRun is a run as the store lists it for the trend.
type StoredRun struct {
	RunID       string
	StartedAt   time.Time
	Commit      string
	PiVersion   string
	Model       string
	Effort      string
	Fingerprint string
	Variant     string
	Replayed    bool
	ReplayOf    string
	Stopped     string
	Overall     Rate
	PassK       float64
	K           int
	Happy       Rate
	// Reviewed counts the operator's verdicts on the run's trials.
	Reviewed int
}

// Runs lists the stored runs by start time.
func (s *ResultsStore) Runs(ctx context.Context) ([]StoredRun, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT r.run_id, r.started_at, r.commit_name, r.pi_version, r.model, r.effort, r.fingerprint, r.variant, r.replayed, r.replay_of,
       r.stopped, r.trials_passed, r.trials_ran, r.pass_k, r.k, r.happy_passed, r.happy_ran,
       (SELECT COUNT(*) FROM verdicts v WHERE v.run_id = r.run_id)
FROM runs r ORDER BY r.started_at, r.run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []StoredRun
	for rows.Next() {
		var run StoredRun
		var started string
		var replayed int
		if err := rows.Scan(&run.RunID, &started, &run.Commit, &run.PiVersion, &run.Model, &run.Effort, &run.Fingerprint, &run.Variant, &replayed, &run.ReplayOf,
			&run.Stopped, &run.Overall.Passed, &run.Overall.Ran, &run.PassK, &run.K, &run.Happy.Passed, &run.Happy.Ran, &run.Reviewed); err != nil {
			return nil, err
		}
		run.StartedAt, _ = time.Parse(time.RFC3339, started)
		run.Replayed = replayed == 1
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// CaseRate is one case's trials in one run.
type CaseRate struct {
	ID     string
	Kind   string
	Happy  bool
	Ran    int
	Passed int
	// ModelCalls, Input and Output add up the trials that ran.
	ModelCalls int64
	Input      int64
	Output     int64
	// Model is the model the case's trials ran on: its own user.model, or else the run's.
	Model string
}

// Rate is the case's pass rate over the trials that ran.
func (c CaseRate) Rate() float64 { return float64(c.Passed) / float64(c.Ran) }

// RunDetail is what a comparison needs of one run.
type RunDetail struct {
	Run   StoredRun
	Cases map[string]CaseRate
}

// ErrNoRun is returned for a run ID the store does not hold.
var ErrNoRun = errors.New("no such run in the results store")

// Detail reads one run's per-case rates and costs.
func (s *ResultsStore) Detail(ctx context.Context, runID string) (*RunDetail, error) {
	runs, err := s.Runs(ctx)
	if err != nil {
		return nil, err
	}
	var detail RunDetail
	found := false
	for _, run := range runs {
		if run.RunID == runID {
			detail.Run, found = run, true
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrNoRun, runID)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT case_id, kind, happy, COUNT(*), SUM(status = 'passed'), SUM(model_calls), SUM(tokens_input), SUM(tokens_output), MAX(model)
FROM trials WHERE run_id = ? AND status != 'aborted' GROUP BY case_id, kind, happy ORDER BY case_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	detail.Cases = map[string]CaseRate{}
	for rows.Next() {
		var c CaseRate
		var happy int
		if err := rows.Scan(&c.ID, &c.Kind, &happy, &c.Ran, &c.Passed, &c.ModelCalls, &c.Input, &c.Output, &c.Model); err != nil {
			return nil, err
		}
		c.Happy = happy == 1
		detail.Cases[c.ID] = c
	}
	return &detail, rows.Err()
}

// ResolveRun returns the run ID the name stands for: an exact ID, or "latest".
func (s *ResultsStore) ResolveRun(ctx context.Context, name string) (string, error) {
	runs, err := s.Runs(ctx)
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return "", errors.New("the results store is empty; import runs first")
	}
	if name == "" || name == "latest" {
		return runs[len(runs)-1].RunID, nil
	}
	for _, run := range runs {
		if run.RunID == name {
			return name, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNoRun, strings.TrimSpace(name))
}

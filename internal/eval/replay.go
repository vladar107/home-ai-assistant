package eval

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReplayOptions says which run to re-score and against which cases.
type ReplayOptions struct {
	ResultsDir string
	// SourceRunID is the run whose cassettes are replayed.
	SourceRunID string
	CasesDir    string
	Registry    *Registry
	Now         func() time.Time
}

// Replay re-scores a run's cassettes with the cases' current checks and writes the outcome as a
// new run marked replayed and linked to its source. It starts no model and no Pi: a trial is only
// as good as its recorded turn, so a replay tries a new or fixed check, never a prompt. Store
// checks need the world's store, which a cassette does not hold, so a trial keeps the store
// verdicts it had when the case still expects them.
func Replay(ctx context.Context, opts ReplayOptions) (*Result, error) {
	resultsDir := cmp.Or(opts.ResultsDir, DefaultResultsDir())
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	source, err := LoadResult(filepath.Join(resultsDir, opts.SourceRunID))
	if err != nil {
		return nil, err
	}
	cases, err := cmp.Or(opts.Registry, DefaultRegistry).LoadCases(opts.CasesDir)
	if err != nil {
		return nil, err
	}
	byID := map[string]Case{}
	for _, c := range cases {
		byID[c.ID] = c
	}

	started := now()
	replayed := *source
	replayed.RunID = newRunID(resultsDir, started)
	replayed.Dir = filepath.Join(resultsDir, replayed.RunID)
	replayed.StartedAt, replayed.FinishedAt = started, started
	replayed.Replayed, replayed.ReplayOf = true, source.RunID
	replayed.RunLog, replayed.Review, replayed.ReplayChanges = "", nil, nil
	replayed.Cases = nil
	if err := os.MkdirAll(filepath.Join(replayed.Dir, cassetteDir), 0o700); err != nil {
		return nil, fmt.Errorf("create the results directory: %w", err)
	}

	for _, sourceCase := range source.Cases {
		out := sourceCase
		out.Trials = nil
		for _, trial := range sourceCase.Trials {
			rescored := replayTrial(ctx, source.Dir, replayed.Dir, byID[sourceCase.ID], sourceCase.ID, trial)
			if rescored.Status != trial.Status {
				replayed.ReplayChanges = append(replayed.ReplayChanges,
					fmt.Sprintf("%s: %s -> %s", trialKey(sourceCase.ID, trial.N), trial.Status, rescored.Status))
			}
			out.Trials = append(out.Trials, rescored)
		}
		replayed.Cases = append(replayed.Cases, out)
	}
	replayed.Summarize()

	review, found, err := LoadReview(source.Dir)
	if err != nil {
		return nil, err
	}
	if found {
		review.RunID = replayed.RunID
		if err := saveReview(replayed.Dir, review); err != nil {
			return nil, err
		}
	}
	if err := replayed.WriteReport(); err != nil {
		return &replayed, err
	}
	return &replayed, nil
}

// newRunID is the timestamp run IDs use, made unique among the results directory's runs.
func newRunID(resultsDir string, at time.Time) string {
	id := at.UTC().Format("20060102-150405")
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(resultsDir, id)); errors.Is(err, os.ErrNotExist) {
			return id
		}
		id = fmt.Sprintf("%s-%d", at.UTC().Format("20060102-150405"), n)
	}
}

// replayTrial scores one trial's cassette against the case's current expectations. A trial that
// never produced a verdict, or whose cassette cannot be matched to the case, keeps or reports its
// error.
func replayTrial(ctx context.Context, sourceDir, targetDir string, c Case, caseID string, trial TrialResult) TrialResult {
	if trial.Status == TrialError || trial.Status == TrialAborted || trial.Cassette == "" {
		return trial
	}
	fail := func(format string, args ...any) TrialResult {
		trial.Status, trial.Error, trial.Failures = TrialError, fmt.Sprintf(format, args...), nil
		return trial
	}
	if c.ID == "" {
		return fail("replay: case %s no longer exists", caseID)
	}
	contents, err := os.ReadFile(filepath.Join(sourceDir, trial.Cassette))
	if err != nil {
		return fail("replay: read the cassette: %v", err)
	}
	var cassette Cassette
	if err := json.Unmarshal(contents, &cassette); err != nil {
		return fail("replay: decode the cassette: %v", err)
	}
	if len(cassette.Steps) != len(c.Steps) {
		return fail("replay: case %s has %d step(s) now and its cassette %d", caseID, len(c.Steps), len(cassette.Steps))
	}
	start, err := c.Clock.Time()
	if err != nil {
		return fail("replay: %v", err)
	}

	trial.Failures, trial.Error = nil, ""
	for i, step := range c.Steps {
		recorded := &cassette.Steps[i]
		expect := step.Expect
		if expect != nil {
			withoutStore := *expect
			withoutStore.Store = nil
			expect = &withoutStore
		}
		failures := CheckStep(ctx, expect, turnRecord(*recorded), CheckEnv{Location: start.Location()})
		if step.Expect != nil && len(step.Expect.Store) > 0 {
			for _, old := range recorded.Failures {
				if strings.HasPrefix(old.Check, "store") {
					failures = append(failures, old)
				}
			}
		}
		for j := range failures {
			failures[j].Step = i + 1
		}
		recorded.Failures = failures
		trial.Failures = append(trial.Failures, failures...)
	}
	trial.Status = TrialPassed
	if len(trial.Failures) > 0 {
		trial.Status = TrialFailed
	}
	cassette.Failures, cassette.Status, cassette.Error, cassette.RunID = trial.Failures, trial.Status, "", filepath.Base(targetDir)
	encoded, err := json.MarshalIndent(cassette, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(targetDir, trial.Cassette), append(encoded, '\n'), 0o600)
	}
	if err != nil {
		return fail("replay: write the cassette: %v", err)
	}
	return trial
}

// turnRecord is the part of a cassette step the checks read.
func turnRecord(step CassetteStep) TurnRecord {
	return TurnRecord{
		Kind: step.Kind, Message: step.Message, Reply: step.Reply, Quiet: step.Quiet, Proposals: step.Proposals,
		Resolved: step.Resolved, Reconsider: step.Reconsider, ToolCalls: step.ToolCalls, Usage: step.Usage,
		Tokens: step.Tokens, Model: step.Model,
	}
}

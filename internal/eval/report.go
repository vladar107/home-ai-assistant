package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	TrialPassed  = "passed"
	TrialFailed  = "failed"
	TrialError   = "error"
	TrialAborted = "aborted"

	StopBudget   = "budget"
	StopQuota    = "quota"
	StopAuth     = "auth"
	StopCanceled = "canceled"

	resultFile  = "result.json"
	reportFile  = "report.md"
	cassetteDir = "cassettes"
)

// TrialResult is one trial of a case: its verdict, the checks that failed and what it cost.
type TrialResult struct {
	N      int    `json:"n"`
	Status string `json:"status"`
	// TypeSafe is true when the trial ran with the safety checks and the tool hint on.
	TypeSafe bool   `json:"typesafe"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	// ReportedModel is the model ID the provider said answered.
	ReportedModel string                   `json:"reported_model,omitempty"`
	Failures      []Failure                `json:"failures,omitempty"`
	Error         string                   `json:"error,omitempty"`
	ModelCalls    int                      `json:"model_calls"`
	Tokens        agentruntime.TokenCounts `json:"tokens"`
	DurationMS    int64                    `json:"duration_ms"`
	Cassette      string                   `json:"cassette,omitempty"`
}

// Ran reports whether the trial counts toward the rates.
func (t TrialResult) Ran() bool { return t.Status != TrialAborted }

// CaseResult is every trial of one case.
type CaseResult struct {
	ID     string        `json:"id"`
	Title  string        `json:"title"`
	Kind   string        `json:"kind"`
	Tags   []string      `json:"tags,omitempty"`
	Trials []TrialResult `json:"trials"`
}

// Happy reports whether the case guards a flow that works today.
func (c CaseResult) Happy() bool { return c.Kind == KindHappy || slices.Contains(c.Tags, KindHappy) }

func (c CaseResult) counts() (ran, passed int) {
	for _, trial := range c.Trials {
		if trial.Ran() {
			ran++
		}
		if trial.Status == TrialPassed {
			passed++
		}
	}
	return ran, passed
}

// NotRun names the trials of a case the run never started.
type NotRun struct {
	Case   string `json:"case"`
	Trials int    `json:"trials"`
}

// Rate is passed trials out of ran trials.
type Rate struct {
	Passed int `json:"passed"`
	Ran    int `json:"ran"`
}

func (r Rate) String() string {
	if r.Ran == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(r.Passed)/float64(r.Ran), r.Passed, r.Ran)
}

// Summary is what a run's report leads with.
type Summary struct {
	Overall Rate `json:"overall"`
	// PassK is the share of cases whose every trial passed, at K trials per case.
	PassK    float64         `json:"pass_k"`
	K        int             `json:"k"`
	Happy    Rate            `json:"happy"`
	HasHappy bool            `json:"has_happy"`
	PerKind  map[string]Rate `json:"per_kind"`
	TypeSafe int             `json:"typesafe_trials"`
	NoSafety []string        `json:"without_typesafe,omitempty"`
}

// WindowUsage is a quota window as the pre-flight saw it.
type WindowUsage struct {
	Name        string  `json:"name"`
	UsedPercent float64 `json:"used_percent"`
}

// Result is a whole run, written as result.json.
type Result struct {
	RunID      string    `json:"run_id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Commit     string    `json:"commit"`
	PiVersion  string    `json:"pi_version"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Effort     string    `json:"effort"`
	// ReportedModels are the model IDs the provider reported answering.
	ReportedModels []string `json:"reported_models,omitempty"`
	// PromptFingerprint is the SHA-256 of the sorted distinct system prompts of the run, each
	// rendered without memory and jobs; PromptFingerprints lists those prompts' own hashes.
	PromptFingerprint  string   `json:"prompt_fingerprint,omitempty"`
	PromptFingerprints []string `json:"prompt_fingerprints,omitempty"`
	Replayed           bool     `json:"replayed,omitempty"`
	// Variant names the prompt variant the run tried; nil is production's prompt.
	Variant     *VariantInfo             `json:"variant,omitempty"`
	Budget      int                      `json:"budget"`
	ModelCalls  int                      `json:"model_calls"`
	Tokens      agentruntime.TokenCounts `json:"tokens"`
	QuotaBefore []WindowUsage            `json:"quota_before,omitempty"`
	// RunLog names the file of warn-and-above runtime records beside the report; empty when none.
	RunLog        string       `json:"run_log,omitempty"`
	Stopped       string       `json:"stopped,omitempty"`
	StoppedDetail string       `json:"stopped_detail,omitempty"`
	NotRun        []NotRun     `json:"not_run,omitempty"`
	Cases         []CaseResult `json:"cases"`
	Summary       Summary      `json:"summary"`
	// Dir is where the run's files are; it is not part of the file.
	Dir string `json:"-"`
	// Review is the operator's review of the run, when there is one; it lives in review.json.
	Review *ReviewFile `json:"-"`
}

// Summarize computes the run's rates from its cases.
func (r *Result) Summarize() {
	summary := Summary{PerKind: map[string]Rate{}}
	var caseRan, caseAll int
	for _, c := range r.Cases {
		ran, passed := c.counts()
		summary.Overall.Ran += ran
		summary.Overall.Passed += passed
		kind := summary.PerKind[c.Kind]
		kind.Ran, kind.Passed = kind.Ran+ran, kind.Passed+passed
		summary.PerKind[c.Kind] = kind
		if c.Happy() {
			summary.HasHappy = true
			summary.Happy.Ran += ran
			summary.Happy.Passed += passed
		}
		if ran > 0 {
			caseRan++
			summary.K = max(summary.K, ran)
			if ran == passed {
				caseAll++
			}
		}
		for _, trial := range c.Trials {
			switch {
			case !trial.Ran():
			case trial.TypeSafe:
				summary.TypeSafe++
			default:
				summary.NoSafety = append(summary.NoSafety, fmt.Sprintf("%s#%d", c.ID, trial.N))
			}
		}
	}
	if caseRan > 0 {
		summary.PassK = float64(caseAll) / float64(caseRan)
	}
	r.Summary = summary
}

// WriteReport writes result.json and report.md into the run's directory.
func (r *Result) WriteReport() error {
	if r.Dir == "" {
		return fmt.Errorf("write report: the run has no directory")
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	if err := os.WriteFile(filepath.Join(r.Dir, resultFile), append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	if review, found, err := LoadReview(r.Dir); err != nil {
		return err
	} else if found {
		r.Review = review
	}
	if err := os.WriteFile(filepath.Join(r.Dir, reportFile), []byte(r.Markdown()), 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

// Markdown renders the report.
func (r *Result) Markdown() string {
	var b strings.Builder
	s := r.Summary
	fmt.Fprintf(&b, "# Eval run %s\n\n", r.RunID)
	if r.Stopped != "" {
		fmt.Fprintf(&b, "**Stopped early: %s.** %s\n\n", r.Stopped, r.StoppedDetail)
	}
	if r.Replayed {
		b.WriteString("This run was replayed from cassettes; no model ran.\n\n")
	}
	fmt.Fprintf(&b, "- Pass rate: %s\n", s.Overall)
	if s.K > 0 {
		fmt.Fprintf(&b, "- pass^k (k=%d, every trial of a case passes): %.1f%%\n", s.K, 100*s.PassK)
	}
	if s.HasHappy {
		fmt.Fprintf(&b, "- Happy-path pass rate: %s\n", s.Happy)
	} else {
		b.WriteString("- Happy-path pass rate: no happy-path cases ran\n")
	}
	fmt.Fprintf(&b, "- Model calls: %d of a budget of %d\n", r.ModelCalls, r.Budget)
	fmt.Fprintf(&b, "- Tokens: %d input, %d output, %d cache read, %d cache write\n",
		r.Tokens.Input, r.Tokens.Output, r.Tokens.CacheRead, r.Tokens.CacheWrite)
	fmt.Fprintf(&b, "- Pi %s, provider %s, model %s, effort %s\n", r.PiVersion, r.Provider, r.Model, r.Effort)
	if len(r.ReportedModels) > 0 {
		fmt.Fprintf(&b, "- Model IDs reported by the provider: %s\n", strings.Join(r.ReportedModels, ", "))
	}
	if r.PromptFingerprint != "" {
		fmt.Fprintf(&b, "- Prompt fingerprint: `%s` (system prompts without memory and jobs: %d)\n", r.PromptFingerprint, len(r.PromptFingerprints))
	}
	if r.Variant != nil {
		fmt.Fprintf(&b, "- Variant: `%s` (file SHA-256 `%s`): %s\n", r.Variant.Name, r.Variant.SHA256, r.Variant.Description)
	} else {
		b.WriteString("- Variant: none (production's prompt)\n")
	}
	fmt.Fprintf(&b, "- Commit: %s\n", r.Commit)
	if r.RunLog != "" {
		fmt.Fprintf(&b, "- Runtime warnings and errors were logged to `%s` beside this report\n", r.RunLog)
	}
	fmt.Fprintf(&b, "- Started %s, finished %s\n", r.StartedAt.Format(time.RFC3339), r.FinishedAt.Format(time.RFC3339))
	for _, window := range r.QuotaBefore {
		fmt.Fprintf(&b, "- Quota before the run: %s window %.0f%% used\n", window.Name, window.UsedPercent)
	}

	b.WriteString("\n## Pass rate per failure kind\n\n| Kind | Pass rate |\n| --- | --- |\n")
	kinds := make([]string, 0, len(s.PerKind))
	for kind := range s.PerKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(&b, "| %s | %s |\n", kind, s.PerKind[kind])
	}

	b.WriteString("\n## Safety checks (TypeSafe)\n\n")
	fmt.Fprintf(&b, "%d trial(s) ran with the safety checks and the tool hint on.", s.TypeSafe)
	if len(s.NoSafety) > 0 {
		fmt.Fprintf(&b, " Off in: %s.", strings.Join(s.NoSafety, ", "))
	}
	b.WriteString("\n\n## Cases\n\n| Case | Kind | Trials | Passed |\n| --- | --- | --- | --- |\n")
	for _, c := range r.Cases {
		ran, passed := c.counts()
		fmt.Fprintf(&b, "| %s | %s | %d | %d |\n", c.ID, c.Kind, ran, passed)
	}

	var failing bool
	for _, c := range r.Cases {
		for _, trial := range c.Trials {
			if trial.Status == TrialPassed {
				continue
			}
			if !failing {
				b.WriteString("\n## Failing trials\n")
				failing = true
			}
			fmt.Fprintf(&b, "\n### %s, trial %d: %s\n\n", c.ID, trial.N, trial.Status)
			if trial.Error != "" {
				fmt.Fprintf(&b, "- error: %s\n", trial.Error)
			}
			for _, failure := range trial.Failures {
				fmt.Fprintf(&b, "- step %d, %s: %s\n", failure.Step, failure.Check, failure.Message)
			}
			if trial.Cassette != "" {
				fmt.Fprintf(&b, "- cassette: `%s`\n", trial.Cassette)
			}
		}
	}
	if len(r.NotRun) > 0 {
		b.WriteString("\n## Not run\n\n")
		for _, skipped := range r.NotRun {
			fmt.Fprintf(&b, "- %s: %d trial(s)\n", skipped.Case, skipped.Trials)
		}
	}
	if r.Review != nil {
		r.reviewMarkdown(&b)
	}
	return b.String()
}

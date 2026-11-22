package eval

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
)

const (
	// DefaultBudget is the number of model calls a run may spend unless the command line raises it.
	DefaultBudget = 300
	// DefaultPiProvider is the model provider production runs Pi with.
	DefaultPiProvider = "openai-codex"

	maxErrorRunes = 300
)

// DefaultResultsDir is where runs are written: ~/eval-results, outside the checkout, so cleaning
// or recloning the repository cannot delete them.
func DefaultResultsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "eval-results"
	}
	return filepath.Join(home, "eval-results")
}

// PiOptions says how the runner starts Pi. The zero value is production's: the `pi` binary, the
// pinned release and provider, and no web search.
type PiOptions struct {
	Binary     string
	NodeBinary string
	Provider   string
	// ExpectedVersion is the release Pi must report; empty means config.DefaultPiVersion.
	ExpectedVersion string
	// WebSearchExtension is the entry file of the vendored search extension, as production
	// configures it; empty runs without web search.
	WebSearchExtension string
}

// RunOptions is everything a run needs from its caller.
type RunOptions struct {
	// Profiles is the prepared eval profiles root. PrepareProfiles refuses the application's data
	// directory, and the runner refuses a zero value, so there is no default root.
	Profiles Profiles
	CasesDir string
	// CaseIDs and Tags select cases; both empty runs them all, and a case matches when it has any
	// of the tags.
	CaseIDs []string
	Tags    []string
	// Trials replaces every case's own trial count when positive.
	Trials int
	// Budget is the most model calls the run may start trials for; zero means DefaultBudget.
	Budget int
	// MaxUsedPercent is the quota the pre-flight tolerates already used; zero means the default.
	MaxUsedPercent float64
	// ScanEnvPaths and ScanDirs feed the privacy scan that runs first.
	ScanEnvPaths []string
	ScanDirs     []string
	ResultsDir   string
	Endpoints    Endpoints
	Pi           PiOptions
	// ChatModel is the default model and effort of a case that pins none; it is required.
	ChatModel, TaskModel, HeadsUpModel agentruntime.ModelEffort
	TypeSafe                           judgment.Config
	StepTimeout, TurnTimeout           time.Duration
	// Commit names the code under test, such as `git describe` output.
	Commit string
	Now    func() time.Time
	// Out receives one progress line per trial; nil discards them.
	Out io.Writer
	// Wrap lets a caller put a layer around the Pi runtime, such as a test's scripted model.
	Wrap     func(*pi.Runtime) agentruntime.AgentRuntime
	Registry *Registry
	// Variant is the prompt variant to try; nil runs production's prompt.
	Variant *Variant
}

type runner struct {
	opts      RunOptions
	runtime   agentruntime.AgentRuntime
	accountID string
	result    *Result
	prints    map[string]struct{}
	reported  map[string]struct{}
	log       *warnLog
}

type planned struct {
	c      Case
	trials int
}

// Run plays the selected cases on the eval profile and writes the run's result, report and
// cassettes. It refuses before any model call when the profiles root is unset, the privacy scan
// finds a household value, Pi is not the pinned release, or the quota is too high. A run that stops
// on its budget, a quota or an authentication failure still returns its result, with Stopped set.
func Run(ctx context.Context, opts RunOptions) (*Result, error) {
	if opts.Profiles.Root == "" || opts.Profiles.Dir == "" || opts.Profiles.StorePath == "" {
		return nil, errors.New("a run needs the eval profiles root (--profiles)")
	}
	if opts.ChatModel.Model == "" {
		return nil, errors.New("a run needs a chat model (HOME_AGENT_PI_MODEL or --model)")
	}
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	if opts.MaxUsedPercent <= 0 {
		opts.MaxUsedPercent = DefaultMaxUsedPercent
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	registry := cmp.Or(opts.Registry, DefaultRegistry)
	if v := opts.Variant; v != nil {
		opts.ChatModel.Model = cmp.Or(v.Model, opts.ChatModel.Model)
		opts.ChatModel.Effort = cmp.Or(v.Effort, opts.ChatModel.Effort)
		// A zero task or Heads-up model already follows the chat model.
		for _, scheduled := range []*agentruntime.ModelEffort{&opts.TaskModel, &opts.HeadsUpModel} {
			if v.Model != "" && scheduled.Model != "" {
				scheduled.Model = v.Model
				scheduled.Effort = cmp.Or(v.Effort, scheduled.Effort)
			}
		}
	}

	cases, err := registry.LoadCases(opts.CasesDir)
	if err != nil {
		return nil, err
	}
	plan, err := selectCases(cases, opts)
	if err != nil {
		return nil, err
	}

	findings, found, err := Scan(opts.ScanEnvPaths, opts.ScanDirs)
	if err != nil {
		return nil, err
	}
	if len(findings) > 0 {
		return nil, fmt.Errorf("the privacy scan found household values in %d field(s), first %s %s; run the scan command",
			len(findings), findings[0].File, findings[0].Field)
	}
	if !found {
		fmt.Fprintln(opts.Out, "privacy scan: no env file found, nothing was checked")
	}

	store, err := opts.Profiles.OpenStore(ctx)
	if err != nil {
		return nil, err
	}
	binding, bound, err := store.LoadModelConnection(ctx, PrincipalID)
	_ = store.Close()
	if err != nil {
		return nil, fmt.Errorf("read the eval profile's binding: %w", err)
	}
	if !bound {
		return nil, errors.New("the eval profile is not logged in: run the login command first")
	}

	piVersion := cmp.Or(opts.Pi.ExpectedVersion, config.DefaultPiVersion)
	provider := cmp.Or(opts.Pi.Provider, DefaultPiProvider)
	log := newWarnLog()
	piRuntime, err := pi.New(ctx, pi.Config{
		Binary: cmp.Or(opts.Pi.Binary, "pi"), NodeBinary: opts.Pi.NodeBinary, Provider: provider,
		Model: opts.ChatModel, ExpectedVersion: piVersion, ProfilesDir: opts.Profiles.Root,
		WebSearchExtension: opts.Pi.WebSearchExtension, Logger: log.logger(),
	})
	if err != nil {
		return nil, fmt.Errorf("start Pi %s: %w", piVersion, err)
	}

	if err := opts.validateVariantTools(ctx, plan, binding.AccountID); err != nil {
		return nil, err
	}

	windows, err := Preflight(ctx, opts.Profiles, opts.Endpoints, opts.MaxUsedPercent)
	if err != nil {
		return nil, err
	}

	started := opts.Now()
	r := &runner{
		opts: opts, accountID: binding.AccountID, prints: map[string]struct{}{}, reported: map[string]struct{}{}, log: log,
		result: &Result{
			RunID: started.UTC().Format("20060102-150405"), StartedAt: started, Commit: cmp.Or(opts.Commit, "unknown"),
			PiVersion: piVersion, Provider: provider, Model: opts.ChatModel.Model,
			Effort: cmp.Or(opts.ChatModel.Effort, agentruntime.DefaultEffort), Budget: opts.Budget,
			Variant:   opts.Variant.Info(),
			Selection: &Selection{CaseIDs: opts.CaseIDs, Tags: opts.Tags, FullSuite: len(opts.CaseIDs) == 0 && len(opts.Tags) == 0},
		},
	}
	r.runtime = piRuntime
	if opts.Wrap != nil {
		r.runtime = opts.Wrap(piRuntime)
	}
	for _, window := range windows {
		r.result.QuotaBefore = append(r.result.QuotaBefore, WindowUsage{Name: window.Name, UsedPercent: window.UsedPercent})
	}
	r.result.Dir = filepath.Join(cmp.Or(opts.ResultsDir, DefaultResultsDir()), r.result.RunID)
	if err := os.MkdirAll(filepath.Join(r.result.Dir, cassetteDir), 0o700); err != nil {
		return nil, fmt.Errorf("create the results directory: %w", err)
	}

	log.open(r.result.Dir)
	r.play(ctx, plan)
	if log.close() {
		r.result.RunLog = warnLogFile
	}

	r.result.FinishedAt = opts.Now()
	r.result.ReportedModels = sortedSet(r.reported)
	r.result.PromptFingerprints = sortedSet(r.prints)
	if len(r.result.PromptFingerprints) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(r.result.PromptFingerprints, "\n")))
		r.result.PromptFingerprint = hex.EncodeToString(sum[:])
	}
	r.result.Summarize()
	if err := r.result.WriteReport(); err != nil {
		return r.result, err
	}
	return r.result, nil
}

// validateVariantTools checks the variant's tool_guidance names against the tools the planned cases'
// worlds offer, before any model call.
func (opts RunOptions) validateVariantTools(ctx context.Context, plan []planned, accountID string) error {
	if opts.Variant == nil || len(opts.Variant.ToolGuidance) == 0 {
		return nil
	}
	var offered []string
	for _, entry := range plan {
		names, err := OfferedTools(ctx, entry.c, WorldOptions{
			ProfilesDir: opts.Profiles.Root, AccountID: accountID, AuthBaseURL: opts.Endpoints.AuthBaseURL,
			ChatGPT:   chatgpt.Config{BaseURL: opts.Endpoints.ChatGPTBaseURL, HTTPClient: opts.Endpoints.HTTPClient},
			ChatModel: opts.ChatModel, TaskModel: opts.TaskModel, HeadsUpModel: opts.HeadsUpModel,
		})
		if err != nil {
			return fmt.Errorf("list the tools of case %s: %w", entry.c.ID, err)
		}
		offered = append(offered, names...)
		if opts.Variant.ValidateTools(offered) == nil {
			return nil
		}
	}
	return opts.Variant.ValidateTools(offered)
}

func selectCases(cases []Case, opts RunOptions) ([]planned, error) {
	for _, id := range opts.CaseIDs {
		if !slices.ContainsFunc(cases, func(c Case) bool { return c.ID == id }) {
			return nil, fmt.Errorf("no case %q in %s", id, opts.CasesDir)
		}
	}
	var plan []planned
	for _, c := range cases {
		if len(opts.CaseIDs) > 0 && !slices.Contains(opts.CaseIDs, c.ID) {
			continue
		}
		if len(opts.Tags) > 0 && !slices.ContainsFunc(opts.Tags, func(tag string) bool { return slices.Contains(c.Tags, tag) }) {
			continue
		}
		plan = append(plan, planned{c: c, trials: cmp.Or(max(opts.Trials, 0), c.Trials)})
	}
	if len(plan) == 0 {
		return nil, errors.New("no case matches the selection")
	}
	return plan, nil
}

func (r *runner) play(ctx context.Context, plan []planned) {
	for i, entry := range plan {
		c := entry.c
		caseResult := CaseResult{ID: c.ID, Title: c.Title, Kind: c.Kind, Tags: c.Tags}
		for n := 1; n <= entry.trials; n++ {
			reason, detail := r.stopBefore(ctx)
			started := reason == ""
			if started {
				var trial TrialResult
				trial, reason, detail = r.trial(ctx, c, n)
				caseResult.Trials = append(caseResult.Trials, trial)
				fmt.Fprintf(r.opts.Out, "%s trial %d/%d: %s\n", c.ID, n, entry.trials, trial.Status)
			}
			if reason == "" {
				continue
			}
			r.stop(reason, detail)
			unplayed := entry.trials - n
			if !started {
				unplayed++
			}
			if unplayed > 0 {
				r.result.NotRun = append(r.result.NotRun, NotRun{Case: c.ID, Trials: unplayed})
			}
			r.finishCase(caseResult)
			r.skip(plan[i+1:])
			return
		}
		r.finishCase(caseResult)
	}
}

func (r *runner) finishCase(c CaseResult) {
	if len(c.Trials) > 0 {
		r.result.Cases = append(r.result.Cases, c)
	}
}

func (r *runner) skip(rest []planned) {
	for _, entry := range rest {
		r.result.NotRun = append(r.result.NotRun, NotRun{Case: entry.c.ID, Trials: entry.trials})
	}
}

func (r *runner) stop(reason, detail string) {
	r.result.Stopped, r.result.StoppedDetail = reason, detail
}

func (r *runner) stopBefore(ctx context.Context) (string, string) {
	if ctx.Err() != nil {
		return StopCanceled, "the run was interrupted"
	}
	if r.result.ModelCalls >= r.opts.Budget {
		return StopBudget, fmt.Sprintf("%d model calls spent, budget %d", r.result.ModelCalls, r.opts.Budget)
	}
	return "", ""
}

// trialModel is the model and effort a case runs on, as the world resolves them.
func (r *runner) trialModel(c Case) (string, string) {
	model := cmp.Or(c.User.Model, r.opts.ChatModel.Model)
	effort := cmp.Or(c.User.Effort, r.opts.ChatModel.Effort, agentruntime.DefaultEffort)
	return model, effort
}

func (r *runner) typeSafeOn(c Case) bool {
	if r.opts.TypeSafe.APIKey == "" {
		return false
	}
	enabled, set := c.User.Switches[judgment.OptInSwitch.Integration]
	return !set || enabled
}

// trial plays one trial in a fresh world. A non-empty stop means the run must end: a quota or
// authentication failure, or an interrupt.
func (r *runner) trial(ctx context.Context, c Case, n int) (result TrialResult, stop, detail string) {
	model, effort := r.trialModel(c)
	result = TrialResult{N: n, TypeSafe: r.typeSafeOn(c), Model: model, Effort: effort}
	cassette := Cassette{
		RunID: r.result.RunID, Case: c.ID, Trial: n, PiVersion: r.result.PiVersion, Model: model, Effort: effort, TypeSafe: result.TypeSafe,
	}
	started := time.Now()
	logMark := r.log.mark()
	defer func() {
		cassette.Log = r.log.since(logMark)
		result.DurationMS = time.Since(started).Milliseconds()
		cassette.Failures = result.Failures
		cassette.Status, cassette.Error = result.Status, result.Error
		result.Cassette = r.writeCassette(c.ID, n, cassette)
	}()

	world, err := NewWorld(ctx, c, WorldOptions{
		Runtime: r.runtime, ProfilesDir: r.opts.Profiles.Root, AccountID: r.accountID,
		AuthBaseURL: r.opts.Endpoints.AuthBaseURL,
		ChatGPT:     chatgpt.Config{BaseURL: r.opts.Endpoints.ChatGPTBaseURL, HTTPClient: r.opts.Endpoints.HTTPClient},
		TypeSafe:    r.opts.TypeSafe, Variant: r.opts.Variant, ChatModel: r.opts.ChatModel, TaskModel: r.opts.TaskModel, HeadsUpModel: r.opts.HeadsUpModel,
		TurnTimeout: r.opts.TurnTimeout, StepTimeout: r.opts.StepTimeout,
	})
	if err != nil {
		result.Status, result.Error = TrialError, "build the world: "+clipError(err)
		return result, "", ""
	}
	defer func() {
		if err := world.Close(); err != nil && result.Error == "" && result.Status != TrialAborted {
			result.Error = "close the world: " + clipError(err)
			result.Status = TrialError
		}
	}()

	env := CheckEnv{Store: world.Store, Location: world.Location}
	for i, step := range c.Steps {
		record, err := world.Play(ctx, step)
		r.account(&result, record)
		cassette.Steps = append(cassette.Steps, r.cassetteStep(i+1, step, record, err))
		if category := providerStop(record.Runs); category != "" {
			result.Status = TrialAborted
			result.Error = fmt.Sprintf("step %d: the model provider reported %s", i+1, category)
			return result, stopReason(category), result.Error
		}
		if err != nil {
			if ctx.Err() != nil {
				result.Status, result.Error = TrialAborted, "interrupted"
				return result, StopCanceled, "the run was interrupted"
			}
			result.Status, result.Error = TrialError, fmt.Sprintf("step %d: %s", i+1, clipError(err))
			return result, "", ""
		}
		for _, failure := range CheckStep(ctx, step.Expect, record, env) {
			failure.Step = i + 1
			result.Failures = append(result.Failures, failure)
			cassette.Steps[i].Failures = append(cassette.Steps[i].Failures, failure)
		}
	}
	if len(result.Failures) > 0 {
		result.Status = TrialFailed
	} else {
		result.Status = TrialPassed
	}
	return result, "", ""
}

func (r *runner) account(trial *TrialResult, record TurnRecord) {
	trial.ModelCalls += record.Usage.ModelCalls
	trial.Tokens.Input += record.Tokens.Input
	trial.Tokens.Output += record.Tokens.Output
	trial.Tokens.CacheRead += record.Tokens.CacheRead
	trial.Tokens.CacheWrite += record.Tokens.CacheWrite
	if record.Model != "" {
		trial.ReportedModel = record.Model
		r.reported[record.Model] = struct{}{}
	}
	r.result.ModelCalls += record.Usage.ModelCalls
	r.result.Tokens.Input += record.Tokens.Input
	r.result.Tokens.Output += record.Tokens.Output
	r.result.Tokens.CacheRead += record.Tokens.CacheRead
	r.result.Tokens.CacheWrite += record.Tokens.CacheWrite
}

func providerStop(runs []RunRecord) agentruntime.FailureCategory {
	for _, run := range runs {
		var failure *agentruntime.ProviderError
		if errors.As(run.Err, &failure) {
			return failure.Category
		}
	}
	return ""
}

func stopReason(category agentruntime.FailureCategory) string {
	if category == agentruntime.FailureQuota {
		return StopQuota
	}
	return StopAuth
}

// Fingerprint is the SHA-256 of the system prompt a request renders, without memory and jobs: the
// fixed sections, variant or default, and the tools' guidance. It renders the prompt the way the Pi
// runtime does.
func Fingerprint(request agentruntime.RunRequest) string {
	sum := sha256.Sum256([]byte(request.SystemPrompt()))
	return hex.EncodeToString(sum[:])
}

func clipError(err error) string { return excerptTo(err.Error(), maxErrorRunes) }

func excerptTo(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

func sortedSet(set map[string]struct{}) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	slices.Sort(values)
	return values
}

// Cassette is everything one trial saw and did, written beside the run's report. Checks can be
// tried on it later without a model.
type Cassette struct {
	RunID     string         `json:"run_id"`
	Case      string         `json:"case"`
	Trial     int            `json:"trial"`
	PiVersion string         `json:"pi_version"`
	Model     string         `json:"model"`
	Effort    string         `json:"effort"`
	TypeSafe  bool           `json:"typesafe"`
	Status    string         `json:"status"`
	Error     string         `json:"error,omitempty"`
	Failures  []Failure      `json:"failures,omitempty"`
	Steps     []CassetteStep `json:"steps"`
	// Log holds the warn-and-above records the runtime logged during the trial, such as the scrubbed
	// stderr of a Pi that exited early.
	Log []json.RawMessage `json:"log,omitempty"`
}

// CassetteStep is one played step: the composed message, each model run, and what came out.
type CassetteStep struct {
	Index      int                      `json:"index"`
	Step       Step                     `json:"step"`
	Kind       string                   `json:"kind"`
	Message    string                   `json:"message,omitempty"`
	Runs       []CassetteRun            `json:"runs,omitempty"`
	ToolCalls  []ToolCall               `json:"tool_calls,omitempty"`
	Proposals  []Proposal               `json:"proposals,omitempty"`
	Resolved   []Proposal               `json:"resolved,omitempty"`
	Reply      string                   `json:"reply"`
	Quiet      bool                     `json:"quiet,omitempty"`
	Reconsider string                   `json:"reconsider,omitempty"`
	Usage      agentruntime.Usage       `json:"usage"`
	Tokens     agentruntime.TokenCounts `json:"tokens"`
	Model      string                   `json:"model,omitempty"`
	Failures   []Failure                `json:"failures,omitempty"`
	Error      string                   `json:"error,omitempty"`
}

// CassetteRun is one run of the model runtime inside a step.
type CassetteRun struct {
	Fingerprint string               `json:"prompt_fingerprint"`
	Model       string               `json:"model,omitempty"`
	Effort      string               `json:"effort,omitempty"`
	Events      []agentruntime.Event `json:"events,omitempty"`
	ToolCalls   []ToolCall           `json:"tool_calls,omitempty"`
	Response    string               `json:"response,omitempty"`
	Error       string               `json:"error,omitempty"`
}

func (r *runner) cassetteStep(index int, step Step, record TurnRecord, playErr error) CassetteStep {
	out := CassetteStep{
		Index: index, Step: step, Kind: record.Kind, Message: record.Message, ToolCalls: record.ToolCalls,
		Proposals: record.Proposals, Resolved: record.Resolved, Reply: record.Reply, Quiet: record.Quiet,
		Reconsider: record.Reconsider, Usage: record.Usage, Tokens: record.Tokens, Model: record.Model,
	}
	if playErr != nil {
		out.Error = clipError(playErr)
	}
	for _, run := range record.Runs {
		fingerprint := Fingerprint(run.Request)
		r.prints[fingerprint] = struct{}{}
		entry := CassetteRun{
			Fingerprint: fingerprint, Model: run.Request.Model, Effort: run.Request.Effort, Events: run.Events,
			ToolCalls: run.ToolCalls, Response: run.Result.Response,
		}
		if run.Err != nil {
			var failure *agentruntime.ProviderError
			if errors.As(run.Err, &failure) {
				entry.Error = string(failure.Category)
			} else {
				entry.Error = clipError(run.Err)
			}
		}
		out.Runs = append(out.Runs, entry)
	}
	return out
}

func (r *runner) writeCassette(caseID string, trial int, cassette Cassette) string {
	relative := filepath.Join(cassetteDir, fmt.Sprintf("%s-%d.json", caseID, trial))
	encoded, err := json.MarshalIndent(cassette, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.WriteFile(filepath.Join(r.result.Dir, relative), append(encoded, '\n'), 0o600); err != nil {
		return ""
	}
	return relative
}

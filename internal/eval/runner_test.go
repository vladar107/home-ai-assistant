package eval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/eval"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"github.com/vladar107/home-ai-assistant/internal/testutil/codexauth"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pifixture"
)

// scriptedModel plays a model that makes the calls a message lists as "fixture:call <tool> <json>"
// lines, fails like an exhausted subscription on "script:quota", and otherwise echoes through the
// Pi fixture. Every run counts as one model call.
type scriptedModel struct {
	*pi.Runtime
	runs *atomic.Int64
}

func (s scriptedModel) Run(ctx context.Context, request agentruntime.RunRequest, sink agentruntime.EventSink) (agentruntime.RunResult, error) {
	s.runs.Add(1)
	if strings.Contains(request.Message, "script:quota") {
		return agentruntime.RunResult{}, &agentruntime.ProviderError{Category: agentruntime.FailureQuota}
	}
	var texts []string
	var calls int
	for _, line := range strings.Split(request.Message, "\n") {
		call, ok := strings.CutPrefix(line, "fixture:call ")
		if !ok {
			continue
		}
		name, arguments, _ := strings.Cut(call, " ")
		result, err := request.Tools.Executor.Execute(ctx, agentruntime.ToolCall{ID: fmt.Sprint("call-", calls), Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil {
			return agentruntime.RunResult{}, err
		}
		texts = append(texts, result.Text)
		calls++
	}
	if calls == 0 {
		result, err := s.Runtime.Run(ctx, request, sink)
		result.Usage.ModelCalls = max(result.Usage.ModelCalls, 1)
		result.Tokens.Input, result.Tokens.Output = 20, 7
		return result, err
	}
	text := strings.Join(texts, "\n")
	return agentruntime.RunResult{Response: text, Final: text, Usage: agentruntime.Usage{ModelCalls: calls}, Tokens: agentruntime.TokenCounts{Input: 20, Output: 7}}, nil
}

type runFixture struct {
	t       *testing.T
	options eval.RunOptions
	cases   string
	env     string
	runs    *atomic.Int64
}

func newRunFixture(t *testing.T, used float64) *runFixture {
	t.Helper()
	sim := codexauth.New(t)
	profiles, err := eval.PrepareProfiles(filepath.Join(t.TempDir(), "profiles"), filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	login(t, profiles, sim)
	var seen []string
	stub := usageStub(t, used, &seen)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	f := &runFixture{t: t, cases: filepath.Join(root, "cases"), env: filepath.Join(root, ".env"), runs: &atomic.Int64{}}
	if err := os.MkdirAll(f.cases, 0o755); err != nil {
		t.Fatal(err)
	}
	f.options = eval.RunOptions{
		Profiles: profiles, CasesDir: f.cases, ResultsDir: filepath.Join(root, "results"),
		Endpoints:    eval.Endpoints{AuthBaseURL: sim.URL(), ChatGPTBaseURL: stub.URL},
		Pi:           eval.PiOptions{Binary: binary, Provider: "fixture-provider", ExpectedVersion: pifixture.Version},
		ChatModel:    agentruntime.ModelEffort{Model: "gpt-test", Effort: "low"},
		ScanEnvPaths: []string{f.env}, ScanDirs: []string{f.cases},
		StepTimeout: 30 * time.Second, Commit: "v0-test",
		Wrap: func(runtime *pi.Runtime) agentruntime.AgentRuntime {
			return scriptedModel{Runtime: runtime, runs: f.runs}
		},
	}
	return f
}

type stepSpec struct {
	Say    string         `json:"say"`
	Expect map[string]any `json:"expect,omitempty"`
}

func (f *runFixture) addCase(id, kind string, trials int, steps ...stepSpec) {
	f.t.Helper()
	body, err := json.Marshal(map[string]any{
		"id": id, "title": "Case " + id, "kind": kind, "trials": trials,
		"clock": map[string]string{"local": "2026-03-10T08:30", "zone": "Europe/Berlin"},
		"steps": steps,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cases, id+".json"), body, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *runFixture) run() (*eval.Result, error) {
	f.t.Helper()
	return eval.Run(f.t.Context(), f.options)
}

func echoes(text string) stepSpec {
	return stepSpec{Say: "hello", Expect: map[string]any{"reply": map[string]any{"contains": []string{text}}}}
}

func TestRunPlaysTrialsChecksThemAndWritesTheReport(t *testing.T) {
	f := newRunFixture(t, 10)
	f.addCase("remember-the-flat", "happy", 2, stepSpec{
		Say: `fixture:call memory_add {"kind":"fact","text":"Lives upstairs."}`,
		Expect: map[string]any{
			"calls": []map[string]any{{"tool": "memory_add", "args": []map[string]any{{"path": "text", "contains": "upstairs"}}}},
			"store": []map[string]any{{"check": "memory", "contains": "upstairs", "count": 1}},
		},
	})
	f.addCase("wrong-reply", "wrong-date", 1, echoes("never said"))

	result, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Summary.Overall; got.Passed != 2 || got.Ran != 3 {
		t.Fatalf("overall = %+v, want 2 of 3", got)
	}
	if got := result.Summary.Happy; !result.Summary.HasHappy || got.Passed != 2 || got.Ran != 2 {
		t.Fatalf("happy = %+v", got)
	}
	if got := result.Summary.PerKind["wrong-date"]; got.Passed != 0 || got.Ran != 1 {
		t.Fatalf("per kind = %+v", result.Summary.PerKind)
	}
	if result.Summary.PassK != 0.5 || result.Summary.K != 2 {
		t.Fatalf("pass^k = %v at k=%d, want 0.5 at 2", result.Summary.PassK, result.Summary.K)
	}
	if result.Stopped != "" || result.ModelCalls != 3 || result.Tokens.Input != 60 {
		t.Fatalf("stopped %q, calls %d, tokens %+v", result.Stopped, result.ModelCalls, result.Tokens)
	}
	if result.PiVersion != pifixture.Version || result.Model != "gpt-test" || result.Effort != "low" || result.Commit != "v0-test" || len(result.PromptFingerprint) != 64 {
		t.Fatalf("identity = %+v", result)
	}

	var failing eval.TrialResult
	for _, c := range result.Cases {
		for _, trial := range c.Trials {
			if c.ID == "wrong-reply" {
				failing = trial
			}
		}
	}
	if failing.Status != eval.TrialFailed || len(failing.Failures) != 1 || failing.Failures[0].Check != "reply.contains" || failing.Failures[0].Step != 1 {
		t.Fatalf("failing trial = %+v", failing)
	}

	report, err := os.ReadFile(filepath.Join(result.Dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Pass rate: 66.7% (2/3)", "Happy-path pass rate: 100.0% (2/2)", "| wrong-date | 0.0% (0/1) |", "### wrong-reply, trial 1: failed",
		"step 1, reply.contains", "Pi " + pifixture.Version, "Prompt fingerprint: `" + result.PromptFingerprint + "`", "Commit: v0-test",
		"0 trial(s) ran with the safety checks",
	} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	stored, err := os.ReadFile(filepath.Join(result.Dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded eval.Result
	if err := json.Unmarshal(stored, &decoded); err != nil || decoded.Summary.Overall != result.Summary.Overall || decoded.PromptFingerprint != result.PromptFingerprint {
		t.Fatalf("result.json = %v %+v", err, decoded.Summary)
	}

	cassette, err := os.ReadFile(filepath.Join(result.Dir, failing.Cassette))
	if err != nil {
		t.Fatal(err)
	}
	var recorded eval.Cassette
	if err := json.Unmarshal(cassette, &recorded); err != nil {
		t.Fatal(err)
	}
	if len(recorded.Steps) != 1 || !strings.Contains(recorded.Steps[0].Message, "hello") || !strings.HasPrefix(recorded.Steps[0].Reply, "reply:") ||
		len(recorded.Steps[0].Runs) != 1 || recorded.Steps[0].Runs[0].Fingerprint == "" || recorded.Steps[0].Usage.ModelCalls != 1 || len(recorded.Steps[0].Failures) != 1 {
		t.Fatalf("cassette = %+v", recorded)
	}
	if info, err := os.Stat(filepath.Join(result.Dir, failing.Cassette)); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("cassette permissions: %v %v", info, err)
	}
}

func TestRunSelectsCasesByIDAndTagAndOverridesTrials(t *testing.T) {
	f := newRunFixture(t, 10)
	f.addCase("alpha", "happy", 3, echoes("reply:"))
	f.addCase("beta", "happy", 3, echoes("reply:"))
	f.options.CaseIDs, f.options.Trials = []string{"beta"}, 1

	result, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Cases) != 1 || result.Cases[0].ID != "beta" || len(result.Cases[0].Trials) != 1 {
		t.Fatalf("cases = %+v", result.Cases)
	}
	f.options.CaseIDs = []string{"missing"}
	if _, err := f.run(); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unknown case: %v", err)
	}
	f.options.CaseIDs, f.options.Tags = nil, []string{"nothing-has-this"}
	if _, err := f.run(); err == nil {
		t.Fatal("a tag matching nothing started a run")
	}
}

func TestRunStopsAtItsBudgetAndReportsWhatItDidNotRun(t *testing.T) {
	f := newRunFixture(t, 10)
	f.addCase("alpha", "happy", 3, echoes("reply:"))
	f.addCase("beta", "happy", 2, echoes("reply:"))
	f.options.Budget = 2

	result, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if result.Stopped != eval.StopBudget || result.ModelCalls != 2 || f.runs.Load() != 2 {
		t.Fatalf("stopped %q after %d calls (%d runs)", result.Stopped, result.ModelCalls, f.runs.Load())
	}
	want := []eval.NotRun{{Case: "alpha", Trials: 1}, {Case: "beta", Trials: 2}}
	if fmt.Sprint(result.NotRun) != fmt.Sprint(want) {
		t.Fatalf("not run = %+v, want %+v", result.NotRun, want)
	}
	report, err := os.ReadFile(filepath.Join(result.Dir, "report.md"))
	if err != nil || !strings.Contains(string(report), "Stopped early: budget") || !strings.Contains(string(report), "- beta: 2 trial(s)") {
		t.Fatalf("report = %v\n%s", err, report)
	}
}

func TestRunStopsOnTheFirstQuotaFailure(t *testing.T) {
	f := newRunFixture(t, 10)
	f.addCase("alpha", "happy", 1, echoes("reply:"))
	f.addCase("beta", "happy", 3, stepSpec{Say: "script:quota"})
	f.addCase("gamma", "happy", 2, echoes("reply:"))

	result, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if result.Stopped != eval.StopQuota {
		t.Fatalf("stopped = %q (%s)", result.Stopped, result.StoppedDetail)
	}
	if f.runs.Load() != 2 {
		t.Fatalf("%d model runs started, want alpha's and beta's first", f.runs.Load())
	}
	want := []eval.NotRun{{Case: "beta", Trials: 2}, {Case: "gamma", Trials: 2}}
	if fmt.Sprint(result.NotRun) != fmt.Sprint(want) {
		t.Fatalf("not run = %+v, want %+v", result.NotRun, want)
	}
	beta := result.Cases[1]
	if beta.ID != "beta" || len(beta.Trials) != 1 || beta.Trials[0].Status != eval.TrialAborted {
		t.Fatalf("beta = %+v", beta)
	}
	if got := result.Summary.Overall; got.Ran != 1 || got.Passed != 1 {
		t.Fatalf("the aborted trial counted toward the rate: %+v", got)
	}
}

func TestRunRefusesAPiOfAnotherVersion(t *testing.T) {
	f := newRunFixture(t, 10)
	f.addCase("alpha", "happy", 1, echoes("reply:"))
	f.options.Pi.ExpectedVersion = ""

	_, err := f.run()
	if err == nil || !strings.Contains(err.Error(), pifixture.Version) || !strings.Contains(err.Error(), "want \"1.0.0\"") {
		t.Fatalf("a Pi of another version ran: %v", err)
	}
	if f.runs.Load() != 0 {
		t.Fatal("a model ran")
	}
	if _, statErr := os.Stat(f.options.ResultsDir); statErr == nil {
		t.Fatal("a refused run wrote results")
	}
}

func TestRunRefusesWhenTheQuotaIsTooHigh(t *testing.T) {
	f := newRunFixture(t, 80)
	f.addCase("alpha", "happy", 1, echoes("reply:"))

	_, err := f.run()
	if err == nil || !strings.Contains(err.Error(), "weekly") || f.runs.Load() != 0 {
		t.Fatalf("run = %v after %d model runs", err, f.runs.Load())
	}
	f.options.MaxUsedPercent = 90
	if _, err := f.run(); err != nil {
		t.Fatalf("raising the threshold: %v", err)
	}
}

func TestRunRefusesWhenTheScanFindsAHouseholdValue(t *testing.T) {
	f := newRunFixture(t, 10)
	f.addCase("alpha", "happy", 1, stepSpec{Say: "Meet me on Quillfeather Lane"})
	if err := os.WriteFile(f.env, []byte("HOME_AGENT_PLACES=Quillfeather Lane\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := f.run()
	if err == nil || !strings.Contains(err.Error(), "alpha.json") || !strings.Contains(err.Error(), "steps[0].say") || f.runs.Load() != 0 {
		t.Fatalf("run = %v after %d model runs", err, f.runs.Load())
	}
	if strings.Contains(strings.ToLower(err.Error()), "quillfeather") {
		t.Fatalf("the refusal carries the value: %v", err)
	}
}

func TestRunNeedsProfilesALoginAndAModel(t *testing.T) {
	f := newRunFixture(t, 10)
	f.addCase("alpha", "happy", 1, echoes("reply:"))

	options := f.options
	options.Profiles = eval.Profiles{}
	if _, err := eval.Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "--profiles") {
		t.Fatalf("without a profiles root: %v", err)
	}
	options = f.options
	options.ChatModel = agentruntime.ModelEffort{}
	if _, err := eval.Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "chat model") {
		t.Fatalf("without a model: %v", err)
	}
	options = f.options
	fresh, err := eval.PrepareProfiles(filepath.Join(t.TempDir(), "profiles"), filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	options.Profiles = fresh
	if _, err := eval.Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("without a login: %v", err)
	}
}

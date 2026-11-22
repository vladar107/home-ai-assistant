package eval_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/eval"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
)

func loadTestVariant(t *testing.T, body string) *eval.Variant {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "try.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := eval.LoadVariant(dir, "try")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRunUnderAVariantRecordsItAndSendsItsPrompt(t *testing.T) {
	t.Parallel()
	var prompts []string
	f := newRunFixture(t, 10)
	inner := f.options.Wrap
	f.options.Wrap = func(runtime *pi.Runtime) agentruntime.AgentRuntime {
		return promptSpy{inner: inner(runtime), seen: &prompts}
	}
	f.addCase("greets", "happy", 1, echoes("hello"))

	defaultRun, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if defaultRun.Variant != nil || strings.Contains(prompts[0], "Variant tail.") {
		t.Fatalf("the default run carried a variant: %+v", defaultRun.Variant)
	}

	f.options.Variant = loadTestVariant(t, `{"description":"adds a tail","sections":{"initiative":"Variant tail. "},"model":"gpt-variant","effort":"high"}`)
	f.options.ResultsDir = filepath.Join(filepath.Dir(f.options.ResultsDir), "variant-results")
	variantRun, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if variantRun.Variant == nil || variantRun.Variant.Name != "try" || len(variantRun.Variant.SHA256) != 64 {
		t.Fatalf("variant = %+v", variantRun.Variant)
	}
	if !strings.Contains(prompts[len(prompts)-1], "Variant tail.") {
		t.Error("the model was not sent the variant's prompt")
	}
	if variantRun.PromptFingerprint == defaultRun.PromptFingerprint {
		t.Error("the variant run has the default run's fingerprint")
	}
	if variantRun.Model != "gpt-variant" || variantRun.Effort != "high" {
		t.Errorf("model %q effort %q, want the variant's", variantRun.Model, variantRun.Effort)
	}
	report, err := os.ReadFile(filepath.Join(variantRun.Dir, "report.md"))
	if err != nil || !strings.Contains(string(report), "Variant: `try`") || !strings.Contains(string(report), variantRun.Variant.SHA256) {
		t.Errorf("report does not name the variant: %v\n%s", err, report)
	}
}

func TestAVariantModelReachesTheScheduledHeadsUpRun(t *testing.T) {
	t.Parallel()
	var models []string
	f := newRunFixture(t, 10)
	inner := f.options.Wrap
	f.options.Wrap = func(runtime *pi.Runtime) agentruntime.AgentRuntime {
		return modelSpy{inner: inner(runtime), seen: &models}
	}
	f.options.TaskModel = agentruntime.ModelEffort{Model: "gpt-strong-task", Effort: "medium"}
	f.options.HeadsUpModel = agentruntime.ModelEffort{Model: "gpt-strong-headsup", Effort: "high"}
	f.options.Variant = loadTestVariant(t, `{"description":"weaker","model":"gpt-weak"}`)
	body := `{"id": "briefing", "title": "Case briefing", "kind": "happy", "trials": 1,
		"clock": {"local": "2026-03-10T08:00", "zone": "Europe/Berlin"},
		"world": {"jobs": [{"id": "briefing", "name": "Morning Heads-up", "heads_up": true, "prompt": "Keep it short.", "cron": "0 8 * * *"}]},
		"steps": [{"run_job": "briefing"}]}`
	if err := os.WriteFile(filepath.Join(f.options.CasesDir, "briefing.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatalf("the Heads-up job ran no model; result %+v", result.Cases)
	}
	for _, model := range models {
		if model != "gpt-weak" {
			t.Errorf("a scheduled run went to %q, want the variant's gpt-weak (all: %v)", model, models)
		}
	}
}

type modelSpy struct {
	inner agentruntime.AgentRuntime
	seen  *[]string
}

func (s modelSpy) Run(ctx context.Context, request agentruntime.RunRequest, sink agentruntime.EventSink) (agentruntime.RunResult, error) {
	*s.seen = append(*s.seen, request.Model)
	return s.inner.Run(ctx, request, sink)
}

func TestRunRefusesAVariantNamingAnUnofferedTool(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, 10)
	f.addCase("greets", "happy", 1, echoes("hello"))
	f.options.Variant = loadTestVariant(t, `{"description":"d","tool_guidance":{"no_such_tool":"x"}}`)
	if _, err := f.run(); err == nil || !strings.Contains(err.Error(), "no_such_tool") {
		t.Fatalf("got %v", err)
	}
	if f.runs.Load() != 0 {
		t.Errorf("%d model calls before the refusal", f.runs.Load())
	}
}

type promptSpy struct {
	inner agentruntime.AgentRuntime
	seen  *[]string
}

func (s promptSpy) Run(ctx context.Context, request agentruntime.RunRequest, sink agentruntime.EventSink) (agentruntime.RunResult, error) {
	*s.seen = append(*s.seen, request.SystemPrompt())
	return s.inner.Run(ctx, request, sink)
}

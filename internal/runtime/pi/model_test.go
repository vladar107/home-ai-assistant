package pi_test

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/observability"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pifixture"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	index := slices.Index(args, flag)
	if index < 0 || index+1 >= len(args) {
		t.Fatalf("%s missing from %v", flag, args)
	}
	return args[index+1]
}

func TestEveryRunNamesModelAndEffort(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	tests := []struct {
		name         string
		kind         agentruntime.RunKind
		model        string
		effort       string
		wantModel    string
		wantThinking string
	}{
		{"background run without a pair uses the default", agentruntime.RunBackground, "", "", "fixture-model", "low"},
		{"chat run without a pair uses the default", agentruntime.RunChat, "", "", "fixture-model", "low"},
		{"task run with its own pair", agentruntime.RunTask, "task-model", "high", "task-model", "high"},
		{"chat override", agentruntime.RunChat, "chosen-model", "max", "chosen-model", "max"},
		{"model without effort runs on medium", agentruntime.RunChat, "chosen-model", "", "chosen-model", "medium"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := "pair-" + string(rune('a'+i))
			run := request(session, "fixture:model")
			run.Kind, run.Model, run.Effort = tc.kind, tc.model, tc.effort
			result, err := r.Run(context.Background(), run, &eventSink{})
			if err != nil {
				t.Fatal(err)
			}
			report := readReport(t, cfg, testPrincipal, session)
			if got := flagValue(t, report.Args, "--model"); got != tc.wantModel {
				t.Errorf("--model = %q, want %q", got, tc.wantModel)
			}
			if got := flagValue(t, report.Args, "--thinking"); got != tc.wantThinking {
				t.Errorf("--thinking = %q, want %q", got, tc.wantThinking)
			}
			if result.Model != tc.wantModel {
				t.Errorf("answering model = %q, want %q", result.Model, tc.wantModel)
			}
		})
	}
}

func TestResumedSessionIsGivenTheFlagsAgain(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	first := request("resumed", "remember amber")
	first.Model, first.Effort = "first-model", "high"
	if _, err := r.Run(context.Background(), first, &eventSink{}); err != nil {
		t.Fatal(err)
	}
	second := request("resumed", "fixture:model")
	if _, err := r.Run(context.Background(), second, &eventSink{}); err != nil {
		t.Fatal(err)
	}
	report := readReport(t, cfg, testPrincipal, "resumed")
	if got := flagValue(t, report.Args, "--model"); got != "fixture-model" {
		t.Errorf("--model on the resumed run = %q, want the default", got)
	}
	if got := flagValue(t, report.Args, "--thinking"); got != "low" {
		t.Errorf("--thinking on the resumed run = %q, want the default", got)
	}
}

func TestResultReportsAnsweringModelAndTokens(t *testing.T) {
	cfg := config(t)
	reader := sdkmetric.NewManualReader()
	telemetry, err := observability.NewWithReader(reader)
	if err != nil {
		t.Fatal(err)
	}
	r := newRuntime(t, cfg)
	r.SetTelemetry(telemetry)
	run := request("tokens", "fixture:model")
	run.Model, run.Effort = "chosen-model", "high"
	result, err := r.Run(context.Background(), run, &eventSink{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (agentruntime.TokenCounts{Input: 40, Output: 7, CacheRead: 30, CacheWrite: 3}); result.Tokens != want {
		t.Errorf("tokens = %+v, want %+v", result.Tokens, want)
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	efforts := map[string]bool{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "home_agent.model.tokens" {
				continue
			}
			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				effort, _ := point.Attributes.Value("home_agent.effort")
				efforts[effort.AsString()] = true
			}
		}
	}
	if len(efforts) != 1 || !efforts["high"] {
		t.Errorf("token metrics carry efforts %v, want only the requested one", efforts)
	}
}

func TestRunRejectsAnUnknownEffortBeforeStartingPi(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	run := request("bad-effort", "fixture:model")
	run.Model, run.Effort = "chosen-model", "extreme"
	if _, err := r.Run(context.Background(), run, &eventSink{}); err == nil {
		t.Fatal("an unknown effort was accepted")
	}
	if _, err := os.Stat(pifixture.ReportPath(sessionDir(cfg, testPrincipal), "bad-effort")); err == nil {
		t.Fatal("Pi was started")
	}
}

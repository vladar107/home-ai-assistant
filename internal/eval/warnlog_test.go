package eval_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/eval"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
)

// crashingModel makes Pi die on startup for a message that asks for it and echoes otherwise.
type crashingModel struct{ *pi.Runtime }

func (m crashingModel) Run(ctx context.Context, request agentruntime.RunRequest, sink agentruntime.EventSink) (agentruntime.RunResult, error) {
	if strings.Contains(request.Message, "script:crash") {
		request.Message = "fixture:crash"
	}
	return m.Runtime.Run(ctx, request, sink)
}

func TestRunLogsAPiThatExitedEarlyToTheRunDirectory(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, 10)
	f.options.Wrap = func(runtime *pi.Runtime) agentruntime.AgentRuntime { return crashingModel{runtime} }
	f.addCase("pi-crashes", "happy", 1, stepSpec{Say: "script:crash"})
	f.addCase("quiet-case", "happy", 1, echoes("hello"))

	result, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(result.Dir, "run.log")
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "pi.process.exited_early") || !strings.Contains(string(logged), "synthetic startup failure") || strings.Contains(string(logged), "synthetic-private-token") {
		t.Fatalf("run.log = %s", logged)
	}
	if info, err := os.Stat(logPath); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("run.log permissions: %v %v", info, err)
	}
	report, err := os.ReadFile(filepath.Join(result.Dir, "report.md"))
	if err != nil || !strings.Contains(string(report), "run.log") {
		t.Fatalf("report lacks the run log: %v\n%s", err, report)
	}
	for _, c := range result.Cases {
		raw, err := os.ReadFile(filepath.Join(result.Dir, c.Trials[0].Cassette))
		if err != nil {
			t.Fatal(err)
		}
		var recorded eval.Cassette
		if err := json.Unmarshal(raw, &recorded); err != nil {
			t.Fatal(err)
		}
		if got := len(recorded.Log) > 0; got != (c.ID == "pi-crashes") {
			t.Errorf("%s cassette has log records: %v", c.ID, got)
		}
	}
}

func TestRunWritesNoRunLogWhenNothingWasLogged(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, 10)
	f.addCase("quiet-case", "happy", 1, echoes("hello"))

	result, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(result.Dir, "run.log")); !os.IsNotExist(err) {
		t.Fatalf("run.log exists or failed to stat: %v", err)
	}
}

package pi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/observability"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pifixture"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type processCount struct{ kind, state string }

func processCounts(t *testing.T, reader *metric.ManualReader) map[processCount]int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	counts := map[processCount]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "home_agent.model.processes" {
				continue
			}
			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				kind, _ := point.Attributes.Value("home_agent.kind")
				state, _ := point.Attributes.Value("home_agent.state")
				counts[processCount{kind.AsString(), state.AsString()}] += point.Value
			}
		}
	}
	return counts
}

func awaitProcessCount(t *testing.T, reader *metric.ManualReader, kind, state string, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		counts := processCounts(t, reader)
		if counts[processCount{kind, state}] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s processes never reached %d: %v", kind, state, want, counts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func limitedRuntime(t *testing.T) (*pi.Runtime, pi.Config, *metric.ManualReader) {
	t.Helper()
	reader := metric.NewManualReader()
	telemetry, err := observability.NewWithReader(reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config(t)
	cfg.StallTimeout = testStallTimeout
	r := newRuntime(t, cfg)
	r.SetTelemetry(telemetry)
	// The first turn creates the principal's session directory, which concurrent first turns
	// would race to create.
	if _, err := r.Run(context.Background(), request("warm-up", "hello"), &eventSink{}); err != nil {
		t.Fatal(err)
	}
	return r, cfg, reader
}

// turns runs requests in the background and stops them all when the test ends.
type turns struct {
	ctx     context.Context
	cancel  context.CancelFunc
	r       *pi.Runtime
	wg      sync.WaitGroup
	mu      sync.Mutex
	results map[string]error
	cancels map[string]context.CancelFunc
}

func startTurns(t *testing.T, r *pi.Runtime) *turns {
	ctx, cancel := context.WithCancel(context.Background())
	running := &turns{ctx: ctx, cancel: cancel, r: r, results: map[string]error{}, cancels: map[string]context.CancelFunc{}}
	t.Cleanup(func() {
		cancel()
		running.wg.Wait()
	})
	return running
}

func (s *turns) start(run agentruntime.RunRequest) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.cancels[run.SessionID] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_, err := s.r.Run(ctx, run, &eventSink{})
		s.mu.Lock()
		s.results[run.SessionID] = err
		s.mu.Unlock()
	}()
}

func (s *turns) stop(session string) {
	s.mu.Lock()
	cancel := s.cancels[session]
	s.mu.Unlock()
	cancel()
}

func ofKind(kind agentruntime.RunKind, run agentruntime.RunRequest) agentruntime.RunRequest {
	run.Kind = kind
	return run
}

func reportExists(cfg pi.Config, session string) bool {
	_, err := os.Stat(pifixture.ReportPath(sessionDir(cfg, testPrincipal), session))
	return err == nil
}

func TestBackgroundTurnsWaitWhileChatTurnsRun(t *testing.T) {
	r, cfg, reader := limitedRuntime(t)
	running := startTurns(t, r)
	running.start(request("background-1", "fixture:busy"))
	running.start(request("background-2", "fixture:busy"))
	readReport(t, cfg, testPrincipal, "background-1")
	readReport(t, cfg, testPrincipal, "background-2")

	done := make(chan error, 1)
	go func() {
		result, err := r.Run(running.ctx, request("background-3", "hello"), &eventSink{})
		if err == nil && result.Response != "reply: hello" {
			err = errors.New("unexpected response " + result.Response)
		}
		done <- err
	}()
	awaitProcessCount(t, reader, "background", "waiting", 1)
	if reportExists(cfg, "background-3") {
		t.Fatal("a third background turn started while two were running")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Run(ctx, ofKind(agentruntime.RunChat, request("chat", "hello")), &eventSink{})
	if err != nil || result.Response != "reply: hello" {
		t.Fatalf("chat turn = %q, %v", result.Response, err)
	}
	result, err = r.Run(ctx, ofKind(agentruntime.RunTask, request("task", "hello")), &eventSink{})
	if err != nil || result.Response != "reply: hello" {
		t.Fatalf("task turn = %q, %v", result.Response, err)
	}

	// Waiting longer than the stall timeout must not count as a stall.
	time.Sleep(testStallTimeout + 500*time.Millisecond)
	running.stop("background-1")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiting background turn = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiting background turn never ran")
	}
	if stalls := stallOutcomes(t, reader); len(stalls) != 0 {
		t.Fatalf("stalls = %v, want none", stalls)
	}
	running.stop("background-2")
	awaitProcessCount(t, reader, "background", "running", 0)
	awaitProcessCount(t, reader, "chat", "running", 0)
}

func TestFreedSlotGoesToAWaitingChatTurnFirst(t *testing.T) {
	r, cfg, reader := limitedRuntime(t)
	running := startTurns(t, r)
	for _, run := range []agentruntime.RunRequest{
		request("background-1", "fixture:busy"),
		request("background-2", "fixture:busy"),
		ofKind(agentruntime.RunChat, request("chat-1", "fixture:busy")),
		ofKind(agentruntime.RunChat, request("chat-2", "fixture:busy")),
		ofKind(agentruntime.RunChat, request("chat-3", "fixture:busy")),
	} {
		running.start(run)
		readReport(t, cfg, testPrincipal, run.SessionID)
	}
	running.start(request("background-waiting", "fixture:busy"))
	awaitProcessCount(t, reader, "background", "waiting", 1)
	running.start(ofKind(agentruntime.RunChat, request("chat-waiting", "fixture:busy")))
	awaitProcessCount(t, reader, "chat", "waiting", 1)

	running.stop("background-1")
	readReport(t, cfg, testPrincipal, "chat-waiting")
	awaitProcessCount(t, reader, "chat", "waiting", 0)
	if reportExists(cfg, "background-waiting") || processCounts(t, reader)[processCount{"background", "waiting"}] != 1 {
		t.Fatal("the background turn took the slot before the waiting chat turn")
	}

	running.stop("chat-1")
	readReport(t, cfg, testPrincipal, "background-waiting")
	awaitProcessCount(t, reader, "background", "waiting", 0)
}

func TestWaitingTurnEndsOnCancellation(t *testing.T) {
	r, cfg, reader := limitedRuntime(t)
	running := startTurns(t, r)
	running.start(request("background-1", "fixture:busy"))
	running.start(request("background-2", "fixture:busy"))
	readReport(t, cfg, testPrincipal, "background-1")
	readReport(t, cfg, testPrincipal, "background-2")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := r.Run(ctx, request("background-3", "hello"), &eventSink{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want %v", err, context.DeadlineExceeded)
	}
	if reportExists(cfg, "background-3") {
		t.Fatal("the cancelled turn started a process")
	}
	awaitProcessCount(t, reader, "background", "waiting", 0)
	awaitProcessCount(t, reader, "background", "running", 2)
}

func TestEarlyExitLogsScrubbedStderr(t *testing.T) {
	cfg := config(t)
	var logs bytes.Buffer
	cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	r := newRuntime(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := r.Run(ctx, request("crash", "fixture:crash"), &eventSink{})
	if err == nil || !strings.Contains(err.Error(), "exit code 3") || strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("error = %v, want the exit code and no stderr text", err)
	}
	logged := logs.String()
	if !strings.Contains(logged, `"msg":"pi.process.exited_early"`) || !strings.Contains(logged, `"exit_code":3`) ||
		!strings.Contains(logged, "synthetic startup failure") || !strings.Contains(logged, "token=[redacted]") {
		t.Fatalf("log does not show the cause: %s", logged)
	}
	if strings.Contains(logged, "synthetic-private-token") {
		t.Fatalf("log leaked a secret: %s", logged)
	}
}

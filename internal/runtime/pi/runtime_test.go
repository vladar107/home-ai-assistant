package pi_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pifixture"
)

func TestMain(m *testing.M) {
	pifixture.RunIfRequested()
	os.Exit(m.Run())
}

type eventSink struct {
	events []agentruntime.Event
	err    error
}

func (s *eventSink) Emit(_ context.Context, event agentruntime.Event) error {
	s.events = append(s.events, event)
	return s.err
}

func config(t *testing.T) pi.Config {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return pi.Config{
		Binary: binary, Provider: "fixture-provider", Model: "fixture-model:low",
		ExpectedVersion: pifixture.Version, SessionDir: filepath.Join(t.TempDir(), "sessions"),
	}
}

func newRuntime(t *testing.T, cfg pi.Config) *pi.Runtime {
	t.Helper()
	r, err := pi.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func request(session, message string) agentruntime.RunRequest {
	return agentruntime.RunRequest{PrincipalID: "test-principal", SessionID: session, Message: message}
}

func TestProcessConfigurationAndSessionContinuity(t *testing.T) {
	t.Setenv("HOME_AGENT_TELEGRAM_BOT_TOKEN", "synthetic-private-token")
	t.Setenv("OPENAI_API_KEY", "synthetic-private-key")
	t.Setenv("NODE_OPTIONS", "synthetic-node-options")
	cfg := config(t)
	r := newRuntime(t, cfg)
	sink := &eventSink{}
	result, err := r.Run(context.Background(), request("session-one", "remember amber"), sink)
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "remembered amber" {
		t.Fatalf("response = %q", result.Response)
	}
	var streamed strings.Builder
	for _, event := range sink.events {
		if event.Type != "text_delta" {
			t.Fatalf("event type = %q", event.Type)
		}
		streamed.WriteString(event.Text)
	}
	if len(sink.events) != 2 || streamed.String() != result.Response {
		t.Fatalf("streamed events = %#v", sink.events)
	}
	report := readReport(t, cfg.SessionDir, "session-one")
	for _, flag := range []string{"--no-tools", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-approve"} {
		if !slices.Contains(report.Args, flag) {
			t.Errorf("missing lockdown flag %s", flag)
		}
	}
	for flag, want := range map[string]string{"--mode": "rpc", "--provider": cfg.Provider, "--model": cfg.Model, "--session-id": "session-one", "--session-dir": cfg.SessionDir} {
		index := slices.Index(report.Args, flag)
		if index < 0 || index+1 >= len(report.Args) || report.Args[index+1] != want {
			t.Errorf("%s not forwarded as %q: %#v", flag, want, report.Args)
		}
	}
	index := slices.Index(report.Args, "--system-prompt")
	if index < 0 || index+1 >= len(report.Args) || report.Args[index+1] == "" {
		t.Error("missing application system prompt")
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wantEnvironment := []string{"HOME=" + homeDir, "PATH=" + filepath.Dir(cfg.Binary), "PI_TELEMETRY=0"}
	slices.Sort(wantEnvironment)
	slices.Sort(report.Environment)
	if !reflect.DeepEqual(report.Environment, wantEnvironment) {
		t.Errorf("child environment was not restricted to the authentication home, executable path, and telemetry opt-out")
	}
	if len(report.Files) != 0 {
		t.Errorf("child working directory contained files: %v", report.Files)
	}
	if report.PromptID != "session-one" || report.Message != "remember amber" {
		t.Errorf("unexpected RPC prompt: %#v", report)
	}
	assertProcessStopped(t, report)
	info, err := os.Stat(cfg.SessionDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("session directory permissions: info=%v err=%v", info, err)
	}
	// A new adapter process must use the same persisted session history.
	r = newRuntime(t, cfg)
	for _, tc := range []struct{ session, want string }{{"session-one", "amber"}, {"session-two", "nothing remembered"}} {
		result, err := r.Run(context.Background(), request(tc.session, "recall"), &eventSink{})
		if err != nil || result.Response != tc.want {
			t.Errorf("session %s: response=%q err=%v", tc.session, result.Response, err)
		}
	}
}

func TestVersionCompatibility(t *testing.T) {
	cfg := config(t)
	cfg.ExpectedVersion = "incompatible-version"
	_, err := pi.New(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "check Pi version") {
		t.Fatalf("version mismatch error = %v", err)
	}
	cfg.Binary = filepath.Join(t.TempDir(), "missing-executable")
	_, err = pi.New(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing executable error = %v", err)
	}
}

func TestProcessFailuresAreBoundedAndContentFree(t *testing.T) {
	for _, tc := range []struct{ scenario, message string }{
		{"malformed", "read Pi output"},
		{"oversized-event", "read Pi output"},
		{"oversized-response", "response exceeded the size limit"},
		{"reject", "rejected the prompt"},
		{"tool", "tool event while tools are disabled"},
		{"toolcall", "tool event while tools are disabled"},
		{"extension-error", "extension error"},
		{"no-ack", "did not acknowledge"},
		{"empty", "empty response"},
		{"exit", ""},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			cfg := config(t)
			r := newRuntime(t, cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := r.Run(ctx, request("failure", "fixture:"+tc.scenario), &eventSink{})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want containing %q", err, tc.message)
			}
			if result.Response != "" || strings.Contains(err.Error(), "synthetic-private-text") {
				t.Fatalf("runtime failure exposed response content: result=%#v error=%v", result, err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("runtime did not fail before the test deadline: %v", err)
			}
			assertProcessStopped(t, readReport(t, cfg.SessionDir, "failure"))
		})
	}
}

func TestCancellationAndTimeoutStopRealProcess(t *testing.T) {
	for _, tc := range []struct{ name, message string }{{"cancel", "fixture:hold"}, {"timeout", "fixture:hold"}, {"uncooperative child", "fixture:stubborn"}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(t)
			r := newRuntime(t, cfg)
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if tc.name == "timeout" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
				want = context.DeadlineExceeded
			}
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := r.Run(ctx, request("blocked", tc.message), &eventSink{})
				finished <- err
			}()
			report := readReport(t, cfg.SessionDir, "blocked")
			if tc.name != "timeout" {
				cancel()
			}
			select {
			case err := <-finished:
				if !errors.Is(err, want) {
					t.Errorf("error = %v, want %v", err, want)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("cancelled subprocess did not stop")
			}
			assertProcessStopped(t, report)
		})
	}
}

func TestSinkFailureStopsProcess(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	sinkErr := errors.New("sink closed")
	_, err := r.Run(context.Background(), request("sink-failure", "hello"), &eventSink{err: sinkErr})
	if !errors.Is(err, sinkErr) {
		t.Fatalf("error = %v", err)
	}
	assertProcessStopped(t, readReport(t, cfg.SessionDir, "sink-failure"))
}

func TestRejectingQueuedEventsReleasesReader(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	before := eventReaderCount()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := r.Run(ctx, request("flood", "fixture:flood-after-tool"), &eventSink{})
	if err == nil || !strings.Contains(err.Error(), "tool event") {
		t.Fatalf("error = %v", err)
	}
	assertProcessStopped(t, readReport(t, cfg.SessionDir, "flood"))
	deadline := time.Now().Add(time.Second)
	for eventReaderCount() > before {
		if time.Now().After(deadline) {
			t.Fatal("event reader remained blocked after the child process stopped")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func eventReaderCount() int {
	stack := make([]byte, 1<<20)
	n := runtime.Stack(stack, true)
	return strings.Count(string(stack[:n]), "internal/runtime/pi.scanEvents(")
}

func readReport(t *testing.T, directory, session string) pifixture.Report {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(pifixture.ReportPath(directory, session))
		var report pifixture.Report
		if err == nil && json.Unmarshal(data, &report) == nil {
			return report
		}
		if time.Now().After(deadline) {
			t.Fatalf("subprocess did not write its report: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertProcessStopped(t *testing.T, report pifixture.Report) {
	t.Helper()
	if err := syscall.Kill(report.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("child process %d still exists: %v", report.PID, err)
	}
	if _, err := os.Stat(report.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("isolated child directory was not removed: %v", err)
	}
}

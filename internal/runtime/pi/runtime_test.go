package pi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

const testPrincipal = "test-principal"

func config(t *testing.T) pi.Config {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return pi.Config{
		Binary: binary, Provider: "fixture-provider", Model: "fixture-model:low",
		ExpectedVersion: pifixture.Version, ProfilesDir: filepath.Join(t.TempDir(), "profiles"),
	}
}

// newRuntime provisions the default principal the way the connection ceremony will.
func newRuntime(t *testing.T, cfg pi.Config) *pi.Runtime {
	t.Helper()
	r := newUnprovisionedRuntime(t, cfg)
	prepareProfile(t, cfg, testPrincipal)
	return r
}

func newUnprovisionedRuntime(t *testing.T, cfg pi.Config) *pi.Runtime {
	t.Helper()
	r, err := pi.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func prepareProfile(t *testing.T, cfg pi.Config, principal string) string {
	t.Helper()
	directory := agentDir(cfg, principal)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func agentDir(cfg pi.Config, principal string) string {
	return filepath.Join(cfg.ProfilesDir, principal)
}

func sessionDir(cfg pi.Config, principal string) string {
	return filepath.Join(agentDir(cfg, principal), "sessions")
}

func request(session, message string) agentruntime.RunRequest {
	return principalRequest(testPrincipal, session, message)
}

func principalRequest(principal, session, message string) agentruntime.RunRequest {
	return agentruntime.RunRequest{PrincipalID: principal, SessionID: session, Message: message}
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
	report := readReport(t, cfg, testPrincipal, "session-one")
	for _, flag := range []string{"--no-tools", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-approve"} {
		if !slices.Contains(report.Args, flag) {
			t.Errorf("missing lockdown flag %s", flag)
		}
	}
	for flag, want := range map[string]string{"--mode": "rpc", "--provider": cfg.Provider, "--model": cfg.Model, "--session-id": "session-one", "--session-dir": sessionDir(cfg, testPrincipal)} {
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
	wantEnvironment := []string{
		"HOME=" + homeDir,
		"PATH=" + filepath.Dir(cfg.Binary),
		"PI_TELEMETRY=0",
		"PI_CODING_AGENT_DIR=" + agentDir(cfg, testPrincipal),
	}
	slices.Sort(wantEnvironment)
	slices.Sort(report.Environment)
	if !reflect.DeepEqual(report.Environment, wantEnvironment) {
		t.Errorf("child environment was not restricted to the authentication home, executable path, telemetry opt-out, and the principal agent directory: %v", report.Environment)
	}
	if len(report.Files) != 0 {
		t.Errorf("child working directory contained files: %v", report.Files)
	}
	if report.PromptID != "session-one" || report.Message != "remember amber" {
		t.Errorf("unexpected RPC prompt: %#v", report)
	}
	assertProcessStopped(t, report)
	for _, directory := range []string{cfg.ProfilesDir, agentDir(cfg, testPrincipal), sessionDir(cfg, testPrincipal)} {
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s permissions: info=%v err=%v", directory, info, err)
		}
	}
	// A new adapter process must use the same persisted session history.
	r = newUnprovisionedRuntime(t, cfg)
	for _, tc := range []struct{ session, want string }{{"session-one", "amber"}, {"session-two", "nothing remembered"}} {
		result, err := r.Run(context.Background(), request(tc.session, "recall"), &eventSink{})
		if err != nil || result.Response != tc.want {
			t.Errorf("session %s: response=%q err=%v", tc.session, result.Response, err)
		}
	}
}

func TestPromptCarriesImages(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	picture := []byte("\x89PNG synthetic picture bytes")
	run := request("session-image", "what is on this label?")
	run.Images = []agentruntime.Image{{Name: "photo.png", MimeType: "image/png", Data: picture}}

	result, err := r.Run(context.Background(), run, &eventSink{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "reply: what is on this label?" {
		t.Fatalf("response = %q", result.Response)
	}
	report := readReport(t, cfg, testPrincipal, "session-image")
	if len(report.Images) != 1 {
		t.Fatalf("prompt images = %#v, want one", report.Images)
	}
	image := report.Images[0]
	decoded, err := base64.StdEncoding.DecodeString(image.Data)
	if err != nil {
		t.Fatal(err)
	}
	if image.Type != "image" || image.MimeType != "image/png" || !bytes.Equal(decoded, picture) {
		t.Fatalf("prompt image = {type:%q mime:%q bytes:%d}, want the picture as an image/png block",
			image.Type, image.MimeType, len(decoded))
	}
}

func TestConcurrentPrincipalsUseSeparateAgentDirectories(t *testing.T) {
	cfg := config(t)
	r := newUnprovisionedRuntime(t, cfg)
	principals := []string{"alice", "bob"}
	for _, principal := range principals {
		prepareProfile(t, cfg, principal)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, len(principals))
	for _, principal := range principals {
		go func() {
			_, err := r.Run(ctx, principalRequest(principal, "shared-session", "fixture:hold"), &eventSink{})
			finished <- err
		}()
	}
	// Both reports exist only while both children are running, so reading them proves overlap.
	reports := map[string]pifixture.Report{}
	for _, principal := range principals {
		reports[principal] = readReport(t, cfg, principal, "shared-session")
	}
	cancel()
	for range principals {
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want %v", err, context.Canceled)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("concurrent turns did not stop")
		}
	}
	for _, principal := range principals {
		report := reports[principal]
		if !slices.Contains(report.Environment, "PI_CODING_AGENT_DIR="+agentDir(cfg, principal)) {
			t.Errorf("%s ran outside its own agent directory: %v", principal, report.Environment)
		}
		index := slices.Index(report.Args, "--session-dir")
		if index < 0 || index+1 >= len(report.Args) || report.Args[index+1] != sessionDir(cfg, principal) {
			t.Errorf("%s session directory not forwarded: %#v", principal, report.Args)
		}
		assertProcessStopped(t, report)
	}
	if reports[principals[0]].PID == reports[principals[1]].PID {
		t.Error("both principals shared one process")
	}
}

func TestUnconnectedPrincipalNeverStartsAProcess(t *testing.T) {
	for _, tc := range []struct {
		name, principal string
		prepare         func(*testing.T, pi.Config)
	}{
		{name: "missing profile", principal: "unprovisioned"},
		{name: "shared profile", principal: "loose", prepare: func(t *testing.T, cfg pi.Config) {
			if err := os.Chmod(prepareProfile(t, cfg, "loose"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(t)
			r := newUnprovisionedRuntime(t, cfg)
			if tc.prepare != nil {
				tc.prepare(t, cfg)
			}
			result, err := r.Run(context.Background(), principalRequest(tc.principal, "unconnected", "hello"), &eventSink{})
			var providerErr *agentruntime.ProviderError
			if !errors.As(err, &providerErr) || providerErr.Category != agentruntime.FailureAuth {
				t.Fatalf("error = %v, want an authentication provider error", err)
			}
			if result.Response != "" {
				t.Errorf("result = %#v", result)
			}
			assertNoTurnStarted(t, cfg)
		})
	}
}

func TestInvalidPrincipalIsRejectedBeforeStart(t *testing.T) {
	cfg := config(t)
	r := newUnprovisionedRuntime(t, cfg)
	for _, principal := range []string{"../escape", "with/slash", "with space", ".hidden", ""} {
		t.Run(principal, func(t *testing.T) {
			_, err := r.Run(context.Background(), principalRequest(principal, "invalid", "hello"), &eventSink{})
			if err == nil {
				t.Fatal("invalid principal was accepted")
			}
			var providerErr *agentruntime.ProviderError
			if errors.As(err, &providerErr) {
				t.Fatalf("invalid principal reported a provider failure: %v", providerErr.Category)
			}
			assertNoTurnStarted(t, cfg)
		})
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
			assertProcessStopped(t, readReport(t, cfg, testPrincipal, "failure"))
		})
	}
}

func TestActionableProviderFailuresEndTheTurn(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		category agentruntime.FailureCategory
		retrying bool
	}{
		{"quota", agentruntime.FailureQuota, false},
		{"auth", agentruntime.FailureAuth, false},
		{"missing-key", agentruntime.FailureAuth, false},
		{"retry-quota", agentruntime.FailureQuota, true},
		{"retry-auth", agentruntime.FailureAuth, true},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			cfg := config(t)
			r := newRuntime(t, cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := time.Now()
			result, err := r.Run(ctx, request("provider-failure", "fixture:"+tc.scenario), &eventSink{})
			elapsed := time.Since(started)
			var providerErr *agentruntime.ProviderError
			if !errors.As(err, &providerErr) {
				t.Fatalf("error = %v, want a provider error", err)
			}
			if providerErr.Category != tc.category {
				t.Errorf("category = %q, want %q", providerErr.Category, tc.category)
			}
			if elapsed > 3*time.Second {
				t.Errorf("turn ended after %s instead of failing fast", elapsed)
			}
			if result.Response != "" || strings.Contains(err.Error(), "synthetic-private-text") {
				t.Fatalf("provider failure exposed content: result=%#v error=%v", result, err)
			}
			report := readReport(t, cfg, testPrincipal, "provider-failure")
			if tc.retrying && !report.AbortReceived {
				t.Error("adapter did not send abort_retry while Pi was retrying")
			}
			assertProcessStopped(t, report)
		})
	}
}

func TestUncategorizedProviderFailuresStayWithPi(t *testing.T) {
	for _, tc := range []struct{ scenario, response, message string }{
		{"unrelated-error", "", "empty response"},
		{"retry-transient", "recovered", ""},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			cfg := config(t)
			r := newRuntime(t, cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := r.Run(ctx, request("uncategorized", "fixture:"+tc.scenario), &eventSink{})
			var providerErr *agentruntime.ProviderError
			if errors.As(err, &providerErr) {
				t.Fatalf("uncategorized text was classified as %q", providerErr.Category)
			}
			if result.Response != tc.response {
				t.Errorf("response = %q, want %q", result.Response, tc.response)
			}
			switch {
			case tc.message == "":
				if err != nil {
					t.Fatalf("error = %v", err)
				}
			case err == nil || !strings.Contains(err.Error(), tc.message):
				t.Fatalf("error = %v, want containing %q", err, tc.message)
			case strings.Contains(err.Error(), "synthetic-private-text"):
				t.Fatalf("error exposed provider text: %v", err)
			}
			assertProcessStopped(t, readReport(t, cfg, testPrincipal, "uncategorized"))
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
			report := readReport(t, cfg, testPrincipal, "blocked")
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
	assertProcessStopped(t, readReport(t, cfg, testPrincipal, "sink-failure"))
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
	assertProcessStopped(t, readReport(t, cfg, testPrincipal, "flood"))
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

func readReport(t *testing.T, cfg pi.Config, principal, session string) pifixture.Report {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(pifixture.ReportPath(sessionDir(cfg, principal), session))
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
	info, err := os.Lstat(report.Directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || filepath.Base(report.Directory) != "workspace" {
		t.Errorf("child ran outside the principal's private workspace directory: %s (%v)", report.Directory, err)
	}
}

// assertNoTurnStarted fails when the adapter prepared a session directory or a child wrote a
// report, either of which would mean a process ran under the profiles root.
func assertNoTurnStarted(t *testing.T, cfg pi.Config) {
	t.Helper()
	err := filepath.WalkDir(cfg.ProfilesDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == "sessions" || strings.HasSuffix(entry.Name(), ".fixture.json") {
			return fmt.Errorf("turn was started: %s exists", path)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

// webSearchConfig points the runtime at a stand-in extension file; the fixture never loads it.
func webSearchConfig(t *testing.T) pi.Config {
	t.Helper()
	cfg := config(t)
	cfg.WebSearchExtension = filepath.Join(t.TempDir(), "index.ts")
	if err := os.WriteFile(cfg.WebSearchExtension, []byte("export default function () {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func extensionArgs(args []string) []string {
	var extensions []string
	for index, arg := range args {
		if arg == "--extension" && index+1 < len(args) {
			extensions = append(extensions, args[index+1])
		}
	}
	return extensions
}

func TestWebSearchTurnLoadsOnlyTheSearchExtension(t *testing.T) {
	cfg := webSearchConfig(t)
	r := newRuntime(t, cfg)
	run := request("web-search", "fixture:web-search")
	run.WebSearch = true
	sink := &eventSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Run(ctx, run, sink)
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Searching.\n\nfound: synthetic search answer" {
		t.Fatalf("response = %q", result.Response)
	}
	if !slices.Contains(sink.events, agentruntime.Event{Type: "tool_call", Text: "web_search"}) {
		t.Errorf("sink did not receive the search tool name: %+v", sink.events)
	}

	report := readReport(t, cfg, testPrincipal, "web-search")
	if slices.Contains(report.Args, "--no-tools") || !slices.Contains(report.Args, "--no-builtin-tools") || !slices.Contains(report.Args, "--no-extensions") {
		t.Errorf("web search turn flags: %v", report.Args)
	}
	if extensions := extensionArgs(report.Args); !slices.Equal(extensions, []string{cfg.WebSearchExtension}) {
		t.Errorf("extensions = %v, want only the search extension", extensions)
	}
	if environmentValue(report, "HOME_AGENT_SEARCH_CHECK_URL") == "" || environmentValue(report, "HOME_AGENT_TOOL_BRIDGE_TOKEN") == "" {
		t.Errorf("web search turn lost the search check environment: %v", report.Environment)
	}
	if len(report.BridgeFiles) != 0 || len(report.Files) != 0 {
		t.Errorf("web search turn wrote files: bridge=%v workspace=%v", report.BridgeFiles, report.Files)
	}
	if prompt := report.Args[slices.Index(report.Args, "--system-prompt")+1]; !strings.Contains(prompt, "web_search") || strings.Contains(prompt, "no integrations or tools") {
		t.Errorf("system prompt did not describe web search: %q", prompt)
	}
	assertProcessStopped(t, report)
	entries, err := os.ReadDir(agentDir(cfg, testPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "bridge-") {
			t.Errorf("web search turn created bridge directory %s", entry.Name())
		}
	}
}

func TestWebSearchTurnWithToolsLoadsBridgeFirst(t *testing.T) {
	cfg := webSearchConfig(t)
	r := newRuntime(t, cfg)
	provider := &echoProvider{principal: testPrincipal}
	run := toolRequest("web-search-tools", `fixture:call echo_tool {"text":"amber"}`, toolSet(t, provider, testPrincipal))
	run.WebSearch = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Run(ctx, run, &eventSink{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(result.Response, "echoed: amber") {
		t.Fatalf("response = %q", result.Response)
	}
	report := readReport(t, cfg, testPrincipal, "web-search-tools")
	if slices.Contains(report.Args, "--no-tools") || !slices.Contains(report.Args, "--no-builtin-tools") || !slices.Contains(report.Args, "--no-extensions") {
		t.Errorf("combined turn flags: %v", report.Args)
	}
	extensions := extensionArgs(report.Args)
	if len(extensions) != 2 || filepath.Base(extensions[0]) != "bridge.ts" || extensions[1] != cfg.WebSearchExtension {
		t.Fatalf("extensions = %v, want the bridge and then the search extension", extensions)
	}
	if environmentValue(report, "HOME_AGENT_TOOL_BRIDGE_URL") == "" {
		t.Errorf("combined turn lost the bridge environment: %v", report.Environment)
	}
	assertProcessStopped(t, report)
	assertBridgeRemoved(t, cfg, extensions[0])
}

func TestWebSearchWithoutExtensionNeverStarts(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	run := request("web-search-unconfigured", "hello")
	run.WebSearch = true
	_, err := r.Run(context.Background(), run, &eventSink{})
	if err == nil || !strings.Contains(err.Error(), "web search") {
		t.Fatalf("error = %v", err)
	}
	assertNoTurnStarted(t, cfg)
}

func TestUnavailableWebSearchExtensionFailsStartup(t *testing.T) {
	for name, path := range map[string]string{
		"missing":   filepath.Join(t.TempDir(), "missing.ts"),
		"directory": t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config(t)
			cfg.WebSearchExtension = path
			if _, err := pi.New(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "web search extension") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

type sessionReferences map[string]struct{}

func (r sessionReferences) ReferencedSessions(context.Context) (map[string]struct{}, error) {
	return r, nil
}

func TestPrunerRemovesOnlyOldUnreferencedSessions(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	directory := sessionDir(cfg, testPrincipal)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	files := map[string]time.Time{
		"2026-01-01T00-00-00-000Z_chat-session.jsonl": cutoff.Add(-time.Hour),
		"2026-01-01T00-00-00-000Z_old-run.jsonl":      cutoff.Add(-time.Hour),
		"2026-01-01T00-00-00-000Z_recent-run.jsonl":   cutoff.Add(time.Hour),
	}
	for name, modified := range files {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.SessionPruner(sessionReferences{"chat-session": {}}).PruneSessions(context.Background(), cutoff); err != nil {
		t.Fatal(err)
	}
	for name := range files {
		_, err := os.Stat(filepath.Join(directory, name))
		if removed := errors.Is(err, fs.ErrNotExist); removed != strings.Contains(name, "old-run") {
			t.Errorf("%s removed = %v", name, removed)
		}
	}
}

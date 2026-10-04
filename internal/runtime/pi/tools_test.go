package pi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pifixture"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const echoSchema = `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`

type providerCall struct {
	principal string
	call      tools.Call
}

// echoProvider grants one principal an echo tool and records every execution it receives.
type echoProvider struct {
	principal string
	guidance  string
	mu        sync.Mutex
	calls     []providerCall
	respond   func(tools.Call) (tools.Result, error)
}

func (p *echoProvider) Contribute(_ context.Context, principal string) (tools.Contribution, error) {
	if principal != p.principal {
		return tools.Contribution{}, nil
	}
	return tools.Contribution{
		Guidance: p.guidance,
		Definitions: []tools.Definition{{
			Name: "echo_tool", Description: "Echo the text back.", InputSchema: json.RawMessage(echoSchema), Risk: tools.RiskRead,
		}},
	}, nil
}

func (p *echoProvider) Execute(_ context.Context, principal string, call tools.Call) (tools.Result, error) {
	p.mu.Lock()
	p.calls = append(p.calls, providerCall{principal: principal, call: call})
	p.mu.Unlock()
	if p.respond != nil {
		return p.respond(call)
	}
	var arguments struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Text: "echoed: " + arguments.Text}, nil
}

func (p *echoProvider) recorded() []providerCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providerCall(nil), p.calls...)
}

func toolSet(t *testing.T, provider tools.Provider, principal string) agentruntime.ToolSet {
	t.Helper()
	broker, err := tools.NewBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(provider); err != nil {
		t.Fatal(err)
	}
	set, err := broker.ToolsFor(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func toolRequest(session, message string, set agentruntime.ToolSet) agentruntime.RunRequest {
	request := request(session, message)
	request.Tools = set
	return request
}

func environmentValue(report pifixture.Report, name string) string {
	for _, entry := range report.Environment {
		if value, found := strings.CutPrefix(entry, name+"="); found {
			return value
		}
	}
	return ""
}

func TestToolTurnBridgesCallsThroughTheBroker(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	provider := &echoProvider{principal: testPrincipal, guidance: "Use echo_tool to repeat text."}
	set := toolSet(t, provider, testPrincipal)
	sink := &eventSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Run(ctx, toolRequest("tools", `fixture:call echo_tool {"text":"amber"}`, set), sink)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(result.Response, "\n\n", 2)
	if len(parts) != 2 || parts[0] != "Checking." || !strings.HasSuffix(parts[1], "echoed: amber") || !strings.Contains(parts[1], "untrusted") {
		t.Fatalf("response = %q, want the two assistant messages joined and the result marked untrusted", result.Response)
	}
	if calls := provider.recorded(); len(calls) != 1 || calls[0].principal != testPrincipal || calls[0].call.Name != "echo_tool" || string(calls[0].call.Arguments) != `{"text":"amber"}` {
		t.Fatalf("provider calls = %+v", calls)
	}
	if !slices.Contains(sink.events, agentruntime.Event{Type: "tool_call", Text: "echo_tool"}) {
		t.Errorf("sink did not receive the tool name: %+v", sink.events)
	}
	for _, event := range sink.events {
		if event.Type == "tool_call" && event.Text != "echo_tool" {
			t.Errorf("sink received unexpected tool event %+v", event)
		}
	}

	report := readReport(t, cfg, testPrincipal, "tools")
	if slices.Contains(report.Args, "--no-tools") || !slices.Contains(report.Args, "--no-builtin-tools") || !slices.Contains(report.Args, "--no-extensions") {
		t.Errorf("tool turn flags: %v", report.Args)
	}
	index := slices.Index(report.Args, "--extension")
	if index < 0 || index+1 >= len(report.Args) {
		t.Fatalf("missing --extension: %v", report.Args)
	}
	extension := report.Args[index+1]
	if filepath.Base(extension) != "bridge.ts" || !strings.HasPrefix(extension, agentDir(cfg, testPrincipal)+string(filepath.Separator)+"bridge-") {
		t.Errorf("extension %q is not inside a per-turn bridge directory under the agent directory", extension)
	}
	slices.Sort(report.BridgeFiles)
	if !slices.Equal(report.BridgeFiles, []string{"bridge.ts", "tools.json"}) {
		t.Errorf("bridge directory contents = %v", report.BridgeFiles)
	}
	var listed []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(report.Tools, &listed); err != nil || len(listed) != 1 || listed[0].Name != "echo_tool" ||
		listed[0].Description != "Echo the text back." || string(listed[0].Parameters) != echoSchema {
		t.Errorf("tools.json = %s (%v)", report.Tools, err)
	}
	if prompt := report.Args[slices.Index(report.Args, "--system-prompt")+1]; !strings.Contains(prompt, "Use echo_tool to repeat text.") || strings.Contains(prompt, "no integrations") {
		t.Errorf("system prompt did not carry the tool guidance: %q", prompt)
	}
	if environmentValue(report, "HOME_AGENT_TOOL_BRIDGE_URL") == "" || environmentValue(report, "HOME_AGENT_TOOL_BRIDGE_TOKEN") == "" ||
		environmentValue(report, "HOME_AGENT_TOOL_BRIDGE_TOOLS") != filepath.Join(filepath.Dir(extension), "tools.json") {
		t.Errorf("bridge environment incomplete: %v", report.Environment)
	}
	if len(report.Files) != 0 {
		t.Errorf("workspace contained files during a tool turn: %v", report.Files)
	}
	assertProcessStopped(t, report)
	assertBridgeRemoved(t, cfg, extension)
	if _, err := net.Dial("tcp", strings.TrimPrefix(strings.TrimSuffix(environmentValue(report, "HOME_AGENT_TOOL_BRIDGE_URL"), "/call"), "http://")); err == nil {
		t.Error("bridge listener still accepted connections after the turn")
	}

	// A later turn without tools keeps the lockdown and writes nothing.
	plain, err := r.Run(ctx, request("plain", "hello"), &eventSink{})
	if err != nil || plain.Response != "reply: hello" {
		t.Fatalf("plain turn: %q %v", plain.Response, err)
	}
	report = readReport(t, cfg, testPrincipal, "plain")
	if !slices.Contains(report.Args, "--no-tools") || slices.Contains(report.Args, "--extension") || environmentValue(report, "HOME_AGENT_TOOL_BRIDGE_URL") != "" {
		t.Errorf("tool-less turn was not locked down: %v %v", report.Args, report.Environment)
	}
}

func TestToolBridgeRejectsCallsOutsideTheGrant(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	oversized := strings.Repeat("x", (16<<10)+1)
	provider := &echoProvider{principal: testPrincipal}
	provider.respond = func(call tools.Call) (tools.Result, error) {
		if strings.Contains(string(call.Arguments), "oversized") {
			return tools.Result{Text: oversized}, nil
		}
		if strings.Contains(string(call.Arguments), "fail") {
			return tools.Result{}, errors.New("synthetic-private-text")
		}
		return tools.Result{Text: "ok"}, nil
	}
	set := toolSet(t, provider, testPrincipal)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, toolRequest("bridge", "fixture:hold", set), &eventSink{})
		finished <- err
	}()
	report := readReport(t, cfg, testPrincipal, "bridge")
	url := environmentValue(report, "HOME_AGENT_TOOL_BRIDGE_URL")
	token := environmentValue(report, "HOME_AGENT_TOOL_BRIDGE_TOKEN")
	post := func(bearer, body string) (int, pifixture.BridgeResult) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result pifixture.BridgeResult
		if response.StatusCode == http.StatusOK {
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
		}
		return response.StatusCode, result
	}
	call := func(arguments string) string {
		return fmt.Sprintf(`{"name":"echo_tool","arguments":%s}`, arguments)
	}

	if status, _ := post("", call(`{"text":"anonymous"}`)); status != http.StatusUnauthorized {
		t.Errorf("missing token status = %d", status)
	}
	if status, _ := post("wrong-token", call(`{"text":"forged"}`)); status != http.StatusUnauthorized {
		t.Errorf("wrong token status = %d", status)
	}
	if status, _ := post(token, `{"name":"echo_tool","arguments":{"text":"`+strings.Repeat("y", 64<<10)+`"}}`); status != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized arguments status = %d", status)
	}
	if status, _ := post(token, `not json`); status != http.StatusBadRequest {
		t.Errorf("malformed body status = %d", status)
	}
	if status, result := post(token, `{"name":"delete_everything","arguments":{}}`); status != http.StatusOK || !result.IsError || !strings.Contains(result.Text, "not available") {
		t.Errorf("unknown tool = %d %+v", status, result)
	}
	if len(provider.recorded()) != 0 {
		t.Fatalf("rejected calls reached the provider: %+v", provider.recorded())
	}
	if _, result := post(token, call(`{"text":"oversized"}`)); !result.IsError || !strings.Contains(result.Text, "size limit") || strings.Contains(result.Text, "xxxx") {
		t.Errorf("oversized result = %+v", result)
	}
	if _, result := post(token, call(`{"text":"fail"}`)); !result.IsError || strings.Contains(result.Text, "synthetic-private-text") {
		t.Errorf("failed call = %+v", result)
	}
	for i := 0; i < 5; i++ {
		if _, result := post(token, call(`{"text":"fine"}`)); result.IsError || !strings.HasSuffix(result.Text, "ok") {
			t.Fatalf("call %d = %+v", i, result)
		}
	}
	if _, result := post(token, call(`{"text":"ninth"}`)); !result.IsError || !strings.Contains(result.Text, "budget") {
		t.Errorf("call past the budget = %+v", result)
	}
	if calls := provider.recorded(); len(calls) != 7 {
		t.Errorf("provider received %d calls, want 7 (1 unknown and 1 past budget rejected first)", len(calls))
	}

	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("tool turn did not stop")
	}
	assertProcessStopped(t, report)
	assertBridgeRemoved(t, cfg, report.Args[slices.Index(report.Args, "--extension")+1])
	if _, err := http.Post(url, "application/json", bytes.NewReader(nil)); err == nil {
		t.Error("bridge accepted a call after the turn ended")
	}
}

func TestToolTurnWithoutExecutorNeverStarts(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	set := agentruntime.ToolSet{Definitions: []agentruntime.ToolDefinition{{Name: "echo_tool", Description: "Echo.", InputSchema: json.RawMessage(echoSchema)}}}
	_, err := r.Run(context.Background(), toolRequest("no-executor", "hello", set), &eventSink{})
	if err == nil || !strings.Contains(err.Error(), "executor") {
		t.Fatalf("error = %v", err)
	}
	assertNoTurnStarted(t, cfg)
}

func assertBridgeRemoved(t *testing.T, cfg pi.Config, extension string) {
	t.Helper()
	if _, err := os.Stat(filepath.Dir(extension)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("bridge directory survived the turn: %v", err)
	}
	entries, err := os.ReadDir(agentDir(cfg, testPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "bridge-") {
			t.Errorf("stale bridge directory %s remained", entry.Name())
		}
	}
}

func TestMemoryFollowsTheToolGuidanceInTheSystemPrompt(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	provider := &echoProvider{principal: testPrincipal, guidance: "Use echo_tool to repeat text."}
	request := toolRequest("memory", "hello", toolSet(t, provider, testPrincipal))
	request.Memory = "Memory about this user.\n- [aaaaaaa1] likes tea"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.Run(ctx, request, &eventSink{}); err != nil {
		t.Fatal(err)
	}
	report := readReport(t, cfg, testPrincipal, "memory")
	prompt := report.Args[slices.Index(report.Args, "--system-prompt")+1]
	if !strings.HasSuffix(prompt, "Use echo_tool to repeat text.\n\n"+request.Memory) {
		t.Fatalf("system prompt does not end with the guidance and then the memory: %q", prompt)
	}
}

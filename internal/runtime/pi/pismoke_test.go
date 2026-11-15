//go:build pismoke

package pi_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
)

// TestRealPiAcceptsTheRuntimeArguments starts the real Pi in RPC mode with the runtime's exact
// argument list, bridge and vendored web search extension included. The profile holds no
// credentials, so Pi rejects the prompt before any model call. That rejection proves Pi accepted
// every flag and loaded both extensions, because a rejected flag or a failing extension ends Pi
// at startup instead and the turn fails differently. It needs no network, so it stays out of CI
// behind the pismoke tag. HOME_AGENT_SMOKE_NODE names the Node binary when Pi's shebang cannot
// find one:
//
//	HOME_AGENT_SMOKE_PI=$PWD/third_party/pi-runtime/node_modules/.bin/pi go test -tags pismoke -run TestRealPiAcceptsTheRuntimeArguments ./internal/runtime/pi
func TestRealPiAcceptsTheRuntimeArguments(t *testing.T) {
	binary := os.Getenv("HOME_AGENT_SMOKE_PI")
	if binary == "" {
		t.Skip("HOME_AGENT_SMOKE_PI names the Pi binary to start")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	webSearch, err := filepath.Abs("../../../third_party/pi-web-search/src/index.ts")
	if err != nil {
		t.Fatal(err)
	}
	cfg := pi.Config{
		Binary: binary, NodeBinary: os.Getenv("HOME_AGENT_SMOKE_NODE"), Provider: "openai-codex",
		Model:           agentruntime.ModelEffort{Model: "gpt-6-luna", Effort: "low"},
		ExpectedVersion: os.Getenv("HOME_AGENT_PI_VERSION"), ProfilesDir: filepath.Join(t.TempDir(), "profiles"),
		WebSearchExtension: webSearch,
	}
	if cfg.ExpectedVersion == "" {
		cfg.ExpectedVersion = "1.1.0"
	}
	r := newRuntime(t, cfg)
	set := toolSet(t, &echoProvider{principal: testPrincipal}, testPrincipal)
	run := toolRequest("smoke", "hello", set)
	run.WebSearch = true

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err = r.Run(ctx, run, &eventSink{})
	if err == nil || !strings.Contains(err.Error(), "rejected the prompt") {
		t.Fatalf("error = %v, want Pi to start and reject the prompt for its missing credentials", err)
	}
}

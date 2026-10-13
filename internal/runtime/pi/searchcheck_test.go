package pi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

type searchRecorder struct {
	mu      sync.Mutex
	queries []string
}

func (r *searchRecorder) check(decide func(string) agentruntime.SearchDecision) agentruntime.SearchCheck {
	return func(_ context.Context, query string) agentruntime.SearchDecision {
		r.mu.Lock()
		r.queries = append(r.queries, query)
		r.mu.Unlock()
		return decide(query)
	}
}

func searchTurn(t *testing.T, check agentruntime.SearchCheck, query string) string {
	t.Helper()
	cfg := webSearchConfig(t)
	r := newRuntime(t, cfg)
	run := request("search-check", "fixture:search "+query)
	run.WebSearch = true
	run.SearchCheck = check
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Run(ctx, run, &eventSink{})
	if err != nil {
		t.Fatal(err)
	}
	return result.Response
}

func TestSearchRunsOnlyWhenTheCheckAllowsIt(t *testing.T) {
	recorder := &searchRecorder{}
	decisions := map[string]agentruntime.SearchDecision{
		"plain":   {Verdict: agentruntime.SearchAllow, Reason: "clean"},
		"address": {Verdict: agentruntime.SearchConfirm, Reason: "pattern", Text: "waiting for the user"},
		"refused": {Verdict: agentruntime.SearchDeny, Reason: "pattern", Text: "not available here"},
	}
	check := recorder.check(func(query string) agentruntime.SearchDecision { return decisions[query] })

	for query, want := range map[string]string{
		"plain":   "Searching.\n\nsaw: found: synthetic search answer",
		"address": "Searching.\n\nsaw: waiting for the user",
		"refused": "Searching.\n\nsaw: not available here",
	} {
		if got := searchTurn(t, check, query); got != want {
			t.Errorf("query %q: response = %q, want %q", query, got, want)
		}
	}
	if len(recorder.queries) != 3 {
		t.Errorf("checked queries = %v", recorder.queries)
	}
}

// An empty query is the model's slip; it is told so rather than that search is down, and the check
// never sees it.
func TestEmptySearchQueryIsRefusedAsEmpty(t *testing.T) {
	check := func(context.Context, string) agentruntime.SearchDecision {
		t.Error("check ran for an empty query")
		return agentruntime.SearchDecision{Verdict: agentruntime.SearchAllow}
	}
	got := searchTurn(t, check, "")
	if want := "Searching.\n\nsaw: The search query was empty, so nothing was searched; search again with the words to look for."; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
}

func TestSearchWithoutACheckIsRefused(t *testing.T) {
	got := searchTurn(t, nil, "plain")
	if want := "Searching.\n\nsaw: Web search is unavailable right now; the search was not run."; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
}

func TestSearchCheckSeesTheTurnContext(t *testing.T) {
	type key struct{}
	cfg := webSearchConfig(t)
	r := newRuntime(t, cfg)
	run := request("search-context", "fixture:search plain")
	run.WebSearch = true
	var seen any
	run.SearchCheck = func(ctx context.Context, _ string) agentruntime.SearchDecision {
		seen = ctx.Value(key{})
		return agentruntime.SearchDecision{Verdict: agentruntime.SearchAllow}
	}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), key{}, "turn"), 10*time.Second)
	defer cancel()
	if _, err := r.Run(ctx, run, &eventSink{}); err != nil {
		t.Fatal(err)
	}
	if seen != "turn" {
		t.Fatalf("check context value = %v, want the turn's", seen)
	}
}

func TestSearchCheckEndpointRequiresTheTurnToken(t *testing.T) {
	cfg := webSearchConfig(t)
	r := newRuntime(t, cfg)
	run := request("search-endpoint", "fixture:hold")
	run.WebSearch = true
	run.SearchCheck = func(context.Context, string) agentruntime.SearchDecision {
		t.Error("check ran for an unauthenticated request")
		return agentruntime.SearchDecision{Verdict: agentruntime.SearchAllow}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, run, &eventSink{})
		finished <- err
	}()
	report := readReport(t, cfg, testPrincipal, "search-endpoint")
	url := environmentValue(report, "HOME_AGENT_SEARCH_CHECK_URL")
	for name, headers := range map[string]string{"missing": "", "wrong": "Bearer wrong"} {
		status := postSearchCheck(t, url, headers, `{"query":"plain"}`)
		if status != 401 {
			t.Errorf("%s token: status = %d", name, status)
		}
	}
	token := "Bearer " + environmentValue(report, "HOME_AGENT_TOOL_BRIDGE_TOKEN")
	if status := postSearchCheck(t, url, token, `{"query":"  "}`); status != 200 {
		t.Errorf("blank query: status = %d", status)
	}
	if status := postSearchCheck(t, url, token, `not json`); status != 400 {
		t.Errorf("malformed body: status = %d", status)
	}
	if status := postSearchCheck(t, url, token, `{"query":"`+strings.Repeat("x", 65<<10)+`"}`); status != 413 {
		t.Errorf("oversized body: status = %d", status)
	}
	cancel()
	<-finished
}

// The vendored extension is written in TypeScript, which only Node can run here, so its fail-closed
// behaviour is exercised only where Node is installed.
func TestVendoredExtensionRefusesUnlessTheCheckAllows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := filepath.Abs(filepath.Join("..", "..", "..", "third_party", "pi-web-search", "src", "search_check.ts"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "check.mjs")
	body := "import { searchRefusal } from " + strconv.Quote(source) + ";\n" +
		"process.stdout.write(JSON.stringify((await searchRefusal(\"query\")) ?? null));\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reply := func(status int, text string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(text))
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	unavailable := `"Web search is unavailable right now; the search was not run."`
	cases := []struct {
		name, url, token, want string
	}{
		{"allow", reply(200, `{"verdict":"allow"}`), "token", "null"},
		{"confirm", reply(200, `{"verdict":"confirm","text":"ask first"}`), "token", `"ask first"`},
		{"deny", reply(200, `{"verdict":"deny","text":"no"}`), "token", `"no"`},
		{"unknown verdict", reply(200, `{"verdict":"maybe","text":"x"}`), "token", unavailable},
		{"confirm without text", reply(200, `{"verdict":"confirm"}`), "token", unavailable},
		{"malformed", reply(200, `nope`), "token", unavailable},
		{"server error", reply(500, ``), "token", unavailable},
		{"wrong token", reply(200, `{"verdict":"allow"}`), "other", unavailable},
		{"unreachable", "http://127.0.0.1:1", "token", unavailable},
		{"no endpoint", "", "token", unavailable},
		{"no token", reply(200, `{"verdict":"allow"}`), "", unavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			command := exec.Command(node, script)
			command.Env = append(os.Environ(), "HOME_AGENT_SEARCH_CHECK_URL="+c.url, "HOME_AGENT_TOOL_BRIDGE_TOKEN="+c.token)
			output, err := command.Output()
			if err != nil {
				t.Fatalf("node: %v", err)
			}
			if string(output) != c.want {
				t.Errorf("refusal = %s, want %s", output, c.want)
			}
		})
	}
}

func postSearchCheck(t *testing.T, url, authorization, body string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

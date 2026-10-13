package pi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	bridgeExtensionFile   = "bridge.ts"
	bridgeToolsFile       = "tools.json"
	bridgePath            = "/call"
	searchCheckPath       = "/search-check"
	maximumArgumentBytes  = 64 << 10
	bridgeCallFailedText  = "The tool call could not be executed."
	searchUnavailableText = "Web search is unavailable right now; the search was not run."
	emptyQueryText        = "The search query was empty, so nothing was searched; search again with the words to look for."
	bridgeURLVariable     = "HOME_AGENT_TOOL_BRIDGE_URL"
	bridgeTokenVariable   = "HOME_AGENT_TOOL_BRIDGE_TOKEN"
	bridgeToolsVariable   = "HOME_AGENT_TOOL_BRIDGE_TOOLS"
	searchCheckVariable   = "HOME_AGENT_SEARCH_CHECK_URL"
	bridgeShutdownTimeout = time.Second
)

//go:embed bridge.ts
var bridgeExtension []byte

// bridge is one turn's tool transport: a private directory holding the extension and the tool
// definitions, and a loopback listener that accepts calls carrying the turn's bearer token.
type bridge struct {
	directory string
	extension string
	toolsPath string
	token     string
	url       string
	checkURL  string
	executor  agentruntime.ToolExecutor
	check     agentruntime.SearchCheck
	server    *http.Server
	cancel    context.CancelFunc
	closed    chan struct{}
}

type bridgeTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func startBridge(ctx context.Context, agentDir string, tools agentruntime.ToolSet, check agentruntime.SearchCheck) (*bridge, error) {
	directory, err := os.MkdirTemp(agentDir, "bridge-")
	if err != nil {
		return nil, fmt.Errorf("create Pi tool bridge directory: %w", err)
	}
	b := &bridge{
		directory: directory,
		extension: filepath.Join(directory, bridgeExtensionFile),
		toolsPath: filepath.Join(directory, bridgeToolsFile),
		executor:  tools.Executor,
		check:     check,
		closed:    make(chan struct{}),
	}
	if err := b.writeFiles(tools.Definitions); err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	token, err := newBridgeToken()
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	b.token = token

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("listen for Pi tool bridge: %w", err)
	}
	b.url = "http://" + listener.Addr().String() + bridgePath
	b.checkURL = "http://" + listener.Addr().String() + searchCheckPath
	bridgeCtx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.server = &http.Server{
		Handler:           b,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return bridgeCtx },
	}
	go func() {
		defer close(b.closed)
		_ = b.server.Serve(listener)
	}()
	return b, nil
}

func (b *bridge) writeFiles(definitions []agentruntime.ToolDefinition) error {
	listed := make([]bridgeTool, 0, len(definitions))
	for _, definition := range definitions {
		listed = append(listed, bridgeTool{
			Name:        definition.Name,
			Description: definition.Description,
			Parameters:  definition.InputSchema,
		})
	}
	encoded, err := json.Marshal(listed)
	if err != nil {
		return fmt.Errorf("encode Pi tool definitions: %w", err)
	}
	if err := os.WriteFile(b.extension, bridgeExtension, 0o600); err != nil {
		return fmt.Errorf("write Pi tool bridge extension: %w", err)
	}
	if err := os.WriteFile(b.toolsPath, encoded, 0o600); err != nil {
		return fmt.Errorf("write Pi tool definitions: %w", err)
	}
	return nil
}

func (b *bridge) environment() []string {
	return []string{
		bridgeURLVariable + "=" + b.url,
		bridgeTokenVariable + "=" + b.token,
		bridgeToolsVariable + "=" + b.toolsPath,
		searchCheckVariable + "=" + b.checkURL,
	}
}

// close refuses further calls, cancels calls in flight, and removes the turn's files.
func (b *bridge) close() {
	b.cancel()
	_ = b.server.Close()
	select {
	case <-b.closed:
	case <-time.After(bridgeShutdownTimeout):
	}
	_ = os.RemoveAll(b.directory)
}

func (b *bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || (r.URL.Path != bridgePath && r.URL.Path != searchCheckPath) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maximumArgumentBytes+1))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(body) > maximumArgumentBytes {
		http.Error(w, "arguments exceed the size limit", http.StatusRequestEntityTooLarge)
		return
	}
	if r.URL.Path == searchCheckPath {
		b.serveSearchCheck(w, r, body)
		return
	}
	if b.executor == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(body, &call); err != nil || call.Name == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage("{}")
	}

	result, err := b.executor.Execute(r.Context(), agentruntime.ToolCall{Name: call.Name, Arguments: call.Arguments})
	if err != nil {
		result = agentruntime.ToolResult{Text: bridgeCallFailedText, IsError: true}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Text    string `json:"text"`
		IsError bool   `json:"isError"`
	}{result.Text, result.IsError})
}

// serveSearchCheck answers whether the search extension may send a query out. A turn without a
// check refuses every query.
func (b *bridge) serveSearchCheck(w http.ResponseWriter, r *http.Request, body []byte) {
	var search struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &search); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	decision := agentruntime.SearchDecision{Verdict: agentruntime.SearchDeny, Reason: "no_check", Text: searchUnavailableText}
	// A blank query is the model's slip, not an outage: the extension reports any failed check as
	// search being unavailable, which the model would pass on to the user.
	if strings.TrimSpace(search.Query) == "" {
		decision = agentruntime.SearchDecision{Verdict: agentruntime.SearchDeny, Reason: "empty_query", Text: emptyQueryText}
	} else if b.check != nil {
		decision = b.check(r.Context(), search.Query)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(decision)
}

func newBridgeToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate Pi tool bridge token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

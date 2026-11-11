// Package mcp is a client for Model Context Protocol servers on the streamable HTTP transport. It
// speaks the revisions that open with an initialize handshake and only what tool use needs:
// initialize, tools/list, and tools/call. It asks the server for nothing: it advertises no client
// capabilities and declines every request the server sends back.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

// LatestRevision is the revision the client asks for in initialize: the newest one that has the
// handshake. Revision 2026-07-28 dropped initialize for per-request versions and is not spoken.
const LatestRevision = "2025-11-25"

const (
	DefaultTimeout = 30 * time.Second
	// MaximumResponseBytes bounds one HTTP answer, a whole event stream included.
	MaximumResponseBytes = 4 << 20
	MaximumPages         = 20
	MaximumTools         = 1000

	maximumSessionIDBytes = 1024
	clientName            = "home-ai-assistant"
	clientVersion         = "1.0.0"
)

// supportedRevisions are accepted in the server's initialize answer. 2024-11-05 predates the
// streamable HTTP transport, but a stdio-to-HTTP proxy relays the initialize answer of the server
// behind it, and initialize, tools/list, and tools/call are unchanged since that revision.
var supportedRevisions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// requiresVersionHeader reports whether the revision requires MCP-Protocol-Version on every
// request after initialize; the header arrived in 2025-06-18, and servers assume 2025-03-26
// without it.
func requiresVersionHeader(revision string) bool { return revision >= "2025-06-18" }

type Options struct {
	// URL is the MCP endpoint, an absolute http or https URL without user info.
	URL string
	// HeaderName and HeaderValue are the credential header, sent as given on every request; both
	// are empty for a server without a credential.
	HeaderName  string
	HeaderValue string
	// Timeout bounds each HTTP exchange; zero means DefaultTimeout.
	Timeout time.Duration
	// Transport replaces the network transport, for tests.
	Transport http.RoundTripper
}

type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema json.RawMessage
}

// Content is one item of a tool result. Text is set only for text items; the other kinds are
// named by Type so the caller can say what it dropped.
type Content struct {
	Type string
	Text string
}

type Result struct {
	Content []Content
	IsError bool
}

// Texts returns the text items in order.
func (r Result) Texts() []string {
	var texts []string
	for _, item := range r.Content {
		if item.Type == "text" {
			texts = append(texts, item.Text)
		}
	}
	return texts
}

// OtherKinds returns the distinct kinds of the items that are not text, in order of appearance.
func (r Result) OtherKinds() []string {
	var kinds []string
	seen := map[string]bool{}
	for _, item := range r.Content {
		if item.Type != "text" && !seen[item.Type] {
			seen[item.Type] = true
			kinds = append(kinds, item.Type)
		}
	}
	return kinds
}

type session struct {
	id       string
	revision string
}

// Client holds one session with one server; it is safe for concurrent calls. The session opens
// on the first call and is renewed once when the server reports it gone.
type Client struct {
	endpoint    string
	headerName  string
	headerValue string
	timeout     time.Duration
	http        *http.Client
	ids         atomic.Int64
	// gate admits one initialization at a time while letting waiters give up with their context.
	gate chan struct{}

	mu      sync.Mutex
	current session
	ready   bool
	closed  bool
}

func New(options Options) (*Client, error) {
	endpoint, err := url.Parse(options.URL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil {
		return nil, ErrInvalidOptions
	}
	if (options.HeaderName == "") != (options.HeaderValue == "") {
		return nil, ErrInvalidOptions
	}
	if options.HeaderName != "" && (!httpguts.ValidHeaderFieldName(options.HeaderName) || !httpguts.ValidHeaderFieldValue(options.HeaderValue)) {
		return nil, ErrInvalidOptions
	}
	// A credential under a header the client sets itself would be overwritten or sent in its place.
	for _, own := range []string{"Accept", "Content-Type", headerSession, headerVersion} {
		if strings.EqualFold(options.HeaderName, own) {
			return nil, ErrInvalidOptions
		}
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	base := options.Transport
	if base == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		base = transport
	}
	return &Client{
		endpoint:    endpoint.String(),
		headerName:  options.HeaderName,
		headerValue: options.HeaderValue,
		timeout:     timeout,
		gate:        make(chan struct{}, 1),
		http: &http.Client{
			Transport: httpretry.New(base, httpretry.Options{Service: "mcp"}),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Initialize opens the session unless one is open. Other calls open it on their own; this is for a
// caller that wants to verify the server and credential first.
func (c *Client) Initialize(ctx context.Context) error {
	_, err := c.establish(ctx)
	return err
}

// ProtocolVersion is the negotiated revision, or empty before the session opens.
func (c *Client) ProtocolVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current.revision
}

// ListTools returns every tool the server lists, following its pagination cursors up to
// MaximumPages pages and MaximumTools tools.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var tools []Tool
	cursor := ""
	for range MaximumPages {
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		var page struct {
			Tools []struct {
				Name        string          `json:"name"`
				Title       string          `json:"title"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &page); err != nil {
			return nil, err
		}
		if len(tools)+len(page.Tools) > MaximumTools {
			return nil, ErrTooLarge
		}
		for _, tool := range page.Tools {
			tools = append(tools, Tool{Name: tool.Name, Title: tool.Title, Description: tool.Description, InputSchema: tool.InputSchema})
		}
		if page.NextCursor == "" {
			return tools, nil
		}
		cursor = page.NextCursor
	}
	return nil, ErrTooLarge
}

// CallTool calls the named tool with arguments, a JSON object or nil for none. A tool that failed
// on its own terms is a Result with IsError, not an error. Structured content is ignored.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (Result, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if name == "" || json.Unmarshal(arguments, &object) != nil || object == nil {
		return Result{}, ErrInvalidArguments
	}
	var answer struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, &answer); err != nil {
		return Result{}, err
	}
	result := Result{IsError: answer.IsError, Content: make([]Content, 0, len(answer.Content))}
	for _, item := range answer.Content {
		switch item.Type {
		case "text":
			result.Content = append(result.Content, Content{Type: "text", Text: item.Text})
		case "":
			result.Content = append(result.Content, Content{Type: "unknown"})
		default:
			result.Content = append(result.Content, Content{Type: item.Type})
		}
	}
	return result, nil
}

// Close ends the session with an HTTP DELETE, as best effort, and refuses later calls.
func (c *Client) Close(ctx context.Context) {
	c.mu.Lock()
	ended, ready := c.current, c.ready
	c.closed, c.ready, c.current = true, false, session{}
	c.mu.Unlock()
	if ready {
		c.end(ctx, ended)
	}
	c.http.CloseIdleConnections()
}

// call runs one request in the session, renewing the session once when the server reports it
// gone and sending the request again in the new one.
func (c *Client) call(ctx context.Context, method string, params, into any) error {
	current, err := c.establish(ctx)
	if err != nil {
		return err
	}
	err = c.request(ctx, current, method, params, into)
	if !errors.Is(err, errSessionGone) {
		return err
	}
	current, err = c.renew(ctx, current)
	if err != nil {
		return err
	}
	err = c.request(ctx, current, method, params, into)
	if errors.Is(err, errSessionGone) {
		return ErrSessionLost
	}
	return err
}

func (c *Client) request(ctx context.Context, current session, method string, params, into any) error {
	raw, _, err := c.exchange(ctx, current, method, params)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return ErrMalformed
	}
	return nil
}

func (c *Client) enter(ctx context.Context) error {
	select {
	case c.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return transportError(ctx.Err())
	}
}

func (c *Client) leave() { <-c.gate }

// establish returns the open session, opening it first when there is none.
func (c *Client) establish(ctx context.Context) (session, error) {
	c.mu.Lock()
	current, ready, closed := c.current, c.ready, c.closed
	c.mu.Unlock()
	if closed {
		return session{}, ErrClosed
	}
	if ready {
		return current, nil
	}
	if err := c.enter(ctx); err != nil {
		return session{}, err
	}
	defer c.leave()
	c.mu.Lock()
	current, ready, closed = c.current, c.ready, c.closed
	c.mu.Unlock()
	if closed {
		return session{}, ErrClosed
	}
	if ready {
		return current, nil
	}
	return c.open(ctx)
}

// renew replaces the lost session, unless a concurrent call already did.
func (c *Client) renew(ctx context.Context, lost session) (session, error) {
	if err := c.enter(ctx); err != nil {
		return session{}, err
	}
	defer c.leave()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return session{}, ErrClosed
	}
	if c.ready && c.current != lost {
		current := c.current
		c.mu.Unlock()
		return current, nil
	}
	c.ready, c.current = false, session{}
	c.mu.Unlock()
	return c.open(ctx)
}

// open runs the handshake and stores the session; the caller holds the gate.
func (c *Client) open(ctx context.Context) (session, error) {
	opened, err := c.initialize(ctx)
	if err != nil {
		return session{}, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.end(ctx, opened)
		return session{}, ErrClosed
	}
	c.current, c.ready = opened, true
	c.mu.Unlock()
	return opened, nil
}

func (c *Client) initialize(ctx context.Context) (session, error) {
	params := map[string]any{
		"protocolVersion": LatestRevision,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": clientName, "version": clientVersion},
	}
	raw, sessionID, err := c.exchange(ctx, session{}, "initialize", params)
	if err != nil {
		return session{}, err
	}
	if !validSessionID(sessionID) {
		return session{}, ErrMalformed
	}
	var answer struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return session{}, ErrMalformed
	}
	opened := session{id: sessionID, revision: answer.ProtocolVersion}
	if !supportedRevisions[answer.ProtocolVersion] {
		c.end(ctx, session{id: sessionID})
		return session{}, ErrUnsupportedVersion
	}
	if err := c.notify(ctx, opened, "notifications/initialized"); err != nil {
		if errors.Is(err, errSessionGone) {
			return session{}, ErrSessionLost
		}
		c.end(ctx, opened)
		return session{}, err
	}
	return opened, nil
}

// validSessionID accepts no ID, for a stateless server, or one of visible ASCII characters as the
// transport requires.
func validSessionID(id string) bool {
	if len(id) > maximumSessionIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

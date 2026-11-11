// Package mcpsim is a loopback stand-in for a Model Context Protocol server on the streamable
// HTTP transport. A test chooses the revision it answers, whether it keeps sessions, its tools and
// call results, how it frames answers, and the failures it injects. Every request it receives is
// recorded so a test can assert what was and was not sent on the wire.
package mcpsim

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	Path = "/mcp"

	SessionHeader = "Mcp-Session-Id"
	VersionHeader = "Mcp-Protocol-Version"

	Revision20251125 = "2025-11-25"
	Revision20250618 = "2025-06-18"
	Revision20250326 = "2025-03-26"
	Revision20241105 = "2024-11-05"

	// ServerErrorText appears in every error body the simulator writes, so a test can check that
	// nothing a server said reaches an error or a log.
	ServerErrorText = "synthetic-server-error-text"
	// Instructions is what initialize answers as the server's instructions.
	Instructions = "synthetic-server-instructions"
)

// Revisions are the revisions the simulator knows, newest first.
var Revisions = []string{Revision20251125, Revision20250618, Revision20250326, Revision20241105}

type Tool struct {
	Name        string
	Title       string
	Description string
	// InputSchema defaults to an empty object schema.
	InputSchema map[string]any
	// RawInputSchema, when set, is sent as the schema instead, so a test can send one that is not
	// an object.
	RawInputSchema json.RawMessage
	Annotations    map[string]any
}

// Content is one item of a tool result, written to the wire as given.
type Content map[string]any

func Text(text string) Content { return Content{"type": "text", "text": text} }

func Image(mimeType string) Content {
	return Content{"type": "image", "data": "iVBORw0KGgo=", "mimeType": mimeType}
}

func Audio(mimeType string) Content {
	return Content{"type": "audio", "data": "UklGRg==", "mimeType": mimeType}
}

func ResourceLink(uri string) Content {
	return Content{"type": "resource_link", "uri": uri, "name": "linked"}
}

func EmbeddedResource(uri, text string) Content {
	return Content{"type": "resource", "resource": map[string]any{"uri": uri, "mimeType": "text/plain", "text": text}}
}

type Result struct {
	Content    []Content
	IsError    bool
	Structured any
}

// Request is one HTTP request the simulator received. Kind is "request", "notification",
// "response", or empty for a request without a JSON-RPC body.
type Request struct {
	HTTPMethod      string
	Header          http.Header
	Kind            string
	RPCMethod       string
	ID              string
	SessionID       string
	ProtocolVersion string
	Params          json.RawMessage
	// ErrorCode is the code of the JSON-RPC error a client sent back, when it sent one.
	ErrorCode int
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error,omitempty"`
}

type Simulator struct {
	server  *httptest.Server
	release chan struct{}

	mu            sync.Mutex
	stateful      bool
	revision      string
	tools         []Tool
	pageSize      int
	results       map[string]Result
	stream        bool
	interleave    bool
	batch         bool
	forget        int
	headerName    string
	headerValues  []string
	down          bool
	redirect      string
	hang          map[string]bool
	held          map[string]*heldMethod
	serverRequest string
	failures      []int
	sessions      map[string]string
	sessionCount  int
	requests      []Request
	replies       map[string]chan Request
	serverCalls   int
}

// New starts a stateless simulator that answers the revision the client asks for when it knows
// it, as JSON, with no tools.
func New(t testing.TB) *Simulator {
	t.Helper()
	s := &Simulator{
		release:  make(chan struct{}),
		results:  map[string]Result{},
		hang:     map[string]bool{},
		held:     map[string]*heldMethod{},
		sessions: map[string]string{},
		replies:  map[string]chan Request{},
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	t.Cleanup(func() { close(s.release) })
	return s
}

// URL is the MCP endpoint.
func (s *Simulator) URL() string { return s.server.URL + Path }

// SetStateful makes initialize issue a session ID and every later request require it.
func (s *Simulator) SetStateful(stateful bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateful = stateful
}

// SetRevision makes initialize answer this revision whatever the client asked; empty restores
// answering the client's revision when the simulator knows it and the newest one otherwise.
func (s *Simulator) SetRevision(revision string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision = revision
}

// SetTools sets the tool list, served pageSize tools at a time; zero serves one page.
func (s *Simulator) SetTools(tools []Tool, pageSize int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = slices.Clone(tools)
	s.pageSize = pageSize
}

// SetResult scripts what tools/call answers for the named tool. A tool without a result is
// answered with a JSON-RPC error.
func (s *Simulator) SetResult(tool string, result Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[tool] = result
}

// SetStream answers requests as event streams instead of single JSON objects.
func (s *Simulator) SetStream(stream bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stream = stream
}

// SetInterleave adds, before each streamed answer, a priming event, a comment, an event of another
// type, a notification, and a response to another request, and spreads the answer over several
// data lines.
func (s *Simulator) SetInterleave(interleave bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interleave = interleave
}

// SetBatch wraps each JSON answer in a one-element array, as revision 2025-03-26 allowed.
func (s *Simulator) SetBatch(batch bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batch = batch
}

// ForgetSessions makes the next count requests other than initialize that carry a session ID
// find it gone: the session is dropped and the request answered 404.
func (s *Simulator) ForgetSessions(count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget = count
}

// RequireHeader answers 401 to every request whose header does not carry exactly one of these
// values.
func (s *Simulator) RequireHeader(name string, values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headerName, s.headerValues = name, slices.Clone(values)
}

// SetDown answers 503 to every request while down is true.
func (s *Simulator) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// RedirectTo answers every request with a temporary redirect to target.
func (s *Simulator) RedirectTo(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redirect = target
}

// HangOn holds requests for the JSON-RPC method open without answering until the client gives
// up; an empty method holds every request. A streamed answer sends its priming event first.
func (s *Simulator) HangOn(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hang[method] = true
}

type heldMethod struct {
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

// HoldOn holds requests for the JSON-RPC method open until release is called or the client gives
// up, then answers them as usual. arrived receives once for each held request, so a test can wait
// for one to be in flight.
func (s *Simulator) HoldOn(method string) (arrived <-chan struct{}, release func()) {
	held := &heldMethod{arrived: make(chan struct{}, 16), release: make(chan struct{})}
	s.mu.Lock()
	s.held[method] = held
	s.mu.Unlock()
	return held.arrived, func() {
		held.once.Do(func() {
			s.mu.Lock()
			if s.held[method] == held {
				delete(s.held, method)
			}
			s.mu.Unlock()
			close(held.release)
		})
	}
}

// wait holds a request for a held method and reports whether to go on answering it.
func (s *Simulator) wait(r *http.Request, method string) bool {
	s.mu.Lock()
	held := s.held[method]
	s.mu.Unlock()
	if held == nil {
		return true
	}
	select {
	case held.arrived <- struct{}{}:
	default:
	}
	select {
	case <-held.release:
		return true
	case <-r.Context().Done():
	case <-s.release:
	}
	return false
}

// SendServerRequest makes every streamed answer to a request other than initialize first send a
// request of this method to the client and wait for the client's reply.
func (s *Simulator) SendServerRequest(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serverRequest = method
}

// FailNext answers the next requests with these statuses, one each, before normal handling.
func (s *Simulator) FailNext(statuses ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, statuses...)
}

// Sessions are the session IDs the simulator currently knows.
func (s *Simulator) Sessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (s *Simulator) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// RequestsFor are the recorded requests with this JSON-RPC method.
func (s *Simulator) RequestsFor(method string) []Request {
	var matching []Request
	for _, request := range s.Requests() {
		if request.RPCMethod == method {
			matching = append(matching, request)
		}
	}
	return matching
}

// Replies are the JSON-RPC responses the client sent to the simulator's own requests.
func (s *Simulator) Replies() []Request {
	var replies []Request
	for _, request := range s.Requests() {
		if request.Kind == "response" {
			replies = append(replies, request)
		}
	}
	return replies
}

func (s *Simulator) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	record := Request{
		HTTPMethod:      r.Method,
		Header:          r.Header.Clone(),
		SessionID:       r.Header.Get(SessionHeader),
		ProtocolVersion: r.Header.Get(VersionHeader),
	}
	var incoming message
	if len(body) > 0 && json.Unmarshal(body, &incoming) == nil {
		record.RPCMethod = incoming.Method
		record.ID = string(incoming.ID)
		record.Params = incoming.Params
		switch {
		case incoming.Method != "" && len(incoming.ID) > 0:
			record.Kind = "request"
		case incoming.Method != "":
			record.Kind = "notification"
		default:
			record.Kind = "response"
			if incoming.Error != nil {
				record.ErrorCode = incoming.Error.Code
			}
		}
	}

	s.mu.Lock()
	s.requests = append(s.requests, record)
	failure := 0
	if len(s.failures) > 0 {
		failure, s.failures = s.failures[0], s.failures[1:]
	}
	redirect, headerName, headerValues, down := s.redirect, s.headerName, s.headerValues, s.down
	s.mu.Unlock()

	switch {
	case r.URL.Path != Path:
		writeError(w, http.StatusNotFound)
	case down:
		writeError(w, http.StatusServiceUnavailable)
	case redirect != "":
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
	case headerName != "" && !slices.Contains(headerValues, r.Header.Get(headerName)):
		writeError(w, http.StatusUnauthorized)
	case failure != 0:
		writeError(w, failure)
	case r.Method == http.MethodDelete:
		s.endSession(w, record.SessionID)
	case r.Method != http.MethodPost:
		writeError(w, http.StatusMethodNotAllowed)
	case !accepts(r.Header.Get("Accept")):
		writeError(w, http.StatusNotAcceptable)
	case record.Kind == "":
		writeError(w, http.StatusBadRequest)
	case record.Kind == "response":
		s.deliver(record)
		w.WriteHeader(http.StatusAccepted)
	case record.RPCMethod == "initialize":
		s.initialize(w, r, incoming)
	default:
		if status := s.checkSession(record); status != 0 {
			writeError(w, status)
			return
		}
		if record.Kind == "notification" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.handle(w, r, incoming, record.SessionID)
	}
}

func accepts(header string) bool {
	return strings.Contains(header, "application/json") && strings.Contains(header, "text/event-stream")
}

func (s *Simulator) endSession(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stateful {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, known := s.sessions[id]; !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	delete(s.sessions, id)
	w.WriteHeader(http.StatusOK)
}

// checkSession applies the session and version-header rules to a request after initialize and
// returns the status to refuse it with, or zero.
func (s *Simulator) checkSession(record Request) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stateful {
		if record.ProtocolVersion != "" && !slices.Contains(Revisions, record.ProtocolVersion) {
			return http.StatusBadRequest
		}
		return 0
	}
	if record.SessionID == "" {
		return http.StatusBadRequest
	}
	negotiated, known := s.sessions[record.SessionID]
	if !known {
		return http.StatusNotFound
	}
	if s.forget > 0 && record.Kind == "request" {
		s.forget--
		delete(s.sessions, record.SessionID)
		return http.StatusNotFound
	}
	if negotiated >= Revision20250618 && record.ProtocolVersion != negotiated {
		return http.StatusBadRequest
	}
	return 0
}

func (s *Simulator) initialize(w http.ResponseWriter, r *http.Request, incoming message) {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(incoming.Params, &params)
	s.mu.Lock()
	answer := s.revision
	if answer == "" {
		answer = Revisions[0]
		if slices.Contains(Revisions, params.ProtocolVersion) {
			answer = params.ProtocolVersion
		}
	}
	if s.stateful {
		s.sessionCount++
		id := fmt.Sprintf("session-%d", s.sessionCount)
		s.sessions[id] = answer
		w.Header().Set(SessionHeader, id)
	}
	hang := s.hang["initialize"] || s.hang[""]
	s.mu.Unlock()
	if hang {
		s.hold(w, r)
		return
	}
	if !s.wait(r, "initialize") {
		return
	}
	s.answer(w, r, incoming.ID, map[string]any{
		"protocolVersion": answer,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "mcpsim", "version": "1.0.0"},
		"instructions":    Instructions,
	}, nil, "")
}

func (s *Simulator) handle(w http.ResponseWriter, r *http.Request, incoming message, session string) {
	s.mu.Lock()
	hang := s.hang[incoming.Method] || s.hang[""]
	serverRequest := s.serverRequest
	s.mu.Unlock()
	if hang {
		s.hold(w, r)
		return
	}
	if !s.wait(r, incoming.Method) {
		return
	}
	switch incoming.Method {
	case "tools/list":
		var params struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(incoming.Params, &params)
		result, ok := s.listPage(params.Cursor)
		if !ok {
			s.answer(w, r, incoming.ID, nil, rpcError(-32602, "Invalid cursor"), serverRequest)
			return
		}
		s.answer(w, r, incoming.ID, result, nil, serverRequest)
	case "tools/call":
		var params struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(incoming.Params, &params)
		s.mu.Lock()
		result, known := s.results[params.Name]
		s.mu.Unlock()
		if !known {
			s.answer(w, r, incoming.ID, nil, rpcError(-32602, "Unknown tool"), serverRequest)
			return
		}
		s.answer(w, r, incoming.ID, callResult(result), nil, serverRequest)
	case "ping":
		s.answer(w, r, incoming.ID, map[string]any{}, nil, serverRequest)
	default:
		s.answer(w, r, incoming.ID, nil, rpcError(-32601, "Method not found"), serverRequest)
	}
}

func (s *Simulator) listPage(cursor string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(strings.TrimPrefix(cursor, "page-"))
		if err != nil || parsed < 0 || parsed > len(s.tools) {
			return nil, false
		}
		start = parsed
	}
	end := len(s.tools)
	if s.pageSize > 0 {
		end = min(end, start+s.pageSize)
	}
	tools := make([]map[string]any, 0, end-start)
	for _, tool := range s.tools[start:end] {
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		entry := map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": schema}
		if tool.RawInputSchema != nil {
			entry["inputSchema"] = tool.RawInputSchema
		}
		if tool.Title != "" {
			entry["title"] = tool.Title
		}
		if tool.Annotations != nil {
			entry["annotations"] = tool.Annotations
		}
		tools = append(tools, entry)
	}
	result := map[string]any{"tools": tools}
	if end < len(s.tools) {
		result["nextCursor"] = fmt.Sprintf("page-%d", end)
	}
	return result, true
}

func callResult(result Result) map[string]any {
	content := make([]map[string]any, 0, len(result.Content))
	for _, item := range result.Content {
		content = append(content, item)
	}
	answer := map[string]any{"content": content, "isError": result.IsError}
	if result.Structured != nil {
		answer["structuredContent"] = result.Structured
	}
	return answer
}

func rpcError(code int, text string) map[string]any {
	return map[string]any{"code": code, "message": text + " " + ServerErrorText}
}

// answer writes the response to the request with id, as JSON or as an event stream.
func (s *Simulator) answer(w http.ResponseWriter, r *http.Request, id json.RawMessage, result, failure map[string]any, serverRequest string) {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if failure != nil {
		response["error"] = failure
	} else {
		response["result"] = result
	}
	s.mu.Lock()
	stream, interleave, batch := s.stream, s.interleave, s.batch
	s.mu.Unlock()

	if !stream {
		var payload any = response
		if batch {
			payload = []any{response}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(text string) {
		_, _ = io.WriteString(w, text)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if interleave {
		write("id: prime-1\ndata: \n\n")
		write(": keep-alive\n\n")
		write("event: other\ndata: {\"jsonrpc\":\"2.0\",\"id\":" + string(id) + ",\"result\":{\"decoy\":true}}\n\n")
		write("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":\"" + ServerErrorText + "\"}}\n\n")
		write("data: {\"jsonrpc\":\"2.0\",\"id\":987654321,\"result\":{\"decoy\":true}}\n\n")
		write("data: not json\n\n")
	}
	if serverRequest != "" {
		reply := s.ask(write, serverRequest)
		select {
		case <-reply:
		case <-r.Context().Done():
			return
		case <-s.release:
			return
		}
	}
	var encoded []byte
	if interleave {
		encoded, _ = json.MarshalIndent(response, "", " ")
	} else {
		encoded, _ = json.Marshal(response)
	}
	var event strings.Builder
	event.WriteString("event: message\r\n")
	for line := range strings.SplitSeq(string(encoded), "\n") {
		event.WriteString("data: " + line + "\n")
	}
	event.WriteString("\n")
	write(event.String())
}

// ask sends a server-initiated request on the stream and returns where the client's reply arrives.
func (s *Simulator) ask(write func(string), method string) chan Request {
	s.mu.Lock()
	s.serverCalls++
	id := fmt.Sprintf("\"server-%d\"", s.serverCalls)
	reply := make(chan Request, 1)
	s.replies[id] = reply
	s.mu.Unlock()
	write("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":" + id + ",\"method\":\"" + method + "\",\"params\":{}}\n\n")
	return reply
}

func (s *Simulator) deliver(record Request) {
	s.mu.Lock()
	reply, waiting := s.replies[record.ID]
	delete(s.replies, record.ID)
	s.mu.Unlock()
	if waiting {
		reply <- record
	}
}

// hold keeps the request open until the client or the test gives up. A streaming simulator sends
// its priming event first, so the client is left waiting inside an open stream.
func (s *Simulator) hold(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "id: prime-1\ndata: \n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	select {
	case <-r.Context().Done():
	case <-s.release:
	}
}

func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"%s"}}`, ServerErrorText)
}

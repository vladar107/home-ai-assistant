package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

const (
	headerSession = "Mcp-Session-Id"
	headerVersion = "Mcp-Protocol-Version"
	acceptBoth    = "application/json, text/event-stream"

	// codeMethodNotFound declines a server request: the client offers no capability to serve it.
	codeMethodNotFound = -32601

	// maximumServerRequests bounds the replies one answer can make the client send.
	maximumServerRequests = 8
)

// envelope is any JSON-RPC message: a request or notification has a method, a response does not.
type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// exchange sends one request and returns its result and the session ID the answer carried.
func (c *Client) exchange(ctx context.Context, current session, method string, params any) (json.RawMessage, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	id := c.ids.Add(1)
	message := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	body, err := json.Marshal(message)
	if err != nil {
		return nil, "", ErrInvalidArguments
	}
	if method != "tools/call" {
		ctx = httpretry.Idempotent(ctx)
	}
	response, err := c.send(ctx, current, http.MethodPost, body)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", statusError(response.StatusCode, current.id)
	}
	sessionID := response.Header.Get(headerSession)
	want := []byte(strconv.FormatInt(id, 10))
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	var result json.RawMessage
	reading := &answer{client: c, ctx: ctx, session: current, want: want}
	switch mediaType {
	case "application/json":
		result, err = reading.readJSON(response.Body)
	case "text/event-stream":
		result, err = reading.readStream(response.Body)
	default:
		err = ErrMalformed
	}
	return result, sessionID, err
}

// notify sends a notification; the server accepts it with 202 and no body.
func (c *Client) notify(ctx context.Context, current session, method string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"jsonrpc": "2.0", "method": method})
	response, err := c.send(httpretry.Idempotent(ctx), current, http.MethodPost, body)
	if err != nil {
		return err
	}
	discard(response)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return statusError(response.StatusCode, current.id)
	}
	return nil
}

// end asks the server to drop the session; a server may refuse with 405, and any failure is
// ignored because the session expires on its own.
func (c *Client) end(ctx context.Context, ended session) {
	if ended.id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.send(ctx, ended, http.MethodDelete, nil)
	if err == nil {
		discard(response)
	}
}

// reply answers a request the server sent in a stream: a ping with an empty result, as the
// protocol requires of every party, and anything else with an error.
func (c *Client) reply(ctx context.Context, current session, id json.RawMessage, method string) {
	message := map[string]any{"jsonrpc": "2.0", "id": id}
	if method == "ping" {
		message["result"] = map[string]any{}
	} else {
		message["error"] = map[string]any{"code": codeMethodNotFound, "message": "The client offers no capabilities."}
	}
	body, err := json.Marshal(message)
	if err != nil {
		return
	}
	response, err := c.send(ctx, current, http.MethodPost, body)
	if err == nil {
		discard(response)
	}
}

func (c *Client) send(ctx context.Context, current session, method string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint, reader)
	if err != nil {
		return nil, ErrInvalidOptions
	}
	request.Header.Set("Accept", acceptBoth)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.headerName != "" {
		request.Header.Set(c.headerName, c.headerValue)
	}
	if current.id != "" {
		request.Header.Set(headerSession, current.id)
	}
	if requiresVersionHeader(current.revision) {
		request.Header.Set(headerVersion, current.revision)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, transportError(err)
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		discard(response)
		return nil, ErrRedirect
	}
	return response, nil
}

func discard(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
}

// answer is the reading of one exchange's answer: the response it waits for and how many server
// requests it has replied to on the way.
type answer struct {
	client  *Client
	ctx     context.Context
	session session
	want    []byte
	replies int
}

func (a *answer) readJSON(body io.Reader) (json.RawMessage, error) {
	payload, err := io.ReadAll(io.LimitReader(body, MaximumResponseBytes+1))
	if err != nil {
		return nil, transportError(err)
	}
	if len(payload) > MaximumResponseBytes {
		return nil, ErrTooLarge
	}
	result, found, err := a.consume(payload)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrMalformed
	}
	return result, nil
}

// readStream reads server-sent events until the one that answers the request. It skips events of
// other types, comments, notifications, answers to other requests, and data that is not JSON-RPC,
// and replies to requests from the server on the way.
func (a *answer) readStream(body io.Reader) (json.RawMessage, error) {
	limited := &io.LimitedReader{R: body, N: MaximumResponseBytes + 1}
	reader := bufio.NewReader(limited)
	var data []byte
	var event string
	hasData := false
	first := true
	for {
		line, readErr := reader.ReadBytes('\n')
		if first {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
			first = false
		}
		if len(line) > 0 {
			line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch {
			case len(line) == 0:
				if hasData && (event == "" || event == "message") {
					result, found, err := a.consume(data)
					if err != nil || found {
						return result, err
					}
				}
				data, event, hasData = data[:0], "", false
			case len(field) == 0:
			case string(field) == "event":
				event = string(value)
			case string(field) == "data":
				if hasData {
					data = append(data, '\n')
				}
				data = append(data, value...)
				hasData = true
			}
		}
		if readErr != nil {
			switch {
			case limited.N <= 0:
				return nil, ErrTooLarge
			case errors.Is(readErr, io.EOF):
				// The stream ended without the answer; resuming it is not supported.
				return nil, ErrUnavailable
			default:
				return nil, transportError(readErr)
			}
		}
	}
}

// consume looks through one JSON-RPC message or batch for the response with the wanted ID.
func (a *answer) consume(payload []byte) (json.RawMessage, bool, error) {
	payload = bytes.TrimSpace(payload)
	messages := []json.RawMessage{payload}
	if len(payload) > 0 && payload[0] == '[' {
		if err := json.Unmarshal(payload, &messages); err != nil {
			return nil, false, nil
		}
	}
	for _, raw := range messages {
		var message envelope
		if err := json.Unmarshal(raw, &message); err != nil {
			continue
		}
		id := bytes.TrimSpace(message.ID)
		hasID := len(id) > 0 && !bytes.Equal(id, []byte("null"))
		switch {
		case message.Method != "" && hasID:
			if a.replies == maximumServerRequests {
				return nil, true, ErrProtocol
			}
			a.replies++
			a.client.reply(a.ctx, a.session, id, message.Method)
		case message.Method != "":
		case bytes.Equal(id, a.want):
			if message.Error != nil {
				return nil, true, ErrProtocol
			}
			if len(message.Result) == 0 || bytes.Equal(message.Result, []byte("null")) {
				return nil, true, ErrMalformed
			}
			return message.Result, true, nil
		}
	}
	return nil, false, nil
}

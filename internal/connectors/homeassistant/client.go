package homeassistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

const (
	requestTimeout  = 10 * time.Second
	maximumAPIBytes = 4 << 20

	mcpPath       = "/api/mcp"
	websocketPath = "/api/websocket"
)

// apiError is a failed Home Assistant request reduced to a class; its text is safe for the model
// and the log, since it never carries the token, the address, or the response body.
type apiError struct {
	class string
	text  string
}

func (e *apiError) Error() string { return e.text }

var (
	errUnauthorized = &apiError{"unauthorized", "Home Assistant refused the household token; the operator needs to check it."}
	errNotFound     = &apiError{"not_found", "Home Assistant does not know that entity, service, or tool."}
	errBadRequest   = &apiError{"bad_request", "Home Assistant could not carry out that request."}
	errUnavailable  = &apiError{"unavailable", "Home Assistant is unavailable right now."}
	errTimeout      = &apiError{"timeout", "Home Assistant did not respond in time."}
	errMalformed    = &apiError{"unexpected_response", "Home Assistant returned an unexpected response."}
)

type client struct {
	http  *http.Client
	base  *url.URL
	token string
}

func newClient(base *url.URL, token string) *client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &client{
		base:  base,
		token: token,
		http: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// do sends one request under the fixed base and returns the body of a 200 answer.
func (c *client) do(ctx context.Context, method, path string, body any, accept string) ([]byte, string, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, "", errMalformed
		}
		reader = bytes.NewReader(encoded)
	}
	target := *c.base
	target.Path = path
	request, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, "", errMalformed
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", accept)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, "", transportError(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maximumAPIBytes+1))
	if err != nil {
		return nil, "", transportError(err)
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return nil, "", statusError(response.StatusCode)
	}
	if len(payload) > maximumAPIBytes {
		return nil, "", errMalformed
	}
	return payload, response.Header.Get("Content-Type"), nil
}

func (c *client) getJSON(ctx context.Context, path string, into any) error {
	payload, _, err := c.do(ctx, http.MethodGet, path, nil, "application/json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return errMalformed
	}
	return nil
}

func (c *client) postJSON(ctx context.Context, path string, body, into any) error {
	payload, _, err := c.do(ctx, http.MethodPost, path, body, "application/json")
	if err != nil {
		return err
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return errMalformed
	}
	return nil
}

// mcp sends one JSON-RPC request to the MCP server. Home Assistant's server keeps no session, so
// every request stands alone; it may answer as JSON or as a one-event stream.
func (c *client) mcp(ctx context.Context, method string, params any, into any) error {
	request := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		request["params"] = params
	}
	payload, contentType, err := c.do(ctx, http.MethodPost, mcpPath, request, "application/json, text/event-stream")
	if err != nil {
		return err
	}
	if mediaType, _, _ := mime.ParseMediaType(contentType); mediaType == "text/event-stream" {
		payload = lastEventData(payload)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return errMalformed
	}
	if envelope.Error != nil {
		if envelope.Error.Code == -32601 {
			return errNotFound
		}
		return errBadRequest
	}
	if err := json.Unmarshal(envelope.Result, into); err != nil {
		return errMalformed
	}
	return nil
}

func lastEventData(stream []byte) []byte {
	var data []byte
	scanner := bufio.NewScanner(bytes.NewReader(stream))
	scanner.Buffer(make([]byte, 0, 64<<10), maximumAPIBytes)
	for scanner.Scan() {
		if line, found := strings.CutPrefix(scanner.Text(), "data:"); found {
			data = []byte(strings.TrimSpace(line))
		}
	}
	return data
}

// websocketCommands authenticates one short-lived WebSocket connection and runs the commands in
// order, returning each result. Registries and statistics exist only on this API.
func (c *client) websocketCommands(ctx context.Context, commands ...map[string]any) ([]json.RawMessage, error) {
	target := *c.base
	target.Scheme = map[string]string{"https": "wss", "http": "ws"}[c.base.Scheme]
	target.Path = websocketPath
	config, err := websocket.NewConfig(target.String(), c.base.String())
	if err != nil {
		return nil, errMalformed
	}
	deadline := time.Now().Add(requestTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	config.Dialer = &net.Dialer{Deadline: deadline}
	conn, err := config.DialContext(ctx)
	if err != nil {
		return nil, transportError(err)
	}
	defer conn.Close()
	conn.MaxPayloadBytes = maximumAPIBytes
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, errUnavailable
	}

	var message struct {
		Type    string          `json:"type"`
		ID      int             `json:"id"`
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := websocket.JSON.Receive(conn, &message); err != nil || message.Type != "auth_required" {
		return nil, websocketError(err)
	}
	if err := websocket.JSON.Send(conn, map[string]string{"type": "auth", "access_token": c.token}); err != nil {
		return nil, websocketError(err)
	}
	if err := websocket.JSON.Receive(conn, &message); err != nil {
		return nil, websocketError(err)
	}
	if message.Type != "auth_ok" {
		return nil, errUnauthorized
	}

	results := make([]json.RawMessage, 0, len(commands))
	for index, command := range commands {
		id := index + 1
		framed := map[string]any{"id": id}
		for key, value := range command {
			framed[key] = value
		}
		if err := websocket.JSON.Send(conn, framed); err != nil {
			return nil, websocketError(err)
		}
		for {
			message.ID, message.Success, message.Result, message.Error = 0, false, nil, nil
			if err := websocket.JSON.Receive(conn, &message); err != nil {
				return nil, websocketError(err)
			}
			if message.Type == "result" && message.ID == id {
				break
			}
		}
		if !message.Success {
			if message.Error != nil && message.Error.Code == "unauthorized" {
				return nil, errUnauthorized
			}
			return nil, errBadRequest
		}
		results = append(results, message.Result)
	}
	return results, nil
}

func websocketError(err error) error {
	if err == nil {
		return errMalformed
	}
	return transportError(err)
}

func statusError(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errUnauthorized
	case status == http.StatusNotFound:
		return errNotFound
	case status == http.StatusBadRequest:
		return errBadRequest
	case status >= 500:
		return errUnavailable
	default:
		return errMalformed
	}
}

func transportError(err error) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return errTimeout
	}
	return errUnavailable
}

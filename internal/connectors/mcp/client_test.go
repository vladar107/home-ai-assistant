package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/mcp"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/testutil/mcpsim"
)

const credential = "Bearer test-key-0123456789abcdef"

func newClient(t *testing.T, sim *mcpsim.Simulator, options mcp.Options) *mcp.Client {
	t.Helper()
	options.URL = sim.URL()
	client, err := mcp.New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(context.Background()) })
	return client
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func weatherTools() []mcpsim.Tool {
	return []mcpsim.Tool{
		{Name: "forecast", Title: "Forecast", Description: "Forecast for a place.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"place": map[string]any{"type": "string"}},
		}},
		{Name: "alerts", Description: "Weather alerts.", Annotations: map[string]any{"readOnlyHint": true}},
	}
}

func rpcMethods(requests []mcpsim.Request) []string {
	var methods []string
	for _, request := range requests {
		name := request.RPCMethod
		if request.Kind == "response" {
			name = "(response)"
		}
		if name == "" {
			name = "(" + request.HTTPMethod + ")"
		}
		methods = append(methods, name)
	}
	return methods
}

func TestStatelessServer(t *testing.T) {
	sim := mcpsim.New(t)
	sim.SetTools(weatherTools(), 0)
	sim.SetResult("forecast", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text("Sunny")}})
	sim.RequireHeader("Authorization", credential)
	client := newClient(t, sim, mcp.Options{HeaderName: "Authorization", HeaderValue: credential})

	tools, err := client.ListTools(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "forecast" || tools[0].Title != "Forecast" || tools[1].Description != "Weather alerts." {
		t.Fatalf("tools %+v", tools)
	}
	var schema map[string]any
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil || schema["type"] != "object" {
		t.Fatalf("schema %s", tools[0].InputSchema)
	}
	result, err := client.CallTool(testContext(t), "forecast", json.RawMessage(`{"place":"Springfield"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Texts(); len(got) != 1 || got[0] != "Sunny" || result.IsError {
		t.Fatalf("result %+v", result)
	}

	requests := sim.Requests()
	if got := rpcMethods(requests); !slices.Equal(got, []string{"initialize", "notifications/initialized", "tools/list", "tools/call"}) {
		t.Fatalf("methods %v", got)
	}
	initialize := requests[0]
	if initialize.ProtocolVersion != "" || initialize.SessionID != "" {
		t.Fatalf("initialize carried version %q session %q", initialize.ProtocolVersion, initialize.SessionID)
	}
	var params struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(initialize.Params, &params); err != nil || params.ProtocolVersion != mcp.LatestRevision || params.Capabilities == nil || len(params.Capabilities) != 0 {
		t.Fatalf("initialize params %s", initialize.Params)
	}
	for _, request := range requests {
		if request.SessionID != "" {
			t.Fatalf("%s carried a session to a stateless server", request.RPCMethod)
		}
		if request.Header.Get("Authorization") != credential {
			t.Fatalf("%s lacked the credential", request.RPCMethod)
		}
		if accept := request.Header.Get("Accept"); !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			t.Fatalf("%s accepted %q", request.RPCMethod, accept)
		}
	}
	for _, request := range requests[1:] {
		if request.ProtocolVersion != mcp.LatestRevision {
			t.Fatalf("%s carried version %q", request.RPCMethod, request.ProtocolVersion)
		}
	}
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(requests[3].Params, &call); err != nil || call.Name != "forecast" || call.Arguments["place"] != "Springfield" {
		t.Fatalf("call params %s", requests[3].Params)
	}
}

func TestStatefulServerKeepsTheSession(t *testing.T) {
	sim := mcpsim.New(t)
	sim.SetStateful(true)
	sim.SetTools(weatherTools(), 0)
	sim.SetResult("alerts", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text("None")}})
	client := newClient(t, sim, mcp.Options{})

	if _, err := client.ListTools(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CallTool(testContext(t), "alerts", nil); err != nil {
		t.Fatal(err)
	}
	requests := sim.Requests()
	if requests[0].SessionID != "" {
		t.Fatalf("initialize carried session %q", requests[0].SessionID)
	}
	for _, request := range requests[1:] {
		if request.SessionID != "session-1" {
			t.Fatalf("%s carried session %q", request.RPCMethod, request.SessionID)
		}
	}
	if got := sim.RequestsFor("initialize"); len(got) != 1 {
		t.Fatalf("initialized %d times", len(got))
	}
	var call struct {
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(requests[3].Params, &call); err != nil || call.Arguments == nil || len(call.Arguments) != 0 {
		t.Fatalf("call params %s", requests[3].Params)
	}
}

func TestEachSupportedRevision(t *testing.T) {
	for _, revision := range []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"} {
		t.Run(revision, func(t *testing.T) {
			sim := mcpsim.New(t)
			sim.SetStateful(true)
			sim.SetRevision(revision)
			sim.SetTools(weatherTools(), 1)
			client := newClient(t, sim, mcp.Options{})

			tools, err := client.ListTools(testContext(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(tools) != 2 || client.ProtocolVersion() != revision {
				t.Fatalf("%d tools at %q", len(tools), client.ProtocolVersion())
			}
			wantHeader := ""
			if revision >= "2025-06-18" {
				wantHeader = revision
			}
			for _, request := range sim.Requests()[1:] {
				if request.ProtocolVersion != wantHeader {
					t.Fatalf("%s carried version %q, want %q", request.RPCMethod, request.ProtocolVersion, wantHeader)
				}
			}
		})
	}
}

func TestUnsupportedRevisionIsRefused(t *testing.T) {
	for _, revision := range []string{"2026-07-28", "2024-10-07", "2099-01-01"} {
		t.Run(revision, func(t *testing.T) {
			sim := mcpsim.New(t)
			sim.SetStateful(true)
			sim.SetRevision(revision)
			sim.SetTools(weatherTools(), 0)
			client := newClient(t, sim, mcp.Options{})

			if _, err := client.ListTools(testContext(t)); !errors.Is(err, mcp.ErrUnsupportedVersion) {
				t.Fatalf("got %v", err)
			}
			if got := rpcMethods(sim.Requests()); !slices.Equal(got, []string{"initialize", "(DELETE)"}) {
				t.Fatalf("methods %v", got)
			}
			if len(sim.Sessions()) != 0 {
				t.Fatalf("sessions left open: %v", sim.Sessions())
			}
		})
	}
}

func TestSessionExpiryRecoversOnce(t *testing.T) {
	sim := mcpsim.New(t)
	sim.SetStateful(true)
	sim.SetResult("forecast", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text("Rain")}})
	client := newClient(t, sim, mcp.Options{})
	if err := client.Initialize(testContext(t)); err != nil {
		t.Fatal(err)
	}

	sim.ForgetSessions(1)
	result, err := client.CallTool(testContext(t), "forecast", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Texts(); len(got) != 1 || got[0] != "Rain" {
		t.Fatalf("result %+v", result)
	}
	calls := sim.RequestsFor("tools/call")
	if len(calls) != 2 || calls[0].SessionID != "session-1" || calls[1].SessionID != "session-2" {
		t.Fatalf("calls %+v", calls)
	}
	initializes := sim.RequestsFor("initialize")
	if len(initializes) != 2 || initializes[1].SessionID != "" {
		t.Fatalf("initializes %+v", initializes)
	}

	if _, err := client.CallTool(testContext(t), "forecast", nil); err != nil {
		t.Fatal(err)
	}
	if got := len(sim.RequestsFor("initialize")); got != 2 {
		t.Fatalf("initialized %d times", got)
	}
}

func TestSecondSessionLossFails(t *testing.T) {
	sim := mcpsim.New(t)
	sim.SetStateful(true)
	sim.SetResult("forecast", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text("Rain")}})
	client := newClient(t, sim, mcp.Options{})
	if err := client.Initialize(testContext(t)); err != nil {
		t.Fatal(err)
	}

	sim.ForgetSessions(2)
	if _, err := client.CallTool(testContext(t), "forecast", nil); !errors.Is(err, mcp.ErrSessionLost) {
		t.Fatalf("got %v", err)
	}
	if got := len(sim.RequestsFor("initialize")); got != 2 {
		t.Fatalf("initialized %d times", got)
	}
	if got := len(sim.RequestsFor("tools/call")); got != 2 {
		t.Fatalf("called %d times", got)
	}

	if _, err := client.CallTool(testContext(t), "forecast", nil); err != nil {
		t.Fatalf("the next call did not open a new session: %v", err)
	}
}

func TestNotFoundWithoutSessionIsNotRenewed(t *testing.T) {
	sim := mcpsim.New(t)
	client := newClient(t, sim, mcp.Options{})
	if err := client.Initialize(testContext(t)); err != nil {
		t.Fatal(err)
	}
	sim.FailNext(http.StatusNotFound)
	if _, err := client.CallTool(testContext(t), "forecast", nil); !errors.Is(err, mcp.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if got := len(sim.RequestsFor("initialize")); got != 1 {
		t.Fatalf("initialized %d times", got)
	}
}

func TestPaginatedToolsList(t *testing.T) {
	sim := mcpsim.New(t)
	var tools []mcpsim.Tool
	for i := range 5 {
		tools = append(tools, mcpsim.Tool{Name: fmt.Sprintf("tool_%d", i), Description: "A tool."})
	}
	sim.SetTools(tools, 2)
	client := newClient(t, sim, mcp.Options{})

	listed, err := client.ListTools(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, []string{"tool_0", "tool_1", "tool_2", "tool_3", "tool_4"}) {
		t.Fatalf("names %v", names)
	}
	var cursors []string
	for _, request := range sim.RequestsFor("tools/list") {
		var params struct {
			Cursor string `json:"cursor"`
		}
		if len(request.Params) > 0 {
			_ = json.Unmarshal(request.Params, &params)
		}
		cursors = append(cursors, params.Cursor)
	}
	if !slices.Equal(cursors, []string{"", "page-2", "page-4"}) {
		t.Fatalf("cursors %v", cursors)
	}
}

func TestToolsListIsBounded(t *testing.T) {
	sim := mcpsim.New(t)
	var tools []mcpsim.Tool
	for i := range mcp.MaximumPages + 1 {
		tools = append(tools, mcpsim.Tool{Name: fmt.Sprintf("tool_%d", i)})
	}
	sim.SetTools(tools, 1)
	client := newClient(t, sim, mcp.Options{})
	if _, err := client.ListTools(testContext(t)); !errors.Is(err, mcp.ErrTooLarge) {
		t.Fatalf("pages: got %v", err)
	}
	if got := len(sim.RequestsFor("tools/list")); got != mcp.MaximumPages {
		t.Fatalf("fetched %d pages", got)
	}

	tools = tools[:0]
	for i := range mcp.MaximumTools + 1 {
		tools = append(tools, mcpsim.Tool{Name: fmt.Sprintf("tool_%d", i)})
	}
	sim.SetTools(tools, 0)
	if _, err := client.ListTools(testContext(t)); !errors.Is(err, mcp.ErrTooLarge) {
		t.Fatalf("tools: got %v", err)
	}
}

func TestAnswerFraming(t *testing.T) {
	cases := []struct {
		name                      string
		stream, interleave, batch bool
	}{
		{name: "json"},
		{name: "json batch", batch: true},
		{name: "stream", stream: true},
		{name: "stream with other events", stream: true, interleave: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			sim := mcpsim.New(t)
			sim.SetStateful(true)
			sim.SetStream(test.stream)
			sim.SetInterleave(test.interleave)
			sim.SetBatch(test.batch)
			sim.SetTools(weatherTools(), 1)
			sim.SetResult("forecast", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text("line one\nline two")}})
			client := newClient(t, sim, mcp.Options{})

			tools, err := client.ListTools(testContext(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(tools) != 2 || tools[0].Name != "forecast" || tools[1].Name != "alerts" {
				t.Fatalf("tools %+v", tools)
			}
			result, err := client.CallTool(testContext(t), "forecast", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Texts(); len(got) != 1 || got[0] != "line one\nline two" {
				t.Fatalf("result %+v", result)
			}
			if replies := sim.Replies(); len(replies) != 0 {
				t.Fatalf("answered %d server messages", len(replies))
			}
		})
	}
}

func TestToolResultContent(t *testing.T) {
	sim := mcpsim.New(t)
	sim.SetResult("lookup", mcpsim.Result{
		Content: []mcpsim.Content{
			mcpsim.Text("first"),
			mcpsim.Image("image/png"),
			mcpsim.Text("second"),
			mcpsim.Audio("audio/wav"),
			mcpsim.Image("image/jpeg"),
			mcpsim.ResourceLink("file:///example.txt"),
			mcpsim.EmbeddedResource("file:///example.txt", "embedded text"),
		},
		Structured: map[string]any{"value": 42},
	})
	sim.SetResult("broken", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text("Invalid date.")}, IsError: true})
	client := newClient(t, sim, mcp.Options{})

	result, err := client.CallTool(testContext(t), "lookup", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Texts(); !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("texts %v", got)
	}
	if got := result.OtherKinds(); !slices.Equal(got, []string{"image", "audio", "resource_link", "resource"}) {
		t.Fatalf("kinds %v", got)
	}
	if result.IsError || len(result.Content) != 7 {
		t.Fatalf("result %+v", result)
	}

	result, err = client.CallTool(testContext(t), "broken", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !slices.Equal(result.Texts(), []string{"Invalid date."}) {
		t.Fatalf("result %+v", result)
	}

	if _, err := client.CallTool(testContext(t), "missing", nil); !errors.Is(err, mcp.ErrProtocol) {
		t.Fatalf("unknown tool: got %v", err)
	}
	for _, arguments := range []string{`[]`, `"text"`, `null`, `{`} {
		if _, err := client.CallTool(testContext(t), "lookup", json.RawMessage(arguments)); !errors.Is(err, mcp.ErrInvalidArguments) {
			t.Fatalf("arguments %s: got %v", arguments, err)
		}
	}
}

func TestOversizedAnswerIsBounded(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			sim := mcpsim.New(t)
			sim.SetStream(stream)
			sim.SetResult("dump", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text(strings.Repeat("x", mcp.MaximumResponseBytes))}})
			client := newClient(t, sim, mcp.Options{})
			if _, err := client.CallTool(testContext(t), "dump", nil); !errors.Is(err, mcp.ErrTooLarge) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestRedirectIsRefused(t *testing.T) {
	target := mcpsim.New(t)
	sim := mcpsim.New(t)
	sim.RedirectTo(target.URL())
	client := newClient(t, sim, mcp.Options{HeaderName: "Authorization", HeaderValue: credential})

	if _, err := client.ListTools(testContext(t)); !errors.Is(err, mcp.ErrRedirect) {
		t.Fatalf("got %v", err)
	}
	if got := target.Requests(); len(got) != 0 {
		t.Fatalf("the redirect target received %d requests", len(got))
	}
}

func TestHangingServerIsBoundedByTheDeadline(t *testing.T) {
	cases := []struct {
		name   string
		stream bool
		method string
	}{
		{name: "initialize", method: "initialize"},
		{name: "call", method: "tools/call"},
		{name: "open stream", method: "tools/call", stream: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			sim := mcpsim.New(t)
			sim.SetStream(test.stream)
			sim.HangOn(test.method)
			client := newClient(t, sim, mcp.Options{})
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			started := time.Now()
			if _, err := client.CallTool(ctx, "forecast", nil); !errors.Is(err, mcp.ErrTimeout) {
				t.Fatalf("got %v", err)
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("took %v", elapsed)
			}
		})
	}

	t.Run("client timeout", func(t *testing.T) {
		sim := mcpsim.New(t)
		sim.HangOn("tools/list")
		client := newClient(t, sim, mcp.Options{Timeout: 200 * time.Millisecond})
		if _, err := client.ListTools(context.Background()); !errors.Is(err, mcp.ErrTimeout) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestServerRequestsAreDeclined(t *testing.T) {
	for _, method := range []string{"sampling/createMessage", "elicitation/create", "roots/list"} {
		t.Run(method, func(t *testing.T) {
			sim := mcpsim.New(t)
			sim.SetStateful(true)
			sim.SetStream(true)
			sim.SendServerRequest(method)
			sim.SetResult("forecast", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text("Fog")}})
			client := newClient(t, sim, mcp.Options{HeaderName: "X-Api-Key", HeaderValue: "test-key-0123456789abcdef"})

			result, err := client.CallTool(testContext(t), "forecast", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Texts(); len(got) != 1 || got[0] != "Fog" {
				t.Fatalf("result %+v", result)
			}
			replies := sim.Replies()
			if len(replies) != 1 {
				t.Fatalf("replies %+v", replies)
			}
			reply := replies[0]
			if reply.ErrorCode != -32601 || reply.ID != `"server-1"` || reply.SessionID != "session-1" || reply.ProtocolVersion != mcp.LatestRevision {
				t.Fatalf("reply %+v", reply)
			}
		})
	}
}

func TestCloseEndsTheSession(t *testing.T) {
	sim := mcpsim.New(t)
	sim.SetStateful(true)
	client := newClient(t, sim, mcp.Options{HeaderName: "Authorization", HeaderValue: credential})
	if err := client.Initialize(testContext(t)); err != nil {
		t.Fatal(err)
	}
	client.Close(testContext(t))

	requests := sim.Requests()
	last := requests[len(requests)-1]
	if last.HTTPMethod != http.MethodDelete || last.SessionID != "session-1" || last.Header.Get("Authorization") != credential {
		t.Fatalf("last request %+v", last)
	}
	if len(sim.Sessions()) != 0 {
		t.Fatalf("sessions left open: %v", sim.Sessions())
	}
	if _, err := client.ListTools(testContext(t)); !errors.Is(err, mcp.ErrClosed) {
		t.Fatalf("after close: got %v", err)
	}
	client.Close(testContext(t))
	if got := len(sim.Requests()); got != len(requests) {
		t.Fatalf("sent %d more requests after close", got-len(requests))
	}

	stateless := mcpsim.New(t)
	other := newClient(t, stateless, mcp.Options{})
	if err := other.Initialize(testContext(t)); err != nil {
		t.Fatal(err)
	}
	other.Close(testContext(t))
	for _, request := range stateless.Requests() {
		if request.HTTPMethod == http.MethodDelete {
			t.Fatal("ended a session the server never issued")
		}
	}
}

func TestStatusesMapToClasses(t *testing.T) {
	httpretry.WithoutWaits(t)
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, mcp.ErrUnauthorized},
		{http.StatusForbidden, mcp.ErrUnauthorized},
		{http.StatusNotFound, mcp.ErrNotFound},
		{http.StatusMethodNotAllowed, mcp.ErrNotFound},
		{http.StatusBadRequest, mcp.ErrProtocol},
		{http.StatusInternalServerError, mcp.ErrUnavailable},
		{http.StatusServiceUnavailable, mcp.ErrUnavailable},
	}
	for _, test := range cases {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			sim := mcpsim.New(t)
			sim.SetResult("forecast", mcpsim.Result{})
			client := newClient(t, sim, mcp.Options{})
			if err := client.Initialize(testContext(t)); err != nil {
				t.Fatal(err)
			}
			sim.FailNext(test.status)
			_, err := client.CallTool(testContext(t), "forecast", nil)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v", err)
			}
			if mcp.ClassOf(err) == "unknown" {
				t.Fatalf("no class for %v", err)
			}
		})
	}
}

func TestWrongCredentialIsUnauthorized(t *testing.T) {
	sim := mcpsim.New(t)
	sim.RequireHeader("Authorization", credential)
	client := newClient(t, sim, mcp.Options{HeaderName: "Authorization", HeaderValue: "Bearer another-key-0123456789"})
	err := client.Initialize(testContext(t))
	if !errors.Is(err, mcp.ErrUnauthorized) || mcp.ClassOf(err) != "unauthorized" {
		t.Fatalf("got %v", err)
	}
}

func TestErrorsCarryNothingSensitive(t *testing.T) {
	httpretry.WithoutWaits(t)
	target := mcpsim.New(t)
	sim := mcpsim.New(t)
	sim.RequireHeader("Authorization", credential)
	client := newClient(t, sim, mcp.Options{HeaderName: "Authorization", HeaderValue: credential})
	host := func(raw string) string {
		parsed, _ := url.Parse(raw)
		return parsed.Host
	}

	var failures []error
	collect := func(_ any, failure error) { failures = append(failures, failure) }
	sim.FailNext(http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError)
	collect(client.ListTools(testContext(t)))
	collect(client.CallTool(testContext(t), "missing", nil))
	sim.FailNext(http.StatusForbidden)
	collect(client.CallTool(testContext(t), "missing", nil))
	sim.RedirectTo(target.URL())
	collect(client.CallTool(testContext(t), "missing", nil))

	for _, failure := range failures {
		if failure == nil {
			t.Fatal("expected a failure")
		}
		text := failure.Error()
		for _, secret := range []string{credential, "test-key", host(sim.URL()), mcpsim.Path, mcpsim.ServerErrorText, "Unknown tool"} {
			if strings.Contains(text, secret) {
				t.Fatalf("error %q contains %q", text, secret)
			}
		}
	}
}

func TestConcurrentCallsShareOneSession(t *testing.T) {
	sim := mcpsim.New(t)
	sim.SetStateful(true)
	sim.SetStream(true)
	sim.SetResult("forecast", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text("Clear")}})
	client := newClient(t, sim, mcp.Options{})

	var wait sync.WaitGroup
	failures := make(chan error, 16)
	for range 16 {
		wait.Go(func() {
			result, err := client.CallTool(testContext(t), "forecast", nil)
			if err == nil && !slices.Equal(result.Texts(), []string{"Clear"}) {
				err = fmt.Errorf("result %+v", result)
			}
			failures <- err
		})
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		if failure != nil {
			t.Fatal(failure)
		}
	}
	if got := len(sim.RequestsFor("initialize")); got != 1 {
		t.Fatalf("initialized %d times", got)
	}

	sim.ForgetSessions(1)
	failures = make(chan error, 8)
	for range 8 {
		wait.Go(func() {
			_, err := client.CallTool(testContext(t), "forecast", nil)
			failures <- err
		})
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		if failure != nil {
			t.Fatal(failure)
		}
	}
	if got := len(sim.RequestsFor("initialize")); got != 2 {
		t.Fatalf("initialized %d times after one loss", got)
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	cases := []mcp.Options{
		{URL: ""},
		{URL: "ftp://example.test/mcp"},
		{URL: "/mcp"},
		{URL: "https://user:secret@example.test/mcp"},
		{URL: "https://example.test/mcp", HeaderName: "Authorization"},
		{URL: "https://example.test/mcp", HeaderValue: "Bearer key"},
		{URL: "https://example.test/mcp", HeaderName: "Bad Name", HeaderValue: "key"},
		{URL: "https://example.test/mcp", HeaderName: "Authorization", HeaderValue: "Bearer key\r\nX-Other: 1"},
	}
	for _, options := range cases {
		if _, failure := mcp.New(options); !errors.Is(failure, mcp.ErrInvalidOptions) {
			t.Fatalf("%+v: got %v", options, failure)
		}
	}
	if _, failure := mcp.New(mcp.Options{URL: "http://127.0.0.1:1/mcp"}); failure != nil {
		t.Fatal(failure)
	}
}

func TestServerPingIsAnswered(t *testing.T) {
	sim := mcpsim.New(t)
	sim.SetStream(true)
	sim.SendServerRequest("ping")
	sim.SetResult("forecast", mcpsim.Result{Content: []mcpsim.Content{mcpsim.Text("Fog")}})
	client := newClient(t, sim, mcp.Options{})

	if _, err := client.CallTool(testContext(t), "forecast", nil); err != nil {
		t.Fatal(err)
	}
	replies := sim.Replies()
	if len(replies) != 1 || replies[0].ErrorCode != 0 || replies[0].ID != `"server-1"` {
		t.Fatalf("replies %+v", replies)
	}
}

// streamServer answers the handshake itself and every other request with the event stream the
// test builds for that request's ID; it counts the replies the client posts back.
func streamServer(t *testing.T, stream func(id string) string) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	replies := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &message)
		switch {
		case message.Method == "":
			mu.Lock()
			replies++
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		case message.Method == "initialize":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{}}}`, message.ID, mcp.LatestRevision)
		case len(message.ID) == 0:
			w.WriteHeader(http.StatusAccepted)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, stream(string(message.ID)))
		}
	}))
	t.Cleanup(server.Close)
	return server, func() int {
		mu.Lock()
		defer mu.Unlock()
		return replies
	}
}

func TestServerRequestsPerAnswerAreBounded(t *testing.T) {
	server, replies := streamServer(t, func(id string) string {
		var stream strings.Builder
		for index := range 20 {
			fmt.Fprintf(&stream, "data: {\"jsonrpc\":\"2.0\",\"id\":\"s%d\",\"method\":\"roots/list\"}\n\n", index)
		}
		fmt.Fprintf(&stream, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[]}}\n\n", id)
		return stream.String()
	})
	client, err := mcp.New(mcp.Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(context.Background()) })

	if _, err := client.ListTools(testContext(t)); !errors.Is(err, mcp.ErrProtocol) {
		t.Fatalf("got %v", err)
	}
	if got := replies(); got != 8 {
		t.Fatalf("client sent %d replies", got)
	}
}

func TestStreamMayStartWithByteOrderMark(t *testing.T) {
	server, _ := streamServer(t, func(id string) string {
		return "\xef\xbb\xbfdata: {\"jsonrpc\":\"2.0\",\"id\":" + id + ",\"result\":{\"tools\":[{\"name\":\"forecast\"}]}}\n\n"
	})
	client, err := mcp.New(mcp.Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(context.Background()) })

	listed, err := client.ListTools(testContext(t))
	if err != nil || len(listed) != 1 || listed[0].Name != "forecast" {
		t.Fatalf("got %+v, %v", listed, err)
	}
}

func TestCancelledCallIsNotAServerFailure(t *testing.T) {
	sim := mcpsim.New(t)
	client := newClient(t, sim, mcp.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.ListTools(ctx); !errors.Is(err, mcp.ErrCancelled) {
		t.Fatalf("got %v", err)
	}
}

func TestCredentialCannotTakeTheClientsOwnHeader(t *testing.T) {
	for _, name := range []string{"Accept", "content-type", "MCP-Session-Id", "Mcp-Protocol-Version"} {
		if _, err := mcp.New(mcp.Options{URL: "https://mcp.example.com/mcp", HeaderName: name, HeaderValue: "test-key-0123456789abcdef"}); !errors.Is(err, mcp.ErrInvalidOptions) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

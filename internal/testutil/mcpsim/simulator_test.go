package mcpsim_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/testutil/mcpsim"
)

func post(t *testing.T, sim *mcpsim.Simulator, body string, header map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, sim.URL(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	for name, value := range header {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestStatefulSimulatorEnforcesTheTransportRules(t *testing.T) {
	sim := mcpsim.New(t)
	sim.SetStateful(true)
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`

	response := post(t, sim, initialize, nil)
	session := response.Header.Get(mcpsim.SessionHeader)
	if response.StatusCode != http.StatusOK || session == "" {
		t.Fatalf("initialize answered %d with session %q", response.StatusCode, session)
	}
	cases := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"no session", map[string]string{mcpsim.VersionHeader: "2025-06-18"}, http.StatusBadRequest},
		{"unknown session", map[string]string{mcpsim.SessionHeader: "session-0", mcpsim.VersionHeader: "2025-06-18"}, http.StatusNotFound},
		{"no version header", map[string]string{mcpsim.SessionHeader: session}, http.StatusBadRequest},
		{"other version header", map[string]string{mcpsim.SessionHeader: session, mcpsim.VersionHeader: "2025-11-25"}, http.StatusBadRequest},
		{"no event stream accepted", map[string]string{mcpsim.SessionHeader: session, mcpsim.VersionHeader: "2025-06-18", "Accept": "application/json"}, http.StatusNotAcceptable},
		{"valid", map[string]string{mcpsim.SessionHeader: session, mcpsim.VersionHeader: "2025-06-18"}, http.StatusOK},
	}
	for _, test := range cases {
		if got := post(t, sim, list, test.header).StatusCode; got != test.want {
			t.Fatalf("%s: answered %d, want %d", test.name, got, test.want)
		}
	}
	if got := len(sim.RequestsFor("tools/list")); got != len(cases) {
		t.Fatalf("recorded %d tools/list requests", got)
	}
}

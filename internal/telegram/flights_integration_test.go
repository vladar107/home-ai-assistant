package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/connectors/flights"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// flightGateway stands in for AeroDataBox: it answers every lookup with the delayed fixture, or
// with a refusal while failing is set.
func flightGateway(t *testing.T, failing *atomic.Bool) string {
	t.Helper()
	fixture, err := os.ReadFile("../connectors/flights/testdata/delayed.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestFlightStatusHiddenWithoutOperatorKey(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()

	settings := h.requestSettings(1, 101)
	if strings.Contains(settings.text, "Flights") || slices.ContainsFunc(buttonData(settings.keyboard), func(data string) bool {
		return strings.HasPrefix(data, flightsSectionPrefix+":")
	}) {
		t.Fatalf("settings showed flights without a key: %+v", settings)
	}
	h.push(textUpdate(2, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if names := toolNames(call.request.Tools); slices.Contains(names, "flight_status") {
		t.Fatalf("tools without a key = %v", names)
	}
	call.answer <- agentruntime.RunResult{Response: "hi"}
	h.wantReply(101, 2, "hi")
}

// The tool is offered while the user has Flight status on, and a reply from a turn that read
// flight data names the source even when the model left it out; a failed check, of a number the
// cache does not hold, adds nothing.
func TestFlightStatusSwitchAndAttribution(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	var failing atomic.Bool
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{flights: flightGateway(t, &failing)})
	h.provision("alice", "account-alice")
	h.start()

	turn := func(update int64, reply, number string, call func(agentruntime.ToolSet) string) string {
		t.Helper()
		h.push(textUpdate(update, 101, "is my flight late?"))
		got := receive(t, h.ctx, runtime.calls)
		names := toolNames(got.request.Tools)
		if call != nil {
			if !slices.Contains(names, "flight_status") {
				t.Fatalf("tools = %v, want flight_status", names)
			}
			result, err := got.request.Tools.Executor.Execute(got.ctx, agentruntime.ToolCall{Name: "flight_status", Arguments: json.RawMessage(`{"flight_number":"` + number + `"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if want := call(got.request.Tools); !strings.Contains(result.Text, want) {
				t.Fatalf("result = %+v, want %q", result, want)
			}
		}
		got.answer <- agentruntime.RunResult{Response: reply}
		return strings.Join(names, ",")
	}

	turn(1, "It is late.", "EX123", func(agentruntime.ToolSet) string { return "delayed +45 min" })
	h.wantReply(101, 1, "It is late.\n\n"+flights.Attribution)
	h.waitState(1, "delivered")

	turn(2, "It is late, says AeroDataBox (aerodatabox.com).", "EX123", func(agentruntime.ToolSet) string { return "delayed +45 min" })
	h.wantReply(101, 2, "It is late, says AeroDataBox (aerodatabox.com).")
	h.waitState(2, "delivered")

	failing.Store(true)
	turn(3, "The check failed.", "EX124", func(agentruntime.ToolSet) string { return "Flight status check failed" })
	h.wantReply(101, 3, "The check failed.")
	h.waitState(3, "delivered")

	h.push(textUpdate(4, 101, "/privacy"))
	if message := h.nextMessage(); !strings.Contains(message.text, localetest.English(FlightsPrivacyLine)) {
		t.Fatalf("privacy = %q", message.text)
	}
	h.requestSettings(5, 101)
	settings := h.openSettingsPage(6, 101, settingsMessageID(5), "tools")
	if !strings.Contains(settings.text, localetest.English(FlightsOnLine)) || !slices.Contains(buttonData(settings.keyboard), "flights:off") {
		t.Fatalf("settings = %+v", settings)
	}
	if edit := h.press(7, 101, settingsMessageID(5), "flights:off"); !strings.Contains(edit.text, localetest.English(FlightsOffLine)) {
		t.Fatalf("after turning flights off = %+v", edit)
	}
	if names := turn(8, "ok", "", nil); strings.Contains(names, "flight_status") {
		t.Fatalf("tools with flights off = %v", names)
	}
	h.wantReply(101, 8, "ok")
}

// A key RapidAPI does not subscribe to AeroDataBox fails the check and leaves Flight status
// reported as unavailable in /settings, with the overview pointing at it.
func TestFlightStatusUnsubscribedKeyIsShownInSettings(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You are not subscribed to this API."}`))
	}))
	t.Cleanup(server.Close)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{flights: server.URL})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "is EX123 late?"))
	call := receive(t, h.ctx, runtime.calls)
	result, err := call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: "flight_status", Arguments: json.RawMessage(`{"flight_number":"EX123"}`)})
	if err != nil || !result.IsError || !strings.Contains(result.Text, "not subscribed to AeroDataBox") {
		t.Fatalf("result = %+v %v", result, err)
	}
	call.answer <- agentruntime.RunResult{Response: "The check failed."}
	h.wantReply(101, 1, "The check failed.")
	h.waitState(1, "delivered")

	overview := h.requestSettings(2, 101)
	if !strings.Contains(overview.text, "Flights: unavailable until the operator fixes it.") {
		t.Fatalf("overview = %q", overview.text)
	}
	settings := h.openSettingsPage(3, 101, settingsMessageID(2), "tools")
	if !strings.Contains(settings.text, "Flight status is unavailable: the flight status provider refused the operator's key") {
		t.Fatalf("settings = %q", settings.text)
	}
}

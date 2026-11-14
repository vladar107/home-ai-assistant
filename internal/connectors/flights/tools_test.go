package flights_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/flights"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

func (h *harness) call(arguments string) (tools.Result, *tools.Run) {
	h.t.Helper()
	run := tools.NewRun("how is my flight?")
	result, err := h.service.Execute(tools.WithRun(context.Background(), run), "alice", tools.Call{Name: "flight_status", Arguments: json.RawMessage(arguments)})
	if err != nil {
		h.t.Fatal(err)
	}
	return result, run
}

// Each fixture renders as one block per leg that opens with its status phrase, in each airport's
// local time, and ends with the source; the run then owes the reply that source.
func TestFlightStatusRendersEachStatusAndNamesTheSource(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cases := []struct {
		fixture string
		want    []string
	}{
		{"on_time.json", []string{"on time\nEX123 Example City (XAA) \u2192 Other City (XBB)", "Departure: scheduled Mon 9 Nov 13:40, expected 13:40, gate B7, terminal 1", "Arrival: scheduled Mon 9 Nov 19:55, expected 19:55, terminal A"}},
		{"delayed.json", []string{"delayed +45 min\n", "Departure: scheduled Mon 9 Nov 13:40, expected 14:25, gate B12, terminal 2", "expected 20:35"}},
		{"cancelled.json", []string{"cancelled\n"}},
		{"diverted.json", []string{"diverted\nEX123 Example City (XAA) \u2192 Third City (XCC)", "actual 13:52"}},
		{"schedule_only.json", []string{"schedule only, no live data\n"}},
		{"two_legs.json", []string{"Example City (XAA) \u2192 Other City (XBB)", "Other City (XBB) \u2192 Third City (XCC)", "Departure: scheduled Mon 9 Nov 21:00"}},
		{"codeshare.json", []string{"EX123 (operated by Example Air) Example City (XAA)"}},
	}
	for index, c := range cases {
		h.fake.answer(reply{status: http.StatusOK, fixture: c.fixture})
		h.clock.Advance(10 * time.Minute)
		result, run := h.call(`{"flight_number":"EX 0123","date":"2026-11-09"}`)
		if result.IsError || !strings.HasSuffix(result.Text, "Times are local to each airport.\n"+flights.Attribution) {
			t.Fatalf("%s: result = %+v", c.fixture, result)
		}
		for _, want := range c.want {
			if !strings.Contains(result.Text, want) {
				t.Errorf("%s lacks %q:\n%s", c.fixture, want, result.Text)
			}
		}
		if got := run.Attributed("It leaves on time."); got != "It leaves on time.\n\n"+flights.Attribution {
			t.Errorf("%s: attributed reply = %q", c.fixture, got)
		}
		if got := run.Attributed("Per AeroDataBox.com it leaves on time."); got != "Per AeroDataBox.com it leaves on time." {
			t.Errorf("%s: a reply that names the source = %q", c.fixture, got)
		}
		if h.fake.Count() != index+1 {
			t.Fatalf("%s: requests = %d", c.fixture, h.fake.Count())
		}
	}
	if query := h.fake.Requests()[0].URL.Query(); query.Get("dateLocalRole") != "Departure" {
		t.Fatalf("the tool asked with %q", query.Get("dateLocalRole"))
	}
}

// A day more than a week away, a malformed number, a flight the provider does not know, and a
// failed check each answer without data, and only the first two without a request.
func TestFlightStatusRefusesFarDaysAndSaysWhenItFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, arguments := range []string{`{"flight_number":"EX123","date":"2026-11-17"}`, `{"flight_number":"EX123","date":"2026-11-01"}`, `{"flight_number":"nonsense"}`} {
		if result, _ := h.call(arguments); !result.IsError || h.fake.Count() != 0 {
			t.Fatalf("%s: %+v, requests %d", arguments, result, h.fake.Count())
		}
	}
	if result, _ := h.call(`{"flight_number":"EX123","date":"2026-11-16"}`); result.IsError || h.fake.Count() != 1 {
		t.Fatalf("seven days ahead: %+v", result)
	}
	if got := h.fake.Requests()[0].URL.Path; got != "/flights/number/EX123/2026-11-16" {
		t.Fatalf("path = %q", got)
	}

	h.fake.answer(reply{status: http.StatusNoContent})
	result, run := h.call(`{"flight_number":"ZZ456"}`)
	if result.IsError || !strings.Contains(result.Text, "ZZ456 was not found departing on Mon 9 Nov") || !strings.Contains(result.Text, `"operated by"`) {
		t.Fatalf("not found = %+v", result)
	}
	if got := run.Attributed("Not found."); got != "Not found." {
		t.Fatalf("a reply without flight data got %q", got)
	}
	if h.fake.Requests()[1].URL.Path != "/flights/number/ZZ456/2026-11-09" {
		t.Fatalf("the default day is today: %q", h.fake.Requests()[1].URL.Path)
	}

	h.fake.answer(reply{status: http.StatusBadRequest, body: `{"message":"bad"}`})
	result, run = h.call(`{"flight_number":"EX124"}`)
	if !result.IsError || !strings.HasPrefix(result.Text, "Flight status check failed: ") || strings.Contains(result.Text, "on time") {
		t.Fatalf("failed = %+v", result)
	}
	if got := run.Attributed("The check failed."); got != "The check failed." {
		t.Fatalf("a failed check got %q", got)
	}
}

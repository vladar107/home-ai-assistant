package flights_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/flights"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

// A lookup sends the key only in RapidAPI's headers, asks for no image, position, or flight plan,
// and reads the fixture's leg; a malformed number sends nothing.
func TestLookupSendsTheKeyInHeadersAndAsksForNothingBilledExtra(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	legs, err := h.lookup("ex 0123", flights.PurposeTracking, "")
	if err != nil || len(legs) != 1 {
		t.Fatalf("lookup = %+v %v", legs, err)
	}
	leg := legs[0]
	if leg.Designator != "EX123" || leg.Departure.Airport.Code != "XAA" || leg.Arrival.Airport.Name != "Other City" ||
		leg.Departure.Gate != "B7" || !leg.Live || leg.Status != flights.StatusExpected || leg.Codeshare != flights.CodeshareOperator ||
		!leg.Departure.Scheduled.Equal(time.Date(2026, 11, 9, 13, 40, 0, 0, time.UTC)) || leg.Departure.Delay() != 0 {
		t.Fatalf("leg = %+v", leg)
	}
	requests := h.fake.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %d", len(requests))
	}
	request := requests[0]
	if request.URL.Path != "/flights/number/EX123/2026-11-09" {
		t.Fatalf("path = %q", request.URL.Path)
	}
	query := request.URL.Query()
	for name, want := range map[string]string{"withAircraftImage": "false", "withLocation": "false", "withFlightPlan": "false", "dateLocalRole": "Departure"} {
		if query.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, query.Get(name), want)
		}
	}
	if request.Header.Get("X-RapidAPI-Key") != apiKey || request.Header.Get("X-RapidAPI-Host") != "aerodatabox.p.rapidapi.com" {
		t.Fatalf("headers = %v", request.Header)
	}
	if strings.Contains(request.URL.String(), apiKey) || strings.Contains(h.Logs(), apiKey) {
		t.Fatal("the key left the headers")
	}

	if _, err := h.lookup("not a flight", flights.PurposeTool, "alice"); err == nil || h.fake.Count() != 1 {
		t.Fatalf("a malformed number: %v, requests %d", err, h.fake.Count())
	}
}

func TestFixturesReadAsTheirCases(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	day := 0
	read := func(fixture string) []flights.Leg {
		t.Helper()
		h.fake.answer(reply{status: http.StatusOK, fixture: fixture})
		day++
		legs, err := h.service.Lookup(t.Context(), flights.Request{Designator: "EX123", Day: fmt.Sprintf("2026-11-%02d", day), Role: flights.RoleBoth})
		if err != nil {
			t.Fatalf("%s: %v", fixture, err)
		}
		return legs
	}
	if leg := read("delayed.json")[0]; leg.Departure.Delay() != 45*time.Minute || leg.Arrival.Delay() != 40*time.Minute || leg.Departure.Gate != "B12" || leg.Departure.Terminal != "2" {
		t.Errorf("delayed = %+v", leg)
	}
	if leg := read("cancelled.json")[0]; leg.Status != flights.StatusCanceled {
		t.Errorf("cancelled = %+v", leg)
	}
	if leg := read("diverted.json")[0]; leg.Status != flights.StatusDiverted || leg.Arrival.Airport.Code != "XCC" || !leg.Departed() {
		t.Errorf("diverted = %+v", leg)
	}
	if leg := read("schedule_only.json")[0]; leg.Live {
		t.Errorf("schedule only = %+v", leg)
	}
	if legs := read("two_legs.json"); len(legs) != 2 || legs[1].Departure.Airport.Code != "XBB" {
		t.Errorf("two legs = %+v", legs)
	}
	if leg := read("codeshare.json")[0]; leg.Codeshare != flights.CodeshareMarketed || leg.Designator != "ZZ456" || leg.Airline != "Example Air" {
		t.Errorf("codeshare = %+v", leg)
	}
}

// A repeat within five minutes is served from the cache, whoever asks; a 204 is not found and is
// cached too.
func TestCacheServesARepeatAndNoContentIsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, purpose := range []flights.Purpose{flights.PurposeTracking, flights.PurposeTool} {
		if _, err := h.lookup("EX123", purpose, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	if h.fake.Count() != 1 || h.used(flights.ScopeAll) != 2 {
		t.Fatalf("requests %d, units %d", h.fake.Count(), h.used(flights.ScopeAll))
	}
	h.clock.Advance(5 * time.Minute)
	if _, err := h.lookup("EX123", flights.PurposeTracking, ""); err != nil || h.fake.Count() != 2 {
		t.Fatalf("after the cache: %v, requests %d", err, h.fake.Count())
	}

	h.fake.answer(reply{status: http.StatusNoContent})
	for range 2 {
		if _, err := h.lookup("EX999", flights.PurposeTracking, ""); !errors.Is(err, flights.ErrNotFound) {
			t.Fatalf("204 = %v", err)
		}
	}
	if h.fake.Count() != 3 {
		t.Fatalf("requests %d", h.fake.Count())
	}
}

// Every attempt, retries included, is reserved before it goes out, and requests are spaced two
// seconds apart.
func TestRetriedAttemptsCostUnitsAndRequestsAreSpaced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.answer(reply{status: http.StatusServiceUnavailable}, reply{status: http.StatusOK, fixture: "on_time.json"})
	if _, err := h.lookup("EX123", flights.PurposeTracking, ""); err != nil {
		t.Fatal(err)
	}
	if h.fake.Count() != 2 || h.used(flights.ScopeAll) != 4 {
		t.Fatalf("requests %d, units %d", h.fake.Count(), h.used(flights.ScopeAll))
	}
	slept := h.clock.Slept()
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("waits = %v, want one of 2s before the retry", slept)
	}
}

// Concurrent lookups at the limit reserve no more than it, and only the reserved ones are sent.
func TestConcurrentLookupsNeverReservePastTheBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.monthly = 10
	h.service = h.build(h.store)
	var wg sync.WaitGroup
	var mu sync.Mutex
	spent := 0
	for index := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.lookup(fmt.Sprintf("EX%d", 100+index), flights.PurposeTracking, "")
			var refused *flights.SpentError
			if errors.As(err, &refused) {
				mu.Lock()
				spent++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if h.used(flights.ScopeAll) != 10 || h.fake.Count() != 5 || spent != 7 {
		t.Fatalf("units %d, requests %d, refused %d", h.used(flights.ScopeAll), h.fake.Count(), spent)
	}
}

// The household budget, the tool's share, and one user's share each refuse without a request, and
// the counts survive a restart on the same database.
func TestBudgetsRefuseWithoutARequestAndSurviveARestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for index := range 25 {
		if _, err := h.lookup(fmt.Sprintf("EX%d", 100+index), flights.PurposeTool, "alice"); err != nil {
			t.Fatalf("lookup %d: %v", index, err)
		}
	}
	_, err := h.lookup("EX200", flights.PurposeTool, "alice")
	var refused *flights.SpentError
	if !errors.As(err, &refused) || refused.Scope != flights.ScopeUser || !refused.Until.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) ||
		!strings.Contains(flights.ModelText(err), "your share of flight questions until Tue 1 Dec") {
		t.Fatalf("alice's 26th question = %v", err)
	}
	if h.fake.Count() != 25 {
		t.Fatalf("requests = %d", h.fake.Count())
	}

	// A restart on the same database keeps every count.
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.store = reopen(t, h.path)
	h.service = h.build(h.store)
	if _, err := h.lookup("EX201", flights.PurposeTool, "alice"); !errors.As(err, &refused) || refused.Scope != flights.ScopeUser {
		t.Fatalf("after a restart: %v", err)
	}
	for index := range 25 {
		if _, err := h.lookup(fmt.Sprintf("EX%d", 300+index), flights.PurposeTool, "bob"); err != nil {
			t.Fatalf("bob's lookup %d: %v", index, err)
		}
	}
	if _, err := h.lookup("EX400", flights.PurposeTool, "carol"); !errors.As(err, &refused) || refused.Scope != flights.ScopeTool ||
		!strings.Contains(flights.ModelText(err), "tracked trips are still followed") {
		t.Fatalf("the tool's share = %v", err)
	}
	// Tracking may spend the rest of the household budget, and no more.
	for index := range 120 {
		if _, err := h.lookup(fmt.Sprintf("EX%d", 1000+index), flights.PurposeTracking, ""); err != nil {
			t.Fatalf("tracking lookup %d: %v", index, err)
		}
	}
	if _, err := h.lookup("EX2000", flights.PurposeTracking, ""); !errors.As(err, &refused) || refused.Scope != flights.ScopeAll ||
		flights.ModelText(err) != "Flight tracking is paused until Tue 1 Dec: the household's free flight-status allowance for this month is used up. Check the airline's app." {
		t.Fatalf("the household budget = %v", err)
	}
	if h.fake.Count() != 170 || h.used(flights.ScopeAll) != 340 {
		t.Fatalf("requests %d, units %d", h.fake.Count(), h.used(flights.ScopeAll))
	}
	left, until, err := h.service.UnitsLeft(t.Context())
	if err != nil || left != 0 || !until.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("units left = %d %v %v", left, until, err)
	}

	// The next period starts on the reset day, with every count back at zero.
	h.clock.Set(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if _, err := h.lookup("EX2000", flights.PurposeTracking, ""); err != nil {
		t.Fatalf("in the next period: %v", err)
	}
	if _, err := h.lookup("EX2001", flights.PurposeTool, "alice"); err != nil {
		t.Fatalf("alice in the next period: %v", err)
	}
}

func reopen(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestPeriodsStartOnTheResetDay(t *testing.T) {
	t.Parallel()
	budget := flights.NewBudget(340, 15)
	if budget.Tool != 100 || budget.User != 50 {
		t.Fatalf("shares = %d, %d", budget.Tool, budget.User)
	}
	start, end := budget.Period(time.Date(2026, 11, 14, 23, 59, 0, 0, time.UTC))
	if !start.Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("period = %v to %v", start, end)
	}
	start, _ = budget.Period(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC))
	if !start.Equal(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("on the reset day the period starts = %v", start)
	}
	// A reset day past a month's end falls on its last day.
	start, end = flights.NewBudget(340, 31).Period(time.Date(2027, 2, 28, 12, 0, 0, 0, time.UTC))
	if !start.Equal(time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("short month period = %v to %v", start, end)
	}
}

// The marketplace's own word that the quota is spent stops lookups: a remaining count below one
// lookup after the answer, or a quota refusal, which is not retried.
func TestTheMarketplaceQuotaStopsLookups(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.answer(reply{status: http.StatusOK, fixture: "on_time.json", header: http.Header{"X-Ratelimit-Api-Units-Remaining": {"1"}}})
	if _, err := h.lookup("EX123", flights.PurposeTracking, ""); err != nil {
		t.Fatalf("the answer that said 1 unit was left: %v", err)
	}
	var refused *flights.SpentError
	if _, err := h.lookup("EX124", flights.PurposeTracking, ""); !errors.As(err, &refused) || refused.Scope != flights.ScopeAll || h.fake.Count() != 1 {
		t.Fatalf("after it: %v, requests %d", err, h.fake.Count())
	}

	other := newHarness(t)
	other.fake.answerAlways(reply{status: http.StatusTooManyRequests, body: `{"message":"You have exceeded the MONTHLY quota for Units on your current plan, BASIC."}`})
	if _, err := other.lookup("EX123", flights.PurposeTool, "alice"); !errors.As(err, &refused) || refused.Scope != flights.ScopeAll {
		t.Fatalf("a quota refusal = %v", err)
	}
	if _, err := other.lookup("EX124", flights.PurposeTracking, ""); !errors.As(err, &refused) || other.fake.Count() != 1 {
		t.Fatalf("after a quota refusal: %v, requests %d", err, other.fake.Count())
	}
}

// Each failure is one sentence for the model and the log that carries neither the key nor the
// body; a refused key stops every later lookup until restart.
func TestFailuresAreOneSentenceWithoutTheKeyOrBody(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leak := `{"message":"` + apiKey + ` secret body"}`
	for _, c := range []struct {
		status int
		class  string
	}{
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusUnavailableForLegalReasons, "unavailable"},
		{http.StatusInternalServerError, "unavailable"},
		{http.StatusBadRequest, "unexpected_response"},
		{http.StatusOK, "unexpected_response"},
		{http.StatusUnauthorized, "unauthorized"},
	} {
		h.fake.answerAlways(reply{status: c.status, body: leak})
		_, err := h.lookup(fmt.Sprintf("EX%d", c.status), flights.PurposeTool, "alice")
		text := flights.ModelText(err)
		if flights.ErrorClass(err) != c.class || strings.Contains(text, apiKey) || strings.Contains(text, "secret") || strings.Count(text, ". ") > 1 {
			t.Errorf("%d: class %q, text %q", c.status, flights.ErrorClass(err), text)
		}
	}
	if strings.Contains(h.Logs(), apiKey) || strings.Contains(h.Logs(), "secret") {
		t.Fatalf("the log carries the key or body:\n%s", h.Logs())
	}
	count := h.fake.Count()
	h.fake.answerAlways(reply{status: http.StatusOK, fixture: "on_time.json"})
	if _, err := h.lookup("EX777", flights.PurposeTracking, ""); !errors.Is(err, flights.ErrUnauthorized) || h.fake.Count() != count {
		t.Fatalf("after a refused key: %v, requests %d of %d", err, h.fake.Count(), count)
	}
	if h.service.Unavailable() == "" {
		t.Fatal("a refused key is not reported as unavailable")
	}
}

// AeroDataBox answers 402 once the plan's units are spent: lookups stop as for a spent budget.
// RapidAPI answers 403 for a key not subscribed to the API: it is not retried, and every later
// lookup is skipped and reported as unavailable.
func TestPaymentRequiredPausesAndForbiddenStopsLookups(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.answerAlways(reply{status: http.StatusPaymentRequired, body: `{"message":"Your plan is expired and/or included monthly API units and credits have been exhausted."}`})
	var refused *flights.SpentError
	if _, err := h.lookup("EX123", flights.PurposeTracking, ""); !errors.As(err, &refused) || refused.Scope != flights.ScopeAll {
		t.Fatalf("402 = %v", err)
	}
	if _, err := h.lookup("EX124", flights.PurposeTool, "alice"); !errors.As(err, &refused) || h.fake.Count() != 1 {
		t.Fatalf("after a 402: %v, requests %d", err, h.fake.Count())
	}
	if h.service.Unavailable() != "" {
		t.Fatal("a spent allowance is reported as a configuration error")
	}

	other := newHarness(t)
	other.fake.answerAlways(reply{status: http.StatusForbidden, body: `{"message":"You are not subscribed to this API."}`})
	_, err := other.lookup("EX123", flights.PurposeTool, "alice")
	if flights.ErrorClass(err) != "not_subscribed" || other.fake.Count() != 1 || !strings.Contains(flights.ModelText(err), "not subscribed") {
		t.Fatalf("403 = %v, requests %d", err, other.fake.Count())
	}
	if _, err := other.lookup("EX124", flights.PurposeTracking, ""); err == nil || other.fake.Count() != 1 || other.service.Unavailable() == "" {
		t.Fatalf("after a 403: %v, requests %d, unavailable %q", err, other.fake.Count(), other.service.Unavailable())
	}
	if !strings.Contains(other.Logs(), "error_class=not_subscribed") {
		t.Fatalf("the 403 was not logged:\n%s", other.Logs())
	}
}

// Callers asking for the same flight at once share one request; a different flight is asked
// for on its own.
func TestConcurrentIdenticalLookupsShareOneRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	arrived, gate := make(chan struct{}, 8), make(chan struct{})
	h.fake.mu.Lock()
	h.fake.arrived, h.fake.gate = arrived, gate
	h.fake.mu.Unlock()
	var wg sync.WaitGroup
	results := make([]error, 4)
	for index, purpose := range []flights.Purpose{flights.PurposeTracking, flights.PurposeTool, flights.PurposeTool, flights.PurposeTracking} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[index] = h.lookup("EX123", purpose, "alice")
		}()
	}
	<-arrived
	close(gate)
	wg.Wait()
	for index, err := range results {
		if err != nil {
			t.Fatalf("caller %d: %v", index, err)
		}
	}
	if h.fake.Count() != 1 || h.used(flights.ScopeAll) != 2 {
		t.Fatalf("requests %d, units %d", h.fake.Count(), h.used(flights.ScopeAll))
	}
}

// A flight without a scheduled departure is left out of the answer, which fails only when no
// flight in it is usable.
func TestALegWithoutAScheduleIsSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	fixture, err := os.ReadFile(filepath.Join("testdata", "two_legs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var flightsJSON []map[string]any
	if err := json.Unmarshal(fixture, &flightsJSON); err != nil {
		t.Fatal(err)
	}
	delete(flightsJSON[0]["departure"].(map[string]any), "scheduledTime")
	partial, _ := json.Marshal(flightsJSON)
	delete(flightsJSON[1]["departure"].(map[string]any), "scheduledTime")
	none, _ := json.Marshal(flightsJSON)
	h.fake.answer(reply{status: http.StatusOK, body: string(partial)}, reply{status: http.StatusOK, body: string(none)})
	legs, err := h.lookup("EX123", flights.PurposeTracking, "")
	if err != nil || len(legs) != 1 || legs[0].Departure.Airport.Code != "XBB" {
		t.Fatalf("partial answer = %+v %v", legs, err)
	}
	if _, err := h.lookup("EX124", flights.PurposeTracking, ""); flights.ErrorClass(err) != "unexpected_response" {
		t.Fatalf("an answer without a usable flight = %v", err)
	}
}

package flights_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/flights"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

const apiKey = "test-rapidapi-key-7d2e"

// lookupDay is the day the fixtures describe: EX123 leaves Example City at 13:40 UTC.
var lookupDay = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

type clock struct {
	mu  sync.Mutex
	now time.Time
	// slept holds every wait the limiter asked for.
	slept []time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func (c *clock) Advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

// Sleep stands in for the limiter's wait: it moves the clock instead of waiting.
func (c *clock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	return ctx.Err()
}

func (c *clock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// reply is one answer of the fake AeroDataBox.
type reply struct {
	status  int
	fixture string
	body    string
	header  http.Header
}

// aeroDataBox stands in for AeroDataBox on RapidAPI at its HTTP boundary: it answers each request
// with the next scripted reply, or the fallback once the script is used up, and records every
// request it received.
type aeroDataBox struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	script   []reply
	fallback reply
	requests []*http.Request
	// arrived hears of each request and gate holds its answer until closed, when they are set.
	arrived chan struct{}
	gate    chan struct{}
}

func newAeroDataBox(t *testing.T) *aeroDataBox {
	t.Helper()
	fake := &aeroDataBox{t: t, fallback: reply{status: http.StatusOK, fixture: "on_time.json"}}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (a *aeroDataBox) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.requests = append(a.requests, r.Clone(context.Background()))
	next := a.fallback
	if len(a.script) > 0 {
		next, a.script = a.script[0], a.script[1:]
	}
	arrived, gate := a.arrived, a.gate
	a.mu.Unlock()
	if arrived != nil {
		arrived <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	for name, values := range next.header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	body := []byte(next.body)
	if next.fixture != "" {
		var err error
		if body, err = os.ReadFile(filepath.Join("testdata", next.fixture)); err != nil {
			a.t.Error(err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(next.status)
	_, _ = w.Write(body)
}

// answer scripts the next replies.
func (a *aeroDataBox) answer(replies ...reply) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.script = append(a.script, replies...)
}

// answerAlways sets the reply once the script is used up.
func (a *aeroDataBox) answerAlways(next reply) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fallback = next
}

func (a *aeroDataBox) Requests() []*http.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*http.Request(nil), a.requests...)
}

func (a *aeroDataBox) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

type harness struct {
	t       *testing.T
	fake    *aeroDataBox
	clock   *clock
	path    string
	store   *sqlite.Store
	service *flights.Service
	logs    *logBuffer
	monthly int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	httpretry.WithoutWaits(t)
	h := &harness{t: t, fake: newAeroDataBox(t), clock: &clock{now: lookupDay}, path: filepath.Join(t.TempDir(), "home-agent.db"), logs: &logBuffer{}, monthly: 340}
	h.store = testutil.OpenStore(t, h.path)
	t.Cleanup(func() { _ = h.store.Close() })
	h.service = h.build(h.store)
	return h
}

// build makes a service over the store, as a restart would.
func (h *harness) build(store *sqlite.Store) *flights.Service {
	h.t.Helper()
	logger := slog.New(slog.NewTextHandler(h.logs, nil))
	switches, err := integration.NewSwitches(store, logger)
	if err != nil {
		h.t.Fatal(err)
	}
	service, err := flights.NewService(flights.Config{
		APIKey: apiKey, MonthlyUnits: h.monthly, QuotaResetDay: 1, Zone: time.UTC,
		BaseURL: h.fake.server.URL, Now: h.clock.Now, Sleep: h.clock.Sleep,
	}, store, switches, logger)
	if err != nil {
		h.t.Fatal(err)
	}
	return service
}

// logBuffer keeps one harness's log lines; each test has its own.
type logBuffer struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.Write(p)
}

func (h *harness) Logs() string {
	h.logs.mu.Lock()
	defer h.logs.mu.Unlock()
	return h.logs.builder.String()
}

func (h *harness) lookup(designator string, purpose flights.Purpose, principalID string) ([]flights.Leg, error) {
	h.t.Helper()
	return h.service.Lookup(h.t.Context(), flights.Request{Designator: designator, Day: "2026-11-09", Role: flights.RoleDeparture, Purpose: purpose, PrincipalID: principalID})
}

func (h *harness) used(scope string) int {
	h.t.Helper()
	start, _ := flights.NewBudget(h.monthly, 1).Period(h.clock.Now())
	used, err := h.store.FlightUnitsUsed(h.t.Context(), start.Format(time.DateOnly), scope)
	if err != nil {
		h.t.Fatal(err)
	}
	return used
}

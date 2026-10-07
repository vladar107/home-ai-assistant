package httpretry_test

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

type clock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func newClock() *clock { return &clock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	c.advance(d)
	return nil
}

type observer struct {
	mu       sync.Mutex
	services []string
	outcomes []string
}

func (o *observer) RecordHTTPRetry(_ context.Context, service, outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.services = append(o.services, service)
	o.outcomes = append(o.outcomes, outcome)
}

type counting struct {
	base  http.RoundTripper
	calls atomic.Int32
}

func (c *counting) RoundTrip(request *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.base.RoundTrip(request)
}

func harness() (*clock, *observer, *counting, *http.Client) {
	clk := newClock()
	obs := &observer{}
	counter := &counting{base: http.DefaultTransport}
	jitter := rand.New(rand.NewPCG(1, 2))
	client := &http.Client{Transport: httpretry.New(counter, httpretry.Options{
		Service:  "fake",
		Observer: obs,
		Now:      clk.Now,
		Sleep:    clk.Sleep,
		Jitter:   func(limit time.Duration) time.Duration { return time.Duration(jitter.Int64N(int64(limit) + 1)) },
	})}
	return clk, obs, counter, client
}

func do(t *testing.T, client *http.Client, method, url string, body io.Reader) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return client.Do(request)
}

func flaky(failures int32, status int) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= failures {
			http.Error(w, "busy", status)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	return server, &calls
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFailsTwiceThenSucceeds(t *testing.T) {
	server, calls := flaky(2, http.StatusServiceUnavailable)
	defer server.Close()
	clk, obs, _, client := harness()

	response, err := do(t, client, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readBody(t, response); got != "ok" || calls.Load() != 3 {
		t.Fatalf("body %q after %d calls", got, calls.Load())
	}
	if len(clk.waits) != 2 || clk.waits[0] > 250*time.Millisecond || clk.waits[1] > 500*time.Millisecond {
		t.Fatalf("waits %v exceed the backoff limits", clk.waits)
	}
	if len(obs.outcomes) != 1 || obs.outcomes[0] != httpretry.OutcomeRecovered || obs.services[0] != "fake" {
		t.Fatalf("recorded %v for %v", obs.outcomes, obs.services)
	}
}

func TestFirstSuccessIsNotRecorded(t *testing.T) {
	server, _ := flaky(0, 0)
	defer server.Close()
	_, obs, _, client := harness()

	response, err := do(t, client, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	readBody(t, response)
	if len(obs.outcomes) != 0 {
		t.Fatalf("recorded %v", obs.outcomes)
	}
}

func TestAttemptCap(t *testing.T) {
	server, calls := flaky(100, http.StatusBadGateway)
	defer server.Close()
	_, obs, _, client := harness()

	response, err := do(t, client, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readBody(t, response); response.StatusCode != http.StatusBadGateway || !strings.Contains(got, "busy") {
		t.Fatalf("last response %d %q", response.StatusCode, got)
	}
	if calls.Load() != httpretry.MaximumAttempts {
		t.Fatalf("%d attempts", calls.Load())
	}
	if len(obs.outcomes) != 1 || obs.outcomes[0] != httpretry.OutcomeExhausted {
		t.Fatalf("recorded %v", obs.outcomes)
	}
}

func TestWindowStopsNewAttempts(t *testing.T) {
	var calls atomic.Int32
	clk := newClock()
	obs := &observer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		clk.advance(2 * time.Second)
		http.Error(w, "slow", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := &http.Client{Transport: httpretry.New(nil, httpretry.Options{
		Service: "fake", Observer: obs, Now: clk.Now, Sleep: clk.Sleep,
		Jitter: func(time.Duration) time.Duration { return 0 },
	})}

	response, err := do(t, client, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	readBody(t, response)
	if calls.Load() != 3 {
		t.Fatalf("%d attempts; the third ended 6 s after the first began", calls.Load())
	}
	if len(obs.outcomes) != 1 || obs.outcomes[0] != httpretry.OutcomeExhausted {
		t.Fatalf("recorded %v", obs.outcomes)
	}
}

func TestRetryAfterThatFits(t *testing.T) {
	for name, header := range map[string]func(*clock) string{
		"seconds": func(*clock) string { return "3" },
		"date":    func(c *clock) string { return c.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat) },
	} {
		t.Run(name, func(t *testing.T) {
			clk := newClock()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", header(clk))
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				_, _ = io.WriteString(w, "ok")
			}))
			defer server.Close()
			client := &http.Client{Transport: httpretry.New(nil, httpretry.Options{
				Service: "fake", Now: clk.Now, Sleep: clk.Sleep,
			})}

			response, err := do(t, client, http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := readBody(t, response); got != "ok" || len(clk.waits) != 1 || clk.waits[0] != 3*time.Second {
				t.Fatalf("body %q, waits %v", got, clk.waits)
			}
		})
	}
}

func TestRetryAfterThatDoesNotFit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		http.Error(w, "later", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	clk, obs, _, client := harness()

	response, err := do(t, client, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readBody(t, response); response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(got, "later") {
		t.Fatalf("response %d %q", response.StatusCode, got)
	}
	if calls.Load() != 1 || len(clk.waits) != 0 {
		t.Fatalf("%d attempts, waits %v", calls.Load(), clk.waits)
	}
	if len(obs.outcomes) != 1 || obs.outcomes[0] != httpretry.OutcomeNotRetryable {
		t.Fatalf("recorded %v", obs.outcomes)
	}
}

func TestOtherStatusesAreReturned(t *testing.T) {
	server, calls := flaky(100, http.StatusNotFound)
	defer server.Close()
	_, obs, _, client := harness()

	response, err := do(t, client, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	readBody(t, response)
	if response.StatusCode != http.StatusNotFound || calls.Load() != 1 || len(obs.outcomes) != 0 {
		t.Fatalf("status %d after %d calls, recorded %v", response.StatusCode, calls.Load(), obs.outcomes)
	}
}

func TestUnmarkedPostIsNotRetriedAfterServerError(t *testing.T) {
	server, calls := flaky(1, http.StatusServiceUnavailable)
	defer server.Close()
	_, obs, _, client := harness()

	response, err := do(t, client, http.MethodPost, server.URL, strings.NewReader("write"))
	if err != nil {
		t.Fatal(err)
	}
	readBody(t, response)
	if response.StatusCode != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatalf("status %d after %d calls", response.StatusCode, calls.Load())
	}
	if len(obs.outcomes) != 1 || obs.outcomes[0] != httpretry.OutcomeNotRetryable {
		t.Fatalf("recorded %v", obs.outcomes)
	}
}

func TestUnmarkedPostIsRetriedAfterRefusedConnection(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	address := server.URL
	server.Close()
	_, obs, counter, client := harness()

	_, err := do(t, client, http.MethodPost, address, strings.NewReader("write"))
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if counter.calls.Load() != httpretry.MaximumAttempts {
		t.Fatalf("%d attempts", counter.calls.Load())
	}
	if len(obs.outcomes) != 1 || obs.outcomes[0] != httpretry.OutcomeExhausted {
		t.Fatalf("recorded %v", obs.outcomes)
	}
}

func TestMarkedPostIsRetriedWithItsBody(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(data))
		count := len(bodies)
		mu.Unlock()
		if count < 3 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	_, _, _, client := harness()

	request, err := http.NewRequestWithContext(httpretry.Idempotent(context.Background()), http.MethodPost, server.URL, strings.NewReader(`{"query":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if got := readBody(t, response); got != "ok" {
		t.Fatalf("body %q", got)
	}
	if len(bodies) != 3 || bodies[0] != `{"query":"x"}` || bodies[1] != bodies[0] || bodies[2] != bodies[0] {
		t.Fatalf("bodies %q", bodies)
	}
}

func TestBodyWithoutGetBodyIsNotRetried(t *testing.T) {
	server, calls := flaky(1, http.StatusServiceUnavailable)
	defer server.Close()
	_, _, _, client := harness()

	request, err := http.NewRequestWithContext(httpretry.Idempotent(context.Background()), http.MethodPost, server.URL, io.NopCloser(strings.NewReader("stream")))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	readBody(t, response)
	if response.StatusCode != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatalf("status %d after %d calls", response.StatusCode, calls.Load())
	}
}

func TestCancellationInterruptsTheWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "4")
		http.Error(w, "later", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := &http.Client{Transport: httpretry.New(nil, httpretry.Options{Service: "fake"})}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err = client.Do(request)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("the wait was not interrupted")
	}
}

func TestNoWaitBeyondTheContextDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "later", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var waits atomic.Int32
	client := &http.Client{Transport: httpretry.New(nil, httpretry.Options{
		Service: "fake",
		Sleep:   func(context.Context, time.Duration) error { waits.Add(1); return nil },
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	readBody(t, response)
	if response.StatusCode != http.StatusServiceUnavailable || waits.Load() != 0 {
		t.Fatalf("status %d, %d waits", response.StatusCode, waits.Load())
	}
}

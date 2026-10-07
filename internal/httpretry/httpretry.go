// Package httpretry retries transient failures of calls to external services with one policy, so
// every connector behaves alike without changing its call sites.
package httpretry

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	MaximumAttempts = 4
	Window          = 5 * time.Second
	initialBackoff  = 250 * time.Millisecond
	maximumBackoff  = 2 * time.Second
	drainLimit      = 64 << 10
)

// Outcomes of a request that needed more than one look: it was retried and ended well, it ran out
// of attempts or time, or its failure could not be retried at all.
const (
	OutcomeRecovered    = "recovered"
	OutcomeExhausted    = "exhausted"
	OutcomeNotRetryable = "not_retryable"
)

// Observer receives one record per request that failed transiently at least once.
type Observer interface {
	RecordHTTPRetry(ctx context.Context, service, outcome string)
}

type idempotentKey struct{}

// Idempotent marks the requests made with the returned context as safe to repeat, which lets the
// transport retry a POST that only reads.
func Idempotent(ctx context.Context) context.Context {
	return context.WithValue(ctx, idempotentKey{}, true)
}

type defaults struct {
	observer Observer
	logger   *slog.Logger
}

var processDefaults atomic.Pointer[defaults]

// SetDefaults sets the observer and logger used by transports that were not given their own.
func SetDefaults(observer Observer, logger *slog.Logger) {
	processDefaults.Store(&defaults{observer: observer, logger: logger})
}

// Options configure a transport. Everything but Service is optional.
type Options struct {
	Service  string
	Observer Observer
	Logger   *slog.Logger
	Now      func() time.Time
	// Sleep waits for d, returning the context's error if it ends first.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter returns a duration in [0, limit].
	Jitter func(limit time.Duration) time.Duration
}

type transport struct {
	base http.RoundTripper
	opts Options
}

// New wraps base, or http.DefaultTransport when it is nil.
func New(base http.RoundTripper, opts Options) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleep
	}
	if opts.Jitter == nil {
		opts.Jitter = func(limit time.Duration) time.Duration { return rand.N(limit + 1) }
	}
	return &transport{base: base, opts: opts}
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 || skipWaits.Load() {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	started := t.opts.Now()
	retried := false
	for attempt := 1; ; attempt++ {
		attemptRequest := request
		if attempt > 1 {
			attemptRequest = request.Clone(ctx)
			if request.GetBody != nil {
				body, err := request.GetBody()
				if err != nil {
					return nil, err
				}
				attemptRequest.Body = body
			}
		}
		response, err := t.base.RoundTrip(attemptRequest)

		status := 0
		if response != nil {
			status = response.StatusCode
		}
		reason, nothingSent := classify(ctx, response, err)
		if reason == "" {
			if retried {
				t.record(ctx, OutcomeRecovered)
			}
			return response, err
		}
		if !t.mayRepeat(request, nothingSent) {
			t.record(ctx, OutcomeNotRetryable)
			return response, err
		}
		if attempt >= MaximumAttempts {
			t.record(ctx, OutcomeExhausted)
			return response, err
		}
		wait, fits, asked := t.waitFor(response, attempt, started)
		if !fits {
			if asked {
				t.record(ctx, OutcomeNotRetryable)
			} else {
				t.record(ctx, OutcomeExhausted)
			}
			return response, err
		}
		if deadline, has := ctx.Deadline(); has && !t.opts.Now().Add(wait).Before(deadline) {
			t.record(ctx, OutcomeExhausted)
			return response, err
		}
		if response != nil {
			drain(response)
		}
		t.log(ctx, attempt, status, reason)
		retried = true
		if err := t.opts.Sleep(ctx, wait); err != nil {
			t.record(ctx, OutcomeExhausted)
			return nil, err
		}
	}
}

// mayRepeat reports whether the request can be sent again: its method must be safe or marked so,
// or the failure must prove nothing reached the server, and its body must be replayable.
func (t *transport) mayRepeat(request *http.Request, nothingSent bool) bool {
	if request.Body != nil && request.Body != http.NoBody && request.GetBody == nil {
		return false
	}
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if marked, _ := request.Context().Value(idempotentKey{}).(bool); marked {
		return true
	}
	return nothingSent
}

// waitFor picks the pause before the next attempt: the server's Retry-After on a 429 or 503, else
// full-jitter exponential backoff. It reports false when the pause would start the next attempt
// outside the retry window; asked is true when that pause was the server's.
func (t *transport) waitFor(response *http.Response, attempt int, started time.Time) (wait time.Duration, fits, asked bool) {
	wait, asked = retryAfter(response, t.opts.Now())
	if !asked {
		wait = t.opts.Jitter(min(maximumBackoff, initialBackoff<<(attempt-1)))
	}
	return wait, t.opts.Now().Add(wait).Sub(started) <= Window, asked
}

// retryAfter returns the delay a 429 or 503 asks for, given as seconds or as an HTTP date.
func retryAfter(response *http.Response, now time.Time) (time.Duration, bool) {
	if response == nil || (response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusServiceUnavailable) {
		return 0, false
	}
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}

// classify names the transient failure in an attempt, or returns "" when there is none.
// nothingSent is true when the failure proves the server never saw the request.
func classify(ctx context.Context, response *http.Response, err error) (reason string, nothingSent bool) {
	if err != nil {
		if ctx.Err() != nil {
			return "", false
		}
		return classifyError(err)
	}
	switch response.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "status", false
	}
	return "", false
}

func classifyError(err error) (string, bool) {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns", true
	}
	var op *net.OpError
	if errors.As(err, &op) {
		if op.Op == "dial" {
			return "connection", true
		}
		return "connection", false
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout", false
	}
	var header tls.RecordHeaderError
	switch {
	case errors.As(err, &header),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.EPIPE),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection", errors.Is(err, syscall.ECONNREFUSED)
	}
	return "", false
}

func drain(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, drainLimit))
	_ = response.Body.Close()
}

func (t *transport) record(ctx context.Context, outcome string) {
	observer := t.opts.Observer
	if observer == nil {
		if d := processDefaults.Load(); d != nil {
			observer = d.observer
		}
	}
	if observer != nil {
		observer.RecordHTTPRetry(ctx, t.opts.Service, outcome)
	}
}

func (t *transport) log(ctx context.Context, attempt, status int, reason string) {
	logger := t.opts.Logger
	if logger == nil {
		if d := processDefaults.Load(); d != nil {
			logger = d.logger
		}
	}
	if logger != nil {
		logger.DebugContext(ctx, "httpretry.retry", "service", t.opts.Service, "attempt", attempt, "status", status, "reason", reason)
	}
}

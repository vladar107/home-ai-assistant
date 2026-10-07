package httpretry

import "sync/atomic"

var skipWaits atomic.Bool

// WithoutWaits makes transports that sleep by default return at once until the test ends, so
// tests of a connector see every attempt without waiting out the backoff.
func WithoutWaits(t interface{ Cleanup(func()) }) {
	skipWaits.Store(true)
	t.Cleanup(func() { skipWaits.Store(false) })
}

package transit

import (
	"context"
	"sync"
	"time"
)

// Directory is reference data, such as a station or stop list, fetched on first use and again once
// it is older than Lifetime. Concurrent callers wait on one fetch instead of repeating it.
type Directory[T any] struct {
	Fetch    func(context.Context) (T, error)
	Now      func() time.Time
	Lifetime time.Duration
	// RetryDelay keeps a failing refresh from costing a request on every call while the stale data
	// still answers.
	RetryDelay time.Duration
	// Interrupted turns the context error of a caller that stopped waiting for another caller's
	// fetch into the error it returns.
	Interrupted func(error) error

	mu         sync.Mutex
	value      T
	loaded     bool
	fetchedAt  time.Time
	retryAfter time.Time
	refresh    *refresh[T]
}

// refresh is one fetch that concurrent callers wait on instead of repeating.
type refresh[T any] struct {
	done  chan struct{}
	value T
	err   error
	// abandoned marks a fetch that failed because its caller gave up, which says nothing about the
	// service, so waiters try again themselves.
	abandoned bool
}

// Load returns the data, or the zero value when none was ever fetched. A refresh that fails while
// older data is at hand returns that data together with the refresh error, so the caller can log
// it and carry on.
func (d *Directory[T]) Load(ctx context.Context) (T, error) {
	for {
		d.mu.Lock()
		now := d.Now()
		if d.loaded && (now.Sub(d.fetchedAt) < d.Lifetime || now.Before(d.retryAfter)) {
			value := d.value
			d.mu.Unlock()
			return value, nil
		}
		if running := d.refresh; running != nil {
			stale := d.value
			d.mu.Unlock()
			select {
			case <-running.done:
				if running.abandoned {
					continue
				}
				return running.value, running.err
			case <-ctx.Done():
				return stale, d.Interrupted(ctx.Err())
			}
		}
		running := &refresh[T]{done: make(chan struct{})}
		d.refresh = running
		d.mu.Unlock()

		value, err := d.Fetch(ctx)

		d.mu.Lock()
		switch {
		case err == nil:
			d.value, d.loaded, d.fetchedAt, d.retryAfter = value, true, now, time.Time{}
		case ctx.Err() != nil:
			running.abandoned = true
		case d.loaded:
			d.retryAfter = now.Add(d.RetryDelay)
		}
		running.value, running.err = d.value, err
		d.refresh = nil
		d.mu.Unlock()
		close(running.done)
		return running.value, running.err
	}
}

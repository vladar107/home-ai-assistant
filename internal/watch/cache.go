package watch

import (
	"context"
	"fmt"
	"sync"
)

// Cache holds the answers to upstream queries made during one check cycle, so watches that ask
// the same thing, such as two departures between the same stations, cause one request. A new
// cache starts every cycle, so nothing is reused across cycles.
type Cache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	done  chan struct{}
	value any
	err   error
}

func NewCache() *Cache {
	return &Cache{entries: make(map[string]*cacheEntry)}
}

// Cached returns the cycle's answer for key, calling fetch only for the first query with that key;
// a concurrent query waits for it. Failures are kept too, so a failing service is asked once per
// cycle. The key must name everything the answer depends on, including the principal when access
// to the service is per user. A nil cache always fetches.
func Cached[T any](ctx context.Context, cache *Cache, key string, fetch func(context.Context) (T, error)) (T, error) {
	if cache == nil {
		return fetch(ctx)
	}
	cache.mu.Lock()
	entry, found := cache.entries[key]
	if !found {
		entry = &cacheEntry{done: make(chan struct{})}
		cache.entries[key] = entry
	}
	cache.mu.Unlock()

	var zero T
	if !found {
		entry.value, entry.err = fetch(ctx)
		close(entry.done)
	} else {
		select {
		case <-entry.done:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	if entry.err != nil {
		return zero, entry.err
	}
	value, ok := entry.value.(T)
	if !ok {
		return zero, fmt.Errorf("watch cache key %q holds another type", key)
	}
	return value, nil
}

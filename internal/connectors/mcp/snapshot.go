package mcp

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
)

const (
	listLifetime = 5 * time.Minute
	// staleListLimit is how long a list keeps serving after refreshes start failing, so a server
	// restart does not take its tools away mid-conversation.
	staleListLimit        = time.Hour
	defaultRefreshTimeout = 3 * time.Second
	// A failed refresh is not retried before the backoff has passed, so a server that is down or
	// hanging costs one background refresh per backoff rather than one per turn.
	initialBackoff = 30 * time.Second
	maximumBackoff = 5 * time.Minute
)

// listEntry is one cached tool list: a server's for a server without keys, a principal's for a
// keyed one. Its lock is never held across a request.
type listEntry struct {
	mu          sync.Mutex
	fingerprint string
	mapped      []mappedTool
	fetched     time.Time
	listed      bool
	// attempted reports that a refresh finished, well or not; until then a turn waits for one.
	attempted bool
	failures  int
	retryAt   time.Time
	// refreshing is closed when the refresh in flight ends; nil when none is.
	refreshing chan struct{}
}

// reset forgets the list of a replaced key; the caller holds the entry's lock.
func (e *listEntry) reset(fingerprint string) {
	e.fingerprint, e.mapped, e.fetched, e.listed, e.attempted = fingerprint, nil, time.Time{}, false, false
	e.failures, e.retryAt, e.refreshing = 0, time.Time{}, nil
}

func (e *listEntry) staleOrNil(now time.Time) []mappedTool {
	if e.listed && now.Sub(e.fetched) < staleListLimit {
		return e.mapped
	}
	return nil
}

func (e *listEntry) has(name string) bool {
	for _, tool := range e.mapped {
		if tool.definition.Name == name {
			return true
		}
	}
	return false
}

// listKey is where a server's list and the session that refreshes it are kept: shared for a
// server without keys, since listing tools carries nothing of the user, and per principal for a
// keyed one.
func listKey(principalID string, server config.MCPServer) sessionKey {
	if server.Auth == config.MCPAuthKey {
		return sessionKey{server: server.Name, principal: principalID}
	}
	return sessionKey{server: server.Name}
}

func (s *Service) listEntryFor(key sessionKey) *listEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.lists[key]
	if entry == nil {
		entry = &listEntry{}
		s.lists[key] = entry
	}
	return entry
}

// toolList returns the server's mapped tools at once when it has a list under an hour old,
// refreshing it in the background once it is five minutes old and out of backoff. A list never
// refreshed, or one that aged past the hour while nobody asked, makes the turn wait, for at most
// the refresh timeout; before waiting, the refreshes of the principal's other lists that are due
// start too, so a turn that must wait waits for them together. A list that failed and has nothing
// to serve is skipped and retried in the background after its backoff.
func (s *Service) toolList(ctx context.Context, principalID string, key sessionKey, server config.MCPServer, cred credential, gen generation) []mappedTool {
	entry := s.listEntryFor(key)
	entry.mu.Lock()
	if fresh, ok := s.startIfDueLocked(entry, key, server, cred, gen); ok {
		defer entry.mu.Unlock()
		return fresh
	}
	served := entry.staleOrNil(s.now())
	wait := entry.refreshing
	if entry.attempted && (served != nil || entry.failures > 0) {
		wait = nil
	}
	entry.mu.Unlock()
	if wait == nil {
		return served
	}
	s.refreshDue(ctx, principalID, server.Name)
	select {
	case <-wait:
	case <-ctx.Done():
		return nil
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.fingerprint != cred.fingerprint {
		return nil
	}
	return entry.staleOrNil(s.now())
}

// startIfDueLocked returns the entry's list when it is fresh, and otherwise starts its refresh
// unless one is running or the entry is in backoff. The caller holds the entry's lock.
func (s *Service) startIfDueLocked(entry *listEntry, key sessionKey, server config.MCPServer, cred credential, gen generation) ([]mappedTool, bool) {
	if entry.fingerprint != cred.fingerprint {
		entry.reset(cred.fingerprint)
	}
	now := s.now()
	if entry.listed && now.Sub(entry.fetched) < listLifetime {
		return entry.mapped, true
	}
	if entry.refreshing == nil && !now.Before(entry.retryAt) {
		entry.refreshing = s.startRefresh(entry, key, server, cred, gen)
	}
	return nil, false
}

// refreshDue starts the refresh of every other list the principal's turn will ask for that is
// due, without waiting for any of them.
func (s *Service) refreshDue(ctx context.Context, principalID, except string) {
	for _, p := range s.providers {
		if p.server.Name == except || !p.server.Allows(principalID) {
			continue
		}
		if enabled, err := s.switches.Enabled(ctx, principalID, p.sw); err != nil || !enabled {
			continue
		}
		key := listKey(principalID, p.server)
		gen := s.generation(key)
		cred, state, err := s.credentialFor(ctx, principalID, p.server)
		if err != nil || !usable(state) {
			continue
		}
		entry := s.listEntryFor(key)
		entry.mu.Lock()
		s.startIfDueLocked(entry, key, p.server, cred, gen)
		entry.mu.Unlock()
	}
}

// startRefresh runs one refresh of the entry in the background and returns the channel closed when
// it ends, or nil when the service is not running. The caller holds the entry's lock.
func (s *Service) startRefresh(entry *listEntry, key sessionKey, server config.MCPServer, cred credential, gen generation) chan struct{} {
	done := make(chan struct{})
	if !s.track(func(lifecycle context.Context) { s.refresh(lifecycle, entry, done, key, server, cred, gen) }) {
		return nil
	}
	return done
}

func (s *Service) refresh(lifecycle context.Context, entry *listEntry, done chan struct{}, key sessionKey, server config.MCPServer, cred credential, gen generation) {
	ctx, cancel := context.WithTimeout(lifecycle, s.refreshTimeout)
	defer cancel()
	listed, err := s.listTools(ctx, key, server, cred, gen)
	var mapped []mappedTool
	dropped := 0
	if err == nil {
		mapped, dropped = mapTools(server, listed)
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	defer close(done)
	if entry.refreshing != done || entry.fingerprint != cred.fingerprint {
		return
	}
	entry.refreshing = nil
	entry.attempted = true
	now := s.now()
	switch {
	case err == nil:
		entry.mapped, entry.fetched, entry.listed = mapped, now, true
		entry.failures, entry.retryAt = 0, time.Time{}
		if dropped > 0 {
			s.logger.WarnContext(lifecycle, "mcp.tools.dropped", "server", server.Name, "count", dropped)
		}
	// Shutting down or a key retired meanwhile says nothing about the server.
	case lifecycle.Err() != nil, errors.Is(err, errRetired), errors.Is(err, ErrCancelled):
	default:
		if errors.Is(err, ErrUnauthorized) {
			s.markProblem(lifecycle, key.principal, server)
		}
		entry.failures++
		backoff := initialBackoff
		for i := 1; i < entry.failures && backoff < maximumBackoff; i++ {
			backoff *= 2
		}
		entry.retryAt = now.Add(min(backoff, maximumBackoff))
		s.logger.WarnContext(lifecycle, "mcp.refresh.failed", "principal_id", key.principal, "server", server.Name, "class", ClassOf(err), "stale", entry.staleOrNil(now) != nil)
	}
}

func (s *Service) listTools(ctx context.Context, key sessionKey, server config.MCPServer, cred credential, gen generation) ([]Tool, error) {
	client, release, err := s.clientFor(key, server, cred, gen)
	if err != nil {
		return nil, err
	}
	defer release()
	return client.ListTools(ctx)
}

// warm starts the first refresh of a server without keys, so the first turn rarely waits.
func (s *Service) warm(server config.MCPServer) {
	key := listKey("", server)
	gen := s.generation(key)
	entry := s.listEntryFor(key)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.refreshing == nil && !entry.attempted {
		entry.refreshing = s.startRefresh(entry, key, server, credential{}, gen)
	}
}

// offers reports whether the cached list for the key, when there is one, still offers the name,
// so a tool the list dropped as malformed or colliding is not run either.
func (s *Service) offers(key sessionKey, name string) bool {
	s.mu.Lock()
	entry := s.lists[key]
	s.mu.Unlock()
	if entry == nil {
		return true
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return !entry.listed || entry.has(name)
}

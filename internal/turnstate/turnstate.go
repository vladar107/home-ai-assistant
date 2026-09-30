// Package turnstate holds, per principal, the last few user messages and whether outside content
// has recently reached the model. Guards read it to judge an action against what the user asked
// and how much the turn can be trusted. It is memory-only by design: a restart forgets it, and
// pending approvals expire on restart anyway.
package turnstate

import (
	"strings"
	"sync"
	"time"
)

const (
	// Window is how long a message or an exposure mark stays relevant.
	Window = 30 * time.Minute
	// MaxMessages is how many recent messages are kept per principal.
	MaxMessages = 2
)

// Level is how far a principal's recent turns have been exposed to content from outside the
// household.
type Level int

const (
	LevelNone Level = iota
	// LevelExposed means a tool returned third-party content recently.
	LevelExposed
	// LevelSuspicious means screening flagged that content.
	LevelSuspicious
)

type message struct {
	text string
	at   time.Time
}

type entry struct {
	messages   []message
	exposedAt  time.Time
	suspicious time.Time
}

// Registry is safe for concurrent use; tool calls of one turn may run in parallel.
type Registry struct {
	now func() time.Time

	mu         sync.Mutex
	principals map[string]*entry
}

// New returns an empty registry; a nil now means time.Now.
func New(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{now: now, principals: make(map[string]*entry)}
}

// RecordMessage remembers a user message, dropping the oldest beyond MaxMessages. Blank text is
// ignored.
func (r *Registry) RecordMessage(principalID, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	e := r.entryLocked(principalID)
	e.messages = append(e.messages, message{text: text, at: now})
	if len(e.messages) > MaxMessages {
		e.messages = e.messages[len(e.messages)-MaxMessages:]
	}
}

// RecentMessages returns the principal's messages from the last Window, oldest first.
func (r *Registry) RecentMessages(principalID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	e, ok := r.principals[principalID]
	if !ok {
		return nil
	}
	texts := make([]string, 0, len(e.messages))
	for _, m := range e.messages {
		texts = append(texts, m.text)
	}
	return texts
}

// MarkExposed records that a tool returned third-party content to the principal's turn.
func (r *Registry) MarkExposed(principalID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	r.entryLocked(principalID).exposedAt = now
}

// MarkSuspicious records that screening flagged content; it implies exposure.
func (r *Registry) MarkSuspicious(principalID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	e := r.entryLocked(principalID)
	e.exposedAt = now
	e.suspicious = now
}

// Exposure returns the highest mark still within Window.
func (r *Registry) Exposure(principalID string) Level {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	e, ok := r.principals[principalID]
	switch {
	case !ok:
		return LevelNone
	case !e.suspicious.IsZero():
		return LevelSuspicious
	case !e.exposedAt.IsZero():
		return LevelExposed
	}
	return LevelNone
}

func (r *Registry) entryLocked(principalID string) *entry {
	e, ok := r.principals[principalID]
	if !ok {
		e = &entry{}
		r.principals[principalID] = e
	}
	return e
}

// pruneLocked runs on every access, which keeps memory bounded by the principals active within
// the window without a background sweeper.
func (r *Registry) pruneLocked(now time.Time) {
	cutoff := now.Add(-Window)
	for id, e := range r.principals {
		kept := e.messages[:0]
		for _, m := range e.messages {
			if m.at.After(cutoff) {
				kept = append(kept, m)
			}
		}
		e.messages = kept
		if !e.exposedAt.After(cutoff) {
			e.exposedAt = time.Time{}
		}
		if !e.suspicious.After(cutoff) {
			e.suspicious = time.Time{}
		}
		if len(e.messages) == 0 && e.exposedAt.IsZero() && e.suspicious.IsZero() {
			delete(r.principals, id)
		}
	}
}

// Package turnstate holds, per principal, the last few user messages and the assistant's latest chat
// reply in each Telegram tab, and whether outside content has recently reached the model. Guards read it to judge an
// action against what the user asked and how much the turn can be trusted. It is memory-only by
// design: a restart forgets it, and pending approvals expire on restart anyway.
package turnstate

import (
	"strings"
	"sync"
	"time"
)

const (
	// Window is how long a message or an exposure mark stays relevant.
	Window = 30 * time.Minute
	// MaxMessages is how many recent messages are kept per tab.
	MaxMessages = 2
	// MaxReplyRunes bounds a kept reply to its end, where the question a user answers usually is.
	// A reader scrubs the reply and then keeps only a much shorter end of it, so a fragment this
	// cut leaves at the start never leaves the process.
	MaxReplyRunes = 4000
)

// Roles of the lines of an exchange.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Line is one message of a recent exchange.
type Line struct {
	Role string
	Text string
}

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
	// seq orders messages and replies recorded within the same clock reading.
	seq uint64
}

// exchange is what one tab said most recently.
type exchange struct {
	messages []message
	reply    message
}

// entry keeps exposure per principal rather than per tab: outside content one tab read can still
// steer what the principal does in another.
type entry struct {
	exchanges  map[int64]*exchange
	exposedAt  time.Time
	suspicious time.Time
}

// Registry is safe for concurrent use; tool calls of one turn may run in parallel.
type Registry struct {
	now func() time.Time

	mu         sync.Mutex
	principals map[string]*entry
	seq        uint64
}

// New returns an empty registry; a nil now means time.Now.
func New(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{now: now, principals: make(map[string]*entry)}
}

// RecordMessage remembers a user message in a tab, dropping the oldest beyond MaxMessages. Blank
// text is ignored.
func (r *Registry) RecordMessage(principalID string, threadID int64, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	e := r.exchangeLocked(principalID, threadID)
	r.seq++
	e.messages = append(e.messages, message{text: text, at: now, seq: r.seq})
	if len(e.messages) > MaxMessages {
		e.messages = e.messages[len(e.messages)-MaxMessages:]
	}
}

// RecordReply remembers the assistant's latest chat reply in place of the previous one, keeping
// its last MaxReplyRunes runes from a line start, so the cut never splits a path or address for the
// scrubber to miss. Blank text is ignored.
func (r *Registry) RecordReply(principalID string, threadID int64, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if runes := []rune(text); len(runes) > MaxReplyRunes {
		text = string(runes[len(runes)-MaxReplyRunes:])
		if _, rest, found := strings.Cut(text, "\n"); found {
			text = rest
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	r.seq++
	r.exchangeLocked(principalID, threadID).reply = message{text: text, at: now, seq: r.seq}
}

// RecentMessages returns the principal's messages in a tab from the last Window, oldest first.
func (r *Registry) RecentMessages(principalID string, threadID int64) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	e := r.findLocked(principalID, threadID)
	if e == nil {
		return nil
	}
	texts := make([]string, 0, len(e.messages))
	for _, m := range e.messages {
		texts = append(texts, m.text)
	}
	return texts
}

// RecentExchange returns the principal's messages and the assistant's latest reply in a tab from
// the last Window, oldest first.
func (r *Registry) RecentExchange(principalID string, threadID int64) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	e := r.findLocked(principalID, threadID)
	if e == nil {
		return nil
	}
	lines := make([]Line, 0, len(e.messages)+1)
	pending := e.reply.text != ""
	for _, m := range e.messages {
		if pending && e.reply.seq < m.seq {
			lines = append(lines, Line{Role: RoleAssistant, Text: e.reply.text})
			pending = false
		}
		lines = append(lines, Line{Role: RoleUser, Text: m.text})
	}
	if pending {
		lines = append(lines, Line{Role: RoleAssistant, Text: e.reply.text})
	}
	return lines
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
		e = &entry{exchanges: make(map[int64]*exchange)}
		r.principals[principalID] = e
	}
	return e
}

func (r *Registry) exchangeLocked(principalID string, threadID int64) *exchange {
	e := r.entryLocked(principalID)
	x, ok := e.exchanges[threadID]
	if !ok {
		x = &exchange{}
		e.exchanges[threadID] = x
	}
	return x
}

func (r *Registry) findLocked(principalID string, threadID int64) *exchange {
	e, ok := r.principals[principalID]
	if !ok {
		return nil
	}
	return e.exchanges[threadID]
}

// pruneLocked runs on every access, which keeps memory bounded by the principals active within
// the window without a background sweeper.
func (r *Registry) pruneLocked(now time.Time) {
	cutoff := now.Add(-Window)
	for id, e := range r.principals {
		for threadID, x := range e.exchanges {
			kept := x.messages[:0]
			for _, m := range x.messages {
				if m.at.After(cutoff) {
					kept = append(kept, m)
				}
			}
			x.messages = kept
			if !x.reply.at.After(cutoff) {
				x.reply = message{}
			}
			if len(x.messages) == 0 && x.reply.text == "" {
				delete(e.exchanges, threadID)
			}
		}
		if !e.exposedAt.After(cutoff) {
			e.exposedAt = time.Time{}
		}
		if !e.suspicious.After(cutoff) {
			e.suspicious = time.Time{}
		}
		if len(e.exchanges) == 0 && e.exposedAt.IsZero() && e.suspicious.IsZero() {
			delete(r.principals, id)
		}
	}
}

package tools

import (
	"context"
	"sync"
	"time"
)

// maximumLoggedCalls bounds the calls one log keeps; a run's budget allows far fewer.
const maximumLoggedCalls = 256

// CallLog is what the code that started a run learns from its tool calls themselves, rather than
// from what the model reports: whether the call budget refused a call, the newest receipt time
// of a message a connector read in full, such as Gmail's own received time, and how each call the
// runtime named ended and what it looked at.
type CallLog struct {
	mu        sync.Mutex
	exhausted bool
	newest    time.Time
	calls     map[string]CallRecord
	opened    map[openedKey]bool
}

type openedKey struct{ account, id string }

// CallRecord is what the broker learned of one call: how it ended and, when its tool names one, a
// short label of what it looked at. The label can be private, so it never reaches a log or
// telemetry.
type CallRecord struct {
	Outcome string
	Label   string
}

type callLogKey struct{}

// WithCallLog attaches a call log to the context tools are collected under; the tool set made
// under it records into it, and its calls carry it to the connectors.
func WithCallLog(ctx context.Context, log *CallLog) context.Context {
	return context.WithValue(ctx, callLogKey{}, log)
}

// CallLogFrom returns the call log of a call, or nil when nobody asked for one.
func CallLogFrom(ctx context.Context) *CallLog {
	log, _ := ctx.Value(callLogKey{}).(*CallLog)
	return log
}

// ReadMessage records that a message was read in full: the account and id the connector shows the
// model, when Gmail received it, and whether it was still unread when read. A zero received time
// leaves the newest receipt time alone.
func (l *CallLog) ReadMessage(account, id string, received time.Time, unread bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !received.IsZero() && received.After(l.newest) {
		l.newest = received
	}
	key := openedKey{account: account, id: id}
	if l.opened == nil {
		l.opened = make(map[openedKey]bool)
	}
	if _, kept := l.opened[key]; !kept && len(l.opened) >= maximumLoggedCalls {
		return
	}
	l.opened[key] = unread
}

// Opened reports whether the message was unread when a connector read it in full, and whether it
// was read in full at all.
func (l *CallLog) Opened(account, id string) (unread bool, found bool) {
	if l == nil {
		return false, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	unread, found = l.opened[openedKey{account: account, id: id}]
	return unread, found
}

func (l *CallLog) refused() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.exhausted = true
}

// record keeps how the call with the runtime's id ended; a call without an id cannot be matched to
// its events, so it is not kept.
func (l *CallLog) record(id, outcome, label string) {
	if id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls == nil {
		l.calls = make(map[string]CallRecord)
	}
	if _, kept := l.calls[id]; !kept && len(l.calls) >= maximumLoggedCalls {
		return
	}
	l.calls[id] = CallRecord{Outcome: outcome, Label: label}
}

// Call returns what the broker learned of the call with the runtime's id.
func (l *CallLog) Call(id string) (CallRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	record, found := l.calls[id]
	return record, found
}

// Exhausted reports whether the budget refused a call.
func (l *CallLog) Exhausted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exhausted
}

// NewestRead is the newest receipt time a connector recorded, or zero for none.
func (l *CallLog) NewestRead() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.newest
}

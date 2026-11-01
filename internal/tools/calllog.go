package tools

import (
	"context"
	"sync"
	"time"
)

// CallLog is what the code that started a run learns from its tool calls themselves, rather than
// from what the model reports: whether the call budget refused a call, and the newest receipt time
// of a message a connector read in full, such as Gmail's own received time.
type CallLog struct {
	mu        sync.Mutex
	exhausted bool
	newest    time.Time
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

// Read records that a message received at the given instant was read in full.
func (l *CallLog) Read(received time.Time) {
	if l == nil || received.IsZero() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if received.After(l.newest) {
		l.newest = received
	}
}

func (l *CallLog) refused() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.exhausted = true
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

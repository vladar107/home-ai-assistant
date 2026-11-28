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
	opened    map[openedKey]OpenedMessage
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

// OpenedMessage is what a connector learned of a message it read in full, from the message
// itself rather than from what the model reports.
type OpenedMessage struct {
	// Received is when the mailbox received it, zero when unknown.
	Received time.Time
	// Unread is whether it was still unread when read.
	Unread bool
	// From is the sender's address, lowercase, empty when the message has no single readable one.
	From string
	// To is the first address the To header names, lowercase, empty when it names none.
	To string
	// ThreadID is the mailbox's thread of the message.
	ThreadID string
	// DMARCPass is whether the mailbox's own authentication check shows DMARC passing for the
	// sender's domain.
	DMARCPass bool
}

// ReadMessage records that a message was read in full: the account and id the connector shows the
// model, when Gmail received it, and whether it was still unread when read. A zero received time
// leaves the newest receipt time alone.
func (l *CallLog) ReadMessage(account, id string, received time.Time, unread bool) {
	l.Read(account, id, OpenedMessage{Received: received, Unread: unread})
}

// Read records that a message was read in full, by the account and id the connector shows the
// model, with what the connector learned of it.
func (l *CallLog) Read(account, id string, message OpenedMessage) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !message.Received.IsZero() && message.Received.After(l.newest) {
		l.newest = message.Received
	}
	key := openedKey{account: account, id: id}
	if l.opened == nil {
		l.opened = make(map[openedKey]OpenedMessage)
	}
	if _, kept := l.opened[key]; !kept && len(l.opened) >= maximumLoggedCalls {
		return
	}
	l.opened[key] = message
}

// Opened reports whether the message was unread when a connector read it in full, and whether it
// was read in full at all.
func (l *CallLog) Opened(account, id string) (unread bool, found bool) {
	message, found := l.Message(account, id)
	return message.Unread, found
}

// Message returns what a connector learned of the message when it read it in full, and whether it
// was read in full at all.
func (l *CallLog) Message(account, id string) (OpenedMessage, bool) {
	if l == nil {
		return OpenedMessage{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	message, found := l.opened[openedKey{account: account, id: id}]
	return message, found
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

// NewestReadWhere is the newest receipt time of the messages read in full that keep accepts, by
// their account and id, or zero for none.
func (l *CallLog) NewestReadWhere(keep func(account, id string) bool) time.Time {
	if l == nil {
		return time.Time{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var newest time.Time
	for key, message := range l.opened {
		if message.Received.After(newest) && keep(key.account, key.id) {
			newest = message.Received
		}
	}
	return newest
}

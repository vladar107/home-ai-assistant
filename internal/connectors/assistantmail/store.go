package assistantmail

import (
	"context"
	"time"
)

// Direction says whether a routed message arrived in the assistant's mailbox or was sent from it.
type Direction string

const (
	DirectionIn  Direction = "in"
	DirectionOut Direction = "out"
)

// Route ties one mailbox message to one principal. Gmail message and thread ids are opaque.
type Route struct {
	GmailID      string
	PrincipalID  string
	ThreadID     string
	Direction    Direction
	InternalDate time.Time
	Spam         bool
	RecordedAt   time.Time
	// NotifiedAt is zero until the principal has been told about an incoming message.
	NotifiedAt time.Time
}

// SwitchState is a principal's Assistant mailbox switch as the poller last saw it.
type SwitchState struct {
	Enabled bool
	// OnSince is when the switch was last turned on; mail Gmail received before it is never
	// announced.
	OnSince time.Time
	// ChangedAt is the switch's own change time when it was seen, zero while the principal has
	// never chosen.
	ChangedAt time.Time
}

// Repository stores what the mailbox poller has seen, who each message belongs to, how each
// principal's switch was last seen, and the sends counted against each principal's daily limit.
type Repository interface {
	// RecordSeen stores the message id with its routes in one transaction. An id that was already
	// seen changes nothing and reports fresh as false. A message with no routes is still recorded.
	RecordSeen(ctx context.Context, gmailID string, internalDate time.Time, routes []Route) (fresh bool, err error)
	// RecordSent records a message the assistant sent for the principal as an outgoing route,
	// even when the message was already seen.
	RecordSent(ctx context.Context, gmailID, threadID, principalID string, internalDate, at time.Time) error
	// PrincipalsForThread returns the distinct principals with a sent message in the thread.
	PrincipalsForThread(ctx context.Context, threadID string) ([]string, error)
	// NewestSeenInternalDate returns the internal date of the newest seen message, for recovering
	// the poll position.
	NewestSeenInternalDate(ctx context.Context) (time.Time, bool, error)
	// Seen reports whether the message id was already recorded, by the poller or at send.
	Seen(ctx context.Context, gmailID string) (bool, error)
	HistoryCursor(ctx context.Context) (string, bool, error)
	// HistoryCursorSavedAt returns when the history cursor was last saved, the point from which an
	// expired cursor is recovered when no dated message was seen.
	HistoryCursorSavedAt(ctx context.Context) (time.Time, bool, error)
	SaveHistoryCursor(ctx context.Context, historyID string) error
	// PendingNotices returns the principal's incoming routes not yet notified, oldest first. Spam
	// routes are included; the caller decides how to show them.
	PendingNotices(ctx context.Context, principalID string, limit int) ([]Route, error)
	MarkNotified(ctx context.Context, principalID string, gmailIDs []string, at time.Time) error
	// MarkNotifiedBefore marks the principal's unnotified incoming routes with an internal date
	// before the given time as notified.
	MarkNotifiedBefore(ctx context.Context, principalID string, before time.Time, at time.Time) error
	// ListMailRoutes returns the principal's own routes in both directions, newest first.
	ListMailRoutes(ctx context.Context, principalID string, limit int) ([]Route, error)
	// ListThreadRoutes returns the principal's own routes in one thread, in both directions, newest
	// first.
	ListThreadRoutes(ctx context.Context, principalID, threadID string, limit int) ([]Route, error)
	// Route returns the principal's own route to the message; another principal's route is not found.
	Route(ctx context.Context, principalID, gmailID string) (Route, bool, error)
	MailSwitchState(ctx context.Context, principalID string) (SwitchState, bool, error)
	SaveMailSwitchState(ctx context.Context, principalID string, state SwitchState, at time.Time) error
	// IntegrationSwitchChangedAt returns when the principal last changed the integration's switch,
	// or false when they never chose.
	IntegrationSwitchChangedAt(ctx context.Context, principalID, integration string) (time.Time, bool, error)
	// StartSend counts a send against the principal's limit for the day [dayStart, dayEnd) and
	// reports whether it may go ahead. Starting the same sendID again is allowed and not counted twice.
	StartSend(ctx context.Context, principalID, sendID string, dayStart, dayEnd time.Time, limit int, at time.Time) (allowed bool, err error)
	// RecordContacts remembers that the principal has sent mail to each address, compared without
	// regard to case.
	RecordContacts(ctx context.Context, principalID string, addresses []string, at time.Time) error
	// KnownContacts returns, lowercase, those of the addresses the principal has sent mail to before.
	KnownContacts(ctx context.Context, principalID string, addresses []string) ([]string, error)
}

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
	// FirstThreadRoute returns the thread's earliest route of any principal, by internal date.
	FirstThreadRoute(ctx context.Context, threadID string) (Route, bool, error)
	// StartRoutineReply counts a routine reply against its thread, which takes one ever, the
	// principal's routine replies of the day, and their sends of the day, in one transaction, and
	// records it in both when every limit allows it.
	StartRoutineReply(ctx context.Context, start RoutineStart) (RoutineVerdict, error)
	// UnreportedRoutineReplies returns the principal's routine replies that were sent, or may have
	// been, and that no delivered notice showed yet, oldest first, each with the arguments of its
	// approval record. A reply may have been sent when its send failed in a way that leaves it
	// unknown, such as a timeout or a restart, or when its record never finished.
	UnreportedRoutineReplies(ctx context.Context, principalID string) ([]RoutineReply, error)
	MarkRoutineRepliesReported(ctx context.Context, principalID string, sendIDs []string, at time.Time) error
}

// RoutineStart is a routine reply about to be sent, with the limits of the day [DayStart, DayEnd).
type RoutineStart struct {
	PrincipalID string
	ThreadID    string
	// SendID is the reply's approval record, which also counts it as a send.
	SendID     string
	DayStart   time.Time
	DayEnd     time.Time
	DailyLimit int
	SendLimit  int
	At         time.Time
}

// RoutineVerdict is whether a routine reply may go ahead: RoutineAllowed, or the limit it reached.
type RoutineVerdict string

const (
	RoutineAllowed RoutineVerdict = ""
	// RoutineThreadReplied means the thread already had its one routine reply.
	RoutineThreadReplied RoutineVerdict = "thread"
	// RoutineDailyLimit means the principal had all their routine replies of the day.
	RoutineDailyLimit RoutineVerdict = "day"
	// RoutineSendLimit means the principal had all their sends of the day.
	RoutineSendLimit RoutineVerdict = "send"
)

// RoutineReply is a routine reply that was sent, as its approval record keeps it, in its thread.
// Uncertain marks one that may or may not have gone out.
type RoutineReply struct {
	SendID    string
	ThreadID  string
	Arguments []byte
	StartedAt time.Time
	Uncertain bool
}

// Package trip is each user's record of trips: their dates, destination, and the bookings whose
// check-in the assistant reminds about. A trip is suggested when found in mail or a calendar,
// planned once the user accepts it or tells the assistant about it, and active between its start
// and end, when recurring jobs marked to pause while traveling stay silent.
package trip

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/localtime"
)

const (
	StatusSuggested = "suggested"
	StatusPlanned   = "planned"
	StatusDismissed = "dismissed"
	StatusCancelled = "cancelled"

	SourceChat     = "chat"
	SourceMail     = "mail"
	SourceCalendar = "calendar"

	KindFlight = "flight"
	KindHotel  = "hotel"
	KindTrain  = "train"
	KindCar    = "car"
	KindOther  = "other"

	CheckInStated   = "stated"
	CheckInLookedUp = "looked_up"
	CheckInDefault  = "default"

	OutcomeNew     = "new"
	OutcomeUpdated = "updated"
	OutcomeKnown   = "known"

	NoticeStart = "start"
	NoticeEnd   = "end"

	// UnsetPast, UnsetCap, and UnsetFar are why a booking with a check-in time got no reminder: it
	// has already opened, the scheduler is full, or it is more than a year ahead.
	UnsetPast = "past"
	UnsetCap  = "cap"
	UnsetFar  = "far"
	// UnsetAfterEnd means the check-in opens after the trip ends; UnsetInvalid that the scheduler
	// refused the time.
	UnsetAfterEnd = "after_end"
	UnsetInvalid  = "invalid"

	MaximumTitleRunes     = 80
	MaximumReferenceRunes = 40
	MaximumUnfinished     = 30
	MaximumSuggested      = 20
	MaximumBookings       = 20
)

// ErrReminderRefused is returned by SetReminder when the scheduler refuses the reminder itself,
// not for lack of room.
var ErrReminderRefused = errors.New("the reminder was refused")

// Trip is one journey. A zero StartedNotifiedAt or EndedNotifiedAt means that notice has not
// been sent, and a zero SummaryMessageID that the trip's tab holds no summary of it.
type Trip struct {
	ID                string
	PrincipalID       string
	Title             string
	Destination       string
	StartsAt          time.Time
	EndsAt            time.Time
	Zone              string
	Status            string
	Source            string
	StartedNotifiedAt time.Time
	EndedNotifiedAt   time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SummaryMessageID  int64
	Bookings          []Booking
}

// Active is true while the trip is planned and now is in [StartsAt, EndsAt).
func (t Trip) Active(now time.Time) bool {
	return t.Status == StatusPlanned && !now.Before(t.StartsAt) && now.Before(t.EndsAt)
}

// Ended is true from the trip's end on.
func (t Trip) Ended(now time.Time) bool {
	return !now.Before(t.EndsAt)
}

// EndLabel is the trip's end in its zone. An end at local midnight is the start of the next day,
// so it reads as the previous day, as a date alone with dateLayout; any other end uses layout.
func (t Trip) EndLabel(layout, dateLayout string) string {
	return endLabel(t.StartsAt, t.EndsAt, localtime.SavedZone(t.Zone), layout, dateLayout)
}

func endLabel(start, end time.Time, location *time.Location, layout, dateLayout string) string {
	local := end.In(location)
	if local.Hour() == 0 && local.Minute() == 0 && local.Second() == 0 && end.After(start) {
		return local.AddDate(0, 0, -1).Format(dateLayout)
	}
	return local.Format(layout)
}

// Booking is one flight, stay, train, or car with the instant its check-in or pickup opens. A
// zero CheckInAt means none is known and CheckInSource is then empty.
type Booking struct {
	ID            string
	Kind          string
	Title         string
	Reference     string
	StartsAt      time.Time
	CheckInAt     time.Time
	CheckInSource string
	// Fingerprint identifies the booking across mail, calendar, and chat; the service computes it
	// and it is empty when the booking has no reference to identify it by.
	Fingerprint string
	// SourceKey is a calendar event ID that stands in for the reference when the booking comes
	// from a calendar. It is not stored.
	SourceKey string
}

type Repository interface {
	// ListTrips returns the principal's trips with the status, with their bookings, earliest
	// start first.
	ListTrips(ctx context.Context, principalID, status string) ([]Trip, error)
	LoadTrip(ctx context.Context, principalID, id string) (Trip, bool, error)
	// FindTripsByFingerprints returns the principal's trips holding a booking with any of the
	// fingerprints, earliest start first.
	FindTripsByFingerprints(ctx context.Context, principalID string, fingerprints []string) ([]Trip, error)
	// ListActiveTrips returns the principal's planned trips with starts_at <= at < ends_at,
	// without their bookings.
	ListActiveTrips(ctx context.Context, principalID string, at time.Time) ([]Trip, error)
	// ListPlannedTrips returns every principal's planned trips that end after the instant, with
	// their bookings.
	ListPlannedTrips(ctx context.Context, after time.Time) ([]Trip, error)
	AddTrip(context.Context, Trip) error
	// UpdateTrip saves the trip's fields other than its owner, source, notice times, and summary,
	// and replaces its bookings.
	UpdateTrip(context.Context, Trip) (bool, error)
	// SetTripSummary records the message that sums the trip up in its tab; zero forgets it.
	SetTripSummary(ctx context.Context, principalID, id string, messageID int64) error
	// ListNoticeTrips returns every principal's trips that may owe a notice at now: planned or
	// cancelled, with no end notice sent, and either started or still ahead of their end.
	ListNoticeTrips(ctx context.Context, now time.Time) ([]Trip, error)
	// MarkTripStarted records the start notice once; false means it was already recorded.
	MarkTripStarted(ctx context.Context, principalID, id string, at time.Time) (bool, error)
	// MarkTripEnded records the end notice once; false means it was already recorded.
	MarkTripEnded(ctx context.Context, principalID, id string, at time.Time) (bool, error)
	// ResetTripNotices forgets the start and, or, end notice so the trip owes it again.
	ResetTripNotices(ctx context.Context, principalID, id string, started, ended bool) error
	// DeleteTrip deletes one suggested trip with its bookings.
	DeleteTrip(ctx context.Context, principalID, id string) error
	// PruneTrips deletes trips of every status that ended before the instant, with their bookings.
	PruneTrips(ctx context.Context, before time.Time) (int, error)
}

// SentUpdate is one item of news about a trip that was sent to its tab. Reference identifies the
// item within its source: a mail message ID, a page address, or a calendar event ID with what
// changed. Origin is the sender's or site's name and may be empty.
type SentUpdate struct {
	Source    string
	Reference string
	Origin    string
	Title     string
	Text      string
	SentAt    time.Time
}

// UpdateState is what the check for news about one trip remembers between runs. CalendarSnapshot
// is the JSON of the events from the last complete list, Missing the IDs absent from it, and
// MailReadAt how far back mail was last read. A zero time means it never happened.
type UpdateState struct {
	CalendarSnapshot json.RawMessage
	Missing          []string
	LastAttemptAt    time.Time
	LastSuccessAt    time.Time
	MailReadAt       time.Time
}

// UpdateLog keeps the news sent for each trip and the check's state; it is the SQLite store. Every
// method is scoped to the trip's owner and reads nothing of, and writes nothing for, another's.
type UpdateLog interface {
	// RecordSentUpdates stores the items in one transaction; an item whose source and reference
	// were already recorded for the trip is ignored.
	RecordSentUpdates(ctx context.Context, principalID, tripID string, items []SentUpdate) error
	// ListSentUpdates returns up to limit of the trip's sent items, newest first.
	ListSentUpdates(ctx context.Context, principalID, tripID string, limit int) ([]SentUpdate, error)
	// LoadUpdateState returns the trip's state; false means none was saved.
	LoadUpdateState(ctx context.Context, principalID, tripID string) (UpdateState, bool, error)
	// SaveUpdateState replaces the trip's whole state.
	SaveUpdateState(ctx context.Context, principalID, tripID string, state UpdateState) error
	// MarkUpdateAttempt sets only the trip's last attempt time, creating its state when it has none.
	MarkUpdateAttempt(ctx context.Context, principalID, tripID string, at time.Time) error
}

// Reminders sets the check-in reminders of planned trips; the adapter over the scheduler is
// NewScheduleReminders.
type Reminders interface {
	// SetReminder creates the one-time reminder with the ID or moves it to the new time, and
	// delivers it to the destination, which is its trip's. schedule.ErrTooManyJobs is returned
	// when it cannot be created for lack of room, and ErrReminderRefused when the scheduler
	// refuses the time or text.
	SetReminder(ctx context.Context, principalID, id string, at time.Time, zone, text, destination string) error
	// RemoveReminder deletes the reminder; one that does not exist is not an error.
	RemoveReminder(ctx context.Context, principalID, id string) error
	// SetReminderDestination gives the reminder the destination when it exists and has none, and
	// changes nothing else.
	SetReminderDestination(ctx context.Context, principalID, id, destination string) error
}

// Paused names what stays silent during a trip.
type Paused interface {
	TravelPausedNames(ctx context.Context, principalID string) ([]string, error)
}

// Sender delivers plain text about one of the principal's trips to their active chat, in the
// trip's tab while it has one.
type Sender interface {
	Send(ctx context.Context, principalID, tripID, text string) error
}

// CardSender delivers a suggested trip with Track and Ignore buttons.
type CardSender interface {
	SendTripCard(ctx context.Context, trip Trip) error
}

// Tabs keeps the tab of each planned trip and the summary pinned there; the Telegram side
// implements it. TripChanged hears of every write that may change what the summary shows, reads
// the trip again, and does nothing while the bot has tabs off.
type Tabs interface {
	TripChanged(ctx context.Context, trip Trip)
	// CheckTab says whether the trip, or with no ID a trip about to be added, could use one of the
	// principal's tabs, which the user is in. With named, only a tab the user named will do.
	CheckTab(ctx context.Context, principalID, tripID string, threadID int64, named bool) (TabMove, error)
	// TakeTab makes the tab the trip's, in place of the tab it had or a new one, checking again
	// that it can be used; its summary is then pinned there and everything it sends goes there.
	TakeTab(ctx context.Context, trip Trip, threadID int64, named bool) (TabMove, error)
}

// TabsOffRefusal is why no trip can use a tab while the bot has tabs off.
const TabsOffRefusal = "Tabs are off, so a trip cannot use a tab."

// TabMove is the outcome of a trip using a tab the user is in. A non-empty Refusal says in words
// for the model why it cannot; Already that the tab is already the trip's, and Left that the trip
// left a tab of its own for it.
type TabMove struct {
	Refusal string
	Already bool
	Left    bool
}

// TabClaim is the tab of the turn a trip is added in, for the trip to use from the start. With
// Named, only a tab the user named is used, and any other leaves the trip to get a tab of its own.
type TabClaim struct {
	ThreadID int64
	Named    bool
}

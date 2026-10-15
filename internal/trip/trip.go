// Package trip is each user's record of trips: their dates, destination, and the bookings whose
// check-in the assistant reminds about. A trip is suggested when found in mail or a calendar,
// planned once the user accepts it or tells the assistant about it, and active between its start
// and end, when recurring jobs marked to pause while traveling stay silent.
package trip

import (
	"context"
	"errors"
	"time"
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
// been sent.
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
	Bookings          []Booking
}

// Active is true while the trip is planned and now is in [StartsAt, EndsAt).
func (t Trip) Active(now time.Time) bool {
	return t.Status == StatusPlanned && !now.Before(t.StartsAt) && now.Before(t.EndsAt)
}

// EndLabel is the trip's end in its zone. An end at local midnight is the start of the next day,
// so it reads as the previous day, as a date alone with dateLayout; any other end uses layout.
func (t Trip) EndLabel(layout, dateLayout string) string {
	return endLabel(t.StartsAt, t.EndsAt, loadZone(t.Zone), layout, dateLayout)
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
	AddTrip(context.Context, Trip) error
	// UpdateTrip saves the trip's fields other than its owner, source, and notice times, and
	// replaces its bookings.
	UpdateTrip(context.Context, Trip) (bool, error)
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

// Reminders sets the check-in reminders of planned trips; the adapter over the scheduler is
// NewScheduleReminders.
type Reminders interface {
	// SetReminder creates the one-time reminder with the ID or moves it to the new time.
	// schedule.ErrTooManyJobs is returned when it cannot be created for lack of room, and
	// ErrReminderRefused when the scheduler refuses the time or text.
	SetReminder(ctx context.Context, principalID, id string, at time.Time, zone, text string) error
	// RemoveReminder deletes the reminder; one that does not exist is not an error.
	RemoveReminder(ctx context.Context, principalID, id string) error
}

// Paused names what stays silent during a trip.
type Paused interface {
	TravelPausedNames(ctx context.Context, principalID string) ([]string, error)
}

// Sender delivers plain text to the principal's active chat.
type Sender interface {
	Send(ctx context.Context, principalID, text string) error
}

// CardSender delivers a suggested trip with Track and Ignore buttons.
type CardSender interface {
	SendTripCard(ctx context.Context, trip Trip) error
}

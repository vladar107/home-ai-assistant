package trip

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/localtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	// turnLineHorizon is how far ahead an upcoming trip reaches the turn line.
	turnLineHorizon = 14 * 24 * time.Hour
	// pruneAfter is how long a trip is kept after it ends.
	pruneAfter = 30 * 24 * time.Hour
	// reminderHorizon is how far ahead the scheduler accepts a one-time reminder.
	reminderHorizon = 365 * 24 * time.Hour

	turnDateLayout = "Mon 2006-01-02"
)

type Config struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

type Service struct {
	repository Repository
	reminders  Reminders
	tabs       Tabs
	logger     *slog.Logger
	now        func() time.Time

	// mu serialises writes so the caps hold when a turn's tool calls run in parallel. Reminders
	// are synced under it, and the scheduler never calls back into this service.
	mu sync.Mutex
}

func NewService(cfg Config, repository Repository, reminders Reminders, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("trip repository is required")
	}
	if reminders == nil {
		return nil, fmt.Errorf("trip reminders are required")
	}
	if logger == nil {
		return nil, fmt.Errorf("trip logger is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{repository: repository, reminders: reminders, logger: logger, now: now}, nil
}

func (s *Service) Now() time.Time { return s.now() }

// SetTabs gives each planned trip its own tab with a pinned summary, at wiring time; without it
// trips have no tabs.
func (s *Service) SetTabs(tabs Tabs) {
	s.tabs = tabs
}

// changed tells the tabs of a write to the trip.
func (s *Service) changed(ctx context.Context, trip Trip) {
	if s.tabs != nil {
		s.tabs.TripChanged(ctx, trip)
	}
}

// announce tells the tabs of the trip a write saved. Writes defer it before taking the lock, so it
// runs once the lock is released and the Telegram round trips of one trip never hold up the writes
// of another.
func (s *Service) announce(ctx context.Context, result *Result) {
	if result.written {
		s.changed(ctx, result.Trip)
	}
}

// SetSummaryMessage records the message that sums the trip up in its tab; zero forgets it.
func (s *Service) SetSummaryMessage(ctx context.Context, principalID, id string, messageID int64) error {
	return s.repository.SetTripSummary(ctx, principalID, id, messageID)
}

// Settle runs once at start. Every planned trip that has not ended gives its destination to those
// of its check-in reminders that exist and were set before reminders had one. Nothing else is
// created or changed, so a reminder the user deleted or moved stays as they left it, and one about
// to fire is untouched; running it again changes nothing. Each trip is then handed to the tabs,
// which gives a trip without a tab its own.
func (s *Service) Settle(ctx context.Context) error {
	planned, err := s.repository.ListPlannedTrips(ctx, s.now())
	if err != nil {
		return err
	}
	for _, listed := range planned {
		if err := ctx.Err(); err != nil {
			return err
		}
		failed := false
		for _, booking := range listed.Bookings {
			if err := s.reminders.SetReminderDestination(ctx, listed.PrincipalID, ReminderID(listed.ID, booking.ID), thread.TripDestination(listed.ID)); err != nil {
				s.logger.WarnContext(ctx, "trip.settle.failed", "principal_id", listed.PrincipalID, "trip_id", listed.ID, "error", err)
				failed = true
				break
			}
		}
		if !failed {
			s.changed(ctx, listed)
		}
	}
	return nil
}

// Draft is the editable content of a trip. Bookings without an ID are new.
type Draft struct {
	Title       string
	Destination string
	StartsAt    time.Time
	EndsAt      time.Time
	Zone        string
	// Source is chat, mail, or calendar; empty means chat.
	Source   string
	Bookings []Booking
}

// Unset is a booking with a check-in time that got no reminder, and why.
type Unset struct {
	BookingID string
	Title     string
	Reason    string
}

// Notice is a start or end notice a trip owes its owner.
type Notice struct {
	Trip Trip
	Kind string
}

// Result is the outcome of a write. A non-empty Rejection says in words for the model why
// nothing was written. Outcome is set by Suggest only.
type Result struct {
	Trip      Trip
	Outcome   string
	Rejection string
	Unset     []Unset
	// Previous is the trip before a Suggest that updated it, and Added the IDs of the bookings
	// that update brought in.
	Previous *Trip
	Added    []string
	// written is set when the write saved the trip.
	written bool
}

const (
	unknownTripText = "No trip has that id. Use trip_list to find it."
	staleText       = "The trip changed since this was proposed; propose the change again."
)

// stale is true when the trip was updated after the instant the caller read it.
func stale(trip Trip, expectedUpdatedAt time.Time) bool {
	return !expectedUpdatedAt.IsZero() && !trip.UpdatedAt.Equal(expectedUpdatedAt)
}

func rejected(text string) Result { return Result{Rejection: text} }

// Add stores a planned trip and sets the reminders of its bookings.
func (s *Service) Add(ctx context.Context, principalID string, draft Draft) (result Result, err error) {
	defer s.announce(ctx, &result)
	s.mu.Lock()
	defer s.mu.Unlock()
	if draft.Source == "" {
		draft.Source = SourceChat
	}
	normalized, text := normalizeDraft(draft, nil, false)
	if text != "" {
		return rejected(text), nil
	}
	if text, err := s.checkCap(ctx, principalID); err != nil || text != "" {
		return rejected(text), err
	}
	return s.create(ctx, principalID, normalized, StatusPlanned)
}

// Suggest stores a trip found in mail or a calendar as suggested, unless one of its bookings is
// already known. A known suggested or planned trip is merged with it: matched bookings are
// updated in place, new ones are added, and nothing the user added is removed. A trip the user
// dismissed is never changed or offered again.
func (s *Service) Suggest(ctx context.Context, principalID string, draft Draft) (result Result, err error) {
	defer s.announce(ctx, &result)
	s.mu.Lock()
	defer s.mu.Unlock()
	if draft.Source != SourceMail && draft.Source != SourceCalendar {
		return rejected("A suggested trip comes from mail or the calendar."), nil
	}
	normalized, text := normalizeDraft(draft, nil, false)
	if text != "" {
		return rejected(text), nil
	}
	dedupeFingerprints(normalized.Bookings)
	var fingerprints []string
	for _, booking := range normalized.Bookings {
		if booking.Fingerprint != "" {
			fingerprints = append(fingerprints, booking.Fingerprint)
		}
	}
	if len(fingerprints) == 0 {
		return rejected("A suggested trip needs a booking with a reference or a calendar event id, so it is recognised when found again."), nil
	}
	matches, err := s.repository.FindTripsByFingerprints(ctx, principalID, fingerprints)
	if err != nil {
		return Result{}, err
	}
	if len(matches) == 0 {
		if text, err := s.checkSuggestedCap(ctx, principalID); err != nil || text != "" {
			return rejected(text), err
		}
		result, err = s.create(ctx, principalID, normalized, StatusSuggested)
		result.Outcome = OutcomeNew
		return result, err
	}
	var target *Trip
	for index := range matches {
		switch matches[index].Status {
		case StatusDismissed:
			return Result{Trip: matches[index], Outcome: OutcomeKnown}, nil
		case StatusPlanned, StatusSuggested:
			if target == nil {
				target = &matches[index]
			}
		}
	}
	if target == nil {
		return Result{Trip: matches[0], Outcome: OutcomeKnown}, nil
	}
	foreign := map[string]bool{}
	for _, match := range matches {
		if match.ID == target.ID {
			continue
		}
		for _, booking := range match.Bookings {
			foreign[booking.Fingerprint] = true
		}
	}
	next, added, text := mergeTrip(*target, normalized, foreign)
	if text != "" {
		return rejected(text), nil
	}
	if added == nil && next.StartsAt.Equal(target.StartsAt) && next.EndsAt.Equal(target.EndsAt) && bookingSignature(next.Bookings) == bookingSignature(target.Bookings) {
		return Result{Trip: *target, Outcome: OutcomeKnown}, nil
	}
	result, err = s.commit(ctx, *target, next)
	result.Outcome, result.Previous = OutcomeUpdated, target
	for _, booking := range result.Trip.Bookings[len(target.Bookings):] {
		result.Added = append(result.Added, booking.ID)
	}
	return result, err
}

// mergeTrip folds a found trip into a known one. Bookings are matched by fingerprint, or by kind,
// title, and start when they have none; a matched booking takes the found start and check-in
// time, keeping its own check-in when none was found. The dates widen to cover everything.
func mergeTrip(known Trip, found Draft, foreign map[string]bool) (Trip, []Booking, string) {
	next := known
	next.Bookings = append([]Booking(nil), known.Bookings...)
	var added []Booking
	for _, booking := range found.Bookings {
		index := -1
		for position, existing := range next.Bookings {
			if booking.Fingerprint != "" && existing.Fingerprint == booking.Fingerprint ||
				booking.Fingerprint == "" && existing.Fingerprint == "" && existing.Kind == booking.Kind &&
					strings.EqualFold(existing.Title, booking.Title) && existing.StartsAt.Equal(booking.StartsAt) {
				index = position
				break
			}
		}
		switch {
		case index >= 0:
			existing := &next.Bookings[index]
			existing.StartsAt = booking.StartsAt
			if !booking.CheckInAt.IsZero() && (existing.CheckInAt.IsZero() || sourceRank(booking.CheckInSource) >= sourceRank(existing.CheckInSource)) {
				existing.CheckInAt, existing.CheckInSource = booking.CheckInAt, booking.CheckInSource
			}
		case booking.Fingerprint != "" && foreign[booking.Fingerprint]:
		default:
			if len(next.Bookings) >= MaximumBookings {
				return Trip{}, nil, fmt.Sprintf("A trip holds at most %d bookings.", MaximumBookings)
			}
			booking.ID = ""
			next.Bookings = append(next.Bookings, booking)
			added = append(added, booking)
		}
	}
	next.StartsAt, next.EndsAt = earliest(known.StartsAt, found.StartsAt), latest(known.EndsAt, found.EndsAt)
	for _, booking := range next.Bookings {
		next.StartsAt, next.EndsAt = earliest(next.StartsAt, booking.StartsAt), latest(next.EndsAt, booking.StartsAt)
	}
	return next, added, ""
}

// sourceRank orders where a check-in time came from, strongest first, so a weaker find never
// replaces a stronger one.
func sourceRank(source string) int {
	switch source {
	case CheckInStated:
		return 3
	case CheckInLookedUp:
		return 2
	}
	return 1
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Update replaces the editable content of a suggested or planned trip and moves its reminders.
// Bookings that keep their ID keep their reminder. A non-zero expectedUpdatedAt must be the
// trip's last update time, so a change proposed earlier cannot overwrite a later one.
func (s *Service) Update(ctx context.Context, principalID, id string, expectedUpdatedAt time.Time, draft Draft) (result Result, err error) {
	defer s.announce(ctx, &result)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, found, err := s.repository.LoadTrip(ctx, principalID, id)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return rejected(unknownTripText), nil
	}
	if existing.Status != StatusSuggested && existing.Status != StatusPlanned {
		return rejected("That trip is " + existing.Status + " and cannot be changed."), nil
	}
	if stale(existing, expectedUpdatedAt) {
		return rejected(staleText), nil
	}
	draft.Source = existing.Source
	normalized, text := normalizeDraft(draft, existing.Bookings, true)
	if text != "" {
		return rejected(text), nil
	}
	if err := s.resolveFingerprints(ctx, principalID, id, normalized.Bookings, existing.Bookings); err != nil {
		return Result{}, err
	}
	next := existing
	next.Title, next.Destination, next.StartsAt, next.EndsAt, next.Zone = normalized.Title, normalized.Destination, normalized.StartsAt, normalized.EndsAt, normalized.Zone
	next.Bookings = normalized.Bookings
	return s.commit(ctx, existing, next)
}

// Cancel cancels a planned trip, or dismisses a suggested one, and removes its reminders. A trip that has already started is
// left owing its end notice.
func (s *Service) Cancel(ctx context.Context, principalID, id string, expectedUpdatedAt time.Time) (result Result, err error) {
	defer s.announce(ctx, &result)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, found, err := s.repository.LoadTrip(ctx, principalID, id)
	if err != nil {
		return Result{}, err
	}
	switch {
	case !found:
		return rejected(unknownTripText), nil
	case stale(existing, expectedUpdatedAt):
		return rejected(staleText), nil
	case existing.Status == StatusSuggested:
		return s.setStatus(ctx, existing, StatusDismissed)
	case existing.Status != StatusPlanned:
		return rejected("That trip is already " + existing.Status + "."), nil
	}
	return s.setStatus(ctx, existing, StatusCancelled)
}

// EndNow ends an active trip at the current time.
func (s *Service) EndNow(ctx context.Context, principalID, id string, expectedUpdatedAt time.Time) (result Result, err error) {
	defer s.announce(ctx, &result)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, found, err := s.repository.LoadTrip(ctx, principalID, id)
	if err != nil {
		return Result{}, err
	}
	now := s.now()
	switch {
	case !found:
		return rejected(unknownTripText), nil
	case stale(existing, expectedUpdatedAt):
		return rejected(staleText), nil
	case !existing.Active(now):
		return rejected("That trip is not under way. Cancel it instead if it is not going ahead."), nil
	}
	next := existing
	next.EndsAt = now
	return s.commit(ctx, existing, next)
}

// Track makes a suggested trip planned and sets the reminders of its bookings.
func (s *Service) Track(ctx context.Context, principalID, id string) (result Result, err error) {
	defer s.announce(ctx, &result)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, found, err := s.repository.LoadTrip(ctx, principalID, id)
	if err != nil {
		return Result{}, err
	}
	switch {
	case !found:
		return rejected(unknownTripText), nil
	case existing.Status != StatusSuggested:
		return rejected("That trip is " + existing.Status + ", not suggested."), nil
	}
	if text, err := s.checkCap(ctx, principalID); err != nil || text != "" {
		return rejected(text), err
	}
	return s.setStatus(ctx, existing, StatusPlanned)
}

// Dismiss makes a suggested trip dismissed, so it is never offered again.
func (s *Service) Dismiss(ctx context.Context, principalID, id string) (result Result, err error) {
	defer s.announce(ctx, &result)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, found, err := s.repository.LoadTrip(ctx, principalID, id)
	if err != nil {
		return Result{}, err
	}
	switch {
	case !found:
		return rejected(unknownTripText), nil
	case existing.Status != StatusSuggested:
		return rejected("That trip is " + existing.Status + ", not suggested."), nil
	}
	return s.setStatus(ctx, existing, StatusDismissed)
}

// Trips returns the principal's suggested and planned trips that have not ended, earliest first.
func (s *Service) Trips(ctx context.Context, principalID string) ([]Trip, error) {
	now := s.now()
	var trips []Trip
	for _, status := range []string{StatusSuggested, StatusPlanned} {
		listed, err := s.repository.ListTrips(ctx, principalID, status)
		if err != nil {
			return nil, err
		}
		for _, trip := range listed {
			if trip.EndsAt.After(now) {
				trips = append(trips, trip)
			}
		}
	}
	sort.SliceStable(trips, func(i, j int) bool { return trips[i].StartsAt.Before(trips[j].StartsAt) })
	return trips, nil
}

// Trip finds one of the principal's trips of any status.
func (s *Service) Trip(ctx context.Context, principalID, id string) (Trip, bool, error) {
	return s.repository.LoadTrip(ctx, principalID, id)
}

// Traveling is true when a planned trip of the principal spans the instant, from its start up
// to but not including its end.
func (s *Service) Traveling(ctx context.Context, principalID string, at time.Time) (bool, error) {
	active, err := s.repository.ListActiveTrips(ctx, principalID, at)
	return len(active) > 0, err
}

// ActiveTrips returns the principal's planned trips under way at the instant, without bookings.
func (s *Service) ActiveTrips(ctx context.Context, principalID string, at time.Time) ([]Trip, error) {
	return s.repository.ListActiveTrips(ctx, principalID, at)
}

// TurnLine tells a turn about the active trip or the next one starting within 14 days; empty
// when there is none.
func (s *Service) TurnLine(ctx context.Context, principalID string) (string, error) {
	planned, err := s.repository.ListTrips(ctx, principalID, StatusPlanned)
	if err != nil {
		return "", err
	}
	now := s.now()
	var next *Trip
	for index := range planned {
		trip := planned[index]
		if trip.Active(now) {
			location := localtime.SavedZone(trip.Zone)
			return fmt.Sprintf("User is traveling: %q, destination %q, %s to %s (%s); local time there is %s.",
				trip.Title, trip.Destination, trip.StartsAt.In(location).Format("Mon 2006-01-02"),
				trip.EndLabel(turnDateLayout, turnDateLayout), location, now.In(location).Format("Mon 2006-01-02 15:04")), nil
		}
		if trip.StartsAt.After(now) && trip.StartsAt.Sub(now) <= turnLineHorizon && (next == nil || trip.StartsAt.Before(next.StartsAt)) {
			next = &planned[index]
		}
	}
	if next == nil {
		return "", nil
	}
	location := localtime.SavedZone(next.Zone)
	return fmt.Sprintf("User's next trip: %q, destination %q, %s to %s (%s).",
		next.Title, next.Destination, next.StartsAt.In(location).Format("Mon 2006-01-02"),
		next.EndLabel(turnDateLayout, turnDateLayout), location), nil
}

// DueNotices returns the notices owed at now across all principals: a start notice for a planned
// trip that is under way, and an end notice for a started trip that has ended or was cancelled.
func (s *Service) DueNotices(ctx context.Context, now time.Time) ([]Notice, error) {
	candidates, err := s.repository.ListNoticeTrips(ctx, now)
	if err != nil {
		return nil, err
	}
	var notices []Notice
	for _, trip := range candidates {
		switch {
		case trip.StartedNotifiedAt.IsZero():
			if trip.Active(now) {
				notices = append(notices, Notice{Trip: trip, Kind: NoticeStart})
			}
		case trip.EndedNotifiedAt.IsZero():
			if trip.Status == StatusCancelled || !now.Before(trip.EndsAt) {
				notices = append(notices, Notice{Trip: trip, Kind: NoticeEnd})
			}
		}
	}
	return notices, nil
}

// MarkStarted records that the start notice was sent; false means it already was.
func (s *Service) MarkStarted(ctx context.Context, principalID, id string, at time.Time) (bool, error) {
	return s.repository.MarkTripStarted(ctx, principalID, id, at)
}

// MarkEnded records that the end notice was sent, and tells the tabs, whose summary then says the
// trip ended; false means it already was.
func (s *Service) MarkEnded(ctx context.Context, principalID, id string, at time.Time) (bool, error) {
	marked, err := s.repository.MarkTripEnded(ctx, principalID, id, at)
	if err != nil || !marked {
		return marked, err
	}
	if ended, found, err := s.repository.LoadTrip(ctx, principalID, id); err == nil && found {
		s.changed(ctx, ended)
	}
	return true, nil
}

// Prune deletes trips of every status that ended more than 30 days before now.
func (s *Service) Prune(ctx context.Context, now time.Time) error {
	pruned, err := s.repository.PruneTrips(ctx, now.Add(-pruneAfter))
	if err != nil {
		return err
	}
	if pruned > 0 {
		s.logger.InfoContext(ctx, "trip.pruned", "count", pruned)
	}
	return nil
}

// checkCap refuses another planned trip beyond MaximumUnfinished.
func (s *Service) checkCap(ctx context.Context, principalID string) (string, error) {
	count, err := s.countUnfinished(ctx, principalID, StatusPlanned)
	if err != nil {
		return "", err
	}
	if count >= MaximumUnfinished {
		return fmt.Sprintf("There are already %d unfinished trips. Cancel one first.", MaximumUnfinished), nil
	}
	return "", nil
}

// checkSuggestedCap refuses another suggested trip beyond MaximumSuggested, so a flood of finds
// cannot crowd out the trips the user plans.
func (s *Service) checkSuggestedCap(ctx context.Context, principalID string) (string, error) {
	count, err := s.countUnfinished(ctx, principalID, StatusSuggested)
	if err != nil {
		return "", err
	}
	if count >= MaximumSuggested {
		return fmt.Sprintf("There are already %d suggested trips waiting for an answer.", MaximumSuggested), nil
	}
	return "", nil
}

func (s *Service) countUnfinished(ctx context.Context, principalID, status string) (int, error) {
	listed, err := s.repository.ListTrips(ctx, principalID, status)
	if err != nil {
		return 0, err
	}
	now, count := s.now(), 0
	for _, trip := range listed {
		if trip.EndsAt.After(now) {
			count++
		}
	}
	return count, nil
}

// Forget deletes a suggested trip, for a find whose card could not be sent, so the next scan finds
// it as new. Any other trip is left alone.
func (s *Service) Forget(ctx context.Context, principalID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, found, err := s.repository.LoadTrip(ctx, principalID, id)
	if err != nil || !found || existing.Status != StatusSuggested {
		return err
	}
	return s.repository.DeleteTrip(ctx, principalID, id)
}

// create stores a validated draft as a new trip.
func (s *Service) create(ctx context.Context, principalID string, draft Draft, status string) (Result, error) {
	id, err := newID()
	if err != nil {
		return Result{}, err
	}
	if err := s.resolveFingerprints(ctx, principalID, id, draft.Bookings, nil); err != nil {
		return Result{}, err
	}
	now := s.now()
	trip := Trip{
		ID: id, PrincipalID: principalID, Title: draft.Title, Destination: draft.Destination,
		StartsAt: draft.StartsAt, EndsAt: draft.EndsAt, Zone: draft.Zone,
		Status: status, Source: draft.Source, CreatedAt: now, UpdatedAt: now,
	}
	if trip.Bookings, err = withIDs(draft.Bookings); err != nil {
		return Result{}, err
	}
	if err := s.repository.AddTrip(ctx, trip); err != nil {
		return Result{}, err
	}
	s.logger.InfoContext(ctx, "trip.added", "principal_id", principalID, "trip_id", id, "status", status, "source", draft.Source)
	unset, err := s.sync(ctx, nil, trip)
	return Result{Trip: trip, Unset: unset, written: true}, err
}

// commit saves next in place of existing, forgets notices a changed date makes due again, and
// syncs the reminders.
func (s *Service) commit(ctx context.Context, existing, next Trip) (Result, error) {
	now := s.now()
	next.UpdatedAt = now
	var err error
	if next.Bookings, err = withIDs(next.Bookings); err != nil {
		return Result{}, err
	}
	if _, err := s.repository.UpdateTrip(ctx, next); err != nil {
		return Result{}, err
	}
	postponed := !existing.StartedNotifiedAt.IsZero() && next.StartsAt.After(now)
	extended := !existing.EndedNotifiedAt.IsZero() && next.EndsAt.After(now)
	if postponed || extended {
		if err := s.repository.ResetTripNotices(ctx, next.PrincipalID, next.ID, postponed, postponed || extended); err != nil {
			return Result{}, err
		}
		if postponed {
			next.StartedNotifiedAt = time.Time{}
		}
		next.EndedNotifiedAt = time.Time{}
	}
	s.logger.InfoContext(ctx, "trip.updated", "principal_id", next.PrincipalID, "trip_id", next.ID, "status", next.Status)
	unset, err := s.sync(ctx, &existing, next)
	return Result{Trip: next, Unset: unset, written: true}, err
}

func (s *Service) setStatus(ctx context.Context, existing Trip, status string) (Result, error) {
	next := existing
	next.Status = status
	return s.commit(ctx, existing, next)
}

// resolveFingerprints settles which of a trip's bookings keep their fingerprint when stored: an
// unchanged booking keeps its own, and a fingerprint already held by another booking of the
// principal, or repeated in the trip, is cleared because it can identify only one.
func (s *Service) resolveFingerprints(ctx context.Context, principalID, tripID string, bookings, existing []Booking) error {
	known := make(map[string]Booking, len(existing))
	for _, booking := range existing {
		known[booking.ID] = booking
	}
	for index, booking := range bookings {
		if old, found := known[booking.ID]; found && old.Kind == booking.Kind && old.Reference == booking.Reference {
			bookings[index].Fingerprint = old.Fingerprint
		}
	}
	dedupeFingerprints(bookings)
	var fingerprints []string
	for _, booking := range bookings {
		if booking.Fingerprint != "" {
			fingerprints = append(fingerprints, booking.Fingerprint)
		}
	}
	owners, err := s.repository.FindTripsByFingerprints(ctx, principalID, fingerprints)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, owner := range owners {
		if owner.ID == tripID {
			continue
		}
		for _, booking := range owner.Bookings {
			taken[booking.Fingerprint] = true
		}
	}
	for index, booking := range bookings {
		if taken[booking.Fingerprint] {
			bookings[index].Fingerprint = ""
		}
	}
	return nil
}

// dedupeFingerprints keeps each fingerprint on its first booking only, as a round trip on one
// reservation shares a reference.
func dedupeFingerprints(bookings []Booking) {
	seen := map[string]bool{}
	for index, booking := range bookings {
		if booking.Fingerprint == "" {
			continue
		}
		if seen[booking.Fingerprint] {
			bookings[index].Fingerprint = ""
		}
		seen[booking.Fingerprint] = true
	}
}

// sync makes the principal's reminders match the trip: one per booking whose check-in is ahead
// and before the trip ends while the trip is planned, none otherwise. The trip is already saved,
// so a failure leaves it recorded and the next write syncs again.
func (s *Service) sync(ctx context.Context, previous *Trip, next Trip) ([]Unset, error) {
	keep := make(map[string]bool, len(next.Bookings))
	for _, booking := range next.Bookings {
		keep[booking.ID] = true
	}
	if previous != nil {
		for _, booking := range previous.Bookings {
			if keep[booking.ID] {
				continue
			}
			if err := s.reminders.RemoveReminder(ctx, next.PrincipalID, ReminderID(next.ID, booking.ID)); err != nil {
				return nil, err
			}
		}
	}
	now := s.now()
	was := map[string]time.Time{}
	if previous != nil {
		for _, booking := range previous.Bookings {
			was[booking.ID] = booking.CheckInAt.Truncate(time.Minute)
		}
	}
	var unset []Unset
	for _, booking := range next.Bookings {
		id := ReminderID(next.ID, booking.ID)
		// Reminders are set to the minute, so a check-in later in the current minute is past.
		at := booking.CheckInAt.Truncate(time.Minute)
		reason := ""
		switch {
		case next.Status != StatusPlanned || booking.CheckInAt.IsZero():
		case !at.After(now):
			reason = UnsetPast
		case !at.Before(next.EndsAt):
			reason = UnsetAfterEnd
		case at.Sub(now) > reminderHorizon:
			reason = UnsetFar
		default:
			err := s.reminders.SetReminder(ctx, next.PrincipalID, id, at, next.Zone, reminderText(booking), thread.TripDestination(next.ID))
			switch {
			case errors.Is(err, schedule.ErrTooManyJobs):
				reason = UnsetCap
			case errors.Is(err, ErrReminderRefused):
				reason = UnsetInvalid
			case err != nil:
				return unset, fmt.Errorf("set check-in reminder: %w", err)
			default:
				continue
			}
		}
		if reason != "" {
			unset = append(unset, Unset{BookingID: booking.ID, Title: booking.Title, Reason: reason})
		}
		// A reminder for a check-in that has just opened may be about to fire, so it is left to.
		pending := reason == UnsetPast && was[booking.ID].Equal(at)
		if reason != UnsetCap && !pending {
			if err := s.reminders.RemoveReminder(ctx, next.PrincipalID, id); err != nil {
				return unset, err
			}
		}
	}
	return unset, nil
}

func reminderText(booking Booking) string {
	text := "Check-in opens: " + booking.Title
	if booking.Reference != "" {
		text += " (" + booking.Reference + ")"
	}
	return text
}

// normalizeDraft validates a draft and returns it with single-line text. With keepIDs set, a
// booking's ID must be one of the existing bookings; otherwise IDs are dropped.
func normalizeDraft(draft Draft, existing []Booking, keepIDs bool) (Draft, string) {
	var text string
	if draft.Title, text = cleanText("title", draft.Title, MaximumTitleRunes, true); text != "" {
		return Draft{}, text
	}
	if draft.Destination, text = cleanText("destination", draft.Destination, MaximumTitleRunes, true); text != "" {
		return Draft{}, text
	}
	if draft.StartsAt.IsZero() || draft.EndsAt.IsZero() || !draft.EndsAt.After(draft.StartsAt) {
		return Draft{}, "The trip must start before it ends."
	}
	switch draft.Source {
	case SourceChat, SourceMail, SourceCalendar:
	default:
		return Draft{}, "The source must be chat, mail, or calendar."
	}
	draft.Zone = strings.TrimSpace(draft.Zone)
	if _, err := schedule.LoadZone(draft.Zone); err != nil {
		return Draft{}, "The time zone must be an IANA name such as Europe/Lisbon."
	}
	if len(draft.Bookings) > MaximumBookings {
		return Draft{}, fmt.Sprintf("A trip holds at most %d bookings.", MaximumBookings)
	}
	known := make(map[string]bool, len(existing))
	for _, booking := range existing {
		known[booking.ID] = true
	}
	seen := make(map[string]bool, len(draft.Bookings))
	bookings := make([]Booking, 0, len(draft.Bookings))
	for _, booking := range draft.Bookings {
		if !keepIDs {
			booking.ID = ""
		} else if booking.ID != "" && (!known[booking.ID] || seen[booking.ID]) {
			return Draft{}, "A booking id is unknown or repeated. Use trip_list to find the ids."
		}
		seen[booking.ID] = true
		switch booking.Kind {
		case KindFlight, KindHotel, KindTrain, KindCar, KindOther:
		default:
			return Draft{}, "A booking kind must be flight, hotel, train, car, or other."
		}
		if booking.Title, text = cleanText("booking title", booking.Title, MaximumTitleRunes, true); text != "" {
			return Draft{}, text
		}
		if booking.Reference, text = cleanText("booking reference", booking.Reference, MaximumReferenceRunes, false); text != "" {
			return Draft{}, text
		}
		if booking.StartsAt.IsZero() {
			return Draft{}, "A booking needs the time it starts."
		}
		switch booking.CheckInSource {
		case CheckInStated, CheckInLookedUp, CheckInDefault:
			if booking.CheckInAt.IsZero() {
				return Draft{}, "A check-in source needs a check-in time."
			}
		case "":
			if !booking.CheckInAt.IsZero() {
				return Draft{}, "A check-in time needs its source: stated, looked_up, or default."
			}
		default:
			return Draft{}, "A check-in source must be stated, looked_up, or default."
		}
		booking.SourceKey = strings.TrimSpace(booking.SourceKey)
		booking.Fingerprint = fingerprint(draft.Source, booking)
		booking.SourceKey = ""
		bookings = append(bookings, booking)
	}
	draft.Bookings = bookings
	return draft, ""
}

// plain drops invisible formatting characters, turns other control characters into spaces, and
// makes the text a single line.
func plain(value string) string {
	value = strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cf, r):
			return -1
		case unicode.IsControl(r):
			return ' '
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

// WithoutAddresses removes the words of text that read as a link or a web address, such as a
// provider's domain in a booking title.
func WithoutAddresses(text string) string {
	var kept []string
	for _, word := range strings.Fields(plain(text)) {
		lower := dotLike.Replace(strings.ToLower(word))
		if domainPattern.MatchString(lower) || strings.Contains(lower, "://") || strings.Contains(lower, "www.") {
			continue
		}
		kept = append(kept, word)
	}
	return strings.Join(kept, " ")
}

var domainPattern = regexp.MustCompile(`[\p{L}\p{N}-]+\.\p{L}{2,}`)

var dotLike = strings.NewReplacer("\u3002", ".", "\uff0e", ".", "\uff61", ".")

// cleanText makes text a single line without control or invisible formatting characters and
// refuses anything that reads as a link or a web address.
func cleanText(field, value string, limit int, required bool) (string, string) {
	if !utf8.ValidString(value) {
		return "", field + " must be valid UTF-8."
	}
	value = plain(value)
	lower := strings.ToLower(value)
	switch {
	case required && value == "":
		return "", field + " must not be empty."
	case utf8.RuneCountInString(value) > limit:
		return "", fmt.Sprintf("%s is %d characters; it holds at most %d.", field, utf8.RuneCountInString(value), limit)
	case strings.Contains(lower, "http://"), strings.Contains(lower, "https://"), strings.Contains(lower, "www."),
		domainPattern.MatchString(dotLike.Replace(lower)):
		return "", field + " must not contain a link or web address."
	}
	return value, ""
}

// fingerprint identifies a booking by its kind and reference, or by the calendar event it came
// from; empty when it has neither.
func fingerprint(source string, booking Booking) string {
	if source == SourceCalendar && booking.SourceKey != "" {
		sum := sha256.Sum256([]byte("calendar\x00" + booking.SourceKey))
		return hex.EncodeToString(sum[:])
	}
	var reference strings.Builder
	for _, r := range strings.ToUpper(booking.Reference) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			reference.WriteRune(r)
		}
	}
	if reference.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte("booking\x00" + booking.Kind + "\x00" + reference.String()))
	return hex.EncodeToString(sum[:])
}

func bookingSignature(bookings []Booking) string {
	lines := make([]string, 0, len(bookings))
	for _, booking := range bookings {
		lines = append(lines, fmt.Sprintf("%s|%s|%s|%s|%d|%d|%s", booking.ID, booking.Kind, booking.Title, booking.Reference,
			booking.StartsAt.Unix(), booking.CheckInAt.Unix(), booking.CheckInSource))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func withIDs(bookings []Booking) ([]Booking, error) {
	assigned := make([]Booking, len(bookings))
	used := make(map[string]bool, len(bookings))
	for _, booking := range bookings {
		if booking.ID != "" {
			used[booking.ID] = true
		}
	}
	for index, booking := range bookings {
		for booking.ID == "" {
			id, err := newID()
			if err != nil {
				return nil, err
			}
			if !used[id] {
				booking.ID, used[id] = id, true
			}
		}
		assigned[index] = booking
	}
	return assigned, nil
}

func newID() (string, error) {
	var value [4]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate trip identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

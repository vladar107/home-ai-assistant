package trip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/localtime"
)

const (
	UpdateSourceFlight = "flight"

	// The flight statuses the tracking tells apart.
	FlightScheduled      = "scheduled"
	FlightDeparted       = "departed"
	FlightLanded         = "landed"
	FlightCancelled      = "cancelled"
	FlightMaybeCancelled = "maybe_cancelled"
	FlightDiverted       = "diverted"

	// flightLead is how long before a booking's stored departure its flight is first looked up:
	// the 24 hours of the cadence and the 14 by which a time typed in the trip's zone can be off.
	flightLead = 38 * time.Hour
	// flightRetry is when a flight not found at first is looked up once more, before the stored
	// departure.
	flightRetry = 3 * time.Hour
	// flightMatchWindow is how far a leg's scheduled departure may be from the booking's and still
	// be its flight.
	flightMatchWindow = 30 * time.Minute
	// flightFailedRetry spaces the attempts at a match whose lookup failed.
	flightFailedRetry = time.Hour
	// flightStale is how late a slot may be served; one older is skipped, as after a long outage.
	flightStale = 2 * time.Hour
	// flightUnmatchedFor is how long after the booking's stored departure a flight never found is
	// given up.
	flightUnmatchedFor = 12 * time.Hour
	// flightKept is how long after a leg ends its stored lookup and its messages' text are kept.
	flightKept = 7 * 24 * time.Hour

	// flightDelayThreshold is the delay worth a message, and the move of a reported delay worth
	// another.
	flightDelayThreshold = 15 * time.Minute
	// flightGateLead is how close to departure a first gate is worth a message.
	flightGateLead = 4 * time.Hour
	// flightSendTimeout bounds sending one lookup's messages, the loop told to stop or not, since
	// its state is already stored: the rewrite's own limit and time to deliver.
	flightSendTimeout = rewriteTimeout + 15*time.Second
	// flightLandedFor is how long after its scheduled arrival a leg is followed at most, however
	// late its expected arrival keeps moving.
	flightLandedFor = 12 * time.Hour

	flightClock = "15:04"
	flightDay   = "Mon 2 Jan"

	flightNoteNotFound = "not_found"
	flightNoteNoLive   = "no_live"
	flightNoteFailed   = "failed"
	flightNoteMaybe    = "maybe_cancelled"
	flightNoteDiverted = "diverted"
	flightNoteLate     = "late_30"
)

// FlightAirport is one end of a leg; Zone is its IANA time zone.
type FlightAirport struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Zone string `json:"zone"`
}

// FlightTimes is a leg's departure or arrival: scheduled, expected (revised or predicted), and
// actual (the runway's), each zero when unknown.
type FlightTimes struct {
	Scheduled time.Time `json:"scheduled"`
	Expected  time.Time `json:"expected,omitzero"`
	Actual    time.Time `json:"actual,omitzero"`
	Gate      string    `json:"gate,omitempty"`
	Terminal  string    `json:"terminal,omitempty"`
}

// Now is when the movement happened or is expected, or its schedule when nothing else is known.
func (t FlightTimes) Now() time.Time {
	switch {
	case !t.Actual.IsZero():
		return t.Actual
	case !t.Expected.IsZero():
		return t.Expected
	}
	return t.Scheduled
}

// FlightLeg is one flight as the flight status provider reports it. Operator names the airline
// flying a leg sold under another's number, and Live is false when only its schedule is known.
type FlightLeg struct {
	Designator  string        `json:"designator"`
	Operator    string        `json:"operator,omitempty"`
	Origin      FlightAirport `json:"origin"`
	Destination FlightAirport `json:"destination"`
	Departure   FlightTimes   `json:"departure"`
	Arrival     FlightTimes   `json:"arrival"`
	Live        bool          `json:"live"`
	Status      string        `json:"status"`
}

// FlightLookup is what the tracking needs of the flight status connector; the application adapts
// it. LookUpFlight returns the legs under the number on the local day, departing that day or, with
// anyEnd, departing or arriving on it; ErrFlightNotFound for none, and a *FlightPausedError while
// the household's allowance is spent.
type FlightLookup interface {
	LookUpFlight(ctx context.Context, designator, day string, anyEnd bool) ([]FlightLeg, error)
	FlightStatusEnabled(ctx context.Context, principalID string) (bool, error)
	// FlightUnitsLeft is what the household budget has left in its period, and when it ends.
	FlightUnitsLeft(ctx context.Context) (int, time.Time, error)
}

// ErrFlightNotFound is a flight the provider has no record of on that day.
var ErrFlightNotFound = errors.New("flight not found")

// FlightPausedError is a lookup refused because the household's allowance is spent until Until;
// Text says so to the user.
type FlightPausedError struct {
	Until time.Time
	Text  string
}

func (e *FlightPausedError) Error() string { return e.Text }

// FlightState is what the tracking remembers of one flight booking of a trip: the designator and
// the booking's stored departure it was made for, and once its leg is found, the leg's
// origin-local day and airports. Lookup holds the last lookup and what was reported from it, the
// flight data that is kept at most a week after the leg ends; Progress holds which slots were
// served and which once-only lines were sent. A zero time means it never happened.
type FlightState struct {
	PrincipalID   string
	TripID        string
	BookingID     string
	Designator    string
	BookedAt      time.Time
	Day           string
	Origin        string
	Destination   string
	Lookup        json.RawMessage
	Progress      json.RawMessage
	LastAttemptAt time.Time
	LastSuccessAt time.Time
	EndedAt       time.Time
}

// FlightLog keeps the flight states and each principal's pause notice; it is the SQLite store.
type FlightLog interface {
	LoadFlightState(ctx context.Context, principalID, tripID, bookingID string) (FlightState, bool, error)
	SaveFlightState(ctx context.Context, state FlightState) error
	// FlightPauseNoticed returns the end of the last pause the principal was told of.
	FlightPauseNoticed(ctx context.Context, principalID string) (time.Time, error)
	SaveFlightPauseNoticed(ctx context.Context, principalID string, until time.Time) error
	// PruneFlightStates forgets the stored lookup of every leg that ended before the instant, and
	// the text of its flight items in the sent log, and deletes the states of bookings that are
	// gone.
	PruneFlightStates(ctx context.Context, before time.Time) error
}

// ScanSwitch says whether the principal's Trips scan job is on, the switch that also follows
// their trips.
type ScanSwitch interface {
	ScanEnabled(ctx context.Context, principalID string) (bool, error)
}

type FlightsConfig struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Language rewrites a message into the trip's language before its source line is added; nil
	// sends it in English.
	Language *Language
	// Attribution ends every message, untranslated.
	Attribution string
}

// Flights follows the flights of planned trips on their day of travel, on slots counted from each
// leg's scheduled departure, and tells the trip's tab when one is delayed, moves gate or terminal,
// is cancelled, or is diverted. Code compares each lookup with the last one stored; no model turn
// decides anything. The trip loop drives it; at most one lookup runs at a time.
type Flights struct {
	trips       Repository
	states      FlightLog
	sent        UpdateLog
	lookup      FlightLookup
	scan        ScanSwitch
	sender      Sender
	notices     *UpdateNotices
	language    *Language
	attribution string
	logger      *slog.Logger
	now         func() time.Time

	mu sync.Mutex
	// resumeAt is the end of the last pause: a slot before it passed while lookups were paused
	// and is skipped.
	resumeAt time.Time
}

func NewFlights(cfg FlightsConfig, trips Repository, states FlightLog, sent UpdateLog, lookup FlightLookup, scan ScanSwitch, sender Sender, notices *UpdateNotices, logger *slog.Logger) (*Flights, error) {
	switch {
	case trips == nil || states == nil || sent == nil:
		return nil, fmt.Errorf("flight tracking repositories are required")
	case lookup == nil || scan == nil:
		return nil, fmt.Errorf("flight tracking lookup and scan switch are required")
	case sender == nil || notices == nil:
		return nil, fmt.Errorf("flight tracking sender and notices are required")
	case logger == nil:
		return nil, fmt.Errorf("flight tracking logger is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Flights{
		trips: trips, states: states, sent: sent, lookup: lookup, scan: scan, sender: sender, notices: notices,
		language: cfg.Language, attribution: cfg.Attribution, logger: logger, now: now,
	}, nil
}

// flightSnapshot is the stored last lookup of a leg and what was reported from it: the expected
// departure and arrival last reported as delays, and the gate last reported.
type flightSnapshot struct {
	Leg               *FlightLeg `json:"leg,omitempty"`
	ReportedDeparture time.Time  `json:"reported_departure,omitzero"`
	ReportedArrival   time.Time  `json:"reported_arrival,omitzero"`
	ReportedGate      string     `json:"reported_gate,omitempty"`
}

// flightProgress is where the tracking of a leg stands. Missed counts the matches that found no
// leg; LastSlot is the time of the last slot served or skipped; Failures counts failed slots in a
// row from three hours before departure.
type flightProgress struct {
	Matched  bool      `json:"matched,omitempty"`
	Missed   int       `json:"missed,omitempty"`
	LastSlot time.Time `json:"last_slot,omitzero"`
	Failures int       `json:"failures,omitempty"`
	// Landed counts the lookups at A+20 min; an arrival that moved later gets one more.
	Landed int      `json:"landed,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

func (p flightProgress) noted(note string) bool { return slices.Contains(p.Notes, note) }

func (p *flightProgress) note(note string) { p.Notes = append(p.Notes, note) }

// trackedLeg is one flight booking in the window with its state.
type trackedLeg struct {
	trip     Trip
	booking  Booking
	state    FlightState
	snapshot flightSnapshot
	progress flightProgress
}

func (l trackedLeg) leg() FlightLeg {
	if l.snapshot.Leg == nil {
		return FlightLeg{}
	}
	return *l.snapshot.Leg
}

type slotKind int

const (
	slotDay slotKind = iota
	slotSix
	slotThree
	slotNinety
	slotFortyFive
	slotFifteen
	slotAfter
	slotLanded
)

// droppedFirst is the order slots are dropped in when the budget runs short; the others are kept
// longest.
var droppedFirst = []slotKind{slotSix, slotAfter, slotNinety}

type flightSlot struct {
	kind slotKind
	at   time.Time
}

// flightSlots are the fixed times a leg is looked up at, counted from its scheduled departure D
// and expected arrival A, so every trip following the leg is due at once and shares one lookup. A
// leg without live data is looked up only a day and three hours ahead, and D−6h is skipped at
// night at the origin.
func flightSlots(leg FlightLeg) []flightSlot {
	departs := leg.Departure.Scheduled
	if !leg.Live {
		return []flightSlot{{slotDay, departs.Add(-24 * time.Hour)}, {slotThree, departs.Add(-3 * time.Hour)}}
	}
	slots := []flightSlot{{slotDay, departs.Add(-24 * time.Hour)}}
	six := departs.Add(-6 * time.Hour)
	if hour := six.In(localtime.SavedZone(leg.Origin.Zone)).Hour(); hour >= 7 && hour < 23 {
		slots = append(slots, flightSlot{slotSix, six})
	}
	slots = append(slots,
		flightSlot{slotThree, departs.Add(-3 * time.Hour)},
		flightSlot{slotNinety, departs.Add(-90 * time.Minute)},
		flightSlot{slotFortyFive, departs.Add(-45 * time.Minute)},
		flightSlot{slotFifteen, departs.Add(-15 * time.Minute)},
		flightSlot{slotAfter, departs.Add(30 * time.Minute)},
		flightSlot{slotLanded, landedSlot(leg)},
	)
	return slots
}

// landedSlot is A+20 min, but never past the leg's last moment, so an expected arrival that keeps
// moving later cannot keep the leg followed.
func landedSlot(leg FlightLeg) time.Time {
	return minTime(leg.Arrival.Now().Add(20*time.Minute), lastFollowed(leg))
}

// lastFollowed is when a leg stops being followed whatever its lookups say: twelve hours after its
// scheduled arrival.
func lastFollowed(leg FlightLeg) time.Time {
	return leg.Arrival.Scheduled.Add(flightLandedFor)
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// plannedSlots are the slots of a booking not matched yet, counted from its stored departure.
func plannedSlots(booked time.Time) []flightSlot {
	return flightSlots(FlightLeg{Departure: FlightTimes{Scheduled: booked}, Arrival: FlightTimes{Scheduled: booked.Add(3 * time.Hour)}, Live: true})
}

// flightJob is the lookup that is due: one leg and the slot it serves, or a match.
type flightJob struct {
	leg   trackedLeg
	slot  flightSlot
	match bool
}

// due finds the legs in the window and returns the one lookup owed at now, if any.
func (f *Flights) due(ctx context.Context, now time.Time) ([]trackedLeg, flightJob, bool, error) {
	legs, err := f.tracked(ctx, now)
	if err != nil || len(legs) == 0 {
		return legs, flightJob{}, false, err
	}
	f.mu.Lock()
	resumeAt := f.resumeAt
	f.mu.Unlock()
	if now.Before(resumeAt) {
		return legs, flightJob{}, false, nil
	}
	dropped, err := f.dropped(ctx, legs, now)
	if err != nil {
		return legs, flightJob{}, false, err
	}
	var best flightJob
	found := false
	for _, leg := range legs {
		job, ok := f.next(leg, now, resumeAt, dropped)
		if ok && (!found || job.slot.at.Before(best.slot.at)) {
			best, found = job, true
		}
	}
	return legs, best, found, nil
}

// next returns the lookup the leg owes at now: its match, or the latest slot that passed and was
// not served yet. Slots that passed during a pause, or long ago, are skipped without a lookup.
func (f *Flights) next(leg trackedLeg, now, resumeAt time.Time, dropped map[slotKind]bool) (flightJob, bool) {
	progress := leg.progress
	if !progress.Matched {
		booked := leg.state.BookedAt
		at := booked.Add(-flightLead)
		if progress.Missed == 1 {
			at = booked.Add(-flightRetry)
		}
		if now.Before(at) || now.Sub(leg.state.LastAttemptAt) < flightFailedRetry {
			return flightJob{}, false
		}
		return flightJob{leg: leg, slot: flightSlot{slotDay, at}, match: true}, true
	}
	var chosen flightSlot
	found := false
	for _, slot := range flightSlots(leg.leg()) {
		if !slot.at.After(progress.LastSlot) || slot.at.After(now) || dropped[slot.kind] {
			continue
		}
		chosen, found = slot, true
	}
	if !found {
		return flightJob{}, false
	}
	return flightJob{leg: leg, slot: chosen}, true
}

// dropped is the kinds of slot to skip so the slots still ahead in this period fit the units left,
// dropped in the order the plan keeps them.
func (f *Flights) dropped(ctx context.Context, legs []trackedLeg, now time.Time) (map[slotKind]bool, error) {
	left, periodEnds, err := f.lookup.FlightUnitsLeft(ctx)
	if err != nil {
		return nil, err
	}
	count := func(dropped map[slotKind]bool) int {
		total := 0
		for _, leg := range legs {
			slots := plannedSlots(leg.state.BookedAt)
			last := now
			if leg.progress.Matched {
				slots, last = flightSlots(leg.leg()), leg.progress.LastSlot
			}
			for _, slot := range slots {
				if slot.at.After(last) && slot.at.Before(periodEnds) && !dropped[slot.kind] {
					total++
				}
			}
		}
		return total
	}
	dropped := map[slotKind]bool{}
	for _, kind := range droppedFirst {
		if count(dropped)*2 <= left {
			break
		}
		dropped[kind] = true
	}
	return dropped, nil
}

// tracked lists the flight bookings in the window: planned trips of owners with the Trips scan and
// Flight status on, bookings with a flight number, from flightLead before their stored departure
// until their leg ends. A booking whose number or time changed starts over.
func (f *Flights) tracked(ctx context.Context, now time.Time) ([]trackedLeg, error) {
	planned, err := f.trips.ListPlannedTrips(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	var legs []trackedLeg
	for _, item := range planned {
		for _, booking := range item.Bookings {
			if booking.Kind != KindFlight || booking.FlightNumber == "" || now.Before(booking.StartsAt.Add(-flightLead)) {
				continue
			}
			on, known := allowed[item.PrincipalID]
			if !known {
				if on, err = f.scan.ScanEnabled(ctx, item.PrincipalID); err != nil {
					return nil, err
				}
				if on {
					if on, err = f.lookup.FlightStatusEnabled(ctx, item.PrincipalID); err != nil {
						return nil, err
					}
				}
				allowed[item.PrincipalID] = on
			}
			if !on {
				continue
			}
			state, found, err := f.states.LoadFlightState(ctx, item.PrincipalID, item.ID, booking.ID)
			if err != nil {
				return nil, err
			}
			if !found || state.Designator != booking.FlightNumber || !state.BookedAt.Equal(booking.StartsAt) {
				state = FlightState{PrincipalID: item.PrincipalID, TripID: item.ID, BookingID: booking.ID, Designator: booking.FlightNumber, BookedAt: booking.StartsAt}
			}
			if !state.EndedAt.IsZero() {
				continue
			}
			leg := trackedLeg{trip: item, booking: booking, state: state}
			if len(state.Lookup) > 0 {
				if err := json.Unmarshal(state.Lookup, &leg.snapshot); err != nil {
					return nil, fmt.Errorf("read flight lookup: %w", err)
				}
			}
			if len(state.Progress) > 0 {
				if err := json.Unmarshal(state.Progress, &leg.progress); err != nil {
					return nil, fmt.Errorf("read flight progress: %w", err)
				}
			}
			legs = append(legs, leg)
		}
	}
	return legs, nil
}

// flightMessage is one message the lookup owes a trip's tab, with the changes it reports.
type flightMessage struct {
	principalID string
	tripID      string
	title       string
	lines       []string
	changes     []SentUpdate
}

// run does the job's lookup, compares and stores what it found, and returns the messages owed,
// which the caller sends after the lookup is done. legs are every leg in the window, for the
// pause notice.
func (f *Flights) run(ctx context.Context, job flightJob, legs []trackedLeg, now time.Time) []flightMessage {
	leg := job.leg
	var messages []flightMessage
	add := func(lines []string, changes []SentUpdate) {
		if len(lines) > 0 {
			messages = append(messages, flightMessage{principalID: leg.trip.PrincipalID, tripID: leg.trip.ID, title: leg.trip.Title, lines: lines, changes: changes})
		}
	}
	leg.state.LastAttemptAt = now
	var err error
	if job.match {
		var lines []string
		var changes []SentUpdate
		lines, changes, err = f.match(ctx, &leg, now)
		add(lines, changes)
	} else {
		var message flightMessage
		message, err = f.follow(ctx, &leg, job.slot, now)
		if len(message.lines) > 0 {
			messages = append(messages, message)
		}
	}
	var paused *FlightPausedError
	if errors.As(err, &paused) {
		return f.pause(ctx, paused, legs)
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := f.save(ctx, &leg); err != nil {
		f.logger.WarnContext(ctx, "trip.flights.failed", "principal_id", leg.trip.PrincipalID, "trip_id", leg.trip.ID, "stage", "save", "error", err)
		return nil
	}
	return messages
}

func (f *Flights) save(ctx context.Context, leg *trackedLeg) error {
	lookup, err := json.Marshal(leg.snapshot)
	if err != nil {
		return err
	}
	progress, err := json.Marshal(leg.progress)
	if err != nil {
		return err
	}
	leg.state.Lookup, leg.state.Progress = lookup, progress
	return f.states.SaveFlightState(ctx, leg.state)
}

// match finds the booking's leg. The booking's time is not trusted as an instant: one typed in the
// trip's zone can be hours off, so the first lookup asks for its date in the trip's zone at either
// end, and when no leg matches, one more asks for the neighbouring day the instant points to.
func (f *Flights) match(ctx context.Context, leg *trackedLeg, now time.Time) ([]string, []SentUpdate, error) {
	booked := leg.state.BookedAt
	zone := localtime.SavedZone(leg.trip.Zone)
	day := booked.In(zone).Format(time.DateOnly)
	found, ok, err := f.matchOn(ctx, leg, day)
	if err == nil && !ok {
		next := booked.In(zone)
		other := next.AddDate(0, 0, 1)
		if utc := booked.UTC().Format(time.DateOnly); utc < day || utc == day && next.Hour() < 12 {
			other = next.AddDate(0, 0, -1)
		}
		found, ok, err = f.matchOn(ctx, leg, other.Format(time.DateOnly))
	}
	if err != nil {
		f.failed(ctx, leg, "match", err)
		return nil, nil, err
	}
	leg.state.LastSuccessAt = now
	if !ok {
		leg.progress.Missed++
		if leg.progress.Missed >= 2 || now.After(booked.Add(flightUnmatchedFor)) {
			leg.state.EndedAt = now
		}
		if leg.progress.noted(flightNoteNotFound) {
			return nil, nil, nil
		}
		leg.progress.note(flightNoteNotFound)
		return []string{fmt.Sprintf("%s on %s was not found; check the flight number and time on the trip.", leg.state.Designator, booked.In(zone).Format(flightDay))}, nil, nil
	}
	leg.progress.Matched = true
	leg.state.Day = found.Departure.Scheduled.In(localtime.SavedZone(found.Origin.Zone)).Format(time.DateOnly)
	leg.state.Origin, leg.state.Destination = found.Origin.Code, found.Destination.Code
	leg.progress.LastSlot = found.Departure.Scheduled.Add(-24 * time.Hour)
	for _, slot := range flightSlots(found) {
		if !slot.at.After(now) && slot.at.After(leg.progress.LastSlot) {
			leg.progress.LastSlot = slot.at
		}
	}
	// A cancellation, diversion, or delay the match finds is a change like any other.
	changes := f.compare(leg, found, slotDay, true, now)
	f.finishIfDone(leg, now)
	lines, sent := changeLines(changes), sentChanges(leg, changes, now)
	if !found.Live && !leg.progress.noted(flightNoteNoLive) && found.Status != FlightCancelled {
		leg.progress.note(flightNoteNoLive)
		lines = append(lines, leg.state.Designator+": no live status for this flight; check the airline's app.")
	}
	return lines, sent, nil
}

// matchOn looks the day up and picks the booking's leg: the one whose scheduled departure is the
// stored instant, or reads at its own origin as the stored wall-clock time in the trip's zone,
// within half an hour; else the only leg returned.
func (f *Flights) matchOn(ctx context.Context, leg *trackedLeg, day string) (FlightLeg, bool, error) {
	found, err := f.lookup.LookUpFlight(ctx, leg.state.Designator, day, true)
	if errors.Is(err, ErrFlightNotFound) {
		return FlightLeg{}, false, nil
	}
	if err != nil {
		return FlightLeg{}, false, err
	}
	booked := leg.state.BookedAt
	wall := naive(booked.In(localtime.SavedZone(leg.trip.Zone)))
	for _, candidate := range found {
		departs := candidate.Departure.Scheduled
		if within(departs, booked) || within(naive(departs.In(localtime.SavedZone(candidate.Origin.Zone))), wall) {
			return candidate, true, nil
		}
	}
	if len(found) == 1 {
		return found[0], true, nil
	}
	return FlightLeg{}, false, nil
}

func naive(at time.Time) time.Time {
	return time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), 0, 0, time.UTC)
}

func within(a, b time.Time) bool {
	gap := a.Sub(b)
	return gap <= flightMatchWindow && gap >= -flightMatchWindow
}

// follow looks the matched leg up for one slot and compares it with the last lookup stored. A
// lookup that failed, or a leg the provider no longer returns, changes nothing.
func (f *Flights) follow(ctx context.Context, leg *trackedLeg, slot flightSlot, now time.Time) (flightMessage, error) {
	message := flightMessage{principalID: leg.trip.PrincipalID, tripID: leg.trip.ID, title: leg.trip.Title}
	served := func() {
		leg.progress.LastSlot = slot.at
		if slot.kind == slotLanded {
			leg.progress.Landed++
		}
		f.finishIfDone(leg, now)
	}
	previous := leg.leg()
	if now.Sub(slot.at) > flightStale {
		served()
		return message, nil
	}
	f.mu.Lock()
	skipped := slot.at.Before(f.resumeAt)
	f.mu.Unlock()
	if skipped {
		served()
		return message, nil
	}
	found, err := f.lookup.LookUpFlight(ctx, leg.state.Designator, leg.state.Day, false)
	var paused *FlightPausedError
	switch {
	case errors.As(err, &paused):
		return message, err
	case ctx.Err() != nil:
		return message, ctx.Err()
	case err != nil && !errors.Is(err, ErrFlightNotFound):
		served()
		f.failed(ctx, leg, "lookup", err)
		if !slot.at.Before(previous.Departure.Scheduled.Add(-3*time.Hour)) && slot.kind != slotLanded {
			leg.progress.Failures++
		}
		if leg.progress.Failures >= 2 && !leg.progress.noted(flightNoteFailed) {
			leg.progress.note(flightNoteFailed)
			message.lines = []string{fmt.Sprintf("Flight status for %s could not be checked; check the airline's app.", leg.state.Designator)}
		}
		return message, nil
	}
	served()
	leg.state.LastSuccessAt = now
	leg.progress.Failures = 0
	current, ok := pickLeg(found, previous)
	if !ok {
		return message, nil
	}
	changes := f.compare(leg, current, slot.kind, false, now)
	f.finishIfDone(leg, now)
	message.lines, message.changes = changeLines(changes), sentChanges(leg, changes, now)
	return message, nil
}

// sentChanges are the changes as the sent log keeps them.
func sentChanges(leg *trackedLeg, changes []flightChange, now time.Time) []SentUpdate {
	sent := make([]SentUpdate, 0, len(changes))
	for _, change := range changes {
		sent = append(sent, SentUpdate{Source: UpdateSourceFlight, Origin: "AeroDataBox",
			Reference: flightReference(leg.state.Designator, leg.state.Day, change.field, now), Text: clip.Runes(change.line, maximumUpdateTextRunes)})
	}
	return sent
}

// flightReference names a change in the sent log by its leg, its field, and the lookup's time; the
// flight data itself is only in the text, which is pruned a week after the leg ends.
func flightReference(designator, day, field string, at time.Time) string {
	return FlightReferencePrefix(designator, day) + field + "|" + at.UTC().Format(time.RFC3339)
}

// FlightReferencePrefix starts every sent-log reference of one leg's changes.
func FlightReferencePrefix(designator, day string) string {
	return designator + "|" + day + "|"
}

// finishIfDone ends a matched leg once no slot is left after the last one served.
func (f *Flights) finishIfDone(leg *trackedLeg, now time.Time) {
	if !leg.progress.Matched || !leg.state.EndedAt.IsZero() {
		return
	}
	if !now.Before(lastFollowed(leg.leg())) || leg.progress.Landed >= 2 {
		leg.state.EndedAt = now
		return
	}
	for _, slot := range flightSlots(leg.leg()) {
		if slot.at.After(leg.progress.LastSlot) {
			return
		}
	}
	leg.state.EndedAt = now
}

// pickLeg finds the stored leg among those returned, by its scheduled departure and origin.
func pickLeg(found []FlightLeg, previous FlightLeg) (FlightLeg, bool) {
	for _, candidate := range found {
		if candidate.Departure.Scheduled.Equal(previous.Departure.Scheduled) || candidate.Origin.Code != "" && candidate.Origin.Code == previous.Origin.Code {
			return candidate, true
		}
	}
	return FlightLeg{}, false
}

func (f *Flights) failed(ctx context.Context, leg *trackedLeg, stage string, err error) {
	if ctx.Err() == nil {
		f.logger.WarnContext(ctx, "trip.flights.failed", "principal_id", leg.trip.PrincipalID, "trip_id", leg.trip.ID, "stage", stage, "error", err)
	}
}

// flightChange is one change a lookup found: the field it is in and the line that tells it.
type flightChange struct {
	field string
	line  string
}

func changeLines(changes []flightChange) []string {
	lines := make([]string, 0, len(changes))
	for _, change := range changes {
		lines = append(lines, change.line)
	}
	return lines
}

// compare finds what changed between the stored lookup and this one, records the new lookup and
// what it reports, and returns one change per line. The first lookup is the baseline, which
// reports only a cancellation, a diversion, or a delay. A cancellation ends the tracking of the
// leg.
func (f *Flights) compare(leg *trackedLeg, current FlightLeg, kind slotKind, baseline bool, now time.Time) []flightChange {
	snapshot := &leg.snapshot
	previous := leg.leg()
	designator := leg.state.Designator
	if current.Operator != "" {
		designator += " (operated by " + current.Operator + ")"
	}
	route := designator + " " + airportName(current.Origin) + " → " + airportName(current.Destination)
	var changes []flightChange
	defer func() { snapshot.Leg = &current }()

	switch current.Status {
	case FlightCancelled:
		leg.state.EndedAt = now
		return []flightChange{{"status", designator + " is cancelled. Check the airline's app for rebooking."}}
	case FlightDiverted:
		if !leg.progress.noted(flightNoteDiverted) {
			leg.progress.note(flightNoteDiverted)
			line := designator + " is diverted."
			if code := current.Destination.Code; code != "" && code != leg.state.Destination {
				line = fmt.Sprintf("%s is diverted to %s.", designator, airportLabel(current.Destination))
			}
			changes = append(changes, flightChange{"status", line})
		}
		return changes
	case FlightMaybeCancelled:
		if !baseline && !leg.progress.noted(flightNoteMaybe) {
			leg.progress.note(flightNoteMaybe)
			changes = append(changes, flightChange{"status", designator + " may be cancelled. Check the airline's app."})
		}
	}

	departed := current.Status == FlightDeparted || current.Status == FlightLanded || !current.Departure.Actual.IsZero()
	if !departed {
		if line := delayLine(route, designator, "departure", "departs", current.Departure, current.Origin, &snapshot.ReportedDeparture); line != "" {
			changes = append(changes, flightChange{"departure", line})
		}
		if !baseline && kind == slotAfter && current.Departure.Expected.IsZero() && !leg.progress.noted(flightNoteLate) {
			leg.progress.note(flightNoteLate)
			changes = append(changes, flightChange{"departure", fmt.Sprintf("%s: departure delayed at least 30 min (was %s).", route, clock(current.Departure.Scheduled, current.Origin))})
		}
	} else if line := delayLine(route, designator, "arrival", "arrives", current.Arrival, current.Destination, &snapshot.ReportedArrival); line != "" {
		changes = append(changes, flightChange{"arrival", line})
	}
	if !baseline && !departed {
		if line := gateLine(designator, current, previous, snapshot, now); line != "" {
			changes = append(changes, flightChange{"gate", line})
		}
	}
	return changes
}

// delayLine reports a movement's delay when its expected time moved by the threshold from the time
// last reported and is that far from its schedule, or when a delay reported before shrank below it.
func delayLine(route, designator, what, verb string, times FlightTimes, airport FlightAirport, reported *time.Time) string {
	if times.Expected.IsZero() {
		return ""
	}
	delay := times.Expected.Sub(times.Scheduled)
	switch {
	case delay >= flightDelayThreshold:
		was := *reported
		if was.IsZero() {
			was = times.Scheduled
		} else if gap := times.Expected.Sub(was); gap < flightDelayThreshold && gap > -flightDelayThreshold {
			return ""
		}
		*reported = times.Expected
		return fmt.Sprintf("%s: %s delayed %d min, now %s (was %s).", route, what, int(delay.Minutes()), clock(times.Expected, airport), clock(was, airport))
	case !reported.IsZero():
		*reported = time.Time{}
		return fmt.Sprintf("%s is back on time: %s %s.", designator, verb, clock(times.Expected, airport))
	}
	return ""
}

// gateLine reports a departure gate or terminal that changed from a known one, and a first gate
// within four hours of departure.
func gateLine(designator string, current, previous FlightLeg, snapshot *flightSnapshot, now time.Time) string {
	gate, terminal := current.Departure.Gate, current.Departure.Terminal
	knownGate, knownTerminal := previous.Departure.Gate, previous.Departure.Terminal
	var parts []string
	if knownGate == "" {
		// A lookup without a gate between two with one compares the new one with the gate last
		// reported, so a gate that comes back unchanged says nothing.
		knownGate = snapshot.ReportedGate
	}
	switch {
	case gate != "" && knownGate != "" && gate != knownGate:
		parts = append(parts, fmt.Sprintf("gate %s (was %s)", gate, knownGate))
	case gate != "" && snapshot.ReportedGate == "" && !now.Before(current.Departure.Scheduled.Add(-flightGateLead)):
		parts = append(parts, "gate "+gate)
	}
	terminalChanged := terminal != "" && knownTerminal != "" && terminal != knownTerminal
	switch {
	case terminalChanged:
		parts = append(parts, fmt.Sprintf("terminal %s (was %s)", terminal, knownTerminal))
	case len(parts) > 0 && terminal != "":
		parts = append(parts, "terminal "+terminal)
	}
	if len(parts) == 0 {
		return ""
	}
	if gate != "" {
		snapshot.ReportedGate = gate
	}
	return designator + ": " + strings.Join(parts, ", ") + "."
}

func airportName(airport FlightAirport) string {
	if airport.Name != "" {
		return airport.Name
	}
	return airport.Code
}

func airportLabel(airport FlightAirport) string {
	if airport.Name == "" || airport.Code == "" {
		return airportName(airport)
	}
	return airport.Name + " (" + airport.Code + ")"
}

// clock is a time local to the airport it belongs to.
func clock(at time.Time, airport FlightAirport) string {
	return at.In(localtime.SavedZone(airport.Zone)).Format(flightClock)
}

// pause stops every lookup until the allowance's period ends, and tells each owner with a flight in
// the window once per period, in that trip's tab.
func (f *Flights) pause(ctx context.Context, paused *FlightPausedError, legs []trackedLeg) []flightMessage {
	f.mu.Lock()
	f.resumeAt = paused.Until
	f.mu.Unlock()
	f.logger.WarnContext(ctx, "trip.flights.paused", "until", paused.Until)
	var messages []flightMessage
	told := map[string]bool{}
	for _, leg := range legs {
		principalID := leg.trip.PrincipalID
		if told[principalID] {
			continue
		}
		told[principalID] = true
		noticed, err := f.states.FlightPauseNoticed(ctx, principalID)
		if err != nil {
			f.failed(ctx, &leg, "pause", err)
			continue
		}
		if !noticed.Before(paused.Until) {
			continue
		}
		if err := f.states.SaveFlightPauseNoticed(ctx, principalID, paused.Until); err != nil {
			f.failed(ctx, &leg, "pause", err)
			continue
		}
		messages = append(messages, flightMessage{principalID: principalID, tripID: leg.trip.ID, title: leg.trip.Title, lines: []string{paused.Text}})
	}
	return messages
}

// send delivers each message to its trip's tab, in the tab's language with the source line added
// after, and records the changes it reports once it was sent.
func (f *Flights) send(ctx context.Context, messages []flightMessage) {
	for _, message := range messages {
		body := strings.Join(message.lines, "\n")
		if f.language != nil {
			body = f.language.localize(ctx, message.principalID, message.tripID, body)
		}
		text := body
		if f.attribution != "" {
			text += "\n" + f.attribution
		}
		if err := f.sender.Send(ctx, message.principalID, message.tripID, text); err != nil {
			if ctx.Err() == nil {
				f.logger.WarnContext(ctx, "trip.flights.send_failed", "principal_id", message.principalID, "trip_id", message.tripID, "error", err)
			}
			continue
		}
		f.notices.rememberFlight(message.principalID, message.title, strings.Join(message.lines, "\n"))
		if len(message.changes) == 0 {
			continue
		}
		at := f.now()
		for index := range message.changes {
			message.changes[index].SentAt = at
		}
		storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), updateStoreTimeout)
		if err := f.sent.RecordSentUpdates(storeCtx, message.principalID, message.tripID, message.changes); err != nil {
			f.logger.WarnContext(ctx, "trip.flights.failed", "principal_id", message.principalID, "trip_id", message.tripID, "stage", "record", "error", err)
		}
		cancel()
		f.logger.InfoContext(ctx, "trip.flights.sent", "principal_id", message.principalID, "trip_id", message.tripID, "changes", len(message.changes))
	}
}

// prune forgets the flight data of legs that ended a week ago.
func (f *Flights) prune(ctx context.Context, now time.Time) error {
	return f.states.PruneFlightStates(ctx, now.Add(-flightKept))
}

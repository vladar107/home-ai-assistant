package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/trip"
)

var _ trip.FlightLog = (*Store)(nil)

func (s *Store) LoadFlightState(ctx context.Context, principalID, tripID, bookingID string) (trip.FlightState, bool, error) {
	if s == nil || s.db == nil {
		return trip.FlightState{}, false, fmt.Errorf("load flight state: SQLite store is not open")
	}
	state := trip.FlightState{PrincipalID: principalID, TripID: tripID, BookingID: bookingID}
	var bookedAt, lookup, progress string
	var attemptAt, successAt, endedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT designator, booked_at, day, origin, destination, lookup, progress, last_attempt_at, last_success_at, ended_at
		 FROM trip_flights WHERE trip_id = ? AND booking_id = ? AND principal_id = ?`, tripID, bookingID, principalID,
	).Scan(&state.Designator, &bookedAt, &state.Day, &state.Origin, &state.Destination, &lookup, &progress, &attemptAt, &successAt, &endedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return trip.FlightState{}, false, nil
	}
	if err != nil {
		return trip.FlightState{}, false, fmt.Errorf("load flight state: %w", err)
	}
	if state.BookedAt, err = time.Parse(time.RFC3339Nano, bookedAt); err != nil {
		return trip.FlightState{}, false, fmt.Errorf("parse flight booking time: %w", err)
	}
	state.Lookup, state.Progress = []byte(lookup), []byte(progress)
	for target, value := range map[*time.Time]sql.NullString{&state.LastAttemptAt: attemptAt, &state.LastSuccessAt: successAt, &state.EndedAt: endedAt} {
		if *target, err = parseOptionalTime(value); err != nil {
			return trip.FlightState{}, false, err
		}
	}
	return state, true, nil
}

// SaveFlightState replaces the booking's state, for a trip the principal owns.
func (s *Store) SaveFlightState(ctx context.Context, state trip.FlightState) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save flight state: SQLite store is not open")
	}
	lookup, progress := string(state.Lookup), string(state.Progress)
	if lookup == "" {
		lookup = "{}"
	}
	if progress == "" {
		progress = "{}"
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO trip_flights (trip_id, booking_id, principal_id, designator, booked_at, day, origin, destination, lookup, progress, last_attempt_at, last_success_at, ended_at)
		 SELECT id, ?, principal_id, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? FROM trips WHERE id = ? AND principal_id = ?
		 ON CONFLICT (trip_id, booking_id) DO UPDATE SET
		     designator = excluded.designator, booked_at = excluded.booked_at, day = excluded.day, origin = excluded.origin,
		     destination = excluded.destination, lookup = excluded.lookup, progress = excluded.progress,
		     last_attempt_at = excluded.last_attempt_at, last_success_at = excluded.last_success_at, ended_at = excluded.ended_at`,
		state.BookingID, state.Designator, formatTime(state.BookedAt), state.Day, state.Origin, state.Destination, lookup, progress,
		nullableTime(state.LastAttemptAt), nullableTime(state.LastSuccessAt), nullableTime(state.EndedAt), state.TripID, state.PrincipalID,
	); err != nil {
		return fmt.Errorf("save flight state: %w", err)
	}
	return nil
}

func (s *Store) FlightPauseNoticed(ctx context.Context, principalID string) (time.Time, error) {
	if s == nil || s.db == nil {
		return time.Time{}, fmt.Errorf("load flight pause: SQLite store is not open")
	}
	var until string
	err := s.db.QueryRowContext(ctx, `SELECT until FROM trip_flight_pauses WHERE principal_id = ?`, principalID).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("load flight pause: %w", err)
	}
	return parseOptionalTime(sql.NullString{String: until, Valid: true})
}

func (s *Store) SaveFlightPauseNoticed(ctx context.Context, principalID string, until time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save flight pause: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO trip_flight_pauses (principal_id, until) VALUES (?, ?)
		 ON CONFLICT (principal_id) DO UPDATE SET until = excluded.until`, principalID, formatTime(until)); err != nil {
		return fmt.Errorf("save flight pause: %w", err)
	}
	return nil
}

// PruneFlightStates forgets the flight data of every leg that ended before the instant: its stored
// lookup and the text of its changes in the sent log. The designator, the day, and the airport
// codes stay, as identifiers. The state of a booking that is no longer on its trip is deleted.
func (s *Store) PruneFlightStates(ctx context.Context, before time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("prune flight states: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin prune flight states: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE trip_updates_sent SET text = ''
		 WHERE source = 'flight' AND text != '' AND EXISTS (
		     SELECT 1 FROM trip_flights f
		     WHERE f.trip_id = trip_updates_sent.trip_id AND f.ended_at IS NOT NULL AND f.ended_at < ? AND f.day != ''
		       AND substr(trip_updates_sent.reference, 1, length(f.designator) + length(f.day) + 2) = f.designator || '|' || f.day || '|')`,
		formatTime(before)); err != nil {
		return fmt.Errorf("prune flight updates: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE trip_flights SET lookup = '{}' WHERE ended_at IS NOT NULL AND ended_at < ? AND lookup != '{}'`, formatTime(before)); err != nil {
		return fmt.Errorf("prune flight lookups: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM trip_flights WHERE NOT EXISTS (
		     SELECT 1 FROM trip_bookings b WHERE b.trip_id = trip_flights.trip_id AND b.id = trip_flights.booking_id)`); err != nil {
		return fmt.Errorf("prune flight states: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit prune flight states: %w", err)
	}
	return nil
}

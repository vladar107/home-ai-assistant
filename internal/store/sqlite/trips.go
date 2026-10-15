package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/trip"
)

var _ trip.Repository = (*Store)(nil)

const (
	tripColumns = `id, principal_id, title, destination, starts_at, ends_at, zone, status, source,
		started_notified_at, ended_notified_at, created_at, updated_at`
	bookingColumns = `id, kind, title, reference, starts_at, check_in_at, check_in_source, fingerprint`
)

func (s *Store) ListTrips(ctx context.Context, principalID, status string) ([]trip.Trip, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list trips: SQLite store is not open")
	}
	return s.queryTrips(ctx, true,
		`SELECT `+tripColumns+` FROM trips WHERE principal_id = ? AND status = ? ORDER BY starts_at, rowid`, principalID, status)
}

func (s *Store) ListNoticeTrips(ctx context.Context, now time.Time) ([]trip.Trip, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list trips: SQLite store is not open")
	}
	return s.queryTrips(ctx, true,
		`SELECT `+tripColumns+` FROM trips
		 WHERE status IN ('planned', 'cancelled') AND ended_notified_at IS NULL
		   AND (started_notified_at IS NOT NULL OR (status = 'planned' AND ends_at > ?))
		 ORDER BY starts_at, rowid`, formatTime(now))
}

func (s *Store) LoadTrip(ctx context.Context, principalID, id string) (trip.Trip, bool, error) {
	return s.loadTrip(ctx, "load trip", `principal_id = ? AND id = ?`, principalID, id)
}

func (s *Store) FindTripsByFingerprints(ctx context.Context, principalID string, fingerprints []string) ([]trip.Trip, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("find trips: SQLite store is not open")
	}
	var wanted []any
	for _, fingerprint := range fingerprints {
		if fingerprint != "" {
			wanted = append(wanted, fingerprint)
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	args := append([]any{principalID}, wanted...)
	return s.queryTrips(ctx, true,
		`SELECT `+tripColumns+` FROM trips WHERE principal_id = ? AND id IN
		 (SELECT trip_id FROM trip_bookings WHERE fingerprint IN (`+strings.TrimSuffix(strings.Repeat("?,", len(wanted)), ",")+`))
		 ORDER BY starts_at, rowid`, args...)
}

func (s *Store) ListActiveTrips(ctx context.Context, principalID string, at time.Time) ([]trip.Trip, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list trips: SQLite store is not open")
	}
	return s.queryTrips(ctx, false,
		`SELECT `+tripColumns+` FROM trips WHERE principal_id = ? AND status = 'planned' AND starts_at <= ? AND ends_at > ?
		 ORDER BY starts_at, rowid`, principalID, formatTime(at), formatTime(at))
}

func (s *Store) ResetTripNotices(ctx context.Context, principalID, id string, started, ended bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("reset trip notices: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE trips SET started_notified_at = CASE WHEN ? THEN NULL ELSE started_notified_at END,
		   ended_notified_at = CASE WHEN ? THEN NULL ELSE ended_notified_at END
		 WHERE principal_id = ? AND id = ?`, started, ended, principalID, id); err != nil {
		return fmt.Errorf("reset trip notices: %w", err)
	}
	return nil
}

func (s *Store) loadTrip(ctx context.Context, action, where string, args ...any) (trip.Trip, bool, error) {
	if s == nil || s.db == nil {
		return trip.Trip{}, false, fmt.Errorf("%s: SQLite store is not open", action)
	}
	trips, err := s.queryTrips(ctx, true, `SELECT `+tripColumns+` FROM trips WHERE `+where, args...)
	if err != nil || len(trips) == 0 {
		return trip.Trip{}, false, err
	}
	return trips[0], true, nil
}

func (s *Store) queryTrips(ctx context.Context, withBookings bool, query string, args ...any) ([]trip.Trip, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query trips: %w", err)
	}
	var trips []trip.Trip
	for rows.Next() {
		item, err := scanTrip(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		trips = append(trips, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read trips: %w", err)
	}
	rows.Close()
	for index := range trips {
		if !withBookings {
			break
		}
		if trips[index].Bookings, err = s.queryBookings(ctx, trips[index].ID); err != nil {
			return nil, err
		}
	}
	return trips, nil
}

func (s *Store) queryBookings(ctx context.Context, tripID string) ([]trip.Booking, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+bookingColumns+` FROM trip_bookings WHERE trip_id = ? ORDER BY position`, tripID)
	if err != nil {
		return nil, fmt.Errorf("query trip bookings: %w", err)
	}
	defer rows.Close()
	var bookings []trip.Booking
	for rows.Next() {
		var booking trip.Booking
		var startsAt string
		var checkInAt, source sql.NullString
		if err := rows.Scan(&booking.ID, &booking.Kind, &booking.Title, &booking.Reference, &startsAt, &checkInAt, &source, &booking.Fingerprint); err != nil {
			return nil, fmt.Errorf("scan trip booking: %w", err)
		}
		if booking.StartsAt, err = time.Parse(time.RFC3339Nano, startsAt); err != nil {
			return nil, fmt.Errorf("parse trip booking time: %w", err)
		}
		if booking.CheckInAt, err = parseOptionalTime(checkInAt); err != nil {
			return nil, err
		}
		booking.CheckInSource = source.String
		bookings = append(bookings, booking)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read trip bookings: %w", err)
	}
	return bookings, nil
}

func (s *Store) AddTrip(ctx context.Context, item trip.Trip) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("add trip: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin add trip: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO trips (`+tripColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.PrincipalID, item.Title, item.Destination, formatTime(item.StartsAt), formatTime(item.EndsAt),
		item.Zone, item.Status, item.Source,
		nullableTime(item.StartedNotifiedAt), nullableTime(item.EndedNotifiedAt),
		formatTime(item.CreatedAt), formatTime(item.UpdatedAt),
	); err != nil {
		return fmt.Errorf("add trip: %w", err)
	}
	if err := insertBookings(ctx, tx, item); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit add trip: %w", err)
	}
	return nil
}

func (s *Store) UpdateTrip(ctx context.Context, item trip.Trip) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("update trip: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin update trip: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE trips SET title = ?, destination = ?, starts_at = ?, ends_at = ?, zone = ?, status = ?, updated_at = ?
		 WHERE principal_id = ? AND id = ?`,
		item.Title, item.Destination, formatTime(item.StartsAt), formatTime(item.EndsAt), item.Zone, item.Status,
		formatTime(item.UpdatedAt), item.PrincipalID, item.ID)
	if err != nil {
		return false, fmt.Errorf("update trip: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read updated trip count: %w", err)
	}
	if changed != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM trip_bookings WHERE trip_id = ?`, item.ID); err != nil {
		return false, fmt.Errorf("replace trip bookings: %w", err)
	}
	if err := insertBookings(ctx, tx, item); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit update trip: %w", err)
	}
	return true, nil
}

func insertBookings(ctx context.Context, tx *sql.Tx, item trip.Trip) error {
	for position, booking := range item.Bookings {
		var source any
		if booking.CheckInSource != "" {
			source = booking.CheckInSource
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO trip_bookings (id, principal_id, trip_id, kind, title, reference, starts_at, check_in_at, check_in_source, fingerprint, position)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			booking.ID, item.PrincipalID, item.ID, booking.Kind, booking.Title, booking.Reference, formatTime(booking.StartsAt),
			nullableTime(booking.CheckInAt), source, booking.Fingerprint, position,
		); err != nil {
			return fmt.Errorf("add trip booking: %w", err)
		}
	}
	return nil
}

// MarkTripStarted records the start notice only while it is still due, so a trip postponed or
// removed while the notice was being sent owes it again. A trip cancelled in that time still
// records it, because its owner was told it had started.
func (s *Store) MarkTripStarted(ctx context.Context, principalID, id string, at time.Time) (bool, error) {
	return s.markTrip(ctx, `started_notified_at = ?`,
		`started_notified_at IS NULL AND status IN ('planned', 'cancelled') AND starts_at <= ?`, principalID, id, at)
}

// MarkTripEnded records the end notice only while it is still due: the trip was announced as
// started and has ended or was cancelled.
func (s *Store) MarkTripEnded(ctx context.Context, principalID, id string, at time.Time) (bool, error) {
	return s.markTrip(ctx, `ended_notified_at = ?`,
		`ended_notified_at IS NULL AND started_notified_at IS NOT NULL AND (status = 'cancelled' OR ends_at <= ?)`, principalID, id, at)
}

func (s *Store) markTrip(ctx context.Context, set, due, principalID, id string, at time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("mark trip: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE trips SET `+set+` WHERE principal_id = ? AND id = ? AND `+due,
		formatTime(at), principalID, id, formatTime(at))
	if err != nil {
		return false, fmt.Errorf("mark trip: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read marked trip count: %w", err)
	}
	return changed == 1, nil
}

func (s *Store) DeleteTrip(ctx context.Context, principalID, id string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("delete trip: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM trips WHERE principal_id = ? AND id = ? AND status = 'suggested'`, principalID, id); err != nil {
		return fmt.Errorf("delete trip: %w", err)
	}
	return nil
}

func (s *Store) PruneTrips(ctx context.Context, before time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("prune trips: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM trips WHERE ends_at < ?`, formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("prune trips: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read pruned trip count: %w", err)
	}
	return int(removed), nil
}

func parseOptionalTime(value sql.NullString) (time.Time, error) {
	if !value.Valid {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse trip time: %w", err)
	}
	return parsed, nil
}

func scanTrip(row rowScanner) (trip.Trip, error) {
	var item trip.Trip
	var startsAt, endsAt, createdAt, updatedAt string
	var startedAt, endedAt sql.NullString
	if err := row.Scan(&item.ID, &item.PrincipalID, &item.Title, &item.Destination, &startsAt, &endsAt, &item.Zone,
		&item.Status, &item.Source, &startedAt, &endedAt, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return trip.Trip{}, err
		}
		return trip.Trip{}, fmt.Errorf("scan trip: %w", err)
	}
	var err error
	for target, value := range map[*time.Time]string{&item.StartsAt: startsAt, &item.EndsAt: endsAt, &item.CreatedAt: createdAt, &item.UpdatedAt: updatedAt} {
		if *target, err = time.Parse(time.RFC3339Nano, value); err != nil {
			return trip.Trip{}, fmt.Errorf("parse trip time: %w", err)
		}
	}
	if item.StartedNotifiedAt, err = parseOptionalTime(startedAt); err != nil {
		return trip.Trip{}, err
	}
	if item.EndedNotifiedAt, err = parseOptionalTime(endedAt); err != nil {
		return trip.Trip{}, err
	}
	return item, nil
}

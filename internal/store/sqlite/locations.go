package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/location"
)

var _ location.Repository = (*Store)(nil)

const locationColumns = `principal_id, latitude, longitude, accuracy, source, venue_title, venue_address,
    live_message_id, live_until, ended_at, updated_at`

func (s *Store) PutLocation(ctx context.Context, fix location.Fix) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("put location: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin location transaction: %w", err)
	}
	defer tx.Rollback()
	if err := upsertLocation(ctx, tx, fix); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit location: %w", err)
	}
	return nil
}

func (s *Store) MoveLiveLocation(ctx context.Context, fix location.Fix) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("move live location: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin location transaction: %w", err)
	}
	defer tx.Rollback()

	var ignored int64
	err = tx.QueryRowContext(ctx, `SELECT live_message_id FROM ignored_live_locations WHERE principal_id = ?`, fix.PrincipalID).Scan(&ignored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("query ignored live location: %w", err)
	}
	if ignored == fix.LiveMessageID {
		return false, nil
	}
	stored, found, err := loadLocation(ctx, tx, fix.PrincipalID)
	if err != nil {
		return false, err
	}
	// Without a stored position the edit restores its share, as when a prune removed a live share
	// that fell silent and later moved again.
	if found && (stored.LiveMessageID != fix.LiveMessageID || !stored.EndedAt.IsZero() || stored.UpdatedAt.After(fix.UpdatedAt)) {
		return false, nil
	}
	if err := upsertLocation(ctx, tx, fix); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit location: %w", err)
	}
	return true, nil
}

func (s *Store) LoadLocation(ctx context.Context, principalID string) (location.Fix, bool, error) {
	if s == nil || s.db == nil {
		return location.Fix{}, false, fmt.Errorf("load location: SQLite store is not open")
	}
	return loadLocation(ctx, s.db, principalID)
}

func (s *Store) ListLocations(ctx context.Context) ([]location.Fix, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list locations: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+locationColumns+` FROM user_locations ORDER BY principal_id`)
	if err != nil {
		return nil, fmt.Errorf("query locations: %w", err)
	}
	defer rows.Close()
	var fixes []location.Fix
	for rows.Next() {
		fix, err := scanLocation(rows)
		if err != nil {
			return nil, err
		}
		fixes = append(fixes, fix)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read locations: %w", err)
	}
	return fixes, nil
}

func (s *Store) DeleteLocation(ctx context.Context, principalID string, updatedAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("delete location: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_locations WHERE principal_id = ? AND updated_at = ?`,
		principalID, formatTime(updatedAt)); err != nil {
		return fmt.Errorf("delete location: %w", err)
	}
	return nil
}

func (s *Store) ForgetLocation(ctx context.Context, principalID string, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("forget location: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin location transaction: %w", err)
	}
	defer tx.Rollback()
	stored, found, err := loadLocation(ctx, tx, principalID)
	if err != nil || !found {
		return false, err
	}
	if stored.Live(now) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ignored_live_locations (principal_id, live_message_id) VALUES (?, ?)
			 ON CONFLICT (principal_id) DO UPDATE SET live_message_id = excluded.live_message_id`,
			principalID, stored.LiveMessageID); err != nil {
			return false, fmt.Errorf("ignore live location: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_locations WHERE principal_id = ?`, principalID); err != nil {
		return false, fmt.Errorf("forget location: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit location: %w", err)
	}
	return true, nil
}

func loadLocation(ctx context.Context, db queryRower, principalID string) (location.Fix, bool, error) {
	fix, err := scanLocation(db.QueryRowContext(ctx, `SELECT `+locationColumns+` FROM user_locations WHERE principal_id = ?`, principalID))
	if errors.Is(err, sql.ErrNoRows) {
		return location.Fix{}, false, nil
	}
	if err != nil {
		return location.Fix{}, false, err
	}
	return fix, true, nil
}

func upsertLocation(ctx context.Context, tx *sql.Tx, fix location.Fix) error {
	var liveMessageID any
	if fix.LiveMessageID != 0 {
		liveMessageID = fix.LiveMessageID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_locations (`+locationColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (principal_id) DO UPDATE
		 SET latitude = excluded.latitude,
		     longitude = excluded.longitude,
		     accuracy = excluded.accuracy,
		     source = excluded.source,
		     venue_title = excluded.venue_title,
		     venue_address = excluded.venue_address,
		     live_message_id = excluded.live_message_id,
		     live_until = excluded.live_until,
		     ended_at = excluded.ended_at,
		     updated_at = excluded.updated_at`,
		fix.PrincipalID, fix.Latitude, fix.Longitude, fix.Accuracy, string(fix.Source), fix.VenueTitle, fix.VenueAddress,
		liveMessageID, nullableTime(fix.LiveUntil), nullableTime(fix.EndedAt), formatTime(fix.UpdatedAt),
	); err != nil {
		return fmt.Errorf("save location: %w", err)
	}
	return nil
}

func scanLocation(row rowScanner) (location.Fix, error) {
	var fix location.Fix
	var source, updatedAt string
	var liveMessageID sql.NullInt64
	var liveUntil, endedAt sql.NullString
	if err := row.Scan(&fix.PrincipalID, &fix.Latitude, &fix.Longitude, &fix.Accuracy, &source, &fix.VenueTitle,
		&fix.VenueAddress, &liveMessageID, &liveUntil, &endedAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return location.Fix{}, err
		}
		return location.Fix{}, fmt.Errorf("scan location: %w", err)
	}
	fix.Source = location.Source(source)
	fix.LiveMessageID = liveMessageID.Int64
	var err error
	if fix.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return location.Fix{}, fmt.Errorf("parse location time: %w", err)
	}
	for _, field := range []struct {
		value  sql.NullString
		target *time.Time
	}{{liveUntil, &fix.LiveUntil}, {endedAt, &fix.EndedAt}} {
		if !field.value.Valid {
			continue
		}
		if *field.target, err = time.Parse(time.RFC3339Nano, field.value.String); err != nil {
			return location.Fix{}, fmt.Errorf("parse location time: %w", err)
		}
	}
	return fix, nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return formatTime(value)
}

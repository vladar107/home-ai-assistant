package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/location"
)

var _ location.PlaceRepository = (*Store)(nil)

const placeColumns = `id, principal_id, name, latitude, longitude, radius_m, created_at, updated_at`

func (s *Store) CreatePlace(ctx context.Context, place location.Place, limit int) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("create place: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin place creation: %w", err)
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM places WHERE id = ?)`, place.ID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check place: %w", err)
	}
	if exists {
		return false, nil
	}
	if err := checkPlaceRoom(ctx, tx, place, limit); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO places (id, principal_id, name, name_key, latitude, longitude, radius_m, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		place.ID, place.PrincipalID, place.Name, location.NameKey(place.Name), place.Latitude, place.Longitude, place.RadiusM,
		formatTime(place.CreatedAt), formatTime(place.UpdatedAt),
	); err != nil {
		return false, fmt.Errorf("insert place: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit place creation: %w", err)
	}
	return true, nil
}

func (s *Store) ListPlaces(ctx context.Context, principalID string) ([]location.Place, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list places: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+placeColumns+` FROM places WHERE principal_id = ? AND deleted_at IS NULL ORDER BY created_at, id`, principalID)
	if err != nil {
		return nil, fmt.Errorf("list places: %w", err)
	}
	defer rows.Close()
	var places []location.Place
	for rows.Next() {
		place, err := scanPlace(rows)
		if err != nil {
			return nil, err
		}
		places = append(places, place)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate places: %w", err)
	}
	return places, nil
}

func (s *Store) LoadPlace(ctx context.Context, principalID, placeID string) (location.Place, bool, error) {
	if s == nil || s.db == nil {
		return location.Place{}, false, fmt.Errorf("load place: SQLite store is not open")
	}
	place, err := scanPlace(s.db.QueryRowContext(ctx, `SELECT `+placeColumns+` FROM places WHERE id = ? AND principal_id = ? AND deleted_at IS NULL`, placeID, principalID))
	if errors.Is(err, sql.ErrNoRows) {
		return location.Place{}, false, nil
	}
	if err != nil {
		return location.Place{}, false, err
	}
	return place, true, nil
}

func (s *Store) UpdatePlace(ctx context.Context, place location.Place, expectedUpdatedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("update place: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin place update: %w", err)
	}
	defer tx.Rollback()
	if err := checkPlaceName(ctx, tx, place); err != nil {
		return false, err
	}
	var radius int
	err = tx.QueryRowContext(ctx, `SELECT radius_m FROM places WHERE id = ? AND principal_id = ? AND deleted_at IS NULL`, place.ID, place.PrincipalID).Scan(&radius)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load place radius: %w", err)
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE places SET name = ?, name_key = ?, radius_m = ?, updated_at = ? WHERE id = ? AND principal_id = ? AND updated_at = ? AND deleted_at IS NULL`,
		place.Name, location.NameKey(place.Name), place.RadiusM, formatTime(place.UpdatedAt), place.ID, place.PrincipalID, formatTime(expectedUpdatedAt),
	)
	if err != nil {
		return false, fmt.Errorf("update place: %w", err)
	}
	if updated, err := affectedOne(result, "updated place"); err != nil || !updated {
		return false, err
	}
	// A new radius moves the boundary, so what the old one decided no longer holds; a new name
	// changes nothing about where the user is.
	if radius != place.RadiusM {
		if _, err := tx.ExecContext(ctx, `DELETE FROM place_presence WHERE place_id = ? AND principal_id = ?`, place.ID, place.PrincipalID); err != nil {
			return false, fmt.Errorf("forget place presence: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit place update: %w", err)
	}
	return true, nil
}

func (s *Store) DeletePlace(ctx context.Context, principalID, placeID string, updatedAt, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete place: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin place deletion: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE places SET deleted_at = ? WHERE id = ? AND principal_id = ? AND updated_at = ? AND deleted_at IS NULL`,
		formatTime(now), placeID, principalID, formatTime(updatedAt))
	if err != nil {
		return false, fmt.Errorf("delete place: %w", err)
	}
	if deleted, err := affectedOne(result, "deleted place"); err != nil || !deleted {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM place_presence WHERE place_id = ? AND principal_id = ?`, placeID, principalID); err != nil {
		return false, fmt.Errorf("forget place presence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit place deletion: %w", err)
	}
	return true, nil
}

func (s *Store) RestorePlace(ctx context.Context, principalID, placeID string, deletedAt time.Time, limit int) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("restore place: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin place restore: %w", err)
	}
	defer tx.Rollback()
	var name string
	err = tx.QueryRowContext(ctx, `SELECT name FROM places WHERE id = ? AND principal_id = ? AND deleted_at = ?`, placeID, principalID, formatTime(deletedAt)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load deleted place: %w", err)
	}
	if err := checkPlaceRoom(ctx, tx, location.Place{ID: placeID, PrincipalID: principalID, Name: name}, limit); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE places SET deleted_at = NULL WHERE id = ? AND principal_id = ? AND deleted_at = ?`, placeID, principalID, formatTime(deletedAt))
	if err != nil {
		return false, fmt.Errorf("restore place: %w", err)
	}
	if restored, err := affectedOne(result, "restored place"); err != nil || !restored {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit place restore: %w", err)
	}
	return true, nil
}

func (s *Store) PurgePlaces(ctx context.Context, before time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("purge places: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM places WHERE deleted_at IS NOT NULL AND deleted_at < ?`, formatTime(before)); err != nil {
		return fmt.Errorf("purge places: %w", err)
	}
	return nil
}

func (s *Store) ListPresence(ctx context.Context, principalID string) ([]location.Presence, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list presence: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT place_id, principal_id, state, COALESCE(candidate, ''), candidate_since, candidate_fixes, live_message_id, updated_at
		 FROM place_presence WHERE principal_id = ? ORDER BY place_id`, principalID)
	if err != nil {
		return nil, fmt.Errorf("list presence: %w", err)
	}
	defer rows.Close()
	var presences []location.Presence
	for rows.Next() {
		var presence location.Presence
		var since sql.NullString
		var updatedAt string
		if err := rows.Scan(&presence.PlaceID, &presence.PrincipalID, &presence.State, &presence.Candidate, &since,
			&presence.CandidateFixes, &presence.LiveMessageID, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan presence: %w", err)
		}
		if presence.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
			return nil, fmt.Errorf("parse presence time: %w", err)
		}
		if since.Valid {
			if presence.CandidateSince, err = time.Parse(time.RFC3339Nano, since.String); err != nil {
				return nil, fmt.Errorf("parse presence time: %w", err)
			}
		}
		presences = append(presences, presence)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate presence: %w", err)
	}
	return presences, nil
}

func (s *Store) SavePresence(ctx context.Context, presence location.Presence) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save presence: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO place_presence (place_id, principal_id, state, candidate, candidate_since, candidate_fixes, live_message_id, updated_at)
		 SELECT ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM places WHERE id = ? AND principal_id = ? AND deleted_at IS NULL)
		 ON CONFLICT (place_id) DO UPDATE
		 SET state = excluded.state,
		     candidate = excluded.candidate,
		     candidate_since = excluded.candidate_since,
		     candidate_fixes = excluded.candidate_fixes,
		     live_message_id = excluded.live_message_id,
		     updated_at = excluded.updated_at
		 WHERE place_presence.principal_id = excluded.principal_id`,
		presence.PlaceID, presence.PrincipalID, presence.State, presence.Candidate, nullableTime(presence.CandidateSince),
		presence.CandidateFixes, presence.LiveMessageID, formatTime(presence.UpdatedAt), presence.PlaceID, presence.PrincipalID,
	); err != nil {
		return fmt.Errorf("save presence: %w", err)
	}
	return nil
}

func (s *Store) ResetPresence(ctx context.Context, principalID string, liveMessageID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("reset presence: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM place_presence WHERE principal_id = ? AND (? = 0 OR live_message_id = ?)`,
		principalID, liveMessageID, liveMessageID); err != nil {
		return fmt.Errorf("reset presence: %w", err)
	}
	return nil
}

// checkPlaceRoom refuses a place beyond the principal's limit or with a name another of theirs has.
func checkPlaceRoom(ctx context.Context, tx *sql.Tx, place location.Place, limit int) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM places WHERE principal_id = ? AND deleted_at IS NULL`, place.PrincipalID).Scan(&count); err != nil {
		return fmt.Errorf("count places: %w", err)
	}
	if count >= limit {
		return location.ErrTooManyPlaces
	}
	return checkPlaceName(ctx, tx, place)
}

// checkPlaceName refuses a name another of the principal's places has, so the unique index is
// never what turns a confirmed place down.
func checkPlaceName(ctx context.Context, tx *sql.Tx, place location.Place) error {
	var taken bool
	if err := tx.QueryRowContext(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM places WHERE principal_id = ? AND name_key = ? AND id != ? AND deleted_at IS NULL)`,
		place.PrincipalID, location.NameKey(place.Name), place.ID,
	).Scan(&taken); err != nil {
		return fmt.Errorf("check place name: %w", err)
	}
	if taken {
		return location.ErrPlaceNameTaken
	}
	return nil
}

func scanPlace(row rowScanner) (location.Place, error) {
	var place location.Place
	var createdAt, updatedAt string
	if err := row.Scan(&place.ID, &place.PrincipalID, &place.Name, &place.Latitude, &place.Longitude, &place.RadiusM, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return location.Place{}, err
		}
		return location.Place{}, fmt.Errorf("scan place: %w", err)
	}
	var err error
	if place.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return location.Place{}, fmt.Errorf("parse place time: %w", err)
	}
	if place.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return location.Place{}, fmt.Errorf("parse place time: %w", err)
	}
	return place, nil
}

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/routes"
)

var _ routes.Repository = (*Store)(nil)

const routeColumns = `id, principal_id, name, days, COALESCE(arrive_by, ''), legs, created_at, updated_at`

func (s *Store) CreateRoute(ctx context.Context, route routes.Route, limit int) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("create route: SQLite store is not open")
	}
	days, legs, err := encodeRoute(route)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin route creation: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM routes WHERE id = ?)`, route.ID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check route: %w", err)
	}
	if exists {
		return false, nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM routes WHERE principal_id = ?`, route.PrincipalID).Scan(&count); err != nil {
		return false, fmt.Errorf("count routes: %w", err)
	}
	if count >= limit {
		return false, routes.ErrTooManyRoutes
	}
	if err := checkRouteName(ctx, tx, route); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO routes (id, principal_id, name, days, arrive_by, legs, created_at, updated_at) VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?)`,
		route.ID, route.PrincipalID, route.Name, days, route.ArriveBy, legs, formatTime(route.CreatedAt), formatTime(route.UpdatedAt),
	); err != nil {
		return false, fmt.Errorf("insert route: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit route creation: %w", err)
	}
	return true, nil
}

func (s *Store) ListRoutes(ctx context.Context, principalID string) ([]routes.Route, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list routes: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+routeColumns+` FROM routes WHERE principal_id = ? ORDER BY created_at, id`, principalID)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	defer rows.Close()
	var list []routes.Route
	for rows.Next() {
		route, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, route)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate routes: %w", err)
	}
	return list, nil
}

func (s *Store) LoadRoute(ctx context.Context, principalID, routeID string) (routes.Route, bool, error) {
	if s == nil || s.db == nil {
		return routes.Route{}, false, fmt.Errorf("load route: SQLite store is not open")
	}
	route, err := scanRoute(s.db.QueryRowContext(ctx, `SELECT `+routeColumns+` FROM routes WHERE id = ? AND principal_id = ?`, routeID, principalID))
	if errors.Is(err, sql.ErrNoRows) {
		return routes.Route{}, false, nil
	}
	if err != nil {
		return routes.Route{}, false, err
	}
	return route, true, nil
}

func (s *Store) UpdateRoute(ctx context.Context, route routes.Route, expectedUpdatedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("update route: SQLite store is not open")
	}
	days, legs, err := encodeRoute(route)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin route update: %w", err)
	}
	defer tx.Rollback()
	if err := checkRouteName(ctx, tx, route); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE routes SET name = ?, days = ?, arrive_by = NULLIF(?, ''), legs = ?, updated_at = ? WHERE id = ? AND principal_id = ? AND updated_at = ?`,
		route.Name, days, route.ArriveBy, legs, formatTime(route.UpdatedAt), route.ID, route.PrincipalID, formatTime(expectedUpdatedAt),
	)
	if err != nil {
		return false, fmt.Errorf("update route: %w", err)
	}
	updated, err := affectedOne(result, "updated route")
	if err != nil || !updated {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit route update: %w", err)
	}
	return true, nil
}

func (s *Store) DeleteRoute(ctx context.Context, principalID, routeID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete route: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM routes WHERE id = ? AND principal_id = ?`, routeID, principalID)
	if err != nil {
		return false, fmt.Errorf("delete route: %w", err)
	}
	return affectedOne(result, "deleted route")
}

// checkRouteName refuses a name another of the principal's routes has, so the unique index is never
// what turns a confirmed route down.
func checkRouteName(ctx context.Context, tx *sql.Tx, route routes.Route) error {
	var taken bool
	if err := tx.QueryRowContext(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM routes WHERE principal_id = ? AND name = ? COLLATE NOCASE AND id != ?)`,
		route.PrincipalID, route.Name, route.ID,
	).Scan(&taken); err != nil {
		return fmt.Errorf("check route name: %w", err)
	}
	if taken {
		return routes.ErrNameTaken
	}
	return nil
}

func encodeRoute(route routes.Route) (string, string, error) {
	days, err := json.Marshal(route.Days)
	if err != nil {
		return "", "", fmt.Errorf("encode route days: %w", err)
	}
	legs, err := json.Marshal(route.Legs)
	if err != nil {
		return "", "", fmt.Errorf("encode route legs: %w", err)
	}
	return string(days), string(legs), nil
}

func scanRoute(row rowScanner) (routes.Route, error) {
	var route routes.Route
	var days, legs, createdAt, updatedAt string
	if err := row.Scan(&route.ID, &route.PrincipalID, &route.Name, &days, &route.ArriveBy, &legs, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return routes.Route{}, err
		}
		return routes.Route{}, fmt.Errorf("scan route: %w", err)
	}
	if err := json.Unmarshal([]byte(days), &route.Days); err != nil {
		return routes.Route{}, fmt.Errorf("decode route days: %w", err)
	}
	if err := json.Unmarshal([]byte(legs), &route.Legs); err != nil {
		return routes.Route{}, fmt.Errorf("decode route legs: %w", err)
	}
	var err error
	if route.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return routes.Route{}, fmt.Errorf("parse route creation time: %w", err)
	}
	if route.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return routes.Route{}, fmt.Errorf("parse route update time: %w", err)
	}
	return route, nil
}

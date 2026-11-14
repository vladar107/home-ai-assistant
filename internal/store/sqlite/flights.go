package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/flights"
)

var _ flights.Units = (*Store)(nil)

// ReserveFlightUnits adds the units to every limited scope of the period in one transaction, and
// only when each stays within its limit and the period is not marked spent. It returns "" when it
// added them, and otherwise the scope that refused, flights.ScopeSpent for a spent period. Earlier
// periods are deleted on the way, since nothing reads them.
func (s *Store) ReserveFlightUnits(ctx context.Context, period string, units int, limits []flights.UnitLimit) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("reserve flight units: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin flight units: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM flight_lookup_units WHERE period < ?`, period); err != nil {
		return "", fmt.Errorf("prune flight units: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM flight_lookup_periods WHERE period < ?`, period); err != nil {
		return "", fmt.Errorf("prune flight periods: %w", err)
	}
	var spent int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM flight_lookup_periods WHERE period = ?`, period).Scan(&spent); err != nil {
		return "", fmt.Errorf("query flight period: %w", err)
	}
	if spent > 0 {
		return flights.ScopeSpent, nil
	}
	for _, limit := range limits {
		used, err := flightUnitsUsed(ctx, tx, period, limit.Scope)
		if err != nil {
			return "", err
		}
		if used+units > limit.Limit {
			return limit.Scope, nil
		}
	}
	for _, limit := range limits {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO flight_lookup_units (period, scope, units) VALUES (?, ?, ?)
			 ON CONFLICT (period, scope) DO UPDATE SET units = units + excluded.units`,
			period, limit.Scope, units); err != nil {
			return "", fmt.Errorf("reserve flight units: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit flight units: %w", err)
	}
	return "", nil
}

// FlightUnitsUsed returns the units the scope spent in the period.
func (s *Store) FlightUnitsUsed(ctx context.Context, period, scope string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("read flight units: SQLite store is not open")
	}
	return flightUnitsUsed(ctx, s.db, period, scope)
}

// MarkFlightPeriodSpent records that the marketplace has no units left for the period.
func (s *Store) MarkFlightPeriodSpent(ctx context.Context, period string, at time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("mark flight period: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO flight_lookup_periods (period, spent_at) VALUES (?, ?) ON CONFLICT (period) DO NOTHING`,
		period, formatTime(at)); err != nil {
		return fmt.Errorf("mark flight period: %w", err)
	}
	return nil
}

// FlightPeriodSpent reports whether the period was marked spent.
func (s *Store) FlightPeriodSpent(ctx context.Context, period string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("read flight period: SQLite store is not open")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM flight_lookup_periods WHERE period = ?`, period).Scan(&count); err != nil {
		return false, fmt.Errorf("read flight period: %w", err)
	}
	return count > 0, nil
}

func flightUnitsUsed(ctx context.Context, db queryRower, period, scope string) (int, error) {
	var used int
	err := db.QueryRowContext(ctx, `SELECT units FROM flight_lookup_units WHERE period = ? AND scope = ?`, period, scope).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read flight units: %w", err)
	}
	return used, nil
}

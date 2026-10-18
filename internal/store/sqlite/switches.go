package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vladar107/home-ai-assistant/internal/integration"
)

var _ integration.SwitchRepository = (*Store)(nil)

func (s *Store) LoadIntegrationSwitch(ctx context.Context, principalID, name string) (bool, bool, error) {
	if s == nil || s.db == nil {
		return false, false, fmt.Errorf("load integration switch: SQLite store is not open")
	}
	var enabled int
	err := s.db.QueryRowContext(
		ctx,
		`SELECT enabled FROM integration_switches WHERE principal_id = ? AND integration = ?`,
		principalID,
		name,
	).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("query integration switch: %w", err)
	}
	return enabled == 1, true, nil
}

func (s *Store) SaveIntegrationSwitch(ctx context.Context, principalID, name string, enabled bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save integration switch: SQLite store is not open")
	}
	value := 0
	if enabled {
		value = 1
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO integration_switches (principal_id, integration, enabled) VALUES (?, ?, ?)
		 ON CONFLICT (principal_id, integration) DO UPDATE
		 SET enabled = excluded.enabled,
		     changed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE enabled != excluded.enabled`,
		principalID,
		name,
		value,
	); err != nil {
		return fmt.Errorf("save integration switch: %w", err)
	}
	return nil
}

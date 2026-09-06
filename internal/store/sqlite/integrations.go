package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/integration"
)

var _ integration.Repository = (*Store)(nil)

func (s *Store) SaveIntegrationConnection(ctx context.Context, record integration.Connection) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save integration connection: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO integration_connections (
		    principal_id, integration, token_kind, workspace_id, workspace_name, bot_name
		) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (principal_id, integration) DO UPDATE
		 SET token_kind = excluded.token_kind,
		     workspace_id = excluded.workspace_id,
		     workspace_name = excluded.workspace_name,
		     bot_name = excluded.bot_name,
		     writes_enabled = 0,
		     connected_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		record.PrincipalID,
		record.Integration,
		record.TokenKind,
		record.WorkspaceID,
		record.WorkspaceName,
		record.BotName,
	); err != nil {
		return fmt.Errorf("save integration connection: %w", err)
	}
	return nil
}

func (s *Store) LoadIntegrationConnection(ctx context.Context, principalID, name string) (integration.Connection, bool, error) {
	if s == nil || s.db == nil {
		return integration.Connection{}, false, fmt.Errorf("load integration connection: SQLite store is not open")
	}
	record := integration.Connection{PrincipalID: principalID, Integration: name}
	var writes int
	var connectedAt string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT token_kind, workspace_id, workspace_name, bot_name, writes_enabled, connected_at
		 FROM integration_connections WHERE principal_id = ? AND integration = ?`,
		principalID,
		name,
	).Scan(&record.TokenKind, &record.WorkspaceID, &record.WorkspaceName, &record.BotName, &writes, &connectedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return integration.Connection{}, false, nil
	}
	if err != nil {
		return integration.Connection{}, false, fmt.Errorf("query integration connection: %w", err)
	}
	record.WritesEnabled = writes == 1
	record.ConnectedAt, err = time.Parse(time.RFC3339, connectedAt)
	if err != nil {
		return integration.Connection{}, false, fmt.Errorf("parse integration connection timestamp: %w", err)
	}
	return record, true, nil
}

func (s *Store) DeleteIntegrationConnection(ctx context.Context, principalID, name string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete integration connection: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`DELETE FROM integration_connections WHERE principal_id = ? AND integration = ?`,
		principalID,
		name,
	)
	if err != nil {
		return false, fmt.Errorf("delete integration connection: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read deleted integration connection count: %w", err)
	}
	return deleted > 0, nil
}

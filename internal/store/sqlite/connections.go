package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connection"
)

var _ connection.Repository = (*Store)(nil)

func (s *Store) BindModelConnection(ctx context.Context, principalID, provider, accountID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("bind model connection: SQLite store is not open")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin model connection bind: %w", err)
	}
	defer tx.Rollback()

	var owner string
	err = tx.QueryRowContext(
		ctx,
		`SELECT principal_id FROM model_connections WHERE provider = ? AND account_id = ?`,
		provider,
		accountID,
	).Scan(&owner)
	switch {
	case err == nil && owner != principalID:
		return false, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("query model connection owner: %w", err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO model_connections (principal_id, provider, account_id) VALUES (?, ?, ?)
		 ON CONFLICT (principal_id) DO UPDATE
		 SET provider = excluded.provider,
		     account_id = excluded.account_id,
		     connected_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		principalID,
		provider,
		accountID,
	); err != nil {
		return false, fmt.Errorf("bind model connection: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit model connection bind: %w", err)
	}
	return true, nil
}

func (s *Store) LoadModelConnection(ctx context.Context, principalID string) (connection.Connection, bool, error) {
	if s == nil || s.db == nil {
		return connection.Connection{}, false, fmt.Errorf("load model connection: SQLite store is not open")
	}

	record := connection.Connection{PrincipalID: principalID}
	var connectedAt string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT provider, account_id, connected_at FROM model_connections WHERE principal_id = ?`,
		principalID,
	).Scan(&record.Provider, &record.AccountID, &connectedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return connection.Connection{}, false, nil
	}
	if err != nil {
		return connection.Connection{}, false, fmt.Errorf("query model connection: %w", err)
	}
	record.ConnectedAt, err = time.Parse(time.RFC3339, connectedAt)
	if err != nil {
		return connection.Connection{}, false, fmt.Errorf("parse model connection timestamp: %w", err)
	}
	return record, true, nil
}

func (s *Store) DeleteModelConnection(ctx context.Context, principalID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete model connection: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM model_connections WHERE principal_id = ?`, principalID)
	if err != nil {
		return false, fmt.Errorf("delete model connection: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read deleted model connection count: %w", err)
	}
	return deleted > 0, nil
}

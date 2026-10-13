package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/backlog"
)

var _ backlog.Repository = (*Store)(nil)

const backlogColumns = `id, principal_id, title, details, status, created_at, updated_at`

func (s *Store) ListBacklog(ctx context.Context, principalID, status string) ([]backlog.Item, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list backlog: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+backlogColumns+` FROM backlog_items WHERE principal_id = ? AND status = ?
		 ORDER BY created_at, rowid`, principalID, status)
	if err != nil {
		return nil, fmt.Errorf("query backlog: %w", err)
	}
	defer rows.Close()
	var items []backlog.Item
	for rows.Next() {
		item, err := scanBacklogItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read backlog: %w", err)
	}
	return items, nil
}

func (s *Store) LoadBacklogItem(ctx context.Context, principalID, id string) (backlog.Item, bool, error) {
	if s == nil || s.db == nil {
		return backlog.Item{}, false, fmt.Errorf("load backlog item: SQLite store is not open")
	}
	item, err := scanBacklogItem(s.db.QueryRowContext(ctx,
		`SELECT `+backlogColumns+` FROM backlog_items WHERE principal_id = ? AND id = ?`, principalID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return backlog.Item{}, false, nil
	}
	if err != nil {
		return backlog.Item{}, false, err
	}
	return item, true, nil
}

func (s *Store) AddBacklogItem(ctx context.Context, item backlog.Item) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("add backlog item: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO backlog_items (`+backlogColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.PrincipalID, item.Title, item.Details, item.Status, formatTime(item.CreatedAt), formatTime(item.UpdatedAt),
	); err != nil {
		return fmt.Errorf("add backlog item: %w", err)
	}
	return nil
}

func (s *Store) UpdateBacklogItem(ctx context.Context, item backlog.Item) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("update backlog item: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE backlog_items SET title = ?, details = ?, status = ?, updated_at = ? WHERE principal_id = ? AND id = ?`,
		item.Title, item.Details, item.Status, formatTime(item.UpdatedAt), item.PrincipalID, item.ID)
	if err != nil {
		return false, fmt.Errorf("update backlog item: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read updated backlog count: %w", err)
	}
	return changed == 1, nil
}

func (s *Store) RemoveBacklogItem(ctx context.Context, principalID, id string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("remove backlog item: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM backlog_items WHERE principal_id = ? AND id = ?`, principalID, id)
	if err != nil {
		return false, fmt.Errorf("remove backlog item: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read removed backlog count: %w", err)
	}
	return removed == 1, nil
}

func scanBacklogItem(row rowScanner) (backlog.Item, error) {
	var item backlog.Item
	var createdAt, updatedAt string
	if err := row.Scan(&item.ID, &item.PrincipalID, &item.Title, &item.Details, &item.Status, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return backlog.Item{}, err
		}
		return backlog.Item{}, fmt.Errorf("scan backlog item: %w", err)
	}
	var err error
	if item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return backlog.Item{}, fmt.Errorf("parse backlog time: %w", err)
	}
	if item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return backlog.Item{}, fmt.Errorf("parse backlog time: %w", err)
	}
	return item, nil
}

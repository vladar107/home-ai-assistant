package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/memory"
)

var _ memory.Repository = (*Store)(nil)

const memoryColumns = `id, principal_id, kind, text, source, created_at, updated_at`

func (s *Store) ListMemory(ctx context.Context, principalID string) ([]memory.Entry, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list memory: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+memoryColumns+` FROM memory_entries WHERE principal_id = ?
		 ORDER BY CASE kind WHEN 'instruction' THEN 0 ELSE 1 END, created_at, rowid`, principalID)
	if err != nil {
		return nil, fmt.Errorf("query memory: %w", err)
	}
	defer rows.Close()
	var entries []memory.Entry
	for rows.Next() {
		entry, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read memory: %w", err)
	}
	return entries, nil
}

func (s *Store) LoadMemory(ctx context.Context, principalID, id string) (memory.Entry, bool, error) {
	if s == nil || s.db == nil {
		return memory.Entry{}, false, fmt.Errorf("load memory: SQLite store is not open")
	}
	entry, err := scanMemory(s.db.QueryRowContext(ctx,
		`SELECT `+memoryColumns+` FROM memory_entries WHERE principal_id = ? AND id = ?`, principalID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Entry{}, false, nil
	}
	if err != nil {
		return memory.Entry{}, false, err
	}
	return entry, true, nil
}

func (s *Store) AddMemory(ctx context.Context, entry memory.Entry) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("add memory: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_entries (`+memoryColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.PrincipalID, entry.Kind, entry.Text, entry.Source, formatTime(entry.CreatedAt), formatTime(entry.UpdatedAt),
	); err != nil {
		return fmt.Errorf("add memory: %w", err)
	}
	return nil
}

func (s *Store) ReplaceMemory(ctx context.Context, principalID, id, text, source string, at time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("replace memory: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE memory_entries SET text = ?, source = ?, updated_at = ? WHERE principal_id = ? AND id = ?`,
		text, source, formatTime(at), principalID, id)
	if err != nil {
		return false, fmt.Errorf("replace memory: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read replaced memory count: %w", err)
	}
	return changed == 1, nil
}

func (s *Store) RemoveMemory(ctx context.Context, principalID, id string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("remove memory: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM memory_entries WHERE principal_id = ? AND id = ?`, principalID, id)
	if err != nil {
		return false, fmt.Errorf("remove memory: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read removed memory count: %w", err)
	}
	return removed == 1, nil
}

func (s *Store) ClearMemory(ctx context.Context, principalID string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("clear memory: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM memory_entries WHERE principal_id = ?`, principalID)
	if err != nil {
		return 0, fmt.Errorf("clear memory: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read cleared memory count: %w", err)
	}
	return int(removed), nil
}

func (s *Store) MemoryEnabled(ctx context.Context, principalID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("load memory switch: SQLite store is not open")
	}
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM memory_settings WHERE principal_id = ?`, principalID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("query memory switch: %w", err)
	}
	return enabled == 1, nil
}

func (s *Store) SetMemoryEnabled(ctx context.Context, principalID string, enabled bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("set memory switch: SQLite store is not open")
	}
	value := 0
	if enabled {
		value = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_settings (principal_id, enabled) VALUES (?, ?)
		 ON CONFLICT (principal_id) DO UPDATE SET enabled = excluded.enabled`, principalID, value); err != nil {
		return fmt.Errorf("set memory switch: %w", err)
	}
	return nil
}

// MarkConversationExposed flags the session the chat request belongs to as having read
// third-party content, and as externally exposed when that content came from outside the
// household. Neither flag is ever cleared; /new starts a fresh session.
func (s *Store) MarkConversationExposed(ctx context.Context, principalID, requestID string, external bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("mark conversation exposed: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conversation_sessions
		 SET exposed_at = coalesce(exposed_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
		     external_at = CASE WHEN ? THEN coalesce(external_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')) ELSE external_at END
		 WHERE (exposed_at IS NULL OR (? AND external_at IS NULL)) AND principal_id = ? AND id = (
		     SELECT session_id FROM conversation_requests WHERE id = ? AND principal_id = ?
		 )`, external, external, principalID, requestID, principalID); err != nil {
		return fmt.Errorf("mark conversation exposed: %w", err)
	}
	return nil
}

// ConversationExternal reports whether the session the chat request belongs to has read text
// written outside the household.
func (s *Store) ConversationExternal(ctx context.Context, principalID, requestID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("load conversation external exposure: SQLite store is not open")
	}
	var external int
	err := s.db.QueryRowContext(ctx,
		`SELECT session.external_at IS NOT NULL
		 FROM conversation_requests AS request
		 JOIN conversation_sessions AS session
		   ON session.id = request.session_id AND session.principal_id = request.principal_id
		 WHERE request.id = ? AND request.principal_id = ?`, requestID, principalID).Scan(&external)
	if err != nil {
		return false, fmt.Errorf("query conversation external exposure: %w", err)
	}
	return external == 1, nil
}

func (s *Store) ConversationExposed(ctx context.Context, principalID, requestID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("load conversation exposure: SQLite store is not open")
	}
	var exposed int
	err := s.db.QueryRowContext(ctx,
		`SELECT session.exposed_at IS NOT NULL
		 FROM conversation_requests AS request
		 JOIN conversation_sessions AS session
		   ON session.id = request.session_id AND session.principal_id = request.principal_id
		 WHERE request.id = ? AND request.principal_id = ?`, requestID, principalID).Scan(&exposed)
	if err != nil {
		return false, fmt.Errorf("query conversation exposure: %w", err)
	}
	return exposed == 1, nil
}

func scanMemory(row rowScanner) (memory.Entry, error) {
	var entry memory.Entry
	var createdAt, updatedAt string
	if err := row.Scan(&entry.ID, &entry.PrincipalID, &entry.Kind, &entry.Text, &entry.Source, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return memory.Entry{}, err
		}
		return memory.Entry{}, fmt.Errorf("scan memory: %w", err)
	}
	var err error
	if entry.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return memory.Entry{}, fmt.Errorf("parse memory time: %w", err)
	}
	if entry.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return memory.Entry{}, fmt.Errorf("parse memory time: %w", err)
	}
	return entry, nil
}

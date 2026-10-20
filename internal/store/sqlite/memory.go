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

// MergeMemory gives the principal's entry keep the text and source and removes the entries
// removed, all or none; false means one of them is gone.
func (s *Store) MergeMemory(ctx context.Context, principalID, keep, text, source string, removed []string, at time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("merge memory: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin merging memory: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE memory_entries SET text = ?, source = ?, updated_at = ? WHERE principal_id = ? AND id = ?`,
		text, source, formatTime(at), principalID, keep)
	if err != nil {
		return false, fmt.Errorf("merge memory: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return false, err
	}
	for _, id := range removed {
		result, err := tx.ExecContext(ctx, `DELETE FROM memory_entries WHERE principal_id = ? AND id = ?`, principalID, id)
		if err != nil {
			return false, fmt.Errorf("merge memory: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit merging memory: %w", err)
	}
	return true, nil
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

// ClearMemory deletes the principal's entries and procedures together and returns how many there
// were.
func (s *Store) ClearMemory(ctx context.Context, principalID string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("clear memory: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin clearing memory: %w", err)
	}
	defer tx.Rollback()
	total := 0
	for _, query := range []string{`DELETE FROM memory_entries WHERE principal_id = ?`, `DELETE FROM memory_procedures WHERE principal_id = ?`} {
		result, err := tx.ExecContext(ctx, query, principalID)
		if err != nil {
			return 0, fmt.Errorf("clear memory: %w", err)
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("read cleared memory count: %w", err)
		}
		total += int(removed)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit clearing memory: %w", err)
	}
	return total, nil
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

// LearnFromFeedback is true for a principal who never changed the switch.
func (s *Store) LearnFromFeedback(ctx context.Context, principalID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("load learning switch: SQLite store is not open")
	}
	var learn int
	err := s.db.QueryRowContext(ctx, `SELECT learn_from_feedback FROM memory_settings WHERE principal_id = ?`, principalID).Scan(&learn)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("query learning switch: %w", err)
	}
	return learn == 1, nil
}

// SetLearnFromFeedback keeps the memory switch as it is; a principal without a row has memory on.
func (s *Store) SetLearnFromFeedback(ctx context.Context, principalID string, learn bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("set learning switch: SQLite store is not open")
	}
	value := 0
	if learn {
		value = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_settings (principal_id, enabled, learn_from_feedback) VALUES (?, 1, ?)
		 ON CONFLICT (principal_id) DO UPDATE SET learn_from_feedback = excluded.learn_from_feedback`, principalID, value); err != nil {
		return fmt.Errorf("set learning switch: %w", err)
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

// MarkConversationPrivate flags the session the chat request belongs to as having read Notion
// page content or NAS file contents. The flag is set once and kept until /new starts a session.
func (s *Store) MarkConversationPrivate(ctx context.Context, principalID, requestID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("mark conversation private: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conversation_sessions
		 SET private_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE private_at IS NULL AND principal_id = ? AND id = (
		     SELECT session_id FROM conversation_requests WHERE id = ? AND principal_id = ?
		 )`, principalID, requestID, principalID); err != nil {
		return fmt.Errorf("mark conversation private: %w", err)
	}
	return nil
}

// ConversationPrivate reports whether the session the chat request belongs to has read Notion
// page content or NAS file contents.
func (s *Store) ConversationPrivate(ctx context.Context, principalID, requestID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("load conversation privacy: SQLite store is not open")
	}
	var private int
	err := s.db.QueryRowContext(ctx,
		`SELECT session.private_at IS NOT NULL
		 FROM conversation_requests AS request
		 JOIN conversation_sessions AS session
		   ON session.id = request.session_id AND session.principal_id = request.principal_id
		 WHERE request.id = ? AND request.principal_id = ?`, requestID, principalID).Scan(&private)
	if err != nil {
		return false, fmt.Errorf("query conversation privacy: %w", err)
	}
	return private == 1, nil
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

const procedureColumns = `id, principal_id, title, steps, source, created_at, updated_at, read_at`

func (s *Store) ListProcedures(ctx context.Context, principalID string) ([]memory.Procedure, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list procedures: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+procedureColumns+` FROM memory_procedures WHERE principal_id = ? ORDER BY created_at, rowid`, principalID)
	if err != nil {
		return nil, fmt.Errorf("query procedures: %w", err)
	}
	defer rows.Close()
	var procedures []memory.Procedure
	for rows.Next() {
		procedure, err := scanProcedure(rows)
		if err != nil {
			return nil, err
		}
		procedures = append(procedures, procedure)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read procedures: %w", err)
	}
	return procedures, nil
}

func (s *Store) AddProcedure(ctx context.Context, procedure memory.Procedure) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("add procedure: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_procedures (id, principal_id, title, steps, source, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		procedure.ID, procedure.PrincipalID, procedure.Title, procedure.Steps, procedure.Source, formatTime(procedure.CreatedAt), formatTime(procedure.UpdatedAt),
	); err != nil {
		return fmt.Errorf("add procedure: %w", err)
	}
	return nil
}

func (s *Store) ReplaceProcedure(ctx context.Context, principalID, id, title, steps, source string, at time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("replace procedure: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE memory_procedures SET title = ?, steps = ?, source = ?, updated_at = ? WHERE principal_id = ? AND id = ?`,
		title, steps, source, formatTime(at), principalID, id)
	if err != nil {
		return false, fmt.Errorf("replace procedure: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read replaced procedure count: %w", err)
	}
	return changed == 1, nil
}

// ReadProcedure returns the procedure as it is now and stamps when it was read.
func (s *Store) ReadProcedure(ctx context.Context, principalID, id string, at time.Time) (memory.Procedure, bool, error) {
	if s == nil || s.db == nil {
		return memory.Procedure{}, false, fmt.Errorf("read procedure: SQLite store is not open")
	}
	procedure, err := scanProcedure(s.db.QueryRowContext(ctx,
		`UPDATE memory_procedures SET read_at = ? WHERE principal_id = ? AND id = ? RETURNING `+procedureColumns,
		formatTime(at), principalID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Procedure{}, false, nil
	}
	if err != nil {
		return memory.Procedure{}, false, err
	}
	return procedure, true, nil
}

func (s *Store) RemoveProcedure(ctx context.Context, principalID, id string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("remove procedure: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM memory_procedures WHERE principal_id = ? AND id = ?`, principalID, id)
	if err != nil {
		return false, fmt.Errorf("remove procedure: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read removed procedure count: %w", err)
	}
	return removed == 1, nil
}

// LearnProcedures is true for a principal who never changed the switch.
func (s *Store) LearnProcedures(ctx context.Context, principalID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("load procedure learning switch: SQLite store is not open")
	}
	var learn int
	err := s.db.QueryRowContext(ctx, `SELECT learn_procedures FROM memory_settings WHERE principal_id = ?`, principalID).Scan(&learn)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("query procedure learning switch: %w", err)
	}
	return learn == 1, nil
}

// SetLearnProcedures keeps the memory switch as it is; a principal without a row has memory on.
func (s *Store) SetLearnProcedures(ctx context.Context, principalID string, learn bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("set procedure learning switch: SQLite store is not open")
	}
	value := 0
	if learn {
		value = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_settings (principal_id, enabled, learn_procedures) VALUES (?, 1, ?)
		 ON CONFLICT (principal_id) DO UPDATE SET learn_procedures = excluded.learn_procedures`, principalID, value); err != nil {
		return fmt.Errorf("set procedure learning switch: %w", err)
	}
	return nil
}

func scanProcedure(row rowScanner) (memory.Procedure, error) {
	var procedure memory.Procedure
	var createdAt, updatedAt string
	var readAt sql.NullString
	if err := row.Scan(&procedure.ID, &procedure.PrincipalID, &procedure.Title, &procedure.Steps, &procedure.Source, &createdAt, &updatedAt, &readAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return memory.Procedure{}, err
		}
		return memory.Procedure{}, fmt.Errorf("scan procedure: %w", err)
	}
	var err error
	if procedure.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return memory.Procedure{}, fmt.Errorf("parse procedure time: %w", err)
	}
	if procedure.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return memory.Procedure{}, fmt.Errorf("parse procedure time: %w", err)
	}
	if readAt.Valid {
		if procedure.ReadAt, err = time.Parse(time.RFC3339Nano, readAt.String); err != nil {
			return memory.Procedure{}, fmt.Errorf("parse procedure time: %w", err)
		}
	}
	return procedure, nil
}

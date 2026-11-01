package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/intake"
)

var _ intake.Repository = (*Store)(nil)

const intakeColumns = `id, principal_id, source, source_ref, kind, title, sender, facts, action_required, pending_actions, created_at, updated_at`

func (s *Store) CreateIntakeRecord(ctx context.Context, record intake.Record) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("create intake record: SQLite store is not open")
	}
	facts, pending, err := encodeIntake(record)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO intake_records (`+intakeColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (principal_id, source, source_ref) DO NOTHING`,
		record.ID, record.PrincipalID, record.Source, record.SourceRef, record.Kind, record.Title, record.Sender, facts,
		record.ActionRequired, pending, formatTime(record.CreatedAt), formatTime(record.UpdatedAt))
	if err != nil {
		return false, fmt.Errorf("insert intake record: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert intake record: %w", err)
	}
	return inserted == 1, nil
}

func encodeIntake(record intake.Record) (string, string, error) {
	facts, err := json.Marshal(record.Facts)
	if err != nil {
		return "", "", fmt.Errorf("encode intake facts: %w", err)
	}
	pending, err := encodeActions(record.Pending)
	if err != nil {
		return "", "", err
	}
	return string(facts), pending, nil
}

func encodeActions(actions []intake.Action) (string, error) {
	if actions == nil {
		actions = []intake.Action{}
	}
	encoded, err := json.Marshal(actions)
	if err != nil {
		return "", fmt.Errorf("encode intake actions: %w", err)
	}
	return string(encoded), nil
}

func (s *Store) LoadIntakeRecord(ctx context.Context, principalID, id string) (intake.Record, bool, error) {
	if s == nil || s.db == nil {
		return intake.Record{}, false, fmt.Errorf("load intake record: SQLite store is not open")
	}
	return scanIntakeRecord(s.db.QueryRowContext(ctx, `SELECT `+intakeColumns+` FROM intake_records WHERE principal_id = ? AND id = ?`, principalID, id))
}

func (s *Store) DeleteIntakeRecord(ctx context.Context, principalID, id string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete intake record: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM intake_records WHERE principal_id = ? AND id = ?`, principalID, id)
	if err != nil {
		return false, fmt.Errorf("delete intake record: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete intake record: %w", err)
	}
	return deleted == 1, nil
}

func (s *Store) PendingIntakeRecords(ctx context.Context, principalID string) ([]intake.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list pending intake records: SQLite store is not open")
	}
	return queryIntakeRecords(ctx, s.db, `SELECT `+intakeColumns+` FROM intake_records
		 WHERE principal_id = ? AND json_array_length(pending_actions) > 0 ORDER BY created_at, rowid`, principalID)
}

// SetIntakePendingActions leaves updated_at alone: it times the record's own fields, which a
// confirmed change to the record checks.
func (s *Store) SetIntakePendingActions(ctx context.Context, principalID, id string, actions []intake.Action) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("set intake actions: SQLite store is not open")
	}
	pending, err := encodeActions(actions)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE intake_records SET pending_actions = ? WHERE principal_id = ? AND id = ?`,
		pending, principalID, id); err != nil {
		return fmt.Errorf("set intake actions: %w", err)
	}
	return nil
}

func (s *Store) LoadIntakeRecordBySource(ctx context.Context, principalID, source, sourceRef string) (intake.Record, bool, error) {
	if s == nil || s.db == nil {
		return intake.Record{}, false, fmt.Errorf("load intake record: SQLite store is not open")
	}
	return scanIntakeRecord(s.db.QueryRowContext(ctx, `SELECT `+intakeColumns+` FROM intake_records WHERE principal_id = ? AND source = ? AND source_ref = ?`,
		principalID, source, sourceRef))
}

func (s *Store) UpdateIntakeRecord(ctx context.Context, record intake.Record, expectedUpdatedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("update intake record: SQLite store is not open")
	}
	facts, err := json.Marshal(record.Facts)
	if err != nil {
		return false, fmt.Errorf("encode intake facts: %w", err)
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE intake_records SET kind = ?, title = ?, sender = ?, facts = ?, action_required = ?, updated_at = ?
		 WHERE principal_id = ? AND id = ? AND updated_at = ?`,
		record.Kind, record.Title, record.Sender, string(facts), record.ActionRequired, formatTime(record.UpdatedAt),
		record.PrincipalID, record.ID, formatTime(expectedUpdatedAt))
	if err != nil {
		return false, fmt.Errorf("update intake record: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update intake record: %w", err)
	}
	return updated == 1, nil
}

// ListIntakeRecords narrows by kind in SQL, letting through every kind outside ASCII, whose case
// SQLite's lower() does not fold, and then compares with full case folding.
func (s *Store) ListIntakeRecords(ctx context.Context, principalID, kind string, limit int) ([]intake.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list intake records: SQLite store is not open")
	}
	if kind == "" {
		return queryIntakeRecords(ctx, s.db, `SELECT `+intakeColumns+` FROM intake_records WHERE principal_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`,
			principalID, limit)
	}
	records, err := queryIntakeRecords(ctx, s.db, `SELECT `+intakeColumns+` FROM intake_records
		 WHERE principal_id = ? AND (lower(kind) = lower(?) OR kind GLOB '*[^ -~]*' OR ? GLOB '*[^ -~]*')
		 ORDER BY created_at DESC, rowid DESC`, principalID, kind, kind)
	if err != nil {
		return nil, err
	}
	var matching []intake.Record
	for _, record := range records {
		if strings.EqualFold(record.Kind, kind) && len(matching) < limit {
			matching = append(matching, record)
		}
	}
	return matching, nil
}

func (s *Store) InboxReadUntil(ctx context.Context, principalID string) (time.Time, int, bool, error) {
	if s == nil || s.db == nil {
		return time.Time{}, 0, false, fmt.Errorf("load inbox check: SQLite store is not open")
	}
	var until string
	var stalls int
	err := s.db.QueryRowContext(ctx, `SELECT read_until, stalls FROM intake_scans WHERE principal_id = ?`, principalID).Scan(&until, &stalls)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, 0, false, nil
	}
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("load inbox check: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, until)
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("parse inbox check time: %w", err)
	}
	return parsed, stalls, true, nil
}

func (s *Store) RecordInboxReadUntil(ctx context.Context, principalID string, until time.Time, stalls int) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record inbox check: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO intake_scans (principal_id, read_until, stalls) VALUES (?, ?, ?)
		 ON CONFLICT (principal_id) DO UPDATE SET read_until = excluded.read_until, stalls = excluded.stalls`,
		principalID, formatTime(until), stalls); err != nil {
		return fmt.Errorf("record inbox check: %w", err)
	}
	return nil
}

func (s *Store) ResetInbox(ctx context.Context, principalID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("reset inbox check: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin inbox reset: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM intake_scans WHERE principal_id = ?`, principalID); err != nil {
		return fmt.Errorf("reset inbox check: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE intake_records SET pending_actions = '[]' WHERE principal_id = ? AND pending_actions != '[]'`, principalID); err != nil {
		return fmt.Errorf("reset inbox actions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit inbox reset: %w", err)
	}
	return nil
}

func queryIntakeRecords(ctx context.Context, db queryer, query string, arguments ...any) ([]intake.Record, error) {
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query intake records: %w", err)
	}
	defer rows.Close()
	var records []intake.Record
	for rows.Next() {
		record, err := scanIntakeRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query intake records: %w", err)
	}
	return records, nil
}

func scanIntakeRecord(row *sql.Row) (intake.Record, bool, error) {
	record, err := scanIntakeRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return intake.Record{}, false, nil
	}
	if err != nil {
		return intake.Record{}, false, err
	}
	return record, true, nil
}

func scanIntakeRow(row rowScanner) (intake.Record, error) {
	var record intake.Record
	var facts, pending, createdAt, updatedAt string
	if err := row.Scan(&record.ID, &record.PrincipalID, &record.Source, &record.SourceRef, &record.Kind, &record.Title, &record.Sender,
		&facts, &record.ActionRequired, &pending, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return intake.Record{}, err
		}
		return intake.Record{}, fmt.Errorf("scan intake record: %w", err)
	}
	if err := json.Unmarshal([]byte(facts), &record.Facts); err != nil {
		return intake.Record{}, fmt.Errorf("decode intake facts: %w", err)
	}
	if err := json.Unmarshal([]byte(pending), &record.Pending); err != nil {
		return intake.Record{}, fmt.Errorf("decode intake actions: %w", err)
	}
	var err error
	if record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return intake.Record{}, fmt.Errorf("parse intake record time: %w", err)
	}
	if record.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return intake.Record{}, fmt.Errorf("parse intake record time: %w", err)
	}
	return record, nil
}

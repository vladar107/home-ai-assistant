package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

var _ approval.Repository = (*Store)(nil)

const approvalColumns = `id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status,
	COALESCE(reason, ''), COALESCE(outcome, ''), COALESCE(chat_id, 0), COALESCE(message_id, 0),
	created_at, expires_at, COALESCE(started_at, ''), COALESCE(finished_at, ''), mode, COALESCE(request_id, ''),
	COALESCE(undo, ''), COALESCE(undo_until, ''), thread_id, COALESCE(heading, '')`

func (s *Store) CreateApproval(ctx context.Context, record approval.Record) ([]approval.Record, error) {
	return s.CreateApprovals(ctx, []approval.Record{record})
}

func (s *Store) CreateApprovals(ctx context.Context, records []approval.Record) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("create approval: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin approval creation: %w", err)
	}
	defer tx.Rollback()

	var replaced []approval.Record
	for _, record := range records {
		if err := createApproval(ctx, tx, record, &replaced); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit approval creation: %w", err)
	}
	return replaced, nil
}

// createApproval stores one pending record in the transaction. A record that starts its bundle
// first expires the tab's pending records, which it adds to replaced.
func createApproval(ctx context.Context, tx *sql.Tx, record approval.Record, replaced *[]approval.Record) error {
	if record.BundleID == record.ID {
		pending, err := queryApprovals(ctx, tx, `SELECT `+approvalColumns+` FROM approvals WHERE principal_id = ? AND thread_id = ? AND status = 'pending' AND mode = 'proposed' ORDER BY bundle_id, position`, record.PrincipalID, record.ThreadID)
		if err != nil {
			return err
		}
		for index := range pending {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE approvals SET status = 'expired', reason = ?, finished_at = ? WHERE id = ? AND status = 'pending'`,
				approval.ReasonReplaced,
				formatTime(record.CreatedAt),
				pending[index].ID,
			); err != nil {
				return fmt.Errorf("replace pending approval: %w", err)
			}
			pending[index].Status, pending[index].Reason, pending[index].FinishedAt = approval.StatusExpired, approval.ReasonReplaced, record.CreatedAt
		}
		*replaced = append(*replaced, pending...)
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, created_at, expires_at, thread_id, heading)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
		record.ID,
		record.BundleID,
		record.Position,
		record.PrincipalID,
		record.Tool,
		string(record.Arguments),
		record.Hash,
		record.Summary,
		formatTime(record.CreatedAt),
		formatTime(record.ExpiresAt),
		record.ThreadID,
		sql.NullString{String: record.Heading, Valid: record.Heading != ""},
	); err != nil {
		return fmt.Errorf("insert approval: %w", err)
	}
	return nil
}

func (s *Store) LoadApproval(ctx context.Context, id string) (approval.Record, bool, error) {
	if s == nil || s.db == nil {
		return approval.Record{}, false, fmt.Errorf("load approval: SQLite store is not open")
	}
	return scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id))
}

// ResolvedApprovals returns the principal's proposals of the tools made at or after since that the
// user confirmed or rejected or that expired, oldest first.
func (s *Store) ResolvedApprovals(ctx context.Context, principalID string, tools []string, since time.Time) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list resolved approvals: SQLite store is not open")
	}
	if len(tools) == 0 {
		return nil, nil
	}
	arguments := []any{principalID, formatTime(since)}
	for _, tool := range tools {
		arguments = append(arguments, tool)
	}
	return queryApprovals(ctx, s.db, `SELECT `+approvalColumns+` FROM approvals
		 WHERE principal_id = ? AND created_at >= ? AND mode = 'proposed' AND status NOT IN ('pending', 'confirmed')
		   AND tool IN (?`+strings.Repeat(", ?", len(tools)-1)+`)
		 ORDER BY created_at, rowid`, arguments...)
}

func (s *Store) LoadBundle(ctx context.Context, bundleID string) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("load approval bundle: SQLite store is not open")
	}
	return queryApprovals(ctx, s.db, `SELECT `+approvalColumns+` FROM approvals WHERE bundle_id = ? ORDER BY position`, bundleID)
}

func (s *Store) PendingApprovals(ctx context.Context, principalID string, threadID int64) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("load pending approvals: SQLite store is not open")
	}
	return queryApprovals(ctx, s.db, `SELECT `+approvalColumns+` FROM approvals WHERE principal_id = ? AND thread_id = ? AND status = 'pending' AND mode = 'proposed' ORDER BY bundle_id, position`, principalID, threadID)
}

func (s *Store) ConfirmBundle(ctx context.Context, bundleID, principalID string, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("confirm approval bundle: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE approvals SET status = 'confirmed'
		 WHERE bundle_id = ? AND principal_id = ? AND status = 'pending' AND expires_at > ?`,
		bundleID,
		principalID,
		formatTime(now),
	)
	if err != nil {
		return false, fmt.Errorf("confirm approval bundle: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read confirmed approval count: %w", err)
	}
	return changed > 0, nil
}

func (s *Store) StartApproval(ctx context.Context, id string, now time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("start approval: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE approvals SET started_at = ? WHERE id = ? AND status = 'confirmed'`, formatTime(now), id)
	if err != nil {
		return fmt.Errorf("start approval: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read started approval count: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("start approval: record is not confirmed")
	}
	return nil
}

func (s *Store) FinishApproval(ctx context.Context, id, from, status, reason, outcome string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("finish approval: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE approvals
		 SET status = ?, reason = NULLIF(?, ''), outcome = NULLIF(?, ''), finished_at = ?
		 WHERE id = ? AND status = ?`,
		status,
		reason,
		outcome,
		formatTime(time.Now()),
		id,
		from,
	)
	if err != nil {
		return false, fmt.Errorf("finish approval: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read finished approval count: %w", err)
	}
	return changed == 1, nil
}

func (s *Store) SetApprovalSummary(ctx context.Context, id, summary string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update approval summary: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET summary = ? WHERE id = ? AND (status = 'pending' OR (mode = 'direct' AND status = 'done'))`, summary, id); err != nil {
		return fmt.Errorf("update approval summary: %w", err)
	}
	return nil
}

func (s *Store) SetApprovalMessage(ctx context.Context, ids []string, chatID, messageID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record approval message: SQLite store is not open")
	}
	if len(ids) == 0 {
		return nil
	}
	arguments := []any{chatID, messageID}
	for _, id := range ids {
		arguments = append(arguments, id)
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE approvals SET chat_id = ?, message_id = NULLIF(?, 0) WHERE id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`,
		arguments...,
	); err != nil {
		return fmt.Errorf("record approval message: %w", err)
	}
	return nil
}

func (s *Store) ExpireApprovals(ctx context.Context, now time.Time, all bool) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("expire approvals: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin approval expiry: %w", err)
	}
	defer tx.Rollback()

	query := `SELECT ` + approvalColumns + ` FROM approvals WHERE status = 'pending' AND expires_at <= ?`
	arguments := []any{formatTime(now)}
	if all {
		query = `SELECT ` + approvalColumns + ` FROM approvals WHERE status IN ('pending', 'confirmed') OR (mode = 'direct' AND status = 'done' AND reason = ?)`
		arguments = []any{approval.ReasonUndoing}
	}
	expiring, err := queryApprovals(ctx, tx, query+` ORDER BY bundle_id, position`, arguments...)
	if err != nil {
		return nil, err
	}

	finished := formatTime(now)
	for index := range expiring {
		record := &expiring[index]
		if record.Status == approval.StatusDone {
			// The action itself still stands and keeps its finish time; only the interrupted Undo is
			// marked, and the record can be undone again until its deadline.
			record.Reason = approval.ReasonUndoInterrupted
			if _, err := tx.ExecContext(ctx, `UPDATE approvals SET reason = ? WHERE id = ?`, record.Reason, record.ID); err != nil {
				return nil, fmt.Errorf("release interrupted undo: %w", err)
			}
			continue
		}
		if record.Status == approval.StatusConfirmed && !record.StartedAt.IsZero() {
			record.Status, record.Reason = approval.StatusFailed, approval.ReasonInterrupted
		} else if all {
			record.Status, record.Reason = approval.StatusExpired, approval.ReasonRestart
		} else {
			record.Status, record.Reason = approval.StatusExpired, approval.ReasonTimeout
		}
		record.FinishedAt = now
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE approvals SET status = ?, reason = ?, finished_at = ? WHERE id = ?`,
			record.Status,
			record.Reason,
			finished,
			record.ID,
		); err != nil {
			return nil, fmt.Errorf("expire approval: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit approval expiry: %w", err)
	}
	return expiring, nil
}

func (s *Store) UndeliveredApprovals(ctx context.Context) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list undelivered approvals: SQLite store is not open")
	}
	return queryApprovals(
		ctx,
		s.db,
		`SELECT `+approvalColumns+` FROM approvals WHERE status = 'pending' AND mode = 'proposed' AND chat_id IS NOT NULL AND message_id IS NULL ORDER BY bundle_id, position`,
	)
}

func (s *Store) CreateDirectApproval(ctx context.Context, record approval.Record) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("create direct approval: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, created_at, expires_at, started_at, mode, request_id, thread_id)
		 VALUES (?, ?, 0, ?, ?, ?, ?, ?, 'confirmed', ?, ?, ?, 'direct', ?, ?)`,
		record.ID,
		record.ID,
		record.PrincipalID,
		record.Tool,
		string(record.Arguments),
		record.Hash,
		record.Summary,
		formatTime(record.CreatedAt),
		formatTime(record.ExpiresAt),
		formatTime(record.CreatedAt),
		record.RequestID,
		record.ThreadID,
	); err != nil {
		return fmt.Errorf("insert direct approval: %w", err)
	}
	return nil
}

func (s *Store) FinishDirectApproval(ctx context.Context, id, status, reason, outcome string, undo json.RawMessage, undoUntil time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("finish direct approval: SQLite store is not open")
	}
	var storedUndo, storedUntil any
	if len(undo) > 0 && status == approval.StatusDone {
		storedUndo, storedUntil = string(undo), formatTime(undoUntil)
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE approvals
		 SET status = ?, reason = NULLIF(?, ''), outcome = NULLIF(?, ''), finished_at = ?, undo = ?, undo_until = ?
		 WHERE id = ? AND mode = 'direct' AND status = 'confirmed'`,
		status,
		reason,
		outcome,
		formatTime(time.Now()),
		storedUndo,
		storedUntil,
		id,
	)
	if err != nil {
		return false, fmt.Errorf("finish direct approval: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read finished direct approval count: %w", err)
	}
	return changed == 1, nil
}

func (s *Store) DirectApprovals(ctx context.Context, principalID, requestID string) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list direct approvals: SQLite store is not open")
	}
	return queryApprovals(ctx, s.db, `SELECT `+approvalColumns+` FROM approvals WHERE principal_id = ? AND request_id = ? AND mode = 'direct' ORDER BY created_at, rowid`, principalID, requestID)
}

func (s *Store) UnsentDirectApprovals(ctx context.Context, principalID string, threadID int64, now time.Time) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list unsent direct approvals: SQLite store is not open")
	}
	return queryApprovals(
		ctx,
		s.db,
		`SELECT `+approvalColumns+` FROM approvals
		 WHERE principal_id = ? AND thread_id = ? AND mode = 'direct' AND status IN ('done', 'undone') AND message_id IS NULL AND expires_at > ?
		 ORDER BY created_at, rowid`,
		principalID,
		threadID,
		formatTime(now),
	)
}

func (s *Store) ShownDirectApprovals(ctx context.Context, principalID string, chatID, messageID int64) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list shown direct approvals: SQLite store is not open")
	}
	return queryApprovals(
		ctx,
		s.db,
		`SELECT `+approvalColumns+` FROM approvals WHERE principal_id = ? AND chat_id = ? AND message_id = ? AND mode = 'direct' ORDER BY created_at, rowid`,
		principalID,
		chatID,
		messageID,
	)
}

func (s *Store) FindDirectApproval(ctx context.Context, principalID, tool, hash, requestID string) (approval.Record, bool, error) {
	if s == nil || s.db == nil {
		return approval.Record{}, false, fmt.Errorf("find direct approval: SQLite store is not open")
	}
	return scanApproval(s.db.QueryRowContext(
		ctx,
		`SELECT `+approvalColumns+` FROM approvals
		 WHERE principal_id = ? AND tool = ? AND arguments_hash = ? AND request_id = ? AND mode = 'direct'
		 ORDER BY created_at, rowid LIMIT 1`,
		principalID, tool, hash, requestID,
	))
}

func (s *Store) ClaimUndo(ctx context.Context, id, principalID string, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("claim approval undo: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE approvals SET reason = ?
		 WHERE id = ? AND principal_id = ? AND mode = 'direct' AND status = 'done' AND (reason IS NULL OR reason = ?)
		   AND undo IS NOT NULL AND undo_until > ?`,
		approval.ReasonUndoing,
		id,
		principalID,
		approval.ReasonUndoInterrupted,
		formatTime(now),
	)
	if err != nil {
		return false, fmt.Errorf("claim approval undo: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read claimed approval undo count: %w", err)
	}
	return changed == 1, nil
}

func (s *Store) CompleteUndo(ctx context.Context, id, outcome string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("complete approval undo: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE approvals SET status = 'undone', reason = NULL, outcome = NULLIF(?, '')
		 WHERE id = ? AND mode = 'direct' AND status = 'done' AND reason = ?`,
		outcome,
		id,
		approval.ReasonUndoing,
	)
	if err != nil {
		return fmt.Errorf("complete approval undo: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read completed approval undo count: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("complete approval undo: record is not being undone")
	}
	return nil
}

func (s *Store) ReleaseUndo(ctx context.Context, id, reason, outcome string, spent bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("release approval undo: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE approvals
		 SET reason = NULLIF(?, ''), outcome = COALESCE(NULLIF(?, ''), outcome),
		     undo = CASE WHEN ? THEN NULL ELSE undo END, undo_until = CASE WHEN ? THEN NULL ELSE undo_until END
		 WHERE id = ? AND mode = 'direct' AND status = 'done' AND reason = ?`,
		reason,
		outcome,
		spent,
		spent,
		id,
		approval.ReasonUndoing,
	); err != nil {
		return fmt.Errorf("release approval undo: %w", err)
	}
	return nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryApprovals(ctx context.Context, db queryer, query string, arguments ...any) ([]approval.Record, error) {
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list approvals: %w", err)
	}
	defer rows.Close()
	var records []approval.Record
	for rows.Next() {
		record, err := scanApprovalRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate approvals: %w", err)
	}
	return records, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanApproval(row *sql.Row) (approval.Record, bool, error) {
	record, err := scanApprovalRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return approval.Record{}, false, nil
	}
	if err != nil {
		return approval.Record{}, false, err
	}
	return record, true, nil
}

func scanApprovalRow(row rowScanner) (approval.Record, error) {
	var record approval.Record
	var arguments, createdAt, expiresAt, startedAt, finishedAt, undo, undoUntil string
	err := row.Scan(
		&record.ID, &record.BundleID, &record.Position, &record.PrincipalID, &record.Tool, &arguments, &record.Hash, &record.Summary, &record.Status,
		&record.Reason, &record.Outcome, &record.ChatID, &record.MessageID, &createdAt, &expiresAt, &startedAt, &finishedAt,
		&record.Mode, &record.RequestID, &undo, &undoUntil, &record.ThreadID, &record.Heading,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return approval.Record{}, err
		}
		return approval.Record{}, fmt.Errorf("scan approval: %w", err)
	}
	record.Arguments = json.RawMessage(arguments)
	if undo != "" {
		record.Undo = json.RawMessage(undo)
	}
	if undoUntil != "" {
		if record.UndoUntil, err = time.Parse(time.RFC3339Nano, undoUntil); err != nil {
			return approval.Record{}, fmt.Errorf("parse approval undo deadline: %w", err)
		}
	}
	if record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return approval.Record{}, fmt.Errorf("parse approval creation time: %w", err)
	}
	if record.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt); err != nil {
		return approval.Record{}, fmt.Errorf("parse approval expiry: %w", err)
	}
	if startedAt != "" {
		if record.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt); err != nil {
			return approval.Record{}, fmt.Errorf("parse approval start time: %w", err)
		}
	}
	if finishedAt != "" {
		if record.FinishedAt, err = time.Parse(time.RFC3339Nano, finishedAt); err != nil {
			return approval.Record{}, fmt.Errorf("parse approval finish time: %w", err)
		}
	}
	return record, nil
}

// formatTime stores UTC RFC 3339 timestamps with a fixed width so string comparison orders them.
func formatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

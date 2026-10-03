package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

var _ approval.Repository = (*Store)(nil)

const approvalColumns = `id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status,
	COALESCE(reason, ''), COALESCE(outcome, ''), COALESCE(chat_id, 0), COALESCE(message_id, 0),
	created_at, expires_at, COALESCE(started_at, ''), COALESCE(finished_at, '')`

func (s *Store) CreateApproval(ctx context.Context, record approval.Record) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("create approval: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin approval creation: %w", err)
	}
	defer tx.Rollback()

	var replaced []approval.Record
	if record.BundleID == record.ID {
		replaced, err = queryApprovals(ctx, tx, `SELECT `+approvalColumns+` FROM approvals WHERE principal_id = ? AND status = 'pending' ORDER BY bundle_id, position`, record.PrincipalID)
		if err != nil {
			return nil, err
		}
		for index := range replaced {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE approvals SET status = 'expired', reason = ?, finished_at = ? WHERE id = ? AND status = 'pending'`,
				approval.ReasonReplaced,
				formatTime(record.CreatedAt),
				replaced[index].ID,
			); err != nil {
				return nil, fmt.Errorf("replace pending approval: %w", err)
			}
			replaced[index].Status, replaced[index].Reason, replaced[index].FinishedAt = approval.StatusExpired, approval.ReasonReplaced, record.CreatedAt
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
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
	); err != nil {
		return nil, fmt.Errorf("insert approval: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit approval creation: %w", err)
	}
	return replaced, nil
}

func (s *Store) LoadApproval(ctx context.Context, id string) (approval.Record, bool, error) {
	if s == nil || s.db == nil {
		return approval.Record{}, false, fmt.Errorf("load approval: SQLite store is not open")
	}
	return scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id))
}

func (s *Store) LoadBundle(ctx context.Context, bundleID string) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("load approval bundle: SQLite store is not open")
	}
	return queryApprovals(ctx, s.db, `SELECT `+approvalColumns+` FROM approvals WHERE bundle_id = ? ORDER BY position`, bundleID)
}

func (s *Store) PendingApprovals(ctx context.Context, principalID string) ([]approval.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("load pending approvals: SQLite store is not open")
	}
	return queryApprovals(ctx, s.db, `SELECT `+approvalColumns+` FROM approvals WHERE principal_id = ? AND status = 'pending' ORDER BY bundle_id, position`, principalID)
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
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET summary = ? WHERE id = ? AND status = 'pending'`, summary, id); err != nil {
		return fmt.Errorf("update approval summary: %w", err)
	}
	return nil
}

func (s *Store) SetApprovalMessage(ctx context.Context, bundleID string, chatID, messageID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record approval message: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE approvals SET chat_id = ?, message_id = NULLIF(?, 0) WHERE bundle_id = ?`,
		chatID,
		messageID,
		bundleID,
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
		query = `SELECT ` + approvalColumns + ` FROM approvals WHERE status IN ('pending', 'confirmed')`
		arguments = nil
	}
	expiring, err := queryApprovals(ctx, tx, query+` ORDER BY bundle_id, position`, arguments...)
	if err != nil {
		return nil, err
	}

	finished := formatTime(now)
	for index := range expiring {
		record := &expiring[index]
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
		`SELECT `+approvalColumns+` FROM approvals WHERE status = 'pending' AND chat_id IS NOT NULL AND message_id IS NULL ORDER BY bundle_id, position`,
	)
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
	var arguments, createdAt, expiresAt, startedAt, finishedAt string
	err := row.Scan(
		&record.ID, &record.BundleID, &record.Position, &record.PrincipalID, &record.Tool, &arguments, &record.Hash, &record.Summary, &record.Status,
		&record.Reason, &record.Outcome, &record.ChatID, &record.MessageID, &createdAt, &expiresAt, &startedAt, &finishedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return approval.Record{}, err
		}
		return approval.Record{}, fmt.Errorf("scan approval: %w", err)
	}
	record.Arguments = json.RawMessage(arguments)
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

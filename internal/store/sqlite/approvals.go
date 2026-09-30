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

const approvalColumns = `id, principal_id, tool, arguments, arguments_hash, summary, status,
	COALESCE(reason, ''), COALESCE(outcome, ''), COALESCE(chat_id, 0), COALESCE(message_id, 0),
	created_at, expires_at, COALESCE(finished_at, '')`

func (s *Store) CreateApproval(ctx context.Context, record approval.Record) (approval.Record, bool, error) {
	if s == nil || s.db == nil {
		return approval.Record{}, false, fmt.Errorf("create approval: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return approval.Record{}, false, fmt.Errorf("begin approval creation: %w", err)
	}
	defer tx.Rollback()

	previous, hadPrevious, err := scanApproval(tx.QueryRowContext(
		ctx,
		`SELECT `+approvalColumns+` FROM approvals WHERE principal_id = ? AND status = 'pending'`,
		record.PrincipalID,
	))
	if err != nil {
		return approval.Record{}, false, err
	}
	if hadPrevious {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE approvals SET status = 'expired', reason = ?, finished_at = ? WHERE id = ? AND status = 'pending'`,
			approval.ReasonReplaced,
			formatTime(record.CreatedAt),
			previous.ID,
		); err != nil {
			return approval.Record{}, false, fmt.Errorf("replace pending approval: %w", err)
		}
		previous.Status, previous.Reason, previous.FinishedAt = approval.StatusExpired, approval.ReasonReplaced, record.CreatedAt
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO approvals (id, principal_id, tool, arguments, arguments_hash, summary, status, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		record.ID,
		record.PrincipalID,
		record.Tool,
		string(record.Arguments),
		record.Hash,
		record.Summary,
		formatTime(record.CreatedAt),
		formatTime(record.ExpiresAt),
	); err != nil {
		return approval.Record{}, false, fmt.Errorf("insert approval: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return approval.Record{}, false, fmt.Errorf("commit approval creation: %w", err)
	}
	return previous, hadPrevious, nil
}

func (s *Store) LoadApproval(ctx context.Context, id string) (approval.Record, bool, error) {
	if s == nil || s.db == nil {
		return approval.Record{}, false, fmt.Errorf("load approval: SQLite store is not open")
	}
	return scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id))
}

func (s *Store) PendingApproval(ctx context.Context, principalID string) (approval.Record, bool, error) {
	if s == nil || s.db == nil {
		return approval.Record{}, false, fmt.Errorf("load pending approval: SQLite store is not open")
	}
	return scanApproval(s.db.QueryRowContext(
		ctx,
		`SELECT `+approvalColumns+` FROM approvals WHERE principal_id = ? AND status = 'pending'`,
		principalID,
	))
}

func (s *Store) ConfirmApproval(ctx context.Context, id, principalID string, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("confirm approval: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE approvals SET status = 'confirmed'
		 WHERE id = ? AND principal_id = ? AND status = 'pending' AND expires_at > ?`,
		id,
		principalID,
		formatTime(now),
	)
	if err != nil {
		return false, fmt.Errorf("confirm approval: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read confirmed approval count: %w", err)
	}
	return changed == 1, nil
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

func (s *Store) SetApprovalMessage(ctx context.Context, id string, chatID, messageID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record approval message: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE approvals SET chat_id = ?, message_id = NULLIF(?, 0) WHERE id = ?`,
		chatID,
		messageID,
		id,
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
	rows, err := tx.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list expiring approvals: %w", err)
	}
	var expiring []approval.Record
	for rows.Next() {
		record, err := scanApprovalRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		expiring = append(expiring, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate expiring approvals: %w", err)
	}
	rows.Close()

	finished := formatTime(now)
	for index := range expiring {
		record := &expiring[index]
		if record.Status == approval.StatusConfirmed {
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
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+approvalColumns+` FROM approvals WHERE status = 'pending' AND chat_id IS NOT NULL AND message_id IS NULL`,
	)
	if err != nil {
		return nil, fmt.Errorf("list undelivered approvals: %w", err)
	}
	defer rows.Close()
	var undelivered []approval.Record
	for rows.Next() {
		record, err := scanApprovalRow(rows)
		if err != nil {
			return nil, err
		}
		undelivered = append(undelivered, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate undelivered approvals: %w", err)
	}
	return undelivered, nil
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
	var arguments, createdAt, expiresAt, finishedAt string
	err := row.Scan(
		&record.ID, &record.PrincipalID, &record.Tool, &arguments, &record.Hash, &record.Summary, &record.Status,
		&record.Reason, &record.Outcome, &record.ChatID, &record.MessageID, &createdAt, &expiresAt, &finishedAt,
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

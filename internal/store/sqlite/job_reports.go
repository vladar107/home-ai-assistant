package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

const jobReportColumns = `id, principal_id, job_id, job_name, job_type, text, private, delivered_at`

func (s *Store) SaveJobReport(ctx context.Context, report schedule.Report, keep int) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save job report: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin job report save: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO job_reports (id, principal_id, job_id, job_name, job_type, text, private, delivered_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO NOTHING`,
		report.ID, report.PrincipalID, report.JobID, report.JobName, report.JobType, report.Text, report.Private, formatTime(report.DeliveredAt),
	); err != nil {
		return fmt.Errorf("save job report: %w", err)
	}
	for _, messageID := range report.MessageIDs {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO job_report_messages (principal_id, telegram_message_id, report_id)
			 SELECT principal_id, ?, id FROM job_reports WHERE id = ? AND principal_id = ?
			 ON CONFLICT DO NOTHING`,
			messageID, report.ID, report.PrincipalID,
		); err != nil {
			return fmt.Errorf("save job report message: %w", err)
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM job_reports WHERE principal_id = ? AND id NOT IN (
		     SELECT id FROM job_reports WHERE principal_id = ? ORDER BY delivered_at DESC, id DESC LIMIT ?
		 )`,
		report.PrincipalID, report.PrincipalID, keep,
	); err != nil {
		return fmt.Errorf("trim job reports: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit job report save: %w", err)
	}
	return nil
}

func (s *Store) TakeJobReports(ctx context.Context, principalID string) ([]schedule.Report, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("take job reports: SQLite store is not open")
	}
	reports, err := s.queryJobReports(ctx,
		`UPDATE job_reports SET noticed = 1 WHERE principal_id = ? AND noticed = 0 RETURNING `+jobReportColumns,
		principalID,
	)
	if err != nil {
		return nil, err
	}
	// RETURNING gives no order.
	slices.SortStableFunc(reports, func(a, b schedule.Report) int {
		if order := a.DeliveredAt.Compare(b.DeliveredAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	return reports, nil
}

func (s *Store) RecentJobReports(ctx context.Context, principalID string, since time.Time) ([]schedule.Report, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list job reports: SQLite store is not open")
	}
	return s.queryJobReports(ctx,
		`SELECT `+jobReportColumns+` FROM job_reports
		 WHERE principal_id = ? AND delivered_at >= ?
		 ORDER BY delivered_at DESC, id DESC`,
		principalID, formatTime(since),
	)
}

func (s *Store) LoadJobReport(ctx context.Context, principalID, reportID string) (schedule.Report, bool, error) {
	if s == nil || s.db == nil {
		return schedule.Report{}, false, fmt.Errorf("load job report: SQLite store is not open")
	}
	return s.queryJobReport(ctx,
		`SELECT `+jobReportColumns+` FROM job_reports WHERE principal_id = ? AND id = ?`,
		principalID, reportID,
	)
}

func (s *Store) JobReportByMessage(ctx context.Context, principalID string, messageID int64) (schedule.Report, bool, error) {
	if s == nil || s.db == nil {
		return schedule.Report{}, false, fmt.Errorf("load job report by message: SQLite store is not open")
	}
	return s.queryJobReport(ctx,
		`SELECT report.id, report.principal_id, report.job_id, report.job_name, report.job_type, report.text,
		        report.private, report.delivered_at
		 FROM job_report_messages AS message
		 JOIN job_reports AS report ON report.id = message.report_id AND report.principal_id = message.principal_id
		 WHERE message.principal_id = ? AND message.telegram_message_id = ?`,
		principalID, messageID,
	)
}

func (s *Store) HasJobReports(ctx context.Context, principalID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("check job reports: SQLite store is not open")
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_reports WHERE principal_id = ?)`, principalID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check job reports: %w", err)
	}
	return exists, nil
}

func (s *Store) PruneJobReports(ctx context.Context, before time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("prune job reports: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM job_reports WHERE delivered_at < ?`, formatTime(before)); err != nil {
		return fmt.Errorf("prune job reports: %w", err)
	}
	return nil
}

func (s *Store) queryJobReport(ctx context.Context, query string, arguments ...any) (schedule.Report, bool, error) {
	report, err := scanJobReport(s.db.QueryRowContext(ctx, query, arguments...))
	if errors.Is(err, sql.ErrNoRows) {
		return schedule.Report{}, false, nil
	}
	if err != nil {
		return schedule.Report{}, false, err
	}
	return report, true, nil
}

func (s *Store) queryJobReports(ctx context.Context, query string, arguments ...any) ([]schedule.Report, error) {
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list job reports: %w", err)
	}
	defer rows.Close()
	var reports []schedule.Report
	for rows.Next() {
		report, err := scanJobReport(rows)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate job reports: %w", err)
	}
	return reports, nil
}

func scanJobReport(row rowScanner) (schedule.Report, error) {
	var report schedule.Report
	var deliveredAt string
	if err := row.Scan(&report.ID, &report.PrincipalID, &report.JobID, &report.JobName, &report.JobType, &report.Text, &report.Private, &deliveredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return schedule.Report{}, err
		}
		return schedule.Report{}, fmt.Errorf("scan job report: %w", err)
	}
	var err error
	if report.DeliveredAt, err = time.Parse(time.RFC3339Nano, deliveredAt); err != nil {
		return schedule.Report{}, fmt.Errorf("parse job report delivery time: %w", err)
	}
	return report, nil
}

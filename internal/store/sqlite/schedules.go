package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

var _ schedule.Repository = (*Store)(nil)

const scheduledJobColumns = `id, principal_id, name, job_type, cron, run_at, zone, config, enabled, next_run_at, finished_at, created_at, updated_at`

const scheduleRunColumns = `r.id, r.job_id, r.principal_id, r.scheduled_for, r.manual, r.status,
	COALESCE(r.error_kind, ''), COALESCE(r.reply, ''), j.name, j.job_type, j.zone, j.run_at IS NOT NULL, j.config`

func (s *Store) CreateScheduledJob(ctx context.Context, job schedule.Job, limit int) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("create scheduled job: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin scheduled job creation: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM scheduled_jobs WHERE id = ?)`, job.ID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check scheduled job: %w", err)
	}
	if exists {
		return false, nil
	}
	var count int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM scheduled_jobs WHERE principal_id = ? AND (run_at IS NOT NULL) = ? AND finished_at IS NULL`,
		job.PrincipalID, job.OneTime(),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("count scheduled jobs: %w", err)
	}
	if count >= limit {
		return false, schedule.ErrTooManyJobs
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO scheduled_jobs (`+scheduledJobColumns+`) VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, ''), ?, ?)`,
		job.ID, job.PrincipalID, job.Name, job.Type, job.Cron, optionalTime(job.RunAt), job.Zone, string(job.Config), job.Enabled,
		formatTime(job.NextRunAt), optionalTime(job.FinishedAt), formatTime(job.CreatedAt), formatTime(job.UpdatedAt),
	); err != nil {
		return false, fmt.Errorf("insert scheduled job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit scheduled job creation: %w", err)
	}
	return true, nil
}

func (s *Store) ListScheduledJobs(ctx context.Context, principalID string) ([]schedule.Job, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list scheduled jobs: SQLite store is not open")
	}
	return s.queryScheduledJobs(
		ctx,
		`SELECT `+scheduledJobColumns+` FROM scheduled_jobs WHERE principal_id = ? AND finished_at IS NULL
		 ORDER BY run_at IS NOT NULL, COALESCE(run_at, created_at), id`,
		principalID,
	)
}

func (s *Store) LoadScheduledJob(ctx context.Context, principalID, jobID string) (schedule.Job, bool, error) {
	if s == nil || s.db == nil {
		return schedule.Job{}, false, fmt.Errorf("load scheduled job: SQLite store is not open")
	}
	job, err := scanScheduledJob(s.db.QueryRowContext(
		ctx,
		`SELECT `+scheduledJobColumns+` FROM scheduled_jobs WHERE id = ? AND principal_id = ?`,
		jobID, principalID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return schedule.Job{}, false, nil
	}
	if err != nil {
		return schedule.Job{}, false, err
	}
	return job, true, nil
}

func (s *Store) SetScheduledJobEnabled(ctx context.Context, principalID, jobID string, enabled bool, nextRunAt, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("change scheduled job: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE scheduled_jobs SET enabled = ?, next_run_at = ?, updated_at = ? WHERE id = ? AND principal_id = ?`,
		enabled, formatTime(nextRunAt), formatTime(now), jobID, principalID,
	)
	if err != nil {
		return false, fmt.Errorf("change scheduled job: %w", err)
	}
	return affectedOne(result, "changed scheduled job")
}

func (s *Store) UpdateScheduledJob(ctx context.Context, job schedule.Job, expectedUpdatedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("update scheduled job: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE scheduled_jobs SET name = ?, cron = NULLIF(?, ''), run_at = NULLIF(?, ''), zone = ?, config = ?, next_run_at = ?, updated_at = ?
		 WHERE id = ? AND principal_id = ? AND job_type = ? AND finished_at IS NULL AND updated_at = ?`,
		job.Name, job.Cron, optionalTime(job.RunAt), job.Zone, string(job.Config), formatTime(job.NextRunAt), formatTime(job.UpdatedAt),
		job.ID, job.PrincipalID, job.Type, formatTime(expectedUpdatedAt),
	)
	if err != nil {
		return false, fmt.Errorf("update scheduled job: %w", err)
	}
	return affectedOne(result, "updated scheduled job")
}

func (s *Store) DeleteScheduledJob(ctx context.Context, principalID, jobID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete scheduled job: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM scheduled_jobs WHERE id = ? AND principal_id = ?`, jobID, principalID)
	if err != nil {
		return false, fmt.Errorf("delete scheduled job: %w", err)
	}
	return affectedOne(result, "deleted scheduled job")
}

func (s *Store) DueScheduledJobs(ctx context.Context, now time.Time) ([]schedule.Job, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list due scheduled jobs: SQLite store is not open")
	}
	return s.queryScheduledJobs(
		ctx,
		`SELECT `+scheduledJobColumns+` FROM scheduled_jobs WHERE enabled = 1 AND finished_at IS NULL AND next_run_at <= ? ORDER BY next_run_at, id`,
		formatTime(now),
	)
}

func (s *Store) AdvanceScheduledJob(ctx context.Context, jobID string, due, next time.Time, run *schedule.Run) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("advance scheduled job: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin scheduled job advance: %w", err)
	}
	defer tx.Rollback()

	now := formatTime(time.Now())
	query := `UPDATE scheduled_jobs SET next_run_at = ?, updated_at = ? WHERE id = ? AND enabled = 1 AND finished_at IS NULL AND next_run_at = ?`
	arguments := []any{formatTime(next), now, jobID, formatTime(due)}
	if next.IsZero() {
		query = `UPDATE scheduled_jobs SET finished_at = ?, updated_at = ? WHERE id = ? AND enabled = 1 AND finished_at IS NULL AND next_run_at = ? AND run_at IS NOT NULL`
		arguments = []any{now, now, jobID, formatTime(due)}
	}
	result, err := tx.ExecContext(ctx, query, arguments...)
	if err != nil {
		return false, fmt.Errorf("advance scheduled job: %w", err)
	}
	advanced, err := affectedOne(result, "advanced scheduled job")
	if err != nil || !advanced {
		return false, err
	}
	if run != nil {
		if err := insertScheduleRun(ctx, tx, *run, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit scheduled job advance: %w", err)
	}
	return true, nil
}

func (s *Store) ReviveScheduledJob(ctx context.Context, principalID, jobID, jobType string, runAt, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("revive scheduled job: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE scheduled_jobs SET run_at = ?, next_run_at = ?, finished_at = NULL, enabled = 1, updated_at = ?
		 WHERE id = ? AND principal_id = ? AND job_type = ? AND finished_at IS NOT NULL`,
		formatTime(runAt), formatTime(runAt), formatTime(now), jobID, principalID, jobType,
	)
	if err != nil {
		return false, fmt.Errorf("revive scheduled job: %w", err)
	}
	return affectedOne(result, "revived scheduled job")
}

func (s *Store) QueueScheduledRun(ctx context.Context, run schedule.Run) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("queue scheduled run: SQLite store is not open")
	}
	return insertScheduleRun(ctx, s.db, run, formatTime(time.Now()))
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// insertScheduleRun ignores an occurrence that is already recorded, which is what makes a second
// claim of the same occurrence harmless.
func insertScheduleRun(ctx context.Context, db execer, run schedule.Run, now string) error {
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO schedule_runs (id, job_id, principal_id, scheduled_for, manual, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 'queued', ?, ?)
		 ON CONFLICT (job_id, scheduled_for) DO NOTHING`,
		run.ID, run.JobID, run.PrincipalID, formatTime(run.ScheduledFor), run.Manual, now, now,
	); err != nil {
		return fmt.Errorf("insert scheduled run: %w", err)
	}
	return nil
}

func (s *Store) ScheduledRuns(ctx context.Context, statuses ...string) ([]schedule.Run, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list scheduled runs: SQLite store is not open")
	}
	if len(statuses) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(statuses)), ", ")
	arguments := make([]any, 0, len(statuses))
	for _, status := range statuses {
		arguments = append(arguments, status)
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+scheduleRunColumns+` FROM schedule_runs r JOIN scheduled_jobs j ON j.id = r.job_id
		 WHERE r.status IN (`+placeholders+`) ORDER BY r.scheduled_for, r.id`,
		arguments...,
	)
	if err != nil {
		return nil, fmt.Errorf("list scheduled runs: %w", err)
	}
	defer rows.Close()
	var runs []schedule.Run
	for rows.Next() {
		var run schedule.Run
		var scheduledFor, config string
		if err := rows.Scan(
			&run.ID, &run.JobID, &run.PrincipalID, &scheduledFor, &run.Manual, &run.Status,
			&run.ErrorKind, &run.Reply, &run.JobName, &run.JobType, &run.JobZone, &run.JobOneTime, &config,
		); err != nil {
			return nil, fmt.Errorf("scan scheduled run: %w", err)
		}
		if run.ScheduledFor, err = time.Parse(time.RFC3339Nano, scheduledFor); err != nil {
			return nil, fmt.Errorf("parse scheduled run time: %w", err)
		}
		run.Config = json.RawMessage(config)
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scheduled runs: %w", err)
	}
	return runs, nil
}

func (s *Store) TransitionScheduledRun(ctx context.Context, runID, from, to, reply, errorKind string, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("change scheduled run: SQLite store is not open")
	}
	var finished any
	switch to {
	case schedule.StatusDelivered, schedule.StatusSkipped, schedule.StatusFailed:
		finished = formatTime(now)
	}
	if to != schedule.StatusReady {
		reply = ""
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE schedule_runs SET status = ?, reply = NULLIF(?, ''), error_kind = NULLIF(?, ''), updated_at = ?, finished_at = ?
		 WHERE id = ? AND status = ?`,
		to, reply, errorKind, formatTime(now), finished, runID, from,
	)
	if err != nil {
		return false, fmt.Errorf("change scheduled run: %w", err)
	}
	return affectedOne(result, "changed scheduled run")
}

func (s *Store) RecoverScheduledRuns(ctx context.Context, reply, errorKind string, now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("recover scheduled runs: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE schedule_runs SET status = 'ready', reply = ?, error_kind = ?, updated_at = ? WHERE status = 'running'`,
		reply, errorKind, formatTime(now),
	)
	if err != nil {
		return 0, fmt.Errorf("recover scheduled runs: %w", err)
	}
	recovered, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read recovered scheduled run count: %w", err)
	}
	return int(recovered), nil
}

func (s *Store) PruneScheduledRuns(ctx context.Context, before time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("prune scheduled runs: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`DELETE FROM schedule_runs WHERE finished_at IS NOT NULL AND finished_at < ?`,
		formatTime(before),
	); err != nil {
		return fmt.Errorf("prune scheduled runs: %w", err)
	}
	if _, err := s.db.ExecContext(
		ctx,
		`DELETE FROM scheduled_jobs WHERE finished_at IS NOT NULL AND finished_at < ?
		 AND NOT EXISTS (SELECT 1 FROM schedule_runs r WHERE r.job_id = scheduled_jobs.id AND r.finished_at IS NULL)`,
		formatTime(before),
	); err != nil {
		return fmt.Errorf("prune finished scheduled jobs: %w", err)
	}
	return nil
}

func (s *Store) queryScheduledJobs(ctx context.Context, query string, arguments ...any) ([]schedule.Job, error) {
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list scheduled jobs: %w", err)
	}
	defer rows.Close()
	var jobs []schedule.Job
	for rows.Next() {
		job, err := scanScheduledJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scheduled jobs: %w", err)
	}
	return jobs, nil
}

func scanScheduledJob(row rowScanner) (schedule.Job, error) {
	var job schedule.Job
	var config, nextRunAt, createdAt, updatedAt string
	var cron, runAt, finishedAt sql.NullString
	if err := row.Scan(
		&job.ID, &job.PrincipalID, &job.Name, &job.Type, &cron, &runAt, &job.Zone, &config, &job.Enabled,
		&nextRunAt, &finishedAt, &createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return schedule.Job{}, err
		}
		return schedule.Job{}, fmt.Errorf("scan scheduled job: %w", err)
	}
	job.Config = json.RawMessage(config)
	job.Cron = cron.String
	var err error
	if runAt.Valid {
		if job.RunAt, err = time.Parse(time.RFC3339Nano, runAt.String); err != nil {
			return schedule.Job{}, fmt.Errorf("parse scheduled job instant: %w", err)
		}
	}
	if finishedAt.Valid {
		if job.FinishedAt, err = time.Parse(time.RFC3339Nano, finishedAt.String); err != nil {
			return schedule.Job{}, fmt.Errorf("parse scheduled job finish time: %w", err)
		}
	}
	if job.NextRunAt, err = time.Parse(time.RFC3339Nano, nextRunAt); err != nil {
		return schedule.Job{}, fmt.Errorf("parse scheduled job next run: %w", err)
	}
	if job.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return schedule.Job{}, fmt.Errorf("parse scheduled job creation time: %w", err)
	}
	if job.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return schedule.Job{}, fmt.Errorf("parse scheduled job update time: %w", err)
	}
	return job, nil
}

func optionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return formatTime(value)
}

func affectedOne(result sql.Result, what string) (bool, error) {
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read %s count: %w", what, err)
	}
	return changed == 1, nil
}

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/task"
)

var _ task.Repository = (*Store)(nil)

const taskColumns = `id, principal_id, chat_id, request_message_id, title, brief, session_id, status, partial,
	COALESCE(error_kind, ''), COALESCE(result, ''), COALESCE(status_message_id, 0), attempts,
	tool_calls, web_searches, model_calls, prompt_tokens, output_tokens,
	deadline, created_at, started_at, finished_at, delivered_at, thread_id, delivered_chunks`

// taskOpenWork is a task that still needs its worker: one to run, or a report to deliver.
const taskOpenWork = `(status IN ('queued', 'running') OR (finished_at IS NOT NULL AND delivered_at IS NULL))`

func (s *Store) CreateTask(ctx context.Context, created task.Task, maximumOpen int) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("create task: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin task creation: %w", err)
	}
	defer tx.Rollback()
	var open int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM tasks WHERE principal_id = ? AND status IN ('queued', 'running')`,
		created.PrincipalID,
	).Scan(&open); err != nil {
		return 0, fmt.Errorf("count open tasks: %w", err)
	}
	if open >= maximumOpen {
		return open, task.ErrTooManyTasks
	}
	now := formatTime(created.CreatedAt)
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO tasks (id, principal_id, chat_id, request_message_id, title, brief, session_id, status, created_at, updated_at, thread_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?)`,
		created.ID, created.PrincipalID, created.ChatID, created.RequestMessageID, created.Title, created.Brief, created.SessionID, now, now, created.ThreadID,
	); err != nil {
		return 0, fmt.Errorf("insert task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit task creation: %w", err)
	}
	return open, nil
}

func (s *Store) TaskOrigin(ctx context.Context, principalID, requestID string) (int64, int64, bool, error) {
	if s == nil || s.db == nil {
		return 0, 0, false, fmt.Errorf("load task origin: SQLite store is not open")
	}
	var chatID int64
	var messageID sql.NullInt64
	err := s.db.QueryRowContext(
		ctx,
		`SELECT chat_id, telegram_message_id FROM conversation_requests WHERE id = ? AND principal_id = ?`,
		requestID, principalID,
	).Scan(&chatID, &messageID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, 0, false, nil
	case err != nil:
		return 0, 0, false, fmt.Errorf("load task origin: %w", err)
	case !messageID.Valid:
		return 0, 0, false, nil
	}
	return chatID, messageID.Int64, true, nil
}

func (s *Store) SetTaskStatusMessage(ctx context.Context, taskID string, messageID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store task status message: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status_message_id = ? WHERE id = ?`, messageID, taskID); err != nil {
		return fmt.Errorf("store task status message: %w", err)
	}
	return nil
}

func (s *Store) NextTask(ctx context.Context, principalID string) (task.Task, bool, error) {
	if s == nil || s.db == nil {
		return task.Task{}, false, fmt.Errorf("load next task: SQLite store is not open")
	}
	return s.queryTask(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE principal_id = ? AND `+taskOpenWork+`
		 ORDER BY CASE WHEN finished_at IS NOT NULL THEN 0 WHEN status = 'running' THEN 1 ELSE 2 END, created_at, rowid
		 LIMIT 1`,
		principalID,
	)
}

func (s *Store) StartTask(ctx context.Context, taskID string, deadline, now time.Time) (task.Task, bool, error) {
	if s == nil || s.db == nil {
		return task.Task{}, false, fmt.Errorf("start task: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE tasks SET status = 'running', attempts = attempts + 1, started_at = COALESCE(started_at, ?),
		 deadline = COALESCE(deadline, ?), updated_at = ?
		 WHERE id = ? AND status IN ('queued', 'running')`,
		formatTime(now), formatTime(deadline), formatTime(now), taskID,
	)
	if err != nil {
		return task.Task{}, false, fmt.Errorf("start task: %w", err)
	}
	if started, err := affectedOne(result, "started task"); err != nil || !started {
		return task.Task{}, false, err
	}
	return s.queryTask(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, taskID)
}

func (s *Store) SaveTaskProgress(ctx context.Context, taskID string, progress task.Progress, now time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store task progress: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE tasks SET tool_calls = ?, web_searches = ?, model_calls = ?, prompt_tokens = ?, output_tokens = ?, updated_at = ?
		 WHERE id = ? AND status = 'running'`,
		progress.ToolCalls, progress.WebSearches, progress.ModelCalls, progress.PromptTokens, progress.OutputTokens, formatTime(now), taskID,
	); err != nil {
		return fmt.Errorf("store task progress: %w", err)
	}
	return nil
}

func (s *Store) FinishTask(ctx context.Context, taskID string, outcome task.Outcome, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("finish task: SQLite store is not open")
	}
	if outcome.Status != task.StatusDone && outcome.Status != task.StatusFailed {
		return false, fmt.Errorf("finish task: status %q is not a finish", outcome.Status)
	}
	progress := outcome.Progress
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE tasks SET status = ?, partial = ?, result = ?, error_kind = NULLIF(?, ''),
		 tool_calls = ?, web_searches = ?, model_calls = ?, prompt_tokens = ?, output_tokens = ?, finished_at = ?, updated_at = ?
		 WHERE id = ? AND status = 'running'`,
		outcome.Status, outcome.Partial, outcome.Result, outcome.ErrorKind,
		progress.ToolCalls, progress.WebSearches, progress.ModelCalls, progress.PromptTokens, progress.OutputTokens, formatTime(now), formatTime(now), taskID,
	)
	if err != nil {
		return false, fmt.Errorf("finish task: %w", err)
	}
	return affectedOne(result, "finished task")
}

func (s *Store) CancelTask(ctx context.Context, principalID, taskID string, now time.Time) (task.Task, bool, error) {
	if s == nil || s.db == nil {
		return task.Task{}, false, fmt.Errorf("cancel task: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE tasks SET status = 'cancelled', finished_at = ?, delivered_at = ?, updated_at = ?
		 WHERE id = ? AND principal_id = ? AND status IN ('queued', 'running')`,
		formatTime(now), formatTime(now), formatTime(now), taskID, principalID,
	)
	if err != nil {
		return task.Task{}, false, fmt.Errorf("cancel task: %w", err)
	}
	if cancelled, err := affectedOne(result, "cancelled task"); err != nil || !cancelled {
		return task.Task{}, false, err
	}
	return s.LoadTask(ctx, principalID, taskID)
}

func (s *Store) MarkTaskDelivered(ctx context.Context, taskID string, now time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("mark task delivered: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE tasks SET delivered_at = ?, updated_at = ? WHERE id = ? AND finished_at IS NOT NULL AND delivered_at IS NULL`,
		formatTime(now), formatTime(now), taskID,
	); err != nil {
		return fmt.Errorf("mark task delivered: %w", err)
	}
	return nil
}

func (s *Store) SaveTaskDeliveryProgress(ctx context.Context, taskID string, chunks int, now time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save task delivery progress: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE tasks SET delivered_chunks = ?, updated_at = ? WHERE id = ? AND finished_at IS NOT NULL AND delivered_at IS NULL`,
		chunks, formatTime(now), taskID,
	); err != nil {
		return fmt.Errorf("save task delivery progress: %w", err)
	}
	return nil
}

func (s *Store) RecoverTasks(ctx context.Context, maximumAttempts int, failure string, now time.Time) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("recover tasks: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE tasks SET status = 'failed', result = ?, error_kind = 'interrupted', finished_at = ?, updated_at = ?
		 WHERE status = 'running' AND attempts >= ?`,
		failure, formatTime(now), formatTime(now), maximumAttempts,
	); err != nil {
		return nil, fmt.Errorf("fail interrupted tasks: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT principal_id FROM tasks WHERE `+taskOpenWork+` ORDER BY principal_id`)
	if err != nil {
		return nil, fmt.Errorf("list principals with open tasks: %w", err)
	}
	defer rows.Close()
	var principals []string
	for rows.Next() {
		var principalID string
		if err := rows.Scan(&principalID); err != nil {
			return nil, fmt.Errorf("scan principal with open tasks: %w", err)
		}
		principals = append(principals, principalID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate principals with open tasks: %w", err)
	}
	return principals, nil
}

func (s *Store) ListTasks(ctx context.Context, principalID string, since time.Time) ([]task.Task, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list tasks: SQLite store is not open")
	}
	return s.queryTasks(ctx,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE principal_id = ? AND (status IN ('queued', 'running') OR finished_at >= ?)
		 ORDER BY created_at, rowid`,
		principalID, formatTime(since),
	)
}

func (s *Store) LoadTask(ctx context.Context, principalID, taskID string) (task.Task, bool, error) {
	if s == nil || s.db == nil {
		return task.Task{}, false, fmt.Errorf("load task: SQLite store is not open")
	}
	return s.queryTask(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ? AND principal_id = ?`, taskID, principalID)
}

func (s *Store) TakeTaskNotices(ctx context.Context, principalID string, since time.Time) ([]task.Task, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("take task notices: SQLite store is not open")
	}
	tasks, err := s.queryTasks(ctx,
		`UPDATE tasks SET noticed = 1
		 WHERE principal_id = ? AND noticed = 0 AND status IN ('done', 'failed') AND delivered_at >= ?
		 RETURNING `+taskColumns,
		principalID, formatTime(since),
	)
	if err != nil {
		return nil, err
	}
	// RETURNING gives no order.
	slices.SortStableFunc(tasks, func(a, b task.Task) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return tasks, nil
}

func (s *Store) PruneTasks(ctx context.Context, before time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("prune tasks: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`DELETE FROM tasks WHERE finished_at < ? AND (delivered_at IS NOT NULL OR status = 'cancelled')`,
		formatTime(before),
	); err != nil {
		return fmt.Errorf("prune tasks: %w", err)
	}
	return nil
}

func (s *Store) queryTask(ctx context.Context, query string, arguments ...any) (task.Task, bool, error) {
	loaded, err := scanTask(s.db.QueryRowContext(ctx, query, arguments...))
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, false, nil
	}
	if err != nil {
		return task.Task{}, false, err
	}
	return loaded, true, nil
}

func (s *Store) queryTasks(ctx context.Context, query string, arguments ...any) ([]task.Task, error) {
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	var tasks []task.Task
	for rows.Next() {
		loaded, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, loaded)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return tasks, nil
}

func scanTask(row rowScanner) (task.Task, error) {
	var loaded task.Task
	var createdAt string
	var deadline, startedAt, finishedAt, deliveredAt sql.NullString
	if err := row.Scan(
		&loaded.ID, &loaded.PrincipalID, &loaded.ChatID, &loaded.RequestMessageID, &loaded.Title, &loaded.Brief, &loaded.SessionID,
		&loaded.Status, &loaded.Partial, &loaded.ErrorKind, &loaded.Result, &loaded.StatusMessageID, &loaded.Attempts,
		&loaded.Progress.ToolCalls, &loaded.Progress.WebSearches, &loaded.Progress.ModelCalls, &loaded.Progress.PromptTokens, &loaded.Progress.OutputTokens,
		&deadline, &createdAt, &startedAt, &finishedAt, &deliveredAt, &loaded.ThreadID, &loaded.DeliveredChunks,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return task.Task{}, err
		}
		return task.Task{}, fmt.Errorf("scan task: %w", err)
	}
	var err error
	if loaded.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return task.Task{}, fmt.Errorf("parse task creation time: %w", err)
	}
	for _, field := range []struct {
		value  sql.NullString
		target *time.Time
	}{
		{deadline, &loaded.Deadline},
		{startedAt, &loaded.StartedAt},
		{finishedAt, &loaded.FinishedAt},
		{deliveredAt, &loaded.DeliveredAt},
	} {
		if !field.value.Valid {
			continue
		}
		if *field.target, err = time.Parse(time.RFC3339Nano, field.value.String); err != nil {
			return task.Task{}, fmt.Errorf("parse task time: %w", err)
		}
	}
	return loaded, nil
}

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/watch"
)

var _ watch.Repository = (*Store)(nil)

const watchColumns = `id, principal_id, config, enabled, COALESCE(muted_on, ''), created_at, updated_at, travel_pause`

func (s *Store) CreateWatch(ctx context.Context, w watch.Watch, limit int) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("create watch: SQLite store is not open")
	}
	config, err := json.Marshal(w.Spec)
	if err != nil {
		return false, fmt.Errorf("encode watch: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin watch creation: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM watches WHERE id = ?)`, w.ID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check watch: %w", err)
	}
	if exists {
		return false, nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM watches WHERE principal_id = ?`, w.PrincipalID).Scan(&count); err != nil {
		return false, fmt.Errorf("count watches: %w", err)
	}
	if count >= limit {
		return false, watch.ErrTooManyWatches
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO watches (id, principal_id, kind, config, enabled, muted_on, created_at, updated_at, travel_pause) VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?)`,
		w.ID, w.PrincipalID, w.Kind, string(config), w.Enabled, w.MutedOn, formatTime(w.CreatedAt), formatTime(w.UpdatedAt), w.TravelPause,
	); err != nil {
		return false, fmt.Errorf("insert watch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit watch creation: %w", err)
	}
	return true, nil
}

func (s *Store) ListWatches(ctx context.Context, principalID string) ([]watch.Watch, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list watches: SQLite store is not open")
	}
	return s.queryWatches(ctx, `SELECT `+watchColumns+` FROM watches WHERE principal_id = ? ORDER BY created_at, id`, principalID)
}

func (s *Store) AllWatches(ctx context.Context) ([]watch.Watch, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list watches: SQLite store is not open")
	}
	return s.queryWatches(ctx, `SELECT `+watchColumns+` FROM watches ORDER BY created_at, id`)
}

func (s *Store) LoadWatch(ctx context.Context, principalID, watchID string) (watch.Watch, bool, error) {
	if s == nil || s.db == nil {
		return watch.Watch{}, false, fmt.Errorf("load watch: SQLite store is not open")
	}
	w, err := scanWatch(s.db.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE id = ? AND principal_id = ?`, watchID, principalID))
	if errors.Is(err, sql.ErrNoRows) {
		return watch.Watch{}, false, nil
	}
	if err != nil {
		return watch.Watch{}, false, err
	}
	return w, true, nil
}

func (s *Store) UpdateWatch(ctx context.Context, w watch.Watch, expectedUpdatedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("update watch: SQLite store is not open")
	}
	config, err := json.Marshal(w.Spec)
	if err != nil {
		return false, fmt.Errorf("encode watch: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin watch update: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(
		ctx,
		`UPDATE watches SET config = ?, updated_at = ?, travel_pause = ? WHERE id = ? AND principal_id = ? AND kind = ? AND updated_at = ?`,
		string(config), formatTime(w.UpdatedAt), w.TravelPause, w.ID, w.PrincipalID, w.Kind, formatTime(expectedUpdatedAt),
	)
	if err != nil {
		return false, fmt.Errorf("update watch: %w", err)
	}
	updated, err := affectedOne(result, "updated watch")
	if err != nil || !updated {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM watch_states WHERE watch_id = ?`, w.ID); err != nil {
		return false, fmt.Errorf("reset watch states: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit watch update: %w", err)
	}
	return true, nil
}

func (s *Store) SetWatchEnabled(ctx context.Context, principalID, watchID string, enabled bool, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("change watch: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE watches SET enabled = ?, updated_at = ? WHERE id = ? AND principal_id = ?`,
		enabled, formatTime(now), watchID, principalID,
	)
	if err != nil {
		return false, fmt.Errorf("change watch: %w", err)
	}
	return affectedOne(result, "changed watch")
}

// SetWatchTravelPause leaves updated_at alone, like muting: the flag is not a change to the watch a
// pending edit could overwrite.
func (s *Store) SetWatchTravelPause(ctx context.Context, principalID, watchID string, on bool) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("change watch: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE watches SET travel_pause = ? WHERE id = ? AND principal_id = ?`, on, watchID, principalID)
	if err != nil {
		return false, fmt.Errorf("change watch: %w", err)
	}
	return affectedOne(result, "changed watch")
}

// MuteWatch leaves updated_at alone: muting is not a change to the watch a pending edit could overwrite.
func (s *Store) MuteWatch(ctx context.Context, principalID, watchID, date string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("mute watch: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE watches SET muted_on = ? WHERE id = ? AND principal_id = ?`,
		date, watchID, principalID,
	)
	if err != nil {
		return false, fmt.Errorf("mute watch: %w", err)
	}
	return affectedOne(result, "muted watch")
}

func (s *Store) DeleteWatch(ctx context.Context, principalID, watchID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete watch: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM watches WHERE id = ? AND principal_id = ?`, watchID, principalID)
	if err != nil {
		return false, fmt.Errorf("delete watch: %w", err)
	}
	return affectedOne(result, "deleted watch")
}

func (s *Store) DeleteUnchangedWatch(ctx context.Context, principalID, watchID string, updatedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete watch: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM watches WHERE id = ? AND principal_id = ? AND updated_at = ?`, watchID, principalID, formatTime(updatedAt))
	if err != nil {
		return false, fmt.Errorf("delete watch: %w", err)
	}
	return affectedOne(result, "deleted watch")
}

func (s *Store) LoadWatchState(ctx context.Context, watchID, occurrence string) (watch.State, bool, error) {
	if s == nil || s.db == nil {
		return watch.State{}, false, fmt.Errorf("load watch state: SQLite store is not open")
	}
	return s.queryWatchState(ctx, `occurrence = ?`, watchID, occurrence)
}

func (s *Store) LastWatchAlertSequence(ctx context.Context, watchID, occurrence string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("load watch alert sequence: SQLite store is not open")
	}
	var sequence int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM watch_alerts WHERE watch_id = ? AND occurrence = ?`, watchID, occurrence).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("load watch alert sequence: %w", err)
	}
	return sequence, nil
}

func (s *Store) LatestWatchStateBefore(ctx context.Context, watchID, occurrence string) (watch.State, bool, error) {
	if s == nil || s.db == nil {
		return watch.State{}, false, fmt.Errorf("load watch state: SQLite store is not open")
	}
	return s.queryWatchState(ctx, `occurrence < ?`, watchID, occurrence)
}

// queryWatchState reads the newest state of a watch whose occurrence satisfies the condition.
func (s *Store) queryWatchState(ctx context.Context, condition, watchID, occurrence string) (watch.State, bool, error) {
	var state watch.State
	var checkedAt, since string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT watch_id, principal_id, occurrence, COALESCE(state, ''), alerted, messages, failures, failure_noticed, COALESCE(since, ''), checked_at
		 FROM watch_states WHERE watch_id = ? AND `+condition+` ORDER BY occurrence DESC LIMIT 1`,
		watchID, occurrence,
	).Scan(&state.WatchID, &state.PrincipalID, &state.Occurrence, &state.Key, &state.Alerted, &state.Messages, &state.Failures, &state.FailureNoticed, &since, &checkedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return watch.State{}, false, nil
	}
	if err != nil {
		return watch.State{}, false, fmt.Errorf("load watch state: %w", err)
	}
	if state.CheckedAt, err = time.Parse(time.RFC3339Nano, checkedAt); err != nil {
		return watch.State{}, false, fmt.Errorf("parse watch check time: %w", err)
	}
	if since != "" {
		if state.Since, err = time.Parse(time.RFC3339Nano, since); err != nil {
			return watch.State{}, false, fmt.Errorf("parse watch condition start: %w", err)
		}
	}
	return state, true, nil
}

func (s *Store) SaveWatchCheck(ctx context.Context, state watch.State, alert *watch.Alert, now time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save watch check: SQLite store is not open")
	}
	var since string
	if !state.Since.IsZero() {
		since = formatTime(state.Since)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin watch check: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO watch_states (watch_id, principal_id, occurrence, state, alerted, messages, failures, failure_noticed, since, checked_at, updated_at)
		 VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, ''), ?, ?)
		 ON CONFLICT (watch_id, occurrence) DO UPDATE SET
		     state = excluded.state, alerted = excluded.alerted, messages = excluded.messages, failures = excluded.failures,
		     failure_noticed = excluded.failure_noticed, since = excluded.since, checked_at = excluded.checked_at, updated_at = excluded.updated_at`,
		state.WatchID, state.PrincipalID, state.Occurrence, state.Key, state.Alerted, state.Messages, state.Failures,
		state.FailureNoticed, since, formatTime(state.CheckedAt), formatTime(now),
	); err != nil {
		return fmt.Errorf("save watch state: %w", err)
	}
	if alert != nil {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO watch_alerts (id, watch_id, principal_id, occurrence, sequence, type, text, status, expires_at, private, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 'ready', ?, ?, ?, ?)
			 ON CONFLICT DO NOTHING`,
			alert.ID, alert.WatchID, alert.PrincipalID, alert.Occurrence, alert.Sequence, alert.Type, alert.Text,
			formatTime(alert.ExpiresAt), alert.Private, formatTime(now), formatTime(now),
		); err != nil {
			return fmt.Errorf("queue watch alert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit watch check: %w", err)
	}
	return nil
}

func (s *Store) ReadyWatchAlerts(ctx context.Context) ([]watch.Alert, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list watch alerts: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT a.id, a.watch_id, a.principal_id, a.occurrence, a.sequence, a.type, a.text, a.expires_at, a.private, w.kind, w.enabled, COALESCE(w.muted_on, ''), w.travel_pause,
		        COALESCE(json_extract(w.config, '$.language'), '')
		 FROM watch_alerts a JOIN watches w ON w.id = a.watch_id
		 WHERE a.status = 'ready' ORDER BY a.created_at, a.sequence, a.id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list watch alerts: %w", err)
	}
	defer rows.Close()
	var alerts []watch.Alert
	for rows.Next() {
		var alert watch.Alert
		var expiresAt string
		if err := rows.Scan(
			&alert.ID, &alert.WatchID, &alert.PrincipalID, &alert.Occurrence, &alert.Sequence, &alert.Type, &alert.Text,
			&expiresAt, &alert.Private, &alert.Kind, &alert.Enabled, &alert.MutedOn, &alert.TravelPause, &alert.Language,
		); err != nil {
			return nil, fmt.Errorf("scan watch alert: %w", err)
		}
		if alert.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt); err != nil {
			return nil, fmt.Errorf("parse watch alert expiry: %w", err)
		}
		alerts = append(alerts, alert)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate watch alerts: %w", err)
	}
	return alerts, nil
}

func (s *Store) FinishWatchAlert(ctx context.Context, alertID, status, reason string, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("finish watch alert: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE watch_alerts SET status = ?, reason = NULLIF(?, ''), text = NULL, updated_at = ?, finished_at = ?
		 WHERE id = ? AND status = 'ready'`,
		status, reason, formatTime(now), formatTime(now), alertID,
	)
	if err != nil {
		return false, fmt.Errorf("finish watch alert: %w", err)
	}
	return affectedOne(result, "finished watch alert")
}

func (s *Store) PruneWatchHistory(ctx context.Context, before time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("prune watch history: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM watch_alerts WHERE finished_at IS NOT NULL AND finished_at < ?`, formatTime(before)); err != nil {
		return fmt.Errorf("prune watch alerts: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM watch_states WHERE updated_at < ?`, formatTime(before)); err != nil {
		return fmt.Errorf("prune watch states: %w", err)
	}
	return nil
}

func (s *Store) queryWatches(ctx context.Context, query string, arguments ...any) ([]watch.Watch, error) {
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list watches: %w", err)
	}
	defer rows.Close()
	var watches []watch.Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		watches = append(watches, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate watches: %w", err)
	}
	return watches, nil
}

func scanWatch(row rowScanner) (watch.Watch, error) {
	var w watch.Watch
	var config, createdAt, updatedAt string
	if err := row.Scan(&w.ID, &w.PrincipalID, &config, &w.Enabled, &w.MutedOn, &createdAt, &updatedAt, &w.TravelPause); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return watch.Watch{}, err
		}
		return watch.Watch{}, fmt.Errorf("scan watch: %w", err)
	}
	if err := json.Unmarshal([]byte(config), &w.Spec); err != nil {
		return watch.Watch{}, fmt.Errorf("decode watch: %w", err)
	}
	var err error
	if w.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return watch.Watch{}, fmt.Errorf("parse watch creation time: %w", err)
	}
	if w.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return watch.Watch{}, fmt.Errorf("parse watch update time: %w", err)
	}
	return w, nil
}

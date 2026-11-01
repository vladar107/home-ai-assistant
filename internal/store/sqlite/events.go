package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/events"
)

var _ events.Repository = (*Store)(nil)

const eventSourceColumns = `id, principal_id, name, mode, token_hash, enabled, created_at, last_used_at, rotated_at`

func (s *Store) EventSources(ctx context.Context, principalID string) ([]events.Source, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list event sources: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventSourceColumns+` FROM event_sources WHERE principal_id = ? ORDER BY created_at, id`, principalID)
	if err != nil {
		return nil, fmt.Errorf("list event sources: %w", err)
	}
	defer rows.Close()
	var sources []events.Source
	for rows.Next() {
		source, err := scanEventSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list event sources: %w", err)
	}
	return sources, nil
}

func (s *Store) EventSource(ctx context.Context, principalID, id string) (events.Source, bool, error) {
	if s == nil || s.db == nil {
		return events.Source{}, false, fmt.Errorf("load event source: SQLite store is not open")
	}
	return s.queryEventSource(ctx, `SELECT `+eventSourceColumns+` FROM event_sources WHERE principal_id = ? AND id = ?`, principalID, id)
}

func (s *Store) EventSourceByTokenHash(ctx context.Context, hash string) (events.Source, bool, error) {
	if s == nil || s.db == nil {
		return events.Source{}, false, fmt.Errorf("load event source by token: SQLite store is not open")
	}
	return s.queryEventSource(ctx, `SELECT `+eventSourceColumns+` FROM event_sources WHERE token_hash = ?`, hash)
}

func (s *Store) queryEventSource(ctx context.Context, query string, arguments ...any) (events.Source, bool, error) {
	source, err := scanEventSource(s.db.QueryRowContext(ctx, query, arguments...))
	if errors.Is(err, sql.ErrNoRows) {
		return events.Source{}, false, nil
	}
	if err != nil {
		return events.Source{}, false, err
	}
	return source, true, nil
}

func (s *Store) AddEventSource(ctx context.Context, source events.Source, limit int) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("add event source: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO event_sources (id, principal_id, name, mode, token_hash, enabled, created_at)
		 SELECT ?, ?, ?, ?, ?, ?, ?
		 WHERE (SELECT count(*) FROM event_sources WHERE principal_id = ?) < ?`,
		source.ID, source.PrincipalID, source.Name, source.Mode, source.TokenHash, source.Enabled, formatTime(source.CreatedAt),
		source.PrincipalID, limit,
	)
	if err != nil {
		return false, fmt.Errorf("add event source: %w", err)
	}
	return affectedOne(result, "added event source")
}

func (s *Store) RotateEventSourceToken(ctx context.Context, principalID, id, hash string, at time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("rotate event source token: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE event_sources SET token_hash = ?, rotated_at = ? WHERE principal_id = ? AND id = ?`,
		hash, formatTime(at), principalID, id)
	if err != nil {
		return false, fmt.Errorf("rotate event source token: %w", err)
	}
	return affectedOne(result, "rotated event source token")
}

func (s *Store) SetEventSourceEnabled(ctx context.Context, principalID, id string, enabled bool) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("switch event source: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE event_sources SET enabled = ? WHERE principal_id = ? AND id = ?`, enabled, principalID, id)
	if err != nil {
		return false, fmt.Errorf("switch event source: %w", err)
	}
	return affectedOne(result, "switched event source")
}

func (s *Store) SetEventSourceMode(ctx context.Context, principalID, id, mode string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("set event source mode: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE event_sources SET mode = ? WHERE principal_id = ? AND id = ?`, mode, principalID, id)
	if err != nil {
		return false, fmt.Errorf("set event source mode: %w", err)
	}
	return affectedOne(result, "event source mode")
}

func (s *Store) RemoveEventSource(ctx context.Context, principalID, id string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("remove event source: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM event_sources WHERE principal_id = ? AND id = ?`, principalID, id)
	if err != nil {
		return false, fmt.Errorf("remove event source: %w", err)
	}
	return affectedOne(result, "removed event source")
}

func (s *Store) RecordInboundEvent(ctx context.Context, event events.Event, since time.Time, limit int) (events.Admission, error) {
	if s == nil || s.db == nil {
		return events.Limited, fmt.Errorf("record inbound event: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return events.Limited, fmt.Errorf("begin inbound event record: %w", err)
	}
	defer tx.Rollback()
	var recent int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM inbound_events WHERE source_id = ? AND principal_id = ? AND received_at >= ?`,
		event.SourceID, event.PrincipalID, formatTime(since),
	).Scan(&recent); err != nil {
		return events.Limited, fmt.Errorf("count inbound events: %w", err)
	}
	if recent >= limit {
		return events.Limited, nil
	}
	admission, outcome := events.Admitted, events.OutcomePending
	if event.DedupeKey != "" {
		var seen bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM inbound_events WHERE source_id = ? AND principal_id = ? AND dedupe_key = ? AND received_at >= ?)`,
			event.SourceID, event.PrincipalID, event.DedupeKey, formatTime(since),
		).Scan(&seen); err != nil {
			return events.Limited, fmt.Errorf("check inbound event dedupe key: %w", err)
		}
		if seen {
			admission, outcome = events.Duplicated, events.OutcomeDuplicate
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inbound_events (id, principal_id, source_id, received_at, title, text, severity, dedupe_key, outcome)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?)`,
		event.ID, event.PrincipalID, event.SourceID, formatTime(event.ReceivedAt), event.Title, event.Text, event.Severity, event.DedupeKey, outcome,
	); err != nil {
		return events.Limited, fmt.Errorf("record inbound event: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE event_sources SET last_used_at = ? WHERE id = ? AND principal_id = ?`,
		formatTime(event.ReceivedAt), event.SourceID, event.PrincipalID,
	); err != nil {
		return events.Limited, fmt.Errorf("mark event source used: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return events.Limited, fmt.Errorf("commit inbound event record: %w", err)
	}
	return admission, nil
}

const inboundEventColumns = `event.id, event.principal_id, event.source_id, event.received_at, event.title, event.text, event.severity,
	coalesce(event.dedupe_key, ''), event.outcome, coalesce(event.assessment, ''), coalesce(event.assessed_message, ''),
	source.name, source.mode, source.enabled`

func (s *Store) PendingInboundEvents(ctx context.Context) ([]events.Event, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list pending inbound events: SQLite store is not open")
	}
	return s.queryInboundEvents(ctx,
		`SELECT `+inboundEventColumns+` FROM inbound_events AS event
		 JOIN event_sources AS source ON source.id = event.source_id AND source.principal_id = event.principal_id
		 WHERE event.outcome = 'pending'
		 ORDER BY event.received_at, event.id`)
}

func (s *Store) RecentInboundEvents(ctx context.Context, principalID, source string, since time.Time, limit int) ([]events.Event, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list inbound events: SQLite store is not open")
	}
	return s.queryInboundEvents(ctx,
		`SELECT `+inboundEventColumns+` FROM inbound_events AS event
		 JOIN event_sources AS source ON source.id = event.source_id AND source.principal_id = event.principal_id
		 WHERE event.principal_id = ? AND event.received_at >= ? AND (? = '' OR source.name = ? COLLATE NOCASE)
		 ORDER BY event.received_at DESC, event.id DESC
		 LIMIT ?`,
		principalID, formatTime(since), source, source, limit)
}

func (s *Store) queryInboundEvents(ctx context.Context, query string, arguments ...any) ([]events.Event, error) {
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query inbound events: %w", err)
	}
	defer rows.Close()
	var found []events.Event
	for rows.Next() {
		var event events.Event
		var receivedAt string
		if err := rows.Scan(&event.ID, &event.PrincipalID, &event.SourceID, &receivedAt, &event.Title, &event.Text, &event.Severity,
			&event.DedupeKey, &event.Outcome, &event.Assessment, &event.AssessedMessage, &event.SourceName, &event.SourceMode, &event.SourceEnabled); err != nil {
			return nil, fmt.Errorf("scan inbound event: %w", err)
		}
		if event.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt); err != nil {
			return nil, fmt.Errorf("parse inbound event time: %w", err)
		}
		found = append(found, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query inbound events: %w", err)
	}
	return found, nil
}

func (s *Store) FinishInboundEvent(ctx context.Context, principalID, id, outcome string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("finish inbound event: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE inbound_events SET outcome = ? WHERE principal_id = ? AND id = ? AND outcome = 'pending'`, outcome, principalID, id)
	if err != nil {
		return false, fmt.Errorf("finish inbound event: %w", err)
	}
	return affectedOne(result, "finished inbound event")
}

func (s *Store) SaveInboundAssessment(ctx context.Context, principalID, id, assessment, message string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("save inbound event assessment: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE inbound_events SET assessment = ?, assessed_message = NULLIF(?, '')
		 WHERE principal_id = ? AND id = ? AND outcome = 'pending'`, assessment, message, principalID, id)
	if err != nil {
		return false, fmt.Errorf("save inbound event assessment: %w", err)
	}
	return affectedOne(result, "saved inbound event assessment")
}

func (s *Store) PruneInboundEvents(ctx context.Context, before time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("prune inbound events: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM inbound_events WHERE received_at < ?`, formatTime(before)); err != nil {
		return fmt.Errorf("prune inbound events: %w", err)
	}
	return nil
}

func (s *Store) AddEventTokenMessage(ctx context.Context, message events.TokenMessage) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record event token message: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO event_token_messages (principal_id, chat_id, message_id, delete_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (principal_id, chat_id, message_id) DO UPDATE SET delete_at = excluded.delete_at`,
		message.PrincipalID, message.ChatID, message.MessageID, formatTime(message.DeleteAt),
	); err != nil {
		return fmt.Errorf("record event token message: %w", err)
	}
	return nil
}

func (s *Store) DueEventTokenMessages(ctx context.Context, at time.Time) ([]events.TokenMessage, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list due event token messages: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT principal_id, chat_id, message_id, delete_at FROM event_token_messages WHERE delete_at <= ? ORDER BY delete_at`, formatTime(at))
	if err != nil {
		return nil, fmt.Errorf("list due event token messages: %w", err)
	}
	defer rows.Close()
	var due []events.TokenMessage
	for rows.Next() {
		var message events.TokenMessage
		var deleteAt string
		if err := rows.Scan(&message.PrincipalID, &message.ChatID, &message.MessageID, &deleteAt); err != nil {
			return nil, fmt.Errorf("scan event token message: %w", err)
		}
		if message.DeleteAt, err = time.Parse(time.RFC3339Nano, deleteAt); err != nil {
			return nil, fmt.Errorf("parse event token message time: %w", err)
		}
		due = append(due, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due event token messages: %w", err)
	}
	return due, nil
}

func (s *Store) RemoveEventTokenMessage(ctx context.Context, message events.TokenMessage) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("forget event token message: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM event_token_messages WHERE principal_id = ? AND chat_id = ? AND message_id = ?`,
		message.PrincipalID, message.ChatID, message.MessageID,
	); err != nil {
		return fmt.Errorf("forget event token message: %w", err)
	}
	return nil
}

func scanEventSource(row rowScanner) (events.Source, error) {
	var source events.Source
	var createdAt string
	var lastUsedAt, rotatedAt sql.NullString
	if err := row.Scan(&source.ID, &source.PrincipalID, &source.Name, &source.Mode, &source.TokenHash, &source.Enabled,
		&createdAt, &lastUsedAt, &rotatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return events.Source{}, err
		}
		return events.Source{}, fmt.Errorf("scan event source: %w", err)
	}
	var err error
	if source.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return events.Source{}, fmt.Errorf("parse event source time: %w", err)
	}
	for _, field := range []struct {
		value  sql.NullString
		target *time.Time
	}{{lastUsedAt, &source.LastUsedAt}, {rotatedAt, &source.RotatedAt}} {
		if !field.value.Valid {
			continue
		}
		if *field.target, err = time.Parse(time.RFC3339Nano, field.value.String); err != nil {
			return events.Source{}, fmt.Errorf("parse event source time: %w", err)
		}
	}
	return source, nil
}

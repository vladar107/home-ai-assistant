package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/turn"
)

// SaveTurnTrace stores a turn's trace. A trace already kept under the key, such as a background
// task's run before its report run, gains the new calls after its own and takes the newer run's
// kind, finish time, and model; outside and private content once read stays marked.
func (s *Store) SaveTurnTrace(ctx context.Context, trace turn.Trace) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save turn trace: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin turn trace: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO turn_traces (principal_id, trace_key, kind, model, effort, detail, external, private, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (principal_id, trace_key) DO UPDATE SET
		     kind = excluded.kind,
		     model = CASE WHEN excluded.model = '' THEN turn_traces.model ELSE excluded.model END,
		     effort = CASE WHEN excluded.model = '' THEN turn_traces.effort ELSE excluded.effort END,
		     detail = CASE WHEN excluded.detail = '' THEN turn_traces.detail ELSE excluded.detail END,
		     external = MAX(turn_traces.external, excluded.external),
		     private = MAX(turn_traces.private, excluded.private),
		     finished_at = excluded.finished_at`,
		trace.PrincipalID, trace.Key, trace.Kind, trace.Model, trace.Effort, trace.Detail, trace.External, trace.Private,
		formatTime(trace.StartedAt), formatTime(trace.FinishedAt),
	); err != nil {
		return fmt.Errorf("save turn trace: %w", err)
	}
	var offset int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(sequence), 0) FROM turn_trace_calls WHERE principal_id = ? AND trace_key = ?`,
		trace.PrincipalID, trace.Key,
	).Scan(&offset); err != nil {
		return fmt.Errorf("read turn trace calls: %w", err)
	}
	for index, call := range trace.Calls {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO turn_trace_calls (principal_id, trace_key, sequence, call_id, tool, outcome, duration_ms, result_bytes, label)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			trace.PrincipalID, trace.Key, offset+index+1, call.CallID, call.Tool, call.Outcome,
			max(call.Duration.Milliseconds(), 0), max(call.ResultBytes, 0), call.Label,
		); err != nil {
			return fmt.Errorf("save turn trace call: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit turn trace: %w", err)
	}
	return nil
}

// RecordTurnMessage links a delivered Telegram message to the principal's trace under the key. A key
// without a trace, such as a reminder's run, links nothing.
func (s *Store) RecordTurnMessage(ctx context.Context, principalID, key string, chatID, messageID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record turn message: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO turn_messages (principal_id, chat_id, telegram_message_id, trace_key)
		 SELECT ?, ?, ?, ?
		 WHERE EXISTS (SELECT 1 FROM turn_traces WHERE principal_id = ? AND trace_key = ?)
		 ON CONFLICT (principal_id, chat_id, telegram_message_id) DO UPDATE SET trace_key = excluded.trace_key`,
		principalID, chatID, messageID, key, principalID, key,
	); err != nil {
		return fmt.Errorf("record turn message: %w", err)
	}
	return nil
}

// TurnTraceByMessage returns the trace of the turn that delivered the principal's message, with its
// calls in order. Nothing is found for another principal's message, nor for one whose trace was
// pruned or never kept.
func (s *Store) TurnTraceByMessage(ctx context.Context, principalID string, chatID, messageID int64) (turn.Trace, bool, error) {
	if s == nil || s.db == nil {
		return turn.Trace{}, false, fmt.Errorf("load turn trace: SQLite store is not open")
	}
	trace := turn.Trace{PrincipalID: principalID}
	var started, finished string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT t.trace_key, t.kind, t.model, t.effort, t.detail, t.external, t.private, t.started_at, t.finished_at
		 FROM turn_messages AS m
		 JOIN turn_traces AS t ON t.principal_id = m.principal_id AND t.trace_key = m.trace_key
		 WHERE m.principal_id = ? AND m.chat_id = ? AND m.telegram_message_id = ?`,
		principalID, chatID, messageID,
	).Scan(&trace.Key, &trace.Kind, &trace.Model, &trace.Effort, &trace.Detail, &trace.External, &trace.Private, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return turn.Trace{}, false, nil
	}
	if err != nil {
		return turn.Trace{}, false, fmt.Errorf("load turn trace: %w", err)
	}
	if trace.StartedAt, err = time.Parse(conversationTimeLayout, started); err != nil {
		return turn.Trace{}, false, fmt.Errorf("parse turn trace start: %w", err)
	}
	if trace.FinishedAt, err = time.Parse(conversationTimeLayout, finished); err != nil {
		return turn.Trace{}, false, fmt.Errorf("parse turn trace finish: %w", err)
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT sequence, call_id, tool, outcome, duration_ms, result_bytes, label
		 FROM turn_trace_calls WHERE principal_id = ? AND trace_key = ? ORDER BY sequence`,
		principalID, trace.Key,
	)
	if err != nil {
		return turn.Trace{}, false, fmt.Errorf("load turn trace calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var call turn.TraceCall
		var duration int64
		if err := rows.Scan(&call.Sequence, &call.CallID, &call.Tool, &call.Outcome, &duration, &call.ResultBytes, &call.Label); err != nil {
			return turn.Trace{}, false, fmt.Errorf("scan turn trace call: %w", err)
		}
		call.Duration = time.Duration(duration) * time.Millisecond
		trace.Calls = append(trace.Calls, call)
	}
	if err := rows.Err(); err != nil {
		return turn.Trace{}, false, fmt.Errorf("load turn trace calls: %w", err)
	}
	return trace, true, nil
}

// PruneTurnTraces removes every trace finished before before, with its calls and linked messages,
// and the traces of checks and assessments finished before unlinkedBefore that no delivered message
// is linked to: most checks raise no alert.
func (s *Store) PruneTurnTraces(ctx context.Context, before, unlinkedBefore time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("prune turn traces: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`DELETE FROM turn_traces WHERE finished_at < ?
		    OR (kind IN ('check', 'assess') AND finished_at < ? AND NOT EXISTS (
		        SELECT 1 FROM turn_messages AS m WHERE m.principal_id = turn_traces.principal_id AND m.trace_key = turn_traces.trace_key))`,
		formatTime(before), formatTime(unlinkedBefore),
	); err != nil {
		return fmt.Errorf("prune turn traces: %w", err)
	}
	return nil
}

// ReplySession returns the runtime session a delivered chat reply of the principal belongs to and
// when it was sent, so the conversation's history can say what the user asked before it.
func (s *Store) ReplySession(ctx context.Context, principalID string, messageID int64) (string, time.Time, bool, error) {
	if s == nil || s.db == nil {
		return "", time.Time{}, false, fmt.Errorf("query reply session: SQLite store is not open")
	}
	var runtimeSessionID, sent string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT session.runtime_session_id, reply.created_at
		 FROM conversation_reply_messages AS reply
		 JOIN conversation_sessions AS session ON session.id = reply.session_id AND session.principal_id = reply.principal_id
		 WHERE reply.principal_id = ? AND reply.telegram_message_id = ?`,
		principalID, messageID,
	).Scan(&runtimeSessionID, &sent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("query reply session: %w", err)
	}
	sentAt, err := time.Parse(conversationTimeLayout, sent)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("query reply session: invalid send time")
	}
	return runtimeSessionID, sentAt, true, nil
}

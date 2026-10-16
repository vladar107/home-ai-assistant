package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

const conversationTimeLayout = "2006-01-02T15:04:05.000Z"

// sessionColumns reads a session with its last activity: the newest request it took, or its
// creation when it took none, and the name of its tab when the registry knows it. Queries order by
// the activity, the fourth column.
const sessionColumns = `session.id, session.runtime_session_id, session.status = 'active',
	COALESCE((SELECT MAX(request.created_at) FROM conversation_requests AS request
	          WHERE request.session_id = session.id), session.created_at),
	session.private_at IS NOT NULL, session.thread_id,
	COALESCE((SELECT tab.name FROM telegram_tabs AS tab
	          WHERE tab.principal_id = session.principal_id AND tab.thread_id = session.thread_id), '')`

// ReferencedSessions returns the runtime session IDs of every conversation and every task kept.
func (s *Store) ReferencedSessions(ctx context.Context) (map[string]struct{}, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list referenced sessions: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT runtime_session_id FROM conversation_sessions UNION SELECT session_id FROM tasks`)
	if err != nil {
		return nil, fmt.Errorf("list referenced sessions: %w", err)
	}
	defer rows.Close()
	referenced := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan referenced session: %w", err)
		}
		referenced[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list referenced sessions: %w", err)
	}
	return referenced, nil
}

func (s *Store) ConversationSessions(ctx context.Context, principalID string, limit int) ([]conversation.SessionRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list conversation sessions: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+sessionColumns+`
		 FROM conversation_sessions AS session
		 WHERE session.principal_id = ?
		 ORDER BY 4 DESC, session.id LIMIT ?`,
		principalID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query conversation sessions: %w", err)
	}
	defer rows.Close()

	var records []conversation.SessionRecord
	for rows.Next() {
		record, err := scanSessionRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversation sessions: %w", err)
	}
	return records, nil
}

func (s *Store) SwitchConversation(ctx context.Context, lane conversation.Lane, sessionID string) (conversation.SwitchResult, error) {
	if s == nil || s.db == nil {
		return conversation.SwitchResult{}, fmt.Errorf("switch conversation: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return conversation.SwitchResult{}, fmt.Errorf("begin conversation switch: %w", err)
	}
	defer tx.Rollback()

	pending, err := lanePending(ctx, tx, lane)
	if err != nil {
		return conversation.SwitchResult{}, err
	}
	if pending {
		return conversation.SwitchResult{Busy: true}, nil
	}

	outcome, err := switchActiveSession(ctx, tx, lane, sessionID)
	if err != nil {
		return conversation.SwitchResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return conversation.SwitchResult{}, fmt.Errorf("commit conversation switch: %w", err)
	}
	return conversation.SwitchResult{
		Found:     outcome.found,
		Elsewhere: outcome.elsewhere,
		Switched:  outcome.switched,
		Session:   outcome.session,
		Previous:  outcome.previous,
	}, nil
}

func (s *Store) SessionMessageIDs(ctx context.Context, principalID, sessionID string) ([]int64, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list session messages: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT telegram_message_id FROM conversation_requests
		 WHERE principal_id = ? AND session_id = ? AND telegram_message_id IS NOT NULL
		 UNION
		 SELECT telegram_message_id FROM conversation_reply_messages
		 WHERE principal_id = ? AND session_id = ?
		 ORDER BY 1`,
		principalID,
		sessionID,
		principalID,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list session messages: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session message: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list session messages: %w", err)
	}
	return ids, nil
}

func (s *Store) RecordConversationReply(ctx context.Context, principalID, sessionID, requestID string, messageID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record conversation reply: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO conversation_reply_messages (principal_id, telegram_message_id, session_id, request_id)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (principal_id, telegram_message_id)
		 DO UPDATE SET session_id = excluded.session_id, request_id = excluded.request_id`,
		principalID,
		messageID,
		sessionID,
		requestID,
	); err != nil {
		return fmt.Errorf("record conversation reply: %w", err)
	}
	return nil
}

// ReplyOrigin returns the session and request a delivered Telegram message came from, when it was
// sent, and the tab it was sent to. The request is empty for a message delivered before requests
// were recorded, and nothing is found for a message that is not one of the principal's recorded
// replies.
func (s *Store) ReplyOrigin(ctx context.Context, principalID string, messageID int64) (conversation.ReplyOrigin, bool, error) {
	if s == nil || s.db == nil {
		return conversation.ReplyOrigin{}, false, fmt.Errorf("query reply origin: SQLite store is not open")
	}
	var origin conversation.ReplyOrigin
	var sent string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT reply.session_id, reply.request_id, reply.created_at,
		        COALESCE((SELECT request.thread_id FROM conversation_requests AS request WHERE request.id = reply.request_id),
		                 (SELECT session.thread_id FROM conversation_sessions AS session WHERE session.id = reply.session_id), 0)
		 FROM conversation_reply_messages AS reply WHERE reply.principal_id = ? AND reply.telegram_message_id = ?`,
		principalID,
		messageID,
	).Scan(&origin.SessionID, &origin.RequestID, &sent, &origin.ThreadID)
	if errors.Is(err, sql.ErrNoRows) {
		return conversation.ReplyOrigin{}, false, nil
	}
	if err != nil {
		return conversation.ReplyOrigin{}, false, fmt.Errorf("query reply origin: %w", err)
	}
	origin.SentAt, err = time.Parse(conversationTimeLayout, sent)
	if err != nil {
		return conversation.ReplyOrigin{}, false, fmt.Errorf("query reply origin: invalid send time")
	}
	return origin, true, nil
}

// SetReplyFeedback keeps the latest reaction class and emoji for a recorded reply. A message that
// is not a recorded reply is rejected by the foreign key.
func (s *Store) SetReplyFeedback(ctx context.Context, principalID string, messageID int64, class, emoji string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("set reply feedback: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO reply_feedback (principal_id, telegram_message_id, class, emoji)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (principal_id, telegram_message_id)
		 DO UPDATE SET class = excluded.class, emoji = excluded.emoji,
		               updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		principalID,
		messageID,
		class,
		emoji,
	); err != nil {
		return fmt.Errorf("set reply feedback: %w", err)
	}
	return nil
}

// DeleteReplyFeedback removes the reaction kept for a reply, if any.
func (s *Store) DeleteReplyFeedback(ctx context.Context, principalID string, messageID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("delete reply feedback: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`DELETE FROM reply_feedback WHERE principal_id = ? AND telegram_message_id = ?`,
		principalID,
		messageID,
	); err != nil {
		return fmt.Errorf("delete reply feedback: %w", err)
	}
	return nil
}

// ClaimReplyReconsider records that a reconsider turn was started for the key and reports whether
// this call was the first to do so.
func (s *Store) ClaimReplyReconsider(ctx context.Context, principalID, key string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("claim reply reconsider: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`INSERT INTO reply_reconsiders (principal_id, reconsider_key) VALUES (?, ?)
		 ON CONFLICT (principal_id, reconsider_key) DO NOTHING`,
		principalID,
		key,
	)
	if err != nil {
		return false, fmt.Errorf("claim reply reconsider: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim reply reconsider: %w", err)
	}
	return affected > 0, nil
}

// ReplyReconsidered reports whether a reconsider turn was already started for the key.
func (s *Store) ReplyReconsidered(ctx context.Context, principalID, key string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("query reply reconsider: SQLite store is not open")
	}
	var exists int
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM reply_reconsiders WHERE principal_id = ? AND reconsider_key = ?)`,
		principalID,
		key,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("query reply reconsider: %w", err)
	}
	return exists != 0, nil
}

type switchOutcome struct {
	found bool
	// elsewhere is set when the target is the active conversation of another tab.
	elsewhere bool
	// switched is set when the target was closed and is now active.
	switched bool
	session  conversation.SessionRecord
	previous string
}

// switchToRepliedSession returns the lane to the session whose answer the message replied to. A
// reply to anything else, including another principal's message, changes nothing, and so does one
// to a conversation another tab holds.
func switchToRepliedSession(ctx context.Context, tx *sql.Tx, lane conversation.Lane, messageID int64) (switchOutcome, error) {
	var sessionID string
	err := tx.QueryRowContext(
		ctx,
		`SELECT session_id FROM conversation_reply_messages WHERE principal_id = ? AND telegram_message_id = ?`,
		lane.PrincipalID,
		messageID,
	).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return switchOutcome{}, nil
	}
	if err != nil {
		return switchOutcome{}, fmt.Errorf("query replied conversation: %w", err)
	}
	return switchActiveSession(ctx, tx, lane, sessionID)
}

// switchActiveSession makes the principal's own session the lane's active one, moving it into the
// lane's tab, and closes the session it replaces as switched. A session active in another tab
// stays there. The target's last activity is read before the switch touches anything.
func switchActiveSession(ctx context.Context, tx *sql.Tx, lane conversation.Lane, sessionID string) (switchOutcome, error) {
	principalID := lane.PrincipalID
	target, err := scanSessionRecord(tx.QueryRowContext(
		ctx,
		`SELECT `+sessionColumns+`
		 FROM conversation_sessions AS session
		 WHERE session.id = ? AND session.principal_id = ?`,
		sessionID,
		principalID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return switchOutcome{}, nil
	}
	if err != nil {
		return switchOutcome{}, err
	}
	if target.Active {
		return switchOutcome{found: true, elsewhere: target.ThreadID != lane.ThreadID, session: target}, nil
	}

	previous, _, err := loadActiveConversationSession(ctx, tx, lane)
	if err != nil {
		return switchOutcome{}, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE conversation_sessions
		 SET status = 'closed', close_reason = 'switched',
		     closed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND thread_id = ? AND status = 'active'`,
		principalID,
		lane.ThreadID,
	); err != nil {
		return switchOutcome{}, fmt.Errorf("close switched conversation: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE conversation_sessions
		 SET status = 'active', close_reason = NULL, closed_at = NULL, thread_id = ?,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE id = ? AND principal_id = ?`,
		lane.ThreadID,
		target.ID,
		principalID,
	); err != nil {
		return switchOutcome{}, fmt.Errorf("reopen conversation: %w", err)
	}
	target.Active, target.ThreadID, target.TabName = true, lane.ThreadID, ""
	return switchOutcome{found: true, switched: true, session: target, previous: previous.ID}, nil
}

func scanSessionRecord(row rowScanner) (conversation.SessionRecord, error) {
	var record conversation.SessionRecord
	var lastActive string
	if err := row.Scan(&record.ID, &record.RuntimeSessionID, &record.Active, &lastActive, &record.Private, &record.ThreadID, &record.TabName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return record, err
		}
		return record, fmt.Errorf("scan conversation session: %w", err)
	}
	var err error
	if record.LastActive, err = time.Parse(conversationTimeLayout, lastActive); err != nil {
		return record, fmt.Errorf("parse conversation session activity time: %w", err)
	}
	return record, nil
}

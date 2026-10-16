package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

var _ conversation.Repository = (*Store)(nil)

func (s *Store) EnqueueConversation(
	ctx context.Context,
	input conversation.EnqueueInput,
	candidate conversation.Session,
	requestID string,
	attachments []attachment.Record,
) (conversation.EnqueueResult, error) {
	if s == nil || s.db == nil {
		return conversation.EnqueueResult{}, fmt.Errorf("enqueue conversation: SQLite store is not open")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return conversation.EnqueueResult{}, fmt.Errorf("begin conversation enqueue: %w", err)
	}
	defer tx.Rollback()

	existing, found, err := loadConversationByUpdate(ctx, tx, input.BotUserID, input.TelegramUpdateID)
	if err != nil {
		return conversation.EnqueueResult{}, err
	}
	if found {
		if existing.PrincipalID != input.PrincipalID || existing.ChatID != input.ChatID {
			return conversation.EnqueueResult{}, fmt.Errorf("Telegram update is already owned by another conversation")
		}
		return conversation.EnqueueResult{Request: existing}, nil
	}

	var switched switchOutcome
	if input.ReplyToMessageID > 0 {
		switched, err = switchToRepliedSession(ctx, tx, conversation.Lane{PrincipalID: input.PrincipalID, ThreadID: input.ThreadID}, input.ReplyToMessageID)
		if err != nil {
			return conversation.EnqueueResult{}, err
		}
	}
	session, found, err := loadActiveConversationSession(ctx, tx, conversation.Lane{PrincipalID: input.PrincipalID, ThreadID: input.ThreadID})
	if err != nil {
		return conversation.EnqueueResult{}, err
	}
	if !found && input.ThreadID > 0 {
		session, found, err = reopenGoneTab(ctx, tx, conversation.Lane{PrincipalID: input.PrincipalID, ThreadID: input.ThreadID})
		if err != nil {
			return conversation.EnqueueResult{}, err
		}
	}
	if !found {
		session = candidate
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, thread_id) VALUES (?, ?, ?, ?)`,
			session.ID,
			session.PrincipalID,
			session.RuntimeSessionID,
			input.ThreadID,
		); err != nil {
			return conversation.EnqueueResult{}, fmt.Errorf("create conversation session: %w", err)
		}
	}

	pending, err := lanePending(ctx, tx, conversation.Lane{PrincipalID: input.PrincipalID, ThreadID: input.ThreadID})
	if err != nil {
		return conversation.EnqueueResult{}, err
	}
	if pending {
		return conversation.EnqueueResult{Busy: true}, nil
	}

	request := conversation.Request{
		ID:                requestID,
		PrincipalID:       input.PrincipalID,
		ThreadID:          input.ThreadID,
		ChatID:            input.ChatID,
		TelegramMessageID: input.TelegramMessageID,
		SessionID:         session.ID,
		RuntimeSessionID:  session.RuntimeSessionID,
		Message:           input.Message,
		State:             conversation.RequestStateQueued,
	}
	var replyTo any
	if input.ReplyToMessageID > 0 {
		request.ReplyToMessageID = input.ReplyToMessageID
		replyTo = input.ReplyToMessageID
	}
	var resumedFrom any
	if switched.switched {
		request.ResumedFrom = switched.session.LastActive
		resumedFrom = request.ResumedFrom.Format(conversationTimeLayout)
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO conversation_requests (
		    id, bot_user_id, telegram_update_id, telegram_message_id, principal_id, chat_id, session_id, message,
		    resumed_from, reply_to_message_id, thread_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		request.ID,
		input.BotUserID,
		input.TelegramUpdateID,
		request.TelegramMessageID,
		request.PrincipalID,
		request.ChatID,
		request.SessionID,
		request.Message,
		resumedFrom,
		replyTo,
		request.ThreadID,
	); err != nil {
		return conversation.EnqueueResult{}, fmt.Errorf("create conversation request: %w", err)
	}
	// The request and its attachments become visible together, so a turn can never start on a
	// message whose files are not yet stored.
	if err := insertAttachments(ctx, tx, request.SessionID, attachments); err != nil {
		return conversation.EnqueueResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return conversation.EnqueueResult{}, fmt.Errorf("commit conversation enqueue: %w", err)
	}
	return conversation.EnqueueResult{Request: request, Created: true, SwitchedFrom: switched.previous}, nil
}

func (s *Store) RecoverConversations(ctx context.Context) ([]conversation.Lane, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("recover conversations: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE conversation_requests
		 SET state = 'ready', message = '', response_text = ?, error_kind = 'runtime'
		 WHERE state = 'running'`,
		conversation.InterruptedReply,
	); err != nil {
		return nil, fmt.Errorf("recover interrupted conversations: %w", err)
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE conversation_requests SET state = 'ready' WHERE state = 'delivering'`,
	); err != nil {
		return nil, fmt.Errorf("requeue interrupted conversation deliveries: %w", err)
	}

	rows, err := s.db.QueryContext(
		ctx,
		`SELECT DISTINCT principal_id, thread_id FROM conversation_requests
		 WHERE state IN ('queued', 'ready') ORDER BY principal_id, thread_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list recoverable conversations: %w", err)
	}
	defer rows.Close()

	var lanes []conversation.Lane
	for rows.Next() {
		var lane conversation.Lane
		if err := rows.Scan(&lane.PrincipalID, &lane.ThreadID); err != nil {
			return nil, fmt.Errorf("scan recoverable conversation: %w", err)
		}
		lanes = append(lanes, lane)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable conversations: %w", err)
	}
	return lanes, nil
}

func (s *Store) NextConversationRequest(ctx context.Context, lane conversation.Lane) (conversation.Request, bool, error) {
	if s == nil || s.db == nil {
		return conversation.Request{}, false, fmt.Errorf("load next conversation request: SQLite store is not open")
	}
	request, err := scanConversationRequest(s.db.QueryRowContext(
		ctx,
		`SELECT request.id, request.principal_id, request.chat_id, COALESCE(request.telegram_message_id, 0),
		        request.session_id, session.runtime_session_id, request.message, request.state,
		        COALESCE(request.response_text, ''), COALESCE(request.error_kind, ''), request.created_at,
		        COALESCE(request.resumed_from, ''), COALESCE(request.reply_to_message_id, 0),
		        request.thread_id, `+requestOpensSession+`
		 FROM conversation_requests AS request
		 JOIN conversation_sessions AS session
		   ON session.id = request.session_id AND session.principal_id = request.principal_id
		 WHERE request.principal_id = ? AND request.thread_id = ? AND request.state IN ('queued', 'ready')
		 ORDER BY request.created_at, request.id LIMIT 1`,
		lane.PrincipalID,
		lane.ThreadID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return conversation.Request{}, false, nil
	}
	if err != nil {
		return conversation.Request{}, false, fmt.Errorf("query next conversation request: %w", err)
	}
	return request, true, nil
}

func (s *Store) ClaimConversationRequest(ctx context.Context, requestID string) (bool, error) {
	return s.changeConversationState(
		ctx,
		`UPDATE conversation_requests
		 SET state = 'running', started_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE id = ? AND state = 'queued'`,
		"claim conversation request",
		requestID,
	)
}

func (s *Store) SaveConversationResponse(ctx context.Context, requestID, response, errorKind string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("save conversation response: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE conversation_requests
		 SET state = 'ready', response_text = ?, message = '', error_kind = NULLIF(?, '')
		 WHERE id = ? AND state = 'running'`,
		response,
		errorKind,
		requestID,
	)
	if err != nil {
		return false, fmt.Errorf("save conversation response: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read saved conversation response count: %w", err)
	}
	return changed == 1, nil
}

func (s *Store) MarkConversationDelivered(ctx context.Context, requestID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("mark conversation delivered: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE conversation_requests
		 SET state = 'delivered', response_text = NULL, message = '',
		     finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE id = ? AND state = 'delivering'`,
		requestID,
	); err != nil {
		return fmt.Errorf("mark conversation delivered: %w", err)
	}
	return nil
}

func (s *Store) ClaimConversationDelivery(ctx context.Context, requestID string) (bool, error) {
	return s.changeConversationState(
		ctx,
		`UPDATE conversation_requests SET state = 'delivering' WHERE id = ? AND state = 'ready'`,
		"claim conversation delivery",
		requestID,
	)
}

func (s *Store) ReleaseConversationDelivery(ctx context.Context, requestID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("release conversation delivery: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE conversation_requests SET state = 'ready' WHERE id = ? AND state = 'delivering'`,
		requestID,
	); err != nil {
		return fmt.Errorf("release conversation delivery: %w", err)
	}
	return nil
}

func (s *Store) CancelConversationDelivery(ctx context.Context, requestID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("cancel conversation delivery: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE conversation_requests
		 SET state = 'cancelled', message = '', response_text = NULL,
		     finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE id = ? AND state = 'delivering'`,
		requestID,
	); err != nil {
		return fmt.Errorf("cancel conversation delivery: %w", err)
	}
	return nil
}

func (s *Store) CancelPendingConversation(ctx context.Context, lane conversation.Lane) (bool, error) {
	return s.changeConversationState(
		ctx,
		`UPDATE conversation_requests
		 SET state = 'cancelled', message = '', response_text = NULL,
		     finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND thread_id = ? AND state IN ('queued', 'running', 'ready', 'delivering')`,
		"cancel pending conversation",
		lane.PrincipalID,
		lane.ThreadID,
	)
}

func (s *Store) CancelPrincipalConversations(ctx context.Context, principalID string) (bool, error) {
	return s.changeConversationState(
		ctx,
		`UPDATE conversation_requests
		 SET state = 'cancelled', message = '', response_text = NULL,
		     finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND state IN ('queued', 'running', 'ready', 'delivering')`,
		"cancel principal conversations",
		principalID,
	)
}

func (s *Store) ResetConversation(ctx context.Context, lane conversation.Lane) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("reset conversation: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin conversation reset: %w", err)
	}
	defer tx.Rollback()

	session, _, err := loadActiveConversationSession(ctx, tx, lane)
	if err != nil {
		return "", false, err
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE conversation_requests
		 SET state = 'cancelled', message = '', response_text = NULL,
		     finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND thread_id = ? AND state IN ('queued', 'running', 'ready', 'delivering')`,
		lane.PrincipalID,
		lane.ThreadID,
	); err != nil {
		return "", false, fmt.Errorf("cancel requests during conversation reset: %w", err)
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE conversation_sessions
		 SET status = 'closed', close_reason = 'user_reset',
		     closed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND thread_id = ? AND status = 'active'`,
		lane.PrincipalID,
		lane.ThreadID,
	)
	if err != nil {
		return "", false, fmt.Errorf("close active conversation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("read closed conversation count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit conversation reset: %w", err)
	}
	return session.ID, changed == 1, nil
}

func (s *Store) CloseLane(ctx context.Context, lane conversation.Lane, reason string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("close conversation lane: SQLite store is not open")
	}
	if reason != conversation.CloseTabGone && reason != conversation.CloseTabClosed {
		return "", false, fmt.Errorf("close conversation lane: unknown reason %q", reason)
	}
	var sessionID string
	err := s.db.QueryRowContext(
		ctx,
		`UPDATE conversation_sessions
		 SET status = 'closed', close_reason = ?,
		     closed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND thread_id = ? AND status = 'active'
		 RETURNING id`,
		reason,
		lane.PrincipalID,
		lane.ThreadID,
	).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("close conversation lane: %w", err)
	}
	return sessionID, true, nil
}

// reopenGoneTab makes the lane's most recent conversation closed as tab_gone active again, so a
// message in a tab that was taken for deleted continues it. When another tab holds a conversation
// of the same subject, nothing is reopened and the message starts a fresh one.
func reopenGoneTab(ctx context.Context, tx *sql.Tx, lane conversation.Lane) (conversation.Session, bool, error) {
	var session conversation.Session
	var subjectTaken bool
	err := tx.QueryRowContext(
		ctx,
		`SELECT session.id, session.principal_id, session.runtime_session_id,
		        session.subject IS NOT NULL AND EXISTS (
		            SELECT 1 FROM conversation_sessions AS other
		            WHERE other.principal_id = session.principal_id AND other.subject = session.subject
		              AND other.status = 'active')
		 FROM conversation_sessions AS session
		 WHERE session.principal_id = ? AND session.thread_id = ? AND session.status = 'closed'
		   AND session.close_reason = ?
		 ORDER BY session.closed_at DESC, session.id LIMIT 1`,
		lane.PrincipalID,
		lane.ThreadID,
		conversation.CloseTabGone,
	).Scan(&session.ID, &session.PrincipalID, &session.RuntimeSessionID, &subjectTaken)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && subjectTaken) {
		return conversation.Session{}, false, nil
	}
	if err != nil {
		return conversation.Session{}, false, fmt.Errorf("query gone tab conversation: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE conversation_sessions
		 SET status = 'active', close_reason = NULL, closed_at = NULL,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE id = ? AND principal_id = ?`,
		session.ID,
		session.PrincipalID,
	); err != nil {
		return conversation.Session{}, false, fmt.Errorf("reopen gone tab conversation: %w", err)
	}
	return session, true, nil
}

// requestOpensSession is whether no earlier request joined the request's session.
const requestOpensSession = `NOT EXISTS (
		     SELECT 1 FROM conversation_requests AS earlier
		     WHERE earlier.session_id = request.session_id AND earlier.principal_id = request.principal_id
		       AND (earlier.created_at < request.created_at OR (earlier.created_at = request.created_at AND earlier.id < request.id))
		 )`

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadConversationByUpdate(ctx context.Context, db queryRower, botUserID, updateID int64) (conversation.Request, bool, error) {
	request, err := scanConversationRequest(db.QueryRowContext(
		ctx,
		`SELECT request.id, request.principal_id, request.chat_id, COALESCE(request.telegram_message_id, 0),
		        request.session_id, session.runtime_session_id, request.message, request.state,
		        COALESCE(request.response_text, ''), COALESCE(request.error_kind, ''), request.created_at,
		        COALESCE(request.resumed_from, ''), COALESCE(request.reply_to_message_id, 0),
		        request.thread_id, `+requestOpensSession+`
		 FROM conversation_requests AS request
		 JOIN conversation_sessions AS session
		   ON session.id = request.session_id AND session.principal_id = request.principal_id
		 WHERE request.bot_user_id = ? AND request.telegram_update_id = ?`,
		botUserID,
		updateID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return conversation.Request{}, false, nil
	}
	if err != nil {
		return conversation.Request{}, false, fmt.Errorf("query conversation by Telegram update: %w", err)
	}
	return request, true, nil
}

func loadActiveConversationSession(ctx context.Context, db queryRower, lane conversation.Lane) (conversation.Session, bool, error) {
	var session conversation.Session
	err := db.QueryRowContext(
		ctx,
		`SELECT id, principal_id, runtime_session_id
		 FROM conversation_sessions WHERE principal_id = ? AND thread_id = ? AND status = 'active'`,
		lane.PrincipalID,
		lane.ThreadID,
	).Scan(&session.ID, &session.PrincipalID, &session.RuntimeSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return conversation.Session{}, false, nil
	}
	if err != nil {
		return conversation.Session{}, false, fmt.Errorf("query active conversation session: %w", err)
	}
	return session, true, nil
}

// lanePending reports whether the lane has a turn queued, running, or waiting for delivery.
func lanePending(ctx context.Context, db queryRower, lane conversation.Lane) (bool, error) {
	var pending int
	if err := db.QueryRowContext(
		ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM conversation_requests
		    WHERE principal_id = ? AND thread_id = ? AND state IN ('queued', 'running', 'ready', 'delivering')
		)`,
		lane.PrincipalID,
		lane.ThreadID,
	).Scan(&pending); err != nil {
		return false, fmt.Errorf("check pending conversation: %w", err)
	}
	return pending != 0, nil
}

func scanConversationRequest(row *sql.Row) (conversation.Request, error) {
	var request conversation.Request
	var createdAt, resumedFrom string
	err := row.Scan(
		&request.ID,
		&request.PrincipalID,
		&request.ChatID,
		&request.TelegramMessageID,
		&request.SessionID,
		&request.RuntimeSessionID,
		&request.Message,
		&request.State,
		&request.ResponseText,
		&request.ErrorKind,
		&createdAt,
		&resumedFrom,
		&request.ReplyToMessageID,
		&request.ThreadID,
		&request.OpensSession,
	)
	if err != nil {
		return request, err
	}
	request.CreatedAt, err = time.Parse(conversationTimeLayout, createdAt)
	if err != nil {
		return request, fmt.Errorf("parse conversation request creation time: %w", err)
	}
	if resumedFrom != "" {
		request.ResumedFrom, err = time.Parse(conversationTimeLayout, resumedFrom)
		if err != nil {
			return request, fmt.Errorf("parse conversation request resume time: %w", err)
		}
	}
	return request, nil
}

func (s *Store) changeConversationState(ctx context.Context, query, operation string, arguments ...any) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("%s: SQLite store is not open", operation)
	}
	result, err := s.db.ExecContext(ctx, query, arguments...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", operation, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read %s count: %w", operation, err)
	}
	return changed > 0, nil
}

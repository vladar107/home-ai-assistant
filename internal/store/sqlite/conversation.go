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

	session, found, err := loadActiveConversationSession(ctx, tx, input.PrincipalID)
	if err != nil {
		return conversation.EnqueueResult{}, err
	}
	if !found {
		session = candidate
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id) VALUES (?, ?, ?)`,
			session.ID,
			session.PrincipalID,
			session.RuntimeSessionID,
		); err != nil {
			return conversation.EnqueueResult{}, fmt.Errorf("create conversation session: %w", err)
		}
	}

	var pending int
	err = tx.QueryRowContext(
		ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM conversation_requests
		    WHERE principal_id = ? AND state IN ('queued', 'running', 'ready', 'delivering')
		)`,
		input.PrincipalID,
	).Scan(&pending)
	if err != nil {
		return conversation.EnqueueResult{}, fmt.Errorf("check pending conversation: %w", err)
	}
	if pending != 0 {
		return conversation.EnqueueResult{Busy: true}, nil
	}

	request := conversation.Request{
		ID:                requestID,
		PrincipalID:       input.PrincipalID,
		ChatID:            input.ChatID,
		TelegramMessageID: input.TelegramMessageID,
		SessionID:         session.ID,
		RuntimeSessionID:  session.RuntimeSessionID,
		Message:           input.Message,
		State:             conversation.RequestStateQueued,
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO conversation_requests (
		    id, bot_user_id, telegram_update_id, telegram_message_id, principal_id, chat_id, session_id, message
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		request.ID,
		input.BotUserID,
		input.TelegramUpdateID,
		request.TelegramMessageID,
		request.PrincipalID,
		request.ChatID,
		request.SessionID,
		request.Message,
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
	return conversation.EnqueueResult{Request: request, Created: true}, nil
}

func (s *Store) RecoverConversations(ctx context.Context) ([]string, error) {
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
		`UPDATE conversation_sessions
		 SET runtime_session_id = (
		     lower(hex(randomblob(4))) || '-' ||
		     lower(hex(randomblob(2))) || '-' ||
		     lower(hex(randomblob(2))) || '-' ||
		     lower(hex(randomblob(2))) || '-' ||
		     lower(hex(randomblob(6)))
		 ), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE status = 'active' AND EXISTS (
		     SELECT 1 FROM conversation_requests AS request
		     WHERE request.session_id = conversation_sessions.id
		       AND request.state = 'ready'
		       AND request.error_kind = 'runtime'
		       AND request.response_text = ?
		 )`,
		conversation.InterruptedReply,
	); err != nil {
		return nil, fmt.Errorf("rotate interrupted conversation sessions: %w", err)
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE conversation_requests SET state = 'ready' WHERE state = 'delivering'`,
	); err != nil {
		return nil, fmt.Errorf("requeue interrupted conversation deliveries: %w", err)
	}

	rows, err := s.db.QueryContext(
		ctx,
		`SELECT DISTINCT principal_id FROM conversation_requests
		 WHERE state IN ('queued', 'ready') ORDER BY principal_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list recoverable conversations: %w", err)
	}
	defer rows.Close()

	var principals []string
	for rows.Next() {
		var principalID string
		if err := rows.Scan(&principalID); err != nil {
			return nil, fmt.Errorf("scan recoverable conversation: %w", err)
		}
		principals = append(principals, principalID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable conversations: %w", err)
	}
	return principals, nil
}

func (s *Store) NextConversationRequest(ctx context.Context, principalID string) (conversation.Request, bool, error) {
	if s == nil || s.db == nil {
		return conversation.Request{}, false, fmt.Errorf("load next conversation request: SQLite store is not open")
	}
	request, err := scanConversationRequest(s.db.QueryRowContext(
		ctx,
		`SELECT request.id, request.principal_id, request.chat_id, COALESCE(request.telegram_message_id, 0),
		        request.session_id, session.runtime_session_id, request.message, request.state,
		        COALESCE(request.response_text, ''), COALESCE(request.error_kind, ''), request.created_at
		 FROM conversation_requests AS request
		 JOIN conversation_sessions AS session
		   ON session.id = request.session_id AND session.principal_id = request.principal_id
		 WHERE request.principal_id = ? AND request.state IN ('queued', 'ready')
		 ORDER BY request.created_at, request.id LIMIT 1`,
		principalID,
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
		requestID,
		"claim conversation request",
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
		requestID,
		"claim conversation delivery",
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

func (s *Store) CancelPendingConversation(ctx context.Context, principalID string) (bool, error) {
	return s.changeConversationState(
		ctx,
		`UPDATE conversation_requests
		 SET state = 'cancelled', message = '', response_text = NULL,
		     finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND state IN ('queued', 'running', 'ready', 'delivering')`,
		principalID,
		"cancel pending conversation",
	)
}

func (s *Store) ResetConversation(ctx context.Context, principalID string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("reset conversation: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin conversation reset: %w", err)
	}
	defer tx.Rollback()

	session, _, err := loadActiveConversationSession(ctx, tx, principalID)
	if err != nil {
		return "", false, err
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE conversation_requests
		 SET state = 'cancelled', message = '', response_text = NULL,
		     finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND state IN ('queued', 'running', 'ready', 'delivering')`,
		principalID,
	); err != nil {
		return "", false, fmt.Errorf("cancel requests during conversation reset: %w", err)
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE conversation_sessions
		 SET status = 'closed', close_reason = 'user_reset',
		     closed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND status = 'active'`,
		principalID,
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

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadConversationByUpdate(ctx context.Context, db queryRower, botUserID, updateID int64) (conversation.Request, bool, error) {
	request, err := scanConversationRequest(db.QueryRowContext(
		ctx,
		`SELECT request.id, request.principal_id, request.chat_id, COALESCE(request.telegram_message_id, 0),
		        request.session_id, session.runtime_session_id, request.message, request.state,
		        COALESCE(request.response_text, ''), COALESCE(request.error_kind, ''), request.created_at
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

func loadActiveConversationSession(ctx context.Context, db queryRower, principalID string) (conversation.Session, bool, error) {
	var session conversation.Session
	err := db.QueryRowContext(
		ctx,
		`SELECT id, principal_id, runtime_session_id
		 FROM conversation_sessions WHERE principal_id = ? AND status = 'active'`,
		principalID,
	).Scan(&session.ID, &session.PrincipalID, &session.RuntimeSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return conversation.Session{}, false, nil
	}
	if err != nil {
		return conversation.Session{}, false, fmt.Errorf("query active conversation session: %w", err)
	}
	return session, true, nil
}

func scanConversationRequest(row *sql.Row) (conversation.Request, error) {
	var request conversation.Request
	var createdAt string
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
	)
	if err != nil {
		return request, err
	}
	request.CreatedAt, err = time.Parse("2006-01-02T15:04:05.000Z", createdAt)
	if err != nil {
		return request, fmt.Errorf("parse conversation request creation time: %w", err)
	}
	return request, nil
}

func (s *Store) changeConversationState(ctx context.Context, query, identifier, operation string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("%s: SQLite store is not open", operation)
	}
	result, err := s.db.ExecContext(ctx, query, identifier)
	if err != nil {
		return false, fmt.Errorf("%s: %w", operation, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read %s count: %w", operation, err)
	}
	return changed > 0, nil
}

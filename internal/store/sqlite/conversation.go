package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/thread"
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

	lane := conversation.Lane{PrincipalID: input.PrincipalID, ThreadID: input.ThreadID}
	// A trip's tab routes its messages as a feed tab does.
	tabSubject, err := laneTabSubject(ctx, tx, lane)
	if err != nil {
		return conversation.EnqueueResult{}, err
	}
	inFeed := tabSubject != ""
	var switched switchOutcome
	var previousSubject string
	switch {
	case inFeed:
		if previousSubject, err = laneSubject(ctx, tx, lane); err != nil {
			return conversation.EnqueueResult{}, err
		}
		if switched, err = routeInFeed(ctx, tx, lane, input.ReplyToMessageID, candidate); err != nil {
			return conversation.EnqueueResult{}, err
		}
		if switched.elsewhere {
			return conversation.EnqueueResult{Elsewhere: true, ElsewhereThread: switched.session.ThreadID, ElsewhereTab: switched.session.TabName}, nil
		}
	case input.ReplyToMessageID > 0:
		if switched, err = switchToRepliedSession(ctx, tx, lane, input.ReplyToMessageID); err != nil {
			return conversation.EnqueueResult{}, err
		}
	}
	session, found, err := loadActiveConversationSession(ctx, tx, lane)
	if err != nil {
		return conversation.EnqueueResult{}, err
	}
	if !found && input.ThreadID > 0 {
		session, found, err = reopenGoneTab(ctx, tx, lane)
		if err != nil {
			return conversation.EnqueueResult{}, err
		}
	}
	if !found {
		session = candidate
		subject, err := newSessionSubject(ctx, tx, lane, tabSubject)
		if err != nil {
			return conversation.EnqueueResult{}, err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, thread_id, subject) VALUES (?, ?, ?, ?, NULLIF(?, ''))`,
			session.ID,
			session.PrincipalID,
			session.RuntimeSessionID,
			input.ThreadID,
			subject,
		); err != nil {
			return conversation.EnqueueResult{}, fmt.Errorf("create conversation session: %w", err)
		}
	}

	pending, err := lanePending(ctx, tx, lane)
	if err != nil {
		return conversation.EnqueueResult{}, err
	}
	if pending {
		return conversation.EnqueueResult{Busy: true}, nil
	}
	var subjectLabel string
	if inFeed {
		subject, err := sessionSubject(ctx, tx, session)
		if err != nil {
			return conversation.EnqueueResult{}, err
		}
		// A trip's tab already names the trip its conversations are about.
		if subject != "" && subject != previousSubject && subject != tabSubject {
			if subjectLabel, err = loadSubjectLabel(ctx, tx, input.PrincipalID, subject); err != nil {
				return conversation.EnqueueResult{}, err
			}
		}
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
		SubjectLabel:      subjectLabel,
		Forwarded:         input.Forwarded,
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
		    resumed_from, reply_to_message_id, thread_id, subject_label, forwarded
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?)`,
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
		subjectLabel,
		request.Forwarded,
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
		        request.thread_id, `+requestOpensSession+`,
		        COALESCE(session.subject, ''), COALESCE(request.subject_label, ''), request.delivered_chunks, request.forwarded
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
	if request.Subject != "" {
		if request.SubjectName, err = loadSubjectLabel(ctx, s.db, request.PrincipalID, request.Subject); err != nil {
			return conversation.Request{}, false, err
		}
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

func (s *Store) SaveConversationProgress(ctx context.Context, requestID string, chunks int) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save conversation delivery progress: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE conversation_requests SET delivered_chunks = ? WHERE id = ? AND state = 'delivering'`,
		chunks,
		requestID,
	); err != nil {
		return fmt.Errorf("save conversation delivery progress: %w", err)
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
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE telegram_tabs SET reset_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE principal_id = ? AND thread_id = ?`,
		lane.PrincipalID,
		lane.ThreadID,
	); err != nil {
		return "", false, fmt.Errorf("record tab reset: %w", err)
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

// laneTabSubject is what the registry records the lane's tab as serving when it is a feed or a
// trip's tab, and "" otherwise.
func laneTabSubject(ctx context.Context, tx *sql.Tx, lane conversation.Lane) (string, error) {
	if lane.ThreadID <= 0 {
		return "", nil
	}
	var subject string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE((SELECT subject FROM telegram_tabs WHERE principal_id = ? AND thread_id = ? AND (subject LIKE ? OR subject LIKE ?)), '')`,
		lane.PrincipalID,
		lane.ThreadID,
		thread.FeedSubject("%"),
		thread.TripDestination("%"),
	).Scan(&subject); err != nil {
		return "", fmt.Errorf("check feed tab: %w", err)
	}
	return subject, nil
}

// newSessionSubject is the subject of a conversation that starts in the lane: a trip's tab starts
// conversations about its trip, unless another tab holds one already or the trip is gone, and any
// other tab starts conversations about nothing in particular.
func newSessionSubject(ctx context.Context, tx *sql.Tx, lane conversation.Lane, tabSubject string) (string, error) {
	tripID, isTrip := thread.TripOf(tabSubject)
	if !isTrip {
		return "", nil
	}
	var taken bool
	if err := tx.QueryRowContext(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM conversation_sessions WHERE principal_id = ? AND subject = ? AND status = 'active')
		     OR NOT EXISTS(SELECT 1 FROM trips WHERE principal_id = ? AND id = ?)`,
		lane.PrincipalID,
		tabSubject,
		lane.PrincipalID,
		tripID,
	).Scan(&taken); err != nil {
		return "", fmt.Errorf("check trip conversation: %w", err)
	}
	if taken {
		return "", nil
	}
	return tabSubject, nil
}

// routeInFeed finds the conversation a message in a feed tab belongs to. A reply to one of the
// bot's answers returns to its conversation, and a reply to a report or an alert goes to the
// conversation about its job or watch. A reply to anything else, the tab's root included, counts as
// no reply. A message that replies to nothing goes to the subject of the tab's latest report or
// alert when that arrived after the tab's last exchange, and otherwise stays in the tab's current
// conversation. Only an explicit reply comes back as elsewhere when another tab holds its
// conversation, so a tab is never stuck.
func routeInFeed(ctx context.Context, tx *sql.Tx, lane conversation.Lane, replyTo int64, candidate conversation.Session) (switchOutcome, error) {
	// Telegram numbers a tab's root message with the tab's thread ID.
	if replyTo == lane.ThreadID {
		replyTo = 0
	}
	if replyTo > 0 {
		outcome, err := switchToRepliedSession(ctx, tx, lane, replyTo)
		if err != nil || outcome.found {
			return outcome, err
		}
		subject, found, err := feedMessageSubject(ctx, tx, lane.PrincipalID, replyTo)
		if err != nil {
			return switchOutcome{}, err
		}
		if found {
			return switchToSubject(ctx, tx, lane, subject, candidate)
		}
	}
	subject, found, err := latestFeedSubject(ctx, tx, lane)
	if err != nil || !found {
		return switchOutcome{}, err
	}
	outcome, err := switchToSubject(ctx, tx, lane, subject, candidate)
	if err != nil || outcome.elsewhere {
		return switchOutcome{}, err
	}
	return outcome, nil
}

// reportSubject is the subject of a report: its trip when its job was delivered to one, such as a
// check-in reminder, and its job otherwise.
const reportSubject = `CASE WHEN report.destination LIKE ? THEN report.destination ELSE ? || report.job_id END`

// feedMessageSubject is the subject of the principal's report or alert sent as the message.
func feedMessageSubject(ctx context.Context, tx *sql.Tx, principalID string, messageID int64) (string, bool, error) {
	var subject string
	err := tx.QueryRowContext(
		ctx,
		`SELECT `+reportSubject+` FROM job_report_messages AS message
		 JOIN job_reports AS report ON report.id = message.report_id AND report.principal_id = message.principal_id
		 WHERE message.principal_id = ? AND message.telegram_message_id = ?
		 UNION ALL
		 SELECT ? || watch_id FROM watch_alert_messages WHERE principal_id = ? AND telegram_message_id = ?
		 LIMIT 1`,
		thread.TripDestination("%"), conversation.JobSubject(""), principalID, messageID,
		conversation.WatchSubject(""), principalID, messageID,
	).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("query feed message subject: %w", err)
	}
	return subject, true, nil
}

// latestFeedSubject is the subject of the newest report or alert sent to the lane's tab after its
// last exchange: its newest request or delivered answer, the latest change of a conversation in it,
// which counts a conversation's start and a switch, or its last /new.
func latestFeedSubject(ctx context.Context, tx *sql.Tx, lane conversation.Lane) (string, bool, error) {
	var subject string
	err := tx.QueryRowContext(
		ctx,
		`SELECT subject FROM (
		     SELECT `+reportSubject+` AS subject, message.sent_at AS sent_at
		     FROM job_report_messages AS message
		     JOIN job_reports AS report ON report.id = message.report_id AND report.principal_id = message.principal_id
		     WHERE message.principal_id = ? AND message.thread_id = ? AND message.sent_at IS NOT NULL
		     UNION ALL
		     SELECT ? || watch_id, sent_at FROM watch_alert_messages WHERE principal_id = ? AND thread_id = ?
		 )
		 WHERE sent_at > max(
		     COALESCE((SELECT MAX(COALESCE(finished_at, created_at)) FROM conversation_requests WHERE principal_id = ? AND thread_id = ?), ''),
		     COALESCE((SELECT MAX(updated_at) FROM conversation_sessions WHERE principal_id = ? AND thread_id = ?), ''),
		     COALESCE((SELECT reset_at FROM telegram_tabs WHERE principal_id = ? AND thread_id = ?), ''))
		 ORDER BY sent_at DESC LIMIT 1`,
		thread.TripDestination("%"), conversation.JobSubject(""), lane.PrincipalID, lane.ThreadID,
		conversation.WatchSubject(""), lane.PrincipalID, lane.ThreadID,
		lane.PrincipalID, lane.ThreadID,
		lane.PrincipalID, lane.ThreadID,
		lane.PrincipalID, lane.ThreadID,
	).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("query latest feed message: %w", err)
	}
	return subject, true, nil
}

// switchToSubject makes the conversation about the subject the lane's active one: the one already
// active there, the most recent earlier one switched back in, or a new one, which closes the lane's
// current conversation as switched. One active in another tab stays there.
func switchToSubject(ctx context.Context, tx *sql.Tx, lane conversation.Lane, subject string, candidate conversation.Session) (switchOutcome, error) {
	var sessionID string
	err := tx.QueryRowContext(
		ctx,
		`SELECT id FROM conversation_sessions WHERE principal_id = ? AND subject = ?
		 ORDER BY status = 'active' DESC, updated_at DESC, id LIMIT 1`,
		lane.PrincipalID,
		subject,
	).Scan(&sessionID)
	if err == nil {
		return switchActiveSession(ctx, tx, lane, sessionID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return switchOutcome{}, fmt.Errorf("query subject conversation: %w", err)
	}
	previous, _, err := loadActiveConversationSession(ctx, tx, lane)
	if err != nil {
		return switchOutcome{}, err
	}
	if err := closeSwitchedLane(ctx, tx, lane); err != nil {
		return switchOutcome{}, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, thread_id, subject) VALUES (?, ?, ?, ?, ?)`,
		candidate.ID,
		candidate.PrincipalID,
		candidate.RuntimeSessionID,
		lane.ThreadID,
		subject,
	); err != nil {
		return switchOutcome{}, fmt.Errorf("create subject conversation: %w", err)
	}
	return switchOutcome{found: true, previous: previous.ID}, nil
}

// laneSubject is the subject of the lane's active conversation, or "".
func laneSubject(ctx context.Context, tx *sql.Tx, lane conversation.Lane) (string, error) {
	var subject string
	err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(subject, '') FROM conversation_sessions WHERE principal_id = ? AND thread_id = ? AND status = 'active'`,
		lane.PrincipalID,
		lane.ThreadID,
	).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query lane subject: %w", err)
	}
	return subject, nil
}

func sessionSubject(ctx context.Context, tx *sql.Tx, session conversation.Session) (string, error) {
	var subject string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(subject, '') FROM conversation_sessions WHERE id = ? AND principal_id = ?`,
		session.ID,
		session.PrincipalID,
	).Scan(&subject); err != nil {
		return "", fmt.Errorf("query conversation subject: %w", err)
	}
	return subject, nil
}

// loadSubjectLabel is the name a subject's job or watch went by in the last report or alert it
// sent, or a trip's title, or "" when none is kept.
func loadSubjectLabel(ctx context.Context, db queryRower, principalID, subject string) (string, error) {
	kind, id := conversation.SplitSubject(subject)
	var query string
	switch kind {
	case conversation.SubjectJob:
		query = `SELECT job_name FROM job_reports WHERE principal_id = ? AND job_id = ? ORDER BY delivered_at DESC, id DESC LIMIT 1`
	case conversation.SubjectWatch:
		query = `SELECT watch_label FROM watch_alert_messages WHERE principal_id = ? AND watch_id = ? ORDER BY sent_at DESC LIMIT 1`
	case conversation.SubjectTrip:
		query = `SELECT title FROM trips WHERE principal_id = ? AND id = ?`
	default:
		return "", nil
	}
	var label string
	err := db.QueryRowContext(ctx, query, principalID, id).Scan(&label)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query subject label: %w", err)
	}
	return label, nil
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
		        request.thread_id, `+requestOpensSession+`,
		        COALESCE(session.subject, ''), COALESCE(request.subject_label, ''), request.delivered_chunks, request.forwarded
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
		&request.Subject,
		&request.SubjectLabel,
		&request.DeliveredChunks,
		&request.Forwarded,
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

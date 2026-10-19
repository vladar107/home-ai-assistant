package telegram

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	SessionsTitle       = "Recent conversations"
	NoSessionsReply     = "There are no conversations to return to yet."
	SessionGoneReply    = "That conversation is no longer available."
	SessionCurrentReply = "That is already the current conversation."
	SessionElsewhere    = "That conversation is open in another tab."
	SessionNotInTab     = "That conversation is not open in another tab."
	// SessionHere is posted in the tab that holds a conversation an Open press asked for, which
	// moves that tab to the top of the list.
	SessionHere = "You asked to return to this conversation. Your next message here continues it."
	// TabSuspectReply answers an Open press whose tab did not take the line, when the tab may still
	// exist.
	TabSuspectReply = "That tab did not accept my message, so it may be gone. If you cannot find it, close it here; the conversation stays in /sessions."
	// HistoryUnavailable follows a resume whose earlier messages could not be forwarded.
	HistoryUnavailable = "I could not bring the earlier messages of this conversation into this tab."
	untitledTab        = "Conversation from "

	sessionsPrefix       = "sessions"
	sessionResumeAction  = "resume"
	sessionCloseAction   = "close"
	sessionButtonsPerRow = 3
	untitledSession      = "(no text)"
)

// SessionInTab answers an Open press: the bot cannot switch the user's tab, only post in it.
func SessionInTab(tabName string) string {
	return "That conversation is in " + tabPlace(tabName) + ". Telegram does not let me switch tabs for you, so I posted there to bring it to the top."
}

// SessionOpenIn answers a reply to a conversation another tab, or the main view, holds; the message
// does not run.
func SessionOpenIn(threadID int64, tabName string) string {
	place := tabPlace(tabName)
	if threadID == 0 {
		place = "the main view"
	}
	return "That conversation is open in " + place + ". Write there to continue it."
}

// CloseTabQuestion asks before closing a tab from /sessions.
func CloseTabQuestion(tabName string) string {
	place := "the tab that holds that conversation"
	if tabName != "" {
		place = tabPlace(tabName)
	}
	return "Close " + place + "? Telegram deletes its messages, so resuming the conversation later cannot bring them back. The conversation stays in /sessions."
}

// renderSessions lists the principal's recent conversations, newest first, with a Resume button for
// each one that is not the lane's current conversation, or an Open button for one another tab
// holds, named when its tab is known, with a Close button beside it when tabs are on. Buttons are
// numbered like the list.
func renderSessions(ctx context.Context, lane conversation.Lane, conversations ConversationController, closable bool) (string, InlineKeyboard, error) {
	sessions, err := conversations.Sessions(ctx, lane)
	if err != nil {
		return "", nil, err
	}
	if len(sessions) == 0 || (len(sessions) == 1 && sessions[0].Active) {
		return NoSessionsReply, nil, nil
	}
	lines := []string{SessionsTitle, ""}
	var keyboard InlineKeyboard
	var row []InlineButton
	for index, session := range sessions {
		number := strconv.Itoa(index + 1)
		title := session.Title
		if title == "" {
			title = untitledSession
		}
		line := fmt.Sprintf("%s. %s, %s", number, session.LastActive.Format("2 Jan 2006"), title)
		if session.Active {
			lines = append(lines, line+" (current)")
			continue
		}
		buttons := []InlineButton{{Text: "Resume " + number, CallbackData: sessionCallback(sessionResumeAction, session.ID)}}
		if session.Elsewhere {
			place := "another tab"
			if session.TabName != "" {
				place = "“" + session.TabName + "”"
			}
			line += " (open in " + place + ")"
			buttons[0].Text = "Open " + number
			if closable {
				buttons = append(buttons, InlineButton{Text: "Close " + number, CallbackData: sessionCallback(sessionCloseAction, session.ID)})
			}
		}
		lines = append(lines, line)
		if len(row)+len(buttons) > sessionButtonsPerRow {
			keyboard, row = append(keyboard, row), nil
		}
		row = append(row, buttons...)
	}
	if len(row) > 0 {
		keyboard = append(keyboard, row)
	}
	return strings.Join(lines, "\n"), keyboard, nil
}

func sessionCallback(action, sessionID string) string {
	return sessionsPrefix + ":" + action + ":" + sessionID
}

// parseSessionCallback accepts only sessions:resume:<session id> and sessions:close:<session id>;
// anything else is not a session press.
func parseSessionCallback(data string) (string, string, bool) {
	if len(data) > maximumCallbackDataBytes {
		return "", "", false
	}
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != sessionsPrefix || (parts[1] != sessionResumeAction && parts[1] != sessionCloseAction) || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// processSessionPress returns to the chosen conversation and refreshes the list from current
// state, so a press on an outdated list can only reach the principal's own conversations. With
// tabs on the conversation opens in a tab of its own; otherwise it replaces the current one. A Close
// press asks before closing the tab that holds the conversation.
func (s *Service) processSessionPress(ctx context.Context, bot Bot, callback *CallbackQuery, principalID, action, sessionID string) (processingResult, string, error) {
	if action == sessionCloseAction {
		return s.processSessionClosePress(ctx, bot, callback, principalID, sessionID)
	}
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "sessions." + sessionResumeAction}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	lane := laneOf(ctx, principalID)
	followUp, followUpCtx := "", ctx
	var followUpKeyboard InlineKeyboard
	if bot.HasTopicsEnabled {
		var failure string
		var err error
		if followUp, followUpKeyboard, followUpCtx, failure, err = s.resumeInTab(ctx, chatID, lane, sessionID); err != nil {
			return processingResult{}, failure, err
		}
	}
	if followUp == "" {
		resumed, err := s.conversation.Resume(ctx, lane, sessionID)
		if err != nil {
			return processingResult{}, "sessions_action", err
		}
		followUp = resumedReply(resumed)
	}

	text, keyboard, err := renderSessions(ctx, lane, s.conversation, bot.HasTopicsEnabled)
	if err != nil {
		return processingResult{}, "render_sessions", err
	}
	err = s.client.EditKeyboard(ctx, chatID, messageID, text, keyboard)
	if err != nil && isPermanentAPIError(err) {
		err = s.client.SendKeyboard(ctx, chatID, 0, text, keyboard)
	}
	if err == nil {
		err = s.client.SendKeyboard(followUpCtx, chatID, 0, followUp, followUpKeyboard)
	}
	if err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

// processSessionClosePress asks whether to close the tab that holds the chosen conversation. It is
// how a tab the user cannot reach is cleaned up.
func (s *Service) processSessionClosePress(ctx context.Context, bot Bot, callback *CallbackQuery, principalID, sessionID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "sessions." + sessionCloseAction}
	sessions, err := s.conversation.Sessions(ctx, laneOf(ctx, principalID))
	if err != nil {
		return processingResult{}, "render_sessions", err
	}
	text, keyboard := SessionGoneReply, InlineKeyboard(nil)
	if index := slices.IndexFunc(sessions, func(session conversation.SessionSummary) bool { return session.ID == sessionID }); index >= 0 {
		text = SessionNotInTab
		if chosen := sessions[index]; chosen.Elsewhere && bot.HasTopicsEnabled {
			text, keyboard = CloseTabQuestion(chosen.TabName), closeTabKeyboard(chosen.ThreadID)
		}
	}
	if err := s.client.SendKeyboard(ctx, callback.Message.Chat.ID, 0, text, keyboard); err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

// resumeInTab opens the chosen conversation in a tab of its own and returns the line to send, its
// buttons, and the tab to send it to. A conversation another tab holds is pointed to there, unless
// that tab turns out to be gone, in which case it is resumed like a closed one: in a new tab named
// after it, with its earlier messages forwarded in. A tab that refused the line but may still exist
// is offered for closing. An empty line leaves the press to resume in the current tab: the
// conversation is gone or already here, or no tab could be made.
func (s *Service) resumeInTab(ctx context.Context, chatID int64, lane conversation.Lane, sessionID string) (string, InlineKeyboard, context.Context, string, error) {
	sessions, err := s.conversation.Sessions(ctx, lane)
	if err != nil {
		return "", nil, ctx, "render_sessions", err
	}
	index := slices.IndexFunc(sessions, func(session conversation.SessionSummary) bool { return session.ID == sessionID })
	if index < 0 || sessions[index].Active {
		return "", nil, ctx, "", nil
	}
	chosen := sessions[index]
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	if chosen.Elsewhere {
		// The line in the other tab only surfaces it; the answer here says where it went. It never
		// falls back to the main view, which would claim a tab that is gone.
		err := s.client.SendInTab(thread.With(ctx, chosen.ThreadID), chatID, SessionHere)
		verdict, refused := tabRefusal(err)
		switch {
		case refused && verdict == TabSuspect:
			closeButton := InlineButton{Text: "Close " + strconv.Itoa(index+1), CallbackData: sessionCallback(sessionCloseAction, chosen.ID)}
			return TabSuspectReply, InlineKeyboard{{closeButton}}, ctx, "", nil
		case refused:
			logger.InfoContext(ctx, "telegram.sessions.tab_replaced", "principal_id", lane.PrincipalID, "thread_id", chosen.ThreadID)
		case err != nil && !isPermanentAPIError(err):
			return "", nil, ctx, "send_message", err
		default:
			return SessionInTab(chosen.TabName), nil, ctx, "", nil
		}
	}

	name := chosen.Title
	if name == "" {
		name = untitledTab + chosen.LastActive.Format("2 Jan 2006")
	}
	name = topicName(name)
	threadID, err := s.client.CreateForumTopic(ctx, chatID, name)
	if err != nil {
		if !isPermanentAPIError(err) {
			return "", nil, ctx, "create_tab", err
		}
		logger.WarnContext(ctx, "telegram.sessions.tab_refused", "principal_id", lane.PrincipalID, "error", err)
		return "", nil, ctx, "", nil
	}
	tab := conversation.Lane{PrincipalID: lane.PrincipalID, ThreadID: threadID}
	s.tabs.recordBotTab(ctx, lane.PrincipalID, threadID, name)
	tabCtx := thread.With(ctx, threadID)
	resumed, err := s.conversation.Resume(tabCtx, tab, sessionID)
	if err != nil || !resumed.Found || resumed.Busy || resumed.Elsewhere || resumed.Current {
		if deleteErr := s.client.DeleteForumTopic(ctx, chatID, threadID); deleteErr != nil {
			logger.WarnContext(ctx, "telegram.sessions.tab_left", "principal_id", lane.PrincipalID, "error", deleteErr)
		}
		s.tabs.forget(ctx, chatID, tab)
		if err != nil {
			return "", nil, ctx, "sessions_action", err
		}
		return resumedReply(resumed), nil, ctx, "", nil
	}
	followUp := resumedReply(resumed)
	messages, err := s.conversation.SessionMessages(ctx, lane.PrincipalID, sessionID)
	if err == nil {
		err = s.client.ForwardMessages(tabCtx, chatID, messages)
	}
	if err != nil {
		logger.WarnContext(ctx, "telegram.sessions.history_failed", "principal_id", lane.PrincipalID, "error", err)
		followUp = HistoryUnavailable + "\n\n" + followUp
	}
	return followUp, nil, tabCtx, "", nil
}

func resumedReply(resumed conversation.ResumeResult) string {
	switch {
	case resumed.Busy:
		return conversation.BusyReply
	case resumed.Elsewhere:
		return SessionElsewhere
	case !resumed.Found:
		return SessionGoneReply
	case resumed.Current:
		return SessionCurrentReply
	}
	reply := conversation.ResumedLine(resumed.Session.LastActive) + " Your next message continues it."
	if resumed.Session.Title != "" {
		reply += "\n\n“" + resumed.Session.Title + "”"
	}
	return reply
}

// laneOf is the principal's conversation in the tab the update came from.
func laneOf(ctx context.Context, principalID string) conversation.Lane {
	return conversation.Lane{PrincipalID: principalID, ThreadID: thread.From(ctx)}
}

// updateThread is the tab an update came from, or zero for the main view. A reaction names no tab;
// the conversation finds the tab of the reply it is on.
func updateThread(update Update) int64 {
	switch {
	case update.CallbackQuery != nil && update.CallbackQuery.Message != nil:
		return update.CallbackQuery.Message.MessageThreadID
	case update.StoppedMessageGeneration != nil:
		return update.StoppedMessageGeneration.MessageThreadID
	case update.EditedMessage != nil:
		return update.EditedMessage.MessageThreadID
	case update.Message != nil:
		return update.Message.MessageThreadID
	}
	return 0
}

// repliedTo is the message the user answered by replying, or zero. A message in a tab replies to
// the tab's creation without the user replying to anything.
func repliedTo(message *Message) int64 {
	if message.ReplyToMessage == nil || message.ReplyToMessage.ForumTopicCreated != nil {
		return 0
	}
	return message.ReplyToMessage.MessageID
}

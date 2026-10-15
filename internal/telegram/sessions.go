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
	// SessionInItsTab answers a Resume press on a conversation another tab holds; SessionHere is
	// posted in that tab, which moves it to the top of the list.
	SessionInItsTab = "That conversation is open in its own tab, which is now at the top of your tabs."
	SessionHere     = "You asked to return to this conversation. Your next message here continues it."
	// HistoryUnavailable follows a resume whose earlier messages could not be forwarded.
	HistoryUnavailable = "I could not bring the earlier messages of this conversation into this tab."
	untitledTab        = "Conversation from "

	sessionsPrefix       = "sessions"
	sessionResumeAction  = "resume"
	sessionButtonsPerRow = 3
	untitledSession      = "(no text)"
)

// renderSessions lists the principal's recent conversations, newest first, with a Resume button for
// each one that is not the lane's current conversation, or an Open button for one another tab
// holds. Buttons are numbered like the list.
func renderSessions(ctx context.Context, lane conversation.Lane, conversations ConversationController) (string, InlineKeyboard, error) {
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
		label := "Resume "
		if session.Elsewhere {
			line, label = line+" (open in another tab)", "Open "
		}
		lines = append(lines, line)
		row = append(row, InlineButton{Text: label + number, CallbackData: sessionsPrefix + ":" + sessionResumeAction + ":" + session.ID})
		if len(row) == sessionButtonsPerRow {
			keyboard, row = append(keyboard, row), nil
		}
	}
	if len(row) > 0 {
		keyboard = append(keyboard, row)
	}
	return strings.Join(lines, "\n"), keyboard, nil
}

// parseSessionCallback accepts only sessions:resume:<session id>; anything else is not a session
// press.
func parseSessionCallback(data string) (string, bool) {
	if len(data) > maximumCallbackDataBytes {
		return "", false
	}
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != sessionsPrefix || parts[1] != sessionResumeAction || parts[2] == "" {
		return "", false
	}
	return parts[2], true
}

// processSessionPress returns to the chosen conversation and refreshes the list from current
// state, so a press on an outdated list can only reach the principal's own conversations. With
// tabs on the conversation opens in a tab of its own; otherwise it replaces the current one.
func (s *Service) processSessionPress(ctx context.Context, bot Bot, callback *CallbackQuery, principalID, sessionID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "sessions." + sessionResumeAction}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	lane := laneOf(ctx, principalID)
	followUp, followUpCtx := "", ctx
	if bot.HasTopicsEnabled {
		var failure string
		var err error
		if followUp, followUpCtx, failure, err = s.resumeInTab(ctx, chatID, lane, sessionID); err != nil {
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

	text, keyboard, err := renderSessions(ctx, lane, s.conversation)
	if err != nil {
		return processingResult{}, "render_sessions", err
	}
	err = s.client.EditKeyboard(ctx, chatID, messageID, text, keyboard)
	if err != nil && isPermanentAPIError(err) {
		err = s.client.SendKeyboard(ctx, chatID, 0, text, keyboard)
	}
	if err == nil {
		err = s.client.SendReply(followUpCtx, chatID, 0, followUp)
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

// resumeInTab opens the chosen conversation in a tab of its own and returns the line to send and
// the tab to send it to. A conversation another tab holds is pointed to there; any other closed
// one gets a new tab named after it, with its earlier messages forwarded in. An empty line leaves
// the press to resume in the current tab: the conversation is gone or already here, or no tab
// could be made.
func (s *Service) resumeInTab(ctx context.Context, chatID int64, lane conversation.Lane, sessionID string) (string, context.Context, string, error) {
	sessions, err := s.conversation.Sessions(ctx, lane)
	if err != nil {
		return "", ctx, "render_sessions", err
	}
	index := slices.IndexFunc(sessions, func(session conversation.SessionSummary) bool { return session.ID == sessionID })
	if index < 0 || sessions[index].Active {
		return "", ctx, "", nil
	}
	chosen := sessions[index]
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	if chosen.Elsewhere {
		// The line in the other tab only surfaces it; the answer here says where it went.
		if err := s.client.SendReply(thread.With(ctx, chosen.ThreadID), chatID, 0, SessionHere); err != nil && !isPermanentAPIError(err) {
			return "", ctx, "send_message", err
		}
		return SessionInItsTab, ctx, "", nil
	}

	name := chosen.Title
	if name == "" {
		name = untitledTab + chosen.LastActive.Format("2 Jan 2006")
	}
	threadID, err := s.client.CreateForumTopic(ctx, chatID, name)
	if err != nil {
		if !isPermanentAPIError(err) {
			return "", ctx, "create_tab", err
		}
		logger.WarnContext(ctx, "telegram.sessions.tab_refused", "principal_id", lane.PrincipalID, "error", err)
		return "", ctx, "", nil
	}
	tab := conversation.Lane{PrincipalID: lane.PrincipalID, ThreadID: threadID}
	tabCtx := thread.With(ctx, threadID)
	resumed, err := s.conversation.Resume(tabCtx, tab, sessionID)
	if err != nil || !resumed.Found || resumed.Busy || resumed.Elsewhere || resumed.Current {
		if deleteErr := s.client.DeleteForumTopic(ctx, chatID, threadID); deleteErr != nil {
			logger.WarnContext(ctx, "telegram.sessions.tab_left", "principal_id", lane.PrincipalID, "error", deleteErr)
		}
		if err != nil {
			return "", ctx, "sessions_action", err
		}
		return resumedReply(resumed), ctx, "", nil
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
	return followUp, tabCtx, "", nil
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

// repliedTo is the message the user answered by replying, or zero.
func repliedTo(message *Message) int64 {
	if message.ReplyToMessage == nil {
		return 0
	}
	return message.ReplyToMessage.MessageID
}

package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

const (
	SessionsTitle       = "Recent conversations"
	NoSessionsReply     = "There are no conversations to return to yet."
	SessionGoneReply    = "That conversation is no longer available."
	SessionCurrentReply = "That is already the current conversation."

	sessionsPrefix       = "sessions"
	sessionResumeAction  = "resume"
	sessionButtonsPerRow = 3
	untitledSession      = "(no text)"
)

// renderSessions lists the principal's recent conversations, newest first, with a Resume button for
// each one that is not the current conversation. Buttons are numbered like the list.
func renderSessions(ctx context.Context, principalID string, conversations ConversationController) (string, InlineKeyboard, error) {
	sessions, err := conversations.Sessions(ctx, principalID)
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
		lines = append(lines, line)
		row = append(row, InlineButton{Text: "Resume " + number, CallbackData: sessionsPrefix + ":" + sessionResumeAction + ":" + session.ID})
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
// state, so a press on an outdated list can only reach the principal's own conversations.
func (s *Service) processSessionPress(ctx context.Context, callback *CallbackQuery, principalID, sessionID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "sessions." + sessionResumeAction}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	resumed, err := s.conversation.Resume(ctx, principalID, sessionID)
	if err != nil {
		return processingResult{}, "sessions_action", err
	}
	followUp := resumedReply(resumed)

	text, keyboard, err := renderSessions(ctx, principalID, s.conversation)
	if err != nil {
		return processingResult{}, "render_sessions", err
	}
	err = s.client.EditKeyboard(ctx, chatID, messageID, text, keyboard)
	if err != nil && isPermanentAPIError(err) {
		err = s.client.SendKeyboard(ctx, chatID, 0, text, keyboard)
	}
	if err == nil {
		err = s.client.SendReply(ctx, chatID, 0, followUp)
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

func resumedReply(resumed conversation.ResumeResult) string {
	switch {
	case resumed.Busy:
		return conversation.BusyReply
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

// repliedTo is the message the user answered by replying, or zero.
func repliedTo(message *Message) int64 {
	if message.ReplyToMessage == nil {
		return 0
	}
	return message.ReplyToMessage.MessageID
}

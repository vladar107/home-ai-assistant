package telegram

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	CloseTabPrompt     = "Close this tab? Telegram deletes its messages, so resuming the conversation later cannot bring them back. The conversation stays in /sessions."
	CloseTabButton     = "Close tab"
	KeepTabButton      = "Keep"
	TabKeptReply       = "Kept the tab."
	TabClosedReply     = "Closed the tab. Its conversation stays in /sessions."
	TabNotRemovedReply = "I ended that tab's conversation, which stays in /sessions, but Telegram did not let me remove the tab. You can delete it from the tab's menu."
	CloseMainViewReply = "The main view cannot be closed. /new starts a fresh conversation here."

	tabsPrefix     = "tabs"
	tabCloseAction = "close"
	tabKeepAction  = "keep"

	// tabSuspectWindow is how long a refused tab is kept when the user wrote in it lately, or when
	// the bot made it and it never took a send. Telegram refuses sends to some tabs that exist,
	// including fresh ones the bot made, and says nothing when a tab is deleted.
	tabSuspectWindow = 24 * time.Hour
)

// TabStore is the registry of the tabs in each principal's chat; nil leaves tabs unnamed and
// treats every refused tab as gone.
type TabStore interface {
	AddTab(ctx context.Context, principalID string, threadID int64, name, namedBy string) (bool, error)
	RenameTab(ctx context.Context, principalID string, threadID int64, name string) (bool, error)
	// ConfirmTab confirms a known tab and reports whether the registry now holds it confirmed.
	ConfirmTab(ctx context.Context, principalID string, threadID int64) (bool, error)
	Tab(ctx context.Context, principalID string, threadID int64) (thread.Tab, bool, error)
	DeleteTab(ctx context.Context, principalID string, threadID int64) error
	// ClaimTabName reports whether the bot may name the tab after the session, and records it.
	ClaimTabName(ctx context.Context, principalID string, threadID int64, sessionID string) (bool, error)
	NameBotTab(ctx context.Context, principalID string, threadID int64, sessionID, name, iconCustomEmojiID string) (bool, error)
	// LaneLastRequest returns when the lane last took a message, if it ever did.
	LaneLastRequest(ctx context.Context, lane conversation.Lane) (time.Time, bool, error)
	// SubjectTab returns the tab recorded as serving the subject, such as a feed.
	SubjectTab(ctx context.Context, principalID, subject string) (int64, bool, error)
	// AddSubjectTab records a tab the bot made to serve the subject, under a fixed name.
	AddSubjectTab(ctx context.Context, principalID string, threadID int64, name, subject string) error
	// SetFixedTabIcon records the icon the bot gave a tab of a fixed kind.
	SetFixedTabIcon(ctx context.Context, principalID string, threadID int64, iconCustomEmojiID string) error
	// SetFixedTabName records the name the bot gave a tab of a fixed kind.
	SetFixedTabName(ctx context.Context, principalID string, threadID int64, name string) error
}

type chatTab struct{ chatID, threadID int64 }

var _ TabObserver = (*Service)(nil)

// TabAccepted confirms a tab the first time it takes a send.
func (s *Service) TabAccepted(ctx context.Context, chatID, threadID int64) {
	if s.tabs.store == nil || s.tabs.confirmed(chatID, threadID) {
		return
	}
	principalID, found := s.chatPrincipal(ctx, chatID)
	if !found {
		return
	}
	s.tabs.confirm(ctx, chatID, principalID, threadID)
}

// TabRefused decides what a tab Telegram did not find is. A tab the user wrote in lately, or one the
// bot made that never took a send, is suspect and kept as it is. Any other is gone: its
// conversation closes, to come back if the user writes there again, and the registry forgets it.
// Each refused send is logged, but a gone tab is counted once, by the close of its conversation.
func (s *Service) TabRefused(ctx context.Context, chatID, threadID int64) TabVerdict {
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	principalID, found := s.chatPrincipal(ctx, chatID)
	if !found {
		logger.WarnContext(ctx, "telegram.send.thread_gone", "thread_id", threadID)
		return TabGone
	}
	lane := conversation.Lane{PrincipalID: principalID, ThreadID: threadID}
	if s.tabs.suspect(ctx, lane) {
		logger.InfoContext(ctx, "telegram.send.thread_gone_suspect", "principal_id", principalID, "thread_id", threadID)
		s.telemetry.RecordTab(ctx, principalID, "tab_gone_suspect")
		return TabSuspect
	}
	logger.WarnContext(ctx, "telegram.send.thread_gone", "principal_id", principalID, "thread_id", threadID)
	if s.conversation != nil {
		closed, err := s.conversation.CloseLane(ctx, lane, conversation.CloseTabGone)
		if err != nil {
			logger.WarnContext(ctx, "telegram.tab.close_failed", "principal_id", principalID, "thread_id", threadID, "error", err)
		}
		if closed {
			s.telemetry.RecordTab(ctx, principalID, "tab_gone")
		}
	}
	s.tabs.forget(ctx, chatID, lane)
	return TabGone
}

// chatPrincipal is the principal of a private chat, whose ID is its user's.
func (s *Service) chatPrincipal(ctx context.Context, chatID int64) (string, bool) {
	authorization, err := s.identities.Authorize(ctx, chatID)
	if err != nil || !authorization.Authorized() {
		return "", false
	}
	return authorization.Principal.ID, true
}

// processTopicService records a tab's creation or rename. The bot's own messages are read too,
// because a rename the bot made comes back as one; it changes nothing. A fixed tab keeps its name.
func (s *Service) processTopicService(ctx context.Context, bot Bot, message *Message) (processingResult, string, error) {
	threadID := message.MessageThreadID
	if !bot.HasTopicsEnabled || message.Chat.Type != ChatTypePrivate || message.Chat.ID <= 0 || message.From == nil || threadID <= 0 {
		return ignoredResult("malformed_topic"), "", nil
	}
	authorizeCtx, endAuthorize := s.telemetry.StartStore(ctx, "identity_authorize")
	authorization, err := s.identities.Authorize(authorizeCtx, message.Chat.ID)
	endAuthorize(err)
	if err != nil {
		return processingResult{}, "authorize_identity", err
	}
	if !authorization.Authorized() {
		return ignoredResult(string(authorization.Status)), "", nil
	}
	principalID := authorization.Principal.ID
	byBot := message.From.ID == bot.ID
	result := processingResult{event: "telegram.topic.handled", principalID: principalID, command: "topic"}
	if created := message.ForumTopicCreated; created != nil {
		s.tabs.logTopicService(ctx, principalID, threadID, "created", "message", byBot, created.Name != "", created.IsNameImplicit, created.IconCustomEmojiID != "")
		if !byBot {
			s.rememberFreshTab(chatTab{message.Chat.ID, threadID})
		}
		if s.tabs.store == nil {
			return result, "", nil
		}
		if name := topicName(created.Name); name != "" {
			namedBy := thread.NamedByUser
			if byBot || created.IsNameImplicit {
				namedBy = thread.NamedByBot
			}
			if err := s.tabs.addTab(ctx, principalID, threadID, name, namedBy); err != nil {
				return processingResult{}, "record_tab", err
			}
		}
	} else if edited := message.ForumTopicEdited; edited != nil {
		s.tabs.logTopicService(ctx, principalID, threadID, "edited", "message", byBot, edited.Name != nil, false, edited.IconCustomEmojiID != nil)
		if s.tabs.store == nil || byBot || edited.Name == nil {
			return result, "", nil
		}
		if name := topicName(*edited.Name); name != "" {
			if err := s.tabs.renameTab(ctx, principalID, threadID, name); err != nil {
				return processingResult{}, "record_tab", err
			}
		}
	}
	if !byBot {
		s.tabs.confirm(ctx, message.Chat.ID, principalID, threadID)
	}
	return result, "", nil
}

// topicName is a tab name as the registry keeps it: one trimmed line within Telegram's limit, or ""
// when nothing is left.
func topicName(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(name, "")))
	if runes := []rune(name); len(runes) > topicNameLimit {
		name = strings.TrimSpace(string(runes[:topicNameLimit]))
	}
	return name
}

// tabPlace names a tab for a sentence: its name in quotes, or "another tab".
func tabPlace(name string) string {
	if name == "" {
		return "another tab"
	}
	return "the tab “" + name + "”"
}

func closeTabKeyboard(threadID int64) InlineKeyboard {
	return InlineKeyboard{{
		{Text: CloseTabButton, CallbackData: tabsPrefix + ":" + tabCloseAction + ":" + strconv.FormatInt(threadID, 10)},
		{Text: KeepTabButton, CallbackData: tabsPrefix + ":" + tabKeepAction},
	}}
}

// parseTabCallback accepts tabs:keep and tabs:close:<thread>.
func parseTabCallback(data string) (string, int64, bool) {
	if len(data) > maximumCallbackDataBytes {
		return "", 0, false
	}
	parts := strings.Split(data, ":")
	switch {
	case len(parts) == 2 && parts[0] == tabsPrefix && parts[1] == tabKeepAction:
		return tabKeepAction, 0, true
	case len(parts) == 3 && parts[0] == tabsPrefix && parts[1] == tabCloseAction:
		threadID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || threadID <= 0 {
			return "", 0, false
		}
		return tabCloseAction, threadID, true
	}
	return "", 0, false
}

// processCloseCommand asks before closing the tab /close was sent in. The main view cannot be
// closed.
func (s *Service) processCloseCommand(ctx context.Context, message *Message, principalID string) (processingResult, string, error) {
	threadID := thread.From(ctx)
	if threadID <= 0 {
		return s.sendCommandReply(ctx, message, principalID, "close", CloseMainViewReply, nil)
	}
	return s.sendCommandReply(ctx, message, principalID, "close", CloseTabPrompt, closeTabKeyboard(threadID))
}

// processTabPress closes a tab once the user confirmed it, or keeps it. Closing cancels the tab's
// turn, so no reply of it reaches the main view, ends its conversation, and then deletes the tab.
// The conversation stays closed even when Telegram refuses the delete. The outcome replaces the
// question, or, when the question was in the closed tab, goes to the main view.
func (s *Service) processTabPress(ctx context.Context, callback *CallbackQuery, principalID, action string, threadID int64) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: tabsPrefix + "." + action}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	text := TabKeptReply
	inClosedTab := false
	if action == tabCloseAction {
		lane := conversation.Lane{PrincipalID: principalID, ThreadID: threadID}
		if _, err := s.conversation.CloseLane(ctx, lane, conversation.CloseTabClosed); err != nil {
			return processingResult{}, "close_tab", err
		}
		err := s.client.DeleteForumTopic(ctx, chatID, threadID)
		if err != nil && !isPermanentAPIError(err) {
			return processingResult{}, "delete_tab", err
		}
		// A tab Telegram does not find is already gone.
		removed := err == nil || isThreadGone(err)
		s.tabs.forget(ctx, chatID, lane)
		s.telemetry.RecordTab(ctx, principalID, "tab_closed")
		s.telemetry.ContextLogger(ctx, s.logger).InfoContext(ctx, "telegram.tab.closed", "principal_id", principalID, "thread_id", threadID, "removed", removed)
		text = TabClosedReply
		if !removed {
			text = TabNotRemovedReply
		}
		inClosedTab = thread.From(ctx) == threadID
	}
	var err error
	if inClosedTab {
		err = s.client.SendReply(thread.With(ctx, 0), chatID, 0, text)
	} else {
		err = s.client.EditKeyboard(ctx, chatID, messageID, text, nil)
		if err != nil && isPermanentAPIError(err) {
			err = s.client.SendReply(ctx, chatID, 0, text)
		}
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

// freshTabWindow is how long after the user opens a tab its first message may still be a command
// sent from the main view; the client sends the two about a second apart.
const freshTabWindow = 2 * time.Minute

// rememberFreshTab notes a tab the user just opened, dropping the ones whose window has passed,
// so tabs that never get a message are not kept.
func (s *Service) rememberFreshTab(tab chatTab) {
	now := time.Now()
	s.freshTabs.Range(func(key, opened any) bool {
		if now.Sub(opened.(time.Time)) >= freshTabWindow {
			s.freshTabs.Delete(key)
		}
		return true
	})
	s.freshTabs.Store(tab, now)
}

// openedForCommand reports whether a command is the first message of a tab the user just opened.
// The client opens a tab for every message sent from the main view, commands included, so such a
// command belongs to the main view. The tab stays fresh until the command is answered, so a
// retried update is still moved.
func (s *Service) openedForCommand(chatID, threadID int64) bool {
	if threadID <= 0 {
		return false
	}
	opened, fresh := s.freshTabs.Load(chatTab{chatID, threadID})
	return fresh && time.Since(opened.(time.Time)) < freshTabWindow
}

// processMainViewCommand answers a command the client put in a tab of its own in the main view, as
// if it had been sent there, and then deletes that tab. The answer is not a reply to the command,
// because a reply lands in the tab of the message it answers.
func (s *Service) processMainViewCommand(ctx context.Context, bot Bot, message *Message, principalID, command string, threadID int64) (processingResult, string, error) {
	moved := *message
	moved.MessageID, moved.MessageThreadID, moved.ReplyToMessage = 0, 0, nil
	result, failure, err := s.dispatchCommand(thread.With(ctx, 0), bot, &moved, principalID, command)
	if err != nil || failure != "" {
		return result, failure, err
	}
	key := chatTab{message.Chat.ID, threadID}
	s.freshTabs.Delete(key)
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	logger.InfoContext(ctx, "telegram.tab.command_moved", "principal_id", principalID, "thread_id", threadID)
	// A tab Telegram no longer finds is as good as deleted; any other refusal leaves it.
	if deleteErr := s.client.DeleteForumTopic(ctx, message.Chat.ID, threadID); deleteErr != nil && !isThreadGone(deleteErr) {
		logger.WarnContext(ctx, "telegram.tab.command_tab_left", "principal_id", principalID, "thread_id", threadID, "error", deleteErr)
		return result, failure, nil
	}
	s.tabs.forget(ctx, message.Chat.ID, conversation.Lane{PrincipalID: principalID, ThreadID: threadID})
	return result, failure, nil
}

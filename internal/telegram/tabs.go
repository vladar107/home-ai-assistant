package telegram

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
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
	TabsHelpLine       = " /close closes the tab it is sent in."

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
	Tab(ctx context.Context, principalID string, threadID int64) (sqlite.Tab, bool, error)
	DeleteTab(ctx context.Context, principalID string, threadID int64) error
	// LaneLastRequest returns when the lane last took a message, if it ever did.
	LaneLastRequest(ctx context.Context, lane conversation.Lane) (time.Time, bool, error)
	// SubjectTab returns the tab recorded as serving the subject, such as a feed.
	SubjectTab(ctx context.Context, principalID, subject string) (int64, bool, error)
	// AddSubjectTab records a tab the bot made to serve the subject, under a fixed name.
	AddSubjectTab(ctx context.Context, principalID string, threadID int64, name, subject string) error
}

// SetTabs gives the service the tab registry; call it before Start.
func (s *Service) SetTabs(tabs TabStore) {
	s.tabs = tabs
}

type chatTab struct{ chatID, threadID int64 }

var _ TabObserver = (*Service)(nil)

// TabAccepted confirms a tab the first time it takes a send.
func (s *Service) TabAccepted(ctx context.Context, chatID, threadID int64) {
	if s.tabs == nil {
		return
	}
	if _, confirmed := s.confirmedTabs.Load(chatTab{chatID, threadID}); confirmed {
		return
	}
	principalID, found := s.chatPrincipal(ctx, chatID)
	if !found {
		return
	}
	s.confirmTab(ctx, chatID, principalID, threadID)
}

func (s *Service) confirmTab(ctx context.Context, chatID int64, principalID string, threadID int64) {
	if _, confirmed := s.confirmedTabs.Load(chatTab{chatID, threadID}); confirmed {
		return
	}
	confirmed, err := s.tabs.ConfirmTab(ctx, principalID, threadID)
	if err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.tab.confirm_failed", "principal_id", principalID, "thread_id", threadID, "error", err)
		return
	}
	// A tab the registry does not know yet may be added later, and must be confirmed then.
	if confirmed {
		s.confirmedTabs.Store(chatTab{chatID, threadID}, struct{}{})
	}
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
	if s.tabSuspect(ctx, lane) {
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
	s.forgetTab(ctx, chatID, lane)
	return TabGone
}

// tabSuspect reports whether a refused tab should be kept. A tab whose state cannot be read is kept.
func (s *Service) tabSuspect(ctx context.Context, lane conversation.Lane) bool {
	if s.tabs == nil {
		return false
	}
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	last, wrote, err := s.tabs.LaneLastRequest(ctx, lane)
	if err != nil {
		logger.WarnContext(ctx, "telegram.tab.read_failed", "principal_id", lane.PrincipalID, "thread_id", lane.ThreadID, "error", err)
		return true
	}
	if wrote && time.Since(last) < tabSuspectWindow {
		return true
	}
	// A message of the user's confirms the tab it came from, so an unconfirmed tab is one the bot made.
	tab, known, err := s.tabs.Tab(ctx, lane.PrincipalID, lane.ThreadID)
	if err != nil {
		logger.WarnContext(ctx, "telegram.tab.read_failed", "principal_id", lane.PrincipalID, "thread_id", lane.ThreadID, "error", err)
		return true
	}
	return known && !tab.Confirmed && time.Since(tab.CreatedAt) < tabSuspectWindow
}

func (s *Service) forgetTab(ctx context.Context, chatID int64, lane conversation.Lane) {
	s.confirmedTabs.Delete(chatTab{chatID, lane.ThreadID})
	s.knownTabs.Delete(chatTab{chatID, lane.ThreadID})
	if s.tabs == nil {
		return
	}
	if err := s.tabs.DeleteTab(ctx, lane.PrincipalID, lane.ThreadID); err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.tab.forget_failed", "principal_id", lane.PrincipalID, "thread_id", lane.ThreadID, "error", err)
	}
}

// chatPrincipal is the principal of a private chat, whose ID is its user's.
func (s *Service) chatPrincipal(ctx context.Context, chatID int64) (string, bool) {
	authorization, err := s.identities.Authorize(ctx, chatID)
	if err != nil || !authorization.Authorized() {
		return "", false
	}
	return authorization.Principal.ID, true
}

// recordBotTab adds a tab the bot made to the registry before anything is sent there.
func (s *Service) recordBotTab(ctx context.Context, principalID string, threadID int64, name string) {
	if s.tabs == nil {
		return
	}
	if _, err := s.tabs.AddTab(ctx, principalID, threadID, name, sqlite.TabNamedByBot); err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.tab.record_failed", "principal_id", principalID, "thread_id", threadID, "error", err)
	}
}

// noteTab learns from a message of the user's in a tab: the tab exists, and the message replies to
// the one that created the tab, which names it. That root is on every message in a tab, so it only
// adds a tab the registry does not know and never changes a known one, and a tab once known is not
// looked up again until it is forgotten.
func (s *Service) noteTab(ctx context.Context, bot Bot, principalID string, message *Message) {
	threadID := thread.From(ctx)
	if s.tabs == nil || threadID <= 0 {
		return
	}
	key := chatTab{message.Chat.ID, threadID}
	_, known := s.knownTabs.Load(key)
	if root := message.ReplyToMessage; !known && root != nil && root.ForumTopicCreated != nil {
		if name := topicName(root.ForumTopicCreated.Name); name != "" {
			namedBy := sqlite.TabNamedByUser
			if (root.From != nil && root.From.ID == bot.ID) || root.ForumTopicCreated.IsNameImplicit {
				namedBy = sqlite.TabNamedByBot
			}
			added, err := s.tabs.AddTab(ctx, principalID, threadID, name, namedBy)
			if err != nil {
				s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.tab.record_failed", "principal_id", principalID, "thread_id", threadID, "error", err)
			} else {
				s.knownTabs.Store(key, struct{}{})
			}
			if added {
				s.logTopicService(ctx, principalID, threadID, "created", "reply", root.From != nil && root.From.ID == bot.ID, true, root.ForumTopicCreated.IsNameImplicit, root.ForumTopicCreated.IconCustomEmojiID != "")
			}
		}
	}
	s.confirmTab(ctx, message.Chat.ID, principalID, threadID)
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
		s.logTopicService(ctx, principalID, threadID, "created", "message", byBot, created.Name != "", created.IsNameImplicit, created.IconCustomEmojiID != "")
		if s.tabs == nil {
			return result, "", nil
		}
		if name := topicName(created.Name); name != "" {
			namedBy := sqlite.TabNamedByUser
			if byBot || created.IsNameImplicit {
				namedBy = sqlite.TabNamedByBot
			}
			if _, err := s.tabs.AddTab(ctx, principalID, threadID, name, namedBy); err != nil {
				return processingResult{}, "record_tab", err
			}
		}
	} else if edited := message.ForumTopicEdited; edited != nil {
		s.logTopicService(ctx, principalID, threadID, "edited", "message", byBot, edited.Name != nil, false, edited.IconCustomEmojiID != nil)
		if s.tabs == nil || byBot || edited.Name == nil {
			return result, "", nil
		}
		if name := topicName(*edited.Name); name != "" {
			if _, err := s.tabs.RenameTab(ctx, principalID, threadID, name); err != nil {
				return processingResult{}, "record_tab", err
			}
		}
	}
	if !byBot && s.tabs != nil {
		s.confirmTab(ctx, message.Chat.ID, principalID, threadID)
	}
	return result, "", nil
}

// logTopicService records which fields a topic service message carried, never the name.
func (s *Service) logTopicService(ctx context.Context, principalID string, threadID int64, kind, source string, byBot, name, implicit, icon bool) {
	sender := "user"
	if byBot {
		sender = "bot"
	}
	s.telemetry.ContextLogger(ctx, s.logger).InfoContext(ctx, "telegram.topic.service",
		"principal_id", principalID, "thread_id", threadID, "kind", kind, "source", source, "sender", sender,
		"name_present", name, "name_implicit", implicit, "icon_present", icon)
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
		s.forgetTab(ctx, chatID, lane)
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

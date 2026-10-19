package telegram

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	tabNameTimeout = 30 * time.Second
	// The icons Telegram offers change rarely, so they are read again once a day, and an hour after
	// a failed read.
	topicIconRefresh = 24 * time.Hour
	topicIconRetry   = time.Hour
)

// topicIcons is the set of icons Telegram lets a tab show, as emoji to custom emoji ID.
type topicIcons struct {
	mu       sync.Mutex
	emoji    []string
	byEmoji  map[string]string
	due      time.Time
	fetching bool
}

// refreshTopicIcons reads the icon set again once it is due, outside the lock, so IconFor never
// waits for Telegram. A failed read keeps the set it had, which may be none, so tabs are then named
// without icons. Telegram may list an emoji twice; its first icon is kept.
func (s *Tabs) refreshTopicIcons(ctx context.Context) {
	s.icons.mu.Lock()
	if s.icons.fetching || time.Now().Before(s.icons.due) {
		s.icons.mu.Unlock()
		return
	}
	s.icons.fetching = true
	s.icons.mu.Unlock()

	icons, err := s.client.GetForumTopicIconStickers(ctx)
	s.icons.mu.Lock()
	defer s.icons.mu.Unlock()
	s.icons.fetching = false
	if err != nil {
		s.icons.due = time.Now().Add(topicIconRetry)
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.tab.icons_failed", "error", err)
		return
	}
	emoji, byEmoji := make([]string, 0, len(icons)), make(map[string]string, len(icons))
	for _, icon := range icons {
		key := plainIcon(icon.Emoji)
		if key == "" || icon.CustomEmojiID == "" {
			continue
		}
		if _, seen := byEmoji[key]; seen {
			continue
		}
		emoji = append(emoji, icon.Emoji)
		byEmoji[key] = icon.CustomEmojiID
	}
	s.icons.emoji, s.icons.byEmoji = emoji, byEmoji
	s.icons.due = time.Now().Add(topicIconRefresh)
}

// IconFor returns the custom emoji ID of the tab icon Telegram offers for emoji, as it was last
// read, so a tab of a fixed kind can show its icon; ok is false when Telegram offers none for it.
// It never reads the set itself: that happens at start and, once due, when a tab is named.
func (s *Tabs) IconFor(emoji string) (customEmojiID string, ok bool) {
	s.icons.mu.Lock()
	defer s.icons.mu.Unlock()
	customEmojiID, ok = s.icons.byEmoji[plainIcon(emoji)]
	return customEmojiID, ok
}

func (s *Tabs) offeredIcons() []string {
	s.icons.mu.Lock()
	defer s.icons.mu.Unlock()
	return s.icons.emoji
}

// plainIcon drops the variation selector, which Telegram and models add or leave out freely.
func plainIcon(emoji string) string {
	return strings.ReplaceAll(strings.TrimSpace(emoji), "\uFE0F", "")
}

var _ conversation.TabReplies = (*Tabs)(nil)

// TabReplied names a tab after its conversation once its first plain reply is delivered: a short
// name and an icon from the user's own subscription. Only a tab the bot named may be renamed, once
// per conversation, which the registry keeps across restarts; a tab the user named keeps its name.
// A failure leaves the tab as it is and is not retried. Neither the exchange nor the name is
// logged.
func (s *Tabs) TabReplied(ctx context.Context, reply conversation.TabReply) {
	if !s.topics.Load() || s.store == nil || s.namer == nil || reply.ThreadID <= 0 {
		return
	}
	key := chatTab{reply.ChatID, reply.ThreadID}
	if named, seen := s.namedTabs.Load(key); seen && named == reply.SessionID {
		return
	}
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	claimed, err := s.store.ClaimTabName(ctx, reply.PrincipalID, reply.ThreadID, reply.SessionID)
	if err != nil {
		logger.WarnContext(ctx, "telegram.tab.name_failed", "principal_id", reply.PrincipalID, "thread_id", reply.ThreadID, "reason", "claim", "error", err)
		return
	}
	s.namedTabs.Store(key, reply.SessionID)
	if !claimed {
		return
	}
	s.refreshTopicIcons(ctx)
	nameCtx, cancel := context.WithTimeout(ctx, tabNameTimeout)
	chosen, err := s.namer.NameTab(nameCtx, reply.PrincipalID, reply.Message, reply.Reply, s.offeredIcons())
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			logger.InfoContext(ctx, "telegram.tab.name_failed", "principal_id", reply.PrincipalID, "thread_id", reply.ThreadID, "reason", "model", "error", err)
		}
		return
	}
	// While the name was chosen, the user may have renamed the tab, or a later conversation there
	// may have claimed it; either one's name wins.
	tab, known, err := s.store.Tab(ctx, reply.PrincipalID, reply.ThreadID)
	if err != nil {
		logger.WarnContext(ctx, "telegram.tab.name_failed", "principal_id", reply.PrincipalID, "thread_id", reply.ThreadID, "reason", "read", "error", err)
		return
	}
	if !known || tab.NamedBy != thread.NamedByBot || tab.NamedSessionID != reply.SessionID {
		logger.InfoContext(ctx, "telegram.tab.name_failed", "principal_id", reply.PrincipalID, "thread_id", reply.ThreadID, "reason", "superseded")
		return
	}
	iconID := ""
	if chosen.Icon != "" {
		iconID, _ = s.IconFor(chosen.Icon)
	}
	// Telegram refuses an edit that changes nothing.
	if chosen.Name == tab.Name && (iconID == "" || iconID == tab.IconCustomEmojiID) {
		return
	}
	if err := s.client.EditForumTopic(ctx, reply.ChatID, reply.ThreadID, chosen.Name, iconID); err != nil {
		logger.WarnContext(ctx, "telegram.tab.name_failed", "principal_id", reply.PrincipalID, "thread_id", reply.ThreadID, "reason", "edit", "error", err)
		return
	}
	named, err := s.store.NameBotTab(ctx, reply.PrincipalID, reply.ThreadID, reply.SessionID, chosen.Name, iconID)
	if err != nil {
		logger.WarnContext(ctx, "telegram.tab.name_failed", "principal_id", reply.PrincipalID, "thread_id", reply.ThreadID, "reason", "record", "error", err)
		return
	}
	// A rename the user made around the edit keeps the user's name in the registry, but Telegram
	// may show the bot's until the user renames the tab again.
	if !named {
		logger.InfoContext(ctx, "telegram.tab.name_failed", "principal_id", reply.PrincipalID, "thread_id", reply.ThreadID, "reason", "superseded")
		return
	}
	logger.InfoContext(ctx, "telegram.tab.named", "principal_id", reply.PrincipalID, "thread_id", reply.ThreadID, "icon", iconID != "")
}

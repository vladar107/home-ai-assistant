package telegram

import (
	"context"
	"fmt"
	"sync"

	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// feedNames are the fixed names of the feed tabs.
var feedNames = map[string]string{
	thread.FeedReminders: "Reminders",
	thread.FeedReports:   "Reports",
	thread.FeedHome:      "Home",
	thread.FeedWatches:   "Watches",
}

// feedIcons are the emoji of the feed tabs' icons, used when Telegram's icon set offers them.
var feedIcons = map[string]string{
	thread.FeedReminders: "⏰",
	thread.FeedReports:   "📰",
	thread.FeedHome:      "🏠",
	thread.FeedWatches:   "👀",
}

// Feeds resolves the feed tabs of each principal's chat from the tab registry, making a feed's tab
// the first time it is needed.
type Feeds struct{ service *Service }

var _ thread.Feeds = Feeds{}

// Feeds is the resolver of this service's feed tabs; with tabs off every feed is the main view.
func (s *Service) Feeds() Feeds { return Feeds{service: s} }

// Thread returns the feed's tab, making it when the registry has none. Two deliveries of one feed
// at once make one tab. A tab the registry keeps is returned even while it refuses sends: it is
// suspect for its first day, its sends fall back to the main view, and only once it is taken for
// gone is its row dropped and a new one made. A failure is logged and returned, and the next
// delivery tries again.
func (f Feeds) Thread(ctx context.Context, principalID string, chatID int64, feed string) (int64, error) {
	s := f.service
	if s == nil || s.tabs == nil {
		return 0, nil
	}
	on, err := s.tabsOn(ctx)
	if err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.feed.failed", "principal_id", principalID, "feed", feed, "stage", "topics", "error", err)
		return 0, err
	}
	if !on {
		return 0, nil
	}
	name, known := feedNames[feed]
	if !known {
		return 0, fmt.Errorf("unknown feed %q", feed)
	}
	lock, _ := s.feedLocks.LoadOrStore(principalID+"\x00"+feed, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	logger := s.telemetry.ContextLogger(ctx, s.logger)
	subject := thread.FeedSubject(feed)
	threadID, found, err := s.tabs.SubjectTab(ctx, principalID, subject)
	if err != nil {
		logger.WarnContext(ctx, "telegram.feed.failed", "principal_id", principalID, "feed", feed, "stage", "load", "error", err)
		return 0, err
	}
	if found {
		s.giveFeedIcon(ctx, principalID, chatID, threadID, feed)
		return threadID, nil
	}
	threadID, err = s.client.CreateForumTopic(ctx, chatID, name)
	if err != nil {
		logger.WarnContext(ctx, "telegram.feed.failed", "principal_id", principalID, "feed", feed, "stage", "create", "error", err)
		return 0, err
	}
	if err := s.tabs.AddSubjectTab(ctx, principalID, threadID, name, subject); err != nil {
		logger.WarnContext(ctx, "telegram.feed.failed", "principal_id", principalID, "thread_id", threadID, "feed", feed, "stage", "record", "error", err)
		// A tab the registry does not hold would be left behind by the next delivery's new one.
		if removeErr := s.client.DeleteForumTopic(ctx, chatID, threadID); removeErr != nil {
			logger.WarnContext(ctx, "telegram.feed.failed", "principal_id", principalID, "thread_id", threadID, "feed", feed, "stage", "remove", "error", removeErr)
		}
		return 0, err
	}
	s.knownTabs.Store(chatTab{chatID, threadID}, struct{}{})
	logger.InfoContext(ctx, "telegram.feed.created", "principal_id", principalID, "thread_id", threadID, "feed", feed)
	s.giveFeedIcon(ctx, principalID, chatID, threadID, feed)
	return threadID, nil
}

// tabsOn reports the bot's tabs switch. Scheduled runs, watches, and trips start before the poller
// does, so the first of them may ask before Start has read the switch.
func (s *Service) tabsOn(ctx context.Context) (bool, error) {
	if !s.topicsKnown.Load() {
		bot, err := s.client.GetMe(ctx)
		if err != nil {
			return false, err
		}
		s.topics.Store(bot.HasTopicsEnabled)
		s.topicsKnown.Store(true)
	}
	return s.topics.Load(), nil
}

func (s *Service) giveFeedIcon(ctx context.Context, principalID string, chatID, threadID int64, feed string) {
	s.giveFixedIcon(ctx, principalID, chatID, threadID, feedIcons[feed], "telegram.feed.failed", "feed", feed)
}

// giveFixedIcon sets a fixed tab's icon once Telegram's icon set offers it, so a tab made before
// the set was read gets it with a later message. A tab that has its icon, or that refused it, is
// not asked again until a restart; a failure only costs the icon, and is logged as the event with
// the attributes that name the tab's kind.
func (s *Service) giveFixedIcon(ctx context.Context, principalID string, chatID, threadID int64, emoji, event string, kind ...any) {
	key := chatTab{chatID, threadID}
	if _, done := s.iconedFeeds.Load(key); done {
		return
	}
	iconID, offered := s.IconFor(emoji)
	if !offered {
		return
	}
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	failed := func(err error) {
		attributes := append([]any{"principal_id", principalID, "thread_id", threadID}, kind...)
		logger.WarnContext(ctx, event, append(attributes, "stage", "icon", "error", err)...)
	}
	tab, found, err := s.tabs.Tab(ctx, principalID, threadID)
	if err != nil {
		failed(err)
		return
	}
	s.iconedFeeds.Store(key, struct{}{})
	if !found || tab.IconCustomEmojiID == iconID {
		return
	}
	if err := s.client.EditForumTopic(ctx, chatID, threadID, "", iconID); err != nil {
		failed(err)
		return
	}
	if err := s.tabs.SetFixedTabIcon(ctx, principalID, threadID, iconID); err != nil {
		failed(err)
	}
}

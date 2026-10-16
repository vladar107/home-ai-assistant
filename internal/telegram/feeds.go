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
	// Scheduled runs and watches start before the poller does, so the first of them may ask
	// before Start has read the switch.
	if !s.topicsKnown.Load() {
		bot, err := s.client.GetMe(ctx)
		if err != nil {
			s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.feed.failed", "principal_id", principalID, "feed", feed, "stage", "topics", "error", err)
			return 0, err
		}
		s.topics.Store(bot.HasTopicsEnabled)
		s.topicsKnown.Store(true)
	}
	if !s.topics.Load() {
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
	return threadID, nil
}

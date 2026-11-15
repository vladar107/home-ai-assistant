package telegram

import (
	"context"
	"fmt"
	"sync"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// feedTabNames are the fixed names of the feed tabs in each language.
var feedTabNames = map[string]locale.Text{
	thread.FeedReminders: locale.T("Reminders", "Напоминания"),
	thread.FeedReports:   locale.T("Reports", "Отчёты"),
	thread.FeedHome:      locale.T("Home", "Дом"),
	thread.FeedWatches:   locale.T("Watches", "Наблюдения"),
	thread.FeedEvents:    locale.T("Events", "События"),
	thread.FeedInbox:     locale.T("Inbox", "Входящие"),
}

// feedNames are the English names, for text that names a feed tab.
var feedNames = func() map[string]string {
	names := make(map[string]string, len(feedTabNames))
	for feed, name := range feedTabNames {
		names[feed] = name.In(locale.English)
	}
	return names
}()

// feedIcons are the emoji of the feed tabs' icons, used when Telegram's icon set offers them.
var feedIcons = map[string]string{
	thread.FeedReminders: "⏰",
	thread.FeedReports:   "📰",
	thread.FeedHome:      "🏠",
	thread.FeedWatches:   "👀",
	thread.FeedEvents:    "🔔",
	thread.FeedInbox:     "📥",
}

var _ thread.Feeds = (*Tabs)(nil)

// Thread returns the feed's tab, making it when the registry has none. Two deliveries of one feed
// at once make one tab. A tab the registry keeps is returned even while it refuses sends: it is
// suspect for its first day, its sends fall back to the main view, and only once it is taken for
// gone is its row dropped and a new one made, by the delivery that found it gone. A failure is
// logged and returned, and the next delivery tries again.
func (s *Tabs) Thread(ctx context.Context, principalID string, chatID int64, feed string) (int64, error) {
	if s.store == nil {
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
	text, known := feedTabNames[feed]
	if !known {
		return 0, fmt.Errorf("unknown feed %q", feed)
	}
	name := text.Format(ctx)
	lock, _ := s.feedLocks.LoadOrStore(principalID+"\x00"+feed, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	logger := s.telemetry.ContextLogger(ctx, s.logger)
	subject := thread.FeedSubject(feed)
	threadID, found, err := s.store.SubjectTab(ctx, principalID, subject)
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
	if err := s.store.AddSubjectTab(ctx, principalID, threadID, name, subject); err != nil {
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

// Route returns the delivery's route to its destination, or with none its feed. Nothing is looked up
// until a send asks for the tab.
func (s *Tabs) Route(ctx context.Context, principalID string, chatID int64, destination, feed string) thread.Route {
	return &tabRoute{tabs: s, principalID: principalID, chatID: chatID, destination: destination, feed: feed, language: locale.From(ctx)}
}

// replaced records that a delivery's tab taken for gone was replaced, with Telegram's description
// of the refusal that found it gone; object says whether the replacement is an object's tab or the
// feed's.
func (s *Tabs) replaced(ctx context.Context, principalID string, gone, replacement int64, feed string, object bool, cause error) {
	attributes := []any{"principal_id", principalID, "gone_thread_id", gone, "thread_id", replacement, "feed", feed, "object", object}
	s.telemetry.ContextLogger(ctx, s.logger).InfoContext(ctx, "telegram.feed.replaced", append(attributes, refusalAttributes(cause)...)...)
	s.telemetry.RecordTab(ctx, principalID, "tab_replaced")
}

// tabRoute finds its tab with the first send that asks, under that send's context, so the send's
// deadline, cancellation, and trace cover the lookup, and names a tab it makes in the language of
// the context it was made under. A lookup the sending context ended is not kept, so the next send
// tries again; any other failure leaves the route in the main view, as a delivery whose tab cannot
// be had always was. Concurrent sends wait for one lookup.
type tabRoute struct {
	tabs              *Tabs
	principalID       string
	chatID            int64
	destination, feed string
	language          locale.Language
	mu                sync.Mutex
	resolved          bool
	threadID          int64
	// replaced is set once the route looked for a replacement, which it does once.
	replaced bool
}

func (r *tabRoute) Tab(ctx context.Context) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resolved {
		return r.threadID
	}
	threadID, _, err := r.tabs.destination(r.lookupContext(ctx), r.principalID, r.chatID, r.destination, r.feed)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		threadID = 0
	}
	r.resolved, r.threadID = true, threadID
	return threadID
}

func (r *tabRoute) Peek(ctx context.Context) (int64, bool) {
	r.mu.Lock()
	resolved, threadID := r.resolved, r.threadID
	r.mu.Unlock()
	if resolved {
		return threadID, true
	}
	return r.tabs.peekDestination(r.lookupContext(ctx), r.principalID, r.destination, r.feed)
}

func (r *tabRoute) Resolved() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.threadID
}

// Replace finds the tab that takes over from gone, a tab the observer took for gone and whose row
// it dropped. The route looks itself up again, under the lock of its feed or object: a feed gets a
// new tab, or the one a concurrent delivery just made; an open object gets a new tab with a fresh
// summary, and any other its feed. A route whose tab is already another, because another send of
// it replaced gone, keeps that one. The route looks once: when that finds nothing, finds gone again
// because its row could not be dropped, or is asked again because its replacement was refused too,
// it sends to the main view from then on.
func (r *tabRoute) Replace(ctx context.Context, gone int64, cause error) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resolved && r.threadID != gone {
		return r.threadID, r.threadID > 0
	}
	r.resolved, r.threadID = true, 0
	if r.replaced {
		return 0, false
	}
	r.replaced = true
	replacement, object, err := r.tabs.destination(r.lookupContext(ctx), r.principalID, r.chatID, r.destination, r.feed)
	if err != nil || replacement <= 0 || replacement == gone {
		return 0, false
	}
	r.threadID = replacement
	r.tabs.replaced(ctx, r.principalID, gone, replacement, r.feed, object, cause)
	return replacement, true
}

// lookupContext is the sending context in the route's language, without the route, so a message
// the lookup sends, such as a new object tab's summary, never asks the route again.
func (r *tabRoute) lookupContext(ctx context.Context) context.Context {
	return locale.With(thread.WithDelivery(ctx, 0), r.language)
}

// tabsOn reports the bot's tabs switch. Scheduled runs, watches, and trips start before the poller
// does, so the first of them may ask before Start has read the switch.
func (s *Tabs) tabsOn(ctx context.Context) (bool, error) {
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

func (s *Tabs) giveFeedIcon(ctx context.Context, principalID string, chatID, threadID int64, feed string) {
	s.giveFixedIcon(ctx, principalID, chatID, threadID, feedIcons[feed], "telegram.feed.failed", "feed", feed)
}

// giveFixedIcon sets a fixed tab's icon once Telegram's icon set offers it, so a tab made before
// the set was read gets it with a later message. A tab that has its icon, or that refused it, is
// not asked again until a restart; a failure only costs the icon, and is logged as the event with
// the attributes that name the tab's kind. A tab the user named, such as one a trip took, keeps the
// icon the user gave it.
func (s *Tabs) giveFixedIcon(ctx context.Context, principalID string, chatID, threadID int64, emoji, event string, kind ...any) {
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
	tab, found, err := s.store.Tab(ctx, principalID, threadID)
	if err != nil {
		failed(err)
		return
	}
	s.iconedFeeds.Store(key, struct{}{})
	if !found || tab.NamedBy != thread.NamedByFixed || tab.IconCustomEmojiID == iconID {
		return
	}
	if err := s.client.EditForumTopic(ctx, chatID, threadID, "", iconID); err != nil {
		failed(err)
		return
	}
	if err := s.store.SetFixedTabIcon(ctx, principalID, threadID, iconID); err != nil {
		failed(err)
	}
}

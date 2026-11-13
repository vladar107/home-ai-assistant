package telegram

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const (
	// watchTabIcon is the emoji of an agent watch tab's icon, a magnifying glass, used when
	// Telegram's icon set offers it.
	watchTabIcon = "\U0001F50E"
	// watchTabFallbackName names the tab of a watch whose name leaves nothing a tab can show.
	watchTabFallbackName = "Watch"
)

// WatchReader is what the watch tabs need of the watches: one watch and the record of its summary
// message.
type WatchReader interface {
	Watch(ctx context.Context, principalID, id string) (watch.Watch, bool, error)
	// SetSummaryMessage records the message that sums the watch up in its tab.
	SetSummaryMessage(ctx context.Context, principalID, id string, messageID int64) error
}

// WatchFeeds keeps each agent watch's tab in step with the watch; the service's Tabs implement it.
type WatchFeeds interface {
	// SummarizeWatch gives an agent watch its tab when it has none, and keeps its pinned summary
	// and the tab's name current.
	SummarizeWatch(ctx context.Context, principalID string, chatID int64, watchID string)
}

var _ WatchFeeds = (*Tabs)(nil)

// SummarizeWatch keeps an agent watch's tab in step after a write to the watch: one without a tab
// gets one, its summary is edited, or sent and pinned again when it is gone, and the tab is renamed
// when the watch's name changed. A failure is logged and left to the next write.
func (s *Tabs) SummarizeWatch(ctx context.Context, principalID string, chatID int64, watchID string) {
	s.summarize(ctx, thread.ObjectWatch, principalID, chatID, watchID)
}

// watchKind is the agent watches as their tabs see them. A watch of any other kind is unknown here,
// so its alerts resolve to their feed and it never gets a tab. An agent watch gets a tab while it
// exists, paused or not, and the tab of one that was removed stays until the user closes it.
func watchKind(watches WatchReader) objectKind {
	return objectKind{
		kind: thread.ObjectWatch, icon: watchTabIcon, fallbackName: watchTabFallbackName,
		event: "telegram.watch_tab", idKey: "watch_id",
		load: func(ctx context.Context, principalID, id string) (tabObject, bool, error) {
			found, known, err := watches.Watch(ctx, principalID, id)
			if err != nil || !known || found.Kind != watch.KindAgent {
				return tabObject{}, false, err
			}
			return tabObject{
				ID: found.ID, Title: found.Label(), SummaryMessageID: found.SummaryMessageID,
				Open: true, Summary: clipUnits(watch.Summary(found), messageLimit),
			}, true, nil
		},
		setSummary: watches.SetSummaryMessage,
	}
}

// WatchNotifier keeps each agent watch's tab in step with the watch in the principal's active chat.
// Its messages carry no markup and no link previews.
type WatchNotifier struct {
	Chats TripChats
	// Feeds keeps the tabs; nil leaves watches without tabs of their own.
	Feeds WatchFeeds
}

var _ watch.Tabs = WatchNotifier{}

// WatchChanged keeps the watch's tab and its summary in step with a write to the watch. A failure
// is logged by the resolver and leaves the next write to try again.
func (n WatchNotifier) WatchChanged(ctx context.Context, principalID, watchID string) {
	if n.Feeds == nil || n.Chats == nil {
		return
	}
	chatID, found, err := n.Chats.ResolveActiveChat(ctx, principalID)
	if err != nil || !found {
		return
	}
	n.Feeds.SummarizeWatch(ctx, principalID, chatID, watchID)
}

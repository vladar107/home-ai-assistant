package telegram

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// TripReader is what the trip tabs need of the trips: one trip, the record of its summary message,
// and the clock that says whether it has ended.
type TripReader interface {
	Trip(ctx context.Context, principalID, id string) (trip.Trip, bool, error)
	// SetSummaryMessage records the message that sums the trip up in its tab.
	SetSummaryMessage(ctx context.Context, principalID, id string, messageID int64) error
	// Now is the trips' clock, which says whether a trip has ended.
	Now() time.Time
}

// Tabs is what the bot knows of the tabs in its users' chats: the tab registry, the feed and trip
// tabs that messages nobody asked for are delivered to, and the names tabs get after their
// conversations. It is built before the services that deliver into tabs, and works before the
// poller starts.
type Tabs struct {
	client    botAPI
	store     TabStore
	trips     TripReader
	namer     TabNamer
	logger    *slog.Logger
	telemetry Telemetry

	// topics is the bot's tabs switch as getMe last reported it.
	topics atomic.Bool
	// topicsKnown is set once getMe has answered, which may be before Start for a feed delivery.
	topicsKnown atomic.Bool
	// confirmedTabs holds the tabs the registry holds as confirmed, so a send there writes nothing.
	confirmedTabs sync.Map
	// knownTabs holds the tabs the registry has a row for, so a message there adds none.
	knownTabs sync.Map
	// feedLocks holds a lock per principal and feed or trip, so deliveries to one of them at once
	// make one tab.
	feedLocks sync.Map
	// iconedFeeds holds the feed and trip tabs whose fixed icon was settled since start.
	iconedFeeds sync.Map
	// pinFailures holds the trips whose summary could not be pinned since start, so each is logged
	// once.
	pinFailures sync.Map
	// namedTabs holds the conversation each tab was last considered for naming after, so a reply
	// there asks the registry once per conversation.
	namedTabs sync.Map
	icons     topicIcons
}

// NewTabs returns the tabs over the registry. A nil store leaves tabs unrecorded and unnamed and
// takes every refused tab for gone, a nil trips leaves trips without tabs, and a nil namer leaves
// tabs with their first name.
func NewTabs(client botAPI, store TabStore, trips TripReader, namer TabNamer, logger *slog.Logger, telemetry Telemetry) *Tabs {
	return &Tabs{client: client, store: store, trips: trips, namer: namer, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

// setTopics records the bot's tabs switch as getMe reported it.
func (s *Tabs) setTopics(enabled bool) {
	s.topics.Store(enabled)
	s.topicsKnown.Store(true)
}

func (s *Tabs) enabled() bool { return s.topics.Load() }

func (s *Tabs) confirmed(chatID, threadID int64) bool {
	_, confirmed := s.confirmedTabs.Load(chatTab{chatID, threadID})
	return confirmed
}

func (s *Tabs) confirm(ctx context.Context, chatID int64, principalID string, threadID int64) {
	if s.store == nil || s.confirmed(chatID, threadID) {
		return
	}
	confirmed, err := s.store.ConfirmTab(ctx, principalID, threadID)
	if err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.tab.confirm_failed", "principal_id", principalID, "thread_id", threadID, "error", err)
		return
	}
	// A tab the registry does not know yet may be added later, and must be confirmed then.
	if confirmed {
		s.confirmedTabs.Store(chatTab{chatID, threadID}, struct{}{})
	}
}

// suspect reports whether a refused tab should be kept. A tab whose state cannot be read is kept.
func (s *Tabs) suspect(ctx context.Context, lane conversation.Lane) bool {
	if s.store == nil {
		return false
	}
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	last, wrote, err := s.store.LaneLastRequest(ctx, lane)
	if err != nil {
		logger.WarnContext(ctx, "telegram.tab.read_failed", "principal_id", lane.PrincipalID, "thread_id", lane.ThreadID, "error", err)
		return true
	}
	if wrote && time.Since(last) < tabSuspectWindow {
		return true
	}
	// A message of the user's confirms the tab it came from, so an unconfirmed tab is one the bot made.
	tab, known, err := s.store.Tab(ctx, lane.PrincipalID, lane.ThreadID)
	if err != nil {
		logger.WarnContext(ctx, "telegram.tab.read_failed", "principal_id", lane.PrincipalID, "thread_id", lane.ThreadID, "error", err)
		return true
	}
	return known && !tab.Confirmed && time.Since(tab.CreatedAt) < tabSuspectWindow
}

func (s *Tabs) forget(ctx context.Context, chatID int64, lane conversation.Lane) {
	s.confirmedTabs.Delete(chatTab{chatID, lane.ThreadID})
	s.knownTabs.Delete(chatTab{chatID, lane.ThreadID})
	s.namedTabs.Delete(chatTab{chatID, lane.ThreadID})
	s.iconedFeeds.Delete(chatTab{chatID, lane.ThreadID})
	if s.store == nil {
		return
	}
	if err := s.store.DeleteTab(ctx, lane.PrincipalID, lane.ThreadID); err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.tab.forget_failed", "principal_id", lane.PrincipalID, "thread_id", lane.ThreadID, "error", err)
	}
}

// recordBotTab adds a tab the bot made to the registry before anything is sent there.
func (s *Tabs) recordBotTab(ctx context.Context, principalID string, threadID int64, name string) {
	if s.store == nil {
		return
	}
	if _, err := s.store.AddTab(ctx, principalID, threadID, name, thread.NamedByBot); err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.tab.record_failed", "principal_id", principalID, "thread_id", threadID, "error", err)
	}
}

// noteTab learns from a message of the user's in a tab: the tab exists, and the message replies to
// the one that created the tab, which names it. That root is on every message in a tab, so it only
// adds a tab the registry does not know and never changes a known one, and a tab once known is not
// looked up again until it is forgotten.
func (s *Tabs) noteTab(ctx context.Context, bot Bot, principalID string, message *Message) {
	threadID := thread.From(ctx)
	if s.store == nil || threadID <= 0 {
		return
	}
	key := chatTab{message.Chat.ID, threadID}
	_, known := s.knownTabs.Load(key)
	if root := message.ReplyToMessage; !known && root != nil && root.ForumTopicCreated != nil {
		if name := topicName(root.ForumTopicCreated.Name); name != "" {
			// A tab first seen through the root its messages reply to was open before the bot
			// recorded it, and a tab that was already there keeps its name.
			added, err := s.store.AddTab(ctx, principalID, threadID, name, thread.NamedByUser)
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
	s.confirm(ctx, message.Chat.ID, principalID, threadID)
}

// addTab records a tab a topic service message announced; without a registry it records nothing.
func (s *Tabs) addTab(ctx context.Context, principalID string, threadID int64, name, namedBy string) error {
	if s.store == nil {
		return nil
	}
	_, err := s.store.AddTab(ctx, principalID, threadID, name, namedBy)
	return err
}

// renameTab records a rename the user made; without a registry it records nothing.
func (s *Tabs) renameTab(ctx context.Context, principalID string, threadID int64, name string) error {
	if s.store == nil {
		return nil
	}
	_, err := s.store.RenameTab(ctx, principalID, threadID, name)
	return err
}

// logTopicService records which fields a topic service message carried, never the name.
func (s *Tabs) logTopicService(ctx context.Context, principalID string, threadID int64, kind, source string, byBot, name, implicit, icon bool) {
	sender := "user"
	if byBot {
		sender = "bot"
	}
	s.telemetry.ContextLogger(ctx, s.logger).InfoContext(ctx, "telegram.topic.service",
		"principal_id", principalID, "thread_id", threadID, "kind", kind, "source", source, "sender", sender,
		"name_present", name, "name_implicit", implicit, "icon_present", icon)
}

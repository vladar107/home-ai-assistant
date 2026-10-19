package telegram

import (
	"context"
	"sync"

	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

const (
	// tripTabIcon is the emoji of a trip tab's icon, used when Telegram's icon set offers it.
	tripTabIcon = "✈️"
	// tripTabFallbackName names the tab of a trip whose title leaves nothing a tab can show.
	tripTabFallbackName = "Trip"
)

var _ TripFeeds = (*Tabs)(nil)

// Destination resolves a trip's destination to the trip's tab, which a planned trip that has none
// gets now. Anything else, including a trip that ended, was cancelled, or no longer exists and has
// no tab, resolves to the feed. A trip whose tab cannot be had, or was just found gone, falls back
// to the feed as well.
func (s *Tabs) Destination(ctx context.Context, principalID string, chatID int64, destination, feed string) (int64, error) {
	if tripID, isTrip := thread.TripOf(destination); isTrip {
		if threadID, err := s.tripThread(ctx, principalID, chatID, tripID, false); err == nil && threadID > 0 {
			return threadID, nil
		}
	}
	return s.Thread(ctx, principalID, chatID, feed)
}

// Summarize keeps a trip's tab in step after a write to the trip: a planned trip without a tab gets
// one, its summary is edited, or sent and pinned again when it is gone, and the tab is renamed when
// the title changed. A failure is logged and left to the next write.
func (s *Tabs) Summarize(ctx context.Context, principalID string, chatID int64, tripID string) {
	_, _ = s.tripThread(ctx, principalID, chatID, tripID, true)
}

// tripThread returns the trip's tab, or zero when it has none and gets none. A trip keeps its tab
// whatever becomes of it, so its end notice lands there; only a planned trip that has not ended is
// given one. The registry is the only record of the tab, so a tab taken for gone, whose row is
// dropped, is made again with a fresh summary. With refresh set, the summary and the tab's name are
// brought up to date; otherwise only a missing summary is sent. One trip's tab is resolved by one
// caller at a time.
func (s *Tabs) tripThread(ctx context.Context, principalID string, chatID int64, tripID string, refresh bool) (int64, error) {
	if s.store == nil || s.trips == nil {
		return 0, nil
	}
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	on, err := s.tabsOn(ctx)
	if err != nil {
		logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", tripID, "stage", "topics", "error", err)
		return 0, err
	}
	if !on {
		return 0, nil
	}
	subject := thread.TripDestination(tripID)
	lock, _ := s.feedLocks.LoadOrStore(principalID+"\x00"+subject, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	found, known, err := s.trips.Trip(ctx, principalID, tripID)
	if err != nil {
		logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", tripID, "stage", "load", "error", err)
		return 0, err
	}
	if !known {
		return 0, nil
	}
	threadID, tabbed, err := s.store.SubjectTab(ctx, principalID, subject)
	if err != nil {
		logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", tripID, "stage", "load", "error", err)
		return 0, err
	}
	if !tabbed {
		if found.Status != trip.StatusPlanned || found.Ended(s.trips.Now()) {
			return 0, nil
		}
		if threadID, err = s.makeTripTab(ctx, principalID, chatID, found); err != nil {
			return 0, err
		}
		if !s.sendTripSummary(ctx, principalID, chatID, threadID, found) {
			return 0, nil
		}
		return threadID, nil
	}
	s.giveFixedIcon(ctx, principalID, chatID, threadID, tripTabIcon, "telegram.trip_tab.failed", "trip_id", tripID)
	if refresh {
		s.renameTripTab(ctx, principalID, chatID, threadID, found)
	}
	kept := true
	switch {
	case found.SummaryMessageID <= 0:
		kept = s.sendTripSummary(ctx, principalID, chatID, threadID, found)
	case refresh:
		err := editPlain(ctx, s.client, chatID, found.SummaryMessageID, trip.Summary(found, s.trips.Now()))
		switch {
		case err != nil && isPermanentAPIError(err):
			// Telegram no longer has the summary to edit, so it is sent and pinned again.
			kept = s.sendTripSummary(ctx, principalID, chatID, threadID, found)
		case err != nil:
			logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", tripID, "thread_id", threadID, "stage", "edit_summary", "error", err)
		}
	}
	if !kept {
		return 0, nil
	}
	return threadID, nil
}

// makeTripTab opens the trip's tab under the trip's title and records it as the trip's, with a fixed
// name that only a new title changes.
func (s *Tabs) makeTripTab(ctx context.Context, principalID string, chatID int64, found trip.Trip) (int64, error) {
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	name := tripTabName(found)
	threadID, err := s.client.CreateForumTopic(ctx, chatID, name)
	if err != nil {
		logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", found.ID, "stage", "create", "error", err)
		return 0, err
	}
	if err := s.store.AddSubjectTab(ctx, principalID, threadID, name, thread.TripDestination(found.ID)); err != nil {
		logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", found.ID, "thread_id", threadID, "stage", "record", "error", err)
		// A tab the registry does not hold would be left behind by the next message's new one.
		if removeErr := s.client.DeleteForumTopic(ctx, chatID, threadID); removeErr != nil {
			logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", found.ID, "thread_id", threadID, "stage", "remove", "error", removeErr)
		}
		return 0, err
	}
	s.knownTabs.Store(chatTab{chatID, threadID}, struct{}{})
	logger.InfoContext(ctx, "telegram.trip_tab.created", "principal_id", principalID, "trip_id", found.ID, "thread_id", threadID)
	s.giveFixedIcon(ctx, principalID, chatID, threadID, tripTabIcon, "telegram.trip_tab.failed", "trip_id", found.ID)
	return threadID, nil
}

// sendTripSummary posts the trip's summary in its tab, with sound like any message, records it,
// and pins it, which in a private chat is silent. The summary goes only to the tab: one in the main
// view would be pinned there. A pin Telegram refuses leaves the summary as the tab's first message
// and is logged once per trip. It reports false when Telegram's refusal had the tab taken for gone.
func (s *Tabs) sendTripSummary(ctx context.Context, principalID string, chatID, threadID int64, found trip.Trip) bool {
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	messageID, err := s.client.SendPlainInTab(thread.WithDelivery(ctx, threadID), chatID, trip.Summary(found, s.trips.Now()))
	if err != nil {
		logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", found.ID, "thread_id", threadID, "stage", "send_summary", "error", err)
		verdict, refused := tabRefusal(err)
		return !refused || verdict != TabGone
	}
	// A summary that is not recorded is sent again by the next write, which pins a second one.
	for attempt := 1; attempt <= 2; attempt++ {
		if err = s.trips.SetSummaryMessage(ctx, principalID, found.ID, messageID); err == nil {
			break
		}
		logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", found.ID, "thread_id", threadID, "stage", "record_summary", "attempt", attempt, "error", err)
	}
	if err := s.client.PinChatMessage(ctx, chatID, messageID); err != nil {
		if _, logged := s.pinFailures.LoadOrStore(principalID+"\x00"+found.ID, struct{}{}); !logged {
			logger.WarnContext(ctx, "telegram.trip_tab.pin_failed", "principal_id", principalID, "trip_id", found.ID, "thread_id", threadID, "error", err)
		}
	}
	return true
}

// renameTripTab names the tab after the trip's title once the title changed.
func (s *Tabs) renameTripTab(ctx context.Context, principalID string, chatID, threadID int64, found trip.Trip) {
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	tab, known, err := s.store.Tab(ctx, principalID, threadID)
	name := tripTabName(found)
	if err != nil || !known || tab.Name == name {
		if err != nil {
			logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", found.ID, "thread_id", threadID, "stage", "rename", "error", err)
		}
		return
	}
	if err := s.client.EditForumTopic(ctx, chatID, threadID, name, ""); err != nil {
		logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", found.ID, "thread_id", threadID, "stage", "rename", "error", err)
		return
	}
	if err := s.store.SetFixedTabName(ctx, principalID, threadID, name); err != nil {
		logger.WarnContext(ctx, "telegram.trip_tab.failed", "principal_id", principalID, "trip_id", found.ID, "thread_id", threadID, "stage", "rename", "error", err)
	}
}

func tripTabName(found trip.Trip) string {
	if name := topicName(found.Title); name != "" {
		return name
	}
	return tripTabFallbackName
}

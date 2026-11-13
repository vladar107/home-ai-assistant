package telegram

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

const (
	// tripTabIcon is the emoji of a trip tab's icon, used when Telegram's icon set offers it.
	tripTabIcon = "✈️"
	// tripTabFallbackName names the tab of a trip whose title leaves nothing a tab can show.
	tripTabFallbackName = "Trip"

	// TripMovedNote replaces the summary in the tab a trip left, above the trip's title.
	TripMovedNote = "This trip moved to another tab."
	// TripTabPassedNote replaces the summary of a trip that is over in its tab, above its title,
	// once another trip took the tab.
	TripTabPassedNote = "This trip is over, and this tab now belongs to another trip."

	tripTabMainViewRefusal = "This is the main view, which cannot be a trip's tab. The user can say it in the tab they want the trip in."
	tripTabUnknownRefusal  = "This tab is not in my records yet, so a trip cannot use it."
	tripTabOtherRefusal    = "This tab cannot be a trip's tab."
	tripTabObjectRefusal   = "This tab is %s, so it cannot be a trip's tab."
	tripTabUnnamedRefusal  = "The user did not name this tab."
	tripTabChangedRefusal  = "This tab changed while the trip was moving to it; nothing moved."
	tripGoneRefusal        = "That trip no longer exists."
	tripClosedRefusal      = "That trip has ended or is no longer planned, so it does not move to another tab."
)

// tripTabHolders say whose tab a trip is refused, by the kind of object the tab serves.
var tripTabHolders = map[string]string{
	thread.ObjectProject: "a project's",
	thread.ObjectWatch:   "a watch's",
}

var _ TripFeeds = (*Tabs)(nil)

// Summarize keeps a trip's tab in step after a write to the trip: a planned trip without a tab gets
// one, its summary is edited, or sent and pinned again when it is gone, and the tab is renamed when
// the title changed. A failure is logged and left to the next write.
func (s *Tabs) Summarize(ctx context.Context, principalID string, chatID int64, tripID string) {
	s.summarize(ctx, thread.ObjectTrip, principalID, chatID, tripID)
}

// CheckTripTab says whether the trip, or with no ID a trip about to be added, could use the tab;
// with named, only a tab the user named will do.
func (s *Tabs) CheckTripTab(ctx context.Context, principalID, tripID string, threadID int64, named bool) (trip.TabMove, error) {
	kind, known := s.objects[thread.ObjectTrip]
	if !known {
		return trip.TabMove{Refusal: trip.TabsOffRefusal}, nil
	}
	move, _, _, err := s.checkTripTab(ctx, kind, principalID, tripID, threadID, named)
	return move, err
}

// TakeTripTab makes the tab the trip's in place of the tab it had, or of the one it would get,
// under the lock that orders the trip's tab work, so no tab is made for it meanwhile, and, for the
// tab of a trip that is over, under that trip's lock too, so nothing of that trip lands there. The
// tab keeps the name and icon the user gave it; one the bot made for the trip that is over takes
// the new trip's name and icon. The summary in the tab the trip left says it moved, that of the
// trip that is over says the tab passed on, and the trip's summary is sent and pinned in the taken
// tab; when that send is lost, the trip's next message sends it. The tab it left stays with its
// messages.
func (s *Tabs) TakeTripTab(ctx context.Context, principalID string, chatID int64, tripID string, threadID int64, named bool) (trip.TabMove, error) {
	kind, known := s.objects[thread.ObjectTrip]
	if !known || s.store == nil {
		return trip.TabMove{Refusal: trip.TabsOffRefusal}, nil
	}
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	failed := kind.event + ".failed"
	subject := thread.TripDestination(tripID)
	held := ""
	if threadID > 0 {
		tab, known, err := s.store.Tab(ctx, principalID, threadID)
		if err != nil {
			logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, tripID, "thread_id", threadID, "stage", "load", "error", err)
			return trip.TabMove{}, err
		}
		if heldKind, _, isObject := thread.ObjectOf(tab.Subject); known && isObject && heldKind == thread.ObjectTrip && tab.Subject != subject {
			held = tab.Subject
		}
	}
	defer s.lockObjects(principalID, subject, held)()

	found, exists, err := kind.load(ctx, principalID, tripID)
	switch {
	case err != nil:
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, tripID, "stage", "load", "error", err)
		return trip.TabMove{}, err
	case !exists:
		return trip.TabMove{Refusal: tripGoneRefusal}, nil
	case !found.Open:
		return trip.TabMove{Refusal: tripClosedRefusal}, nil
	}
	move, holder, previous, err := s.checkTripTab(ctx, kind, principalID, tripID, threadID, named)
	if err != nil || move.Refusal != "" || move.Already {
		return move, err
	}
	if holder != "" && holder != held {
		return trip.TabMove{Refusal: tripTabChangedRefusal}, nil
	}
	left, hadTab, err := s.store.SubjectTab(ctx, principalID, subject)
	if err != nil {
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, tripID, "stage", "load", "error", err)
		return trip.TabMove{}, err
	}
	taken, err := s.store.TakeTab(ctx, principalID, threadID, subject, holder)
	if err != nil {
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, tripID, "thread_id", threadID, "stage", "take", "error", err)
		return trip.TabMove{}, err
	}
	if !taken {
		return trip.TabMove{Refusal: tripTabChangedRefusal}, nil
	}
	logger.InfoContext(ctx, kind.event+".taken", "principal_id", principalID, kind.idKey, tripID, "thread_id", threadID, "left_thread_id", left)
	if hadTab && found.SummaryMessageID > 0 {
		if err := editPlain(ctx, s.client, chatID, found.SummaryMessageID, TripMovedNote+"\n"+found.Title); err != nil {
			logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, tripID, "thread_id", left, "stage", "moved_note", "error", err)
		}
	}
	if previous.SummaryMessageID > 0 {
		if err := editPlain(ctx, s.client, chatID, previous.SummaryMessageID, TripTabPassedNote+"\n"+previous.Title); err != nil {
			logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, tripID, "thread_id", threadID, "stage", "passed_note", "error", err)
		}
	}
	s.giveFixedIcon(ctx, principalID, chatID, threadID, kind.icon, failed, kind.idKey, tripID)
	s.renameObjectTab(ctx, kind, principalID, chatID, threadID, found)
	found.SummaryMessageID = 0
	s.sendObjectSummary(ctx, kind, principalID, chatID, threadID, found)
	return trip.TabMove{Left: hadTab}, nil
}

// lockObjects takes the locks that order the tab work of each subject given, in one order so two
// callers never wait on each other, and returns what releases them.
func (s *Tabs) lockObjects(principalID string, subjects ...string) func() {
	var keys []string
	for _, subject := range subjects {
		if subject != "" {
			keys = append(keys, principalID+"\x00"+subject)
		}
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	locks := make([]*sync.Mutex, 0, len(keys))
	for _, key := range keys {
		lock, _ := s.feedLocks.LoadOrStore(key, &sync.Mutex{})
		lock.(*sync.Mutex).Lock()
		locks = append(locks, lock.(*sync.Mutex))
	}
	return func() {
		for index := len(locks) - 1; index >= 0; index-- {
			locks[index].Unlock()
		}
	}
}

// checkTripTab says whether the trip could use the tab, and returns the subject the tab serves now
// with the trip that is over it served, if any.
// A tab of conversations can be used, as can the tab of a trip that ended, was cancelled, or is
// gone; the main view, a feed's tab, an open trip's, the tab of another kind of object such as a
// project or a watch, and a tab the registry does not know cannot. With named, the tab must be one
// the user named.
func (s *Tabs) checkTripTab(ctx context.Context, kind objectKind, principalID, tripID string, threadID int64, named bool) (trip.TabMove, string, tabObject, error) {
	refused := func(text string) (trip.TabMove, string, tabObject, error) {
		return trip.TabMove{Refusal: text}, "", tabObject{}, nil
	}
	if s.store == nil {
		return refused(trip.TabsOffRefusal)
	}
	on, err := s.tabsOn(ctx)
	if err != nil {
		return trip.TabMove{}, "", tabObject{}, err
	}
	if !on {
		return refused(trip.TabsOffRefusal)
	}
	if threadID <= 0 {
		return refused(tripTabMainViewRefusal)
	}
	tab, known, err := s.store.Tab(ctx, principalID, threadID)
	if err != nil {
		return trip.TabMove{}, "", tabObject{}, err
	}
	if !known {
		return refused(tripTabUnknownRefusal)
	}
	if tripID != "" && tab.Subject == thread.TripDestination(tripID) {
		return trip.TabMove{Already: true}, "", tabObject{}, nil
	}
	if feed, isFeed := strings.CutPrefix(tab.Subject, thread.FeedSubject("")); isFeed {
		return refused(fmt.Sprintf("This is the %s tab, which keeps its own kind of messages, so it cannot be a trip's tab.", feedNames[feed]))
	}
	var previous tabObject
	switch heldKind, heldID, held := thread.ObjectOf(tab.Subject); {
	case tab.Subject == "" && tab.NamedBy == thread.NamedByFixed, tab.Subject != "" && !held:
		return refused(tripTabOtherRefusal)
	case held && heldKind != thread.ObjectTrip:
		return refused(fmt.Sprintf(tripTabObjectRefusal, tripTabHolders[heldKind]))
	case held:
		other, exists, err := kind.load(ctx, principalID, heldID)
		if err != nil {
			return trip.TabMove{}, "", tabObject{}, err
		}
		if exists && other.Open {
			return refused(fmt.Sprintf("This tab is the trip %q's, which has not ended, so it cannot be another trip's tab.", other.Title))
		}
		previous = other
	}
	if named && tab.NamedBy != thread.NamedByUser {
		return refused(tripTabUnnamedRefusal)
	}
	return trip.TabMove{}, tab.Subject, previous, nil
}

// tripKind is the trips as their tabs see them. A trip keeps its tab whatever becomes of it, so
// its end notice lands there; only a planned trip that has not ended is given one.
func tripKind(trips TripReader) objectKind {
	return objectKind{
		kind: thread.ObjectTrip, icon: tripTabIcon, fallbackName: tripTabFallbackName,
		event: "telegram.trip_tab", idKey: "trip_id",
		load: func(ctx context.Context, principalID, id string) (tabObject, bool, error) {
			found, known, err := trips.Trip(ctx, principalID, id)
			if err != nil || !known {
				return tabObject{}, known, err
			}
			now := trips.Now()
			return tabObject{
				ID: found.ID, Title: found.Title, SummaryMessageID: found.SummaryMessageID,
				Open:    found.Status == trip.StatusPlanned && !found.Ended(now),
				Summary: trip.Summary(found, now),
			}, true, nil
		},
		setSummary: trips.SetSummaryMessage,
	}
}

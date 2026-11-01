package telegram

import (
	"context"
	"sync"

	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// tabObject is an object with a tab of its own, such as a trip or a project, as its tab needs it.
type tabObject struct {
	ID               string
	Title            string
	SummaryMessageID int64
	// Open is whether the object gets a tab when it has none.
	Open bool
	// Summary is the text pinned in its tab.
	Summary string
}

// objectKind is what the tabs need of one kind of object with a tab of its own: how to read one,
// how to record its summary, and how its tab looks. Each kind is registered once, by its kind in
// thread.ObjectKinds.
type objectKind struct {
	kind string
	// icon is the emoji of the tab's icon, used when Telegram's icon set offers it, and
	// fallbackName names the tab of an object whose title leaves nothing a tab can show.
	icon, fallbackName string
	// event opens the names of the kind's log events, such as telegram.trip_tab, and idKey is the
	// attribute its ID is logged under.
	event, idKey string
	load         func(ctx context.Context, principalID, id string) (tabObject, bool, error)
	setSummary   func(ctx context.Context, principalID, id string, messageID int64) error
}

var _ thread.Feeds = (*Tabs)(nil)

// Destination resolves an object's destination, such as a trip's, to the object's tab, which an
// open object that has none gets now. Anything else, including an object that is no longer open,
// or no longer exists, and has no tab, resolves to the feed. An object whose tab cannot be had, or
// was just found gone, falls back to the feed as well.
func (s *Tabs) Destination(ctx context.Context, principalID string, chatID int64, destination, feed string) (int64, error) {
	if kind, id, isObject := thread.ObjectOf(destination); isObject {
		if registered, known := s.objects[kind]; known {
			if threadID, err := s.objectThread(ctx, registered, principalID, chatID, id, false); err == nil && threadID > 0 {
				return threadID, nil
			}
		}
	}
	return s.Thread(ctx, principalID, chatID, feed)
}

// summarize keeps an object's tab in step after a write to it: an open object without a tab gets
// one, its summary is edited, or sent and pinned again when it is gone, and the tab is renamed when
// the title changed. A failure is logged and left to the next write.
func (s *Tabs) summarize(ctx context.Context, kind, principalID string, chatID int64, id string) {
	if registered, known := s.objects[kind]; known {
		_, _ = s.objectThread(ctx, registered, principalID, chatID, id, true)
	}
}

// objectThread returns the object's tab, or zero when it has none and gets none. An object keeps
// its tab whatever becomes of it, so its last notice lands there; only an open one is given one.
// The registry is the only record of the tab, so a tab taken for gone, whose row is dropped, is made
// again with a fresh summary. With refresh set, the summary and the tab's name are brought up to
// date; otherwise only a missing summary is sent. One object's tab is resolved by one caller at a
// time.
func (s *Tabs) objectThread(ctx context.Context, kind objectKind, principalID string, chatID int64, id string, refresh bool) (int64, error) {
	if s.store == nil {
		return 0, nil
	}
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	failed := kind.event + ".failed"
	on, err := s.tabsOn(ctx)
	if err != nil {
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, id, "stage", "topics", "error", err)
		return 0, err
	}
	if !on {
		return 0, nil
	}
	subject := thread.ObjectDestination(kind.kind, id)
	lock, _ := s.feedLocks.LoadOrStore(principalID+"\x00"+subject, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	found, known, err := kind.load(ctx, principalID, id)
	if err != nil {
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, id, "stage", "load", "error", err)
		return 0, err
	}
	if !known {
		return 0, nil
	}
	threadID, tabbed, err := s.store.SubjectTab(ctx, principalID, subject)
	if err != nil {
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, id, "stage", "load", "error", err)
		return 0, err
	}
	if !tabbed {
		if !found.Open {
			return 0, nil
		}
		if threadID, err = s.makeObjectTab(ctx, kind, principalID, chatID, found); err != nil {
			return 0, err
		}
		if !s.sendObjectSummary(ctx, kind, principalID, chatID, threadID, found) {
			return 0, nil
		}
		return threadID, nil
	}
	s.giveFixedIcon(ctx, principalID, chatID, threadID, kind.icon, failed, kind.idKey, id)
	if refresh {
		s.renameObjectTab(ctx, kind, principalID, chatID, threadID, found)
	}
	kept := true
	switch {
	case found.SummaryMessageID <= 0:
		kept = s.sendObjectSummary(ctx, kind, principalID, chatID, threadID, found)
	case refresh:
		err := editPlain(ctx, s.client, chatID, found.SummaryMessageID, found.Summary)
		switch {
		case err != nil && isPermanentAPIError(err):
			// Telegram no longer has the summary to edit, so it is sent and pinned again.
			kept = s.sendObjectSummary(ctx, kind, principalID, chatID, threadID, found)
		case err != nil:
			logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, id, "thread_id", threadID, "stage", "edit_summary", "error", err)
		}
	}
	if !kept {
		return 0, nil
	}
	return threadID, nil
}

// makeObjectTab opens the object's tab under its title and records it as the object's, with a
// fixed name that only a new title changes.
func (s *Tabs) makeObjectTab(ctx context.Context, kind objectKind, principalID string, chatID int64, found tabObject) (int64, error) {
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	failed := kind.event + ".failed"
	name := objectTabName(kind, found)
	threadID, err := s.client.CreateForumTopic(ctx, chatID, name)
	if err != nil {
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, found.ID, "stage", "create", "error", err)
		return 0, err
	}
	if err := s.store.AddSubjectTab(ctx, principalID, threadID, name, thread.ObjectDestination(kind.kind, found.ID)); err != nil {
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, found.ID, "thread_id", threadID, "stage", "record", "error", err)
		// A tab the registry does not hold would be left behind by the next message's new one.
		if removeErr := s.client.DeleteForumTopic(ctx, chatID, threadID); removeErr != nil {
			logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, found.ID, "thread_id", threadID, "stage", "remove", "error", removeErr)
		}
		return 0, err
	}
	s.knownTabs.Store(chatTab{chatID, threadID}, struct{}{})
	logger.InfoContext(ctx, kind.event+".created", "principal_id", principalID, kind.idKey, found.ID, "thread_id", threadID)
	s.giveFixedIcon(ctx, principalID, chatID, threadID, kind.icon, failed, kind.idKey, found.ID)
	return threadID, nil
}

// sendObjectSummary posts the object's summary in its tab, with sound like any message, records
// it, and pins it, which in a private chat is silent. The summary goes only to the tab: one in the
// main view would be pinned there. A pin Telegram refuses leaves the summary as the tab's first
// message and is logged once per object. It reports false when Telegram's refusal had the tab taken
// for gone.
func (s *Tabs) sendObjectSummary(ctx context.Context, kind objectKind, principalID string, chatID, threadID int64, found tabObject) bool {
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	failed := kind.event + ".failed"
	messageID, err := s.client.SendPlainInTab(thread.WithDelivery(ctx, threadID), chatID, found.Summary)
	if err != nil {
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, found.ID, "thread_id", threadID, "stage", "send_summary", "error", err)
		verdict, refused := tabRefusal(err)
		return !refused || verdict != TabGone
	}
	// A summary that is not recorded is sent again by the next write, which pins a second one.
	for attempt := 1; attempt <= 2; attempt++ {
		if err = kind.setSummary(ctx, principalID, found.ID, messageID); err == nil {
			break
		}
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, found.ID, "thread_id", threadID, "stage", "record_summary", "attempt", attempt, "error", err)
	}
	if err := s.client.PinChatMessage(ctx, chatID, messageID); err != nil {
		if _, logged := s.pinFailures.LoadOrStore(principalID+"\x00"+thread.ObjectDestination(kind.kind, found.ID), struct{}{}); !logged {
			logger.WarnContext(ctx, kind.event+".pin_failed", "principal_id", principalID, kind.idKey, found.ID, "thread_id", threadID, "error", err)
		}
	}
	return true
}

// renameObjectTab names the tab after the object's title once the title changed.
func (s *Tabs) renameObjectTab(ctx context.Context, kind objectKind, principalID string, chatID, threadID int64, found tabObject) {
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	failed := kind.event + ".failed"
	tab, known, err := s.store.Tab(ctx, principalID, threadID)
	name := objectTabName(kind, found)
	if err != nil || !known || tab.Name == name {
		if err != nil {
			logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, found.ID, "thread_id", threadID, "stage", "rename", "error", err)
		}
		return
	}
	if err := s.client.EditForumTopic(ctx, chatID, threadID, name, ""); err != nil {
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, found.ID, "thread_id", threadID, "stage", "rename", "error", err)
		return
	}
	if err := s.store.SetFixedTabName(ctx, principalID, threadID, name); err != nil {
		logger.WarnContext(ctx, failed, "principal_id", principalID, kind.idKey, found.ID, "thread_id", threadID, "stage", "rename", "error", err)
	}
}

func objectTabName(kind objectKind, found tabObject) string {
	if name := topicName(found.Title); name != "" {
		return name
	}
	return kind.fallbackName
}

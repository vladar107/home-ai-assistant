package looseend

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Config sets the zone, the daily run's time, and the clock of the service.
type Config struct {
	// Location is the daily job's zone, which local due dates and nudge days are reckoned in.
	Location *time.Location
	// RunTime is the local time of day the daily job runs at, so a nudge is set for a run before
	// the due time; zero is midnight.
	RunTime time.Duration
	Now     func() time.Time
}

// Service keeps the principals' loose ends and applies their rules: validation, keys, the thread
// merge, tombstones, caps, nudge days, and status changes.
type Service struct {
	repository Repository
	location   *time.Location
	runTime    time.Duration
	now        func() time.Time
	logger     *slog.Logger
}

func NewService(cfg Config, repository Repository, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("loose end repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("loose end logger is required")
	}
	location := cfg.Location
	if location == nil {
		location = time.UTC
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{repository: repository, location: location, runTime: cfg.RunTime, now: now, logger: logger}, nil
}

// Location is the zone local dates are reckoned in.
func (s *Service) Location() *time.Location { return s.location }

// stamp is now as the store keeps it, so a time read back compares equal to the one written.
func (s *Service) stamp() time.Time { return s.now().UTC().Truncate(time.Millisecond) }

// Tracked is what tracking one draft did. Item is the stored item when it was created, and ID the
// item that kept a duplicate, merged, or tombstoned draft out.
type Tracked struct {
	Outcome Outcome
	Item    Item
	ID      string
	Reason  string
}

// Track tracks one item, such as a promise stated in chat. A chat item's source reference is its
// own ID.
func (s *Service) Track(ctx context.Context, principalID string, draft Draft) (Tracked, error) {
	insert, reason, err := s.prepare(principalID, draft, s.stamp())
	if err != nil || reason != "" {
		return Tracked{Outcome: OutcomeInvalid, Reason: reason}, err
	}
	return s.store(ctx, insert)
}

// prepare validates a draft into the insert that stores it, or says why it cannot be tracked.
func (s *Service) prepare(principalID string, draft Draft, now time.Time) (Insert, string, error) {
	if draft.Source == SourceChat {
		draft.Account, draft.SourceRef, draft.ThreadRef = "", "", ""
	}
	item, reason := validate(draft, now, s.location, s.runTime)
	if reason != "" {
		return Insert{}, reason, nil
	}
	id, err := newID()
	if err != nil {
		return Insert{}, "", err
	}
	item.ID, item.PrincipalID, item.CreatedAt, item.UpdatedAt = id, principalID, now, now
	if item.Source == SourceChat {
		item.SourceRef = id
	}
	start, end := dayBounds(item.DueAt, s.location)
	return Insert{Item: item, DayStart: start, DayEnd: end, MaximumOpen: MaximumOpen}, "", nil
}

func (s *Service) store(ctx context.Context, insert Insert) (Tracked, error) {
	outcome, existing, err := s.repository.CreateLooseEnd(ctx, insert)
	if err != nil {
		return Tracked{}, err
	}
	if outcome != OutcomeCreated {
		return Tracked{Outcome: outcome, ID: existing}, nil
	}
	return Tracked{Outcome: outcome, Item: insert.Item, ID: insert.Item.ID}, nil
}

// Batch is what tracking a scan's drafts did, one outcome for each draft in order.
type Batch struct {
	Tracked []Tracked
	Created int
	Capped  int
}

// TrackBatch tracks a scan's drafts in order. It adds at most MaximumNewPerScan new items, and at
// most MaximumNewPerSender of one sender domain. A capped outcome is a valid new item a cap held
// back: an invalid draft, or one already tracked, merged, or tombstoned, is reported as such and
// takes no place under a cap. The drafts past a cap are counted and logged, and their outcomes let
// the scan decide whether to offer them again.
func (s *Service) TrackBatch(ctx context.Context, principalID string, drafts []Draft) (Batch, error) {
	now := s.stamp()
	batch := Batch{Tracked: make([]Tracked, 0, len(drafts))}
	senders := map[string]int{}
	for _, draft := range drafts {
		tracked, err := s.trackCapped(ctx, principalID, draft, now, batch.Created, senders)
		if err != nil {
			return batch, err
		}
		switch tracked.Outcome {
		case OutcomeCreated:
			batch.Created++
			senders[senderKey(draft)]++
		case OutcomeScanCapped, OutcomeSenderCapped, OutcomeOpenCapped:
			batch.Capped++
		}
		batch.Tracked = append(batch.Tracked, tracked)
	}
	if batch.Capped > 0 {
		s.logger.InfoContext(ctx, "looseend.capped", "principal_id", principalID, "capped", batch.Capped, "created", batch.Created)
	}
	return batch, nil
}

func (s *Service) trackCapped(ctx context.Context, principalID string, draft Draft, now time.Time, created int, senders map[string]int) (Tracked, error) {
	insert, reason, err := s.prepare(principalID, draft, now)
	if err != nil || reason != "" {
		return Tracked{Outcome: OutcomeInvalid, Reason: reason}, err
	}
	var capped Outcome
	switch domain := senderKey(draft); {
	case created >= MaximumNewPerScan:
		capped = OutcomeScanCapped
	case domain != "" && senders[domain] >= MaximumNewPerSender:
		capped = OutcomeSenderCapped
	default:
		return s.store(ctx, insert)
	}
	outcome, existing, err := s.repository.FindLooseEnd(ctx, insert)
	if err != nil {
		return Tracked{}, err
	}
	if outcome != "" {
		return Tracked{Outcome: outcome, ID: existing}, nil
	}
	return Tracked{Outcome: capped}, nil
}

func senderKey(draft Draft) string { return strings.ToLower(strings.TrimSpace(draft.Domain)) }

// List returns the principal's open items, of the kind when it is not empty, soonest due first.
func (s *Service) List(ctx context.Context, principalID string, kind Kind) ([]Item, error) {
	return s.repository.ListLooseEnds(ctx, principalID, StatusOpen, kind)
}

// Refs returns where the principal's items from the source were found, open or not.
func (s *Service) Refs(ctx context.Context, principalID string, source Source) ([]Ref, error) {
	return s.repository.LooseEndRefs(ctx, principalID, source)
}

func (s *Service) Get(ctx context.Context, principalID, id string) (Item, bool, error) {
	return s.repository.LoadLooseEnd(ctx, principalID, id)
}

// Close marks an open item done, dropped, or closed, and returns it as it now is. changed is false
// when the item is missing or no longer open.
func (s *Service) Close(ctx context.Context, principalID, id string, status Status) (item Item, changed bool, err error) {
	if !status.Tombstone() {
		return Item{}, false, fmt.Errorf("close loose end: %s is not a closing status", status)
	}
	changed, err = s.repository.ChangeLooseEndStatus(ctx, StatusChange{PrincipalID: principalID, ID: id, From: []Status{StatusOpen}, To: status, At: s.stamp()})
	if err != nil {
		return Item{}, false, err
	}
	item, _, err = s.repository.LoadLooseEnd(ctx, principalID, id)
	return item, changed, err
}

// Reopen takes back a close while the item is as the close left it, last changed at expected. An
// item past its due date is expired again by the next run.
func (s *Service) Reopen(ctx context.Context, principalID, id string, expected time.Time) (bool, error) {
	return s.repository.ChangeLooseEndStatus(ctx, StatusChange{
		PrincipalID: principalID, ID: id, From: []Status{StatusDone, StatusDropped, StatusClosed}, To: StatusOpen, Expected: expected, At: s.stamp(),
	})
}

// Delete takes back a track while the item is as it was stored, last changed at expected.
func (s *Service) Delete(ctx context.Context, principalID, id string, expected time.Time) (bool, error) {
	return s.repository.DeleteLooseEnd(ctx, principalID, id, expected)
}

// CanSnooze reports whether Tomorrow applies to the item now: it is open and tomorrow's run comes
// before its due time, which is never so on its due day.
func (s *Service) CanSnooze(item Item) bool {
	tomorrow := civil(s.now(), s.location).AddDate(0, 0, 1)
	return item.Status == StatusOpen && runOn(tomorrow, s.location, s.runTime).Before(item.DueAt)
}

// Snooze nudges an open item again on the next local day. It is refused when tomorrow's run would
// come after the item's due time and for an item no longer open; snoozed is false then, and item is
// the item as it is.
func (s *Service) Snooze(ctx context.Context, principalID, id string) (item Item, snoozed bool, err error) {
	item, found, err := s.repository.LoadLooseEnd(ctx, principalID, id)
	if err != nil || !found || !s.CanSnooze(item) {
		return item, false, err
	}
	now := s.stamp()
	tomorrow := civil(now, s.location).AddDate(0, 0, 1).Format(DateLayout)
	if snoozed, err = s.repository.SnoozeLooseEnd(ctx, principalID, id, tomorrow, now); err != nil || !snoozed {
		return item, false, err
	}
	item.NudgeOn, item.UpdatedAt = tomorrow, now
	return item, true, nil
}

// Expire marks the principal's open items whose due time passed at now expired, silently.
func (s *Service) Expire(ctx context.Context, principalID string, now time.Time) (int, error) {
	return s.repository.ExpireLooseEnds(ctx, principalID, now.UTC())
}

// Due returns the principal's open items to nudge at now: those whose nudge day is today or a day
// missed before it, and which were not nudged on or after it.
func (s *Service) Due(ctx context.Context, principalID string, now time.Time) ([]Item, error) {
	return s.repository.DueLooseEnds(ctx, principalID, LocalDate(now, s.location), now.UTC())
}

// MarkNudged records that the items were nudged at now. An expiry then takes its second nudge day
// while that is still ahead; every other item is not nudged again unless the user presses Tomorrow.
func (s *Service) MarkNudged(ctx context.Context, principalID string, ids []string, now time.Time) error {
	today := LocalDate(now, s.location)
	for _, id := range ids {
		item, found, err := s.repository.LoadLooseEnd(ctx, principalID, id)
		if err != nil {
			return err
		}
		if !found || item.Status != StatusOpen {
			continue
		}
		if _, err := s.repository.RecordLooseEndNudge(ctx, principalID, id, item.NudgeOn, today, nextNudge(item, today, s.location)); err != nil {
			return err
		}
	}
	return nil
}

// Prune deletes every principal's items no longer open whose due date passed Retention before now.
func (s *Service) Prune(ctx context.Context, now time.Time) (int, error) {
	return s.repository.PruneLooseEnds(ctx, now.UTC().Add(-Retention))
}

// ScanState returns how far the principal's scan has read; found is false before it first starts.
func (s *Service) ScanState(ctx context.Context, principalID string) (ScanState, bool, error) {
	return s.repository.LoadLooseEndScan(ctx, principalID)
}

// SaveScanState replaces the principal's scan state while it has one; after ResetScan it does
// nothing.
func (s *Service) SaveScanState(ctx context.Context, principalID string, state ScanState) error {
	return s.repository.SaveLooseEndScan(ctx, principalID, state)
}

// StartScan gives a principal without a scan state one that reads mail from now on and backfills
// the BackfillWindow before it, and returns the principal's state.
func (s *Service) StartScan(ctx context.Context, principalID string) (ScanState, error) {
	state, found, err := s.repository.LoadLooseEndScan(ctx, principalID)
	if err != nil || found {
		return state, err
	}
	now := s.stamp()
	state = ScanState{ReadUntil: now, BackfillUntil: now.Add(-BackfillWindow), BackfillBefore: now}
	if err := s.repository.CreateLooseEndScan(ctx, principalID, state); err != nil {
		return ScanState{}, err
	}
	state, _, err = s.repository.LoadLooseEndScan(ctx, principalID)
	return state, err
}

// ResetScan forgets how far the principal's scan has read and its calendar digest, as turning the
// switch off does. The tracked items are kept.
func (s *Service) ResetScan(ctx context.Context, principalID string) error {
	return s.repository.ResetLooseEndScan(ctx, principalID)
}

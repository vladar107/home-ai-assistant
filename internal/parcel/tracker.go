package parcel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	// DefaultSourceInterval is how often Home Assistant is read.
	DefaultSourceInterval = 10 * time.Minute
	// DefaultMailDayInterval is how often mail is read from 07:00 to 22:00 while the principal has
	// a parcel on its way.
	DefaultMailDayInterval = 30 * time.Minute
	// DefaultMailIdleInterval is how often mail is read otherwise, at night too.
	DefaultMailIdleInterval = 3 * time.Hour
	// DefaultTick is how often the loop looks at what is due.
	DefaultTick = time.Minute

	loopPruneInterval = time.Hour
	loopModuleVersion = "0.1.0"
	dayStartHour      = 7
	dayEndHour        = 22
	// sendTimeout bounds a send that follows the record of it; a shutdown does not cut it short.
	sendTimeout = 30 * time.Second
)

// MailRunner reads a principal's shipping mail and stores the parcels it finds; MailSource is one.
// It must not be run twice at once for one principal, which the loop sees to.
type MailRunner interface {
	Run(ctx context.Context, principalID string, now time.Time) (MailResult, error)
}

// Untracker removes a number a principal added from Home Assistant.
type Untracker interface {
	Untrack(ctx context.Context, principalID, carrier, number string) error
}

// Enabler says whether the principal has Parcels on.
type Enabler interface {
	Enabled(ctx context.Context, principalID string) (bool, error)
}

// SwitchEnabler reads the Parcels switch from the integration switches.
type SwitchEnabler struct{ Reader integration.SwitchReader }

// Enabled reports whether the principal has Parcels on.
func (e SwitchEnabler) Enabled(ctx context.Context, principalID string) (bool, error) {
	return e.Reader.Enabled(ctx, principalID, Switch)
}

// LoopConfig is what the loop is built from.
type LoopConfig struct {
	Repository Repository
	// Sources is the Home Assistant side; nil without a token, and Parcels works on mail alone.
	Sources Sources
	// Mail is nil when Google is not configured.
	Mail MailRunner
	// Untracker is nil when Home Assistant cannot be written to.
	Untracker Untracker
	Cards     Cards
	Enabled   Enabler
	// Principals returns the allowlisted principals.
	Principals func() []string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Location is where the day and night are counted.
	Location *time.Location
	// Languages gives each principal's language for what the mail job writes; nil writes English.
	Languages locale.Preferences
	Logger    *slog.Logger
	// The intervals default to the package's.
	Tick, SourceInterval, MailDayInterval, MailIdleInterval time.Duration
}

// Loop keeps every principal's parcels and cards current: it reads Home Assistant and mail on a
// schedule, edits a card when its parcel changes, and sends a notice when it matters. Home Assistant
// is read once per principal and tick, since ParcelSource answers from a 30 second cache and the
// readings differ by principal.
//
// A notice is recorded before it is sent, so a restart never repeats one and a failed send is lost,
// which beats a repeat.
type Loop struct {
	cfg LoopConfig

	mu      sync.Mutex
	states  map[string]*principalState
	pending map[string]struct{}
	wake    chan struct{}
	pruned  time.Time

	workers sync.WaitGroup
	// spawn starts a worker of Cycle; a test can hold the workers back to run them in its own order.
	spawn func(func())

	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// principalState is what the loop remembers of one principal between passes. What a restart must not
// redo is in the store (Tracking and the recorded notices), not here.
type principalState struct {
	// ops serialises the baseline, the Home Assistant read, and the switch handling.
	ops sync.Mutex
	// mailRun serialises mail runs, which read and write the principal's cursor.
	mailRun sync.Mutex
	// cards serialises every change to a parcel and its card.
	cards sync.Mutex

	// meta guards the fields below.
	meta sync.Mutex
	// ready is set once the principal has Parcels on and a finished baseline.
	ready    bool
	open     bool
	lastRead time.Time
	lastMail time.Time
	// dirty holds the cards that failed to follow their parcel, with the mode to edit them to.
	dirty map[string]CardMode
}

// NewLoop builds the loop.
func NewLoop(cfg LoopConfig) (*Loop, error) {
	switch {
	case cfg.Repository == nil:
		return nil, errors.New("the parcel loop needs a repository")
	case cfg.Cards == nil:
		return nil, errors.New("the parcel loop needs cards")
	case cfg.Enabled == nil:
		return nil, errors.New("the parcel loop needs the Parcels switch")
	case cfg.Principals == nil:
		return nil, errors.New("the parcel loop needs the principals")
	case cfg.Location == nil:
		return nil, errors.New("the parcel loop needs a location")
	case cfg.Logger == nil:
		return nil, errors.New("the parcel loop needs a logger")
	}
	if isNil(cfg.Sources) {
		cfg.Sources = nil
	}
	if isNil(cfg.Mail) {
		cfg.Mail = nil
	}
	if isNil(cfg.Untracker) {
		cfg.Untracker = nil
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	for _, interval := range []struct {
		value *time.Duration
		def   time.Duration
	}{{&cfg.Tick, DefaultTick}, {&cfg.SourceInterval, DefaultSourceInterval}, {&cfg.MailDayInterval, DefaultMailDayInterval}, {&cfg.MailIdleInterval, DefaultMailIdleInterval}} {
		if *interval.value <= 0 {
			*interval.value = interval.def
		}
	}
	return &Loop{
		cfg: cfg, states: map[string]*principalState{}, pending: map[string]struct{}{}, wake: make(chan struct{}, 1),
		spawn: func(work func()) { go work() },
	}, nil
}

// isNil reports whether an interface holds nothing or a nil pointer, so a source that is not
// configured is treated as absent however it was passed.
func isNil(value any) bool {
	if value == nil {
		return true
	}
	switch reflected := reflect.ValueOf(value); reflected.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return reflected.IsNil()
	}
	return false
}

var (
	_ Tracker       = (*Loop)(nil)
	_ module.Module = (*Loop)(nil)
)

func (l *Loop) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "parcels", Version: loopModuleVersion, Kind: module.KindSystem}
}

func (l *Loop) Start(context.Context) error {
	l.runMu.Lock()
	defer l.runMu.Unlock()
	if l.done != nil {
		return errors.New("the parcel loop is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	l.cancel, l.done = cancel, done
	go func() {
		defer close(done)
		l.Run(ctx)
	}()
	return nil
}

func (l *Loop) Ready(context.Context) error {
	l.runMu.Lock()
	defer l.runMu.Unlock()
	if l.done == nil {
		return errors.New("the parcel loop is not running")
	}
	return nil
}

func (l *Loop) Shutdown(ctx context.Context) error {
	l.runMu.Lock()
	cancel, done := l.cancel, l.done
	l.cancel, l.done = nil, nil
	l.runMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown parcel loop: %w", ctx.Err())
	}
}

// Run cycles until ctx ends, and returns once the work it started has ended.
func (l *Loop) Run(ctx context.Context) {
	defer l.Wait()
	ticker := time.NewTicker(l.cfg.Tick)
	defer ticker.Stop()
	for {
		l.Cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-l.wake:
		}
	}
}

// Wait waits for the work Cycle started.
func (l *Loop) Wait() { l.workers.Wait() }

// Switched tells the loop that the principal turned Parcels on or off. It queues a pass for the
// loop's own goroutine and returns at once; the pass reads the switch itself.
func (l *Loop) Switched(_ context.Context, principalID string, _ bool) error {
	l.queue(principalID)
	return nil
}

func (l *Loop) queue(principalID string) {
	l.mu.Lock()
	l.pending[principalID] = struct{}{}
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Baseline makes the principal's next pass a silent baseline, as after a restore. The need is
// stored first, so a restart before the pass still runs it.
func (l *Loop) Baseline(ctx context.Context, principalID string) error {
	tracking, err := l.cfg.Repository.LoadTracking(ctx, principalID)
	if err != nil {
		return err
	}
	tracking.BaselinedAt = time.Time{}
	if err := l.cfg.Repository.SaveTracking(ctx, tracking); err != nil {
		return err
	}
	l.queue(principalID)
	return nil
}

func (l *Loop) state(principalID string) *principalState {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.states[principalID]
	if st == nil {
		st = &principalState{dirty: map[string]CardMode{}}
		l.states[principalID] = st
	}
	return st
}

func (l *Loop) hasPending(principalID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, found := l.pending[principalID]
	return found
}

func (l *Loop) takePending(principalID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, found := l.pending[principalID]
	delete(l.pending, principalID)
	return found
}

func (l *Loop) night(now time.Time) bool {
	hour := now.In(l.cfg.Location).Hour()
	return hour < dayStartHour || hour >= dayEndHour
}

// Cycle starts what is due: for each principal a pass over its switch, baseline, and Home Assistant
// read, and the mail run, each on its own goroutine, and once an hour the retention. The loop runs
// it on every tick; Wait waits for what it started.
func (l *Loop) Cycle(ctx context.Context) {
	now := l.cfg.Now()
	l.prune(ctx, now)
	for _, principalID := range l.cfg.Principals() {
		if ctx.Err() != nil {
			return
		}
		st := l.state(principalID)
		st.meta.Lock()
		readDue := now.Sub(st.lastRead) >= l.cfg.SourceInterval
		if readDue {
			st.lastRead = now
		}
		mailDue := l.cfg.Mail != nil && st.ready && l.mailDue(now, now.Sub(st.lastMail), st.open)
		if mailDue {
			st.lastMail = now
		}
		st.meta.Unlock()
		if readDue || l.hasPending(principalID) {
			l.workers.Add(1)
			l.spawn(func() {
				defer l.workers.Done()
				l.step(ctx, principalID, st, readDue)
			})
		}
		// The run is claimed here rather than in its goroutine, so a slot that finds one still
		// running is skipped instead of starting another the moment it ends.
		if mailDue && st.mailRun.TryLock() {
			l.workers.Add(1)
			l.spawn(func() {
				defer l.workers.Done()
				defer st.mailRun.Unlock()
				l.runMail(ctx, principalID, st)
			})
		}
	}
}

// mailDue applies the cadence: every day interval from 07:00 to 22:00 while a parcel is not yet
// delivered, and every idle interval otherwise.
func (l *Loop) mailDue(now time.Time, since time.Duration, open bool) bool {
	if open && !l.night(now) {
		return since >= l.cfg.MailDayInterval
	}
	return since >= l.cfg.MailIdleInterval
}

func isOpen(found Parcel) bool { return !found.Stopped() && found.Status != StatusDelivered }

func (l *Loop) prune(ctx context.Context, now time.Time) {
	l.mu.Lock()
	due := now.Sub(l.pruned) >= loopPruneInterval
	if due {
		l.pruned = now
	}
	l.mu.Unlock()
	if !due || ctx.Err() != nil {
		return
	}
	deleted, err := l.cfg.Repository.DeleteExpired(ctx, now.Add(-Retention), now)
	if err != nil && ctx.Err() == nil {
		l.cfg.Logger.WarnContext(ctx, "parcels.loop.failed", "stage", "retention", "error", errorKind(err))
	}
	pruned, err := l.cfg.Repository.PruneTombstones(ctx, now.Add(-TombstoneLifetime))
	if err != nil && ctx.Err() == nil {
		l.cfg.Logger.WarnContext(ctx, "parcels.loop.failed", "stage", "tombstones", "error", errorKind(err))
	}
	if deleted > 0 || pruned > 0 {
		l.cfg.Logger.InfoContext(ctx, "parcels.loop.pruned", "parcels", deleted, "tombstones", pruned)
	}
}

// fingerprint identifies the sources assigned to the principal, so a change in them runs the
// baseline again.
func (l *Loop) fingerprint(principalID string) string {
	if l.cfg.Sources == nil {
		return ""
	}
	assigned := slices.Clone(l.cfg.Sources.Assigned(principalID))
	slices.Sort(assigned)
	if len(assigned) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(assigned, "\n")))
	return hex.EncodeToString(sum[:16])
}

// step settles a principal's state against what is stored and the switch: it marks the cards off
// when Parcels is off, runs the baseline when one is owed, and otherwise reads Home Assistant.
func (l *Loop) step(ctx context.Context, principalID string, st *principalState, readDue bool) {
	if !st.ops.TryLock() {
		return
	}
	defer st.ops.Unlock()
	enabled, err := l.cfg.Enabled.Enabled(ctx, principalID)
	if err != nil {
		l.fail(ctx, principalID, "enabled", err)
		return
	}
	queued := l.takePending(principalID)
	tracking, err := l.cfg.Repository.LoadTracking(ctx, principalID)
	if err != nil {
		l.fail(ctx, principalID, "tracking", err)
		return
	}
	if !enabled {
		st.setReady(false)
		if !tracking.CardsOff && l.markOff(ctx, principalID, st) {
			tracking.BaselinedAt, tracking.CardsOff = time.Time{}, true
			if err := l.cfg.Repository.SaveTracking(ctx, tracking); err != nil {
				l.fail(ctx, principalID, "save_tracking", err)
			}
		}
		return
	}
	fingerprint := l.fingerprint(principalID)
	if tracking.BaselinedAt.IsZero() || tracking.CardsOff || tracking.Fingerprint != fingerprint {
		st.setReady(false)
		l.baseline(ctx, principalID, st, fingerprint)
		return
	}
	st.setReady(true)
	if readDue || queued || tracking.SourcesOwed {
		settled, err := l.read(ctx, principalID, st, tracking.SourcesOwed)
		if err != nil {
			l.fail(ctx, principalID, "read", err)
		}
		if settled && tracking.SourcesOwed {
			tracking.SourcesOwed = false
			if err := l.cfg.Repository.SaveTracking(ctx, tracking); err != nil {
				l.fail(ctx, principalID, "save_tracking", err)
			}
		}
	}
	l.dropIdle(ctx, principalID, st)
	l.refreshOpen(ctx, principalID, st)
}

// dropIdle deletes the principal's parcels no source has updated for IdleLifetime, so a parcel a
// source dropped before delivery stops counting as on its way, and edits their cards to say they
// are no longer followed. It leaves no tombstone, so news of one follows it again with a new card;
// a card whose edit fails keeps its last state.
func (l *Loop) dropIdle(ctx context.Context, principalID string, st *principalState) {
	st.cards.Lock()
	defer st.cards.Unlock()
	idle, err := l.cfg.Repository.DeleteIdle(ctx, principalID, l.cfg.Now().Add(-IdleLifetime))
	if err != nil {
		l.fail(ctx, principalID, "drop_idle", err)
		return
	}
	for _, found := range idle {
		l.cfg.Logger.InfoContext(ctx, "parcels.idle_dropped", "principal_id", principalID, "parcel_id", found.ID, "status", found.Status)
		if found.CardMessage == 0 {
			continue
		}
		if err := l.cfg.Cards.EditCard(ctx, found, CardIdle); err != nil && !errors.Is(err, ErrCardGone) {
			l.fail(ctx, principalID, "idle_card", err)
		}
	}
}

func (l *Loop) refreshOpen(ctx context.Context, principalID string, st *principalState) {
	parcels, err := l.cfg.Repository.ListParcels(ctx, principalID)
	if err != nil {
		l.fail(ctx, principalID, "open_parcels", err)
		return
	}
	open := slices.ContainsFunc(parcels, isOpen)
	st.meta.Lock()
	st.open = open
	st.meta.Unlock()
}

func (l *Loop) fail(ctx context.Context, principalID, stage string, err error) {
	if ctx.Err() == nil {
		l.cfg.Logger.WarnContext(ctx, "parcels.loop.failed", "principal_id", principalID, "stage", stage, "error", errorKind(err))
	}
}

// errorKind names an error's class without its text, which can carry a parcel's title or number.
func errorKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, ErrCardGone):
		return "card_gone"
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return fmt.Sprintf("%T", err)
		}
		err = next
	}
}

// read lists the principal's parcels from Home Assistant and follows what changed, quietly when
// the baseline is still owed it: no notice for what it finds, and new cards without sound. The cards
// are repaired whether or not Home Assistant answers. It reports whether every assigned source has
// now been read well, which settles a baseline still owed.
func (l *Loop) read(ctx context.Context, principalID string, st *principalState, quiet bool) (bool, error) {
	var reading Reading
	var readErr error
	if l.cfg.Sources != nil {
		reading, readErr = l.cfg.Sources.Read(ctx, principalID)
	}
	if !l.stillOn(ctx, principalID) {
		return false, readErr
	}
	st.cards.Lock()
	defer st.cards.Unlock()
	now := l.cfg.Now()
	l.repair(ctx, st, principalID, now, quiet)
	if readErr != nil {
		return false, readErr
	}
	before, err := l.index(ctx, principalID)
	if err != nil {
		return false, err
	}
	for _, item := range reading.Parcels {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		stored, previous, isNew, ok := l.store(ctx, principalID, item, before, now)
		if !ok {
			continue
		}
		l.follow(ctx, st, stored, previous, isNew, now, quiet)
	}
	return !reading.Unread, nil
}

// stillOn reads the switch again, since a read or a mail run can outlast turning Parcels off.
func (l *Loop) stillOn(ctx context.Context, principalID string) bool {
	enabled, err := l.cfg.Enabled.Enabled(ctx, principalID)
	if err != nil {
		l.fail(ctx, principalID, "enabled", err)
		return false
	}
	return enabled
}

// store upserts a Home Assistant reading unless it is tombstoned or stopped, and returns the parcel
// with the one it replaced.
func (l *Loop) store(ctx context.Context, principalID string, item Parcel, before map[string]Parcel, now time.Time) (stored, previous Parcel, isNew, ok bool) {
	item.PrincipalID = principalID
	item.Source = SourceHomeAssistant
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = now
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	tombstoned, err := l.cfg.Repository.TombstoneSeenAny(ctx, principalID, item.Keys(), now)
	if err != nil {
		l.fail(ctx, principalID, "tombstone", err)
		return Parcel{}, Parcel{}, false, false
	}
	if tombstoned {
		return Parcel{}, Parcel{}, false, false
	}
	stored, isNew, err = l.cfg.Repository.UpsertParcel(ctx, item)
	if err != nil {
		l.fail(ctx, principalID, "upsert", err)
		return Parcel{}, Parcel{}, false, false
	}
	if stored.Stopped() {
		return Parcel{}, Parcel{}, false, false
	}
	previous, known := before[stored.ID]
	before[stored.ID] = stored
	return stored, previous, isNew || !known, true
}

func (l *Loop) index(ctx context.Context, principalID string) (map[string]Parcel, error) {
	parcels, err := l.cfg.Repository.ListParcels(ctx, principalID)
	if err != nil {
		return nil, err
	}
	index := make(map[string]Parcel, len(parcels))
	for _, found := range parcels {
		index[found.ID] = found
	}
	return index, nil
}

// cardChanged reports whether anything a card shows differs, or the parcel moved to another card.
func cardChanged(before, after Parcel) bool {
	return before.Status != after.Status || !before.WindowFrom.Equal(after.WindowFrom) || !before.WindowTo.Equal(after.WindowTo) ||
		!before.DeliveredAt.Equal(after.DeliveredAt) || before.PickupPoint != after.PickupPoint || before.Title != after.Title ||
		before.Sender != after.Sender || before.Link != after.Link || before.Carrier != after.Carrier || before.Number != after.Number ||
		before.CardMessage != after.CardMessage
}

// follow brings the parcel's card up to date and sends the notice a transition owes. The caller holds
// st.cards. A parcel first seen already delivered gets neither a card nor a notice. Quiet records the
// notice without sending it and sends the card without sound, as the baseline does.
func (l *Loop) follow(ctx context.Context, st *principalState, stored, previous Parcel, isNew bool, now time.Time, quiet bool) {
	night := l.night(now) || quiet
	switch {
	case stored.CardMessage == 0:
		if isOpen(stored) {
			stored = l.sendCard(ctx, st, stored, night)
		}
	case isNew || cardChanged(previous, stored):
		stored = l.editCard(ctx, st, stored, night)
	}
	if !stored.Status.Notice() || (!isNew && previous.Status == stored.Status) {
		return
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	first, err := l.cfg.Repository.RecordNotice(sendCtx, stored.PrincipalID, stored.ID, stored.Status, now)
	if err != nil {
		l.fail(ctx, stored.PrincipalID, "record_notice", err)
		return
	}
	if !first || quiet || (isNew && stored.Status == StatusDelivered) {
		return
	}
	if err := l.cfg.Cards.SendNotice(sendCtx, stored, night); err != nil {
		l.fail(ctx, stored.PrincipalID, "send_notice", err)
		return
	}
	l.cfg.Logger.InfoContext(ctx, "parcels.notice.sent", "principal_id", stored.PrincipalID, "parcel_id", stored.ID, "status", stored.Status)
}

func (st *principalState) setReady(ready bool) {
	st.meta.Lock()
	st.ready = ready
	st.meta.Unlock()
}

func (st *principalState) takeDirty() map[string]CardMode {
	st.meta.Lock()
	defer st.meta.Unlock()
	taken := st.dirty
	st.dirty = map[string]CardMode{}
	return taken
}

func (st *principalState) markDirty(id string, mode CardMode) {
	st.meta.Lock()
	st.dirty[id] = mode
	st.meta.Unlock()
}

// sendCard sends the parcel's card and stores where it is. A card that cannot be sent is sent by
// the next repair.
func (l *Loop) sendCard(ctx context.Context, _ *principalState, found Parcel, silent bool) Parcel {
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	message, kind, err := l.cfg.Cards.SendCard(sendCtx, found, silent)
	if err != nil {
		l.fail(ctx, found.PrincipalID, "send_card", err)
		return found
	}
	if err := l.cfg.Repository.SaveCard(sendCtx, found.PrincipalID, found.ID, message, kind); err != nil {
		l.fail(ctx, found.PrincipalID, "save_card", err)
	}
	found.CardMessage, found.CardKind = message, kind
	l.cfg.Logger.InfoContext(ctx, "parcels.card.sent", "principal_id", found.PrincipalID, "parcel_id", found.ID, "status", found.Status)
	return found
}

// editCard shows the parcel on its card. A card that is gone is sent again for a parcel still on
// its way and forgotten for one that is not; a card that failed to follow is edited again by the
// next repair.
func (l *Loop) editCard(ctx context.Context, st *principalState, found Parcel, silent bool) Parcel {
	err := l.cfg.Cards.EditCard(ctx, found, CardCurrent)
	switch {
	case err == nil:
	case errors.Is(err, ErrCardGone):
		if isOpen(found) {
			return l.sendCard(ctx, st, found, silent)
		}
		if err := l.cfg.Repository.SaveCard(context.WithoutCancel(ctx), found.PrincipalID, found.ID, 0, ""); err != nil {
			l.fail(ctx, found.PrincipalID, "save_card", err)
		}
		found.CardMessage, found.CardKind = 0, ""
	default:
		st.markDirty(found.ID, CardCurrent)
		l.fail(ctx, found.PrincipalID, "edit_card", err)
	}
	return found
}

// repair sends the card of each parcel on its way that has none, without sound at night or when
// quiet, and edits again the cards that failed to follow their parcel. The caller holds st.cards.
func (l *Loop) repair(ctx context.Context, st *principalState, principalID string, now time.Time, quiet bool) {
	dirty := st.takeDirty()
	parcels, err := l.cfg.Repository.ListParcels(ctx, principalID)
	if err != nil {
		l.fail(ctx, principalID, "repair", err)
		for id, mode := range dirty {
			st.markDirty(id, mode)
		}
		return
	}
	night := l.night(now) || quiet
	for _, found := range parcels {
		mode, isDirty := dirty[found.ID]
		switch {
		case found.Stopped():
			if isDirty && mode == CardStopped && found.CardMessage != 0 {
				if err := l.cfg.Cards.EditCard(ctx, found, CardStopped); err != nil && !errors.Is(err, ErrCardGone) {
					st.markDirty(found.ID, CardStopped)
					l.fail(ctx, principalID, "stop_card", err)
				}
			}
		case found.CardMessage == 0:
			if isOpen(found) {
				l.sendCard(ctx, st, found, night)
			}
		case isDirty && mode == CardCurrent:
			l.editCard(ctx, st, found, night)
		}
	}
}

// runMail runs the principal's mail job and follows the parcels it stored. The caller holds
// st.mailRun.
func (l *Loop) runMail(ctx context.Context, principalID string, st *principalState) {
	if !l.stillOn(ctx, principalID) {
		return
	}
	st.cards.Lock()
	before, err := l.index(ctx, principalID)
	st.cards.Unlock()
	if err != nil {
		l.fail(ctx, principalID, "list", err)
		return
	}
	now := l.cfg.Now()
	result, err := l.cfg.Mail.Run(l.inLanguage(ctx, principalID), principalID, now)
	if err != nil {
		l.fail(ctx, principalID, "mail", err)
	}
	l.logMail(ctx, principalID, result)
	if len(result.Parcels) == 0 || ctx.Err() != nil || !l.stillOn(ctx, principalID) {
		return
	}
	// A run reports an item at a time, so one parcel can be listed twice, and one reported earlier
	// can have been folded into another or deleted since. Each is read again and followed once.
	var ids []string
	isNew := map[string]bool{}
	for _, upserted := range result.Parcels {
		if _, seen := isNew[upserted.Parcel.ID]; !seen {
			ids = append(ids, upserted.Parcel.ID)
		}
		isNew[upserted.Parcel.ID] = isNew[upserted.Parcel.ID] || upserted.New
	}
	st.cards.Lock()
	defer st.cards.Unlock()
	l.repair(ctx, st, principalID, now, false)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		fresh, found, err := l.cfg.Repository.LoadParcel(ctx, principalID, id)
		if err != nil {
			l.fail(ctx, principalID, "load", err)
			continue
		}
		if !found || fresh.Stopped() {
			continue
		}
		tombstoned, err := l.cfg.Repository.TombstoneSeenAny(ctx, principalID, fresh.Keys(), now)
		if err != nil {
			l.fail(ctx, principalID, "tombstone", err)
			continue
		}
		if tombstoned {
			continue
		}
		previous, known := before[id]
		before[id] = fresh
		l.follow(ctx, st, fresh, previous, isNew[id] || !known, now, false)
	}
	l.refreshOpen(ctx, principalID, st)
}

// inLanguage puts the context in the principal's language, which the cards of proposals the mail
// job offers are written in.
func (l *Loop) inLanguage(ctx context.Context, principalID string) context.Context {
	withLanguage, err := locale.ForPrincipal(ctx, l.cfg.Languages, principalID)
	if err != nil {
		l.fail(ctx, principalID, "language", err)
	}
	return withLanguage
}

func (l *Loop) logMail(ctx context.Context, principalID string, result MailResult) {
	l.cfg.Logger.InfoContext(ctx, "parcels.mail.ran", "principal_id", principalID, "read", result.Read, "parcels", len(result.Parcels),
		"unreadable", result.Unreadable, "failed", result.Failed, "capped", result.Capped)
}

// baseline is the silent first read: no notice for what it finds, the stored cards edited back to
// current, a silent card for each parcel on its way that has none, and one summary line. The
// statuses found are recorded as noticed, so nothing sent later repeats them. It is owed until it
// finishes, when it is stored with the fingerprint of the sources it read, so a restart does not
// rerun it and a change of sources does. When Home Assistant cannot be read, or a source assigned
// to the principal has never read well, the baseline finishes on what it has and leaves Home
// Assistant owed, so its reads stay silent until every source has read well. The caller holds
// st.ops.
func (l *Loop) baseline(ctx context.Context, principalID string, st *principalState, fingerprint string) {
	now := l.cfg.Now()
	owed := false
	if l.cfg.Sources != nil {
		reading, err := l.cfg.Sources.Read(ctx, principalID)
		if err != nil {
			l.fail(ctx, principalID, "baseline_read", err)
		}
		owed = err != nil || reading.Unread
		st.cards.Lock()
		for _, item := range reading.Parcels {
			l.store(ctx, principalID, item, map[string]Parcel{}, now)
		}
		st.cards.Unlock()
	}
	if l.cfg.Mail != nil && ctx.Err() == nil {
		st.mailRun.Lock()
		result, err := l.cfg.Mail.Run(l.inLanguage(ctx, principalID), principalID, now)
		st.mailRun.Unlock()
		if err != nil {
			l.fail(ctx, principalID, "baseline_mail", err)
		}
		l.logMail(ctx, principalID, result)
	}
	if ctx.Err() != nil || !l.stillOn(ctx, principalID) {
		return
	}
	st.cards.Lock()
	defer st.cards.Unlock()
	parcels, err := l.cfg.Repository.ListParcels(ctx, principalID)
	if err != nil {
		l.fail(ctx, principalID, "baseline_list", err)
		return
	}
	st.takeDirty()
	open := 0
	for _, found := range parcels {
		if found.Stopped() {
			continue
		}
		if found.Status.Notice() {
			if _, err := l.cfg.Repository.RecordNotice(ctx, principalID, found.ID, found.Status, now); err != nil {
				l.fail(ctx, principalID, "record_notice", err)
			}
		}
		if isOpen(found) {
			open++
		}
		switch {
		case found.CardMessage != 0:
			l.editCard(ctx, st, found, true)
		case isOpen(found):
			l.sendCard(ctx, st, found, true)
		}
	}
	if open > 0 {
		if err := l.cfg.Cards.SendSummary(ctx, principalID, open); err != nil {
			l.fail(ctx, principalID, "summary", err)
		}
	}
	if err := l.cfg.Repository.SaveTracking(ctx, Tracking{PrincipalID: principalID, BaselinedAt: now, Fingerprint: fingerprint, SourcesOwed: owed}); err != nil {
		l.fail(ctx, principalID, "save_tracking", err)
		return
	}
	st.meta.Lock()
	st.ready, st.open = true, open > 0
	st.lastMail = now
	st.meta.Unlock()
	l.cfg.Logger.InfoContext(ctx, "parcels.baseline.done", "principal_id", principalID, "open", open, "parcels", len(parcels))
}

// markOff edits the cards of the parcels still on their way to say tracking is off, and reports
// whether every one is done.
func (l *Loop) markOff(ctx context.Context, principalID string, st *principalState) bool {
	st.cards.Lock()
	defer st.cards.Unlock()
	parcels, err := l.cfg.Repository.ListParcels(ctx, principalID)
	if err != nil {
		l.fail(ctx, principalID, "off_list", err)
		return false
	}
	done := true
	for _, found := range parcels {
		if !isOpen(found) || found.CardMessage == 0 {
			continue
		}
		if err := l.cfg.Cards.EditCard(ctx, found, CardTrackingOff); err != nil && !errors.Is(err, ErrCardGone) {
			l.fail(ctx, principalID, "off_card", err)
			done = false
		}
	}
	return done
}

// Stop stops tracking one of the principal's parcels: it is tombstoned, a number the user added is
// removed from Home Assistant without failing the stop when that cannot be done, and its card shows
// Stopped, which is tried again by the next pass if it fails.
func (l *Loop) Stop(ctx context.Context, principalID, parcelID string) (Parcel, bool, error) {
	st := l.state(principalID)
	st.cards.Lock()
	stopped, found, err := l.cfg.Repository.StopParcel(ctx, principalID, parcelID, l.cfg.Now())
	if err == nil && found && stopped.CardMessage != 0 {
		if err := l.cfg.Cards.EditCard(ctx, stopped, CardStopped); err != nil && !errors.Is(err, ErrCardGone) {
			st.markDirty(stopped.ID, CardStopped)
			l.fail(ctx, principalID, "stop_card", err)
		}
	}
	st.cards.Unlock()
	if err != nil || !found {
		return stopped, found, err
	}
	if l.cfg.Untracker != nil && stopped.Number != "" {
		l.untrack(ctx, principalID, stopped)
	}
	l.cfg.Logger.InfoContext(ctx, "parcels.stopped", "principal_id", principalID, "parcel_id", stopped.ID)
	return stopped, true, nil
}

func (l *Loop) untrack(ctx context.Context, principalID string, stopped Parcel) {
	added, found, err := l.cfg.Repository.LoadAddedNumber(ctx, principalID, stopped.Carrier, stopped.Number)
	if err != nil {
		l.fail(ctx, principalID, "stop_added_number", err)
		return
	}
	if !found {
		return
	}
	if err := l.cfg.Untracker.Untrack(ctx, principalID, added.Carrier, added.Number); err != nil {
		l.fail(ctx, principalID, "stop_untrack", err)
	}
}

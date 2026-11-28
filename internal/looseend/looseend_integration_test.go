package looseend_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/delivery"
	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/looseend"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

var amsterdam = mustZone("Europe/Amsterdam")

// morningRun is the daily job's time of day, as the application sets it.
const morningRun = 7*time.Hour + 20*time.Minute

func mustZone(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return location
}

// local is a wall-clock time in the test zone.
func local(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, amsterdam)
}

type fixture struct {
	t       *testing.T
	store   *sqlite.Store
	service *looseend.Service
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, store: store, now: local(2026, 12, 1, 9, 0)}
	service, err := looseend.NewService(looseend.Config{Location: amsterdam, RunTime: morningRun, Now: func() time.Time { return f.now }}, store,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f
}

func mail(message, thread, kind, due string) looseend.Draft {
	return looseend.Draft{
		Kind: looseend.Kind(kind), Title: "Hotel booking", Counterpart: "hotel.example", Due: due,
		Source: looseend.SourceMail, Account: "user@example.com", SourceRef: message, ThreadRef: thread, Outside: true, Domain: "hotel.example",
	}
}

func (f *fixture) track(principalID string, draft looseend.Draft) looseend.Tracked {
	f.t.Helper()
	tracked, err := f.service.Track(f.t.Context(), principalID, draft)
	if err != nil {
		f.t.Fatal(err)
	}
	return tracked
}

func (f *fixture) created(principalID string, draft looseend.Draft) looseend.Item {
	f.t.Helper()
	tracked := f.track(principalID, draft)
	if tracked.Outcome != looseend.OutcomeCreated {
		f.t.Fatalf("track %+v = %+v, want it created", draft, tracked)
	}
	return tracked.Item
}

func (f *fixture) get(principalID, id string) looseend.Item {
	f.t.Helper()
	item, found, err := f.service.Get(f.t.Context(), principalID, id)
	if err != nil || !found {
		f.t.Fatalf("get %s = %v %v", id, found, err)
	}
	return item
}

func (f *fixture) due(principalID string, at time.Time) []string {
	f.t.Helper()
	items, err := f.service.Due(f.t.Context(), principalID, at)
	if err != nil {
		f.t.Fatal(err)
	}
	var ids []string
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func (f *fixture) nudge(principalID string, at time.Time) []string {
	f.t.Helper()
	ids := f.due(principalID, at)
	if err := f.service.MarkNudged(f.t.Context(), principalID, ids, at); err != nil {
		f.t.Fatal(err)
	}
	return ids
}

// A second detection of the same message is a no-op, while another kind of deadline in it is its
// own item.
func TestTrackingTheSameMessageTwiceAddsNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	item := f.created("alice", mail("m1", "t1", "cancel_window", "2026-12-10"))
	if again := f.track("alice", mail("m1", "t1", "cancel_window", "2026-12-11")); again.Outcome != looseend.OutcomeDuplicate || again.ID != item.ID {
		t.Fatalf("again = %+v, want a duplicate of %s", again, item.ID)
	}
	f.created("alice", mail("m1", "t1", "return_window", "2026-12-20"))
	if open, _ := f.service.List(t.Context(), "alice", ""); len(open) != 2 {
		t.Fatalf("open = %+v, want two items", open)
	}
}

// A reminder in the same thread for the same local due date merges into the confirmation's item;
// another booking in its own thread, or another day in the same thread, stays its own item.
func TestAThreadMergesOnlyTheSameDeadline(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	confirmation := f.created("alice", mail("m1", "t1", "cancel_window", "2026-12-10"))
	if reminder := f.track("alice", mail("m2", "t1", "cancel_window", "2026-12-10T18:00")); reminder.Outcome != looseend.OutcomeMerged || reminder.ID != confirmation.ID {
		t.Fatalf("reminder = %+v, want it merged into %s", reminder, confirmation.ID)
	}
	f.created("alice", mail("m3", "t2", "cancel_window", "2026-12-10"))
	f.created("alice", mail("m4", "t1", "cancel_window", "2026-12-11"))
	f.created("alice", mail("m5", "t1", "renewal", "2026-12-10"))
	if open, _ := f.service.List(t.Context(), "alice", looseend.KindCancelWindow); len(open) != 3 {
		t.Fatalf("open cancellations = %+v, want three", open)
	}
}

// A dropped item is never tracked again, whether the scan finds its message again or a reminder
// for it in the same thread.
func TestATombstoneBlocksRedetection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	item := f.created("alice", mail("m1", "t1", "cancel_window", "2026-12-10"))
	if _, changed, err := f.service.Close(t.Context(), "alice", item.ID, looseend.StatusDropped); err != nil || !changed {
		t.Fatalf("drop = %v %v", changed, err)
	}
	for _, draft := range []looseend.Draft{mail("m1", "t1", "cancel_window", "2026-12-10"), mail("m2", "t1", "cancel_window", "2026-12-10T12:00")} {
		if again := f.track("alice", draft); again.Outcome != looseend.OutcomeTombstoned || again.ID != item.ID {
			t.Fatalf("again = %+v, want the tombstone %s", again, item.ID)
		}
	}
	if open, _ := f.service.List(t.Context(), "alice", ""); len(open) != 0 {
		t.Fatalf("open = %+v, want none", open)
	}
}

// One scan adds at most two items of one sender and ten in all; the rest are left out and say why.
// An invalid draft, or one already tracked or merged, is reported as such and takes no place under
// a cap. No principal holds more than a hundred open items.
func TestCapsHold(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	other := func(message, thread string, sender int) looseend.Draft {
		draft := mail(message, thread, "renewal", "2026-12-20")
		draft.Domain = fmt.Sprintf("sender%d.example", sender)
		return draft
	}
	invalid := mail("bad", "bad-t", "return_window", "2026-12-20")
	invalid.Title = "Return at https://shop.example/return"
	f.created("alice", mail("shop-old", "shop-old-t", "return_window", "2026-12-20"))
	f.created("alice", other("other-old", "other-old-t", 99))
	drafts := []looseend.Draft{
		mail("shop-old", "shop-old-t", "return_window", "2026-12-20"),
		mail("shop-0", "shop-t0", "return_window", "2026-12-20"),
		invalid,
		mail("shop-1", "shop-t1", "return_window", "2026-12-20"),
		mail("shop-old", "shop-old-t", "return_window", "2026-12-20"),
		mail("shop-2", "shop-t2", "return_window", "2026-12-20"),
	}
	for index := range 8 {
		drafts = append(drafts, other(fmt.Sprintf("other-%d", index), fmt.Sprintf("other-t%d", index), index))
	}
	drafts = append(drafts,
		other("other-old", "other-old-t", 99),
		other("other-0-reminder", "other-t0", 0),
		invalid,
		other("other-8", "other-t8", 8),
	)
	batch, err := f.service.TrackBatch(t.Context(), "alice", drafts)
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []looseend.Outcome
	for _, tracked := range batch.Tracked {
		outcomes = append(outcomes, tracked.Outcome)
	}
	want := []looseend.Outcome{looseend.OutcomeDuplicate, looseend.OutcomeCreated, looseend.OutcomeInvalid, looseend.OutcomeCreated, looseend.OutcomeDuplicate, looseend.OutcomeSenderCapped}
	for range 8 {
		want = append(want, looseend.OutcomeCreated)
	}
	want = append(want, looseend.OutcomeDuplicate, looseend.OutcomeMerged, looseend.OutcomeInvalid, looseend.OutcomeScanCapped)
	if !slices.Equal(outcomes, want) || batch.Created != 10 || batch.Capped != 2 {
		t.Fatalf("batch = %v created %d capped %d, want %v", outcomes, batch.Created, batch.Capped, want)
	}

	for index := range looseend.MaximumOpen - 12 {
		f.created("alice", looseend.Draft{Kind: looseend.KindPromise, Title: fmt.Sprintf("Promise %d", index), Due: "2026-12-24", Source: looseend.SourceChat})
	}
	if over := f.track("alice", looseend.Draft{Kind: looseend.KindPromise, Title: "One too many", Due: "2026-12-24", Source: looseend.SourceChat}); over.Outcome != looseend.OutcomeOpenCapped {
		t.Fatalf("over the cap = %+v", over)
	}
	f.created("bob", looseend.Draft{Kind: looseend.KindPromise, Title: "Call back", Due: "2026-12-24", Source: looseend.SourceChat})
}

// Done, drop, and close end an open item once; an Undo puts it back while it is as the change left
// it, and a track's Undo deletes it only while unchanged.
func TestStatusChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	item := f.created("alice", looseend.Draft{Kind: looseend.KindPromise, Title: "Send the forms to the accountant", Counterpart: "the accountant", Due: "2026-12-04", Source: looseend.SourceChat})
	if item.SourceRef != item.ID || item.Outside {
		t.Fatalf("chat item = %+v, want its own ID as its key", item)
	}
	f.now = f.now.Add(time.Minute)
	done, changed, err := f.service.Close(ctx, "alice", item.ID, looseend.StatusDone)
	if err != nil || !changed || done.Status != looseend.StatusDone || !done.UpdatedAt.After(item.UpdatedAt) {
		t.Fatalf("done = %+v %v %v", done, changed, err)
	}
	if again, changed, err := f.service.Close(ctx, "alice", item.ID, looseend.StatusDropped); err != nil || changed || again.Status != looseend.StatusDone {
		t.Fatalf("drop after done = %+v %v %v", again, changed, err)
	}
	if _, _, err := f.service.Close(ctx, "alice", item.ID, looseend.StatusOpen); err == nil {
		t.Fatal("closing to open was accepted")
	}
	if reopened, err := f.service.Reopen(ctx, "alice", item.ID, item.UpdatedAt); err != nil || reopened {
		t.Fatalf("reopen with a stale time = %v %v", reopened, err)
	}
	if reopened, err := f.service.Reopen(ctx, "alice", item.ID, done.UpdatedAt); err != nil || !reopened || f.get("alice", item.ID).Status != looseend.StatusOpen {
		t.Fatalf("reopen = %v %v", reopened, err)
	}
	if deleted, err := f.service.Delete(ctx, "alice", item.ID, item.UpdatedAt); err != nil || deleted {
		t.Fatalf("delete after a change = %v %v", deleted, err)
	}
	other := f.created("alice", looseend.Draft{Kind: looseend.KindPromise, Title: "Return the ladder", Due: "2026-12-05", Source: looseend.SourceChat})
	if deleted, err := f.service.Delete(ctx, "alice", other.ID, other.UpdatedAt); err != nil || !deleted {
		t.Fatalf("delete = %v %v", deleted, err)
	}
	closed, changed, err := f.service.Close(ctx, "alice", item.ID, looseend.StatusClosed)
	if err != nil || !changed || closed.Status != looseend.StatusClosed {
		t.Fatalf("close = %+v %v %v", closed, changed, err)
	}
}

// Tomorrow moves the nudge to the next local day, never past the due date, and is refused on the
// due day.
func TestTomorrowNeverPassesTheDueDay(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	item := f.created("alice", mail("m1", "t1", "cancel_window", "2026-12-10"))
	if item.NudgeOn != "2026-12-08" || !item.DueAllDay {
		t.Fatalf("item = %+v, want a nudge two days before", item)
	}
	f.now = local(2026, 12, 8, 7, 20)
	if ids := f.nudge("alice", f.now); !slices.Equal(ids, []string{item.ID}) {
		t.Fatalf("nudged %v", ids)
	}
	snoozed, ok, err := f.service.Snooze(ctx, "alice", item.ID)
	if err != nil || !ok || snoozed.NudgeOn != "2026-12-09" {
		t.Fatalf("snooze = %+v %v %v", snoozed, ok, err)
	}
	if ids := f.due("alice", local(2026, 12, 8, 18, 0)); len(ids) != 0 {
		t.Fatalf("due again the same day: %v", ids)
	}
	f.now = local(2026, 12, 9, 7, 20)
	if ids := f.nudge("alice", f.now); !slices.Equal(ids, []string{item.ID}) {
		t.Fatalf("nudged after Tomorrow: %v", ids)
	}
	if snoozed, ok, err := f.service.Snooze(ctx, "alice", item.ID); err != nil || !ok || snoozed.NudgeOn != "2026-12-10" {
		t.Fatalf("snooze the day before = %+v %v %v", snoozed, ok, err)
	}
	f.now = local(2026, 12, 10, 7, 20)
	if ids := f.nudge("alice", f.now); !slices.Equal(ids, []string{item.ID}) {
		t.Fatalf("nudged on the due day: %v", ids)
	}
	current := f.get("alice", item.ID)
	if f.service.CanSnooze(current) {
		t.Fatal("Tomorrow is offered on the due day")
	}
	if after, ok, err := f.service.Snooze(ctx, "alice", item.ID); err != nil || ok || after.NudgeOn != "2026-12-10" {
		t.Fatalf("snooze on the due day = %+v %v %v", after, ok, err)
	}
	if _, _, err := f.service.Close(ctx, "alice", item.ID, looseend.StatusDone); err != nil {
		t.Fatal(err)
	}
	f.now = local(2026, 12, 9, 7, 20)
	if _, ok, _ := f.service.Snooze(ctx, "alice", item.ID); ok {
		t.Fatal("an item no longer open was snoozed")
	}
}

// An item due before the morning run of its nudge day is nudged the morning before, whether it is
// a promise due early on its day or an item found inside its lead time; Tomorrow is refused once
// the next morning's run would come after the due time.
func TestATimedDueBeforeTheMorningRunIsNudgedTheDayBefore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	early := f.created("alice", looseend.Draft{Kind: looseend.KindPromise, Title: "Drop off the keys", Due: "2026-12-04T06:00", Source: looseend.SourceChat})
	later := f.created("alice", looseend.Draft{Kind: looseend.KindPromise, Title: "Call the bank", Due: "2026-12-04T08:00", Source: looseend.SourceChat})
	found := f.created("alice", mail("m1", "t1", "cancel_window", "2026-12-02T06:00"))
	if early.NudgeOn != "2026-12-03" || later.NudgeOn != "2026-12-04" || found.NudgeOn != "2026-12-01" {
		t.Fatalf("nudge days = %s %s %s, want 2026-12-03 2026-12-04 2026-12-01", early.NudgeOn, later.NudgeOn, found.NudgeOn)
	}
	if f.service.CanSnooze(found) {
		t.Fatal("Tomorrow is offered for an item due before tomorrow's run")
	}
	f.now = local(2026, 12, 2, 7, 20)
	if !f.service.CanSnooze(early) || !f.service.CanSnooze(later) {
		t.Fatal("Tomorrow is refused while tomorrow's run comes before the due time")
	}
	f.now = local(2026, 12, 3, 7, 20)
	if ids := f.nudge("alice", f.now); !slices.Equal(ids, []string{early.ID}) {
		t.Fatalf("nudged on the day before = %v", ids)
	}
	if f.service.CanSnooze(early) || !f.service.CanSnooze(later) {
		t.Fatal("Tomorrow is offered for the promise due before tomorrow's run, or refused for the one due after it")
	}
	if _, ok, err := f.service.Snooze(t.Context(), "alice", early.ID); err != nil || ok {
		t.Fatalf("snooze past the due time = %v %v", ok, err)
	}
}

// A date without a time is due at the end of its local day, and an item past its due time expires
// without being nudged.
func TestADateExpiresSilentlyAtTheEndOfItsLocalDay(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	item := f.created("alice", looseend.Draft{Kind: looseend.KindPromise, Title: "Pay the plumber", Due: "2026-12-03", Source: looseend.SourceChat})
	if want := time.Date(2026, 12, 3, 22, 59, 59, 0, time.UTC); !item.DueAt.Equal(want) || !item.DueAllDay || item.NudgeOn != "2026-12-03" {
		t.Fatalf("item = %+v, want due at %v", item, want)
	}
	if count, err := f.service.Expire(ctx, "alice", local(2026, 12, 3, 23, 59)); err != nil || count != 0 {
		t.Fatalf("expired before the day ended: %d %v", count, err)
	}
	if count, err := f.service.Expire(ctx, "alice", local(2026, 12, 4, 0, 0)); err != nil || count != 1 {
		t.Fatalf("expired = %d %v", count, err)
	}
	if expired := f.get("alice", item.ID); expired.Status != looseend.StatusExpired || expired.NudgedOn != "" {
		t.Fatalf("item = %+v, want expired without a nudge", expired)
	}
	if ids := f.due("alice", local(2026, 12, 4, 7, 20)); len(ids) != 0 {
		t.Fatalf("an expired item is due: %v", ids)
	}
	if ids := f.due("alice", local(2026, 12, 3, 23, 59)); len(ids) != 0 {
		t.Fatalf("an expired item is due: %v", ids)
	}

	offset, err := f.service.Track(ctx, "alice", looseend.Draft{Kind: looseend.KindPromise, Title: "Call the bank", Due: "2026-12-05T09:00:00-05:00", Source: looseend.SourceChat})
	if err != nil || offset.Outcome != looseend.OutcomeCreated || !offset.Item.DueAt.Equal(time.Date(2026, 12, 5, 14, 0, 0, 0, time.UTC)) || offset.Item.DueAllDay {
		t.Fatalf("offset due = %+v %v", offset, err)
	}
	wall := f.created("alice", looseend.Draft{Kind: looseend.KindPromise, Title: "Book the table", Due: "2026-12-05T09:00", Source: looseend.SourceChat})
	if !wall.DueAt.Equal(local(2026, 12, 5, 9, 0)) {
		t.Fatalf("local due = %v", wall.DueAt)
	}
}

// A missed nudge day is caught up on the next run, and an item is not nudged twice for one day.
func TestAMissedNudgeDayIsCaughtUp(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	item := f.created("alice", mail("m1", "t1", "return_window", "2026-12-12"))
	if item.NudgeOn != "2026-12-09" {
		t.Fatalf("nudge on %s", item.NudgeOn)
	}
	if ids := f.due("alice", local(2026, 12, 8, 7, 20)); len(ids) != 0 {
		t.Fatalf("due early: %v", ids)
	}
	if ids := f.nudge("alice", local(2026, 12, 10, 7, 20)); !slices.Equal(ids, []string{item.ID}) {
		t.Fatalf("caught up %v", ids)
	}
	for _, at := range []time.Time{local(2026, 12, 10, 18, 0), local(2026, 12, 11, 7, 20), local(2026, 12, 12, 7, 20)} {
		if ids := f.due("alice", at); len(ids) != 0 {
			t.Fatalf("nudged again at %v: %v", at, ids)
		}
	}
	if nudged := f.get("alice", item.ID); nudged.NudgedOn != "2026-12-10" || nudged.UpdatedAt != item.UpdatedAt {
		t.Fatalf("nudged = %+v", nudged)
	}
}

// snoozeMidNudge presses Tomorrow on an item right after a nudge read it, as a press racing the
// nudge would.
type snoozeMidNudge struct {
	*sqlite.Store
	press func()
}

func (r *snoozeMidNudge) LoadLooseEnd(ctx context.Context, principalID, id string) (looseend.Item, bool, error) {
	item, found, err := r.Store.LoadLooseEnd(ctx, principalID, id)
	if r.press != nil {
		press := r.press
		r.press = nil
		press()
	}
	return item, found, err
}

// A Tomorrow pressed while a nudge is being recorded stands: the nudge does not write over it.
func TestTomorrowPressedDuringANudgeStands(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	item := f.created("alice", mail("m1", "t1", "cancel_window", "2026-12-10"))
	f.now = local(2026, 12, 8, 7, 20)
	racing := &snoozeMidNudge{Store: f.store, press: func() {
		if _, ok, err := f.service.Snooze(t.Context(), "alice", item.ID); err != nil || !ok {
			t.Errorf("snooze = %v %v", ok, err)
		}
	}}
	nudger, err := looseend.NewService(looseend.Config{Location: amsterdam, RunTime: morningRun, Now: func() time.Time { return f.now }}, racing, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ids := f.due("alice", f.now)
	if err := nudger.MarkNudged(t.Context(), "alice", ids, f.now); err != nil {
		t.Fatal(err)
	}
	if current := f.get("alice", item.ID); current.NudgeOn != "2026-12-09" {
		t.Fatalf("item = %+v, want Tomorrow kept", current)
	}
	if ids := f.due("alice", local(2026, 12, 9, 7, 20)); !slices.Equal(ids, []string{item.ID}) {
		t.Fatalf("due after Tomorrow = %v", ids)
	}
}

// An expiry is nudged 30 days and again 7 days before it; one found inside its lead time is nudged
// at once.
func TestAnExpiryIsNudgedTwice(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	item := f.created("alice", looseend.Draft{Kind: looseend.KindExpiry, Title: "Passport", Due: "2027-03-01", Source: looseend.SourceChat})
	if item.NudgeOn != "2027-01-30" {
		t.Fatalf("first nudge on %s", item.NudgeOn)
	}
	if ids := f.nudge("alice", local(2027, 1, 30, 7, 20)); !slices.Equal(ids, []string{item.ID}) {
		t.Fatalf("first nudge %v", ids)
	}
	if next := f.get("alice", item.ID); next.NudgeOn != "2027-02-22" || next.NudgedOn != "2027-01-30" {
		t.Fatalf("after the first nudge = %+v", next)
	}
	if ids := f.due("alice", local(2027, 2, 21, 7, 20)); len(ids) != 0 {
		t.Fatalf("due between nudges: %v", ids)
	}
	if ids := f.nudge("alice", local(2027, 2, 22, 7, 20)); !slices.Equal(ids, []string{item.ID}) {
		t.Fatalf("second nudge %v", ids)
	}
	if ids := f.due("alice", local(2027, 2, 28, 7, 20)); len(ids) != 0 {
		t.Fatalf("nudged a third time: %v", ids)
	}

	late := f.created("alice", looseend.Draft{Kind: looseend.KindExpiry, Title: "Gym membership", Due: "2026-12-05", Source: looseend.SourceChat})
	if late.NudgeOn != "2026-12-01" {
		t.Fatalf("late expiry nudge on %s", late.NudgeOn)
	}
	if ids := f.nudge("alice", f.now); !slices.Equal(ids, []string{late.ID}) {
		t.Fatalf("late nudge %v", ids)
	}
	if ids := f.due("alice", local(2026, 12, 4, 7, 20)); len(ids) != 0 {
		t.Fatalf("a late expiry was nudged twice: %v", ids)
	}
}

// An item due just after midnight on the day after a daylight-saving change is nudged on the
// right local day, which a reckoning in UTC would put a day early.
func TestANudgeDayAfterADaylightSavingChangeIsLocal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.now = local(2026, 10, 1, 9, 0)
	item := f.created("alice", mail("m1", "t1", "return_window", "2026-10-26T00:30"))
	if item.DueAt.UTC().Day() != 25 || item.NudgeOn != "2026-10-23" {
		t.Fatalf("item = %+v, want a nudge on 2026-10-23", item)
	}
	if ids := f.due("alice", local(2026, 10, 22, 7, 20)); len(ids) != 0 {
		t.Fatalf("due a day early: %v", ids)
	}
	if ids := f.due("alice", local(2026, 10, 23, 7, 20)); !slices.Equal(ids, []string{item.ID}) {
		t.Fatalf("due %v", ids)
	}
}

// Items that are no longer open are pruned 90 days after their due date; open ones never are.
func TestPruningKeepsTombstonesNinetyDays(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	dropped := f.created("alice", mail("m1", "t1", "cancel_window", "2026-12-10"))
	open := f.created("bob", mail("m1", "t1", "cancel_window", "2026-12-10"))
	if _, _, err := f.service.Close(ctx, "alice", dropped.ID, looseend.StatusDropped); err != nil {
		t.Fatal(err)
	}
	if count, err := f.service.Prune(ctx, dropped.DueAt.Add(looseend.Retention-time.Hour)); err != nil || count != 0 {
		t.Fatalf("pruned early: %d %v", count, err)
	}
	if count, err := f.service.Prune(ctx, dropped.DueAt.Add(looseend.Retention+time.Hour)); err != nil || count != 1 {
		t.Fatalf("pruned %d %v", count, err)
	}
	if _, found, _ := f.service.Get(ctx, "alice", dropped.ID); found {
		t.Fatal("the tombstone was kept")
	}
	f.get("bob", open.ID)
}

// Another principal's items are never seen, changed, or counted against.
func TestPrincipalsAreIsolated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	item := f.created("alice", mail("m1", "t1", "cancel_window", "2026-12-10"))
	if _, found, err := f.service.Get(ctx, "bob", item.ID); err != nil || found {
		t.Fatalf("bob sees alice's item: %v %v", found, err)
	}
	if _, changed, err := f.service.Close(ctx, "bob", item.ID, looseend.StatusDropped); err != nil || changed {
		t.Fatalf("bob dropped alice's item: %v %v", changed, err)
	}
	if _, ok, err := f.service.Snooze(ctx, "bob", item.ID); err != nil || ok {
		t.Fatalf("bob snoozed alice's item: %v %v", ok, err)
	}
	if listed, _ := f.service.List(ctx, "bob", ""); len(listed) != 0 {
		t.Fatalf("bob lists %+v", listed)
	}
	f.created("bob", mail("m1", "t1", "cancel_window", "2026-12-10"))
	if ids := f.due("bob", local(2026, 12, 8, 7, 20)); len(ids) != 1 || ids[0] == item.ID {
		t.Fatalf("bob's due = %v", ids)
	}
	if count, err := f.service.Expire(ctx, "bob", local(2026, 12, 11, 0, 0)); err != nil || count != 1 || f.get("alice", item.ID).Status != looseend.StatusOpen {
		t.Fatalf("bob's expiry = %d %v", count, err)
	}
}

// A title with a link is refused, while a bare web address and a phone number are stripped from it
// and an order number stays; a due date must lie ahead, and at most 400 days away. An item found
// outside chat is always outside content.
func TestValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, draft := range []looseend.Draft{
		{Kind: looseend.KindCancelWindow, Title: "Cancel at https://example.com/cancel", Due: "2026-12-10", Source: looseend.SourceChat},
		{Kind: looseend.KindCancelWindow, Title: "Cancel at www.example.com", Due: "2026-12-10", Source: looseend.SourceChat},
		{Kind: looseend.KindCancelWindow, Title: "Cancel at example.com/cancel", Due: "2026-12-10", Source: looseend.SourceChat},
		{Kind: looseend.KindCancelWindow, Title: "example.com", Due: "2026-12-10", Source: looseend.SourceChat},
		{Kind: looseend.KindCancelWindow, Title: "Booking", Due: "2026-11-30", Source: looseend.SourceChat},
		{Kind: looseend.KindCancelWindow, Title: "Booking", Due: "2028-01-10", Source: looseend.SourceChat},
		{Kind: looseend.KindCancelWindow, Title: "Booking", Due: "next week", Source: looseend.SourceChat},
		{Kind: "reservation", Title: "Booking", Due: "2026-12-10", Source: looseend.SourceChat},
		{Kind: looseend.KindCancelWindow, Title: "Booking", Due: "2026-12-10", Source: looseend.SourceMail},
		{Kind: looseend.KindCancelWindow, Title: "+31 20 123 4567", Due: "2026-12-10", Source: looseend.SourceChat},
	} {
		if tracked := f.track("alice", draft); tracked.Outcome != looseend.OutcomeInvalid || tracked.Reason == "" {
			t.Fatalf("%+v = %+v, want it refused", draft, tracked)
		}
	}
	item := f.created("alice", looseend.Draft{
		Kind: looseend.KindReturnWindow, Title: "Return order 123456789, questions: call +31 20 123 4567",
		Counterpart: "Shop +31 20 123 4567 https://shop.example www.shop.example shop.example/help", Due: "2026-12-20", Source: looseend.SourceChat,
	})
	if item.Title != "Return order 123456789, questions: call" || item.Counterpart != "Shop" || item.Outside {
		t.Fatalf("item = %+v", item)
	}
	domain := f.created("alice", looseend.Draft{Kind: looseend.KindCancelWindow, Title: "Example.com reservation", Due: "2026-12-10", Source: looseend.SourceChat})
	if domain.Title != "reservation" {
		t.Fatalf("title = %q, want the domain stripped", domain.Title)
	}
	long := f.created("alice", looseend.Draft{Kind: looseend.KindPromise, Title: strings.Repeat("a", 200), Counterpart: "shop.example", Due: "2026-12-20", Source: looseend.SourceChat})
	if len([]rune(long.Title)) != looseend.MaximumTitleRunes || long.Counterpart != "shop.example" {
		t.Fatalf("long = %+v", long)
	}
	for _, source := range []looseend.Source{looseend.SourceMail, looseend.SourceSent, looseend.SourceCalendar} {
		draft := mail("m-"+string(source), "", "cancel_window", "2026-12-10")
		draft.Source, draft.Outside = source, false
		if found := f.created("alice", draft); !found.Outside || !f.get("alice", found.ID).Outside {
			t.Fatalf("%s item = %+v, want it outside", source, found)
		}
	}
}

// The scan state starts with a 30-day backfill, keeps what the scan saves, and is forgotten when
// the switch is turned off.
func TestScanState(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	if _, found, err := f.service.ScanState(ctx, "alice"); err != nil || found {
		t.Fatalf("state before start = %v %v", found, err)
	}
	started, err := f.service.StartScan(ctx, "alice")
	if err != nil || !started.ReadUntil.Equal(f.now) || !started.BackfillBefore.Equal(f.now) || !started.BackfillUntil.Equal(f.now.Add(-looseend.BackfillWindow)) {
		t.Fatalf("started = %+v %v", started, err)
	}
	f.now = f.now.Add(time.Hour)
	if again, err := f.service.StartScan(ctx, "alice"); err != nil || !reflect.DeepEqual(again, started) {
		t.Fatalf("started again = %+v %v", again, err)
	}
	saved := looseend.ScanState{
		ReadUntil: f.now.UTC(), Stalls: 1, BackfillStalls: 1, CalendarDigest: "abc123", CalendarCursor: "2026-12-01T09:00:00Z", CalendarDeclined: []string{"0123abcd", "4567ef01"},
	}
	if err := f.service.SaveScanState(ctx, "alice", saved); err != nil {
		t.Fatal(err)
	}
	if state, found, err := f.service.ScanState(ctx, "alice"); err != nil || !found || !reflect.DeepEqual(state, saved) {
		t.Fatalf("state = %+v %v %v, want %+v", state, found, err, saved)
	}
	if _, found, _ := f.service.ScanState(ctx, "bob"); found {
		t.Fatal("bob has alice's state")
	}
	if err := f.service.ResetScan(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.service.ScanState(ctx, "alice"); err != nil || found {
		t.Fatalf("state after reset = %v %v", found, err)
	}
	// A scan that ends after the switch was turned off does not bring its state back.
	if err := f.service.SaveScanState(ctx, "alice", saved); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.service.ScanState(ctx, "alice"); err != nil || found {
		t.Fatalf("state saved after reset = %v %v", found, err)
	}
}

type noFiles struct{}

func (noFiles) SendFile(context.Context, string, delivery.File) error { return nil }

type noOffers struct{}

func (noOffers) Offer(context.Context, string, int64, int64, string, time.Duration, []approval.Proposal) ([]approval.Record, error) {
	return nil, nil
}

// The loose ends section carries open items and tombstones to another database field by field, and
// restoring it again adds nothing.
func TestExportRestoresItemsIntoAnotherDatabase(t *testing.T) {
	t.Parallel()
	source, target := newFixture(t), newFixture(t)
	ctx := t.Context()
	open := source.created("alice", mail("m1", "t1", "cancel_window", "2026-12-10"))
	dropped := source.created("alice", mail("m2", "t2", "renewal", "2026-12-15"))
	if _, _, err := source.service.Close(ctx, "alice", dropped.ID, looseend.StatusDropped); err != nil {
		t.Fatal(err)
	}
	promise := source.created("alice", looseend.Draft{Kind: looseend.KindPromise, Title: "Send the forms", Due: "2026-12-04T17:00", Source: looseend.SourceChat})
	source.nudge("alice", local(2026, 12, 8, 7, 20))
	source.created("bob", looseend.Draft{Kind: looseend.KindPromise, Title: "Bob's promise", Due: "2026-12-04", Source: looseend.SourceChat})

	keyPath := filepath.Join(t.TempDir(), "export.key")
	build := func(f *fixture) *export.Service {
		service, err := export.NewService(export.Config{KeyPath: keyPath, Now: func() time.Time { return f.now }}, f.store, noFiles{}, noOffers{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Register(looseend.Exporter()); err != nil {
			t.Fatal(err)
		}
		return service
	}
	file, err := build(source).Export(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	report, err := build(target).Restore(ctx, "alice", file.Data)
	if err != nil {
		t.Fatal(err)
	}
	if report.Additions() != 3 {
		t.Fatalf("report = %+v, want three additions", report.Sections)
	}
	for _, id := range []string{open.ID, dropped.ID, promise.ID} {
		if want, got := source.get("alice", id), target.get("alice", id); want != got {
			t.Fatalf("restored %+v, want %+v", got, want)
		}
	}
	if listed, _ := target.service.List(ctx, "bob", ""); len(listed) != 0 {
		t.Fatalf("bob's items were restored: %+v", listed)
	}
	if again := target.track("alice", mail("m2", "t2", "renewal", "2026-12-15")); again.Outcome != looseend.OutcomeTombstoned {
		t.Fatalf("a restored tombstone did not hold: %+v", again)
	}
	report, err = build(target).Restore(ctx, "alice", file.Data)
	if err != nil || report.Additions() != 0 {
		t.Fatalf("second restore = %+v %v", report.Sections, err)
	}
}

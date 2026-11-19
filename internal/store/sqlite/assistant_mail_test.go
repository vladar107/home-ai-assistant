package sqlite

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
)

var mailNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func mailRoute(principal, thread string, direction assistantmail.Direction) assistantmail.Route {
	return assistantmail.Route{PrincipalID: principal, ThreadID: thread, Direction: direction, RecordedAt: mailNow}
}

func TestAssistantMailRecordSeenIsIdempotentAndRoutesToEveryPrincipal(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	routes := []assistantmail.Route{mailRoute("alice", "t1", assistantmail.DirectionIn), mailRoute("bob", "t1", assistantmail.DirectionIn)}
	fresh, err := store.RecordSeen(ctx, "m1", mailNow, routes)
	if err != nil || !fresh {
		t.Fatalf("first record = %v %v", fresh, err)
	}
	fresh, err = store.RecordSeen(ctx, "m1", mailNow, []assistantmail.Route{mailRoute("alice", "t2", assistantmail.DirectionOut)})
	if err != nil || fresh {
		t.Fatalf("second record = %v %v", fresh, err)
	}
	for _, principal := range []string{"alice", "bob"} {
		got, found, err := store.Route(ctx, principal, "m1")
		if err != nil || !found || got.ThreadID != "t1" || got.Direction != assistantmail.DirectionIn || !got.InternalDate.Equal(mailNow) {
			t.Fatalf("%s route = %+v %v %v", principal, got, found, err)
		}
	}

	fresh, err = store.RecordSeen(ctx, "m2", mailNow.Add(time.Minute), nil)
	if err != nil || !fresh {
		t.Fatalf("unrouted record = %v %v", fresh, err)
	}
	fresh, err = store.RecordSeen(ctx, "m2", mailNow.Add(time.Minute), nil)
	if err != nil || fresh {
		t.Fatalf("unrouted again = %v %v", fresh, err)
	}
	newest, found, err := store.NewestSeenInternalDate(ctx)
	if err != nil || !found || !newest.Equal(mailNow.Add(time.Minute)) {
		t.Fatalf("newest = %v %v %v", newest, found, err)
	}
}

func TestAssistantMailNewestSeenInternalDateIsEmptyOnAFreshStore(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	if _, found, err := store.NewestSeenInternalDate(ctx); err != nil || found {
		t.Fatalf("newest = %v %v", found, err)
	}
}

func TestAssistantMailPrincipalsNeverSeeEachOthersRoutes(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	if _, err := store.RecordSeen(ctx, "m1", mailNow, []assistantmail.Route{mailRoute("alice", "t1", assistantmail.DirectionIn)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSeen(ctx, "m2", mailNow.Add(time.Minute), []assistantmail.Route{mailRoute("bob", "t2", assistantmail.DirectionOut)}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Route(ctx, "bob", "m1"); err != nil || found {
		t.Fatalf("bob loaded alice's route = %v %v", found, err)
	}
	listed, err := store.ListMailRoutes(ctx, "alice", 10)
	if err != nil || len(listed) != 1 || listed[0].GmailID != "m1" {
		t.Fatalf("alice routes = %+v %v", listed, err)
	}
	listed, err = store.ListMailRoutes(ctx, "bob", 10)
	if err != nil || len(listed) != 1 || listed[0].GmailID != "m2" {
		t.Fatalf("bob routes = %+v %v", listed, err)
	}
}

func TestAssistantMailListMailRoutesNewestFirst(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	for i, id := range []string{"m1", "m2", "m3"} {
		direction := assistantmail.DirectionIn
		if i == 1 {
			direction = assistantmail.DirectionOut
		}
		if _, err := store.RecordSeen(ctx, id, mailNow.Add(time.Duration(i)*time.Minute), []assistantmail.Route{mailRoute("alice", "t1", direction)}); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := store.ListMailRoutes(ctx, "alice", 2)
	if err != nil || len(listed) != 2 || listed[0].GmailID != "m3" || listed[1].GmailID != "m2" {
		t.Fatalf("routes = %+v %v", listed, err)
	}
}

func TestAssistantMailPrincipalsForThreadUseSentMessagesOnly(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	if _, err := store.RecordSeen(ctx, "m1", mailNow, []assistantmail.Route{mailRoute("alice", "t1", assistantmail.DirectionOut)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSeen(ctx, "m2", mailNow.Add(time.Minute), []assistantmail.Route{mailRoute("bob", "t1", assistantmail.DirectionIn)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSeen(ctx, "m3", mailNow.Add(2*time.Minute), []assistantmail.Route{mailRoute("alice", "t1", assistantmail.DirectionOut)}); err != nil {
		t.Fatal(err)
	}
	principals, err := store.PrincipalsForThread(ctx, "t1")
	if err != nil || !slices.Equal(principals, []string{"alice"}) {
		t.Fatalf("principals = %v %v", principals, err)
	}
	principals, err = store.PrincipalsForThread(ctx, "t9")
	if err != nil || len(principals) != 0 {
		t.Fatalf("unknown thread = %v %v", principals, err)
	}
}

func TestAssistantMailPendingNoticesAreOldestFirstAndMarkedOnce(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	minutes := map[string]time.Duration{"m3": 3, "m1": 1, "m2": 2}
	for _, id := range []string{"m3", "m1", "m2"} {
		if _, err := store.RecordSeen(ctx, id, mailNow.Add(minutes[id]*time.Minute), []assistantmail.Route{mailRoute("alice", "t1", assistantmail.DirectionIn)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RecordSeen(ctx, "m4", mailNow, []assistantmail.Route{mailRoute("alice", "t1", assistantmail.DirectionOut)}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingNotices(ctx, "alice", 2)
	if err != nil || len(pending) != 2 || pending[0].GmailID != "m1" || pending[1].GmailID != "m2" {
		t.Fatalf("pending = %+v %v", pending, err)
	}
	if err := store.MarkNotified(ctx, "alice", []string{"m1", "m2"}, mailNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	pending, err = store.PendingNotices(ctx, "alice", 10)
	if err != nil || len(pending) != 1 || pending[0].GmailID != "m3" {
		t.Fatalf("pending after mark = %+v %v", pending, err)
	}
	marked, _, err := store.Route(ctx, "alice", "m1")
	if err != nil || !marked.NotifiedAt.Equal(mailNow.Add(time.Hour)) {
		t.Fatalf("marked = %+v %v", marked, err)
	}
	if other, err := store.PendingNotices(ctx, "bob", 10); err != nil || len(other) != 0 {
		t.Fatalf("bob pending = %+v %v", other, err)
	}
}

func TestAssistantMailMarkNotifiedBeforeLeavesLaterMessagesPending(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	for i, id := range []string{"m1", "m2", "m3"} {
		routes := []assistantmail.Route{mailRoute("alice", "t1", assistantmail.DirectionIn), mailRoute("bob", "t1", assistantmail.DirectionIn)}
		if _, err := store.RecordSeen(ctx, id, mailNow.Add(time.Duration(i)*time.Minute), routes); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MarkNotifiedBefore(ctx, "alice", mailNow.Add(2*time.Minute), mailNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingNotices(ctx, "alice", 10)
	if err != nil || len(pending) != 1 || pending[0].GmailID != "m3" {
		t.Fatalf("alice pending = %+v %v", pending, err)
	}
	if pending, err = store.PendingNotices(ctx, "bob", 10); err != nil || len(pending) != 3 {
		t.Fatalf("bob pending = %+v %v", pending, err)
	}
}

func TestAssistantMailHistoryCursorRoundTrip(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	if _, found, err := store.HistoryCursor(ctx); err != nil || found {
		t.Fatalf("empty cursor = %v %v", found, err)
	}
	if _, found, err := store.HistoryCursorSavedAt(ctx); err != nil || found {
		t.Fatalf("empty cursor time = %v %v", found, err)
	}
	for i, id := range []string{"100", "250"} {
		at := mailNow.Add(time.Duration(i) * time.Hour)
		store.SetClock(func() time.Time { return at })
		if err := store.SaveHistoryCursor(ctx, id); err != nil {
			t.Fatal(err)
		}
		if got, found, err := store.HistoryCursor(ctx); err != nil || !found || got != id {
			t.Fatalf("cursor = %q %v %v, want %q", got, found, err, id)
		}
		if got, found, err := store.HistoryCursorSavedAt(ctx); err != nil || !found || !got.Equal(at) {
			t.Fatalf("cursor time = %v %v %v, want %v", got, found, err, at)
		}
	}
}

func TestAssistantMailSeenCoversPolledAndSentMessages(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	if _, err := store.RecordSeen(ctx, "m1", mailNow, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSent(ctx, "m2", "t2", "alice", mailNow, mailNow); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"m1": true, "m2": true, "m3": false} {
		if seen, err := store.Seen(ctx, id); err != nil || seen != want {
			t.Fatalf("seen %s = %v %v, want %v", id, seen, err, want)
		}
	}
}

func TestAssistantMailStartSendEnforcesTheDailyLimitPerPrincipal(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	dayStart := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	start := func(principal, id string, at time.Time) bool {
		t.Helper()
		allowed, err := store.StartSend(ctx, principal, id, dayStart, dayEnd, 2, at)
		if err != nil {
			t.Fatal(err)
		}
		return allowed
	}
	if !start("alice", "s1", mailNow) || !start("alice", "s2", mailNow) {
		t.Fatal("sends under the limit were refused")
	}
	if !start("alice", "s1", mailNow) {
		t.Fatal("restarting a counted send was refused")
	}
	if start("alice", "s3", mailNow) {
		t.Fatal("a send over the limit was allowed")
	}
	if !start("bob", "s4", mailNow) {
		t.Fatal("another principal's send was refused")
	}
	nextDay := dayEnd.Add(time.Hour)
	allowed, err := store.StartSend(ctx, "alice", "s5", dayEnd, dayEnd.Add(24*time.Hour), 2, nextDay)
	if err != nil || !allowed {
		t.Fatalf("next day send = %v %v", allowed, err)
	}
}

func TestAssistantMailConcurrentSendStartsAtTheLimitAllowExactlyOne(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	dayStart := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	if allowed, err := store.StartSend(ctx, "alice", "s0", dayStart, dayEnd, 2, mailNow); err != nil || !allowed {
		t.Fatalf("first send = %v %v", allowed, err)
	}
	const starts = 8
	results := make([]bool, starts)
	errs := make([]error, starts)
	var wg sync.WaitGroup
	for i := range starts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = store.StartSend(ctx, "alice", fmt.Sprintf("race-%d", i), dayStart, dayEnd, 2, mailNow)
		}()
	}
	wg.Wait()
	allowed := 0
	for i := range starts {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i] {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("%d concurrent starts were allowed, want 1", allowed)
	}
}

func TestAssistantMailRecordSentRoutesRepliesEvenWhenTheMessageWasSeenFirst(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	if fresh, err := store.RecordSeen(ctx, "m1", mailNow, nil); err != nil || !fresh {
		t.Fatalf("seen = %v %v", fresh, err)
	}
	for range 2 {
		if err := store.RecordSent(ctx, "m1", "t1", "alice", mailNow, mailNow); err != nil {
			t.Fatal(err)
		}
	}
	principals, err := store.PrincipalsForThread(ctx, "t1")
	if err != nil || !slices.Equal(principals, []string{"alice"}) {
		t.Fatalf("principals = %v %v", principals, err)
	}
}

func TestAssistantMailRecordSeenToleratesTheSamePrincipalTwice(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	routes := []assistantmail.Route{mailRoute("alice", "t1", assistantmail.DirectionIn), mailRoute("alice", "t1", assistantmail.DirectionIn)}
	if fresh, err := store.RecordSeen(ctx, "m1", mailNow, routes); err != nil || !fresh {
		t.Fatalf("record = %v %v", fresh, err)
	}
	if listed, err := store.ListMailRoutes(ctx, "alice", 10); err != nil || len(listed) != 1 {
		t.Fatalf("routes = %+v %v", listed, err)
	}
}

func TestAssistantMailStartSendRefusesAnotherPrincipalsSendID(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	dayStart := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	if allowed, err := store.StartSend(ctx, "alice", "s1", dayStart, dayEnd, 5, mailNow); err != nil || !allowed {
		t.Fatalf("alice = %v %v", allowed, err)
	}
	if allowed, err := store.StartSend(ctx, "bob", "s1", dayStart, dayEnd, 5, mailNow); err == nil || allowed {
		t.Fatalf("bob = %v %v, want an error", allowed, err)
	}
}

func TestAssistantMailContactsArePerPrincipalAndCaseInsensitive(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)

	if err := store.RecordContacts(ctx, "alice", []string{"Shop@Example.org", "help@example.net"}, mailNow); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordContacts(ctx, "alice", []string{"shop@example.org"}, mailNow.Add(time.Hour)); err != nil {
		t.Fatalf("recording a known contact again: %v", err)
	}
	known, err := store.KnownContacts(ctx, "alice", []string{"SHOP@example.org", "new@example.com", "help@example.net"})
	if err != nil || !slices.Equal(known, []string{"help@example.net", "shop@example.org"}) {
		t.Fatalf("alice's known = %v %v", known, err)
	}
	if known, err := store.KnownContacts(ctx, "bob", []string{"shop@example.org"}); err != nil || len(known) != 0 {
		t.Fatalf("bob's known = %v %v", known, err)
	}
	if known, err := store.KnownContacts(ctx, "alice", nil); err != nil || len(known) != 0 {
		t.Fatalf("no addresses = %v %v", known, err)
	}
}

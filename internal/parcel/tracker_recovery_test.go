package parcel_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/parcel"
)

func TestStartAndShutdownRunTheLoopAsAModule(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	if err := h.loop.Ready(t.Context()); err == nil {
		t.Fatal("ready before start")
	}
	if err := h.loop.Start(t.Context()); err != nil || h.loop.Ready(t.Context()) != nil || h.loop.Start(t.Context()) == nil {
		t.Fatal("start")
	}
	if err := h.loop.Shutdown(t.Context()); err != nil || h.loop.Ready(t.Context()) == nil || h.loop.Descriptor().ID != "parcels" {
		t.Fatalf("shutdown = %v", err)
	}
}

func TestATypedNilSourceIsTreatedAsAbsent(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	loop, err := parcel.NewLoop(parcel.LoopConfig{
		Repository: h.store, Sources: (*fakeSources)(nil), Mail: (*fakeMail)(nil), Untracker: (*fakeUntracker)(nil), Cards: h.cards, Enabled: h.enabled,
		Principals: func() []string { return []string{"alice"} }, Now: h.clock.Now, Location: trackerZone,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	stored := h.seed(numberA, parcel.StatusInTransit)
	loop.Cycle(t.Context())
	loop.Wait()
	h.clock.Set(start.Add(4 * time.Hour))
	loop.Cycle(t.Context())
	loop.Wait()
	if err := loop.Switched(t.Context(), "alice", true); err != nil {
		t.Fatal(err)
	}
	loop.Cycle(t.Context())
	loop.Wait()
	if _, found, err := loop.Stop(t.Context(), "alice", stored.ID); err != nil || !found {
		t.Fatalf("stop = %v %v", found, err)
	}
}

func TestMailParcelsAreFollowedOncePerParcel(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, true)
	h.seed(numberC, parcel.StatusDelivered)
	h.cycle()
	h.cards.reset()
	mailParcel := func(number string) parcel.Parcel {
		return parcel.Parcel{
			PrincipalID: "alice", Carrier: "shop", Number: number, OrderRef: "123-4567890-1234567", Sender: "Example Shop",
			Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: start, UpdatedAt: start,
		}
	}
	h.mail.setOnRun(func(time.Time) (parcel.MailResult, error) {
		ctx := context.Background()
		first, _, err := h.store.UpsertParcel(ctx, mailParcel(""))
		if err != nil {
			t.Error(err)
		}
		folded, isNew, err := h.store.UpsertParcel(ctx, mailParcel("ABCDEFGH12345"))
		if err != nil {
			t.Error(err)
		}
		return parcel.MailResult{Parcels: []parcel.Upserted{{Parcel: first, New: true}, {Parcel: folded, New: isNew}, {Parcel: first}, {Parcel: parcel.Parcel{ID: "gone"}, New: true}}}, nil
	})
	h.clock.Set(start.Add(3 * time.Hour))
	h.cycle()
	if sends := h.cards.list("send"); len(sends) != 1 {
		t.Fatalf("sends = %+v, parcels = %+v", h.cards.events, h.parcels())
	}
	for _, found := range h.parcels() {
		if found.Number == "" && found.OrderRef != "" {
			t.Fatalf("the folded parcel is still stored: %+v", found)
		}
	}
	if stored := h.byNumber("ABCDEFGH12345"); stored.CardMessage == 0 {
		t.Fatalf("no card on %+v", stored)
	}
}

func TestTheSameMailParcelListedTwiceGetsOneCardAndOneNotice(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, true)
	h.cycle()
	h.cards.reset()
	h.mail.setOnRun(func(time.Time) (parcel.MailResult, error) {
		stored, _, err := h.store.UpsertParcel(context.Background(), parcel.Parcel{
			PrincipalID: "alice", Carrier: "postnl", Number: numberA, Source: parcel.SourceMail, Status: parcel.StatusOutForDelivery, CreatedAt: start, UpdatedAt: start,
		})
		if err != nil {
			t.Error(err)
		}
		return parcel.MailResult{Parcels: []parcel.Upserted{{Parcel: stored, New: true}, {Parcel: stored, New: false}}}, nil
	})
	h.clock.Set(start.Add(3 * time.Hour))
	h.cycle()
	if sends, notices := h.cards.list("send"), h.cards.list("notice"); len(sends) != 1 || len(notices) != 1 || h.byNumber(numberA).CardMessage == 0 {
		t.Fatalf("events = %+v", h.cards.events)
	}
}

func TestANewlyAssignedSourceRunsTheBaselineAgain(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.sources.assign("entry-1")
	h.sources.set(listed(numberA, parcel.StatusInTransit))
	h.cycle()
	h.cards.reset()

	h.sources.assign("entry-1", "entry-2")
	h.sources.set(listed(numberA, parcel.StatusInTransit), listed(numberB, parcel.StatusOutForDelivery))
	h.clock.Set(start.Add(10 * time.Minute))
	h.cycle()
	if sends, summaries := h.cards.list("send"), h.cards.list("summary"); len(h.cards.list("notice")) != 0 || len(sends) != 1 || !sends[0].silent || len(summaries) != 1 || summaries[0].count != 2 {
		t.Fatalf("events = %+v", h.cards.events)
	}
	h.cards.reset()
	h.sources.set(listed(numberA, parcel.StatusInTransit), listed(numberB, parcel.StatusDelivered))
	h.clock.Set(start.Add(20 * time.Minute))
	h.cycle()
	if notices := h.cards.list("notice"); len(notices) != 1 || notices[0].status != parcel.StatusDelivered {
		t.Fatalf("events = %+v", h.cards.events)
	}
}

func TestARestartWithNothingStoredDoesNotBaselineAgain(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.cycle()
	h.restart(h.newLoop())
	h.sources.set(listed(numberA, parcel.StatusOutForDelivery))
	h.clock.Set(start.Add(10 * time.Minute))
	h.cycle()
	if notices, summaries := h.cards.list("notice"), h.cards.list("summary"); len(notices) != 1 || len(summaries) != 0 {
		t.Fatalf("events = %+v", h.cards.events)
	}
}

func TestABaselineWithoutHomeAssistantFinishesOnMailAndItsFirstReadIsSilent(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, true)
	h.sources.fail(errors.New("Home Assistant is down"))
	h.mail.setOnRun(func(now time.Time) (parcel.MailResult, error) {
		stored, created, err := h.store.UpsertParcel(t.Context(), parcel.Parcel{
			PrincipalID: "alice", Carrier: "dhl", Number: "1234567890", Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: now, UpdatedAt: now,
		})
		return parcel.MailResult{Parcels: []parcel.Upserted{{Parcel: stored, New: created}}}, err
	})
	h.cycle()
	if sends, summaries := h.cards.list("send"), h.cards.list("summary"); len(h.mail.times()) != 1 || len(sends) != 1 || !sends[0].silent || len(summaries) != 1 {
		t.Fatalf("the baseline did not finish on mail: runs %d, events %+v", len(h.mail.times()), h.cards.events)
	}
	h.mail.setOnRun(nil)
	h.cards.reset()

	h.restart(h.newLoop())
	h.sources.fail(nil)
	h.sources.set(listed(numberA, parcel.StatusOutForDelivery))
	h.clock.Set(start.Add(10 * time.Minute))
	h.cycle()
	if notices, sends := h.cards.list("notice"), h.cards.list("send"); len(notices) != 0 || len(sends) != 1 || !sends[0].silent {
		t.Fatalf("the first read after the baseline was not silent: %+v", h.cards.events)
	}
	h.cards.reset()
	h.restart(h.newLoop())
	h.sources.set(listed(numberA, parcel.StatusDelivered))
	h.clock.Set(start.Add(20 * time.Minute))
	h.cycle()
	if notices := h.cards.list("notice"); len(notices) != 1 || notices[0].status != parcel.StatusDelivered || len(h.cards.list("send")) != 0 {
		t.Fatalf("a later transition = %+v", h.cards.events)
	}
}

func TestCardsAreRepairedWhileHomeAssistantIsDown(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.cycle()
	h.seed(numberA, parcel.StatusInTransit)
	h.sources.fail(errors.New("Home Assistant is down"))
	h.clock.Set(start.Add(10 * time.Minute))
	h.cycle()
	if sends := h.cards.list("send"); len(sends) != 1 || h.byNumber(numberA).CardMessage == 0 {
		t.Fatalf("events = %+v", h.cards.events)
	}
}

func TestBaselineAfterARestoreIsSilent(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.sources.set(listed(numberA, parcel.StatusInTransit))
	h.cycle()
	h.cards.reset()
	if err := h.loop.Baseline(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	h.sources.set(listed(numberA, parcel.StatusOutForDelivery))
	h.clock.Set(start.Add(time.Minute))
	h.cycle()
	if edits, summaries := h.cards.list("edit"), h.cards.list("summary"); len(h.cards.list("notice")) != 0 || len(h.cards.list("send")) != 0 || len(edits) != 1 || len(summaries) != 1 {
		t.Fatalf("events = %+v", h.cards.events)
	}
}

func TestACardWithoutOneIsSentOnTheNextPass(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.cycle()
	h.seed(numberA, parcel.StatusUnknown)
	h.clock.Set(start.Add(10 * time.Minute))
	h.cycle()
	if sends := h.cards.list("send"); len(sends) != 1 || h.byNumber(numberA).CardMessage == 0 {
		t.Fatalf("events = %+v", h.cards.events)
	}
}

func TestAFailedCardSendIsRetried(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.cards.failSends(errors.New("Telegram is down"))
	h.sources.set(listed(numberA, parcel.StatusInTransit))
	h.cycle()
	if h.byNumber(numberA).CardMessage != 0 {
		t.Fatal("a failed send left a card")
	}
	h.cards.failSends(nil)
	h.clock.Set(start.Add(10 * time.Minute))
	h.cycle()
	if h.byNumber(numberA).CardMessage == 0 || len(h.cards.list("send")) != 2 {
		t.Fatalf("events = %+v", h.cards.events)
	}
}

func TestAFailedStoppedEditIsRetried(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, trackerZone)
	h := newTrackerHarness(t, start, false)
	h.sources.set(listed(numberA, parcel.StatusInTransit))
	h.cycle()
	h.cards.reset()
	h.cards.failEdits(errors.New("Telegram is down"))
	if _, found, err := h.loop.Stop(t.Context(), "alice", h.byNumber(numberA).ID); err != nil || !found {
		t.Fatalf("stop = %v %v", found, err)
	}
	h.cards.failEdits(nil)
	h.clock.Set(start.Add(10 * time.Minute))
	h.cycle()
	edits := h.cards.list("edit")
	if len(edits) != 2 || edits[0].mode != parcel.CardStopped || edits[1].mode != parcel.CardStopped {
		t.Fatalf("edits = %+v", edits)
	}
}

func TestNoticesFollowTheLocalClockAcrossDaylightSavingChanges(t *testing.T) {
	t.Parallel()
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		at     time.Time
		silent bool
	}{
		"06:59 on the day clocks go forward": {time.Date(2026, 3, 29, 4, 59, 0, 0, time.UTC), true},
		"07:00 on the day clocks go forward": {time.Date(2026, 3, 29, 5, 0, 0, 0, time.UTC), false},
		"21:59 summer time":                  {time.Date(2026, 10, 24, 19, 59, 0, 0, time.UTC), false},
		"22:00 summer time":                  {time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC), true},
		"21:59 after clocks go back":         {time.Date(2026, 10, 25, 20, 59, 0, 0, time.UTC), false},
		"22:00 after clocks go back":         {time.Date(2026, 10, 25, 21, 0, 0, 0, time.UTC), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newTrackerHarnessIn(t, test.at.Add(-10*time.Minute), false, amsterdam)
			h.sources.set(listed(numberA, parcel.StatusInTransit))
			h.cycle()
			h.cards.reset()
			h.sources.set(listed(numberA, parcel.StatusOutForDelivery))
			h.clock.Set(test.at)
			h.cycle()
			if notices := h.cards.list("notice"); len(notices) != 1 || notices[0].silent != test.silent {
				t.Fatalf("notices = %+v, want silent %v", notices, test.silent)
			}
		})
	}
}

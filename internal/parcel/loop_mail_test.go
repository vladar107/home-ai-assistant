package parcel_test

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
)

// mailLoop runs the loop over the mail harness's real mail source and store.
func mailLoop(t *testing.T, h *harness, sources parcel.Sources, clock *trackerClock, cards *fakeCards) *parcel.Loop {
	t.Helper()
	loop, err := parcel.NewLoop(parcel.LoopConfig{
		Repository: h.store, Sources: sources, Mail: h.source, Cards: cards, Enabled: &fakeSwitch{on: true},
		Principals: func() []string { return []string{"alice"} }, Now: clock.Now, Location: time.UTC, Languages: h.store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return loop
}

func TestOneParcelSeenByAnAccountAndByMailHasOneCardAndOneNoticePerTransition(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.source.Accounts = fakeAccounts{"postnl": true}
	clock := &trackerClock{now: mailNow}
	sources := &fakeSources{clock: clock}
	account := func(status parcel.Status) parcel.Parcel {
		return parcel.Parcel{Carrier: "postnl", Number: postnlNumber, SourceKey: "e1:" + postnlNumber + "-NL-1234AB", Status: status, Title: "Kettle"}
	}
	cards := &fakeCards{gone: map[string]bool{}}
	loop := mailLoop(t, h, sources, clock, cards)
	step := func(at time.Duration) {
		clock.Set(mailNow.Add(at))
		h.now = clock.Now()
		loop.Cycle(t.Context())
		loop.Wait()
	}
	// Mail can only guess between the carriers that share a 3S number, and the carrier's own mail
	// without a number cannot be matched to the account's parcel at all.
	shopMail := func(id, status string, age time.Duration) {
		h.deliver(id, "shipment-tracking@amazon.nl", "Your package", "Shipped with tracking number "+postnlNumber, age)
		h.expect(reply(item{MessageID: "m1", Carrier: "dhl_nl", Number: postnlNumber, Status: status}))
	}

	sources.set(account(parcel.StatusInTransit))
	h.deliver("m-1", "shipment-tracking@amazon.nl", "Your package", "Shipped with tracking number "+postnlNumber, 2*time.Hour)
	h.deliver("m-2", "ship@postnl.nl", "Your parcel is coming", "Order X1 is on its way", time.Hour)
	h.expect(reply(item{MessageID: "m1", Carrier: "dhl_nl", Number: postnlNumber, Status: "in_transit"}, item{MessageID: "m2", OrderReference: "X1", Status: "in_transit"}))
	step(0)

	sources.set(account(parcel.StatusOutForDelivery))
	step(10 * time.Minute)
	shopMail("m-3", "out_for_delivery", time.Minute)
	step(30 * time.Minute)
	shopMail("m-4", "delivered", time.Minute)
	step(60 * time.Minute)
	sources.set(account(parcel.StatusDelivered))
	step(70 * time.Minute)

	found := h.parcels()
	if len(found) != 1 || found[0].Carrier != "postnl" || found[0].Status != parcel.StatusDelivered || found[0].Source != parcel.SourceHomeAssistant {
		t.Fatalf("parcels = %+v", found)
	}
	notices := cards.list("notice")
	if len(cards.list("send")) != 1 || len(notices) != 2 || notices[0].status != parcel.StatusOutForDelivery || notices[1].status != parcel.StatusDelivered {
		t.Fatalf("events = %+v", cards.events)
	}
}

func TestProposalsFromTheMailJobAreInThePrincipalsLanguage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.store.SaveLanguage(t.Context(), "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.dhlMail()
	clock := &trackerClock{now: mailNow}
	loop := mailLoop(t, h, nil, clock, &fakeCards{gone: map[string]bool{}})
	loop.Cycle(t.Context())
	loop.Wait()
	records := h.pending("alice")
	if len(records) != 1 || records[0].Heading != "Посылки из твоей почты" || !strings.Contains(records[0].Summary, "Это взято из почты") ||
		!strings.Contains(records[0].Summary, "Отслеживать посылку "+dhlNumber) {
		t.Fatalf("records = %+v", records)
	}
}

// A parcel no source had news of for a while is dropped, not stopped: new mail about it follows it
// again. Only the user's stop keeps it away.
func TestAnIdleParcelReturnsWithNewsButAStoppedOneDoesNot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	clock := &trackerClock{now: mailNow}
	cards := &fakeCards{gone: map[string]bool{}}
	loop := mailLoop(t, h, nil, clock, cards)
	const idle, stopped = "3SABCD11111111", "3SABCD33333333"
	for _, number := range []string{idle, stopped} {
		if _, _, err := h.store.UpsertParcel(t.Context(), parcel.Parcel{
			PrincipalID: "alice", Carrier: "postnl", Number: number, Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: mailNow, UpdatedAt: mailNow,
		}); err != nil {
			t.Fatal(err)
		}
	}
	step := func(at time.Time) {
		clock.Set(at)
		h.now = at
		loop.Cycle(t.Context())
		loop.Wait()
	}
	step(mailNow)
	for _, held := range h.parcels() {
		if held.Number == stopped {
			if _, _, err := loop.Stop(t.Context(), "alice", held.ID); err != nil {
				t.Fatal(err)
			}
		}
	}

	later := mailNow.Add(parcel.IdleLifetime + time.Hour)
	step(later)
	// Retention has deleted the stopped parcel by now; its tombstones stand for it.
	if found := h.parcels(); len(found) != 0 {
		t.Fatalf("parcels after the idle drop = %+v", found)
	}
	cards.reset()
	for _, number := range []string{idle, stopped} {
		h.deliver("m-"+number, "ship@postnl.nl", "Out for delivery", "Parcel "+number+" arrives today", time.Minute)
	}
	h.expect(reply(item{MessageID: "m1", Number: idle, Status: "out_for_delivery"}, item{MessageID: "m2", Number: stopped, Status: "out_for_delivery"}))
	step(later.Add(parcel.DefaultMailIdleInterval))
	numbers := map[string]parcel.Parcel{}
	for _, held := range h.parcels() {
		numbers[held.Number] = held
	}
	if revived := numbers[idle]; len(numbers) != 1 || revived.Stopped() || revived.Status != parcel.StatusOutForDelivery || revived.CardMessage == 0 ||
		len(cards.list("notice")) != 1 {
		t.Fatalf("parcels = %+v, events = %+v", numbers, cards.events)
	}
}

package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/parcel"
)

func (w *parcelWiring) sentSoFar() []sentMessage {
	var sent []sentMessage
	for len(w.messages) > 0 {
		sent = append(sent, <-w.messages)
	}
	return sent
}

func (r *parcelRig) postcode(value string) {
	r.t.Helper()
	if err := r.store.SaveSettings(r.ctx, parcel.Settings{PrincipalID: "alice", Postcode: value}); err != nil {
		r.t.Fatal(err)
	}
}

func carrierParcel(carrierID, number string) parcel.Parcel {
	found := sampleParcel()
	carrier, _ := parcel.DefaultCarriers().ByID(carrierID)
	found.Carrier, found.Number, found.Link = carrierID, number, carrier.Link(number)
	return found
}

// Every parcel message goes out without a notification, by day too: the baseline's card and
// summary, a card for a parcel found later, and a status notice.
func TestEveryParcelMessageIsSilentByDay(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, "2026-11-10T12:00:00+01:00")
	w := newParcelWiringAt(t, map[string][]string{"alice": {"entry_dhl_a"}}, clock)
	w.cycle()
	baseline := w.sentSoFar()
	if len(baseline) != 2 || !strings.HasPrefix(baseline[1].text, "Tracking 1 parcel") {
		t.Fatalf("baseline = %+v, want a card and the summary", baseline)
	}
	first, ok := w.parcelOf("alice", "1234567890")
	if !ok || first.CardMessage == 0 {
		t.Fatalf("parcel = %+v %v", first, ok)
	}

	w.home.SetAttributes("sensor.example_dhl_a_incoming", map[string]any{"parcels": []map[string]any{
		{"carrier": "DHL", "barcode": "1234567890", "sender": "Example Seller", "status": "out_for_delivery"},
		{"carrier": "DHL", "barcode": "1234567899", "sender": "Example Seller", "status": "in_transit"},
	}})
	clock.Set(t, "2026-11-10T12:10:00+01:00")
	w.cycle()
	later := w.sentSoFar()
	notices, cards := 0, 0
	for _, message := range later {
		switch {
		case message.replyTo == first.CardMessage && strings.HasPrefix(message.text, "📦 Out for delivery"):
			notices++
		case strings.HasPrefix(message.text, "📦 DHL Express"):
			cards++
		}
	}
	if notices != 1 || cards != 1 || len(later) != 2 {
		t.Fatalf("later messages = %+v, want the notice and the new card", later)
	}
	for _, message := range append(baseline, later...) {
		if !message.silent {
			t.Fatalf("a parcel message had sound at noon: %+v", message)
		}
	}
}

// A card's link takes the user's postcode for the carriers whose page can, read when the card is
// drawn; the stored link and the logs never hold it.
func TestAParcelCardsLinkCarriesThePostcode(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	r.postcode("1234 ab")
	postnl := r.add(sampleParcel())
	if _, _, err := r.notifier.SendCard(r.ctx, postnl); err != nil {
		t.Fatal(err)
	}
	if got := r.nextMessage().keyboard[0][0].URL; got != "https://jouw.postnl.nl/track-and-trace/3SABCD1234567-NL-1234AB" {
		t.Fatalf("PostNL button = %q", got)
	}
	gls := r.add(carrierParcel("gls", "12345678901234"))
	card := r.card(gls)
	if got := r.nextMessage().keyboard[0][0].URL; got != "https://www.gls-info.nl/tracking?trackid=12345678901234&zipcode=1234AB" {
		t.Fatalf("GLS button = %q", got)
	}
	if err := r.notifier.EditCard(r.ctx, card, parcel.CardCurrent); err != nil {
		t.Fatal(err)
	}
	if got := r.nextEdit().keyboard[0][0].URL; !strings.HasSuffix(got, "&zipcode=1234AB") {
		t.Fatalf("GLS button after an edit = %q", got)
	}

	r.setWidgets("alice", true)
	widget := r.add(carrierParcel("postnl", "3SABCD7654321"))
	if _, _, err := r.notifier.SendCard(r.ctx, widget); err != nil {
		t.Fatal(err)
	}
	if sent := r.nextWidgetSent(); !strings.Contains(sent.rawCaption, `<a href="https://jouw.postnl.nl/track-and-trace/3SABCD7654321-NL-1234AB">PostNL</a>`) {
		t.Fatalf("widget caption = %q", sent.rawCaption)
	}

	for _, stored := range []parcel.Parcel{postnl, gls, widget} {
		held, _, err := r.store.LoadParcel(r.ctx, "alice", stored.ID)
		if err != nil || held.Link != stored.Link || strings.Contains(held.Link, "1234AB") {
			t.Fatalf("stored link = %q %v, want %q", held.Link, err, stored.Link)
		}
	}
	if logs := r.logs.String(); strings.Contains(logs, "1234AB") || strings.Contains(logs, "1234 AB") {
		t.Fatalf("the logs hold the postcode:\n%s", logs)
	}
}

// Without a postcode of the carrier's country, or when the settings cannot be read, the card keeps
// the plain link; an order page and a link a source supplied are never rewritten.
func TestAParcelCardKeepsItsLinkWithoutAUsablePostcode(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	plain := sampleParcel().Link
	button := func(found parcel.Parcel) string {
		r.t.Helper()
		if _, _, err := r.notifier.SendCard(r.ctx, found); err != nil {
			r.t.Fatal(err)
		}
		return r.nextMessage().keyboard[0][0].URL
	}
	postnl := r.add(sampleParcel())
	if got := button(postnl); got != plain {
		t.Fatalf("without a postcode = %q", got)
	}
	r.postcode("12345")
	if got := button(postnl); got != plain {
		t.Fatalf("with a postcode of another shape = %q", got)
	}

	r.postcode("1234 AB")
	r.settings.loadFailing.Store(true)
	if got := button(postnl); got != plain {
		t.Fatalf("with the settings unreadable = %q", got)
	}
	if logs := r.logs.String(); !strings.Contains(logs, "telegram.parcel.settings_load_failed") || strings.Contains(logs, "1234 AB") || strings.Contains(logs, "1234AB") {
		t.Fatalf("logs = %s", logs)
	}
	r.settings.loadFailing.Store(false)

	shop, _ := parcel.DefaultCarriers().ByID(parcel.ShopID)
	order := r.add(parcel.Parcel{
		CreatedAt: postnl.CreatedAt, UpdatedAt: postnl.UpdatedAt, PrincipalID: "alice", Carrier: parcel.ShopID, OrderRef: "123-4567890-1234567",
		Sender: "amazon.nl", Source: parcel.SourceMail, Status: parcel.StatusInTransit, Link: shop.OrderLink("123-4567890-1234567", "ship@amazon.nl"),
	})
	if got := button(order); got != order.Link || got == "" {
		t.Fatalf("order page = %q, want %q", got, order.Link)
	}
	supplied := sampleParcel()
	supplied.Number, supplied.Link = "3SABCD7654321", "https://example.test/track/3SABCD7654321"
	if got := button(r.add(supplied)); got != supplied.Link {
		t.Fatalf("a supplied link = %q", got)
	}
}

// A shop named like a domain is drawn with one-dot leaders, so Telegram does not link it, on the
// text card, the widget, and the notice; the stored text keeps its dots.
func TestADomainShapedNameIsDrawnUnlinkable(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	found := sampleParcel()
	found.Title, found.Sender, found.PickupPoint, found.Status = "Package from EXAMPLESHOP.NL", "shop.example", "Point at corner.example", parcel.StatusAtPickupPoint
	card := r.card(r.add(found))
	text := r.nextMessage().text
	for _, want := range []string{"Package from EXAMPLESHOP\u2024NL", "From: shop\u2024example", "Pickup point: Point at corner\u2024example"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card lacks %q:\n%s", want, text)
		}
	}
	if err := r.notifier.SendNotice(r.ctx, card); err != nil {
		t.Fatal(err)
	}
	notice := r.nextMessage().text
	r.setWidgets("alice", true)
	other := found
	other.Number = "3SABCD7654321"
	if _, _, err := r.notifier.SendCard(r.ctx, r.add(other)); err != nil {
		t.Fatal(err)
	}
	data := decodeParcelData(t, r.nextWidgetSent().data)
	for _, drawn := range []string{text, notice, data.Title, data.Sender, data.Pickup} {
		for _, name := range []string{"EXAMPLESHOP.NL", "shop.example", "corner.example"} {
			if strings.Contains(drawn, name) {
				t.Fatalf("%q is drawn with its dot in %q", name, drawn)
			}
		}
	}
	if held, _, _ := r.store.LoadParcel(r.ctx, "alice", card.ID); held.Title != found.Title || held.Sender != found.Sender {
		t.Fatalf("stored = %+v", held)
	}
}

// Mail the carrier sends itself records its domain as the sender, which the card does not show.
func TestTheCarriersOwnDomainFromMailIsNotDrawnAsTheSender(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	found := sampleParcel()
	found.Sender = "PostNL.nl"
	r.card(r.add(found))
	if text := r.nextMessage().text; strings.Contains(text, "From:") || strings.Contains(strings.ToLower(text), "postnl\u2024nl") {
		t.Fatalf("card = %q", text)
	}
	r.setWidgets("alice", true)
	found.Number = "3SABCD7654321"
	if _, _, err := r.notifier.SendCard(r.ctx, r.add(found)); err != nil {
		t.Fatal(err)
	}
	if data := decodeParcelData(t, r.nextWidgetSent().data); data.Sender != "" {
		t.Fatalf("widget sender = %q", data.Sender)
	}
}

// A carrier integration's sender that is the carrier's own domain is kept as read but not drawn; a
// shop's domain is drawn unlinkable.
func TestTheCarriersOwnDomainFromAnIntegrationIsNotDrawnAsTheSender(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, "2026-11-10T12:00:00+01:00")
	w := newParcelWiringAt(t, map[string][]string{"alice": {"entry_dhl_a"}}, clock)
	w.home.SetAttributes("sensor.example_dhl_a_incoming", map[string]any{"parcels": []map[string]any{
		{"carrier": "DHL", "barcode": "1234567890", "sender": "DHL.com", "status": "in_transit"},
		{"carrier": "DHL", "barcode": "1234567899", "sender": "shop.example", "status": "in_transit"},
	}})
	w.cycle()
	own, _ := w.parcelOf("alice", "1234567890")
	shop, _ := w.parcelOf("alice", "1234567899")
	if own.Sender != "DHL.com" || shop.Sender != "shop.example" {
		t.Fatalf("stored senders = %q and %q", own.Sender, shop.Sender)
	}
	texts := map[int64]string{}
	for _, message := range w.sentSoFar() {
		texts[message.messageID] = message.text
	}
	if text := texts[own.CardMessage]; text == "" || strings.Contains(text, "From:") {
		t.Fatalf("the carrier's own card = %q", text)
	}
	if text := texts[shop.CardMessage]; !strings.Contains(text, "From: shop\u2024example") {
		t.Fatalf("the shop's card = %q", text)
	}
}

func TestParcelStopShowsADomainShapedTitleUnlinkable(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	now := time.Now()
	held, _, err := w.store.UpsertParcel(w.ctx, parcel.Parcel{
		PrincipalID: "alice", Carrier: "postnl", Number: "3SABCD1234567", Title: "Kettle from shop.example",
		Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := w.startTurn(1, 101)
	if result := executeTool(t, w.ctx, call.request.Tools, parcel.ToolStop, `{"id":"`+held.ID+`"}`); result.IsError {
		t.Fatalf("stopping the parcel = %+v", result)
	}
	w.finishTurn(call, 1, "stopped")
	deadline := time.After(10 * time.Second)
	for {
		select {
		case message := <-w.messages:
			if strings.Contains(message.text, "shop.example") {
				t.Fatalf("a message links the title: %q", message.text)
			}
			if strings.Contains(message.text, "shop․example") {
				return
			}
		case <-deadline:
			t.Fatal("no message names the parcel")
		}
	}
}

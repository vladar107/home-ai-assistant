package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// parcelTracker stands for the tracker's Stop: it stops the parcel in the store and remembers which.
type parcelTracker struct {
	store *sqlite.Store
	cards parcel.Cards
	mu    sync.Mutex
	stops []string
	fail  bool
}

func (t *parcelTracker) Refresh(context.Context, string) error { return nil }

func (t *parcelTracker) Stop(ctx context.Context, principalID, parcelID string) (parcel.Parcel, bool, error) {
	t.mu.Lock()
	t.stops = append(t.stops, principalID+"/"+parcelID)
	fail := t.fail
	t.mu.Unlock()
	if fail {
		return parcel.Parcel{}, false, errors.New("home assistant is down")
	}
	stopped, known, err := t.store.StopParcel(ctx, principalID, parcelID, time.Now())
	if err == nil && known {
		err = t.cards.EditCard(ctx, stopped, parcel.CardStopped)
	}
	return stopped, known, err
}

func (t *parcelTracker) stopped() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.stops...)
}

type parcelSources struct {
	mu      sync.Mutex
	sources []parcel.SourceInfo
	err     error
}

func (s *parcelSources) Assigned(string) []string { return nil }

func (s *parcelSources) List(context.Context, string) ([]parcel.SourceInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]parcel.SourceInfo(nil), s.sources...), s.err
}

func (s *parcelSources) Read(context.Context, string) (parcel.Reading, error) {
	return parcel.Reading{}, nil
}

type parcelMail struct {
	mu       sync.Mutex
	readable bool
}

func (m *parcelMail) Readable(context.Context, string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readable, nil
}

type parcelHook struct {
	mu    sync.Mutex
	calls []bool
	err   error
}

func (h *parcelHook) Switched(_ context.Context, _ string, enabled bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, enabled)
	return h.err
}

func (h *parcelHook) switched() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool(nil), h.calls...)
}

// failingSettings refuses to save while failing is set.
type failingSettings struct {
	*sqlite.Store
	failing atomic.Bool
}

func (f *failingSettings) SaveSettings(ctx context.Context, settings parcel.Settings) error {
	if f.failing.Load() {
		return errors.New("disk full")
	}
	return f.Store.SaveSettings(ctx, settings)
}

// parcelRig is Alice's and Bob's chats with the parcel cards, the Stop tracking taps, and the
// Parcels page wired in as the application does.
type parcelRig struct {
	*conversationHarness
	runtime  *controlledRuntime
	notifier ParcelNotifier
	tracker  *parcelTracker
	sources  *parcelSources
	mail     *parcelMail
	hook     *parcelHook
	settings *failingSettings
}

func newParcelRig(t *testing.T, topics bool) *parcelRig {
	t.Helper()
	rig := &parcelRig{runtime: newControlledRuntime(), sources: &parcelSources{}, mail: &parcelMail{readable: true}, hook: &parcelHook{}}
	rig.conversationHarness = newConversationHarnessWithOptions(t, rig.runtime, harnessOptions{
		topics: topics, languages: true,
		deps: func(h *conversationHarness, deps *Dependencies) {
			rig.tracker = &parcelTracker{store: h.store}
			rig.notifier = ParcelNotifier{
				Client: h.client, Chats: h.store, Feeds: h.tabSet, Widgets: h.switches.Toggle(integration.WidgetsSwitch),
				Languages: h.store, Carriers: parcel.DefaultCarriers(), Location: time.FixedZone("test", 2*3600),
			}
			rig.tracker.cards = rig.notifier
			rig.settings = &failingSettings{Store: h.store}
			deps.Parcels = &ParcelTaps{Parcels: h.store, Tracker: rig.tracker}
			deps.Sections = append(deps.Sections, NewParcelsSection(ParcelsSettings{
				Toggle: h.switches.Toggle(parcel.Switch), Sources: rig.sources, Settings: rig.settings, Mail: rig.mail, Hook: rig.hook,
			}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil))
		},
	})
	h := rig.conversationHarness
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	return rig
}

func (r *parcelRig) russian() {
	r.t.Helper()
	if err := r.store.SaveLanguage(r.ctx, "alice", locale.Russian); err != nil {
		r.t.Fatal(err)
	}
}

func sampleParcel() parcel.Parcel {
	zone := time.FixedZone("test", 2*3600)
	number := "3SABCD1234567"
	carrier, _ := parcel.DefaultCarriers().ByID("postnl")
	at := time.Date(2026, 9, 28, 8, 0, 0, 0, zone)
	return parcel.Parcel{
		CreatedAt: at, UpdatedAt: at, PrincipalID: "alice", Carrier: "postnl", Number: number, Title: "Garden chair", Sender: "Example Shop",
		Source: parcel.SourceMail, Status: parcel.StatusOutForDelivery, Link: carrier.Link(number),
		WindowFrom: time.Date(2026, 9, 29, 14, 0, 0, 0, zone), WindowTo: time.Date(2026, 9, 29, 16, 0, 0, 0, zone),
	}
}

// add stores the parcel and, once it is sent, its card.
func (r *parcelRig) add(found parcel.Parcel) parcel.Parcel {
	r.t.Helper()
	stored, _, err := r.store.UpsertParcel(r.ctx, found)
	if err != nil {
		r.t.Fatal(err)
	}
	return stored
}

func (r *parcelRig) card(found parcel.Parcel, silent bool) parcel.Parcel {
	r.t.Helper()
	message, kind, err := r.notifier.SendCard(r.ctx, found, silent)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.store.SaveCard(r.ctx, found.PrincipalID, found.ID, message, kind); err != nil {
		r.t.Fatal(err)
	}
	found.CardMessage, found.CardKind = message, kind
	return found
}

func (r *parcelRig) nextWidgetSent() sentWidget {
	r.t.Helper()
	return receive(r.t, r.ctx, r.widgets.sent)
}

func decodeParcelData(t *testing.T, raw string) parcelData {
	t.Helper()
	var data parcelData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("parcel data %q: %v", raw, err)
	}
	return data
}

func TestParcelTextCardIsPlainWithItsButtons(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	found := r.add(sampleParcel())
	message, kind, err := r.notifier.SendCard(r.ctx, found, false)
	if err != nil || kind != parcel.CardText {
		t.Fatalf("SendCard = %d %q %v", message, kind, err)
	}
	sent := r.nextMessage()
	want := "📦 PostNL\nGarden chair\nFrom: Example Shop\n\nOut for delivery\nExpected: Tue 29 Sep, 14:00–16:00"
	if sent.text != want || sent.parseMode != "" || sent.threadID != 0 || sent.silent || sent.messageID != message {
		t.Fatalf("card = %+v, want plain %q", sent, want)
	}
	if len(sent.keyboard) != 1 || len(sent.keyboard[0]) != 2 {
		t.Fatalf("keyboard = %+v", sent.keyboard)
	}
	open, stop := sent.keyboard[0][0], sent.keyboard[0][1]
	if open.Text != "Open at PostNL" || open.URL != found.Link || open.CallbackData != "" {
		t.Fatalf("open button = %+v, want a URL button to %s", open, found.Link)
	}
	if stop.Text != "Stop tracking" || stop.CallbackData != "parcel:stop:"+found.ID || len(stop.CallbackData) > 64 {
		t.Fatalf("stop button = %+v", stop)
	}
}

func TestParcelTextCardInRussianWithAPickupPoint(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	r.russian()
	found := sampleParcel()
	found.Status, found.PickupPoint = parcel.StatusAtPickupPoint, "Corner shop"
	found = r.add(found)
	if _, _, err := r.notifier.SendCard(r.ctx, found, true); err != nil {
		t.Fatal(err)
	}
	sent := r.nextMessage()
	want := "📦 PostNL\nGarden chair\nОт: Example Shop\n\nОжидает в пункте выдачи\nОжидается: вт 29 сен, 14:00–16:00\nПункт выдачи: Corner shop"
	if sent.text != want || !sent.silent {
		t.Fatalf("card = %+v, want %q sent silently", sent, want)
	}
	if got := sent.keyboard[0][0].Text; got != "Открыть на сайте: PostNL" {
		t.Fatalf("open button = %q", got)
	}
	if got := sent.keyboard[0][1].Text; got != "Прекратить отслеживание" {
		t.Fatalf("stop button = %q", got)
	}
	if latin := localetest.Latin(strings.Replace(sent.text, "PostNL", "", -1), "Example", "Shop", "Corner", "shop", "Garden", "chair"); len(latin) != 0 {
		t.Fatalf("Latin words in the Russian card: %v", latin)
	}
}

func TestParcelWidgetCardInEnglishAndRussian(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	r.setWidgets("alice", true)
	found := r.add(sampleParcel())
	message, kind, err := r.notifier.SendCard(r.ctx, found, false)
	if err != nil || kind != parcel.CardWidget {
		t.Fatalf("SendCard = %d %q %v", message, kind, err)
	}
	sent := r.nextWidgetSent()
	if sent.messageID != message || sent.html != widgetTemplates[parcelWidget] || sent.silent {
		t.Fatalf("widget = %+v", sent)
	}
	data := decodeParcelData(t, sent.data)
	want := parcelData{Version: 1, ID: found.ID, Carrier: "PostNL", Title: "Garden chair", Sender: "Example Shop", Status: "out_for_delivery", Window: "Tue 29 Sep, 14:00–16:00"}
	if data != want {
		t.Fatalf("data = %+v, want %+v", data, want)
	}
	if sent.parseMode != "HTML" || sent.rawCaption != `📦 <a href="`+strings.ReplaceAll(found.Link, "&", "&amp;")+`">PostNL</a> · Out for delivery` {
		t.Fatalf("caption = %q (%s), want the carrier name linking to its page", sent.rawCaption, sent.parseMode)
	}

	r.russian()
	other := sampleParcel()
	other.Number, other.Link = "3SABCD7654321", ""
	other = r.add(other)
	if _, _, err := r.notifier.SendCard(r.ctx, other, true); err != nil {
		t.Fatal(err)
	}
	russian := r.nextWidgetSent()
	got := decodeParcelData(t, russian.data)
	if got.Lang != "ru" || got.Window != "вт 29 сен, 14:00–16:00" || !russian.silent {
		t.Fatalf("Russian data = %+v silent %v", got, russian.silent)
	}
	if russian.caption != "📦 PostNL · Доставляется" || russian.parseMode != "HTML" {
		t.Fatalf("Russian caption = %q", russian.caption)
	}
	labels := widgetLabels(parcelWidget)
	for _, word := range []string{"Доставляется", "Прекратить отслеживание", "Пункт выдачи:", "Tracking off", "Отслеживание выключено"} {
		if !strings.Contains(labels, word) {
			t.Errorf("the widget's labels lack %q", word)
		}
	}
}

// Every field is capped, so the data fits Telegram's kilobyte however long and however wide the
// texts from outside are, and the caption of the same parcel keeps its link.
func TestParcelWidgetWorstCaseFitsTheDataAndKeepsTheCaptionLink(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	r.setWidgets("alice", true)
	r.russian()
	wide := func(runes int) string { return string([]rune(strings.Repeat(`ж"\😀`, runes))[:runes]) }
	found := sampleParcel()
	// A number no listed carrier's pattern matches keeps the unlisted carrier it was stored with.
	found.Carrier, found.Number = strings.Repeat("c", 32), "EXAMPLE12345678"
	found.Title, found.Sender, found.PickupPoint = wide(60), wide(40), wide(60)
	found.Link = "https://example.test/track/" + strings.Repeat("a", 460)
	found.Status = parcel.StatusAtPickupPoint
	found = r.add(found)
	ctx := locale.With(r.ctx, locale.Russian)
	if raw := r.notifier.widgetData(ctx, found, parcel.CardCurrent); len(raw) > widgetDataLimit {
		t.Fatalf("worst-case data is %d bytes: %s", len(raw), raw)
	}
	data := decodeParcelData(t, r.notifier.widgetData(ctx, found, parcel.CardCurrent))
	if utf8.RuneCountInString(data.Title) > 60 || utf8.RuneCountInString(data.Sender) > 40 || utf8.RuneCountInString(data.Pickup) > 60 {
		t.Fatalf("texts are not capped: %+v", data)
	}

	if _, _, err := r.notifier.SendCard(r.ctx, found, false); err != nil {
		t.Fatal(err)
	}
	sent := r.nextWidgetSent()
	if len(sent.data) > widgetDataLimit || sent.parseMode != "HTML" || !strings.Contains(sent.rawCaption, `<a href="`+found.Link+`">C`+strings.Repeat("c", 31)+`</a>`) {
		t.Fatalf("worst-case widget = %d data bytes, caption %q (%s)", len(sent.data), sent.rawCaption, sent.parseMode)
	}
	if utf16Length(sent.caption) > captionLimit {
		t.Fatalf("caption is %d units", utf16Length(sent.caption))
	}
}

func TestParcelWidgetViewDrawsOutsideTextAsText(t *testing.T) {
	t.Parallel()
	template := widgetTemplates[parcelWidget]
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacent", "document.write", "eval(", "href"} {
		if strings.Contains(template, forbidden) {
			t.Errorf("the parcel view uses %s, which would draw outside text as markup", forbidden)
		}
	}
	if !strings.Contains(template, "textContent") || !strings.Contains(template, "parcel:stop:") {
		t.Fatal("the parcel view must draw with textContent and send the stop request")
	}
}

func TestParcelOutsideTextIsPlainAndCapped(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	hostile := "**bold** [click](https://evil.example/x) <b>tag</b> &amp; `code`\n> quote " + strings.Repeat("z", 20)
	found := sampleParcel()
	found.Title, found.Sender, found.PickupPoint = hostile, hostile, hostile
	found = r.add(found)
	if _, _, err := r.notifier.SendCard(r.ctx, found, false); err != nil {
		t.Fatal(err)
	}
	sent := r.nextMessage()
	if sent.parseMode != "" {
		t.Fatalf("card sent with parse mode %q", sent.parseMode)
	}
	lines := strings.Split(sent.text, "\n")
	title := lines[1]
	if !strings.HasPrefix(title, "**bold** [click](https://evil.example/x) <b>tag</b>") || utf8.RuneCountInString(title) != 60 || !strings.HasSuffix(title, "…") {
		t.Fatalf("title line = %q", title)
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "From:") && utf8.RuneCountInString(strings.TrimPrefix(line, "From: ")) > 40 {
			t.Fatalf("sender line = %q", line)
		}
		if strings.HasPrefix(line, "Pickup point:") && utf8.RuneCountInString(strings.TrimPrefix(line, "Pickup point: ")) > 60 {
			t.Fatalf("pickup line = %q", line)
		}
	}

	r.setWidgets("alice", true)
	widget := r.add(func() parcel.Parcel { p := sampleParcel(); p.Number = "3SABCD7654321"; p.Title = hostile; return p }())
	if _, _, err := r.notifier.SendCard(r.ctx, widget, false); err != nil {
		t.Fatal(err)
	}
	data := decodeParcelData(t, r.nextWidgetSent().data)
	if utf8.RuneCountInString(data.Title) != 60 || strings.Contains(data.Title, "\n") {
		t.Fatalf("widget title = %q", data.Title)
	}
}

func TestParcelCardsGoToTheParcelsTabWhileTabsAreOn(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, true)
	found := r.add(sampleParcel())
	if _, _, err := r.notifier.SendCard(r.ctx, found, false); err != nil {
		t.Fatal(err)
	}
	tab := r.wantFeedTab("Parcels", thread.FeedParcels)
	if sent := r.nextMessage(); sent.threadID != tab {
		t.Fatalf("card went to tab %d, want the Parcels tab %d", sent.threadID, tab)
	}
	card := r.card(found, false)
	if err := r.notifier.SendNotice(r.ctx, card, false); err != nil {
		t.Fatal(err)
	}
	r.nextMessage()
	if notice := r.nextMessage(); notice.threadID != tab {
		t.Fatalf("notice went to tab %d, want %d", notice.threadID, tab)
	}
}

func TestParcelCardIsEditedInPlaceToItsStates(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	card := r.card(r.add(sampleParcel()), false)
	r.nextMessage()

	card.Status = parcel.StatusAtPickupPoint
	card.PickupPoint = "Corner shop"
	if err := r.notifier.EditCard(r.ctx, card, parcel.CardCurrent); err != nil {
		t.Fatal(err)
	}
	edit := r.nextEdit()
	if edit.messageID != card.CardMessage || !strings.Contains(edit.text, "Ready for pickup") || !strings.Contains(edit.text, "Pickup point: Corner shop") || len(edit.keyboard[0]) != 2 {
		t.Fatalf("current edit = %+v", edit)
	}

	if err := r.notifier.EditCard(r.ctx, card, parcel.CardTrackingOff); err != nil {
		t.Fatal(err)
	}
	if edit := r.nextEdit(); !strings.HasSuffix(edit.text, "\n\nTracking off") || len(edit.keyboard) != 0 {
		t.Fatalf("tracking off edit = %+v", edit)
	}

	if err := r.notifier.EditCard(r.ctx, card, parcel.CardStopped); err != nil {
		t.Fatal(err)
	}
	if edit := r.nextEdit(); !strings.HasSuffix(edit.text, "\n\nStopped") || len(edit.keyboard) != 0 {
		t.Fatalf("stopped edit = %+v", edit)
	}

	if err := r.notifier.EditCard(r.ctx, card, parcel.CardIdle); err != nil {
		t.Fatal(err)
	}
	if edit := r.nextEdit(); !strings.HasSuffix(edit.text, "\n\nNo longer followed: no news for 30 days") || len(edit.keyboard) != 0 {
		t.Fatalf("idle edit = %+v", edit)
	}

	card.Status = parcel.StatusDelivered
	if err := r.notifier.EditCard(r.ctx, card, parcel.CardCurrent); err != nil {
		t.Fatal(err)
	}
	if edit := r.nextEdit(); !strings.Contains(edit.text, "\n\nDelivered") || strings.Contains(edit.text, "Expected") || len(edit.keyboard) != 1 || len(edit.keyboard[0]) != 1 || edit.keyboard[0][0].URL == "" {
		t.Fatalf("delivered edit = %+v, want the carrier link and no Stop", edit)
	}
}

func TestParcelWidgetCardIsEditedInPlace(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	r.setWidgets("alice", true)
	card := r.card(r.add(sampleParcel()), false)
	r.nextWidgetSent()

	card.Status = parcel.StatusDelivered
	if err := r.notifier.EditCard(r.ctx, card, parcel.CardCurrent); err != nil {
		t.Fatal(err)
	}
	if caption := r.nextWidgetEdit("editMessageCaption", card.CardMessage); caption.caption != "📦 PostNL · Delivered" {
		t.Fatalf("caption edit = %+v", caption)
	}
	if data := decodeParcelData(t, r.nextWidgetEdit("editMessageWidgetData", card.CardMessage).data); data.Status != "delivered" || data.Window == "" {
		t.Fatalf("data edit = %+v", data)
	}

	if err := r.notifier.EditCard(r.ctx, card, parcel.CardTrackingOff); err != nil {
		t.Fatal(err)
	}
	if caption := r.nextWidgetEdit("editMessageCaption", card.CardMessage); caption.caption != "📦 PostNL · Tracking off" {
		t.Fatalf("caption edit = %+v", caption)
	}
	if data := decodeParcelData(t, r.nextWidgetEdit("editMessageWidgetData", card.CardMessage).data); data.Mode != "o" || data.Window != "" {
		t.Fatalf("data edit = %+v", data)
	}

	if err := r.notifier.EditCard(r.ctx, card, parcel.CardIdle); err != nil {
		t.Fatal(err)
	}
	if caption := r.nextWidgetEdit("editMessageCaption", card.CardMessage); caption.caption != "📦 PostNL · No longer followed: no news for 30 days" {
		t.Fatalf("caption edit = %+v", caption)
	}
	if data := decodeParcelData(t, r.nextWidgetEdit("editMessageWidgetData", card.CardMessage).data); data.Mode != "i" || data.Window != "" {
		t.Fatalf("data edit = %+v", data)
	}
}

func TestEditOfAGoneCardReturnsErrCardGone(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	text := r.card(r.add(sampleParcel()), false)
	r.nextMessage()
	r.setWidgets("alice", true)
	other := sampleParcel()
	other.Number = "3SABCD7654321"
	widget := r.card(r.add(other), false)
	r.nextWidgetSent()

	r.mu.Lock()
	r.editStatus, r.editReason = http.StatusBadRequest, "Bad Request: message to edit not found"
	r.widgets.editStatus = http.StatusBadRequest
	r.mu.Unlock()
	if err := r.notifier.EditCard(r.ctx, text, parcel.CardCurrent); !errors.Is(err, parcel.ErrCardGone) {
		t.Fatalf("EditCard of a gone text card = %v", err)
	}
	if err := r.notifier.EditCard(r.ctx, widget, parcel.CardCurrent); !errors.Is(err, parcel.ErrCardGone) {
		t.Fatalf("EditCard of a gone widget = %v", err)
	}
	if err := r.notifier.EditCard(r.ctx, sampleParcel(), parcel.CardCurrent); !errors.Is(err, parcel.ErrCardGone) {
		t.Fatalf("EditCard of a parcel with no card = %v", err)
	}
}

func TestParcelNoticesReplyToTheCardAndAreSilentWhenAsked(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	card := r.card(r.add(sampleParcel()), false)
	r.nextMessage()

	if err := r.notifier.SendNotice(r.ctx, card, false); err != nil {
		t.Fatal(err)
	}
	notice := r.nextMessage()
	if notice.replyTo != card.CardMessage || notice.text != "📦 Out for delivery: Garden chair" || notice.silent || notice.parseMode != "" || len(notice.keyboard) != 0 {
		t.Fatalf("notice = %+v", notice)
	}

	card.Status, card.PickupPoint = parcel.StatusAtPickupPoint, "Corner shop"
	if err := r.notifier.SendNotice(r.ctx, card, true); err != nil {
		t.Fatal(err)
	}
	if notice := r.nextMessage(); notice.text != "📦 Ready for pickup at Corner shop: Garden chair" || !notice.silent || notice.replyTo != card.CardMessage {
		t.Fatalf("pickup notice = %+v", notice)
	}

	card.Status, card.Title = parcel.StatusDelivered, ""
	if err := r.notifier.SendNotice(r.ctx, card, false); err != nil {
		t.Fatal(err)
	}
	if notice := r.nextMessage(); notice.text != "📦 Delivered: PostNL parcel" {
		t.Fatalf("delivered notice = %+v", notice)
	}

	card.Status = parcel.StatusProblem
	r.russian()
	if err := r.notifier.SendNotice(r.ctx, card, false); err != nil {
		t.Fatal(err)
	}
	if notice := r.nextMessage(); notice.text != "📦 Проблема с доставкой: Посылка PostNL" {
		t.Fatalf("Russian problem notice = %+v", notice)
	}

	card.Status = parcel.StatusInTransit
	if err := r.notifier.SendNotice(r.ctx, card, false); err != nil {
		t.Fatal(err)
	}
	if err := r.notifier.SendSummary(r.ctx, "alice", 2); err != nil {
		t.Fatal(err)
	}
	if next := r.nextMessage(); !strings.HasPrefix(next.text, "Слежу за 2 ") {
		t.Fatalf("a status without a notice sent %+v before the summary", next)
	}
}

func TestParcelSummaryWordsTheCount(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	for count, want := range map[int]string{1: "Tracking 1 parcel", 3: "Tracking 3 parcels"} {
		if err := r.notifier.SendSummary(r.ctx, "alice", count); err != nil {
			t.Fatal(err)
		}
		if summary := r.nextMessage(); summary.text != want || !summary.silent || summary.replyTo != 0 {
			t.Fatalf("summary = %+v, want %q sent silently", summary, want)
		}
	}
	r.russian()
	for count, want := range map[int]string{1: "Слежу за 1 посылкой", 2: "Слежу за 2 посылками", 5: "Слежу за 5 посылками", 21: "Слежу за 21 посылкой"} {
		if err := r.notifier.SendSummary(r.ctx, "alice", count); err != nil {
			t.Fatal(err)
		}
		if summary := r.nextMessage(); summary.text != want {
			t.Fatalf("summary = %q, want %q", summary.text, want)
		}
	}
}

func TestStopTrackingFromTheTextCardsButton(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	card := r.card(r.add(sampleParcel()), false)
	r.nextMessage()

	r.push(callbackUpdate(1, 101, card.CardMessage, "parcel:stop:"+card.ID))
	r.wantAnswer(1)
	edit := r.nextEdit()
	if edit.messageID != card.CardMessage || !strings.HasSuffix(edit.text, "\n\nStopped") || len(edit.keyboard) != 0 {
		t.Fatalf("edit after Stop tracking = %+v", edit)
	}
	if stops := r.tracker.stopped(); len(stops) != 1 || stops[0] != "alice/"+card.ID {
		t.Fatalf("tracker stops = %v", stops)
	}

	// Another user's press, and one from a message that is not the card, stop nothing.
	other := r.card(r.add(func() parcel.Parcel { p := sampleParcel(); p.Number = "3SABCD7654321"; return p }()), false)
	r.nextMessage()
	r.push(callbackUpdate(2, 202, other.CardMessage, "parcel:stop:"+other.ID))
	r.wantAnswer(2)
	r.push(callbackUpdate(3, 101, other.CardMessage+500, "parcel:stop:"+other.ID))
	r.wantAnswer(3)
	r.push(textUpdate(4, 101, "/status"))
	r.nextMessage()
	if stops := r.tracker.stopped(); len(stops) != 1 {
		t.Fatalf("tracker stops = %v, want only the first", stops)
	}
}

func TestStopTrackingFromTheWidgetQuery(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	r.setWidgets("alice", true)
	card := r.card(r.add(sampleParcel()), false)
	r.nextWidgetSent()

	r.push(widgetTap(1, 101, card.CardMessage, "parcel:stop:"+card.ID))
	if answer := r.wantWidgetAnswer(1); answer != localetest.English(WidgetWorkingAnswer) {
		t.Fatalf("answer = %q", answer)
	}
	if caption := r.nextWidgetEdit("editMessageCaption", card.CardMessage); caption.caption != "📦 PostNL · Stopped" {
		t.Fatalf("caption edit = %+v", caption)
	}
	if data := decodeParcelData(t, r.nextWidgetEdit("editMessageWidgetData", card.CardMessage).data); data.Mode != "x" || data.Window != "" {
		t.Fatalf("data edit = %+v", data)
	}
	if stops := r.tracker.stopped(); len(stops) != 1 || stops[0] != "alice/"+card.ID {
		t.Fatalf("tracker stops = %v", stops)
	}

	other := r.card(r.add(func() parcel.Parcel { p := sampleParcel(); p.Number = "3SABCD7654321"; return p }()), false)
	r.nextWidgetSent()
	r.push(widgetTap(2, 202, other.CardMessage, "parcel:stop:"+other.ID))
	if answer := r.wantWidgetAnswer(2); answer != localetest.English(WidgetIgnoredAnswer) {
		t.Fatalf("a stranger's tap was answered %q", answer)
	}
	r.push(widgetTap(3, 101, card.CardMessage, "parcel:stop:"+other.ID))
	if answer := r.wantWidgetAnswer(3); answer != localetest.English(WidgetIgnoredAnswer) {
		t.Fatalf("a tap from another card was answered %q", answer)
	}
	if stops := r.tracker.stopped(); len(stops) != 1 {
		t.Fatalf("tracker stops = %v, want only the first", stops)
	}
}

func (r *parcelRig) parcelsPage(updateID int64, settings sentMessage) editedMessage {
	r.t.Helper()
	return r.openSettingsPage(updateID, 101, settings.messageID, "tools")
}

func TestASourceNotReadYetSinceARestartNeedsNoAttention(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	settings := r.requestSettings(1, 101)
	r.sources.mu.Lock()
	r.sources.sources = []parcel.SourceInfo{{EntryID: "e1", Domain: "carrier_a", Title: "owner@example.test", State: parcel.SourceUnknown}}
	r.sources.mu.Unlock()
	on := r.press(2, 101, settings.messageID, "parcels:on")
	if !strings.Contains(on.text, "• owner@example.test (carrier_a): not read yet") || strings.Contains(on.text, "not reading") {
		t.Fatalf("page = %q", on.text)
	}
	if overview := r.overview(3, settings); strings.Contains(overview, localetest.English(parcelsSourcesStale)) {
		t.Fatalf("a source not read yet raised the attention note:\n%s", overview)
	}
}

func TestParcelsPageSwitchSourcesMailAndHooks(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	settings := r.requestSettings(1, 101)
	off := r.parcelsPage(2, settings)
	if !strings.Contains(off.text, localetest.English(ParcelsOffLine)) || !strings.Contains(strings.Join(buttonData(off.keyboard), " "), "parcels:on") {
		t.Fatalf("page with Parcels off = %+v", off)
	}
	r.push(textUpdate(3, 101, "/privacy"))
	if privacy := r.nextMessage(); strings.Contains(privacy.text, localetest.English(ParcelsPrivacyLine)) {
		t.Fatal("/privacy names Parcels while it is off")
	}

	r.sources.mu.Lock()
	r.sources.sources = []parcel.SourceInfo{
		{EntryID: "e1", Domain: "carrier_a", Title: "owner@example.test", State: parcel.SourceOK},
		{EntryID: "e2", Domain: "carrier_b", Title: "**[x](https://evil.example)**", State: parcel.SourceStale},
	}
	r.sources.mu.Unlock()
	on := r.press(4, 101, settings.messageID, "parcels:on")
	if got := r.hook.switched(); len(got) != 1 || !got[0] {
		t.Fatalf("hook calls = %v", got)
	}
	for _, want := range []string{
		localetest.English(ParcelsOnLine), "• owner@example.test (carrier_a): reading", "• (x)(https://evil.example) (carrier_b): not reading, showing the last parcels",
		"Postcode: not set.", "Mail: readable.",
	} {
		if !strings.Contains(on.text, want) {
			t.Errorf("page with Parcels on lacks %q:\n%s", want, on.text)
		}
	}
	if strings.Contains(on.text, "**") {
		t.Errorf("an account title reached the page as markup:\n%s", on.text)
	}
	if data := strings.Join(buttonData(on.keyboard), " "); !strings.Contains(data, "parcels:off") || !strings.Contains(data, "parcels:postcode") || strings.Contains(data, "parcels:clear") {
		t.Fatalf("buttons = %s", data)
	}
	if overview := r.overview(5, settings); !strings.Contains(overview, "⚠ "+localetest.English(parcelsSourcesStale)) {
		t.Fatalf("the overview does not point at the source that is not reading:\n%s", overview)
	}
	r.push(textUpdate(7, 101, "/privacy"))
	if privacy := r.nextMessage(); !strings.Contains(privacy.text, localetest.English(ParcelsPrivacyLine)) {
		t.Fatalf("/privacy = %q", privacy.text)
	}

	r.mail.mu.Lock()
	r.mail.readable = false
	r.mail.mu.Unlock()
	if blocked := r.openSettingsPage(8, 101, settings.messageID, "tools"); !strings.Contains(blocked.text, "Mail: not readable.") {
		t.Fatalf("page with mail unreadable = %q", blocked.text)
	}

	r.push(callbackUpdate(9, 101, settings.messageID, "parcels:off"))
	r.wantAnswer(9)
	if turnedOff := r.nextEdit(); !strings.Contains(turnedOff.text, localetest.English(ParcelsOffLine)) {
		t.Fatalf("page after turning off = %q", turnedOff.text)
	}
	if got := r.hook.switched(); len(got) != 2 || got[1] {
		t.Fatalf("hook calls = %v, want on then off", got)
	}
	if text, err := r.service.helpText(r.ctx, "alice", "parcels"); err != nil || !strings.Contains(text, ParcelsHelp) {
		t.Fatalf("help = %q %v", text, err)
	}
}

// overview opens the settings overview and returns its text.
func (r *parcelRig) overview(updateID int64, settings sentMessage) string {
	r.t.Helper()
	r.push(callbackUpdate(updateID, 101, settings.messageID, "nav:root"))
	r.wantAnswer(updateID)
	return r.nextEdit().text
}

func TestParcelsPagePostcodeIsSetChangedAndCleared(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	if err := r.switches.SetEnabled(r.ctx, "alice", parcel.Switch, true); err != nil {
		t.Fatal(err)
	}
	settings := r.requestSettings(1, 101)
	r.parcelsPage(2, settings)

	r.push(callbackUpdate(3, 101, settings.messageID, "parcels:postcode"))
	r.wantAnswer(3)
	page := r.nextEdit()
	if ask := r.nextMessage(); !strings.Contains(ask.text, "Send your postcode as your next message") || !strings.Contains(page.text, "Postcode: not set") {
		t.Fatalf("ask = %q page = %q", ask.text, page.text)
	}
	r.push(textUpdate(4, 101, "1234 ab"))
	r.wantMessage(101, localetest.English(parcelsPostcodeSaved))
	if saved, err := r.store.LoadSettings(r.ctx, "alice"); err != nil || saved.Postcode != "1234 AB" {
		t.Fatalf("settings = %+v %v", saved, err)
	}
	if page := r.openSettingsPage(5, 101, settings.messageID, "tools"); !strings.Contains(page.text, "Postcode: 1234 AB") || !strings.Contains(strings.Join(buttonLabels(page.keyboard), "|"), "Change postcode|Clear postcode") {
		t.Fatalf("page with a postcode = %q %v", page.text, buttonLabels(page.keyboard))
	}

	r.push(callbackUpdate(6, 101, settings.messageID, "parcels:postcode"))
	r.wantAnswer(6)
	r.nextEdit()
	r.nextMessage()
	r.push(textUpdate(7, 101, "this is not a postcode at all"))
	r.wantMessage(101, localetest.English(parcelsPostcodeBad))
	if saved, _ := r.store.LoadSettings(r.ctx, "alice"); saved.Postcode != "1234 AB" {
		t.Fatalf("a refused postcode changed the setting to %q", saved.Postcode)
	}
	r.push(textUpdate(8, 101, "9999 ZZ"))
	call := receive(t, r.ctx, r.runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "ok"}
	r.wantReply(101, 8, "ok")
	if saved, _ := r.store.LoadSettings(r.ctx, "alice"); saved.Postcode != "1234 AB" {
		t.Fatalf("a message outside an intake set the postcode to %q", saved.Postcode)
	}

	r.push(callbackUpdate(9, 101, settings.messageID, "parcels:clear"))
	r.wantAnswer(9)
	if cleared := r.nextEdit(); !strings.Contains(cleared.text, localetest.English(parcelsPostcodeGone)) || !strings.Contains(cleared.text, "Postcode: not set") {
		t.Fatalf("page after clearing = %q", cleared.text)
	}
	if saved, _ := r.store.LoadSettings(r.ctx, "alice"); saved.Postcode != "" {
		t.Fatalf("postcode after clearing = %q", saved.Postcode)
	}
}

// A Set postcode button pressed after Parcels was turned off starts no intake.
func TestParcelsPagePostcodeButtonWhileOffTakesNothing(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	settings := r.requestSettings(1, 101)
	r.push(callbackUpdate(2, 101, settings.messageID, "parcels:postcode"))
	r.wantAnswer(2)
	r.nextEdit()
	r.push(textUpdate(3, 101, "1234 AB"))
	call := receive(t, r.ctx, r.runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "ok"}
	r.wantReply(101, 3, "ok")
	if saved, _ := r.store.LoadSettings(r.ctx, "alice"); saved.Postcode != "" {
		t.Fatalf("postcode = %q", saved.Postcode)
	}
}

func TestParcelWidgetDataIsReadableByOlderViews(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	data := r.notifier.widgetData(locale.With(r.ctx, locale.English), sampleParcel(), parcel.CardCurrent)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &keys); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"v", "i", "c", "t", "f", "s", "w"} {
		if _, found := keys[key]; !found {
			t.Errorf("data lacks %q: %s", key, data)
		}
	}
	for key := range keys {
		if len(key) != 1 {
			t.Errorf("data key %q is not one letter", key)
		}
	}
}

func TestStopTrackingTransientFailureDoesNotStallTheUpdate(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	card := r.card(r.add(sampleParcel()), false)
	r.nextMessage()
	r.tracker.mu.Lock()
	r.tracker.fail = true
	r.tracker.mu.Unlock()

	r.push(callbackUpdate(1, 101, card.CardMessage, "parcel:stop:"+card.ID))
	r.wantAnswer(1)
	r.push(callbackUpdate(2, 101, card.CardMessage, "parcel:stop:"+card.ID))
	r.wantAnswer(2)
	r.waitUntil("a stop attempt for each tap", func() bool { return len(r.tracker.stopped()) >= 2 })
	time.Sleep(50 * time.Millisecond)
	if stops := r.tracker.stopped(); len(stops) != 2 {
		t.Fatalf("tracker stops = %v, want one attempt per tap and no retry of the update", stops)
	}
}

func TestParcelsPageSurvivesAFailingSourceListAndHook(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	r.sources.mu.Lock()
	r.sources.err = errors.New("home assistant is down")
	r.sources.mu.Unlock()
	r.hook.mu.Lock()
	r.hook.err = errors.New("queue is full")
	r.hook.mu.Unlock()
	settings := r.requestSettings(1, 101)
	r.openSettingsPage(2, 101, settings.messageID, "tools")

	on := r.press(3, 101, settings.messageID, "parcels:on")
	for _, want := range []string{"Home Assistant accounts: could not be checked.", localetest.English(parcelsCatchUpNote)} {
		if !strings.Contains(on.text, want) {
			t.Errorf("page lacks %q:\n%s", want, on.text)
		}
	}
	if overview := r.overview(4, settings); !strings.Contains(overview, "⚠ "+localetest.English(parcelsSourcesUnknownNote)) {
		t.Fatalf("overview = %s", overview)
	}
	// A press that changes nothing does not signal the tracker again.
	r.press(5, 101, settings.messageID, "parcels:on")
	if got := r.hook.switched(); len(got) != 1 {
		t.Fatalf("hook calls = %v, want one", got)
	}
}

func TestParcelsPostcodeNeedsADigitAndSurvivesASaveFailure(t *testing.T) {
	t.Parallel()
	r := newParcelRig(t, false)
	if err := r.switches.SetEnabled(r.ctx, "alice", parcel.Switch, true); err != nil {
		t.Fatal(err)
	}
	settings := r.requestSettings(1, 101)
	r.parcelsPage(2, settings)
	ask := func(update int64) {
		r.push(callbackUpdate(update, 101, settings.messageID, "parcels:postcode"))
		r.wantAnswer(update)
		r.nextEdit()
		r.nextMessage()
	}
	ask(3)
	r.push(textUpdate(4, 101, "thanks"))
	r.wantMessage(101, localetest.English(parcelsPostcodeBad))

	ask(5)
	r.settings.failing.Store(true)
	r.push(textUpdate(6, 101, "1234 AB"))
	r.wantMessage(101, localetest.English(parcelsPostcodeFailed))
	r.settings.failing.Store(false)
	r.push(textUpdate(7, 101, "1234 AB"))
	r.wantMessage(101, localetest.English(parcelsPostcodeSaved))
	if saved, _ := r.store.LoadSettings(r.ctx, "alice"); saved.Postcode != "1234 AB" {
		t.Fatalf("postcode = %q", saved.Postcode)
	}
	text, err := r.service.helpText(r.ctx, "alice", "parcels")
	if err != nil || strings.Contains(text, "1234") || !strings.Contains(text, "Postcode: set.") {
		t.Fatalf("help shows the postcode to the model: %q %v", text, err)
	}
}

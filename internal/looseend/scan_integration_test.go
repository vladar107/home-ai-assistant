package looseend_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/intake"
	"github.com/vladar107/home-ai-assistant/internal/looseend"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	scanMailbox = "alice@example.com"
	emptyReply  = `{"items":[],"closes":[],"left":0}`
)

var offeredMessage = regexp.MustCompile(`(?m)^- account \S+, id (\S+)$`)

type scanGrants struct{ sim *googlesim.Simulator }

func (g scanGrants) Accounts(_ context.Context, principalID string, _ google.Product) ([]google.Account, error) {
	if principalID != "alice" {
		return nil, nil
	}
	return []google.Account{{ID: "id-alice", Address: scanMailbox}}, nil
}

func (g scanGrants) AccessToken(context.Context, string, string, google.Product, bool) (string, error) {
	return g.sim.Access(scanMailbox, googlesim.Grant{Mail: true, Calendar: true}), nil
}

type scanAccess struct{ mail, calendar bool }

func (a *scanAccess) MailReadable(context.Context, string) (bool, error)     { return a.mail, nil }
func (a *scanAccess) CalendarReadable(context.Context, string) (bool, error) { return a.calendar, nil }

// scanCalendar is alice's calendar as the scan reads it; the application's adapter over the
// Calendar connector is tested in wiring.
type scanCalendar struct{ events []looseend.CalendarEvent }

func (c *scanCalendar) CalendarEvents(_ context.Context, _ string, from, to time.Time) ([]looseend.CalendarEvent, []string, bool, error) {
	var listed []looseend.CalendarEvent
	for _, event := range c.events {
		if !event.Start.Before(from) && event.Start.Before(to) {
			listed = append(listed, event)
		}
	}
	return listed, []string{scanMailbox}, true, nil
}

func (c *scanCalendar) remove(id string) {
	c.events = slices.DeleteFunc(c.events, func(event looseend.CalendarEvent) bool { return event.ID == id })
}

// scanModel plays the model of the scan's turn: respond opens what it likes with the real Gmail
// tools and answers.
type scanModel struct {
	prompts []string
	respond func(ctx context.Context, prompt string) string
}

func (m *scanModel) RunCheck(ctx context.Context, _, message string) (string, string, error) {
	m.prompts = append(m.prompts, message)
	return m.respond(ctx, message), "", nil
}

type scanFixture struct {
	*fixture
	sim    *googlesim.Simulator
	events *scanCalendar
	gmail  *gmail.Service
	access *scanAccess
	model  *scanModel
	scan   *looseend.Scan
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	f := &scanFixture{fixture: newFixture(t), access: &scanAccess{mail: true, calendar: true}}
	f.model = &scanModel{respond: func(context.Context, string) string { return emptyReply }}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.sim = googlesim.New(t)
	f.sim.ListNewestFirst()
	grants := scanGrants{sim: f.sim}
	var err error
	if f.gmail, err = gmail.NewService(gmail.Config{APIBaseURL: f.sim.URL(), HTTPClient: f.sim.Client()}, grants, logger); err != nil {
		t.Fatal(err)
	}
	f.events = &scanCalendar{}
	f.scan, err = looseend.NewScan(looseend.ScanConfig{
		Checker: f.model, Access: f.access, Mail: f.gmail, Calendar: f.events, Intake: f.store,
	}, f.service, logger)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// start turns alice's scan on, with its backfill.
func (f *scanFixture) start() {
	f.t.Helper()
	if _, err := f.service.StartScan(f.t.Context(), "alice"); err != nil {
		f.t.Fatal(err)
	}
}

// begin turns alice's scan on and reads mail from now on, without a backfill.
func (f *scanFixture) begin() {
	f.t.Helper()
	f.start()
	if err := f.service.SaveScanState(f.t.Context(), "alice", looseend.ScanState{ReadUntil: f.now.UTC()}); err != nil {
		f.t.Fatal(err)
	}
}

// event is an event in alice's calendar ten days ahead, changed an hour ago.
func (f *scanFixture) event(id, description, organizer string) looseend.CalendarEvent {
	return looseend.CalendarEvent{
		Account: scanMailbox, CalendarID: "primary", ID: id, Start: f.now.AddDate(0, 0, 10), Description: description, Organizer: organizer,
		Updated: f.now.Add(-time.Hour).UTC().Format(time.RFC3339),
	}
}

// nextDay moves the clock a day on, to the next daily run.
func (f *scanFixture) nextDay() { f.now = f.now.AddDate(0, 0, 1) }

func (f *scanFixture) run() error { return f.scan.Scan(f.t.Context(), "alice", f.now) }

func (f *scanFixture) mustRun() {
	f.t.Helper()
	if err := f.run(); err != nil {
		f.t.Fatal(err)
	}
}

// message is a read inbox message from the sender, received ago before now, whose
// Authentication-Results show DMARC passing.
func (f *scanFixture) message(id, thread, from, subject string, ago time.Duration) googlesim.Message {
	return googlesim.Message{
		ID: id, ThreadID: thread, From: from, To: scanMailbox, Subject: subject, Snippet: subject, Labels: []string{"INBOX"},
		Received: f.now.Add(-ago), PlainText: subject, AuthenticationResults: googlesim.AuthenticationResults(senderAddress(from), "pass"),
	}
}

func (f *scanFixture) deliver(id, thread, from, subject string, ago time.Duration) {
	f.sim.AddMailbox(scanMailbox, f.message(id, thread, from, subject, ago))
}

func senderAddress(from string) string {
	if start, end := strings.Index(from, "<"), strings.Index(from, ">"); start >= 0 && end > start {
		return from[start+1 : end]
	}
	return from
}

// open reads the messages in full with the Gmail tool, as the model would.
func (f *scanFixture) open(ctx context.Context, ids ...string) {
	f.t.Helper()
	for _, id := range ids {
		arguments, _ := json.Marshal(map[string]string{"account": scanMailbox, "id": id})
		result, err := f.gmail.Execute(ctx, "alice", tools.Call{Name: "gmail_read_message", Arguments: arguments})
		if err != nil || result.IsError {
			f.t.Fatalf("read %s: %+v %v", id, result, err)
		}
	}
}

func (f *scanFixture) openItems() []looseend.Item {
	f.t.Helper()
	items, err := f.service.List(f.t.Context(), "alice", "")
	if err != nil {
		f.t.Fatal(err)
	}
	return items
}

func (f *scanFixture) state() looseend.ScanState {
	f.t.Helper()
	state, _, err := f.service.ScanState(f.t.Context(), "alice")
	if err != nil {
		f.t.Fatal(err)
	}
	return state
}

func mailItem(id, kind, title, due string) string {
	return fmt.Sprintf(`{"account":%q,"message_id":%q,"kind":%q,"title":%q,"due":%q}`, scanMailbox, id, kind, title, due)
}

func reply(items ...string) string {
	return `{"items":[` + strings.Join(items, ",") + `],"closes":[],"left":0}`
}

// A read booking confirmation with free cancellation, found in the first scan's backfill, is
// tracked with its sender's registrable domain whatever the model says, its thread, and a nudge two
// days before the deadline; a second scan that reports it again adds nothing.
func TestScanTracksAReadBookingConfirmation(t *testing.T) {
	f := newScanFixture(t)
	f.start()
	f.deliver("m1", "t1", "Grand Hotel <bookings@mail.hotel.example>", "Your stay: free cancellation until 10 December", 48*time.Hour)
	f.model.respond = func(ctx context.Context, prompt string) string {
		if !strings.Contains(prompt, "id m1") {
			t.Errorf("the backfill was not offered:\n%s", prompt)
		}
		f.open(ctx, "m1")
		return `{"items":[{"account":"alice@example.com","message_id":"m1","kind":"cancel_window","title":"Grand Hotel stay","due":"2026-12-10","counterpart":"elsewhere.example"}],"closes":[],"left":0}`
	}
	f.mustRun()
	items := f.openItems()
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	item := items[0]
	if item.Counterpart != "hotel.example" || item.ThreadRef != "t1" || item.SourceRef != "m1" || item.Account != scanMailbox ||
		item.Source != looseend.SourceMail || !item.Outside || item.Kind != looseend.KindCancelWindow || item.NudgeOn != "2026-12-08" {
		t.Fatalf("item = %+v", item)
	}
	if state := f.state(); !state.BackfillUntil.IsZero() || !state.BackfillBefore.IsZero() {
		t.Fatalf("the drained backfill is still pending: %+v", state)
	}

	f.nextDay()
	f.deliver("m2", "t2", "News <news@paper.example>", "Weekly news", time.Hour)
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "m1", "m2")
		return reply(mailItem("m1", "cancel_window", "Grand Hotel stay", "2026-12-10"))
	}
	f.mustRun()
	if len(f.model.prompts) != 2 || len(f.openItems()) != 1 {
		t.Fatalf("turns %d, items %+v", len(f.model.prompts), f.openItems())
	}
}

// A confirmation and a reminder in one thread for the same deadline are one item; two bookings from
// one sender in separate threads stay two.
func TestScanMergesOnlyWithinAThread(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.nextDay()
	f.deliver("c1", "t1", "bookings@hotel.example", "Booking confirmed", 3*time.Hour)
	f.deliver("r1", "t1", "bookings@hotel.example", "Reminder: cancel for free until 20 December", 2*time.Hour)
	f.deliver("b1", "t2", "stays@platform.example", "Booking one confirmed", 2*time.Hour)
	f.deliver("b2", "t3", "stays@platform.example", "Booking two confirmed", time.Hour)
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "c1", "r1", "b1", "b2")
		return reply(
			mailItem("c1", "cancel_window", "Hotel stay", "2026-12-20"),
			mailItem("r1", "cancel_window", "Hotel stay", "2026-12-20"),
			mailItem("b1", "cancel_window", "First stay", "2026-12-20"),
			mailItem("b2", "cancel_window", "Second stay", "2026-12-20"),
		)
	}
	f.mustRun()
	items := f.openItems()
	refs := map[string]bool{}
	for _, item := range items {
		refs[item.SourceRef] = true
	}
	if len(items) != 3 || !refs["c1"] || !refs["b1"] || !refs["b2"] {
		t.Fatalf("items = %+v", items)
	}
}

// A dropped item is not tracked again, from its own message or a reminder in its thread.
func TestScanDoesNotTrackADroppedItemAgain(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.nextDay()
	f.deliver("m1", "t1", "orders@shop.example", "Your order: returns accepted until 30 December", time.Hour)
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "m1")
		return reply(mailItem("m1", "return_window", "Shop order", "2026-12-30"))
	}
	f.mustRun()
	items := f.openItems()
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	if _, changed, err := f.service.Close(t.Context(), "alice", items[0].ID, looseend.StatusDropped); err != nil || !changed {
		t.Fatalf("drop = %v %v", changed, err)
	}
	f.nextDay()
	f.deliver("m2", "t1", "orders@shop.example", "Reminder: returns accepted until 30 December", time.Hour)
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "m1", "m2")
		return reply(mailItem("m1", "return_window", "Shop order", "2026-12-30"), mailItem("m2", "return_window", "Shop order", "2026-12-30"))
	}
	f.mustRun()
	if items := f.openItems(); len(items) != 0 {
		t.Fatalf("a dropped item came back: %+v", items)
	}
}

// A message the inbox intake recorded belongs to it and is skipped.
func TestScanSkipsMailTheIntakeRecorded(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.nextDay()
	if _, err := f.store.CreateIntakeRecord(t.Context(), intake.Record{
		ID: "0000000000000001", PrincipalID: "alice", Source: intake.SourceMail, SourceRef: scanMailbox + "/m1", Kind: "invoice", Title: "Membership invoice",
		ActionRequired: "Pay it", Priority: intake.PriorityAction, CreatedAt: f.now.UTC(), UpdatedAt: f.now.UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	f.deliver("m1", "t1", "billing@club.example", "Membership renews on 15 January", 2*time.Hour)
	f.deliver("m2", "t2", "help@stream.example", "Your trial ends on 20 December", time.Hour)
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "m1", "m2")
		return reply(mailItem("m1", "renewal", "Club membership", "2027-01-15"), mailItem("m2", "trial_end", "Streaming trial", "2026-12-20"))
	}
	f.mustRun()
	if items := f.openItems(); len(items) != 1 || items[0].SourceRef != "m2" {
		t.Fatalf("items = %+v", items)
	}
}

// An item is refused when its message was not opened in full, when its due date is past or too far
// ahead, or when its title holds a link; a phone number is stripped from a title.
func TestScanRefusesWhatItCannotVouchFor(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.nextDay()
	for index, domain := range []string{"one", "two", "three", "four", "five"} {
		f.deliver(fmt.Sprintf("m%d", index+1), fmt.Sprintf("t%d", index+1), "bookings@"+domain+".example", "Free cancellation", time.Hour)
	}
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "m1", "m2", "m3", "m4")
		return reply(
			mailItem("m1", "cancel_window", "Past stay", "2026-11-30"),
			mailItem("m2", "cancel_window", "Far stay", "2028-06-01"),
			mailItem("m3", "cancel_window", "Manage at https://three.example/manage", "2026-12-20"),
			mailItem("m4", "cancel_window", "Lake stay, call +31 20 123 4567", "2026-12-20"),
			mailItem("m5", "cancel_window", "Unopened stay", "2026-12-20"),
		)
	}
	f.mustRun()
	items := f.openItems()
	if len(items) != 1 || items[0].SourceRef != "m4" || strings.Contains(items[0].Title, "4567") || !strings.Contains(items[0].Title, "Lake stay") {
		t.Fatalf("items = %+v", items)
	}
}

// The per-sender and per-scan caps hold.
func TestScanCapsHold(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.nextDay()
	var ids, found []string
	for index := range 13 {
		id, domain := fmt.Sprintf("m%02d", index), "hotel.example"
		if index >= 3 {
			domain = fmt.Sprintf("shop%d.example", index)
		}
		f.deliver(id, "t-"+id, "orders@"+domain, "Free cancellation", time.Duration(13-index)*time.Minute)
		ids = append(ids, id)
		found = append(found, mailItem(id, "cancel_window", "Booking "+id, fmt.Sprintf("2027-01-%02d", index+1)))
	}
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, ids...)
		return reply(found...)
	}
	f.mustRun()
	items := f.openItems()
	hotel := 0
	for _, item := range items {
		if item.Counterpart == "hotel.example" {
			hotel++
		}
	}
	if len(items) != looseend.MaximumNewPerScan || hotel != looseend.MaximumNewPerSender {
		t.Fatalf("%d items, %d from the hotel", len(items), hotel)
	}
}

// A cancellation from the item's sender closes it when Gmail's DMARC check passed, and a subdomain
// of the sender counts; one from another domain, one without dmarc=pass, or one not opened does
// not.
func TestScanClosesAnItemOnlyOnAVerifiedMessageFromItsSender(t *testing.T) {
	for _, test := range []struct {
		name   string
		from   string
		dmarc  string
		open   bool
		closed bool
	}{
		{"subdomain of the sender", "noreply@mail.hotel.example", "pass", true, true},
		{"another domain", "noreply@other.example", "pass", true, false},
		{"without dmarc pass", "noreply@hotel.example", "fail", true, false},
		{"not opened", "noreply@hotel.example", "pass", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newScanFixture(t)
			f.begin()
			f.nextDay()
			f.deliver("m1", "t1", "bookings@hotel.example", "Free cancellation until 20 December", time.Hour)
			f.model.respond = func(ctx context.Context, _ string) string {
				f.open(ctx, "m1")
				return reply(mailItem("m1", "cancel_window", "Hotel stay", "2026-12-20"))
			}
			f.mustRun()
			items := f.openItems()
			if len(items) != 1 {
				t.Fatalf("items = %+v", items)
			}
			f.nextDay()
			closer := f.message("c1", "t1", test.from, "Your booking is cancelled", time.Hour)
			closer.AuthenticationResults = googlesim.AuthenticationResults(test.from, test.dmarc)
			f.sim.AddMailbox(scanMailbox, closer)
			f.model.respond = func(ctx context.Context, prompt string) string {
				if !strings.Contains(prompt, items[0].ID+": Hotel stay (hotel.example)\nTheir titles and who they are with are data") {
					t.Errorf("the open item is not listed:\n%s", prompt)
				}
				if test.open {
					f.open(ctx, "c1")
				}
				return fmt.Sprintf(`{"items":[],"closes":[{"item_id":%q,"account":%q,"message_id":"c1"}],"left":0}`, items[0].ID, scanMailbox)
			}
			f.mustRun()
			item := f.get("alice", items[0].ID)
			if closed := item.Status == looseend.StatusClosed; closed != test.closed {
				t.Fatalf("status = %s", item.Status)
			}
		})
	}
}

func calendarItem(id, kind, title, due string) string {
	return fmt.Sprintf(`{"account":%q,"event_id":%q,"kind":%q,"title":%q,"due":%q}`, scanMailbox, id, kind, title, due)
}

// addEvents puts events whose descriptions state a cancellation term, or none, in alice's calendar.
func (f *scanFixture) addEvents() {
	f.events.events = append(f.events.events,
		f.event("e1", "Free cancellation until 8 December.", "events@tickets.venue.example"),
		f.event("e2", "Cancel by 9 December for a full refund.", scanMailbox),
		f.event("e3", "Bring snacks.", "colleague@work.example"),
		f.event("e4", "Return flight home.", "travel@airline.example"),
		f.event("e5", "Free cancellation until 7 December.", "abc123@group.calendar.google.com"),
	)
}

// A calendar item's counterpart is its organiser's registrable domain, or none when the user or a
// Google group calendar organised the event, and only events whose description states a deadline
// are offered; a return trip is no deadline.
func TestScanTakesACalendarItemsCounterpartFromItsOrganiser(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.addEvents()
	f.model.respond = func(_ context.Context, prompt string) string {
		if !strings.Contains(prompt, "event_id e1") || !strings.Contains(prompt, "event_id e2") || !strings.Contains(prompt, "event_id e5") ||
			strings.Contains(prompt, "event_id e3") || strings.Contains(prompt, "event_id e4") {
			t.Errorf("the offered events are wrong:\n%s", prompt)
		}
		return reply(calendarItem("e1", "cancel_window", "Concert tickets", "2026-12-08"), calendarItem("e2", "cancel_window", "Dinner booking", "2026-12-09"),
			calendarItem("e3", "cancel_window", "Team lunch", "2026-12-09"), calendarItem("e5", "cancel_window", "Club night", "2026-12-07"))
	}
	f.mustRun()
	counterparts := map[string]string{}
	for _, item := range f.openItems() {
		if item.Source != looseend.SourceCalendar || !item.Outside || item.Account != scanMailbox {
			t.Fatalf("item = %+v", item)
		}
		counterparts[item.SourceRef] = item.Counterpart
	}
	if len(counterparts) != 3 || counterparts["e1"] != "venue.example" || counterparts["e2"] != "" || counterparts["e5"] != "" {
		t.Fatalf("counterparts = %v", counterparts)
	}
}

// A calendar item whose event is gone from the calendar is closed by code, without a model turn.
func TestScanClosesAnItemWhoseEventIsGone(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.addEvents()
	f.model.respond = func(context.Context, string) string {
		return reply(calendarItem("e1", "cancel_window", "Concert tickets", "2026-12-08"), calendarItem("e2", "cancel_window", "Dinner booking", "2026-12-09"),
			calendarItem("e5", "cancel_window", "Club night", "2026-12-07"))
	}
	f.mustRun()
	items := f.openItems()
	if len(items) != 3 || items[1].SourceRef != "e1" {
		t.Fatalf("items = %+v", items)
	}
	f.nextDay()
	f.events.remove("e1")
	f.mustRun()
	if item := f.get("alice", items[1].ID); item.Status != looseend.StatusClosed {
		t.Fatalf("status = %s", item.Status)
	}
	if item := f.get("alice", items[2].ID); item.Status != looseend.StatusOpen {
		t.Fatalf("the item of an event still there is %s", item.Status)
	}
	if len(f.model.prompts) != 1 {
		t.Fatalf("%d turns", len(f.model.prompts))
	}
}

// A backfill across more than a run's messages drains over several daily runs, newest first, and
// a cap drops none of it.
func TestScanDrainsABackfillOverSeveralRuns(t *testing.T) {
	f := newScanFixture(t)
	f.start()
	const total = 25
	for index := range total {
		id := fmt.Sprintf("s%02d", index+1)
		f.deliver(id, "t-"+id, fmt.Sprintf("orders@shop%d.example", index+1), "Your order: return within 30 days", time.Duration(index+1)*time.Hour)
	}
	f.model.respond = func(ctx context.Context, prompt string) string {
		var found []string
		for _, match := range offeredMessage.FindAllStringSubmatch(prompt, -1) {
			f.open(ctx, match[1])
			var number int
			_, _ = fmt.Sscanf(match[1], "s%d", &number)
			found = append(found, mailItem(match[1], "return_window", "Order "+match[1], fmt.Sprintf("2027-01-%02d", number)))
		}
		return reply(found...)
	}
	runs := 0
	for ; runs < 5 && (runs == 0 || !f.state().BackfillUntil.IsZero()); runs++ {
		f.mustRun()
		f.nextDay()
	}
	if items := f.openItems(); len(items) != total || runs != 3 {
		t.Fatalf("%d items after %d runs", len(items), runs)
	}
	f.mustRun()
	if len(f.model.prompts) != 3 {
		t.Fatalf("%d turns, want none after the backfill drained", len(f.model.prompts))
	}
}

// Without new mail, a pending backfill, or a calendar change, no model turn runs, and the cursor
// still moves on; with neither mail nor calendar readable, nothing runs at all.
func TestScanSkipsTheTurnWithNothingNew(t *testing.T) {
	f := newScanFixture(t)
	f.access.mail, f.access.calendar = false, false
	f.mustRun()
	if _, found, err := f.service.ScanState(t.Context(), "alice"); err != nil || found {
		t.Fatalf("a scan without mail or calendar started: %v %v", found, err)
	}
	f.access.mail, f.access.calendar = true, true
	f.begin()
	f.events.events = append(f.events.events, f.event("e1", "Dentist check-up.", "desk@dentist.example"))
	f.mustRun()
	f.nextDay()
	f.mustRun()
	if len(f.model.prompts) != 0 {
		t.Fatalf("%d turns without anything new", len(f.model.prompts))
	}
	if state := f.state(); !state.ReadUntil.Equal(f.now.UTC()) || state.CalendarDigest == "" {
		t.Fatalf("state = %+v", state)
	}
	f.nextDay()
	f.deliver("m1", "t1", "news@paper.example", "Weekly news", time.Hour)
	f.mustRun()
	if len(f.model.prompts) != 1 {
		t.Fatalf("%d turns with new mail", len(f.model.prompts))
	}
}

// A reply that is not the JSON asked for records nothing and leaves the cursor; the second in a row
// skips the window once, and the next stall counts from the start again.
func TestScanStallsOnAnUnreadableReplyAndSkipsTheWindowOnce(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.nextDay()
	f.deliver("m1", "t1", "bookings@hotel.example", "Free cancellation until 20 December", time.Hour)
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "m1")
		return "I found a booking with free cancellation."
	}
	since := f.state().ReadUntil
	unreadable := func() *looseend.FailedError {
		t.Helper()
		var failed *looseend.FailedError
		if err := f.run(); !errors.As(err, &failed) || failed.Kind != "unreadable" {
			t.Fatalf("run = %v", err)
		}
		return failed
	}
	if failed := unreadable(); !failed.SkippedFrom.IsZero() {
		t.Fatalf("the first stall skipped mail: %+v", failed)
	}
	if state := f.state(); !state.ReadUntil.Equal(since) || state.Stalls != 1 || len(f.openItems()) != 0 {
		t.Fatalf("after one stall: %+v, items %+v", state, f.openItems())
	}
	f.nextDay()
	if failed := unreadable(); !failed.SkippedUntil.Equal(f.now.UTC()) {
		t.Fatalf("the second stall did not skip the window: %+v", failed)
	}
	skipped := f.now.UTC()
	if state := f.state(); !state.ReadUntil.Equal(skipped) || state.Stalls != 0 {
		t.Fatalf("after the skip: %+v", state)
	}
	f.nextDay()
	f.deliver("m2", "t2", "help@stream.example", "Your trial ends on 20 December", time.Hour)
	if failed := unreadable(); !failed.SkippedFrom.IsZero() {
		t.Fatalf("a first stall after the skip skipped mail: %+v", failed)
	}
	f.model.respond = func(ctx context.Context, prompt string) string {
		if !strings.Contains(prompt, fmt.Sprintf("after:%d", skipped.Add(-5*time.Minute).Unix())) {
			t.Errorf("the scan does not read from the skip:\n%s", prompt)
		}
		f.open(ctx, "m2")
		return reply(mailItem("m2", "trial_end", "Streaming trial", "2026-12-20"))
	}
	f.mustRun()
	if items := f.openItems(); len(items) != 1 || items[0].SourceRef != "m2" {
		t.Fatalf("items = %+v", items)
	}

	// A usable reply that twice in a row reads none of the new mail it leaves skips that mail too,
	// and says so once.
	f.nextDay()
	f.deliver("m3", "t3", "orders@shop.example", "Your order: returns accepted until 30 December", time.Hour)
	f.model.respond = func(context.Context, string) string { return `{"items":[],"closes":[],"left":1}` }
	from := f.state().ReadUntil
	f.mustRun()
	if state := f.state(); !state.ReadUntil.Equal(from) || state.Stalls != 1 {
		t.Fatalf("after one stall on mail left: %+v", state)
	}
	f.nextDay()
	var stalled *looseend.FailedError
	if err := f.run(); !errors.As(err, &stalled) || stalled.Kind != "stalled" || !stalled.SkippedFrom.Equal(from) || !stalled.SkippedUntil.Equal(f.now.UTC()) {
		t.Fatalf("second stall on mail left = %v", err)
	}
	if state := f.state(); !state.ReadUntil.Equal(f.now.UTC()) || state.Stalls != 0 {
		t.Fatalf("after skipping mail left: %+v", state)
	}
}

// Calendar events past one scan's share are offered by the next ones, and the digest is kept only
// once every one was offered.
func TestScanOffersManyCalendarEventsOverSeveralRuns(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	for index := range 12 {
		event := f.event(fmt.Sprintf("e%02d", index+1), "Free cancellation until the day before.", "events@venue.example")
		event.Start = event.Start.Add(time.Duration(index) * time.Hour)
		f.events.events = append(f.events.events, event)
	}
	var offered [][]string
	f.model.respond = func(_ context.Context, prompt string) string {
		offered = append(offered, regexp.MustCompile(`event_id (e\d+)`).FindAllString(prompt, -1))
		return emptyReply
	}
	f.mustRun()
	if state := f.state(); state.CalendarCursor == "" || state.CalendarDigest != "" {
		t.Fatalf("after the first share: %+v", state)
	}
	f.nextDay()
	f.mustRun()
	if state := f.state(); state.CalendarCursor != "" || state.CalendarDigest == "" {
		t.Fatalf("after every event was offered: %+v", state)
	}
	f.nextDay()
	f.mustRun()
	if len(offered) != 2 || len(offered[0]) != 10 || len(offered[1]) != 2 || offered[1][0] != "event_id e11" || offered[1][1] != "event_id e12" {
		t.Fatalf("offered %v", offered)
	}
}

// A backfill message no scan gets through is skipped after two scans in a row, so it does not cost
// a turn every day, and the rest of the window drains.
func TestScanSkipsABackfillMessageItCannotGetThrough(t *testing.T) {
	f := newScanFixture(t)
	f.start()
	f.deliver("b1", "t1", "orders@one.example", "Return within 30 days", time.Hour)
	f.deliver("b2", "t2", "orders@two.example", "Return within 30 days", 2*time.Hour)
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "b2")
		return emptyReply
	}
	before := f.state().BackfillBefore
	f.mustRun()
	if state := f.state(); !state.BackfillBefore.Equal(before) || state.BackfillStalls != 1 {
		t.Fatalf("after one stall: %+v", state)
	}
	f.nextDay()
	f.mustRun()
	if state := f.state(); !state.BackfillUntil.IsZero() || state.BackfillStalls != 0 {
		t.Fatalf("the stuck message was not skipped: %+v", state)
	}
	f.nextDay()
	f.mustRun()
	if len(f.model.prompts) != 2 {
		t.Fatalf("%d turns", len(f.model.prompts))
	}
}

// A backfill item past the open cap counts as read, so the backfill drains.
func TestScanPassesBackfillItemsPastTheOpenCap(t *testing.T) {
	f := newScanFixture(t)
	for index := range looseend.MaximumOpen {
		f.created("alice", looseend.Draft{Kind: looseend.KindPromise, Title: fmt.Sprintf("Promise %d", index), Due: "2027-01-01", Source: looseend.SourceChat})
	}
	f.start()
	f.deliver("b1", "t1", "orders@shop.example", "Return within 30 days", time.Hour)
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "b1")
		return reply(mailItem("b1", "return_window", "Shop order", "2026-12-30"))
	}
	f.mustRun()
	if state := f.state(); !state.BackfillUntil.IsZero() {
		t.Fatalf("the backfill waits on the open cap: %+v", state)
	}
}

// At a public mailbox provider the counterpart is the sender's whole address, and only that
// address closes the item.
func TestScanTakesAPublicMailboxSenderWhole(t *testing.T) {
	for _, test := range []struct {
		name   string
		from   string
		closed bool
	}{
		{"the same address", "seller.one@gmail.com", true},
		{"another address there", "someone.else@gmail.com", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newScanFixture(t)
			f.begin()
			f.nextDay()
			f.deliver("m1", "t1", "Seller <Seller.One@gmail.com>", "Returns accepted until 20 December", time.Hour)
			f.model.respond = func(ctx context.Context, _ string) string {
				f.open(ctx, "m1")
				return reply(mailItem("m1", "return_window", "Second-hand bike", "2026-12-20"))
			}
			f.mustRun()
			items := f.openItems()
			if len(items) != 1 || items[0].Counterpart != "seller.one@gmail.com" {
				t.Fatalf("items = %+v", items)
			}
			f.nextDay()
			f.deliver("c1", "t1", test.from, "Return received, refund sent", time.Hour)
			f.model.respond = func(ctx context.Context, _ string) string {
				f.open(ctx, "c1")
				return fmt.Sprintf(`{"items":[],"closes":[{"item_id":%q,"account":%q,"message_id":"c1"}],"left":0}`, items[0].ID, scanMailbox)
			}
			f.mustRun()
			if closed := f.get("alice", items[0].ID).Status == looseend.StatusClosed; closed != test.closed {
				t.Fatalf("closed = %v", closed)
			}
		})
	}
}

// A scan that ends after the switch was turned off does not bring its state back.
func TestScanEndingAfterTheSwitchWasTurnedOffKeepsNoState(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.nextDay()
	f.deliver("m1", "t1", "bookings@hotel.example", "Free cancellation until 20 December", time.Hour)
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "m1")
		if err := f.service.ResetScan(ctx, "alice"); err != nil {
			t.Error(err)
		}
		return reply(mailItem("m1", "cancel_window", "Hotel stay", "2026-12-20"))
	}
	f.mustRun()
	if _, found, err := f.service.ScanState(t.Context(), "alice"); err != nil || found {
		t.Fatalf("state after the switch was turned off = %v %v", found, err)
	}
	f.nextDay()
	f.mustRun()
	if len(f.model.prompts) != 1 {
		t.Fatalf("a scan without a state ran %d turns", len(f.model.prompts))
	}
}

// sent is a message alice sent to the recipient, ago before now.
func (f *scanFixture) sent(id, thread, to, text string, ago time.Duration) googlesim.Message {
	return googlesim.Message{
		ID: id, ThreadID: thread, From: "Alice <" + scanMailbox + ">", To: to, Subject: "Re: The form", Snippet: text, Labels: []string{"SENT"},
		Received: f.now.Add(-ago), PlainText: text,
	}
}

// The messages alice sent since the last scan are offered by code with when she sent them. A
// promise in one is tracked as a sent item, outside, in its thread, with its first recipient's
// domain as the counterpart whatever the model says, and nudged on its due day; a promise from
// received mail, or another kind from a sent message, is refused. A sent message read past an
// inbox message left unread does not move the cursor past that one.
func TestScanTracksAPromiseInSentMail(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.nextDay()
	f.deliver("i1", "t0", "news@paper.example", "Morning news", 4*time.Hour)
	f.deliver("i2", "t2", "bookings@hotel.example", "Your stay: free cancellation until 20 December", 3*time.Hour)
	f.sim.AddMailbox(scanMailbox,
		f.sent("s1", "t1", "Alex <alex@partner.example>, Kim <kim@other.example>", "I'll send you the signed form on Friday.", 2*time.Hour),
		f.sent("s2", "t3", "Jamie <jamie@friends.example>", "Let's catch up sometime!", time.Hour),
	)
	f.model.respond = func(ctx context.Context, prompt string) string {
		if !strings.Contains(prompt, "id s1, sent ") || !strings.Contains(prompt, "id s2, sent ") || !strings.Contains(prompt, `"promise"`) {
			t.Errorf("the sent mail was not offered:\n%s", prompt)
		}
		f.open(ctx, "i1", "s1", "s2")
		return `{"items":[` + strings.Join([]string{
			`{"account":"alice@example.com","message_id":"s1","kind":"promise","title":"Send the signed form to Alex","due":"2026-12-04","counterpart":"elsewhere.example"}`,
			mailItem("s1", "cancel_window", "Signed form", "2026-12-05"),
			mailItem("i1", "promise", "Read the news", "2026-12-06"),
		}, ",") + `],"closes":[],"left":1}`
	}
	f.mustRun()
	items := f.openItems()
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	item := items[0]
	if item.Kind != looseend.KindPromise || item.Source != looseend.SourceSent || item.SourceRef != "s1" || item.ThreadRef != "t1" ||
		item.Counterpart != "partner.example" || !item.Outside || item.NudgeOn != "2026-12-04" {
		t.Fatalf("item = %+v", item)
	}
	if state := f.state(); !state.ReadUntil.Equal(f.now.Add(-4 * time.Hour).UTC().Truncate(time.Millisecond)) {
		t.Fatalf("read until %s, want the inbox message read, not the later sent ones", state.ReadUntil)
	}
}

// A day with nothing new but sent mail still runs the turn, and a promise's thanks from its
// counterpart does not close it.
func TestScanReadsSentMailAloneAndNeverClosesAPromise(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	f.nextDay()
	f.sim.AddMailbox(scanMailbox, f.sent("s1", "t1", "alex@partner.example", "I will call you tomorrow.", time.Hour))
	f.model.respond = func(ctx context.Context, _ string) string {
		f.open(ctx, "s1")
		return reply(`{"account":"alice@example.com","message_id":"s1","kind":"promise","title":"Call Alex","due":"2026-12-03"}`)
	}
	f.mustRun()
	promise := f.openItems()
	if len(f.model.prompts) != 1 || len(promise) != 1 {
		t.Fatalf("turns %d, items %+v", len(f.model.prompts), promise)
	}

	f.nextDay()
	f.deliver("r1", "t1", "alex@partner.example", "Thanks, talk tomorrow", time.Hour)
	f.model.respond = func(ctx context.Context, prompt string) string {
		if strings.Contains(prompt, promise[0].ID) {
			t.Errorf("the promise was offered for closing:\n%s", prompt)
		}
		f.open(ctx, "r1")
		return `{"items":[],"closes":[{"item_id":"` + promise[0].ID + `","account":"alice@example.com","message_id":"r1"}],"left":0}`
	}
	f.mustRun()
	if items := f.openItems(); len(items) != 1 || items[0].ID != promise[0].ID {
		t.Fatalf("items = %+v, want the promise still open", items)
	}
}

// openOffered plays a model that opens every backfill message offered and reports a return window
// for each, oldest first, the reverse of the order they were offered in.
func (f *scanFixture) openOffered(ctx context.Context, prompt string) []string {
	var found []string
	for _, match := range offeredMessage.FindAllStringSubmatch(prompt, -1) {
		f.open(ctx, match[1])
		found = append(found, mailItem(match[1], "return_window", "Order "+match[1], "2027-01-15"))
	}
	slices.Reverse(found)
	return found
}

// A backfill of one sender's mail drains over several runs and drops none of it, though the model
// reports it oldest first and the per-sender cap holds back all but two a run.
func TestScanDrainsABackfillFromOneSenderReportedOldestFirst(t *testing.T) {
	f := newScanFixture(t)
	f.start()
	const total = 5
	for index := range total {
		id := fmt.Sprintf("s%d", index+1)
		f.deliver(id, "t-"+id, "orders@shop.example", "Your order: return within 30 days", time.Duration(index+1)*time.Hour)
	}
	f.model.respond = func(ctx context.Context, prompt string) string { return reply(f.openOffered(ctx, prompt)...) }
	runs := 0
	for ; runs < 6 && (runs == 0 || !f.state().BackfillUntil.IsZero()); runs++ {
		f.mustRun()
		if state := f.state(); state.BackfillStalls != 0 {
			t.Fatalf("run %d stalled: %+v", runs+1, state)
		}
		f.nextDay()
	}
	if items := f.openItems(); len(items) != total || runs != 3 {
		t.Fatalf("%d items after %d runs", len(items), runs)
	}
}

// A backfill message the per-scan cap holds back behind new mail was read, so it is no stall: it
// waits for a run with room however many runs that takes.
func TestScanKeepsABackfillMessageACapHeldBack(t *testing.T) {
	f := newScanFixture(t)
	f.start()
	f.deliver("b1", "t-b1", "orders@old.example", "Your order: return within 30 days", 2*time.Hour)
	day := 0
	f.model.respond = func(ctx context.Context, prompt string) string {
		found := f.openOffered(ctx, prompt)
		for index := range looseend.MaximumNewPerScan {
			id := fmt.Sprintf("n%d-%d", day, index)
			if day < 3 {
				f.open(ctx, id)
				found = append([]string{mailItem(id, "cancel_window", "Booking "+id, fmt.Sprintf("2027-02-%02d", index+1))}, found...)
			}
		}
		return reply(found...)
	}
	for day = 1; day <= 3; day++ {
		f.nextDay()
		if day < 3 {
			for index := range looseend.MaximumNewPerScan {
				id := fmt.Sprintf("n%d-%d", day, index)
				f.deliver(id, "t-"+id, fmt.Sprintf("bookings@stay%d-%d.example", day, index), "Free cancellation", time.Duration(index+1)*time.Minute)
			}
		}
		f.mustRun()
		if state := f.state(); day < 3 && (state.BackfillUntil.IsZero() || state.BackfillStalls != 0) {
			t.Fatalf("day %d: the capped backfill message was skipped or stalled: %+v", day, state)
		}
	}
	items := f.openItems()
	if !slices.ContainsFunc(items, func(item looseend.Item) bool { return item.SourceRef == "b1" }) || !f.state().BackfillUntil.IsZero() {
		t.Fatalf("the capped backfill message was not tracked once there was room: %+v", f.state())
	}
}

// An event the model was offered and declined is not offered again while only the window slides,
// nor is one tracked in any status; once the declined event changes it is offered again, and the
// declined key of its old version is forgotten.
func TestScanDoesNotOfferADeclinedOrTrackedEventAgainUntilItChanges(t *testing.T) {
	f := newScanFixture(t)
	f.begin()
	entering := f.event("e2", "Bring snacks.", "colleague@work.example")
	entering.Start = f.now.Add(looseend.CalendarWindow + 12*time.Hour)
	f.events.events = append(f.events.events,
		f.event("e1", "Free cancellation until 8 December.", "events@venue.example"),
		f.event("e3", "Cancel by 9 December for a full refund.", "desk@restaurant.example"),
		entering,
	)
	var offered [][]string
	f.model.respond = func(_ context.Context, prompt string) string {
		offered = append(offered, regexp.MustCompile(`event_id (e\d+)`).FindAllString(prompt, -1))
		if len(offered) == 1 {
			return reply(calendarItem("e3", "cancel_window", "Dinner booking", "2026-12-09"))
		}
		return emptyReply
	}
	f.mustRun()
	items := f.openItems()
	if len(items) != 1 || items[0].SourceRef != "e3" || len(f.state().CalendarDeclined) != 1 {
		t.Fatalf("items %+v, state %+v", items, f.state())
	}
	if _, _, err := f.service.Close(t.Context(), "alice", items[0].ID, looseend.StatusDropped); err != nil {
		t.Fatal(err)
	}
	digest := f.state().CalendarDigest

	f.nextDay()
	f.mustRun()
	if state := f.state(); len(f.model.prompts) != 1 || state.CalendarDigest == digest {
		t.Fatalf("turns %d, state %+v: want the slid window read without a turn", len(f.model.prompts), state)
	}

	f.nextDay()
	f.events.events[0].Updated = f.now.UTC().Format(time.RFC3339)
	f.mustRun()
	if len(offered) != 2 || !slices.Equal(offered[1], []string{"event_id e1"}) {
		t.Fatalf("offered %v, want e1 alone again after it changed", offered)
	}
	if declined := f.state().CalendarDeclined; len(declined) != 1 {
		t.Fatalf("declined = %v, want only the changed event's new key", declined)
	}
}

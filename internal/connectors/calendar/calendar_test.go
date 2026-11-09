package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/testutil/calendarsim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	principal = "principal-1"
	work      = "ann@work.example"
	home      = "ann@home.example"
)

type fakeGrants struct {
	mu        sync.Mutex
	enabled   []google.Account
	current   map[string]string
	owners    map[string]string
	broken    map[string]bool
	refreshes int
	issued    int
}

func newGrants(addresses ...string) *fakeGrants {
	grants := &fakeGrants{current: map[string]string{}, owners: map[string]string{}, broken: map[string]bool{}}
	for _, address := range addresses {
		grants.enabled = append(grants.enabled, google.Account{ID: "id-" + address, Address: address})
		grants.issue("id-"+address, address)
	}
	return grants
}

func (g *fakeGrants) issue(accountID, address string) string {
	g.issued++
	token := fmt.Sprintf("token-%d", g.issued)
	g.current[accountID] = token
	g.owners[token] = address
	return token
}

func (g *fakeGrants) accountFor(token string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	address, ok := g.owners[token]
	return address, ok
}

func (g *fakeGrants) token(address string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.current["id-"+address]
}

func (g *fakeGrants) Accounts(_ context.Context, _ string, product google.Product) ([]google.Account, error) {
	if product != google.ProductCalendar {
		panic("calendar asked for another product")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]google.Account(nil), g.enabled...), nil
}

func (g *fakeGrants) AccessToken(_ context.Context, _, accountID string, product google.Product, forceRefresh bool) (string, error) {
	if product != google.ProductCalendar {
		panic("calendar asked for another product")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.broken[accountID] {
		return "", fmt.Errorf("access token: %w", google.ErrNotConnected)
	}
	if forceRefresh {
		g.refreshes++
		address := g.owners[g.current[accountID]]
		return g.issue(accountID, address), nil
	}
	return g.current[accountID], nil
}

type harness struct {
	t       *testing.T
	sim     *calendarsim.Simulator
	grants  *fakeGrants
	service *Service
}

func newHarness(t *testing.T, addresses ...string) *harness {
	t.Helper()
	grants := newGrants(addresses...)
	sim := calendarsim.New(t, grants.accountFor)
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(Config{Location: location, APIBaseURL: sim.APIBaseURL(), HTTPClient: sim.Client()}, grants,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, sim: sim, grants: grants, service: service}
}

func (h *harness) call(name string, arguments any) tools.Result {
	h.t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		h.t.Fatal(err)
	}
	result, err := h.service.Execute(context.Background(), principal, tools.Call{Name: name, Arguments: raw})
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *harness) events(arguments map[string]any) tools.Result {
	h.t.Helper()
	return h.call(toolEvents, arguments)
}

func utc(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC)
}

func requireText(t *testing.T, result tools.Result, wantError bool, fragments ...string) {
	t.Helper()
	if result.IsError != wantError {
		t.Fatalf("IsError = %v, want %v; text:\n%s", result.IsError, wantError, result.Text)
	}
	for _, fragment := range fragments {
		if !strings.Contains(result.Text, fragment) {
			t.Fatalf("result lacks %q:\n%s", fragment, result.Text)
		}
	}
}

func requireOrder(t *testing.T, text string, fragments ...string) {
	t.Helper()
	last := -1
	for _, fragment := range fragments {
		index := strings.Index(text, fragment)
		if index < 0 {
			t.Fatalf("result lacks %q:\n%s", fragment, text)
		}
		if index < last {
			t.Fatalf("%q is out of order in:\n%s", fragment, text)
		}
		last = index
	}
}

func TestEventsMergeAccountsAndCalendarsInStartOrder(t *testing.T) {
	h := newHarness(t, work, home)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Ann Work", Primary: true})
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: "team@group.calendar.google.com", Summary: "Team", SummaryOverride: "Team plans"})
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: "noise@group.calendar.google.com", Summary: "Noise", Hidden: true})
	h.sim.AddCalendar(home, calendarsim.Calendar{ID: home, Summary: "Ann Home", Primary: true})

	h.sim.AddEvents(work, work,
		calendarsim.Event{ID: "standup-1", Summary: "Standup", Start: utc(5, 7, 30), End: utc(5, 7, 45), Location: "Room 4",
			Attendees: []calendarsim.Attendee{{Email: work, Self: true, Response: "accepted"}, {Email: "bob@work.example", Response: "declined"}}},
		calendarsim.Event{ID: "standup-2", Summary: "Standup", Start: utc(6, 7, 30), End: utc(6, 7, 45), RecurringEventID: "standup",
			Attendees: []calendarsim.Attendee{{Email: work, Self: true}}},
	)
	h.sim.AddEvents(work, "team@group.calendar.google.com",
		calendarsim.Event{ID: "offsite", Summary: "Offsite", AllDay: true, Start: utc(5, 0, 0), End: utc(7, 0, 0)},
	)
	h.sim.AddEvents(work, "noise@group.calendar.google.com",
		calendarsim.Event{ID: "hidden-1", Summary: "Hidden holiday", Start: utc(5, 9, 0), End: utc(5, 10, 0)},
	)
	h.sim.AddEvents(home, home,
		calendarsim.Event{ID: "dentist", Summary: "Dentist", Start: time.Date(2026, 10, 5, 12, 0, 0, 0, time.FixedZone("", -4*3600)), End: time.Date(2026, 10, 5, 13, 0, 0, 0, time.FixedZone("", -4*3600)),
			Attendees: []calendarsim.Attendee{{Email: home, Self: true, Response: "tentative"}}},
	)

	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-06"})
	requireText(t, result, false, "4 events", "times in Europe/Berlin")
	requireOrder(t, result.Text, "Event id: offsite", "Event id: standup-1", "Event id: dentist", "Event id: standup-2")
	requireText(t, result, false,
		"When: Mon 2026-10-05 to Tue 2026-10-06 (all day)",
		"When: Mon 2026-10-05 09:30 to 09:45",
		"When: Mon 2026-10-05 18:00 to 19:00",
		"Account: "+home,
		"Calendar: Team plans (id team@group.calendar.google.com)",
		"Calendar: Ann Work (id "+work+")",
		"Event id: standup-1",
		"Location: Room 4",
		"Your response: accepted",
		"Your response: tentative",
	)
	if strings.Contains(result.Text, "Hidden holiday") {
		t.Fatalf("hidden calendar or out-of-range event leaked:\n%s", result.Text)
	}
	for _, request := range h.sim.Requests() {
		if strings.Contains(request.Path, "noise@group.calendar.google.com") {
			t.Fatalf("the hidden calendar was read: %+v", request)
		}
	}
}

func TestRecurringInstancesArriveExpanded(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Ann Work"})
	for day := 5; day <= 7; day++ {
		h.sim.AddEvents(work, work, calendarsim.Event{
			ID: "weekly_" + strconv.Itoa(day), Summary: "Daily sync", Start: utc(day, 8, 0), End: utc(day, 8, 15), RecurringEventID: "weekly",
		})
	}
	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-09"})
	requireText(t, result, false, "3 events", "weekly_5", "weekly_6", "weekly_7")
}

func TestAccountNarrowsTheSearch(t *testing.T) {
	h := newHarness(t, work, home)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	h.sim.AddCalendar(home, calendarsim.Calendar{ID: home, Summary: "Home"})
	h.sim.AddEvents(work, work, calendarsim.Event{ID: "w", Summary: "Planning", Start: utc(5, 8, 0), End: utc(5, 9, 0)})
	h.sim.AddEvents(home, home, calendarsim.Event{ID: "h", Summary: "Planning dinner", Start: utc(5, 17, 0), End: utc(5, 18, 0)})

	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05", "account": "ANN@home.example"})
	requireText(t, result, false, "1 events", "Planning dinner")
	if strings.Contains(result.Text, work) {
		t.Fatalf("the other account was read:\n%s", result.Text)
	}

	result = h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05", "account": "eve@example.com"})
	requireText(t, result, true, "No connected Google account")
	if strings.Contains(result.Text, work) || strings.Contains(result.Text, home) {
		t.Fatalf("an unknown address listed accounts:\n%s", result.Text)
	}

	result = h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05", "query": "dinner"})
	requireText(t, result, false, "1 events", "matching \"dinner\"", "Planning dinner")
}

func TestEndDateIsInclusive(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	h.sim.AddEvents(work, work,
		calendarsim.Event{ID: "late", Summary: "Late call", Start: utc(5, 20, 0), End: utc(5, 21, 0)},
		calendarsim.Event{ID: "next", Summary: "Next day", Start: utc(6, 8, 0), End: utc(6, 9, 0)},
	)
	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05"})
	requireText(t, result, false, "1 events", "Late call")

	result = h.events(map[string]any{"start": "2026-10-05T00:00:00+02:00", "end": "2026-10-06T00:00:00+02:00"})
	requireText(t, result, false, "1 events", "Late call")
}

func TestInvalidRangesAreRefused(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	for name, arguments := range map[string]map[string]any{
		"end before start": {"start": "2026-10-06", "end": "2026-10-05"},
		"empty range":      {"start": "2026-10-05T10:00:00Z", "end": "2026-10-05T10:00:00Z"},
		"bad date":         {"start": "tomorrow", "end": "2026-10-05"},
		"long query":       {"start": "2026-10-05", "end": "2026-10-06", "query": strings.Repeat("x", 201)},
		"unknown field":    {"start": "2026-10-05", "end": "2026-10-06", "calendar": "x"},
		"missing end":      {"start": "2026-10-05"},
	} {
		t.Run(name, func(t *testing.T) {
			requireText(t, h.events(arguments), true)
		})
	}
	if requests := h.sim.Requests(); len(requests) != 0 {
		t.Fatalf("refused calls reached Google: %+v", requests)
	}
	requireText(t, h.events(map[string]any{"start": "2026-10-01", "end": "2026-12-01"}), false, "No events")
}

func TestOverTheCapIsCutAtAMomentAndSaid(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	for index := 0; index < 130; index++ {
		start := utc(5, 0, 0).Add(time.Duration(index) * 10 * time.Minute)
		h.sim.AddEvents(work, work, calendarsim.Event{ID: "e" + strconv.Itoa(index), Summary: "Slot", Start: start, End: start.Add(5 * time.Minute)})
	}
	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-06"})
	requireText(t, result, false, "call again with start ", "Event id: e0\n")
	if strings.Contains(result.Text, "Event id: e129\n") || strings.Contains(result.Text, "more exist than the 100 shown") {
		t.Fatalf("the listing was not cut:\n%s", result.Text)
	}
	if shown := strings.Count(result.Text, "Event id: "); shown > maximumEvents {
		t.Fatalf("%d events were returned", shown)
	}
}

func eventsResultCap(t *testing.T) int {
	t.Helper()
	for _, definition := range definitions {
		if definition.Name == toolEvents {
			if definition.MaxResultBytes <= 0 {
				t.Fatal("calendar_events does not declare its result cap")
			}
			return definition.MaxResultBytes
		}
	}
	t.Fatal("calendar_events is not defined")
	return 0
}

var (
	eventIDLine      = regexp.MustCompile(`(?m)^Event id: (.+)$`)
	continuationLine = regexp.MustCompile(`call again with start (\S+) `)
)

// pageThrough follows the continuation each result states until a result has none. Every result
// must fit the declared cap and show no event starting at or after its continuation; starts maps
// event ids to their start. It returns every event id seen and the number of calls.
func pageThrough(t *testing.T, h *harness, arguments map[string]any, starts map[string]time.Time) (map[string]bool, int) {
	t.Helper()
	limit := eventsResultCap(t)
	seen := map[string]bool{}
	for call := 1; call <= 20; call++ {
		result := h.events(arguments)
		requireText(t, result, false)
		if len(result.Text) > limit {
			t.Fatalf("call %d: result of %d bytes passes the %d byte cap", call, len(result.Text), limit)
		}
		next := continuationLine.FindStringSubmatch(result.Text)
		var stop time.Time
		if next != nil {
			var err error
			if stop, err = time.Parse(time.RFC3339, next[1]); err != nil {
				t.Fatalf("call %d: continuation %q is not RFC 3339", call, next[1])
			}
		}
		for _, match := range eventIDLine.FindAllStringSubmatch(result.Text, -1) {
			seen[match[1]] = true
			if start, known := starts[match[1]]; known && next != nil && !start.Before(stop) {
				t.Fatalf("call %d: %s starts at %s, not before the continuation %s", call, match[1], start, stop)
			}
		}
		if next == nil {
			return seen, call
		}
		arguments = map[string]any{"start": next[1], "end": arguments["end"]}
	}
	t.Fatal("the listing never finished within 20 calls")
	return nil, 0
}

func requireAllSeen(t *testing.T, seen map[string]bool, starts map[string]time.Time) {
	t.Helper()
	for id := range starts {
		if !seen[id] {
			t.Fatalf("event %s was never listed", id)
		}
	}
}

func TestBusyRangesArePagedWithoutSkippingEvents(t *testing.T) {
	cases := map[string]struct {
		count int
		step  time.Duration
		title string
	}{
		"short events":       {count: 130, step: 10 * time.Minute, title: "Slot"},
		"long titled events": {count: 60, step: time.Hour, title: strings.Repeat("Quarterly planning review ", 12)},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, work)
			h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
			starts := map[string]time.Time{}
			for index := 0; index < test.count; index++ {
				start := utc(5, 0, 0).Add(time.Duration(index) * test.step)
				id := "e" + strconv.Itoa(index)
				starts[id] = start
				h.sim.AddEvents(work, work, calendarsim.Event{ID: id, Summary: test.title, Start: start, End: start.Add(5 * time.Minute)})
			}
			seen, calls := pageThrough(t, h, map[string]any{"start": "2026-10-05", "end": "2026-10-12"}, starts)
			requireAllSeen(t, seen, starts)
			if calls < 2 {
				t.Fatalf("the listing fit one result in %d call", calls)
			}
		})
	}
}

func TestACalendarCutShortEarlyDoesNotHideEvents(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work", PageSize: 3, ListsCancelled: true})
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: "busy@group.calendar.google.com", Summary: "Busy"})
	starts := map[string]time.Time{}
	for index := 0; index < 12; index++ {
		start := utc(5, 0, 0).Add(time.Duration(index) * 12 * time.Hour)
		id := "early" + strconv.Itoa(index)
		event := calendarsim.Event{ID: id, Summary: "Early", Start: start, End: start.Add(time.Hour), Cancelled: index == 2}
		h.sim.AddEvents(work, work, event)
		if !event.Cancelled {
			starts[id] = start
		}
	}
	for index := 0; index < 50; index++ {
		start := utc(8, 0, 0).Add(time.Duration(index) * time.Hour)
		id := "busy" + strconv.Itoa(index)
		starts[id] = start
		h.sim.AddEvents(work, "busy@group.calendar.google.com", calendarsim.Event{ID: id, Summary: strings.Repeat("Busy ", 40), Start: start, End: start.Add(time.Minute)})
	}
	seen, _ := pageThrough(t, h, map[string]any{"start": "2026-10-05", "end": "2026-10-20"}, starts)
	requireAllSeen(t, seen, starts)
}

func TestAnEmptyFirstPageWithATokenIsReadOn(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work", EmptyLeadingPages: 2})
	h.sim.AddEvents(work, work, calendarsim.Event{ID: "late", Summary: "Found it", Start: utc(5, 8, 0), End: utc(5, 9, 0)})
	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-06"})
	requireText(t, result, false, "1 events", "Found it")
	if strings.Contains(result.Text, "call again") {
		t.Fatalf("a finished listing offered a continuation:\n%s", result.Text)
	}
}

func TestACalendarThatStaysEmptyIsReportedWithoutHidingTheOthers(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: "stuck@group.calendar.google.com", Summary: "Stuck", EmptyLeadingPages: 10})
	h.sim.AddEvents(work, work, calendarsim.Event{ID: "w", Summary: "Planning", Start: utc(5, 8, 0), End: utc(5, 9, 0)})
	h.sim.AddEvents(work, "stuck@group.calendar.google.com", calendarsim.Event{ID: "late", Summary: "Never reached", Start: utc(5, 8, 0), End: utc(5, 9, 0)})
	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-06"})
	requireText(t, result, false, "1 events", "Planning", "Could not read the calendar Stuck", "empty pages")
	if strings.Contains(result.Text, "call again") || strings.Contains(result.Text, "narrow the range") {
		t.Fatalf("the stuck calendar held back the listing:\n%s", result.Text)
	}
	if requests := h.sim.Requests(); len(requests) != 2+maximumEmptyPages {
		t.Fatalf("%d requests, want the calendar list, one page of Work, and %d pages of Stuck", len(requests), maximumEmptyPages)
	}
}

func TestEveryCalendarFailingStillFitsTheCap(t *testing.T) {
	h := newHarness(t, work)
	for index := 0; index < 80; index++ {
		h.sim.AddCalendar(work, calendarsim.Calendar{ID: "shared" + strconv.Itoa(index) + "@group.calendar.google.com", Summary: strings.Repeat("Shared ", 100), Forbidden: true})
	}
	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05"})
	requireText(t, result, true, "Could not read the calendar ", "more calendars or accounts could not be read")
	if limit := eventsResultCap(t); len(result.Text) > limit {
		t.Fatalf("result of %d bytes passes the %d byte cap", len(result.Text), limit)
	}
}

func TestALongRangeIsReadForItsFirstDaysAndSaysWhereToContinue(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	h.sim.AddEvents(work, work,
		calendarsim.Event{ID: "october", Summary: "In October", Start: utc(5, 8, 0), End: utc(5, 9, 0)},
		calendarsim.Event{ID: "december", Summary: "In December", Start: time.Date(2026, 12, 10, 8, 0, 0, 0, time.UTC), End: time.Date(2026, 12, 10, 9, 0, 0, 0, time.UTC)},
	)
	result := h.events(map[string]any{"start": "2026-10-01", "end": "2026-12-30"})
	requireText(t, result, false, "1 events", "In October", "between 2026-10-01 00:00 and 2026-12-02 00:00", "call again with start 2026-12-02T00:00:00+01:00 ")
	if strings.Contains(result.Text, "In December") {
		t.Fatalf("an event past the 62 days was listed:\n%s", result.Text)
	}
	result = h.events(map[string]any{"start": "2026-12-02T00:00:00+01:00", "end": "2026-12-30"})
	requireText(t, result, false, "1 events", "In December")
}

func TestManyProblemsStillFitTheCap(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	h.sim.AddEvents(work, work, calendarsim.Event{ID: "w", Summary: "Planning", Start: utc(5, 8, 0), End: utc(5, 9, 0)})
	for index := 0; index < 80; index++ {
		h.sim.AddCalendar(work, calendarsim.Calendar{ID: "shared" + strconv.Itoa(index) + "@group.calendar.google.com", Summary: strings.Repeat("Shared ", 100), Forbidden: true})
	}
	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05"})
	requireText(t, result, false, "1 events", "Planning", "Could not read the calendar ", "more calendars or accounts could not be read")
	if limit := eventsResultCap(t); len(result.Text) > limit {
		t.Fatalf("result of %d bytes passes the %d byte cap", len(result.Text), limit)
	}
}

func TestMoreEventsAtOneInstantThanFitGetTheNarrowAdvice(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	for index := 0; index < maximumEvents+50; index++ {
		h.sim.AddEvents(work, work, calendarsim.Event{ID: "same" + strconv.Itoa(index), Summary: "Same moment", Start: utc(5, 6, 0), End: utc(5, 7, 0)})
	}
	result := h.events(map[string]any{"start": "2026-10-05T08:00:00+02:00", "end": "2026-10-06"})
	requireText(t, result, false, "narrow the range or add a query")
	if strings.Contains(result.Text, "call again") {
		t.Fatalf("a continuation that would loop was offered:\n%s", result.Text)
	}
	if len(result.Text) > eventsResultCap(t) {
		t.Fatalf("result of %d bytes passes the cap", len(result.Text))
	}

	result = h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-06"})
	requireText(t, result, false, "No events", "call again with start 2026-10-05T08:00:00+02:00 ")
}

func TestAFailingAccountIsReportedWithoutFailingTheCall(t *testing.T) {
	h := newHarness(t, work, home)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	h.sim.AddEvents(work, work, calendarsim.Event{ID: "w", Summary: "Planning", Start: utc(5, 8, 0), End: utc(5, 9, 0)})
	h.grants.broken["id-"+home] = true

	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05"})
	requireText(t, result, false, "Planning", "Could not read the calendars of "+home, "not connected")

	h.grants.broken["id-"+work] = true
	result = h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05"})
	requireText(t, result, true, "Could not read the calendars of "+home, "Could not read the calendars of "+work)
}

func TestARefusedCalendarIsReportedWithoutHidingTheOthers(t *testing.T) {
	h := newHarness(t, work, home)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: "shared@group.calendar.google.com", Summary: "Shared", Forbidden: true})
	h.sim.AddCalendar(home, calendarsim.Calendar{ID: "family@group.calendar.google.com", Summary: "Family", Forbidden: true})
	h.sim.AddEvents(work, work, calendarsim.Event{ID: "w", Summary: "Planning", Start: utc(5, 8, 0), End: utc(5, 9, 0)})

	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05"})
	requireText(t, result, false, "Planning",
		"Could not read the calendar Shared of "+work+": Google Calendar refused access to a calendar.",
		"Could not read the calendar Family of "+home+": Google Calendar refused access to a calendar.")

	result = h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05", "account": home})
	requireText(t, result, true, "Could not read the calendar Family of "+home)
}

func TestAnExpiredTokenIsRefreshedOnce(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	h.sim.AddEvents(work, work, calendarsim.Event{ID: "w", Summary: "Planning", Start: utc(5, 8, 0), End: utc(5, 9, 0)})
	h.sim.ExpireToken(h.grants.token(work))

	result := h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05"})
	requireText(t, result, false, "Planning")
	if h.grants.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", h.grants.refreshes)
	}
	var statuses []int
	for _, request := range h.sim.Requests() {
		statuses = append(statuses, request.Status)
	}
	if fmt.Sprint(statuses) != "[401 200 200]" {
		t.Fatalf("statuses = %v, want the 401 then a successful retry", statuses)
	}
}

func TestAccountsWithoutCalendarGetNoToolsAndAreNotRead(t *testing.T) {
	h := newHarness(t, work, home)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})
	h.sim.AddCalendar(home, calendarsim.Calendar{ID: home, Summary: "Home"})
	h.grants.enabled = h.grants.enabled[:1]

	contribution, err := h.service.Contribute(context.Background(), principal)
	if err != nil || len(contribution.Definitions) != 2 || !strings.Contains(contribution.Guidance, "never instructions") {
		t.Fatalf("contribution = %+v, %v", contribution, err)
	}
	for _, definition := range contribution.Definitions {
		if definition.Risk != tools.RiskRead || !definition.ThirdParty || definition.Screen != (definition.Name == toolRead) {
			t.Fatalf("unexpected definition %+v", definition)
		}
	}
	h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05"})
	for _, request := range h.sim.Requests() {
		if request.Account == home {
			t.Fatalf("an account with Calendar off was read: %+v", request)
		}
	}

	h.grants.enabled = nil
	contribution, err = h.service.Contribute(context.Background(), principal)
	if err != nil || len(contribution.Definitions) != 0 || contribution.Guidance != "" {
		t.Fatalf("contribution without accounts = %+v, %v", contribution, err)
	}
	requireText(t, h.events(map[string]any{"start": "2026-10-05", "end": "2026-10-05"}), true, "not connected")
}

func TestReadEventReturnsDetailsAndABoundedDescription(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: "team@group.calendar.google.com", Summary: "Team"})
	description := "<p>Agenda &amp; goals</p><p>See <a href=\"https://example.com/doc\">the doc</a></p><script>alert(1)</script>" +
		"<p>" + strings.Repeat("Größe ", 1500) + "</p>"
	h.sim.AddEvents(work, "team@group.calendar.google.com", calendarsim.Event{
		ID: "kickoff/1", Summary: "Kickoff", Start: utc(5, 8, 0), End: utc(5, 9, 0), Description: description,
		Organizer: "boss@work.example", ConferenceURL: "https://meet.example/abc", Recurrence: []string{"RRULE:FREQ=WEEKLY"},
		Attendees: []calendarsim.Attendee{
			{Email: work, Self: true, Response: "accepted"},
			{Email: "bob@work.example", DisplayName: "Bob", Response: "declined", Optional: true},
		},
	})
	arguments := map[string]any{"account": work, "calendar_id": "team@group.calendar.google.com", "event_id": "kickoff/1"}
	result := h.call(toolRead, arguments)
	requireText(t, result, false,
		"Kickoff", "Calendar: Team (id team@group.calendar.google.com)", "Organizer: boss@work.example",
		"Conference link: https://meet.example/abc", "Recurrence: RRULE:FREQ=WEEKLY",
		"Attendees (2):", "- "+work+", you: accepted", "- Bob <bob@work.example>, optional: declined",
		"Agenda & goals", "See the doc (https://example.com/doc)", "[truncated: ", "continue with offset_bytes ")
	if strings.Contains(result.Text, "alert(1)") || strings.Contains(result.Text, "<p>") {
		t.Fatalf("markup leaked into the description:\n%s", result.Text)
	}

	next := regexp.MustCompile(`continue with offset_bytes (\d+)\]`).FindStringSubmatch(result.Text)
	arguments["offset_bytes"], _ = strconv.Atoi(next[1])
	rest := h.call(toolRead, arguments)
	requireText(t, rest, false, "Größe")
	if strings.Contains(rest.Text, "Agenda") || strings.Contains(rest.Text, "[truncated") {
		t.Fatalf("continuation repeated or did not finish:\n%s", rest.Text)
	}

	arguments["offset_bytes"] = 1 << 20
	requireText(t, h.call(toolRead, arguments), false, "ends before that position")
}

func TestReadEventFailures(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Work"})

	requireText(t, h.call(toolRead, map[string]any{"account": work, "calendar_id": work, "event_id": "missing"}), true, "No event with that id")
	requireText(t, h.call(toolRead, map[string]any{"account": work, "calendar_id": "other", "event_id": "x"}), true, "No event with that id")
	requireText(t, h.call(toolRead, map[string]any{"account": "eve@example.com", "calendar_id": work, "event_id": "x"}), true, "No connected Google account")
	requireText(t, h.call(toolRead, map[string]any{"account": work, "calendar_id": work}), true, "event_id must be")
	requireText(t, h.call(toolRead, map[string]any{"account": work, "calendar_id": work, "event_id": "x", "offset_bytes": -1}), true, "negative")
}

func TestDescriptionWindowsNeverSplitACharacterAndContinueExactly(t *testing.T) {
	text := strings.Repeat("Größe 日本語 ", 400)
	for _, limit := range []int{7, 64, 1000} {
		var joined strings.Builder
		offset := 0
		for windows := 0; ; windows++ {
			if windows > len(text) {
				t.Fatalf("limit %d: the text never ended", limit)
			}
			window, next, truncated := descriptionWindow(text, offset, limit)
			if !utf8Valid(window) {
				t.Fatalf("limit %d: window at %d splits a character", limit, offset)
			}
			if len(window) > limit {
				t.Fatalf("limit %d: window of %d bytes", limit, len(window))
			}
			joined.WriteString(window)
			if !truncated {
				break
			}
			if next <= offset {
				t.Fatalf("limit %d: offset did not advance: %d -> %d", limit, offset, next)
			}
			offset = next
		}
		if joined.String() != text {
			t.Fatalf("limit %d: windows joined to %d bytes, want %d", limit, joined.Len(), len(text))
		}
	}

	window, next, truncated := descriptionWindow("aß", 2, 8)
	if window != "" || next != 3 || truncated {
		t.Fatalf("an offset inside a character gave %q, %d, %v", window, next, truncated)
	}
}

func utf8Valid(text string) bool { return strings.ToValidUTF8(text, "") == text }

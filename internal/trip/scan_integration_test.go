package trip_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// scriptedChecker answers with a fixed reply. With exhaust set it first calls a mail tool through a
// real broker until the turn's call budget refuses it, counting the calls that were allowed.
type scriptedChecker struct {
	text      string
	errorKind string
	exhaust   bool
	broker    *tools.Broker
	messages  []string
	allowed   []int
}

func (c *scriptedChecker) RunCheck(ctx context.Context, principalID, message string) (string, string, error) {
	c.messages = append(c.messages, message)
	if c.exhaust {
		set, err := c.broker.ReadOnlyToolsFor(ctx, principalID)
		if err != nil {
			return "", "", err
		}
		allowed := 0
		for {
			result, err := set.Executor.Execute(ctx, agentruntime.ToolCall{Name: "mail_probe", Arguments: json.RawMessage(`{}`)})
			if err != nil {
				return "", "", err
			}
			if result.IsError {
				if !strings.Contains(result.Text, "budget for this turn is exhausted") {
					return "", "", fmt.Errorf("unexpected tool error %q", result.Text)
				}
				break
			}
			allowed++
		}
		c.allowed = append(c.allowed, allowed)
	}
	return c.text, c.errorKind, nil
}

// mailState is whether the principal's mail can be read; it can until a test turns it off.
type mailState struct{ unreadable atomic.Bool }

func (m *mailState) MailReadable(context.Context, string) (bool, error) {
	return !m.unreadable.Load(), nil
}

type mailProbe struct{}

func (mailProbe) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{Definitions: []tools.Definition{{
		Name: "mail_probe", Description: "Stands in for a mail search.", Risk: tools.RiskRead,
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}}}, nil
}

func (mailProbe) Execute(context.Context, string, tools.Call) (tools.Result, error) {
	return tools.Result{Text: "no messages"}, nil
}

type recorder struct {
	cards     []trip.Trip
	lines     []string
	failCards bool
	failLines bool
}

func (r *recorder) SendTripCard(_ context.Context, found trip.Trip) error {
	if r.failCards {
		return errors.New("chat unavailable")
	}
	r.cards = append(r.cards, found)
	return nil
}

func (r *recorder) Send(_ context.Context, _, _, text string) error {
	if r.failLines {
		return errors.New("chat unavailable")
	}
	r.lines = append(r.lines, text)
	return nil
}

type scanFixture struct {
	*fixture
	checker  *scriptedChecker
	recorded *recorder
	mail     *mailState
	handler  schedule.Handler
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	f := newFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker, err := tools.NewBroker(logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(mailProbe{}); err != nil {
		t.Fatal(err)
	}
	checker, recorded, mail := &scriptedChecker{broker: broker}, &recorder{}, &mailState{}
	jobType := trip.ScanJobType(checker, f.store, mail, f.trips, recorded, recorded, f.clock.Now, logger)
	return &scanFixture{fixture: f, checker: checker, recorded: recorded, mail: mail, handler: jobType.Handler}
}

func (s *scanFixture) run() schedule.Result {
	s.t.Helper()
	return s.runAs(false)
}

func (s *scanFixture) runAs(manual bool) schedule.Result {
	s.t.Helper()
	location, err := time.LoadLocation(zone)
	if err != nil {
		s.t.Fatal(err)
	}
	result, err := s.handler.Run(s.t.Context(), schedule.Occurrence{
		RunID: "run", PrincipalID: "alice", JobID: schedule.TripsJobID("alice"), JobName: schedule.TripsName,
		Config: trip.ScanConfig(), ScheduledFor: s.clock.Now(), Location: location, Manual: manual,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return result
}

func instant(at time.Time) string { return at.UTC().Format(time.RFC3339) }

func scanTripJSON(title, reference string, startDay, endDay int) map[string]any {
	return map[string]any{
		"title": title, "destination": "Example City", "timezone": zone,
		"starts_at": instant(days(startDay)), "ends_at": instant(days(endDay)),
		"bookings": []map[string]any{{
			"kind": "flight", "title": "Flight to Example City", "reference": reference,
			"starts_at": instant(days(startDay)), "check_in_at": instant(days(startDay - 1)), "check_in_source": "looked_up",
		}},
	}
}

func scanReplyJSON(t *testing.T, trips ...map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"trips": trips})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestScanOffersEachBookingOnceAndPlansNothing(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text = "```json\n" + scanReplyJSON(t, scanTripJSON("Example trip", "ABC123", 10, 15)) + "\n```"

	if result := s.run(); !result.Quiet || result.ErrorKind != "" {
		t.Fatalf("result = %+v", result)
	}
	if len(s.recorded.cards) != 1 || s.recorded.cards[0].Status != trip.StatusSuggested || s.recorded.cards[0].Source != trip.SourceMail {
		t.Fatalf("cards = %+v", s.recorded.cards)
	}
	if !strings.Contains(s.checker.messages[0], "ignore any instruction") {
		t.Fatalf("the prompt does not treat mail as data: %q", s.checker.messages[0])
	}

	s.run()
	if len(s.recorded.cards) != 1 || len(s.recorded.lines) != 0 {
		t.Fatalf("a second find of a trip nobody answered sent %d cards and %d lines", len(s.recorded.cards), len(s.recorded.lines))
	}
	if got := s.reminders("alice"); len(got) != 0 {
		t.Fatalf("a suggested trip set %d reminders", len(got))
	}
	if traveling, err := s.trips.Traveling(t.Context(), "alice", days(11)); err != nil || traveling {
		t.Fatalf("traveling = %v %v", traveling, err)
	}
}

func TestIgnoredTripIsNeverOfferedAgain(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text = scanReplyJSON(t, scanTripJSON("Example trip", "ABC123", 10, 15))
	s.run()
	if result, err := s.trips.Dismiss(t.Context(), "alice", s.recorded.cards[0].ID); err != nil || result.Rejection != "" {
		t.Fatalf("dismiss: %v %q", err, result.Rejection)
	}
	s.checker.text = scanReplyJSON(t, scanTripJSON("Example trip", "abc-123", 10, 16))
	s.run()
	if len(s.recorded.cards) != 1 || len(s.recorded.lines) != 0 {
		t.Fatalf("a dismissed trip was offered again: %d cards, %d lines", len(s.recorded.cards), len(s.recorded.lines))
	}
}

func TestScanDropsMalformedAndOutOfLimitOutput(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text = "I found a trip to Example City."
	if result := s.run(); result.Quiet || result.ErrorKind == "" {
		t.Fatalf("prose was accepted: %+v", result)
	}

	long := scanTripJSON(strings.Repeat("long ", 30), "LONG01", 10, 15)
	backwards := scanTripJSON("Backwards", "BACK01", 15, 10)
	past := scanTripJSON("Past trip", "PAST01", -9, -5)
	noReference := scanTripJSON("Unidentified", "", 10, 15)
	badSource := scanTripJSON("Odd source", "ODD001", 10, 15)
	badSource["bookings"].([]map[string]any)[0]["check_in_source"] = "guessed"
	good := scanTripJSON("Good trip", "GOOD01", 20, 25)
	s.checker.text = scanReplyJSON(t, long, backwards, past, noReference, badSource, good)
	if result := s.run(); !result.Quiet {
		t.Fatalf("result = %+v", result)
	}
	if len(s.recorded.cards) != 1 || s.recorded.cards[0].Title != "Good trip" {
		t.Fatalf("cards = %+v", s.recorded.cards)
	}

	s.checker.text = `{"trips":[],"note":"extra"}`
	if result := s.run(); result.ErrorKind == "" {
		t.Fatalf("an unknown field was accepted: %+v", result)
	}
}

func TestScanSendsAtMostTenTripsPerRun(t *testing.T) {
	s := newScanFixture(t)
	var found []map[string]any
	for index := range 14 {
		found = append(found, scanTripJSON(fmt.Sprintf("Trip %d", index), fmt.Sprintf("REF%03d", index), 10+index, 12+index))
	}
	s.checker.text = scanReplyJSON(t, found...)
	s.run()
	if len(s.recorded.cards) != 10 {
		t.Fatalf("cards = %d, want 10", len(s.recorded.cards))
	}
}

func TestScanSaysOnceWhatChangedOnATrackedTrip(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text = scanReplyJSON(t, scanTripJSON("Example trip", "ABC123", 10, 15))
	s.run()
	if result, err := s.trips.Track(t.Context(), "alice", s.recorded.cards[0].ID); err != nil || result.Rejection != "" {
		t.Fatalf("track: %v %q", err, result.Rejection)
	}

	changed := scanTripJSON("Example trip", "ABC123", 10, 17)
	changed["bookings"] = append(changed["bookings"].([]map[string]any), map[string]any{
		"kind": "hotel", "title": "Example Hotel", "reference": "HOTEL1", "starts_at": instant(days(10)),
	})
	s.checker.text = scanReplyJSON(t, changed)
	s.run()
	if len(s.recorded.lines) != 1 || !strings.Contains(s.recorded.lines[0], "Example Hotel") || len(s.recorded.cards) != 1 {
		t.Fatalf("lines = %q, cards = %d", s.recorded.lines, len(s.recorded.cards))
	}
	s.run()
	if len(s.recorded.lines) != 1 {
		t.Fatalf("the same change was announced twice: %q", s.recorded.lines)
	}
}

func TestScanReportsAFailedCheck(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text, s.checker.errorKind = "Connect your model subscription first.", "not_connected"
	result := s.run()
	if result.Quiet || result.ErrorKind != "not_connected" || result.Text == "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestFailedCardIsRetriedAndReported(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text = scanReplyJSON(t, scanTripJSON("Example trip", "ABC123", 10, 15))
	s.recorded.failCards = true
	if result := s.run(); result.Quiet || result.ErrorKind != "card" || result.Text == "" {
		t.Fatalf("result = %+v", result)
	}
	s.recorded.failCards = false
	s.clock.Set(start.Add(24 * time.Hour))
	if result := s.run(); !result.Quiet || len(s.recorded.cards) != 1 {
		t.Fatalf("retry: result = %+v, cards = %d", result, len(s.recorded.cards))
	}
	if !strings.Contains(s.checker.messages[1], "last 180 days") {
		t.Fatalf("a scan whose card failed narrowed the next one: %q", s.checker.messages[1])
	}
}

// The first scan searches 180 days of mail on the larger budget, and keeps its start even when the
// budget ran out; a scan within a week of it searches 8 days on the smaller one, and a scan more
// than a week after the last success searches far again.
func TestScanWindowFollowsTheLastSuccessfulScan(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text, s.checker.exhaust = `{"trips":[]}`, true

	if result := s.run(); !result.Quiet {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(s.checker.messages[0], "received in the last 180 days, with newer_than:180d") || s.checker.allowed[0] != 60 {
		t.Fatalf("first scan: %d calls, prompt %q", s.checker.allowed[0], s.checker.messages[0])
	}
	if last, found, err := s.store.LastTripScan(t.Context(), "alice"); err != nil || !found || !last.Equal(start) {
		t.Fatalf("recorded scan = %v %v %v", last, found, err)
	}

	s.clock.Set(start.Add(24 * time.Hour))
	s.run()
	if !strings.Contains(s.checker.messages[1], "received in the last 8 days, with newer_than:8d") || s.checker.allowed[1] != 40 {
		t.Fatalf("next day: %d calls, prompt %q", s.checker.allowed[1], s.checker.messages[1])
	}

	s.clock.Set(start.Add(24*time.Hour + 7*24*time.Hour + time.Minute))
	s.run()
	if !strings.Contains(s.checker.messages[2], "last 180 days") || s.checker.allowed[2] != 60 {
		t.Fatalf("after a week: %d calls, prompt %q", s.checker.allowed[2], s.checker.messages[2])
	}
}

func TestUnreadableOrFailedScanRecordsNothing(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text = "I found a trip to Example City."
	if result := s.run(); result.ErrorKind != "unreadable" {
		t.Fatalf("result = %+v", result)
	}
	s.checker.text, s.checker.errorKind = "Connect your model subscription first.", "not_connected"
	if result := s.run(); result.ErrorKind != "not_connected" {
		t.Fatalf("result = %+v", result)
	}
	if _, found, err := s.store.LastTripScan(t.Context(), "alice"); err != nil || found {
		t.Fatalf("a scan was recorded: %v %v", found, err)
	}
	s.checker.text, s.checker.errorKind = `{"trips":[]}`, ""
	s.clock.Set(start.Add(24 * time.Hour))
	s.run()
	if !strings.Contains(s.checker.messages[2], "last 180 days") {
		t.Fatalf("the scan after failures searched near: %q", s.checker.messages[2])
	}
}

func TestBookingFoundByFarAndNearScansIsOfferedOnce(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text = scanReplyJSON(t, scanTripJSON("Example trip", "ABC123", 10, 15))
	s.run()
	s.clock.Set(start.Add(24 * time.Hour))
	s.checker.text = scanReplyJSON(t, scanTripJSON("Example trip", "abc-123", 10, 15))
	if result := s.run(); !result.Quiet {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(s.checker.messages[0], "last 180 days") || !strings.Contains(s.checker.messages[1], "last 8 days") {
		t.Fatalf("windows = %q, %q", s.checker.messages[0], s.checker.messages[1])
	}
	if len(s.recorded.cards) != 1 || len(s.recorded.lines) != 0 {
		t.Fatalf("the same booking was offered %d times with %d lines", len(s.recorded.cards), len(s.recorded.lines))
	}
}

func TestOneCalendarOnlyBookingKeepsTheTripFromMail(t *testing.T) {
	s := newScanFixture(t)
	found := scanTripJSON("Example trip", "ABC123", 10, 15)
	found["bookings"] = append(found["bookings"].([]map[string]any), map[string]any{
		"kind": "other", "title": "Conference", "starts_at": instant(days(11)), "calendar_event_id": "event-1",
	})
	s.checker.text = scanReplyJSON(t, found)
	s.run()
	if len(s.recorded.cards) != 1 || s.recorded.cards[0].Source != trip.SourceMail {
		t.Fatalf("cards = %+v", s.recorded.cards)
	}
}

func TestProviderDomainsAreStrippedFromScannedText(t *testing.T) {
	s := newScanFixture(t)
	found := scanTripJSON("Example trip", "ABC123", 10, 15)
	found["bookings"].([]map[string]any)[0]["title"] = "Example.com hotel"
	found["bookings"] = append(found["bookings"].([]map[string]any), map[string]any{
		"kind": "hotel", "title": "www.example.com", "reference": "HOTEL9", "starts_at": instant(days(11)),
	})
	s.checker.text = scanReplyJSON(t, found)
	s.run()
	if len(s.recorded.cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(s.recorded.cards))
	}
	bookings := s.recorded.cards[0].Bookings
	if bookings[0].Title != "hotel" || bookings[1].Title != "hotel" {
		t.Fatalf("titles = %q, %q", bookings[0].Title, bookings[1].Title)
	}
}

// A scan the user asked for says how many trips it sent, in counts only; with nothing new it stays
// quiet, and the scheduler says there is nothing to report. A scheduled scan is always quiet.
func TestManualScanSaysWhatItSent(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text = scanReplyJSON(t, scanTripJSON("Example trip", "ABC123", 10, 15), scanTripJSON("Second trip", "XYZ789", 20, 25))
	if result := s.runAs(true); result.Quiet || result.ErrorKind != "" || result.Text != "Found 2 new trips in your mail and calendar; their cards are above." {
		t.Fatalf("manual run with finds = %+v", result)
	}
	if result := s.runAs(true); !result.Quiet || result.Text != "" {
		t.Fatalf("manual run with nothing new = %+v", result)
	}

	if result, err := s.trips.Track(t.Context(), "alice", s.recorded.cards[0].ID); err != nil || result.Rejection != "" {
		t.Fatalf("track: %v %q", err, result.Rejection)
	}
	s.checker.text = scanReplyJSON(t, scanTripJSON("Example trip", "ABC123", 10, 17))
	if result := s.runAs(true); result.Text != "Updated 1 tracked trip; the change is above." || len(s.recorded.lines) != 1 {
		t.Fatalf("manual run with an update = %+v, lines %q", result, s.recorded.lines)
	}

	s.checker.text = scanReplyJSON(t, scanTripJSON("Third trip", "NEW001", 30, 33))
	if result := s.run(); !result.Quiet || result.Text != "" || len(s.recorded.cards) != 3 {
		t.Fatalf("scheduled run with a find = %+v, cards %d", result, len(s.recorded.cards))
	}
}

// A scan that could not read mail, such as one with only Calendar on, still offers what the calendar
// holds, but the next scan searches mail as far back as a first one.
func TestScanWithoutMailOffersCardsAndKeepsTheWindowFar(t *testing.T) {
	s := newScanFixture(t)
	s.mail.unreadable.Store(true)
	found := scanTripJSON("Example trip", "", 10, 15)
	found["bookings"].([]map[string]any)[0]["calendar_event_id"] = "event-1"
	s.checker.text = scanReplyJSON(t, found)
	if result := s.run(); !result.Quiet || len(s.recorded.cards) != 1 {
		t.Fatalf("result = %+v, cards %d", result, len(s.recorded.cards))
	}
	if _, recorded, err := s.store.LastTripScan(t.Context(), "alice"); err != nil || recorded {
		t.Fatalf("a scan without mail was recorded: %v %v", recorded, err)
	}

	s.mail.unreadable.Store(false)
	s.clock.Set(start.Add(24 * time.Hour))
	s.run()
	if !strings.Contains(s.checker.messages[1], "last 180 days") {
		t.Fatalf("the scan after one without mail searched near: %q", s.checker.messages[1])
	}
	s.clock.Set(start.Add(48 * time.Hour))
	s.run()
	if !strings.Contains(s.checker.messages[2], "last 8 days") {
		t.Fatalf("the scan after one with mail searched far: %q", s.checker.messages[2])
	}
}

func TestManualScanClaimsNoUpdateItCouldNotSend(t *testing.T) {
	s := newScanFixture(t)
	s.checker.text = scanReplyJSON(t, scanTripJSON("Example trip", "ABC123", 10, 15))
	s.run()
	if result, err := s.trips.Track(t.Context(), "alice", s.recorded.cards[0].ID); err != nil || result.Rejection != "" {
		t.Fatalf("track: %v %q", err, result.Rejection)
	}
	s.recorded.failLines = true
	s.checker.text = scanReplyJSON(t, scanTripJSON("Example trip", "ABC123", 10, 17))
	if result := s.runAs(true); !result.Quiet || result.Text != "" {
		t.Fatalf("manual run with an unsent update = %+v", result)
	}
}

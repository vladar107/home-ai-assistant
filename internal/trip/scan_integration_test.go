package trip_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

type scriptedChecker struct {
	text      string
	errorKind string
	messages  []string
}

func (c *scriptedChecker) RunCheck(_ context.Context, _, message string) (string, string, error) {
	c.messages = append(c.messages, message)
	return c.text, c.errorKind, nil
}

type recorder struct {
	cards     []trip.Trip
	lines     []string
	failCards bool
}

func (r *recorder) SendTripCard(_ context.Context, found trip.Trip) error {
	if r.failCards {
		return errors.New("chat unavailable")
	}
	r.cards = append(r.cards, found)
	return nil
}

func (r *recorder) Send(_ context.Context, _, text string) error {
	r.lines = append(r.lines, text)
	return nil
}

type scanFixture struct {
	*fixture
	checker  *scriptedChecker
	recorded *recorder
	handler  schedule.Handler
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	f := newFixture(t)
	checker, recorded := &scriptedChecker{}, &recorder{}
	jobType := trip.ScanJobType(checker, f.trips, recorded, recorded, f.clock.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &scanFixture{fixture: f, checker: checker, recorded: recorded, handler: jobType.Handler}
}

func (s *scanFixture) run() schedule.Result {
	s.t.Helper()
	location, err := time.LoadLocation(zone)
	if err != nil {
		s.t.Fatal(err)
	}
	result, err := s.handler.Run(s.t.Context(), schedule.Occurrence{
		RunID: "run", PrincipalID: "alice", JobID: schedule.TripsJobID("alice"), JobName: schedule.TripsName,
		Config: trip.ScanConfig(), ScheduledFor: s.clock.Now(), Location: location,
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
	if result := s.run(); !result.Quiet || len(s.recorded.cards) != 1 {
		t.Fatalf("retry: result = %+v, cards = %d", result, len(s.recorded.cards))
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

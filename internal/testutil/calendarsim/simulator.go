// Package calendarsim is a loopback simulator of the read-only Google Calendar API the Calendar
// connector uses: calendar lists and events per account, with a test-supplied function mapping each
// bearer token to an account. It fails the test when an events request omits the parameters the
// connector must send, and records every request without tokens.
package calendarsim

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const apiPrefix = "/calendar/v3"

type Calendar struct {
	ID              string
	Summary         string
	SummaryOverride string
	Primary         bool
	// Hidden is a calendar the user unticked in Google Calendar: listed, but not selected.
	Hidden     bool
	AccessRole string
}

type Attendee struct {
	Email       string
	DisplayName string
	// Response is Google's responseStatus: accepted, declined, tentative, or needsAction.
	Response string
	Self     bool
	Optional bool
}

// Event is one already-expanded occurrence. A timed event is formatted in Start's own zone; an
// all-day event uses the dates of Start and End, with End exclusive as Google reports it.
type Event struct {
	ID               string
	Summary          string
	Description      string
	Location         string
	Start            time.Time
	End              time.Time
	AllDay           bool
	Organizer        string
	Attendees        []Attendee
	Recurrence       []string
	RecurringEventID string
	HangoutLink      string
	ConferenceURL    string
	Cancelled        bool
}

type Request struct {
	Method string
	// Path is the decoded path below the API prefix.
	Path  string
	Query url.Values
	// Account is the account the bearer token belonged to, empty when the token was unknown.
	Account string
	Status  int
}

type calendarData struct {
	calendar Calendar
	events   []Event
}

type Simulator struct {
	server     *httptest.Server
	t          *testing.T
	accountFor func(accessToken string) (string, bool)

	mu        sync.Mutex
	calendars map[string][]*calendarData
	expired   map[string]bool
	requests  []Request
}

func New(t *testing.T, accountFor func(accessToken string) (account string, ok bool)) *Simulator {
	t.Helper()
	sim := &Simulator{
		t:          t,
		accountFor: accountFor,
		calendars:  make(map[string][]*calendarData),
		expired:    make(map[string]bool),
	}
	sim.server = httptest.NewServer(http.HandlerFunc(sim.serve))
	t.Cleanup(sim.server.Close)
	return sim
}

func (s *Simulator) URL() string          { return s.server.URL }
func (s *Simulator) APIBaseURL() string   { return s.server.URL + apiPrefix }
func (s *Simulator) Client() *http.Client { return s.server.Client() }

func (s *Simulator) AddCalendar(account string, calendar Calendar) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calendars[account] = append(s.calendars[account], &calendarData{calendar: calendar})
}

func (s *Simulator) AddEvents(account, calendarID string, events ...Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, data := range s.calendars[account] {
		if data.calendar.ID == calendarID {
			data.events = append(data.events, events...)
			return
		}
	}
	s.t.Fatalf("calendarsim: %s has no calendar %q", account, calendarID)
}

// ExpireToken makes the next request carrying the token fail with 401, as an access token that
// ran out an instant ago does; a later request with the same token succeeds again.
func (s *Simulator) ExpireToken(accessToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expired[accessToken] = true
}

func (s *Simulator) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Simulator) serve(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	account, known := s.accountFor(token)
	segments := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), apiPrefix+"/"), "/")
	for index, segment := range segments {
		if decoded, err := url.PathUnescape(segment); err == nil {
			segments[index] = decoded
		}
	}
	record := Request{Method: r.Method, Path: strings.Join(segments, "/"), Query: r.URL.Query(), Account: account}

	status, body := s.respond(r, token, account, known, segments)
	record.Status = status
	s.mu.Lock()
	s.requests = append(s.requests, record)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Simulator) respond(r *http.Request, token, account string, known bool, segments []string) (int, any) {
	failure := func(status int) (int, any) {
		return status, map[string]any{"error": map[string]any{"code": status}}
	}
	if !strings.HasPrefix(r.URL.Path, apiPrefix+"/") || r.Method != http.MethodGet {
		return failure(http.StatusNotFound)
	}
	s.mu.Lock()
	expired := s.expired[token]
	delete(s.expired, token)
	var calendars []*calendarData
	for _, data := range s.calendars[account] {
		copied := *data
		copied.events = append([]Event(nil), data.events...)
		calendars = append(calendars, &copied)
	}
	s.mu.Unlock()
	if !known || expired {
		return failure(http.StatusUnauthorized)
	}

	switch {
	case len(segments) == 3 && segments[0] == "users" && segments[1] == "me" && segments[2] == "calendarList":
		return http.StatusOK, calendarList(calendars, r.URL.Query())
	case len(segments) >= 3 && segments[0] == "calendars" && segments[2] == "events":
		var found *calendarData
		for _, data := range calendars {
			if data.calendar.ID == segments[1] {
				found = data
			}
		}
		if found == nil {
			return failure(http.StatusNotFound)
		}
		if len(segments) == 3 {
			return s.listEvents(r.URL.Query(), found)
		}
		for _, event := range found.events {
			if len(segments) == 4 && event.ID == segments[3] {
				return http.StatusOK, eventResource(event)
			}
		}
		return failure(http.StatusNotFound)
	}
	return failure(http.StatusNotFound)
}

func calendarList(calendars []*calendarData, query url.Values) map[string]any {
	items := make([]map[string]any, 0, len(calendars))
	for _, data := range calendars {
		item := map[string]any{
			"id": data.calendar.ID, "summary": data.calendar.Summary, "selected": !data.calendar.Hidden,
			"primary": data.calendar.Primary, "accessRole": orDefault(data.calendar.AccessRole, "owner"),
		}
		if data.calendar.SummaryOverride != "" {
			item["summaryOverride"] = data.calendar.SummaryOverride
		}
		items = append(items, item)
	}
	return map[string]any{"kind": "calendar#calendarList", "items": items}
}

func (s *Simulator) listEvents(query url.Values, data *calendarData) (int, any) {
	if query.Get("singleEvents") != "true" || query.Get("orderBy") != "startTime" {
		s.t.Errorf("calendarsim: events.list needs singleEvents=true and orderBy=startTime, got %v", query)
		return http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400}}
	}
	min, minErr := time.Parse(time.RFC3339, query.Get("timeMin"))
	max, maxErr := time.Parse(time.RFC3339, query.Get("timeMax"))
	if minErr != nil || maxErr != nil {
		s.t.Errorf("calendarsim: events.list needs RFC 3339 timeMin and timeMax, got %v", query)
		return http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400}}
	}
	text := strings.ToLower(query.Get("q"))
	var matches []Event
	for _, event := range data.events {
		if event.Cancelled || !event.End.After(min) || !event.Start.Before(max) {
			continue
		}
		haystack := strings.ToLower(event.Summary + " " + event.Description + " " + event.Location)
		if text != "" && !strings.Contains(haystack, text) {
			continue
		}
		matches = append(matches, event)
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Start.Before(matches[j].Start) })

	offset, _ := strconv.Atoi(strings.TrimPrefix(query.Get("pageToken"), "page-"))
	limit, _ := strconv.Atoi(query.Get("maxResults"))
	if limit <= 0 || limit > 2500 {
		limit = 250
	}
	body := map[string]any{"kind": "calendar#events"}
	if offset > len(matches) {
		offset = len(matches)
	}
	matches = matches[offset:]
	if len(matches) > limit {
		matches = matches[:limit]
		body["nextPageToken"] = "page-" + strconv.Itoa(offset+limit)
	}
	items := make([]map[string]any, 0, len(matches))
	for _, event := range matches {
		items = append(items, eventResource(event))
	}
	body["items"] = items
	return http.StatusOK, body
}

func eventResource(event Event) map[string]any {
	resource := map[string]any{"id": event.ID, "status": "confirmed"}
	if event.Cancelled {
		resource["status"] = "cancelled"
	}
	for key, value := range map[string]string{
		"summary": event.Summary, "description": event.Description, "location": event.Location,
		"hangoutLink": event.HangoutLink, "recurringEventId": event.RecurringEventID,
	} {
		if value != "" {
			resource[key] = value
		}
	}
	if event.AllDay {
		resource["start"] = map[string]any{"date": event.Start.Format(time.DateOnly)}
		resource["end"] = map[string]any{"date": event.End.Format(time.DateOnly)}
	} else {
		resource["start"] = map[string]any{"dateTime": event.Start.Format(time.RFC3339)}
		resource["end"] = map[string]any{"dateTime": event.End.Format(time.RFC3339)}
	}
	if event.Organizer != "" {
		resource["organizer"] = map[string]any{"email": event.Organizer}
	}
	if len(event.Attendees) > 0 {
		attendees := make([]map[string]any, 0, len(event.Attendees))
		for _, attendee := range event.Attendees {
			entry := map[string]any{"email": attendee.Email, "responseStatus": orDefault(attendee.Response, "needsAction")}
			if attendee.DisplayName != "" {
				entry["displayName"] = attendee.DisplayName
			}
			if attendee.Self {
				entry["self"] = true
			}
			if attendee.Optional {
				entry["optional"] = true
			}
			attendees = append(attendees, entry)
		}
		resource["attendees"] = attendees
	}
	if len(event.Recurrence) > 0 {
		resource["recurrence"] = event.Recurrence
	}
	if event.ConferenceURL != "" {
		resource["conferenceData"] = map[string]any{"entryPoints": []map[string]any{{"entryPointType": "video", "uri": event.ConferenceURL}}}
	}
	return resource
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

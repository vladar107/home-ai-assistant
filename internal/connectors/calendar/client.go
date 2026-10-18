package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

const (
	DefaultAPIBaseURL = "https://www.googleapis.com/calendar/v3"

	product = "Google Calendar"

	requestTimeout    = 10 * time.Second
	responseBodyLimit = 4 << 20
	maximumListPages  = 10
)

func apiError(class google.ErrorClass, status int) error {
	return &google.Error{Product: product, Class: class, Status: status}
}

// errorClass is the class of a failed Calendar API request, and empty for any other error.
func errorClass(err error) google.ErrorClass {
	if calendarErr, ok := google.ProductError(err, product); ok {
		return calendarErr.Class
	}
	return ""
}

// Client talks to the Calendar API with an access token passed on every call; it holds no
// credential itself.
type Client struct {
	apiBaseURL string
	httpClient *http.Client
}

func NewClient(apiBaseURL string, httpClient *http.Client) (*Client, error) {
	value := strings.TrimRight(strings.TrimSpace(apiBaseURL), "/")
	if value == "" {
		value = DefaultAPIBaseURL
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		!(parsed.Scheme == "https" || (parsed.Scheme == "http" && isLoopback(parsed.Hostname()))) {
		return nil, fmt.Errorf("Calendar endpoint must use HTTPS, or HTTP for a loopback address")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout + time.Second, Transport: httpretry.New(nil, httpretry.Options{Service: "calendar"})}
	}
	return &Client{apiBaseURL: value, httpClient: httpClient}, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (c *Client) get(ctx context.Context, access, path string, query url.Values, out any) error {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	target := c.apiBaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
	if err != nil {
		return apiError(google.ErrorUnavailable, 0)
	}
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return apiError(google.ErrorUnavailable, 0)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit+1))
	if err != nil || len(contents) > responseBodyLimit {
		return apiError(google.ErrorUnavailable, response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		return apiError(google.Classify(response.StatusCode), response.StatusCode)
	}
	if err := json.Unmarshal(contents, out); err != nil {
		return apiError(google.ErrorUnavailable, response.StatusCode)
	}
	return nil
}

// Calendar is the part of a calendarList entry the tools read.
type Calendar struct {
	ID              string `json:"id"`
	Summary         string `json:"summary"`
	SummaryOverride string `json:"summaryOverride"`
	Selected        bool   `json:"selected"`
	Primary         bool   `json:"primary"`
	AccessRole      string `json:"accessRole"`
}

// Name is the title the user sees: their own rename wins over the calendar's.
func (c Calendar) Name() string {
	if c.SummaryOverride != "" {
		return c.SummaryOverride
	}
	if c.Summary != "" {
		return c.Summary
	}
	return c.ID
}

func (c *Client) ListCalendars(ctx context.Context, access string) ([]Calendar, error) {
	var calendars []Calendar
	token := ""
	for page := 0; page < maximumListPages; page++ {
		query := url.Values{"maxResults": {"250"}}
		if token != "" {
			query.Set("pageToken", token)
		}
		var list struct {
			Items         []Calendar `json:"items"`
			NextPageToken string     `json:"nextPageToken"`
		}
		if err := c.get(ctx, access, "/users/me/calendarList", query, &list); err != nil {
			return nil, err
		}
		calendars = append(calendars, list.Items...)
		if token = list.NextPageToken; token == "" {
			break
		}
	}
	return calendars, nil
}

type EventTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
	TimeZone string `json:"timeZone"`
}

type Person struct {
	Email          string `json:"email"`
	DisplayName    string `json:"displayName"`
	Self           bool   `json:"self"`
	ResponseStatus string `json:"responseStatus"`
	Optional       bool   `json:"optional"`
}

// Event is the part of an event resource the tools read.
type Event struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	Summary          string    `json:"summary"`
	Description      string    `json:"description"`
	Location         string    `json:"location"`
	Start            EventTime `json:"start"`
	End              EventTime `json:"end"`
	Organizer        Person    `json:"organizer"`
	Attendees        []Person  `json:"attendees"`
	Recurrence       []string  `json:"recurrence"`
	RecurringEventID string    `json:"recurringEventId"`
	HangoutLink      string    `json:"hangoutLink"`
	ConferenceData   struct {
		EntryPoints []struct {
			EntryPointType string `json:"entryPointType"`
			URI            string `json:"uri"`
		} `json:"entryPoints"`
	} `json:"conferenceData"`
}

type EventsQuery struct {
	Min, Max  time.Time
	TimeZone  string
	Text      string
	Limit     int
	PageToken string
}

// ListEvents reads one page of a calendar's events in the range, with recurring events expanded
// into their instances and ordered by start. The second result is the next page's token.
func (c *Client) ListEvents(ctx context.Context, access, calendarID string, q EventsQuery) ([]Event, string, error) {
	query := url.Values{
		"singleEvents": {"true"},
		"orderBy":      {"startTime"},
		"timeMin":      {q.Min.Format(time.RFC3339)},
		"timeMax":      {q.Max.Format(time.RFC3339)},
		"maxResults":   {strconv.Itoa(q.Limit)},
	}
	if q.TimeZone != "" {
		query.Set("timeZone", q.TimeZone)
	}
	if q.Text != "" {
		query.Set("q", q.Text)
	}
	if q.PageToken != "" {
		query.Set("pageToken", q.PageToken)
	}
	var list struct {
		Items         []Event `json:"items"`
		NextPageToken string  `json:"nextPageToken"`
	}
	if err := c.get(ctx, access, "/calendars/"+url.PathEscape(calendarID)+"/events", query, &list); err != nil {
		return nil, "", err
	}
	return list.Items, list.NextPageToken, nil
}

func (c *Client) GetEvent(ctx context.Context, access, calendarID, eventID string) (Event, error) {
	var event Event
	err := c.get(ctx, access, "/calendars/"+url.PathEscape(calendarID)+"/events/"+url.PathEscape(eventID), nil, &event)
	return event, err
}

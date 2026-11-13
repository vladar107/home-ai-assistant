package calendar

import (
	"context"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
)

// betweenPageEvents is how many events one page of a calendar is asked for.
const betweenPageEvents = 250

// RangeEvent is one event as code that compares calendars reads it. An all-day event's Start and
// End are its first and last day, both inclusive, at midnight UTC, whatever zone the service reads
// dates in.
type RangeEvent struct {
	CalendarID string
	ID         string
	Title      string
	Place      string
	Start      time.Time
	End        time.Time
	AllDay     bool
}

// EventsBetween lists the principal's events between two instants, with their own Google accounts,
// from the calendars calendar_events lists: selected ones the user can read in full, without
// cancelled events, with recurring events as their instances. The second result is false when the
// list is not whole: no account has Calendar on, an account or a calendar could not be read, an
// event's times could not be read, or more than limit events exist.
func (s *Service) EventsBetween(ctx context.Context, principalID string, from, to time.Time, limit int) ([]RangeEvent, bool, error) {
	accounts, err := s.grants.Accounts(ctx, principalID, google.ProductCalendar)
	if err != nil {
		return nil, false, err
	}
	if len(accounts) == 0 {
		return nil, false, nil
	}
	var all []RangeEvent
	for _, account := range accounts {
		var read []RangeEvent
		whole := true
		err := s.withAccess(ctx, principalID, account, func(access string) error {
			read, whole = nil, true
			calendars, err := s.client.ListCalendars(ctx, access)
			if err != nil {
				return err
			}
			for _, calendar := range calendars {
				if !calendar.Selected || calendar.AccessRole == "freeBusyReader" {
					continue
				}
				request := EventsQuery{Min: from, Max: to, TimeZone: s.location.String(), Limit: betweenPageEvents}
				for page := 0; ; page++ {
					if page == maximumListPages {
						whole = false
						return nil
					}
					events, next, err := s.client.ListEvents(ctx, access, calendar.ID, request)
					if err != nil {
						if errorClass(err) == google.ErrorUnauthorized {
							return err
						}
						s.logFailure(ctx, principalID, "events_between", err)
						whole = false
						return nil
					}
					for _, event := range events {
						if event.Status == "cancelled" {
							continue
						}
						found, ok := rangeEvent(calendar.ID, event)
						if !ok || len(all)+len(read) == limit {
							whole = false
							return nil
						}
						read = append(read, found)
					}
					if next == "" {
						break
					}
					request.PageToken = next
				}
			}
			return nil
		})
		if err != nil {
			s.logFailure(ctx, principalID, "events_between", err)
			return nil, false, nil
		}
		if !whole {
			return nil, false, nil
		}
		all = append(all, read...)
	}
	return all, true, nil
}

func rangeEvent(calendarID string, event Event) (RangeEvent, bool) {
	found := RangeEvent{CalendarID: calendarID, ID: event.ID, Title: event.Summary, Place: event.Location}
	var startErr, endErr error
	if event.Start.DateTime != "" && event.End.DateTime != "" {
		found.Start, startErr = time.Parse(time.RFC3339, event.Start.DateTime)
		found.End, endErr = time.Parse(time.RFC3339, event.End.DateTime)
		return found, startErr == nil && endErr == nil
	}
	found.AllDay = true
	found.Start, startErr = time.Parse(time.DateOnly, event.Start.Date)
	found.End, endErr = time.Parse(time.DateOnly, event.End.Date)
	if startErr != nil || endErr != nil {
		return RangeEvent{}, false
	}
	if found.End = found.End.AddDate(0, 0, -1); found.End.Before(found.Start) {
		found.End = found.Start
	}
	return found, true
}

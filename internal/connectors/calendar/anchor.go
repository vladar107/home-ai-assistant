package calendar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

// maximumMatchEvents bounds one calendar's events read while looking for a match.
const maximumMatchEvents = 50

var _ schedule.AnchorResolver = (*Service)(nil)

// ResolveAnchor finds the event of a calendar anchor in the principal's own calendars, with the
// principal's own Google accounts, and only reads them. A calendar_event anchor's event is
// returned wherever it now is, and schedule.ErrAnchorGone once it is cancelled or deleted. A
// calendar_match anchor's is the first timed event within schedule.AnchorMatchHorizon whose title
// contains the anchor's and whose run, its start plus the offset, comes after the given instant;
// a zero occurrence when none does, never the occurrence exclude names, which it last ran for.
// Calendar switched off fails the call.
func (s *Service) ResolveAnchor(ctx context.Context, principalID string, anchor schedule.Anchor, after time.Time, exclude string) (schedule.AnchorOccurrence, error) {
	accounts, err := s.grants.Accounts(ctx, principalID, google.ProductCalendar)
	if err != nil {
		return schedule.AnchorOccurrence{}, err
	}
	if len(accounts) == 0 {
		return schedule.AnchorOccurrence{}, errors.New(NotConnectedText)
	}
	switch anchor.Kind {
	case schedule.AnchorCalendarEvent:
		return s.resolveEvent(ctx, principalID, accounts, anchor)
	case schedule.AnchorCalendarMatch:
		return s.resolveMatch(ctx, principalID, accounts, anchor, after, exclude)
	default:
		return schedule.AnchorOccurrence{}, fmt.Errorf("the calendar cannot resolve a %s anchor", anchor.Kind)
	}
}

func (s *Service) resolveEvent(ctx context.Context, principalID string, accounts []google.Account, anchor schedule.Anchor) (schedule.AnchorOccurrence, error) {
	account, ok := findAccount(accounts, anchor.Account)
	if !ok {
		return schedule.AnchorOccurrence{}, fmt.Errorf("no connected Google account with Calendar is %s", anchor.Account)
	}
	var event Event
	err := s.withAccess(ctx, principalID, account, func(access string) error {
		var err error
		event, err = s.client.GetEvent(ctx, access, anchor.CalendarID, anchor.EventID)
		return err
	})
	if errorClass(err) == google.ErrorNotFound || (err == nil && event.Status == "cancelled") {
		return schedule.AnchorOccurrence{}, schedule.ErrAnchorGone
	}
	if err != nil {
		s.logFailure(ctx, principalID, "anchor", err)
		return schedule.AnchorOccurrence{}, errors.New(describe(err))
	}
	start, _, _, ok := s.bounds(event)
	if !ok {
		return schedule.AnchorOccurrence{}, errors.New(unavailableText)
	}
	return schedule.AnchorOccurrence{Key: anchor.CalendarID + "/" + event.ID, At: start, Label: event.Summary}, nil
}

func (s *Service) resolveMatch(ctx context.Context, principalID string, accounts []google.Account, anchor schedule.Anchor, after time.Time, exclude string) (schedule.AnchorOccurrence, error) {
	if anchor.Account != "" {
		account, ok := findAccount(accounts, anchor.Account)
		if !ok {
			return schedule.AnchorOccurrence{}, fmt.Errorf("no connected Google account with Calendar is %s", anchor.Account)
		}
		accounts = []google.Account{account}
	}
	offset := time.Duration(anchor.OffsetMinutes) * time.Minute
	from := after.Add(-offset)
	until := from.Add(schedule.AnchorMatchHorizon)
	title := strings.ToLower(anchor.Title)
	var best schedule.AnchorOccurrence
	var failure error
	read := 0
	for _, account := range accounts {
		err := s.withAccess(ctx, principalID, account, func(access string) error {
			calendars, err := s.client.ListCalendars(ctx, access)
			if err != nil {
				return err
			}
			for _, calendar := range calendars {
				if !calendar.Selected || calendar.AccessRole == "freeBusyReader" || (anchor.CalendarID != "" && calendar.ID != anchor.CalendarID) {
					continue
				}
				events, _, err := s.client.ListEvents(ctx, access, calendar.ID, EventsQuery{
					Min: from, Max: until, TimeZone: s.location.String(), Text: anchor.Title, Limit: maximumMatchEvents,
				})
				if err != nil {
					if errorClass(err) == google.ErrorUnauthorized {
						return err
					}
					failure = err
					continue
				}
				read++
				for _, event := range events {
					start, _, allDay, ok := s.bounds(event)
					if event.Status == "cancelled" || !ok || allDay || !strings.Contains(strings.ToLower(event.Summary), title) || !start.After(from) {
						continue
					}
					key := calendar.ID + "/" + event.ID
					if exclude != "" && schedule.StoredKey(key) == exclude {
						continue
					}
					if best.Key == "" || start.Before(best.At) {
						best = schedule.AnchorOccurrence{Key: key, At: start, Label: event.Summary}
					}
				}
			}
			return nil
		})
		if err != nil {
			failure = err
		}
	}
	if read == 0 && failure != nil {
		s.logFailure(ctx, principalID, "anchor", failure)
		return schedule.AnchorOccurrence{}, errors.New(describe(failure))
	}
	return best, nil
}

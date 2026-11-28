package wiring

import (
	"context"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/looseend"
)

// looseEndScanEvents bounds the calendar events one Loose ends scan lists for its digest.
const looseEndScanEvents = 1000

// looseEndScanner builds the Loose ends scan, which the daily job runs after the nudges. It runs on
// the principal's own subscription with read-only tools, as the inbox check does, and reads mail and
// the calendar in code through the Google connectors once they are set up.
func (a *Assembly) looseEndScanner(service *looseend.Service) (*looseend.Scan, error) {
	return looseend.NewScan(looseend.ScanConfig{
		Checker: tripScanChecker{a.Turns}, Access: a.tripState, Mail: looseEndMail{a: a}, Calendar: looseEndCalendar{state: a.tripState}, Intake: a.in.Store,
	}, service, a.in.Logger)
}

// looseEndMail is the Gmail search the Loose ends scan runs in code, and the thread listing its
// nudges read.
type looseEndMail struct{ a *Assembly }

func (m looseEndMail) SearchMail(ctx context.Context, principalID, query string, limit int) ([]gmail.MailRef, error) {
	if m.a.gmail == nil {
		return nil, google.ErrNotConnected
	}
	return m.a.gmail.SearchMail(ctx, principalID, query, limit)
}

func (m looseEndMail) ListThread(ctx context.Context, principalID, account, threadID string) ([]gmail.ThreadMessage, error) {
	if m.a.gmail == nil {
		return nil, google.ErrNotConnected
	}
	return m.a.gmail.ListThread(ctx, principalID, account, threadID)
}

// looseEndCalendar lists the calendar events the Loose ends scan reads in code, with the accounts
// whose calendars it read.
type looseEndCalendar struct{ state *tripScanState }

func (c looseEndCalendar) CalendarEvents(ctx context.Context, principalID string, from, to time.Time) ([]looseend.CalendarEvent, []string, bool, error) {
	if c.state.google == nil || c.state.calendar == nil {
		return nil, nil, false, nil
	}
	accounts, err := c.state.google.Accounts(ctx, principalID, google.ProductCalendar)
	if err != nil {
		return nil, nil, false, err
	}
	read, complete, err := c.state.calendar.EventsBetween(ctx, principalID, from, to, looseEndScanEvents)
	if err != nil || !complete {
		return nil, nil, false, err
	}
	addresses := make([]string, 0, len(accounts))
	for _, account := range accounts {
		addresses = append(addresses, account.Address)
	}
	events := make([]looseend.CalendarEvent, 0, len(read))
	for _, event := range read {
		events = append(events, looseend.CalendarEvent{
			Account: event.Account, CalendarID: event.CalendarID, ID: event.ID, Start: event.Start, Description: event.Description,
			Organizer: event.Organizer, OrganizerSelf: event.OrganizerSelf, Updated: event.Updated,
		})
	}
	return events, addresses, true, nil
}

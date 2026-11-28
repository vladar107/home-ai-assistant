package wiring

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/calendar"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/looseend"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/calendarsim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
)

// The Loose ends scan reads the calendar through the Calendar connector: each event with its
// account, description, organiser, and update time, and the accounts read; before the connector is
// set up it reads nothing.
func TestLooseEndCalendarReadsTheConnectorsEvents(t *testing.T) {
	ctx := t.Context()
	dataDir := t.TempDir()
	store := testutil.OpenStore(t, filepath.Join(dataDir, "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	state := &tripScanState{}
	adapter := looseEndCalendar{state: state}
	now := time.Date(2026, 12, 1, 7, 20, 0, 0, time.UTC)
	if _, _, complete, err := adapter.CalendarEvents(ctx, "alice", now, now.Add(looseend.CalendarWindow)); err != nil || complete {
		t.Fatalf("read before the connector was set up: %v %v", complete, err)
	}

	sim := googlesim.New(t)
	googleService, err := google.NewService(google.Config{
		CredentialsDir: filepath.Join(dataDir, "integrations"), ClientID: googlesim.ClientID, ClientSecret: googlesim.ClientSecret,
		RedirectURL: googlesim.RedirectURL, AuthURL: sim.AuthURL(), TokenURL: sim.TokenURL(), RevokeURL: sim.RevokeURL(),
		UserInfoURL: sim.UserInfoURL(), HTTPClient: sim.Client(),
	}, store, logger)
	if err != nil {
		t.Fatal(err)
	}
	link, err := googleService.BeginSignIn("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := googleService.CompleteSignIn(ctx, "alice", sim.Consent(link, "alice@example.com", googlesim.Grant{Calendar: true})); err != nil {
		t.Fatal(err)
	}
	events := calendarsim.New(t, sim.AccountFor)
	events.AddCalendar("alice@example.com", calendarsim.Calendar{ID: "primary", Primary: true})
	start, updated := now.AddDate(0, 0, 10), now.Add(-time.Hour)
	events.AddEvents("alice@example.com", "primary", calendarsim.Event{
		ID: "e1", Summary: "Concert", Description: "Free cancellation until 8 December.", Organizer: "events@venue.example",
		Start: start, End: start.Add(2 * time.Hour), Updated: updated,
	})
	calendarService, err := calendar.NewService(calendar.Config{Location: time.UTC, APIBaseURL: events.APIBaseURL(), HTTPClient: events.Client()}, googleService, logger)
	if err != nil {
		t.Fatal(err)
	}
	state.google, state.calendar = googleService, calendarService

	read, accounts, complete, err := adapter.CalendarEvents(ctx, "alice", now, now.Add(looseend.CalendarWindow))
	if err != nil || !complete || len(accounts) != 1 || accounts[0] != "alice@example.com" || len(read) != 1 {
		t.Fatalf("read = %+v %v %v %v", read, accounts, complete, err)
	}
	want := looseend.CalendarEvent{
		Account: "alice@example.com", CalendarID: "primary", ID: "e1", Start: start, Description: "Free cancellation until 8 December.",
		Organizer: "events@venue.example", Updated: updated.Format(time.RFC3339Nano),
	}
	if got := read[0]; !got.Start.Equal(want.Start) || got.Account != want.Account || got.CalendarID != want.CalendarID || got.ID != want.ID ||
		got.Description != want.Description || got.Organizer != want.Organizer || got.OrganizerSelf || got.Updated != want.Updated {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
}

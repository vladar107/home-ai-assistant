package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

type idleChecker struct{}

func (idleChecker) RunCheck(context.Context, string, string) (string, string, error) {
	return `{"trips":[]}`, "", nil
}

// trip_list in a chat turn says the Trips scan is off exactly while Google mail or calendar can be
// read and the principal has no enabled Trips job, and a scheduled turn never says it. The scan
// counts as having read mail only while mail itself can be read.
func TestTripListSaysWhenTheScanIsOff(t *testing.T) {
	ctx := t.Context()
	dataDir := t.TempDir()
	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) }
	scheduler, err := schedule.NewService(schedule.Config{Zone: "Europe/Lisbon", Now: now}, store, store, struct{ conversation.Deliverer }{}, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	trips, err := trip.NewService(trip.Config{Now: now}, store, trip.NewScheduleReminders(scheduler), logger)
	if err != nil {
		t.Fatal(err)
	}
	state := &tripScanState{schedules: scheduler}
	if err := scheduler.Register(trip.ScanJobType(idleChecker{}, store, state, trips, nil, nil, now, logger)); err != nil {
		t.Fatal(err)
	}
	offered, err := trip.NewTools(trip.ToolsConfig{Zone: "Europe/Lisbon"}, trips, nil, logger)
	if err != nil {
		t.Fatal(err)
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
	offered.SetScanState(state)
	chat := tools.WithTurn(ctx, outbox.ChatTurn("a1"))
	listed := func(turn context.Context, principalID string) bool {
		t.Helper()
		result, err := offered.Execute(turn, principalID, tools.Call{Name: "trip_list", Arguments: json.RawMessage(`{}`)})
		if err != nil || result.IsError {
			t.Fatalf("trip_list: %+v %v", result, err)
		}
		return strings.Contains(result.Text, "The Trips scan is off")
	}
	scanOff := func(principalID string) bool {
		t.Helper()
		return listed(chat, principalID)
	}
	mailReadable := func(principalID string) bool {
		t.Helper()
		readable, err := state.MailReadable(ctx, principalID)
		if err != nil {
			t.Fatal(err)
		}
		return readable
	}

	if scanOff("alice") || mailReadable("alice") {
		t.Fatal("something was readable before the Google connector was set up")
	}
	state.google = googleService
	if scanOff("alice") || mailReadable("alice") {
		t.Fatal("something was readable without a Google account")
	}
	link, err := googleService.BeginSignIn("alice")
	if err != nil {
		t.Fatal(err)
	}
	account, err := googleService.CompleteSignIn(ctx, "alice", sim.Consent(link, "alice@example.com", googlesim.Grant{Mail: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !scanOff("alice") || scanOff("bob") || !mailReadable("alice") {
		t.Fatalf("with alice's mail readable: alice %v, bob %v, mail %v", scanOff("alice"), scanOff("bob"), mailReadable("alice"))
	}
	if listed(ctx, "alice") {
		t.Fatal("a turn outside chat got the scan-off line")
	}
	link, err = googleService.BeginSignIn("bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := googleService.CompleteSignIn(ctx, "bob", sim.Consent(link, "bob@example.com", googlesim.Grant{Calendar: true})); err != nil {
		t.Fatal(err)
	}
	if !scanOff("bob") || mailReadable("bob") {
		t.Fatalf("with bob's calendar only: scan off %v, mail %v", scanOff("bob"), mailReadable("bob"))
	}

	id := schedule.TripsJobID("alice")
	prepared, err := scheduler.Prepare("alice", schedule.TripsName, schedule.TripScanType, schedule.Timing{Cron: schedule.TripsCron}, trip.ScanConfig())
	if err != nil {
		t.Fatal(err)
	}
	if created, err := scheduler.Create(ctx, id, prepared); err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	if scanOff("alice") {
		t.Fatal("the scan-off line appeared while the scan is on")
	}
	if changed, err := scheduler.SetEnabled(ctx, "alice", id, false); err != nil || !changed {
		t.Fatalf("pause: %v %v", changed, err)
	}
	if !scanOff("alice") {
		t.Fatal("the scan-off line is missing while the scan is paused")
	}

	if changed, err := googleService.SetProduct(ctx, "alice", account.ID, google.ProductMail, false); err != nil || !changed {
		t.Fatalf("mail off: %v %v", changed, err)
	}
	if scanOff("alice") || mailReadable("alice") {
		t.Fatal("something was readable with mail switched off")
	}
}

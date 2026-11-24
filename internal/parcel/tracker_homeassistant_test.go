package parcel_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/homeassistant"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/homesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

// Home Assistant can answer while the carrier entries assigned to the user have not read once, as
// when their sensors are unavailable after a restart. Their parcels are still unknown then, so the
// baseline is not over for them, however many empty reads come first.
func TestTheBaselineStaysSilentUntilEveryAssignedSourceHasRead(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "connectors", "homeassistant", "testdata", "parcels", "household.json"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	clock := &trackerClock{now: start}
	sim := homesim.New(t, raw, clock.Now)
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := homeassistant.NewService(homeassistant.Config{Token: sim.Token(), BaseURL: sim.URL(), Now: clock.Now}, simkit.AlwaysOn{}, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	source, err := service.NewParcelSource(map[string][]string{"alice": {"entry_postnl_a", "entry_dhlnl_a", "entry_dhl_a"}}, store)
	if err != nil {
		t.Fatal(err)
	}
	cards := &fakeCards{gone: map[string]bool{}}
	loop, err := parcel.NewLoop(parcel.LoopConfig{
		Repository: store, Sources: source, Cards: cards, Enabled: &fakeSwitch{on: true},
		Principals: func() []string { return []string{"alice"} }, Now: clock.Now, Location: time.UTC, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	step := func(at time.Duration) {
		clock.Set(start.Add(at))
		loop.Cycle(t.Context())
		loop.Wait()
	}

	sim.FailStates(true)
	step(0)
	step(10 * time.Minute)
	sim.FailStates(false)
	step(20 * time.Minute)
	sends := cards.list("send")
	if len(sends) == 0 || len(cards.list("notice")) != 0 {
		t.Fatalf("events = %+v", cards.events)
	}
	if tracking, err := store.LoadTracking(t.Context(), "alice"); err != nil || tracking.SourcesOwed {
		t.Fatalf("tracking = %+v %v", tracking, err)
	}
}

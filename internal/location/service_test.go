package location_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/location"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

var base = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

type describer struct {
	name string
	err  error
}

func (d describer) Describe(context.Context, float64, float64) (string, error) { return d.name, d.err }

var standard = location.Policy{PinTTL: time.Hour, LiveSilence: time.Hour, LiveGrace: 15 * time.Minute}

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func newService(t *testing.T, store *sqlite.Store, policy location.Policy, c *clock, d location.Describer) *location.Service {
	t.Helper()
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	service, err := location.NewService(location.Config{Policy: policy, PruneInterval: time.Hour, Describer: d, Zone: amsterdam, Now: c.Now}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestPolicyExpiry(t *testing.T) {
	until := base.Add(15 * time.Minute)
	for name, test := range map[string]struct {
		fix  location.Fix
		want time.Time
	}{
		"pin":         {location.Fix{Source: location.SourcePin, UpdatedAt: base}, base.Add(time.Hour)},
		"venue":       {location.Fix{Source: location.SourceVenue, UpdatedAt: base}, base.Add(time.Hour)},
		"live":        {location.Fix{Source: location.SourceLive, LiveUntil: until, UpdatedAt: base}, until.Add(15 * time.Minute)},
		"ended early": {location.Fix{Source: location.SourceLive, LiveUntil: until, EndedAt: base.Add(5 * time.Minute), UpdatedAt: base}, base.Add(20 * time.Minute)},
		"ended after": {location.Fix{Source: location.SourceLive, LiveUntil: until, EndedAt: until.Add(time.Minute), UpdatedAt: base}, until.Add(15 * time.Minute)},
		"open ended":  {location.Fix{Source: location.SourceLive, UpdatedAt: base.Add(10 * time.Minute)}, base.Add(70 * time.Minute)},
		"open stop":   {location.Fix{Source: location.SourceLive, EndedAt: base.Add(time.Minute), UpdatedAt: base}, base.Add(16 * time.Minute)},
	} {
		if got := standard.ExpiresAt(test.fix); !got.Equal(test.want) {
			t.Errorf("%s expires at %v, want %v", name, got, test.want)
		}
	}
}

func TestCurrentHidesExpiredPositionsUnderTheCurrentPolicy(t *testing.T) {
	store := newStore(t)
	c := &clock{now: base}
	service := newService(t, store, standard, c, nil)
	if _, err := service.Share(t.Context(), location.Fix{PrincipalID: "alice", Latitude: 52, Longitude: 4, Source: location.SourcePin, UpdatedAt: base}); err != nil {
		t.Fatal(err)
	}

	c.now = base.Add(59 * time.Minute)
	if _, found, err := service.Current(t.Context(), "alice"); err != nil || !found {
		t.Fatalf("fresh pin = %v %v", found, err)
	}
	c.now = base.Add(time.Hour)
	if _, found, _ := service.Current(t.Context(), "alice"); found {
		t.Fatal("an expired pin is visible")
	}

	c.now = base.Add(20 * time.Minute)
	shorter := newService(t, store, location.Policy{PinTTL: 10 * time.Minute, LiveSilence: time.Hour, LiveGrace: time.Minute}, c, nil)
	if _, found, _ := shorter.Current(t.Context(), "alice"); found {
		t.Fatal("a shorter policy did not hide the stored pin")
	}
	if _, found, _ := service.Current(t.Context(), "alice"); !found {
		t.Fatal("the longer policy lost the pin")
	}
}

func TestPruneDeletesOnlyExpiredRows(t *testing.T) {
	store := newStore(t)
	c := &clock{now: base}
	service := newService(t, store, standard, c, nil)
	if _, err := service.Share(t.Context(), location.Fix{PrincipalID: "alice", Latitude: 52, Longitude: 4, Source: location.SourcePin, UpdatedAt: base}); err != nil {
		t.Fatal(err)
	}
	c.now = base.Add(30 * time.Minute)
	if _, err := service.Share(t.Context(), location.Fix{PrincipalID: "bob", Latitude: 52, Longitude: 4, Source: location.SourcePin, UpdatedAt: c.now}); err != nil {
		t.Fatal(err)
	}

	c.now = base.Add(45 * time.Minute)
	if err := service.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fixes, _ := store.ListLocations(t.Context()); len(fixes) != 2 {
		t.Fatalf("prune removed fresh rows: %+v", fixes)
	}
	c.now = base.Add(70 * time.Minute)
	if err := service.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixes, _ := store.ListLocations(t.Context())
	if len(fixes) != 1 || fixes[0].PrincipalID != "bob" {
		t.Fatalf("after prune = %+v", fixes)
	}
}

func TestTurnLine(t *testing.T) {
	store := newStore(t)
	c := &clock{now: base}
	service := newService(t, store, standard, c, describer{name: "Stationsplein 1, Leiden"})
	ctx := t.Context()

	if line, err := service.TurnLine(ctx, "alice"); err != nil || line != "" {
		t.Fatalf("line without a position = %q %v", line, err)
	}

	live := location.Fix{
		PrincipalID: "alice", Latitude: 52.166123, Longitude: 4.481049, Accuracy: 14.6, Source: location.SourceLive,
		LiveMessageID: 5, LiveUntil: base.Add(15 * time.Minute), VenueTitle: "Cafe Zuid", UpdatedAt: base,
	}
	if _, err := service.Share(ctx, live); err != nil {
		t.Fatal(err)
	}
	c.now = base.Add(40 * time.Second)
	line, err := service.TurnLine(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	// 12:15 UTC is 14:15 in Amsterdam.
	want := "User's location: 52.16612, 4.48105 (±15 m; live until 14:15, updated 40 s ago), at Cafe Zuid, near Stationsplein 1, Leiden."
	if line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	if line, _ := service.TurnLine(ctx, "bob"); line != "" {
		t.Fatalf("bob's line = %q", line)
	}

	failing := newService(t, store, standard, c, describer{err: errors.New("down")})
	line, err = failing.TurnLine(ctx, "alice")
	if err != nil || !strings.HasPrefix(line, "User's location: 52.16612") || strings.Contains(line, "near") {
		t.Fatalf("line after a describer failure = %q %v", line, err)
	}

	c.now = base.Add(time.Hour)
	if line, _ := service.TurnLine(ctx, "alice"); line != "" {
		t.Fatalf("expired line = %q", line)
	}
}

func TestSummaryAndShareAcknowledgements(t *testing.T) {
	store := newStore(t)
	c := &clock{now: base}
	service := newService(t, store, standard, c, describer{name: "Leiden"})
	ctx := t.Context()
	if text, shared, err := service.Summary(ctx, "alice"); err != nil || shared || text != "Not shared" {
		t.Fatalf("summary = %q %v %v", text, shared, err)
	}
	ack, err := service.Share(ctx, location.Fix{PrincipalID: "alice", Latitude: 1, Longitude: 1, Source: location.SourceLive, LiveMessageID: 3, UpdatedAt: base})
	if err != nil || ack != "📍 Following your live location until you stop sharing it, near Leiden." {
		t.Fatalf("ack = %q %v", ack, err)
	}
	if text, shared, _ := service.Summary(ctx, "alice"); !shared || text != "Shared: live until you stop it, near Leiden" {
		t.Fatalf("summary = %q %v", text, shared)
	}
	if existed, err := service.Forget(ctx, "alice"); err != nil || !existed {
		t.Fatalf("forget = %v %v", existed, err)
	}
	moved, err := service.Move(ctx, location.Fix{PrincipalID: "alice", Latitude: 2, Longitude: 2, Source: location.SourceLive, LiveMessageID: 3, UpdatedAt: base.Add(time.Second)})
	if err != nil || moved {
		t.Fatalf("move after forget = %v %v", moved, err)
	}
}

package sqlite

import (
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/location"
)

var locationBase = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func liveFix(principal string, messageID int64, latitude float64, at time.Time) location.Fix {
	return location.Fix{
		PrincipalID: principal, Latitude: latitude, Longitude: 4.48105, Accuracy: 12, Source: location.SourceLive,
		LiveMessageID: messageID, LiveUntil: locationBase.Add(15 * time.Minute), UpdatedAt: at,
	}
}

func mustLoad(t *testing.T, store *Store, principal string) (location.Fix, bool) {
	t.Helper()
	fix, found, err := store.LoadLocation(t.Context(), principal)
	if err != nil {
		t.Fatal(err)
	}
	return fix, found
}

func TestLocationRoundTrip(t *testing.T) {
	store, ctx := openTestStore(t)
	for _, fix := range []location.Fix{
		{PrincipalID: "alice", Latitude: 52.16612, Longitude: -4.48105, Accuracy: 15.5, Source: location.SourcePin, UpdatedAt: locationBase},
		{PrincipalID: "alice", Latitude: 52.1, Longitude: 4.4, Source: location.SourceVenue, VenueTitle: "Café Zuid", VenueAddress: "Kerkstraat 2, Leiden", UpdatedAt: locationBase},
		liveFix("alice", 77, 52.2, locationBase),
		{PrincipalID: "alice", Latitude: 52.3, Longitude: 4.3, Source: location.SourceLive, LiveMessageID: 78, EndedAt: locationBase.Add(time.Minute), UpdatedAt: locationBase.Add(time.Minute)},
	} {
		if err := store.PutLocation(ctx, fix); err != nil {
			t.Fatal(err)
		}
		got, found := mustLoad(t, store, "alice")
		if !found || got != fix {
			t.Fatalf("round trip = %+v %v, want %+v", got, found, fix)
		}
	}
}

func TestMoveLiveLocationRules(t *testing.T) {
	store, ctx := openTestStore(t)
	move := func(fix location.Fix) bool {
		t.Helper()
		moved, err := store.MoveLiveLocation(ctx, fix)
		if err != nil {
			t.Fatal(err)
		}
		return moved
	}
	if err := store.PutLocation(ctx, liveFix("alice", 77, 52.0, locationBase)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutLocation(ctx, liveFix("bob", 88, 40.0, locationBase)); err != nil {
		t.Fatal(err)
	}

	if !move(liveFix("alice", 77, 52.1, locationBase.Add(10*time.Second))) {
		t.Fatal("a newer edit was refused")
	}
	if got, _ := mustLoad(t, store, "alice"); got.Latitude != 52.1 {
		t.Fatalf("stored = %+v", got)
	}
	if move(liveFix("alice", 77, 51.0, locationBase.Add(5*time.Second))) {
		t.Fatal("an older edit was applied")
	}
	if move(liveFix("alice", 99, 50.0, locationBase.Add(time.Minute))) {
		t.Fatal("an edit of another live message replaced the stored share")
	}
	if got, _ := mustLoad(t, store, "alice"); got.Latitude != 52.1 || got.LiveMessageID != 77 {
		t.Fatalf("stored after refused edits = %+v", got)
	}

	ended := liveFix("alice", 77, 52.1, locationBase.Add(20*time.Second))
	ended.LiveUntil, ended.EndedAt = time.Time{}, locationBase.Add(20*time.Second)
	if !move(ended) {
		t.Fatal("the stop edit was refused")
	}
	if move(liveFix("alice", 77, 53.0, locationBase.Add(time.Minute))) {
		t.Fatal("an edit after the share ended was applied")
	}

	if got, _ := mustLoad(t, store, "bob"); got.Latitude != 40.0 {
		t.Fatalf("bob's position was touched: %+v", got)
	}

	if err := store.DeleteLocation(ctx, "bob", locationBase); err != nil {
		t.Fatal(err)
	}
	if !move(liveFix("bob", 88, 41.0, locationBase.Add(time.Minute))) {
		t.Fatal("an edit with no stored row did not restore the share")
	}
	if got, found := mustLoad(t, store, "bob"); !found || got.Latitude != 41.0 {
		t.Fatalf("restored = %+v %v", got, found)
	}
}

func TestForgetLocationIgnoresTheRunningShareForGood(t *testing.T) {
	store, ctx := openTestStore(t)
	if err := store.PutLocation(ctx, liveFix("alice", 77, 52.0, locationBase)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutLocation(ctx, liveFix("bob", 88, 40.0, locationBase)); err != nil {
		t.Fatal(err)
	}
	existed, err := store.ForgetLocation(ctx, "alice", locationBase.Add(time.Minute))
	if err != nil || !existed {
		t.Fatalf("forget = %v %v", existed, err)
	}
	moved, err := store.MoveLiveLocation(ctx, liveFix("alice", 77, 52.5, locationBase.Add(2*time.Minute)))
	if err != nil || moved {
		t.Fatalf("move after forget = %v %v", moved, err)
	}
	if _, found := mustLoad(t, store, "alice"); found {
		t.Fatal("an edit stored the forgotten position again")
	}
	if got, found := mustLoad(t, store, "bob"); !found || got.Latitude != 40.0 {
		t.Fatalf("bob's position = %+v %v", got, found)
	}

	// The ignore outlives later shares and their deletion; only a different live message escapes it.
	pin := location.Fix{PrincipalID: "alice", Latitude: 53.0, Longitude: 4.0, Source: location.SourcePin, UpdatedAt: locationBase.Add(3 * time.Minute)}
	if err := store.PutLocation(ctx, pin); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteLocation(ctx, "alice", pin.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if moved, err := store.MoveLiveLocation(ctx, liveFix("alice", 77, 52.6, locationBase.Add(4*time.Minute))); err != nil || moved {
		t.Fatalf("move of the forgotten message after a pin = %v %v", moved, err)
	}
	if _, found := mustLoad(t, store, "alice"); found {
		t.Fatal("the forgotten message stored a position")
	}
	if err := store.PutLocation(ctx, liveFix("alice", 90, 53.0, locationBase.Add(5*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if moved, err := store.MoveLiveLocation(ctx, liveFix("alice", 90, 53.5, locationBase.Add(6*time.Minute))); err != nil || !moved {
		t.Fatalf("move of a new share = %v %v", moved, err)
	}
	if moved, err := store.MoveLiveLocation(ctx, liveFix("alice", 77, 52.7, locationBase.Add(7*time.Minute))); err != nil || moved {
		t.Fatalf("move of the forgotten message during a new share = %v %v", moved, err)
	}

	if existed, err := store.ForgetLocation(ctx, "alice", locationBase.Add(time.Hour)); err != nil || !existed {
		t.Fatalf("forget of an expired share = %v %v", existed, err)
	}
	if existed, err := store.ForgetLocation(ctx, "alice", locationBase.Add(time.Hour)); err != nil || existed {
		t.Fatalf("second forget = %v %v", existed, err)
	}
}

func TestDeleteLocationOnlyRemovesTheVersionItWasGiven(t *testing.T) {
	store, ctx := openTestStore(t)
	if err := store.PutLocation(ctx, liveFix("alice", 77, 52.0, locationBase)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutLocation(ctx, liveFix("bob", 88, 40.0, locationBase)); err != nil {
		t.Fatal(err)
	}
	if moved, err := store.MoveLiveLocation(ctx, liveFix("alice", 77, 52.1, locationBase.Add(time.Minute))); err != nil || !moved {
		t.Fatal(moved, err)
	}
	if err := store.DeleteLocation(ctx, "alice", locationBase); err != nil {
		t.Fatal(err)
	}
	if _, found := mustLoad(t, store, "alice"); !found {
		t.Fatal("a stale delete removed a position that had moved")
	}
	if err := store.DeleteLocation(ctx, "alice", locationBase.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, found := mustLoad(t, store, "alice"); found {
		t.Fatal("the matching delete kept the position")
	}
	if _, found := mustLoad(t, store, "bob"); !found {
		t.Fatal("alice's delete removed bob's position")
	}
	fixes, err := store.ListLocations(ctx)
	if err != nil || len(fixes) != 1 || fixes[0].PrincipalID != "bob" {
		t.Fatalf("list = %+v %v", fixes, err)
	}
}

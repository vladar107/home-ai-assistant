package parcel_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

// A number is proposed for adding to Home Assistant only when the user has an assigned entry of the
// carrier's domain; otherwise the carrier counts as having no integration, which sends it to mail.
func TestAProposalNeedsAnAssignedEntryOfTheDomain(t *testing.T) {
	t.Parallel()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	has := false
	proposals := parcel.Proposals{
		Repository: store, Approvals: store, Carriers: parcel.DefaultCarriers(), Now: time.Now,
		HasEntry: func(_ context.Context, principalID, domain string) bool {
			return has && principalID == "alice" && domain == "dhl"
		},
	}

	if _, err := proposals.Proposal(t.Context(), "alice", dhlNumber, "dhl"); !errors.Is(err, parcel.ErrNoIntegration) || !errors.Is(err, parcel.ErrNotProposed) {
		t.Fatalf("without an entry: %v", err)
	}
	has = true
	if proposal, err := proposals.Proposal(t.Context(), "alice", dhlNumber, "dhl"); err != nil || proposal.Tool != parcel.ToolTrack {
		t.Fatalf("with an entry: %+v %v", proposal, err)
	}
	if _, err := proposals.Proposal(t.Context(), "alice", dhlNumber, "gls"); !errors.Is(err, parcel.ErrNoIntegration) {
		t.Fatalf("another domain: %v", err)
	}
}

// The user's own request for a number passes a decline and offers that went unanswered, which keep
// the mail job and the inbox check from offering it again; a number they stopped following stays
// refused.
func TestARequestedNumberPassesADeclineAndUnansweredOffers(t *testing.T) {
	t.Parallel()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	proposals := parcel.Proposals{Repository: store, Carriers: parcel.DefaultCarriers(), Now: func() time.Time { return now }}

	const unanswered, declined = "1234567891", "1234567892"
	for range parcel.MaximumOffers {
		if err := proposals.Offered(t.Context(), "alice", unanswered, "dhl", "approval-1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetProposalOutcome(t.Context(), "alice", declined, parcel.OutcomeDeclined, now); err != nil {
		t.Fatal(err)
	}
	for _, number := range []string{unanswered, declined} {
		if _, err := proposals.Proposal(t.Context(), "alice", number, "dhl"); !errors.Is(err, parcel.ErrNotProposed) {
			t.Fatalf("%s is offered unasked: %v", number, err)
		}
	}
	// Without the approvals to ask, the unanswered card counts as waiting, so only the decline is
	// tried here; the waiting check is the same for both callers.
	if proposal, err := proposals.Requested(t.Context(), "alice", declined, "dhl"); err != nil || proposal.Tool != parcel.ToolTrack {
		t.Fatalf("a declined number asked for again = %+v %v", proposal, err)
	}
	if err := proposals.StillWanted(t.Context(), "alice", declined, "dhl"); err != nil {
		t.Fatalf("a declined number asked for again is refused when it runs: %v", err)
	}

	proposals.Approvals = store
	if proposal, err := proposals.Requested(t.Context(), "alice", unanswered, "dhl"); err != nil || proposal.Tool != parcel.ToolTrack {
		t.Fatalf("an unanswered number asked for again = %+v %v", proposal, err)
	}

	stopped, _, err := store.UpsertParcel(t.Context(), parcel.Parcel{
		PrincipalID: "alice", Carrier: "dhl", Number: "1234567893", Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.StopParcel(t.Context(), "alice", stopped.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := proposals.Requested(t.Context(), "alice", "1234567893", "dhl"); !errors.Is(err, parcel.ErrNotProposed) {
		t.Fatalf("a stopped number asked for again: %v", err)
	}
}

// A number typed without a carrier whose pattern several carriers share is followed under one of
// them, with its link, so a source that names that carrier finds the same parcel.
func TestATypedNumberSeveralCarriersShareIsFollowedUnderOneOfThem(t *testing.T) {
	t.Parallel()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	proposals := parcel.Proposals{Repository: store, Carriers: parcel.DefaultCarriers(), Now: time.Now}
	if followed, err := proposals.FollowMail(t.Context(), "alice", "3SEXA12345679", "", "", ""); err != nil || followed != parcel.FollowedNew {
		t.Fatalf("follow = %v %v", followed, err)
	}
	held, err := store.ListParcels(t.Context(), "alice")
	if err != nil || len(held) != 1 || held[0].Carrier != "postnl" || held[0].Link == "" {
		t.Fatalf("parcels = %+v %v", held, err)
	}
}

// A parcel dropped because no source had news of it can be asked for again; one the user stopped
// cannot.
func TestAnIdleParcelCanBeAskedForAgainButAStoppedOneCannot(t *testing.T) {
	t.Parallel()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := start.Add(parcel.IdleLifetime + time.Hour)
	const idle, stopped = "3SABCD11111111", "3SABCD33333333"
	for _, number := range []string{idle, stopped} {
		held, _, err := store.UpsertParcel(t.Context(), parcel.Parcel{
			PrincipalID: "alice", Carrier: "postnl", Number: number, Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: start, UpdatedAt: start,
		})
		if err != nil {
			t.Fatal(err)
		}
		if number == stopped {
			if _, _, err := store.StopParcel(t.Context(), "alice", held.ID, start); err != nil {
				t.Fatal(err)
			}
		}
	}
	if dropped, err := store.DeleteIdle(t.Context(), "alice", now.Add(-parcel.IdleLifetime)); err != nil || len(dropped) != 1 {
		t.Fatalf("dropped = %+v %v", dropped, err)
	}
	proposals := parcel.Proposals{Repository: store, Carriers: parcel.DefaultCarriers(), Now: func() time.Time { return now }}
	for number, want := range map[string]parcel.Followed{idle: parcel.FollowedNew, stopped: parcel.FollowedTombstoned} {
		if followed, err := proposals.FollowMail(t.Context(), "alice", number, "postnl", "", ""); err != nil || followed != want {
			t.Fatalf("asking for %s = %v %v, want %v", number, followed, err, want)
		}
	}
	if err := proposals.StillWanted(t.Context(), "alice", idle, "postnl"); err != nil {
		t.Fatalf("an idle number asked for again is refused when it runs: %v", err)
	}
}

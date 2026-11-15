package intake_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/intake"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const parcelNumber = "1234567890"

var parcelNow = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

// parcelSupport is the parcel side as wiring offers it: the shared proposal check over the store,
// with a Parcels switch a test flips.
type parcelSupport struct {
	parcel.Proposals
	on bool
}

func (p *parcelSupport) Enabled(context.Context, string) (bool, error) { return p.on, nil }

type parcelChecker struct {
	reply   string
	prompts []string
}

func (c *parcelChecker) RunCheck(_ context.Context, _, message string) (string, string, error) {
	c.prompts = append(c.prompts, message)
	return c.reply, "", nil
}

type pendingApprovals struct{ pending string }

func (a pendingApprovals) LoadApproval(_ context.Context, id string) (approval.Record, bool, error) {
	if id != a.pending {
		return approval.Record{}, false, nil
	}
	return approval.Record{ID: id, PrincipalID: "alice", Status: approval.StatusPending, ExpiresAt: parcelNow.Add(time.Hour)}, true, nil
}

type parcelFixture struct {
	t       *testing.T
	store   *sqlite.Store
	support *parcelSupport
	service *intake.Service
	checker *parcelChecker
	offers  *intakeLangOffers
}

func newParcelFixture(t *testing.T, on bool) *parcelFixture {
	t.Helper()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	f := &parcelFixture{t: t, store: store, checker: &parcelChecker{}, offers: &intakeLangOffers{}}
	f.support = &parcelSupport{on: on, Proposals: parcel.Proposals{
		Repository: store, Approvals: pendingApprovals{pending: "pending-card"}, Carriers: parcel.DefaultCarriers(), Now: func() time.Time { return parcelNow },
	}}
	service, err := intake.NewService(intake.Config{Location: time.UTC, Now: func() time.Time { return parcelNow }, Parcels: f.support},
		store, &cleanTurn{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f
}

func (f *parcelFixture) scan(actions string) schedule.Result {
	f.t.Helper()
	f.checker.reply = `{"messages":[{"account":"alice@example.com","message_id":"m1","kind":"parcel notice","title":"Parcel on its way","sender":"Example Carrier",` +
		`"dates":[],"amounts":[],"references":[],"action_required":"","actions":[` + actions + `]}],"left":0}`
	jobType, err := f.service.ScanJobType(intake.ScanConfig{
		Checker: f.checker, Mail: intakeLangMail{}, Approvals: intakeLangApprovals{}, Offers: f.offers, Chats: intakeLangChats{},
		Reminders: intakeLangReminders{}, Watches: parcelWatches{},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	result, err := jobType.Handler.Run(f.t.Context(), schedule.Occurrence{PrincipalID: "alice", JobID: "job", JobName: "Inbox", ScheduledFor: parcelNow, Location: time.UTC})
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

type parcelWatches struct{}

func (parcelWatches) AgentProposal(_ context.Context, _, name string, _ watch.Check) (approval.Proposal, error) {
	return approval.Proposal{Tool: "watch_agent", Arguments: json.RawMessage(`{}`), Summary: "Watch: " + name}, nil
}

const (
	parcelAction = `{"type":"parcel","number":"1234-567 890","carrier":"DHL"}`
	watchAction  = `{"type":"watch","name":"Parcel","instruction":"Look for the delivery mail","condition":"it arrived","interval_minutes":60,"until":"2026-10-20"}`
)

func (f *parcelFixture) tools() []string {
	var names []string
	for _, proposal := range f.offers.proposals {
		names = append(names, proposal.Tool)
	}
	return names
}

func TestInboxCheckOffersAParcelFollowUpOnlyWhileParcelsAreOn(t *testing.T) {
	t.Parallel()
	on := newParcelFixture(t, true)
	on.scan(parcelAction + "," + watchAction)
	if prompt := on.checker.prompts[0]; !strings.Contains(prompt, `{"type":"parcel","number":"","carrier":""}`) || !strings.Contains(prompt, "never for a parcel's delivery") {
		t.Fatalf("the prompt with parcels on:\n%s", prompt)
	}
	if got := on.tools(); len(got) != 2 || got[0] != parcel.ToolTrack {
		t.Fatalf("offered %v", got)
	}
	var arguments parcel.TrackArguments
	if err := json.Unmarshal(on.offers.proposals[0].Arguments, &arguments); err != nil || arguments.Number != parcelNumber || arguments.Carrier != "dhl" {
		t.Fatalf("arguments = %+v %v", arguments, err)
	}
	if summary := on.offers.proposals[0].Summary; !strings.Contains(summary, "outside content") || !strings.Contains(summary, "About: Parcel on its way, from Example Carrier") {
		t.Fatalf("summary = %q", summary)
	}
	recorded, found, err := on.store.LoadProposal(t.Context(), "alice", parcelNumber)
	if err != nil || !found || recorded.ApprovalID != "a" || recorded.Offers != 1 {
		t.Fatalf("recorded = %+v %v %v", recorded, found, err)
	}

	off := newParcelFixture(t, false)
	off.scan(parcelAction + "," + watchAction)
	if prompt := off.checker.prompts[0]; strings.Contains(prompt, `"parcel"`) || !strings.Contains(prompt, "such as for a parcel's delivery") {
		t.Fatalf("the prompt with parcels off:\n%s", prompt)
	}
	if got := off.tools(); len(got) != 1 || got[0] != "watch_agent" {
		t.Fatalf("offered %v", got)
	}
}

func TestInboxCheckDropsMalformedParcelFollowUps(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, true)
	f.scan(`{"type":"parcel","number":"12"},{"type":"parcel","number":"1234567890","carrier":"not a word!"},{"type":"parcel","number":"","carrier":"dhl"}`)
	if len(f.offers.proposals) != 0 {
		t.Fatalf("offered %+v", f.offers.proposals)
	}
}

func TestInboxCheckSkipsNumbersAlreadyTrackedTombstonedPendingOrDeclined(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(*parcelFixture){
		"tracked": func(f *parcelFixture) {
			err := f.store.SaveAddedNumber(f.t.Context(), parcel.AddedNumber{PrincipalID: "alice", Carrier: "dhl", Number: parcelNumber, Domain: "dhl", EntryID: "entry", AddedAt: parcelNow})
			if err != nil {
				f.t.Fatal(err)
			}
		},
		"declined": func(f *parcelFixture) {
			if err := f.store.SetProposalOutcome(f.t.Context(), "alice", parcelNumber, parcel.OutcomeDeclined, parcelNow); err != nil {
				f.t.Fatal(err)
			}
		},
		"pending": func(f *parcelFixture) {
			err := f.store.RecordProposal(f.t.Context(), parcel.ProposedNumber{PrincipalID: "alice", Number: parcelNumber, Carrier: "dhl", ApprovalID: "pending-card", ProposedAt: parcelNow, UpdatedAt: parcelNow})
			if err != nil {
				f.t.Fatal(err)
			}
		},
		"tombstoned": func(f *parcelFixture) {
			stopped, _, err := f.store.UpsertParcel(f.t.Context(), parcel.Parcel{
				PrincipalID: "alice", Carrier: "dhl", Number: parcelNumber, Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: parcelNow, UpdatedAt: parcelNow,
			})
			if err != nil {
				f.t.Fatal(err)
			}
			if _, _, err := f.store.StopParcel(f.t.Context(), "alice", stopped.ID, parcelNow); err != nil {
				f.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newParcelFixture(t, true)
			prepare(f)
			f.scan(parcelAction)
			if len(f.offers.proposals) != 0 {
				t.Fatalf("offered %+v", f.offers.proposals)
			}
		})
	}

	t.Run("expired card is offered once more", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, true)
		err := f.store.RecordProposal(f.t.Context(), parcel.ProposedNumber{PrincipalID: "alice", Number: parcelNumber, Carrier: "dhl", ApprovalID: "gone-card", ProposedAt: parcelNow, UpdatedAt: parcelNow})
		if err != nil {
			t.Fatal(err)
		}
		f.scan(parcelAction)
		if len(f.offers.proposals) != 1 {
			t.Fatalf("offered %+v", f.offers.proposals)
		}
	})
}

func TestChatGuidancePointsADeliveryAtTheParcelToolOnlyWhileParcelsAreOn(t *testing.T) {
	t.Parallel()
	on, err := newParcelFixture(t, true).service.Contribute(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	off, err := newParcelFixture(t, false).service.Contribute(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on.WriteGuidance, "parcel_track for a delivery") || strings.Contains(on.WriteGuidance, "watch_propose") {
		t.Fatalf("guidance with parcels on: %s", on.WriteGuidance)
	}
	if !strings.Contains(off.WriteGuidance, "watch_propose for a delivery") || strings.Contains(off.WriteGuidance, "parcel_track") {
		t.Fatalf("guidance with parcels off: %s", off.WriteGuidance)
	}
}

func TestInboxCheckFollowsANumberNoIntegrationTracksThroughMailWithoutACard(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, true)
	f.scan(`{"type":"parcel","number":"3S ABCD-12345678","carrier":"postnl"},{"type":"parcel","number":"TRACK0001234","carrier":""}`)
	if len(f.offers.proposals) != 0 {
		t.Fatalf("offered %+v", f.offers.proposals)
	}
	followed, err := f.store.ListParcels(t.Context(), "alice")
	if err != nil || len(followed) != 2 {
		t.Fatalf("parcels = %+v %v", followed, err)
	}
	byNumber := map[string]parcel.Parcel{}
	for _, held := range followed {
		byNumber[held.Number] = held
	}
	if got := byNumber["3SABCD12345678"]; got.Carrier != "postnl" || got.Source != parcel.SourceMail || got.Link == "" || got.Title != "Parcel on its way" || got.Sender != "Example Carrier" {
		t.Fatalf("followed = %+v", got)
	}
	if got := byNumber["TRACK0001234"]; got.Carrier != "other" || got.Link != "" {
		t.Fatalf("followed = %+v", got)
	}

	f.scan(`{"type":"parcel","number":"3S ABCD-12345678","carrier":"postnl"}`)
	if again, _ := f.store.ListParcels(t.Context(), "alice"); len(again) != 2 {
		t.Fatalf("a second notice made %+v", again)
	}
}

func TestInboxCheckOffersANumberOnceWhateverTheNoticesSay(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, true)
	f.scan(parcelAction + "," + parcelAction + `,{"type":"parcel","number":"1234567890"}`)
	if got := f.tools(); len(got) != 1 {
		t.Fatalf("offered %v", got)
	}
}

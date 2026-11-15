package parcel_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
)

const (
	mailbox      = "alice@home.example"
	postnlNumber = "3SABCD12345678"
	dhlNumber    = "1234567890"
	orderRef     = "123-4567890-1234567"
)

var mailNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

type grants struct{ sim *googlesim.Simulator }

func (g grants) Accounts(_ context.Context, principalID string, _ google.Product) ([]google.Account, error) {
	if principalID != "alice" {
		return nil, nil
	}
	return []google.Account{{ID: "id-" + mailbox, Address: mailbox}}, nil
}

func (g grants) AccessToken(context.Context, string, string, google.Product, bool) (string, error) {
	return g.sim.Access(mailbox, googlesim.Grant{Mail: true}), nil
}

type scriptedExtractor struct {
	replies []string
	prompts []string
	kind    string
}

func (e *scriptedExtractor) RunExtract(_ context.Context, _, message string) (string, string, error) {
	e.prompts = append(e.prompts, message)
	if len(e.replies) == 0 {
		return `{"messages":[]}`, e.kind, nil
	}
	reply := e.replies[0]
	e.replies = e.replies[1:]
	return reply, e.kind, nil
}

type readable struct{ off bool }

func (r *readable) MailReadable(context.Context, string) (bool, error) { return !r.off, nil }

type view struct{}

func (view) ParcelView(context.Context, string) (int64, int64, bool, error) { return 101, 0, true, nil }

type deliverer struct{ sent int }

func (d *deliverer) SendText(context.Context, int64, string) (int64, error) { return 1, nil }
func (d *deliverer) SendProposal(context.Context, int64, approval.Shown, approval.Buttons) (int64, approval.WidgetKind, error) {
	d.sent++
	return int64(d.sent), approval.NotWidget, nil
}
func (d *deliverer) EditText(context.Context, int64, int64, approval.Shown) error { return nil }
func (d *deliverer) EditProposal(context.Context, int64, int64, approval.Shown, approval.Buttons) error {
	return nil
}
func (d *deliverer) SendPreview(context.Context, int64, int64, string) error { return nil }
func (d *deliverer) SendDirectActions(context.Context, int64, approval.DirectActions) (int64, error) {
	return 1, nil
}
func (d *deliverer) EditDirectActions(context.Context, int64, int64, approval.DirectActions) error {
	return nil
}

type executor struct{ tools []string }

func (e executor) ActionTools() []string { return e.tools }
func (executor) ExecuteApproved(context.Context, approval.Record) (approval.Outcome, error) {
	return approval.Outcome{Text: "Done."}, nil
}

type harness struct {
	t         *testing.T
	store     *sqlite.Store
	sim       *googlesim.Simulator
	source    *parcel.MailSource
	extractor *scriptedExtractor
	mail      *readable
	approvals *approval.Service
	now       time.Time
	sent      int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sim := googlesim.New(t)
	sim.ListNewestFirst()
	service, err := gmail.NewService(gmail.Config{APIBaseURL: sim.URL(), HTTPClient: sim.Client()}, grants{sim: sim}, logger)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, store: store, sim: sim, extractor: &scriptedExtractor{}, mail: &readable{}, now: mailNow}
	approvals, err := approval.NewService(approval.Config{Now: func() time.Time { return h.now }}, store, &deliverer{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := approvals.RegisterExecutor(executor{tools: []string{parcel.ToolTrack, "chat_note"}}); err != nil {
		t.Fatal(err)
	}
	h.approvals = approvals
	carriers := parcel.DefaultCarriers()
	h.source = &parcel.MailSource{
		Repository: store, Mailboxes: service, Mail: h.mail, Extractor: h.extractor, Carriers: carriers,
		Proposals: parcel.Proposals{Repository: store, Approvals: store, Carriers: carriers, Now: func() time.Time { return h.now }},
		Offers:    approvals, Destination: view{}, Location: time.UTC, Logger: logger,
	}
	return h
}

// deliver puts a shipping message in the mailbox, received the given time before now.
func (h *harness) deliver(id, from, subject, body string, age time.Duration) {
	h.sim.AddMailbox(mailbox, googlesim.Message{
		ID: id, ThreadID: "t-" + id, From: from, To: mailbox, Subject: subject, Snippet: subject,
		Received: h.now.Add(-age), PlainText: body,
	})
}

func (h *harness) run() parcel.MailResult {
	h.t.Helper()
	result, err := h.source.Run(h.t.Context(), "alice", h.now)
	if err != nil {
		h.t.Fatalf("run: %v", err)
	}
	return result
}

func (h *harness) parcels() []parcel.Parcel {
	h.t.Helper()
	found, err := h.store.ListParcels(h.t.Context(), "alice")
	if err != nil {
		h.t.Fatal(err)
	}
	return found
}

type item struct {
	MessageID      string `json:"message_id"`
	Carrier        string `json:"carrier,omitempty"`
	Number         string `json:"number,omitempty"`
	OrderReference string `json:"order_reference,omitempty"`
	Title          string `json:"title,omitempty"`
	Status         string `json:"status,omitempty"`
	ExpectedDate   string `json:"expected_date,omitempty"`
	WindowFrom     string `json:"window_from,omitempty"`
	WindowTo       string `json:"window_to,omitempty"`
	PickupPoint    string `json:"pickup_point,omitempty"`
}

func reply(items ...item) string {
	encoded, err := json.Marshal(map[string]any{"messages": items})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func (h *harness) expect(replies ...string) {
	h.extractor.replies = append(h.extractor.replies, replies...)
}

func TestMailCreatesAParcelAndALaterMailMovesItsStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "Example Post <ship@postnl.nl>", "Your parcel is on its way", "Parcel 3S ABCD-12345678. Track it at https://tracking.example-redirect.test/c/abc and read more.", 3*time.Hour)
	h.expect(reply(item{MessageID: "m1", Carrier: "postnl", Number: "3s abcd-12345678", Title: "Kettle", Status: "in_transit", ExpectedDate: "2026-11-11"}))

	first := h.run()
	if first.Read != 1 || len(first.Parcels) != 1 || !first.Parcels[0].New {
		t.Fatalf("first run = %+v", first)
	}
	if strings.Contains(h.extractor.prompts[0], "tracking.example-redirect.test") || !strings.Contains(h.extractor.prompts[0], "ship@postnl.nl") {
		t.Fatalf("the prompt keeps a mail link or loses the sender:\n%s", h.extractor.prompts[0])
	}
	created := h.parcels()
	want := "https://postnl.nl/tracktrace/?B=" + postnlNumber + "&D=NL&T=C"
	if len(created) != 1 || created[0].Number != postnlNumber || created[0].Carrier != "postnl" || created[0].Source != parcel.SourceMail ||
		created[0].Status != parcel.StatusInTransit || created[0].Link != want || created[0].Sender != "postnl.nl" || created[0].Title != "Kettle" ||
		created[0].WindowFrom.IsZero() || !created[0].WindowTo.After(created[0].WindowFrom) {
		t.Fatalf("created = %+v", created)
	}

	h.now = h.now.Add(2 * time.Hour)
	h.deliver("m-2", "ship@postnl.nl", "Out for delivery today", "Parcel "+postnlNumber+" arrives today between 14:00 and 16:00.", time.Minute)
	h.expect(reply(item{MessageID: "m1", Number: postnlNumber, Status: "out_for_delivery", WindowFrom: "2026-11-10T14:00", WindowTo: "2026-11-10T16:00"}))
	second := h.run()
	if len(second.Parcels) != 1 || second.Parcels[0].New {
		t.Fatalf("second run = %+v", second)
	}
	updated := h.parcels()
	if len(updated) != 1 || updated[0].ID != created[0].ID || updated[0].Status != parcel.StatusOutForDelivery || updated[0].Title != "Kettle" ||
		updated[0].WindowFrom.Hour() != 14 || updated[0].WindowTo.Hour() != 16 {
		t.Fatalf("updated = %+v", updated)
	}
}

func TestMailMergesByOrderReferenceAndFollowsTheHandOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "shipment-tracking@amazon.nl", "Your order has shipped", "Order "+orderRef, 5*time.Hour)
	h.expect(reply(item{MessageID: "m1", OrderReference: orderRef, Title: "Desk lamp", Status: "registered"}))
	h.run()
	shop := h.parcels()
	if len(shop) != 1 || shop[0].Carrier != parcel.ShopID || shop[0].Number != "" || shop[0].OrderRef != orderRef || shop[0].Sender != "amazon.nl" ||
		shop[0].Link != "https://www.amazon.nl/gp/your-account/order-details?orderID="+orderRef {
		t.Fatalf("shop parcel = %+v", shop)
	}

	h.deliver("m-2", "auto-confirm@mail.amazon.nl", "Delivery update", "Order "+orderRef+" is delayed", 4*time.Hour)
	h.expect(reply(item{MessageID: "m1", OrderReference: orderRef, Status: "problem"}))
	h.run()
	if merged := h.parcels(); len(merged) != 1 || merged[0].ID != shop[0].ID || merged[0].Status != parcel.StatusProblem {
		t.Fatalf("an update to the order made %+v", merged)
	}

	h.deliver("m-3", "shipment-tracking@amazon.nl", "Your order was shipped", "Order "+orderRef+" is with DHL, tracking "+dhlNumber+".", 3*time.Hour)
	h.expect(reply(item{MessageID: "m1", Carrier: "DHL Express", Number: dhlNumber, OrderReference: orderRef, Status: "in_transit"}))
	h.run()
	handed := h.parcels()
	if len(handed) != 1 || handed[0].ID != shop[0].ID || handed[0].Carrier != "dhl" || handed[0].Number != dhlNumber || handed[0].OrderRef != orderRef ||
		handed[0].Status != parcel.StatusInTransit || !strings.HasPrefix(handed[0].Link, "https://www.dhl.com/") || !strings.HasSuffix(handed[0].Link, dhlNumber) {
		t.Fatalf("hand-off made %+v", handed)
	}

	h.deliver("m-4", "noreply@dhl.com", "Delivered", "Your package "+dhlNumber+" was delivered.", time.Hour)
	h.expect(reply(item{MessageID: "m1", Number: dhlNumber, Status: "delivered"}))
	h.run()
	if delivered := h.parcels(); len(delivered) != 1 || delivered[0].Status != parcel.StatusDelivered || delivered[0].OrderRef != orderRef {
		t.Fatalf("the carrier's own mail made %+v", delivered)
	}
}

func TestMailRejectsMalformedReplies(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@example-shop.test", "Your parcel was shipped", "Number "+postnlNumber, time.Hour)

	h.expect(`not json`)
	if failed := h.run(); !failed.Failed || len(h.parcels()) != 0 {
		t.Fatalf("malformed JSON gave %+v and %v", failed, h.parcels())
	}
	cursor, found, err := h.store.LoadMailCursor(t.Context(), "alice")
	if err != nil || !found || cursor.AfterEpoch != mailNow.Add(-parcel.MailFirstLookBack).Unix() {
		t.Fatalf("a failed extraction moved the cursor: %+v %v %v", cursor, found, err)
	}

	h.expect(reply(item{MessageID: "m1", Number: postnlNumber, Status: "in_transit"}) + ` trailing`)
	if failed := h.run(); !failed.Failed {
		t.Fatalf("trailing text was accepted: %+v", failed)
	}
	if len(h.extractor.prompts) != 2 {
		t.Fatalf("the failed batch was tried %d times", len(h.extractor.prompts))
	}
	h.run()
	if len(h.extractor.prompts) != 2 {
		t.Fatalf("a batch that failed twice was tried again: %d runs", len(h.extractor.prompts))
	}
	if len(h.parcels()) != 0 {
		t.Fatalf("parcels = %+v", h.parcels())
	}
}

func TestMailDropsWhatCodeCannotValidate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@example-shop.test", "Your parcel was shipped", "Ref 12", 5*time.Hour)
	h.deliver("m-2", "ship@example-shop.test", "Your parcel was shipped", "Ref ABC DEF!!GHI", 4*time.Hour)
	h.deliver("m-3", "ship@example-shop.test", "Your parcel was shipped", "Ref TRACK0001234, see shop.example/track", 3*time.Hour)
	h.deliver("m-4", "ship@example-shop.test", "Your parcel was shipped", "order bad order!", 2*time.Hour)
	long := strings.Repeat("x", 200)
	h.expect(reply(
		item{MessageID: "m1", Number: "12"},
		item{MessageID: "m2", Number: "ABC DEF!!GHI", Title: "No number"},
		item{MessageID: "m99", Number: postnlNumber},
		item{MessageID: "m3", Number: "TRACK0001234", Carrier: "Odd Carrier", Title: "Visit https://evil.test/x now " + long, Status: "teleported", PickupPoint: long},
		item{MessageID: "m4", OrderReference: "bad order!"},
	))
	h.run()
	found := h.parcels()
	if len(found) != 1 {
		t.Fatalf("parcels = %+v", found)
	}
	got := found[0]
	if got.Number != "TRACK0001234" || got.Carrier != "other" || got.Status != parcel.StatusUnknown || got.Link != "" ||
		strings.Contains(got.Title, "evil") || len([]rune(got.Title)) > 60 || len([]rune(got.PickupPoint)) > 60 || len([]rune(got.Sender)) > 40 {
		t.Fatalf("parcel = %+v", got)
	}
}

func TestMailNeverTakesALinkFromTheReply(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@postnl.nl", "Your parcel", "Number "+postnlNumber, time.Hour)
	h.expect(`{"messages":[{"message_id":"m1","number":"` + postnlNumber + `","link":"https://evil.test/track"}]}`)
	if failed := h.run(); !failed.Failed || len(h.parcels()) != 0 {
		t.Fatalf("a reply with a link was accepted: %+v %+v", failed, h.parcels())
	}
	h.expect(reply(item{MessageID: "m1", Number: postnlNumber, Title: "https://evil.test/track"}))
	h.run()
	if found := h.parcels(); len(found) != 1 || strings.Contains(found[0].Link, "evil") || strings.Contains(found[0].Title, "evil") || found[0].Link == "" {
		t.Fatalf("parcels = %+v", found)
	}
}

func TestMailNeverRereadsAMessage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@postnl.nl", "Your parcel", "Number "+postnlNumber, 2*time.Hour)
	h.expect(reply(item{MessageID: "m1", Number: postnlNumber, Status: "in_transit"}))
	h.run()
	h.now = h.now.Add(30 * time.Minute)
	if again := h.run(); again.Read != 0 || len(h.extractor.prompts) != 1 {
		t.Fatalf("second run = %+v after %d extractions", again, len(h.extractor.prompts))
	}
	var queries []string
	for _, request := range h.sim.Requests() {
		if request.Path == "/gmail/v1/users/me/messages" {
			queries = append(queries, request.Query.Get("q"))
		}
	}
	all := strings.Join(queries, "\n")
	cursor, _, _ := h.store.LoadMailCursor(t.Context(), "alice")
	if !strings.Contains(all, "after:") || !strings.Contains(all, "from:postnl.nl") || !strings.Contains(all, "subject:bezorging") || cursor.AfterEpoch != mailNow.Add(-2*time.Hour).Unix() {
		t.Fatalf("queries %q, cursor %+v", queries, cursor)
	}
}

func TestMailCapsAMessagesPerRunAndPerDay(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for index := range 30 {
		h.deliver("a-"+strconv.Itoa(index), "ship@example-shop.test", "Your parcel", "text", 10*time.Hour+time.Duration(30-index)*time.Minute)
	}
	reads := func() int { return len(h.extractor.prompts) }
	for want := 1; want <= 3; want++ {
		if result := h.run(); result.Read != 10 || reads() != want {
			t.Fatalf("run %d = %+v after %d extractions", want, result, reads())
		}
	}
	if strings.Count(h.extractor.prompts[0], "\nMessage m") != 10 {
		t.Fatalf("a batch held other than 10 messages:\n%s", h.extractor.prompts[0])
	}
	for index := range 15 {
		h.deliver("b-"+strconv.Itoa(index), "ship@example-shop.test", "Your parcel", "text", time.Duration(15-index)*time.Minute)
	}
	if result := h.run(); result.Read != 10 || reads() != 4 {
		t.Fatalf("fourth run = %+v", result)
	}
	if result := h.run(); !result.Capped || result.Read != 0 || reads() != 4 {
		t.Fatalf("a run past the day's 40 = %+v", result)
	}
	h.now = h.now.Add(25 * time.Hour)
	if result := h.run(); result.Read != 5 || reads() != 5 {
		t.Fatalf("the next day's run = %+v", result)
	}
}

func TestMailDoesNothingWhileMailCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@postnl.nl", "Your parcel", "text", time.Hour)
	h.mail.off = true
	if result := h.run(); !result.Unreadable || len(h.sim.Requests()) != 0 || len(h.extractor.prompts) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func (h *harness) pending(principalID string) []approval.Record {
	h.t.Helper()
	records, err := h.store.PendingApprovals(h.t.Context(), principalID, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return records
}

func (h *harness) dhlMail() {
	h.deliver("m-1", "noreply@dhl.com", "Your package is on its way", "Number "+dhlNumber, time.Hour)
	h.expect(reply(item{MessageID: "m1", Number: dhlNumber, Status: "in_transit", Title: "Headphones"}))
}

func TestMailOffersATrackableNumberOnOneCard(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "noreply@dhl.com", "Your package is on its way", "Number "+dhlNumber, time.Hour)
	h.deliver("m-2", "ship@postnl.nl", "Your parcel", "Number "+postnlNumber, 2*time.Hour)
	h.expect(reply(item{MessageID: "m2", Number: dhlNumber, Status: "in_transit"}, item{MessageID: "m1", Number: postnlNumber, Status: "in_transit"}))

	result := h.run()
	records := h.pending("alice")
	if result.Proposed != 1 || len(records) != 1 || records[0].Tool != parcel.ToolTrack || records[0].Heading != "Parcels from your mail" ||
		!strings.Contains(records[0].Summary, "outside content") || !strings.Contains(string(records[0].Arguments), `"carrier":"dhl"`) ||
		records[0].ExpiresAt.Sub(records[0].CreatedAt) != parcel.MailProposalLifetime {
		t.Fatalf("result %+v, records %+v", result, records)
	}
	proposed, found, err := h.store.LoadProposal(t.Context(), "alice", dhlNumber)
	if err != nil || !found || proposed.ApprovalID != records[0].ID || proposed.Offers != 1 {
		t.Fatalf("proposal record = %+v %v %v", proposed, found, err)
	}
	if again := h.run(); again.Proposed != 0 || len(h.pending("alice")) != 1 {
		t.Fatalf("a waiting proposal was offered again: %+v", again)
	}
}

func TestNoProposalForANumberAlreadyTrackedTombstonedOrDeclined(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(*harness){
		"tracked": func(h *harness) {
			err := h.store.SaveAddedNumber(h.t.Context(), parcel.AddedNumber{PrincipalID: "alice", Carrier: "dhl", Number: dhlNumber, Domain: "dhl", EntryID: "entry", AddedAt: mailNow})
			if err != nil {
				h.t.Fatal(err)
			}
		},
		"declined": func(h *harness) {
			if err := h.store.SetProposalOutcome(h.t.Context(), "alice", dhlNumber, parcel.OutcomeDeclined, mailNow); err != nil {
				h.t.Fatal(err)
			}
		},
		"tombstoned": func(h *harness) {
			stopped, _, err := h.store.UpsertParcel(h.t.Context(), parcel.Parcel{
				PrincipalID: "alice", Carrier: "dhl", Number: dhlNumber, Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: mailNow, UpdatedAt: mailNow,
			})
			if err != nil {
				h.t.Fatal(err)
			}
			if _, _, err := h.store.StopParcel(h.t.Context(), "alice", stopped.ID, mailNow); err != nil {
				h.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			prepare(h)
			h.dhlMail()
			result := h.run()
			if result.Proposed != 0 || len(h.pending("alice")) != 0 {
				t.Fatalf("result %+v, pending %+v", result, h.pending("alice"))
			}
			if name == "tombstoned" {
				if found := h.parcels(); len(found) != 1 || !found[0].Stopped() || found[0].Status != parcel.StatusInTransit {
					t.Fatalf("a tombstoned parcel came back: %+v", found)
				}
			}
		})
	}
}

func TestProposalsTreatAnExpiredCardAsNotWaiting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.dhlMail()
	h.run()
	h.now = h.now.Add(parcel.MailProposalLifetime + time.Hour)
	again := h.run()
	records := h.pending("alice")
	proposed, _, _ := h.store.LoadProposal(t.Context(), "alice", dhlNumber)
	if again.Proposed != 1 || len(records) != 1 || proposed.Offers != 2 {
		t.Fatalf("result %+v, pending %d, proposal %+v", again, len(records), proposed)
	}
}

func TestMailLeavesAChatProposalWaitingInTheSameViewAndOffersLater(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	chat, err := h.approvals.Propose(t.Context(), "alice", approval.Proposal{Tool: "chat_note", Arguments: json.RawMessage(`{"note":"x"}`), Summary: "Save a note"})
	if err != nil {
		t.Fatal(err)
	}
	h.dhlMail()
	result := h.run()
	if !result.Waiting || result.Proposed != 0 || len(result.Parcels) != 1 {
		t.Fatalf("result = %+v", result)
	}
	records := h.pending("alice")
	if len(records) != 1 || records[0].ID != chat.ID {
		t.Fatalf("the chat proposal was replaced: %+v", records)
	}
	if _, found, _ := h.store.LoadProposal(t.Context(), "alice", dhlNumber); found {
		t.Fatal("a proposal that was never offered was recorded")
	}

	h.now = h.now.Add(time.Hour)
	if _, err := h.approvals.Press(t.Context(), "alice", approval.ActionReject, chat.ID); err != nil {
		t.Fatal(err)
	}
	later := h.run()
	if later.Proposed != 1 || later.Read != 0 {
		t.Fatalf("the later run = %+v", later)
	}
}

func TestMailOfAnotherPrincipalIsNeverRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@postnl.nl", "Your parcel", "text", time.Hour)
	_, err := h.source.Run(t.Context(), "bob", h.now)
	if !errors.Is(err, google.ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if found, _ := h.store.ListParcels(t.Context(), "bob"); len(found) != 0 {
		t.Fatalf("parcels = %+v", found)
	}
}

func TestAnExtractionTurnThatDoesNotAnswerCostsNoMessageItsAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@postnl.nl", "Your parcel", "Number "+postnlNumber, time.Hour)
	h.extractor.kind = "timeout"
	for range 5 {
		if result := h.run(); !result.Failed || result.Read != 0 {
			t.Fatalf("result = %+v", result)
		}
	}
	if _, found, _ := h.store.LoadMailCursor(t.Context(), "alice"); found {
		t.Fatal("an outage wrote the cursor")
	}
	h.extractor.kind = ""
	h.expect(reply(item{MessageID: "m1", Number: postnlNumber, Status: "in_transit"}))
	if result := h.run(); result.Read != 1 || len(h.parcels()) != 1 {
		t.Fatalf("after the outage = %+v", result)
	}
}

type flakyMailboxes struct {
	parcel.Mailboxes
	fail map[string]error
}

func (f flakyMailboxes) ReadMail(ctx context.Context, principalID string, ref gmail.MailRef) (gmail.Mail, error) {
	if err := f.fail[ref.ID]; err != nil {
		return gmail.Mail{}, err
	}
	return f.Mailboxes.ReadMail(ctx, principalID, ref)
}

func TestAMessageThatCannotBeReadDoesNotStopTheRestButAnAccountErrorDoes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@postnl.nl", "Your parcel", "Number "+postnlNumber, 3*time.Hour)
	h.deliver("m-2", "noreply@dhl.com", "Your parcel", "Number "+dhlNumber, 2*time.Hour)
	flaky := flakyMailboxes{Mailboxes: h.source.Mailboxes, fail: map[string]error{"m-1": errors.New("boom")}}
	h.source.Mailboxes = flaky
	h.expect(reply(item{MessageID: "m1", Number: dhlNumber, Status: "in_transit"}))
	if result := h.run(); result.Read != 1 || len(h.parcels()) != 1 {
		t.Fatalf("result = %+v", result)
	}
	cursor, _, _ := h.store.LoadMailCursor(t.Context(), "alice")
	if cursor.AfterEpoch >= mailNow.Add(-3*time.Hour).Unix() {
		t.Fatalf("the cursor passed the unreadable message: %+v", cursor)
	}
	flaky.fail["m-1"] = nil
	h.expect(reply(item{MessageID: "m1", Number: postnlNumber, Status: "in_transit"}))
	if result := h.run(); result.Read != 1 || len(h.parcels()) != 2 {
		t.Fatalf("the unreadable message was not found again: %+v", result)
	}

	h.deliver("m-3", "ship@postnl.nl", "Your parcel", "Number "+postnlNumber, time.Minute)
	flaky.fail["m-3"] = google.ErrNotConnected
	if _, err := h.source.Run(t.Context(), "alice", h.now); !errors.Is(err, google.ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
}

func TestAHostileMessageCostsOnlyItself(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("a", "ship@postnl.nl", "Your parcel", "Number "+postnlNumber, 3*time.Hour)
	h.deliver("b", "ship@example-shop.test", "Your parcel", "Number HOSTILE0001", 2*time.Hour)
	h.deliver("c", "noreply@dhl.com", "Your parcel", "Number "+dhlNumber, time.Hour)
	h.expect(`broken`,
		reply(item{MessageID: "m1", Number: postnlNumber}),
		`broken`,
		reply(item{MessageID: "m1", Number: dhlNumber}))
	for range 4 {
		h.run()
	}
	numbers := map[string]bool{}
	for _, found := range h.parcels() {
		numbers[found.Number] = true
	}
	if len(h.extractor.prompts) != 4 || !numbers[postnlNumber] || !numbers[dhlNumber] || len(numbers) != 2 {
		t.Fatalf("%d extractions, parcels %v", len(h.extractor.prompts), numbers)
	}
	for _, single := range h.extractor.prompts[1:] {
		if strings.Contains(single, "Message m2") {
			t.Fatalf("a retry held more than one message:\n%s", single)
		}
	}
}

func TestMailKeepsAtMostThreeDistinctItemsPerMessage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@example-shop.test", "Your parcel", "Numbers TRACK0000001 TRACK0000002 TRACK0000003 TRACK0000004", time.Hour)
	var items []item
	for _, number := range []string{"TRACK0000001", "TRACK0000001", "TRACK0000002", "TRACK0000003", "TRACK0000004"} {
		items = append(items, item{MessageID: "m1", Number: number})
	}
	h.expect(reply(items...))
	h.run()
	if found := h.parcels(); len(found) != 2 {
		t.Fatalf("parcels = %+v", found)
	}
}

func TestMailDropsAnItemWhoseNumberIsNotInTheMessage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@example-shop.test", "Your parcel", "Nothing useful here, order "+orderRef, time.Hour)
	h.expect(reply(item{MessageID: "m1", Number: "TRACK9999999"}, item{MessageID: "m1", OrderReference: "999-0000000-0000000"}))
	h.run()
	if found := h.parcels(); len(found) != 0 {
		t.Fatalf("parcels = %+v", found)
	}
}

func TestMailPrefersTheNumbersOwnCarrierAndStripsBareDomains(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("m-1", "ship@example-shop.test", "Your parcel", "Number "+dhlNumber, time.Hour)
	h.expect(reply(item{MessageID: "m1", Carrier: "postnl", Number: dhlNumber, Title: "Kettle from shop.example/x", PickupPoint: "Point at www.pickup.test/a"}))
	h.run()
	found := h.parcels()
	if len(found) != 1 || found[0].Carrier != "dhl" || strings.Contains(found[0].Title, "example") || strings.Contains(found[0].PickupPoint, "pickup.test") {
		t.Fatalf("parcels = %+v", found)
	}
	if !strings.Contains(h.extractor.prompts[0], "postnl, dhl_nl, dhl, dpd") || strings.Contains(h.extractor.prompts[0], "such as postnl or dhl") {
		t.Fatalf("the prompt does not list the carriers:\n%s", h.extractor.prompts[0])
	}
}

func TestMailSearchesStayWithinGmailsQueryLimitAndSkipShopAdverts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	queries := h.source.Queries(1<<40 + 12345)
	if len(queries) != 2 {
		t.Fatalf("queries = %q", queries)
	}
	for _, query := range queries {
		if len([]rune(query)) > 400 {
			t.Fatalf("a query of %d runes: %s", len([]rune(query)), query)
		}
	}
	if strings.Contains(queries[0], "amazon") || !strings.Contains(queries[0], "from:postnl.nl") {
		t.Fatalf("sender query = %s", queries[0])
	}

	h.deliver("m-1", "deals@amazon.nl", "Deals for you", "Number "+postnlNumber, time.Hour)
	if result := h.run(); result.Read != 0 || len(h.extractor.prompts) != 0 {
		t.Fatalf("an advert was read: %+v", result)
	}
}

func TestMailWalksBackThroughAFloodOldestFirstAndReportsTruncation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for index := range 70 {
		h.deliver("f-"+strconv.Itoa(index), "ship@example-shop.test", "Your parcel", "Number FLOOD"+strconv.Itoa(1000000+index), time.Duration(100-index)*time.Minute)
	}
	for round := range 3 {
		before := len(h.extractor.prompts)
		result := h.run()
		if result.Read != 10 || result.Truncated || len(h.extractor.prompts) != before+1 {
			t.Fatalf("round %d = %+v", round, result)
		}
		prompt := h.extractor.prompts[before]
		for index := range 10 {
			if want := "Number FLOOD" + strconv.Itoa(1000000+round*10+index); !strings.Contains(prompt, want) {
				t.Fatalf("round %d lacks %q:\n%s", round, want, prompt)
			}
		}
	}

	big := newHarness(t)
	for index := range 200 {
		big.deliver("g-"+strconv.Itoa(index), "ship@example-shop.test", "Your parcel", "text", time.Duration(300-index)*time.Minute)
	}
	if result := big.run(); !result.Truncated {
		t.Fatalf("a flood past the walks was not reported: %+v", result)
	}
}

type fakeAccounts map[string]bool

func (a fakeAccounts) AccountCarriers(string) map[string]bool { return a }

// An account whose packages lack numbers holds back its own carrier's mail, which could only make a
// second card, and nothing else: not another carrier's, even one whose numbers look the same.
func TestAnAccountHoldsBackOnlyItsOwnCarriersMail(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.source.Accounts = fakeAccounts{"postnl": false}
	const twin = "3SABCD12345679"
	h.deliver("m-1", "noreply@dhlparcel.nl", "Your parcel", "Parcel "+twin, 2*time.Hour)
	h.deliver("m-2", "ship@postnl.nl", "Your parcel", "Parcel "+postnlNumber, time.Hour)
	h.expect(reply(item{MessageID: "m1", Number: twin, Status: "in_transit"}, item{MessageID: "m2", Number: postnlNumber, Status: "in_transit"}))
	h.run()
	if found := h.parcels(); len(found) != 1 || found[0].Number != twin || found[0].Carrier != "dhl_nl" {
		t.Fatalf("parcels = %+v", found)
	}
}

package telegram

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/homesim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// parcelWiring is the application's Parcels wiring over Home Assistant's simulator: Alice has a
// carrier entry assigned that takes numbers, Bob has none of that carrier.
type parcelWiring struct {
	*conversationHarness
	runtime *controlledRuntime
	home    *homesim.Server
}

var parcelAssignments = map[string][]string{"alice": {"entry_dhl_a"}, "bob": {"entry_gls_b"}}

func newParcelWiring(t *testing.T, assignments map[string][]string) *parcelWiring {
	t.Helper()
	return newParcelWiringAt(t, assignments, nil)
}

// newParcelWiringAt is newParcelWiring on the clock, which the loop and Home Assistant's reads
// follow; nil is the wall clock.
func newParcelWiringAt(t *testing.T, assignments map[string][]string, clock *testClock) *parcelWiring {
	t.Helper()
	raw, err := os.ReadFile("../connectors/homeassistant/testdata/parcels/household.json")
	if err != nil {
		t.Fatal(err)
	}
	var seed homesim.Seed
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	seed.Token = "test-token"
	raw, err = json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	home := homesim.New(t, raw, time.Now)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{
		parcels: true, parcelSources: assignments, homeAssistant: home.URL(), providers: []tools.Provider{readingTools{}}, clock: clock,
	})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	for _, principal := range []string{"alice", "bob"} {
		if err := h.switches.SetEnabled(h.ctx, principal, parcel.Switch, true); err != nil {
			t.Fatal(err)
		}
	}
	h.start()
	return &parcelWiring{conversationHarness: h, runtime: runtime, home: home}
}

func (w *parcelWiring) serviceCalls() []string {
	var calls []string
	for _, call := range w.home.Calls() {
		if strings.HasPrefix(call.Tool, "service:") {
			calls = append(calls, call.Tool)
		}
	}
	return calls
}

func (w *parcelWiring) outcome(principalID, number string) parcel.Outcome {
	w.t.Helper()
	proposed, found, err := w.store.LoadProposal(w.ctx, principalID, number)
	if err != nil || !found {
		w.t.Fatalf("proposal of %s = %+v %v %v", number, proposed, found, err)
	}
	return proposed.Outcome
}

// finish answers the turn of a user and waits until the reply is out in their chat.
func (w *parcelWiring) finish(call runtimeCall, updateID, userID int64, reply string) {
	w.t.Helper()
	call.answer <- agentruntime.RunResult{Response: reply}
	w.wantReply(userID, updateID, reply)
	w.waitState(updateID, "delivered")
}

func (w *parcelWiring) startTurn(updateID, userID int64) runtimeCall {
	w.t.Helper()
	w.push(textUpdate(updateID, userID, "follow this parcel"))
	return receive(w.t, w.ctx, w.runtime.calls)
}

func (w *parcelWiring) track(call runtimeCall, number string) string {
	w.t.Helper()
	result := executeTool(w.t, w.ctx, call.request.Tools, parcel.ToolTrack, `{"number":"`+number+`","carrier":"dhl"}`)
	if result.IsError {
		w.t.Fatalf("parcel_track = %+v", result)
	}
	return result.Text
}

func TestParcelTrackTypedByTheUserRunsAtOnceAndUndoRemovesIt(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	call := w.startTurn(1, 101)

	text := w.track(call, "1234567892")
	if !strings.Contains(text, "followed in Home Assistant") {
		t.Fatalf("result = %q", text)
	}
	if calls := w.serviceCalls(); len(calls) != 1 || calls[0] != "service:dhl.track_parcel" {
		t.Fatalf("calls = %v", calls)
	}
	if again := w.track(call, "1234567892"); !strings.Contains(again, "already followed") || len(w.serviceCalls()) != 1 {
		t.Fatalf("repeated call = %q after %v", again, w.serviceCalls())
	}
	if _, found, err := w.store.LoadAddedNumber(w.ctx, "alice", "dhl", "1234567892"); err != nil || !found {
		t.Fatalf("added number = %v %v", found, err)
	}
	if got := w.outcome("alice", "1234567892"); got != parcel.OutcomeAdded {
		t.Fatalf("outcome = %s", got)
	}
	w.finishTurn(call, 1, "following it")

	var id string
	if err := w.db.QueryRowContext(w.ctx, `SELECT id FROM approvals WHERE tool = ? AND mode = 'direct'`, parcel.ToolTrack).Scan(&id); err != nil {
		t.Fatal(err)
	}
	undid, err := w.approvals.Undo(w.ctx, "alice", id)
	if err != nil || !undid.Owned || undid.Answer != "Undone." {
		t.Fatalf("undo = %+v %v", undid, err)
	}
	if calls := w.serviceCalls(); len(calls) != 2 || calls[1] != "service:dhl.untrack_parcel" {
		t.Fatalf("calls after undo = %v", calls)
	}
	if _, found, _ := w.store.LoadAddedNumber(w.ctx, "alice", "dhl", "1234567892"); found {
		t.Fatal("the added number outlives its undo")
	}
	if got := w.outcome("alice", "1234567892"); got != parcel.OutcomeProposed {
		t.Fatalf("outcome after undo = %s", got)
	}
}

func TestParcelTrackAfterOutsideContentIsProposedAndADeclinedNumberIsProposedOnlyWhenAskedAgain(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	call := w.startTurn(1, 101)
	if result := executeTool(t, w.ctx, call.request.Tools, "outside_read", `{}`); result.IsError {
		t.Fatalf("outside_read = %+v", result)
	}
	if text := w.track(call, "1234567894"); !strings.Contains(text, approval.ProposedText) {
		t.Fatalf("result = %q, want a proposal", text)
	}
	if calls := w.serviceCalls(); len(calls) != 0 {
		t.Fatalf("a proposal reached Home Assistant: %v", calls)
	}
	w.finishTurn(call, 1, "proposed")
	bundle := w.nextMessage()
	_, reject := approvalButtons(t, bundle.keyboard)
	w.push(callbackUpdate(2, 101, bundle.messageID, reject))
	w.wantAnswer(2)
	w.nextEdit()
	if got := w.outcome("alice", "1234567894"); got != parcel.OutcomeDeclined {
		t.Fatalf("outcome = %s", got)
	}

	// The user asks for the declined number again in their own message; the conversation read
	// outside content, so it is proposed again rather than refused, and nothing reaches Home
	// Assistant before Confirm.
	second := w.startTurn(3, 101)
	if text := w.track(second, "1234567894"); !strings.Contains(text, approval.ProposedText) {
		t.Fatalf("result after a decline = %q, want a proposal", text)
	}
	if calls := w.serviceCalls(); len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
	if got := w.outcome("alice", "1234567894"); got != parcel.OutcomeProposed {
		t.Fatalf("outcome after asking again = %s", got)
	}
	w.finishTurn(second, 3, "proposed again")
}

func TestParcelListIsUntrustedAndMakesTheNextTrackAProposalAndShowsOnlyTheUsersOwn(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	now := time.Now()
	for principal, title := range map[string]string{"alice": "Garden chair", "bob": "Desk lamp"} {
		if _, _, err := w.store.UpsertParcel(w.ctx, parcel.Parcel{
			PrincipalID: principal, Carrier: "postnl", Number: "3SABCD1234567", Title: title, Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	call := w.startTurn(1, 101)
	listed := executeTool(t, w.ctx, call.request.Tools, parcel.ToolList, `{}`)
	if listed.IsError || !strings.HasPrefix(listed.Text, prompt.ToolResultUntrusted) || !strings.Contains(listed.Text, "Garden chair") || strings.Contains(listed.Text, "Desk lamp") {
		t.Fatalf("alice's list = %+v", listed)
	}
	if text := w.track(call, "1234567893"); !strings.Contains(text, approval.ProposedText) || len(w.serviceCalls()) != 0 {
		t.Fatalf("track after parcel_list = %q with calls %v", text, w.serviceCalls())
	}
	w.finishTurn(call, 1, "listed")
	w.nextMessage()

	other := w.startTurn(2, 202)
	listed = executeTool(t, w.ctx, other.request.Tools, parcel.ToolList, `{}`)
	if !strings.Contains(listed.Text, "Desk lamp") || strings.Contains(listed.Text, "Garden chair") {
		t.Fatalf("bob's list = %+v", listed)
	}
	w.finish(other, 2, 202, "listed")
}

func TestParcelTrackWithoutAnAssignedEntryIsFollowedByMail(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	call := w.startTurn(1, 202)
	if text := w.track(call, "1234567892"); !strings.Contains(text, "followed through their mail") {
		t.Fatalf("result = %q", text)
	}
	if calls := w.serviceCalls(); len(calls) != 0 {
		t.Fatalf("an entry of someone else was written to: %v", calls)
	}
	held, err := w.store.ListParcels(w.ctx, "bob")
	if err != nil || len(held) != 1 || held[0].Source != parcel.SourceMail || held[0].Carrier != "dhl" || held[0].Number != "1234567892" {
		t.Fatalf("bob's parcels = %+v %v", held, err)
	}
	if other, _ := w.store.ListParcels(w.ctx, "alice"); len(other) != 0 {
		t.Fatalf("alice's parcels = %+v", other)
	}
	w.finish(call, 1, 202, "following it")
}

func TestParcelSourcesNamingOneEntryTwiceOrAStrangerStopStartup(t *testing.T) {
	t.Parallel()
	for name, assignments := range map[string]map[string][]string{
		"one entry for two principals":  {"alice": {"entry_dhl_a"}, "bob": {"entry_dhl_a"}},
		"a principal off the allowlist": {"carol": {"entry_dhl_a"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var failure error
			raw, _ := os.ReadFile("../connectors/homeassistant/testdata/parcels/household.json")
			var seed homesim.Seed
			if err := json.Unmarshal(raw, &seed); err != nil {
				t.Fatal(err)
			}
			seed.Token = "test-token"
			raw, _ = json.Marshal(seed)
			home := homesim.New(t, raw, time.Now)
			newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{
				parcels: true, parcelSources: assignments, homeAssistant: home.URL(), buildError: &failure,
			})
			if failure == nil || !strings.Contains(failure.Error(), "HOME_AGENT_PARCEL_SOURCES") {
				t.Fatalf("startup error = %v", failure)
			}
		})
	}
}

func TestParcelsRestoredFromAnExportGetNewCardsUnderTheSilentBaseline(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	now := time.Now().UTC().Truncate(time.Second)
	stored, _, err := w.store.UpsertParcel(w.ctx, parcel.Parcel{
		PrincipalID: "alice", Carrier: "postnl", Number: "3SABCD1234567", Title: "Garden chair", Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.SaveCard(w.ctx, "alice", stored.ID, 77, parcel.CardText); err != nil {
		t.Fatal(err)
	}
	if err := w.store.SaveTracking(w.ctx, parcel.Tracking{PrincipalID: "alice", BaselinedAt: now}); err != nil {
		t.Fatal(err)
	}
	file, err := w.assembly.Exports.Export(w.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(file.Data), "parcel_notices_sent") || strings.Contains(string(file.Data), "parcel_mail_cursors") {
		t.Fatal("a table the export leaves out is in the file")
	}
	if _, err := w.db.ExecContext(w.ctx, `DELETE FROM parcels WHERE principal_id = 'alice'`); err != nil {
		t.Fatal(err)
	}

	report, err := w.assembly.Exports.Restore(w.ctx, "alice", file.Data)
	if err != nil || report.Additions() == 0 {
		t.Fatalf("restore = %+v %v", report, err)
	}
	restored, err := w.store.ListParcels(w.ctx, "alice")
	if err != nil || len(restored) != 1 || restored[0].Number != "3SABCD1234567" || restored[0].CardMessage != 0 {
		t.Fatalf("restored = %+v %v", restored, err)
	}
	if tracking, err := w.store.LoadTracking(w.ctx, "alice"); err != nil || !tracking.BaselinedAt.IsZero() {
		t.Fatalf("tracking after a restore = %+v %v, want a baseline due", tracking, err)
	}
}

func TestParcelStopStopsOnlyTheUsersOwnParcel(t *testing.T) {
	t.Parallel()
	w := newParcelWiring(t, parcelAssignments)
	now := time.Now()
	mine, _, err := w.store.UpsertParcel(w.ctx, parcel.Parcel{
		PrincipalID: "alice", Carrier: "postnl", Number: "3SABCD1234567", Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	theirs, _, err := w.store.UpsertParcel(w.ctx, parcel.Parcel{
		PrincipalID: "bob", Carrier: "postnl", Number: "3SABCD7654321", Source: parcel.SourceMail, Status: parcel.StatusInTransit, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := w.startTurn(1, 101)
	if result := executeTool(t, w.ctx, call.request.Tools, parcel.ToolStop, `{"id":"`+theirs.ID+`"}`); !result.IsError {
		t.Fatalf("stopping another user's parcel = %+v", result)
	}
	if result := executeTool(t, w.ctx, call.request.Tools, parcel.ToolStop, `{"id":"`+mine.ID+`"}`); result.IsError || !strings.Contains(result.Text, "Done") {
		t.Fatalf("stopping the user's own parcel = %+v", result)
	}
	w.finishTurn(call, 1, "stopped")
	if held, _, _ := w.store.LoadParcel(w.ctx, "alice", mine.ID); !held.Stopped() {
		t.Fatal("the parcel was not stopped")
	}
	if held, _, _ := w.store.LoadParcel(w.ctx, "bob", theirs.ID); held.Stopped() {
		t.Fatal("another user's parcel was stopped")
	}
}

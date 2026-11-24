package homeassistant_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/homeassistant"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/homesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

// canaries are the address data the fixtures put in the attributes the source must never read.
var canaries = []string{"Exampleville", "Jane Placeholder", "Example Street", "Left the depot"}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type parcelFixture struct {
	t       *testing.T
	sim     *homesim.Server
	service *homeassistant.Service
	source  *homeassistant.ParcelSource
	store   *sqlite.Store
	clock   *clock
	logs    *lockedBuffer
}

func parcelSeed(t *testing.T, change func(*homesim.Seed)) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "parcels", "household.json"))
	if err != nil {
		t.Fatal(err)
	}
	if change == nil {
		return raw
	}
	var seed homesim.Seed
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	change(&seed)
	changed, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

var parcelAssignments = map[string][]string{
	"alice": {"entry_postnl_a", "entry_dhlnl_a", "entry_dhl_a"},
	"bob":   {"entry_gls_b", "entry_dhl_b"},
}

func newParcelFixture(t *testing.T, change func(*homesim.Seed)) *parcelFixture {
	t.Helper()
	f := &parcelFixture{t: t, clock: &clock{now: start}, logs: &lockedBuffer{}}
	f.sim = homesim.New(t, parcelSeed(t, change), f.clock.Now)
	f.store = testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = f.store.Close() })
	if err := f.store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(f.logs, nil))
	var err error
	f.service, err = homeassistant.NewService(homeassistant.Config{Token: f.sim.Token(), BaseURL: f.sim.URL(), Now: f.clock.Now}, simkit.AlwaysOn{}, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	f.source, err = f.service.NewParcelSource(parcelAssignments, f.store)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *parcelFixture) read(principalID string) parcel.Reading {
	f.t.Helper()
	reading, err := f.source.Read(f.t.Context(), principalID)
	if err != nil {
		f.t.Fatal(err)
	}
	return reading
}

func byKey(reading parcel.Reading) map[string]parcel.Parcel {
	parcels := map[string]parcel.Parcel{}
	for _, item := range reading.Parcels {
		parcels[item.SourceKey] = item
	}
	return parcels
}

func (f *parcelFixture) serviceCalls() []homesim.Call {
	var calls []homesim.Call
	for _, call := range f.sim.Calls() {
		if strings.HasPrefix(call.Tool, "service:") {
			calls = append(calls, call)
		}
	}
	return calls
}

func moment(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed.UTC()
}

func TestAccountAndNormalisedShapesBecomeParcels(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	reading := f.read("alice")
	if reading.Stale {
		t.Fatal("a clean read is stale")
	}
	got := byKey(reading)
	if len(got) != 7 || len(reading.Parcels) != 7 {
		t.Fatalf("parcels = %+v", reading.Parcels)
	}

	enroute := got["entry_postnl_a:pk-enroute-1"]
	if enroute.Carrier != "postnl" || enroute.Source != parcel.SourceHomeAssistant || enroute.PrincipalID != "alice" ||
		enroute.Title != "Example order" || enroute.Status != parcel.StatusInTransit || enroute.Number != "" || enroute.Link != "" ||
		!enroute.WindowFrom.Equal(moment("2026-11-16T13:30:00+01:00")) || !enroute.WindowTo.Equal(moment("2026-11-16T14:30:00+01:00")) {
		t.Fatalf("account en route = %+v", enroute)
	}
	if later := got["entry_postnl_a:pk-enroute-2"]; later.Status != parcel.StatusInTransit || !later.WindowFrom.Equal(moment("2026-11-17T00:00:00+01:00")) || !later.WindowTo.IsZero() {
		t.Fatalf("account with a planned day only = %+v", later)
	}
	if done := got["entry_postnl_a:pk-done-1"]; done.Status != parcel.StatusDelivered || !done.DeliveredAt.Equal(moment("2026-11-10T11:12:13+01:00")) {
		t.Fatalf("account delivered = %+v", done)
	}

	out := got["entry_dhlnl_a:3SEXA123456789"]
	if out.Carrier != "dhl_nl" || out.Number != "3SEXA123456789" || out.Sender != "Example Shop" || out.Status != parcel.StatusOutForDelivery ||
		out.Link != "https://my.dhlecommerce.nl/home/tracktrace/3SEXA123456789" ||
		!out.WindowFrom.Equal(moment("2026-11-16T09:00:00+01:00")) || !out.WindowTo.Equal(moment("2026-11-16T12:00:00+01:00")) {
		t.Fatalf("normalised out for delivery = %+v", out)
	}
	point := got["entry_dhlnl_a:JVGL00000000000000001"]
	if point.Status != parcel.StatusAtPickupPoint || point.PickupPoint != "Example Point" {
		t.Fatalf("a parcel listed by two sensors keeps its furthest status: %+v", point)
	}
	if done := got["entry_dhlnl_a:3SEXA000000001"]; done.Status != parcel.StatusDelivered || !done.DeliveredAt.Equal(moment("2026-11-12T10:00:00+01:00")) {
		t.Fatalf("normalised delivered = %+v", done)
	}
	if express := got["entry_dhl_a:1234567890"]; express.Carrier != "dhl" || express.Status != parcel.StatusInTransit ||
		express.Link != "https://www.dhl.com/nl-en/home/tracking.html?tracking-id=1234567890" {
		t.Fatalf("dhl = %+v", express)
	}
	for _, ignored := range []string{"3SEXAOUTGOING01", "3SEXADISABLED01", "pk-outgoing-1", "pk-free-1", "DPD00000000001"} {
		for key := range got {
			if strings.Contains(key, ignored) {
				t.Errorf("%s is read though it must not be: %s", ignored, key)
			}
		}
	}

	hub := byKey(f.read("bob"))
	if len(hub) != 4 || hub["entry_gls_b:00000000000"].Carrier != "gls" || hub["entry_gls_b:00000000000"].Status != parcel.StatusRegistered ||
		hub["entry_gls_b:00000000003"].Status != parcel.StatusProblem || hub["entry_gls_b:00000000002"].Status != parcel.StatusDelivered ||
		hub["entry_dhl_b:1234567891"].Carrier != "dhl" {
		t.Fatalf("bob's parcels = %+v", hub)
	}
}

func TestUnassignedAndUnlistedEntriesAreNeverShown(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	defer func() {
		for _, entityID := range f.sim.StateReads() {
			if entityID == "sensor.example_free_delivery" || entityID == "sensor.example_dpd_incoming" {
				t.Errorf("%s was fetched", entityID)
			}
		}
	}()
	for _, principal := range []string{"alice", "bob", "carol"} {
		for key := range byKey(f.read(principal)) {
			if strings.HasPrefix(key, "entry_postnl_free") || strings.HasPrefix(key, "entry_dpd_a") {
				t.Errorf("%s sees %s", principal, key)
			}
		}
	}
	if reading := f.read("carol"); len(reading.Parcels) != 0 || reading.Stale {
		t.Fatalf("a principal without sources reads %+v", reading)
	}

	f.source = mustSource(t, f, map[string][]string{"alice": {"entry_dpd_a", "entry_absent"}})
	sources, err := f.source.List(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].State != parcel.SourceUnknown || sources[1].State != parcel.SourceUnknown {
		t.Fatalf("sources before a read = %+v", sources)
	}
	if reading := f.read("alice"); len(reading.Parcels) != 0 || reading.Stale {
		t.Fatalf("missing entries read %+v", reading)
	}
	if sources, _ = f.source.List(t.Context(), "alice"); sources[0].State != parcel.SourceMissing || sources[1].State != parcel.SourceMissing || sources[0].Domain != "" {
		t.Fatalf("sources = %+v", sources)
	}
}

func mustSource(t *testing.T, f *parcelFixture, assignments map[string][]string) *homeassistant.ParcelSource {
	t.Helper()
	source, err := f.service.NewParcelSource(assignments, f.store)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestListShowsAssignedSourcesWithTheirState(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	f.read("bob")
	sources, err := f.source.List(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].EntryID != "entry_gls_b" || sources[0].Domain != "gls" || sources[0].Title != "GLS (1234 AB)" ||
		sources[0].State != parcel.SourceOK || !sources[0].ReadAt.Equal(start) || sources[1].Domain != "dhl" {
		t.Fatalf("sources = %+v", sources)
	}
	if got := f.source.Assigned("bob"); !slices.Equal(got, []string{"entry_gls_b", "entry_dhl_b"}) {
		t.Fatalf("assigned = %v", got)
	}
}

func TestAnAddedNumberDecidesWhoseParcelItIs(t *testing.T) {
	t.Parallel()
	add := func(f *parcelFixture, principal, number string) {
		t.Helper()
		if err := f.store.SaveAddedNumber(t.Context(), parcel.AddedNumber{PrincipalID: principal, Carrier: "dhl", Number: number, Domain: "dhl", EntryID: "entry_dhl_a", AddedAt: start}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an entry that is not theirs shows a number they added", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, nil)
		add(f, "alice", "1234567891")
		if _, found := byKey(f.read("alice"))["entry_dhl_b:1234567891"]; !found {
			t.Fatal("alice does not see the number she added")
		}
		if _, found := byKey(f.read("bob"))["entry_dhl_b:1234567891"]; found {
			t.Fatal("bob still sees a number alice added in his entry")
		}
	})
	t.Run("a number someone else added is left out of an assigned entry", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, nil)
		add(f, "bob", "1234567890")
		if _, found := byKey(f.read("alice"))["entry_dhl_a:1234567890"]; found {
			t.Fatal("alice sees a number bob added")
		}
		if _, found := byKey(f.read("bob"))["entry_dhl_a:1234567890"]; !found {
			t.Fatal("bob does not see the number he added")
		}
	})
}

func TestRawAndHistoryNeverAppearAnywhere(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	var everything strings.Builder
	for _, principal := range []string{"alice", "bob"} {
		reading := f.read(principal)
		fmt.Fprintf(&everything, "%+v", reading)
		encoded, err := json.Marshal(reading)
		if err != nil {
			t.Fatal(err)
		}
		everything.Write(encoded)
	}

	f.sim.SetAttributes("sensor.example_account_delivery", map[string]any{
		"enroute": []map[string]any{{"key": "pk-bad", "name": "Example order", "delivered": false, "planned_from": 12345, "status_message": "Example Street 1, Exampleville"}},
	})
	f.sim.SetAttributes("sensor.example_dhl_nl_incoming", map[string]any{
		"parcels": []map[string]any{{"barcode": "3SEXA123456789", "status": 7, "raw": map[string]any{"address": "Example Street 1, Exampleville"}}},
	})
	f.clock.advance(time.Hour)
	reading, err := f.source.Read(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reading.Stale {
		t.Fatal("entries whose attributes cannot be decoded are not stale")
	}
	fmt.Fprintf(&everything, "%+v", reading)
	sources, err := f.source.List(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&everything, "%+v", sources)
	f.sim.FailStates(true)
	f.clock.advance(time.Hour)
	_, err = f.source.Read(t.Context(), "alice")
	if err != nil {
		everything.WriteString(err.Error())
	}
	everything.WriteString(f.logs.String())

	for _, canary := range canaries {
		if strings.Contains(everything.String(), canary) {
			t.Errorf("%q reached a parcel, an error, or a log", canary)
		}
	}
	for _, private := range []string{"Example order", "Example Shop", "3SEXA123456789", "Example Point"} {
		if strings.Contains(f.logs.String(), private) {
			t.Errorf("%q reached the log", private)
		}
	}
}

func TestAFailedReadKeepsTheLastParcelsAsStale(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	first := f.read("alice")

	f.sim.FailStates(true)
	f.clock.advance(time.Hour)
	stale := f.read("alice")
	if !stale.Stale || len(stale.Parcels) != len(first.Parcels) || len(stale.Parcels) == 0 {
		t.Fatalf("stale reading = %+v", stale)
	}
	sources, err := f.source.List(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if source.State != parcel.SourceStale || !source.ReadAt.Equal(start) {
			t.Errorf("source = %+v", source)
		}
	}

	f.sim.FailStates(false)
	f.clock.advance(time.Hour)
	if recovered := f.read("alice"); recovered.Stale || len(recovered.Parcels) != len(first.Parcels) {
		t.Fatalf("recovered reading = %+v", recovered)
	}

	f.sim.SetConfigEntryState("entry_dhl_a", "setup_retry")
	f.clock.advance(time.Hour)
	reading := f.read("alice")
	if !reading.Stale || len(byKey(reading)) != len(first.Parcels) {
		t.Fatalf("a source that is not loaded keeps its parcels: %+v", reading)
	}
	if _, found := byKey(reading)["entry_dhl_a:1234567890"]; !found {
		t.Fatal("the entry that is not loaded lost its parcel")
	}
}

func TestOneReadServesEveryPrincipalWithinTheTTL(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	f.read("alice")
	f.sim.SetAttributes("sensor.example_dhl_a_incoming", map[string]any{"parcels": []map[string]any{}})
	f.read("bob")
	if _, found := byKey(f.read("alice"))["entry_dhl_a:1234567890"]; !found {
		t.Fatal("a second read inside the TTL went to Home Assistant again")
	}
	f.clock.advance(time.Hour)
	if _, found := byKey(f.read("alice"))["entry_dhl_a:1234567890"]; found {
		t.Fatal("a read after the TTL kept the old parcels")
	}
}

func serviceBody(t *testing.T, call homesim.Call) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(call.Arguments, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func (f *parcelFixture) track(principal, number, carrier string) (parcel.AddedNumber, bool, error) {
	f.t.Helper()
	return f.source.Track(f.t.Context(), principal, number, carrier)
}

func TestTrackAddsTheNumberToTheAddersOwnEntryAndUntrackRemovesIt(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	added, isNew, err := f.track("alice", "1234 567-892", "")
	if err != nil || !isNew || added.Number != "1234567892" || added.Carrier != "dhl" || added.Domain != "dhl" || added.EntryID != "entry_dhl_a" {
		t.Fatalf("added = %+v %v %v", added, isNew, err)
	}
	calls := f.serviceCalls()
	if len(calls) != 1 || calls[0].Tool != "service:dhl.track_parcel" {
		t.Fatalf("calls = %+v", calls)
	}
	if body := serviceBody(t, calls[0]); len(body) != 2 || body["tracking_code"] != "1234567892" || body["config_entry_id"] != "entry_dhl_a" {
		t.Fatalf("body = %v", body)
	}
	if stored, found, err := f.store.LoadAddedNumber(t.Context(), "alice", "dhl", "1234567892"); err != nil || !found || stored.EntryID != "entry_dhl_a" {
		t.Fatalf("stored = %+v %v %v", stored, found, err)
	}
	if _, found, _ := f.store.LoadProposal(t.Context(), "alice", "1234567892"); found {
		t.Fatal("Track records a proposal outcome")
	}
	if strings.Contains(f.logs.String(), "1234567892") {
		t.Fatal("the number reached the log")
	}

	if err := f.source.Untrack(t.Context(), "alice", "dhl", "1234567892"); err != nil {
		t.Fatal(err)
	}
	calls = f.serviceCalls()
	if len(calls) != 2 || calls[1].Tool != "service:dhl.untrack_parcel" {
		t.Fatalf("calls = %+v", calls)
	}
	if body := serviceBody(t, calls[1]); len(body) != 2 || body["tracking_code"] != "1234567892" || body["config_entry_id"] != "entry_dhl_a" {
		t.Fatalf("untrack body = %v", body)
	}
	if _, found, _ := f.store.LoadAddedNumber(t.Context(), "alice", "dhl", "1234567892"); found {
		t.Fatal("the added number outlives its removal")
	}
	if err := f.source.Untrack(t.Context(), "alice", "dhl", "1234567892"); err != nil || len(f.serviceCalls()) != 2 {
		t.Fatalf("untracking what was not added calls Home Assistant: %v", err)
	}
}

func TestANumberTheEntryAlreadyListsIsNotAddedOrOwned(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	added, isNew, err := f.track("alice", "1234567890", "dhl")
	if err != nil || isNew || added.EntryID != "entry_dhl_a" {
		t.Fatalf("added = %+v %v %v", added, isNew, err)
	}
	if len(f.serviceCalls()) != 0 {
		t.Fatalf("calls = %+v", f.serviceCalls())
	}
	if _, found, _ := f.store.LoadAddedNumber(t.Context(), "alice", "dhl", "1234567890"); found {
		t.Fatal("ownership of a number the entry already had was recorded")
	}
}

func TestAGlsNumberNeedsAHubWithTheUsersPostcode(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	if _, _, err := f.track("bob", "00000000005", "gls"); !errors.Is(err, homeassistant.ErrPostcodeMissing) {
		t.Fatalf("without a postcode: %v", err)
	}
	if err := f.store.SaveSettings(t.Context(), parcel.Settings{PrincipalID: "bob", Postcode: "9999 ZZ"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.track("bob", "00000000005", "gls"); !errors.Is(err, homeassistant.ErrFollowedByMail) {
		t.Fatalf("with another hub's postcode: %v", err)
	}
	if len(f.serviceCalls()) != 0 {
		t.Fatalf("calls = %+v", f.serviceCalls())
	}

	if err := f.store.SaveSettings(t.Context(), parcel.Settings{PrincipalID: "bob", Postcode: "1234 ab"}); err != nil {
		t.Fatal(err)
	}
	added, isNew, err := f.track("bob", "00000000005", "gls")
	if err != nil || !isNew || added.EntryID != "entry_gls_b" {
		t.Fatalf("added = %+v %v %v", added, isNew, err)
	}
	calls := f.serviceCalls()
	if len(calls) != 1 || calls[0].Tool != "service:gls.track_parcel" {
		t.Fatalf("calls = %+v", calls)
	}
	if body := serviceBody(t, calls[0]); len(body) != 2 || body["tracking_code"] != "00000000005" || body["postal_code"] != "1234AB" {
		t.Fatalf("body = %v", body)
	}

	if err := f.source.Untrack(t.Context(), "bob", "gls", "00000000005"); err != nil {
		t.Fatal(err)
	}
	calls = f.serviceCalls()
	if len(calls) != 2 || calls[1].Tool != "service:gls.untrack_parcel" {
		t.Fatalf("calls = %+v", calls)
	}
	if body := serviceBody(t, calls[1]); len(body) != 1 || body["tracking_code"] != "00000000005" {
		t.Fatalf("untrack body = %v", body)
	}
}

func TestNumbersTheIntegrationsCannotTrackAreFollowedByMail(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	cases := map[string]struct {
		principal, number, carrier string
		want                       error
	}{
		"a carrier whose account integration lists parcels": {"alice", "3SEXA123456780", "postnl", homeassistant.ErrFollowedByMail},
		"a carrier with no integration":                     {"alice", "1Z999AA10123456784", "ups", homeassistant.ErrFollowedByMail},
		"a domain that is not a carrier":                    {"alice", "1234567890", "lock", homeassistant.ErrFollowedByMail},
		"a number no carrier owns":                          {"alice", "ABCDEFGHIJ", "", homeassistant.ErrFollowedByMail},
		"a number two carriers share":                       {"alice", "3SEXA123456780", "", homeassistant.ErrFollowedByMail},
		"an adder without an entry of the domain":           {"alice", "00000000005", "gls", homeassistant.ErrFollowedByMail},
		"a principal without sources":                       {"carol", "1234567890", "dhl", homeassistant.ErrFollowedByMail},
		"a malformed number":                                {"alice", "12-3", "dhl", homeassistant.ErrInvalidNumber},
	}
	for name, test := range cases {
		if _, _, err := f.track(test.principal, test.number, test.carrier); !errors.Is(err, test.want) {
			t.Errorf("%s: err = %v, want %v", name, err, test.want)
		}
	}
	if calls := f.serviceCalls(); len(calls) != 0 {
		t.Fatalf("calls = %+v", calls)
	}
}

type failingSave struct{ parcel.Repository }

func (failingSave) SaveAddedNumber(context.Context, parcel.AddedNumber) error {
	return errors.New("disk full")
}

func TestAFailedRecordIsCompensatedByRemovingTheNumber(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	source, err := f.service.NewParcelSource(parcelAssignments, failingSave{f.store})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.Track(t.Context(), "alice", "1234567895", "dhl"); err == nil {
		t.Fatal("a failed record is reported as success")
	}
	calls := f.serviceCalls()
	if len(calls) != 2 || calls[0].Tool != "service:dhl.track_parcel" || calls[1].Tool != "service:dhl.untrack_parcel" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestUntrackLeavesANumberAnotherUserOrEntryStillHas(t *testing.T) {
	t.Parallel()
	save := func(f *parcelFixture, principal, carrier, number, entry string) {
		t.Helper()
		if err := f.store.SaveAddedNumber(t.Context(), parcel.AddedNumber{PrincipalID: principal, Carrier: carrier, Number: number, Domain: carrier, EntryID: entry, AddedAt: start}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("gls untrack acts on every hub, so another owner keeps it", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, nil)
		save(f, "bob", "gls", "00000000005", "entry_gls_b")
		save(f, "alice", "gls", "00000000005", "entry_gls_other")
		if err := f.source.Untrack(t.Context(), "bob", "gls", "00000000005"); err != nil {
			t.Fatal(err)
		}
		if len(f.serviceCalls()) != 0 {
			t.Fatalf("calls = %+v", f.serviceCalls())
		}
		if _, found, _ := f.store.LoadAddedNumber(t.Context(), "bob", "gls", "00000000005"); found {
			t.Fatal("bob's record stays")
		}
		if _, found, _ := f.store.LoadAddedNumber(t.Context(), "alice", "gls", "00000000005"); !found {
			t.Fatal("alice's record was deleted")
		}
	})
	t.Run("gls untrack leaves a number another hub lists", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, func(seed *homesim.Seed) {
			seed.ConfigEntries = append(seed.ConfigEntries, homesim.ConfigEntry{EntryID: "entry_gls_c", Domain: "gls", Title: "GLS (5678CD)"})
			seed.Registry = append(seed.Registry, homesim.RegistryEntity{EntityID: "sensor.example_gls_c_incoming", Platform: "gls", UniqueID: "entry_gls_c_incoming_parcels", ConfigEntryID: "entry_gls_c"})
			seed.Entities = append(seed.Entities, homesim.Entity{EntityID: "sensor.example_gls_c_incoming", Name: "Example GLS C incoming", State: "1",
				Attributes: map[string]any{"parcels": []map[string]any{{"barcode": "00000000005", "status": "in_transit"}}}})
		})
		f.source = mustSource(t, f, map[string][]string{"bob": {"entry_gls_b"}, "alice": {"entry_gls_c"}})
		save(f, "bob", "gls", "00000000005", "entry_gls_b")
		f.read("bob")
		if err := f.source.Untrack(t.Context(), "bob", "gls", "00000000005"); err != nil {
			t.Fatal(err)
		}
		if len(f.serviceCalls()) != 0 {
			t.Fatalf("calls = %+v", f.serviceCalls())
		}
	})
	t.Run("dhl untrack acts on one entry, so another entry's owner does not matter", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, nil)
		save(f, "alice", "dhl", "1234567896", "entry_dhl_a")
		save(f, "bob", "dhl", "1234567896", "entry_dhl_b")
		if err := f.source.Untrack(t.Context(), "alice", "dhl", "1234567896"); err != nil {
			t.Fatal(err)
		}
		if calls := f.serviceCalls(); len(calls) != 1 || calls[0].Tool != "service:dhl.untrack_parcel" {
			t.Fatalf("calls = %+v", calls)
		}
	})
	t.Run("dhl untrack leaves a number another user added to the same entry", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, nil)
		save(f, "alice", "dhl", "1234567896", "entry_dhl_a")
		save(f, "bob", "dhl", "1234567896", "entry_dhl_a")
		if err := f.source.Untrack(t.Context(), "alice", "dhl", "1234567896"); err != nil {
			t.Fatal(err)
		}
		if len(f.serviceCalls()) != 0 {
			t.Fatalf("calls = %+v", f.serviceCalls())
		}
	})
}

func TestNaiveTimestampsAreInHomeAssistantsZone(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	got := byKey(f.read("bob"))["entry_gls_b:00000000002"]
	if want := time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC); !got.DeliveredAt.Equal(want) {
		t.Fatalf("delivered at = %v, want %v", got.DeliveredAt, want)
	}
}

func TestOnlyAssignedEntriesAreFetched(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	f.read("alice")
	f.read("bob")
	reads := f.sim.StateReads()
	if len(reads) == 0 {
		t.Fatal("nothing was read")
	}
	for _, entityID := range reads {
		if entityID == "sensor.example_free_delivery" || entityID == "sensor.example_dpd_incoming" || entityID == "sensor.example_dhl_nl_parcel" {
			t.Errorf("%s was read", entityID)
		}
	}
}

func TestAnEntryThatDisappearsLosesItsParcels(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	f.read("alice")
	f.sim.RemoveConfigEntry("entry_dhl_a")
	f.clock.advance(time.Hour)
	reading := f.read("alice")
	if reading.Stale {
		t.Fatal("a missing entry makes the reading stale")
	}
	if _, found := byKey(reading)["entry_dhl_a:1234567890"]; found {
		t.Fatal("the missing entry's parcel stays")
	}
	sources, err := f.source.List(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if sources[2].EntryID != "entry_dhl_a" || sources[2].State != parcel.SourceMissing || sources[2].Domain != "dhl" || sources[2].Title == "" {
		t.Fatalf("sources = %+v", sources)
	}
}

func TestListNeverReachesHomeAssistant(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	sources, err := f.source.List(t.Context(), "alice")
	if err != nil || len(sources) != 3 || f.sim.Requests() != 0 {
		t.Fatalf("sources = %+v %v, requests = %d", sources, err, f.sim.Requests())
	}
	for _, source := range sources {
		if source.State != parcel.SourceUnknown {
			t.Errorf("an unread source is %+v", source)
		}
	}
	f.read("alice")
	before := f.sim.Requests()
	f.clock.advance(time.Hour)
	if sources, err = f.source.List(t.Context(), "alice"); err != nil || f.sim.Requests() != before || sources[0].State != parcel.SourceOK {
		t.Fatalf("sources = %+v %v, requests %d -> %d", sources, err, before, f.sim.Requests())
	}
}

func TestAFailureIsNotRetriedWithinTheTTLAndStopsAtAnUnreachableServer(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	f.sim.FailStates(true)
	f.read("alice")
	distinct := map[string]bool{}
	for _, entityID := range f.sim.StateReads() {
		distinct[entityID] = true
	}
	if len(distinct) != 1 {
		t.Fatalf("a server that is down was asked about %v", distinct)
	}
	before := len(f.sim.StateReads())
	f.read("bob")
	f.read("alice")
	if len(f.sim.StateReads()) != before {
		t.Fatal("a failed read was repeated within the TTL")
	}
}

func TestEntriesWithoutSummarySensorsAreStale(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, func(seed *homesim.Seed) {
		seed.Registry = slices.DeleteFunc(seed.Registry, func(entity homesim.RegistryEntity) bool { return entity.ConfigEntryID == "entry_gls_b" })
	})
	if reading := f.read("bob"); !reading.Stale {
		t.Fatalf("reading = %+v", reading)
	}
	if !strings.Contains(f.logs.String(), "homeassistant.parcels.no_sensors") {
		t.Fatalf("logs = %s", f.logs.String())
	}
}

func TestUnusableItemsAreCountedNotShown(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	f.sim.SetAttributes("sensor.example_dhl_a_incoming", map[string]any{"parcels": []map[string]any{
		{"barcode": "1234567890", "status": "in_transit"}, {"barcode": "", "status": "in_transit"},
	}})
	if got := byKey(f.read("alice")); len(got) != 7 {
		t.Fatalf("parcels = %+v", got)
	}
	if !strings.Contains(f.logs.String(), "homeassistant.parcels.items_dropped") || !strings.Contains(f.logs.String(), "count=1") {
		t.Fatalf("logs = %s", f.logs.String())
	}
}

func TestAnEntryAssignedToTwoPrincipalsIsRefused(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	if _, err := f.service.NewParcelSource(map[string][]string{"alice": {"entry_dhl_a"}, "bob": {"entry_dhl_a"}}, f.store); err == nil {
		t.Fatal("a shared entry is accepted")
	}
}

func TestParallelReadsAndListsShareOneRead(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	var wg sync.WaitGroup
	for index := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			principal := []string{"alice", "bob"}[index%2]
			for range 5 {
				if _, err := f.source.Read(t.Context(), principal); err != nil {
					t.Error(err)
				}
				if _, err := f.source.List(t.Context(), principal); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func exposeParcelEntity(seed *homesim.Seed) {
	for index := range seed.Entities {
		if seed.Entities[index].EntityID == "sensor.example_account_delivery" {
			seed.Entities[index].Exposed = true
		}
	}
}

func TestLiveContextListingAParcelEntityLogsOneWarning(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, exposeParcelEntity)
	f.read("alice")
	for range 2 {
		if _, err := f.service.Contribute(t.Context(), "alice"); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.advance(time.Hour)
	f.read("alice")
	logs := f.logs.String()
	if count := strings.Count(logs, "homeassistant.parcels.exposed_to_assist"); count != 1 {
		t.Fatalf("warnings = %d in\n%s", count, logs)
	}
	if !strings.Contains(logs, "entity_id=sensor.example_account_delivery") || strings.Contains(logs, "Example account delivery") {
		t.Fatalf("the warning names more than the entity: %s", logs)
	}
}

func TestLiveContextWithoutAParcelEntityLogsNoWarning(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	f.read("alice")
	if _, err := f.service.Contribute(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.logs.String(), "exposed_to_assist") {
		t.Fatalf("logs = %s", f.logs.String())
	}
}

func TestAnyEntityOfAParcelEntryExposedToAssistIsWarnedAbout(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, func(seed *homesim.Seed) {
		for index := range seed.Entities {
			if seed.Entities[index].EntityID == "sensor.example_dhl_nl_parcel" {
				seed.Entities[index].Exposed = true
			}
		}
	})
	f.read("alice")
	if _, err := f.service.Contribute(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "entity_id=sensor.example_dhl_nl_parcel") {
		t.Fatalf("logs = %s", logs)
	}
}

func TestAnAccountPackageKeyedByItsBarcodeCarriesTheNumber(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	if carriers := f.source.AccountCarriers("alice"); len(carriers) != 0 {
		t.Fatalf("an account counts before it was read: %v", carriers)
	}
	f.read("alice")
	if carriers := f.source.AccountCarriers("alice"); len(carriers) != 1 || carriers["postnl"] {
		t.Fatalf("an account whose keys hold no number = %v", carriers)
	}
	f.sim.SetAttributes("sensor.example_account_delivery", map[string]any{
		"enroute": []map[string]any{
			{"key": "3SEXB98765432-NL-1234AB", "name": "Example\norder\u200b https://shop.example/x", "delivered": false},
			{"key": "RR123456785CN-NL-1234AB", "name": "Example import", "delivered": false},
		},
		"delivered": []map[string]any{},
	})
	f.clock.advance(time.Hour)
	got := byKey(f.read("alice"))["entry_postnl_a:3SEXB98765432-NL-1234AB"]
	if got.Number != "3SEXB98765432" || got.Link != "https://postnl.nl/tracktrace/?B=3SEXB98765432&D=NL&T=C" || got.Title != "Example order" {
		t.Fatalf("account package = %+v", got)
	}
	if imported := byKey(f.read("alice"))["entry_postnl_a:RR123456785CN-NL-1234AB"]; imported.Number != "RR123456785CN" {
		t.Fatalf("an international item = %+v", imported)
	}
	if carriers := f.source.AccountCarriers("alice"); len(carriers) != 1 || !carriers["postnl"] {
		t.Fatalf("an account whose keys hold the number = %v", carriers)
	}
	if carriers := f.source.AccountCarriers("bob"); len(carriers) != 0 {
		t.Fatalf("bob has no account source: %v", carriers)
	}
	f.clock.advance(25 * time.Hour)
	if carriers := f.source.AccountCarriers("alice"); len(carriers) != 0 {
		t.Fatalf("an account that has not read for a day still counts: %v", carriers)
	}
}

func TestOutsideTextFromHomeAssistantIsPlainText(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	f.sim.SetAttributes("sensor.example_dhl_nl_incoming", map[string]any{"parcels": []map[string]any{{
		"barcode": "JVGL00000000000000009", "status": "in_transit", "sender": "Example\nShop www.shop.example/deal",
		"pickup_point": "Example\u202ePoint\tvisit https://point.example",
	}}})
	got := byKey(f.read("alice"))["entry_dhlnl_a:JVGL00000000000000009"]
	if got.Sender != "Example Shop" || got.PickupPoint != "Example Point visit" {
		t.Fatalf("parcel = %+v", got)
	}
}

// A shop's name that is a domain is kept with its dots: an account package's name is often only
// that, and a sender is matched by its text.
func TestADomainShapedNameFromHomeAssistantIsKept(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	f.sim.SetAttributes("sensor.example_account_delivery", map[string]any{
		"enroute":   []map[string]any{{"key": "3SEXB98765432-NL-1234AB", "name": "EXAMPLESHOP.NL", "delivered": false}},
		"delivered": []map[string]any{},
	})
	f.sim.SetAttributes("sensor.example_dhl_nl_incoming", map[string]any{"parcels": []map[string]any{{
		"barcode": "JVGL00000000000000009", "status": "in_transit", "sender": "Shop.Example", "pickup_point": "Point shop.example/",
	}}})
	parcels := byKey(f.read("alice"))
	if got := parcels["entry_postnl_a:3SEXB98765432-NL-1234AB"]; got.Title != "EXAMPLESHOP.NL" {
		t.Fatalf("account package = %+v", got)
	}
	if got := parcels["entry_dhlnl_a:JVGL00000000000000009"]; got.Sender != "Shop.Example" || got.PickupPoint != "Point" {
		t.Fatalf("parcel = %+v", got)
	}
}

func TestAPrincipalWithoutSourcesReadsNothingWhileHomeAssistantIsDown(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	f.sim.FailStates(true)
	requests := f.sim.Requests()
	reading, err := f.source.Read(t.Context(), "carol")
	if err != nil || len(reading.Parcels) != 0 || f.sim.Requests() != requests {
		t.Fatalf("reading = %+v, error %v, requests %d -> %d", reading, err, requests, f.sim.Requests())
	}
	if reading, err := f.source.Read(t.Context(), "alice"); err != nil || !reading.Stale {
		t.Fatalf("an assigned principal's read = %+v, %v", reading, err)
	}
}

func TestACancelledReadReturnsWhileHomeAssistantHangs(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, nil)
	release := f.sim.HoldStates()
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	if _, err := f.source.Read(ctx, "alice"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("a cancelled read waited %v for Home Assistant", waited)
	}
	release()
	f.clock.advance(time.Hour)
	if reading := f.read("alice"); len(reading.Parcels) == 0 {
		t.Fatal("the source did not recover once Home Assistant answered")
	}
}

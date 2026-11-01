package collection_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/collection"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// confirmer stands in for the approval service with a user who confirms every proposal at once: a
// proposal is executed as Confirm would execute it, and an action as a clean chat turn would. It
// keeps which tools were proposed rather than acted on, and each action's undo payload.
type confirmer struct {
	mu       sync.Mutex
	service  *collection.Service
	next     int
	proposed []string
	undo     map[string]json.RawMessage
	failure  string
}

func (c *confirmer) execute(ctx context.Context, principalID string, proposal approval.Proposal) (approval.Record, approval.Outcome, error) {
	c.mu.Lock()
	c.next++
	record := approval.Record{ID: fmt.Sprintf("%016x", c.next), PrincipalID: principalID, Tool: proposal.Tool, Arguments: proposal.Arguments}
	c.mu.Unlock()
	outcome, err := c.service.ExecuteApproved(ctx, record)
	return record, outcome, err
}

func (c *confirmer) Propose(ctx context.Context, principalID string, proposal approval.Proposal) (approval.Record, error) {
	c.mu.Lock()
	c.proposed = append(c.proposed, proposal.Tool)
	c.mu.Unlock()
	record, outcome, err := c.execute(ctx, principalID, proposal)
	c.remember(record, outcome, err)
	return record, nil
}

func (c *confirmer) Act(ctx context.Context, principalID string, proposal approval.Proposal) (string, error) {
	record, outcome, err := c.execute(ctx, principalID, proposal)
	c.remember(record, outcome, err)
	if err != nil {
		return "", &approval.FailedError{Text: "The action failed: " + err.Error()}
	}
	return "Done: " + outcome.Text, nil
}

func (c *confirmer) remember(record approval.Record, outcome approval.Outcome, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failure = ""
	var failed *approval.ActionError
	switch {
	case errors.As(err, &failed):
		c.failure = failed.Text
	case err != nil:
		c.failure = err.Error()
	default:
		c.undo[record.Tool] = outcome.Undo
	}
}

type harness struct {
	t         *testing.T
	ctx       context.Context
	store     *sqlite.Store
	service   *collection.Service
	confirmer *confirmer
	now       time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := t.Context()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: ctx, store: store, confirmer: &confirmer{undo: map[string]json.RawMessage{}}, now: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)}
	service, err := collection.NewService(collection.Config{Location: time.UTC, Now: func() time.Time {
		h.now = h.now.Add(time.Second)
		return h.now
	}}, store, h.confirmer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h.service, h.confirmer.service = service, service
	return h
}

func (h *harness) call(principalID, name, arguments string) tools.Result {
	h.t.Helper()
	result, err := h.service.Execute(h.ctx, principalID, tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *harness) ok(principalID, name, arguments string) string {
	h.t.Helper()
	result := h.call(principalID, name, arguments)
	if result.IsError {
		h.t.Fatalf("%s %s = %q", name, arguments, result.Text)
	}
	if h.confirmer.failure != "" {
		h.t.Fatalf("%s %s failed on execution: %s", name, arguments, h.confirmer.failure)
	}
	return result.Text
}

func (h *harness) refused(principalID, name, arguments string, want ...string) {
	h.t.Helper()
	result := h.call(principalID, name, arguments)
	if !result.IsError {
		h.t.Fatalf("%s %s = %q, want a refusal", name, arguments, result.Text)
	}
	for _, text := range want {
		if !strings.Contains(result.Text, text) {
			h.t.Fatalf("%s refusal = %q, want %q", name, result.Text, text)
		}
	}
}

const giftsSchema = `{"name":"Gifts","description":"gift ideas","fields":[` +
	`{"name":"person","type":"person","required":true},` +
	`{"name":"idea","type":"text"},` +
	`{"name":"price","type":"money"},` +
	`{"name":"rating","type":"number"},` +
	`{"name":"bought","type":"bool"},` +
	`{"name":"occasion","type":"choice","options":["birthday","holiday"]},` +
	`{"name":"due","type":"date"},` +
	`{"name":"seen","type":"datetime"}]}`

func (h *harness) seedGifts() {
	h.t.Helper()
	h.ok("alice", "collection_create", giftsSchema)
	h.ok("alice", "collection_add", `{"collection":"gifts","records":[`+
		`{"person":"Sam","idea":"a book on birds","price":{"amount":12.5,"currency":"eur"},"rating":4,"bought":false,"occasion":"Birthday","due":"2026-11-02"},`+
		`{"person":"Kim","idea":"board game","price":{"amount":30,"currency":"EUR"},"rating":5,"bought":true,"occasion":"holiday","due":"2026-12-24","seen":"2026-10-01T18:30"},`+
		`{"person":"sam","idea":"cookbook","price":{"amount":15,"currency":"EUR"},"rating":3,"bought":true,"due":"2026-12-24"},`+
		`{"person":"Lee","idea":"scarf","price":{"amount":20,"currency":"USD"},"bought":false,"seen":"2026-10-02T09:00"},`+
		`{"person":"Kim","idea":"plant","rating":2}]}`)
}

func TestQueryFiltersSortsTotalsAndGroups(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedGifts()

	sam := h.ok("alice", "collection_query", `{"collection":"Gifts","filters":[{"field":"person","op":"=","value":"SAM"}]}`)
	for _, want := range []string{
		"Gifts: 2 of 5 records match.",
		"- price: 2 with a value, sum 27.50 EUR, min 12.50 EUR, max 15.00 EUR, mean 13.75 EUR",
		"- rating: 2 with a value, sum 7, min 3, max 4, mean 3.5",
		"person: Sam; idea: a book on birds; price: 12.50 EUR; rating: 4; bought: no; occasion: birthday; due: 2026-11-02",
		"person: sam; idea: cookbook",
	} {
		if !strings.Contains(sam, want) {
			t.Fatalf("Sam's gifts lack %q:\n%s", want, sam)
		}
	}

	sorted := h.ok("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"price","op":">","value":12.5}],"sort":{"field":"price","descending":true}}`)
	board, scarf, cookbook := strings.Index(sorted, "board game"), strings.Index(sorted, "scarf"), strings.Index(sorted, "cookbook")
	if !strings.Contains(sorted, "3 of 5 records match.") || board < 0 || board > scarf || scarf > cookbook || strings.Contains(sorted, "birds") {
		t.Fatalf("gifts over 12.50, dearest first:\n%s", sorted)
	}
	if !strings.Contains(sorted, "- price: 2 with a value, sum 45.00 EUR") || !strings.Contains(sorted, "- price: 1 with a value, sum 20.00 USD") {
		t.Fatalf("money totals are not per currency:\n%s", sorted)
	}

	unbought := h.ok("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"bought","op":"!=","value":true},{"field":"idea","op":"contains","value":"BIRD"}]}`)
	if !strings.Contains(unbought, "1 of 5 records match.") || !strings.Contains(unbought, "a book on birds") {
		t.Fatalf("unbought bird gifts:\n%s", unbought)
	}

	dated := h.ok("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"due","op":"<","value":"2026-12-01"}]}`)
	undated := h.ok("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"due","op":"empty"}],"limit":0}`)
	day := h.ok("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"seen","op":"=","value":"2026-10-02"}]}`)
	if !strings.Contains(dated, "1 of 5 records match.") || !strings.Contains(undated, "2 of 5 records match.") || strings.Contains(undated, "Records") ||
		!strings.Contains(day, "1 of 5 records match.") || !strings.Contains(day, "seen: 2026-10-02 09:00") {
		t.Fatalf("date filters:\n%s\n\n%s\n\n%s", dated, undated, day)
	}

	grouped := h.ok("alice", "collection_query", `{"collection":"gifts","group_by":"person","limit":0}`)
	for _, want := range []string{
		"Groups by person:\n- Sam: 2 records\n  price: 2 with a value, sum 27.50 EUR",
		"- Kim: 2 records\n  price: 1 with a value, sum 30.00 EUR, min 30.00 EUR, max 30.00 EUR, mean 30.00 EUR\n  rating: 2 with a value, sum 7, min 2, max 5, mean 3.5",
		"- Lee: 1 record\n  price: 1 with a value, sum 20.00 USD",
		"- rating: 4 with a value, sum 14, min 2, max 5, mean 3.5",
	} {
		if !strings.Contains(grouped, want) {
			t.Fatalf("grouped gifts lack %q:\n%s", want, grouped)
		}
	}
	if occasions := h.ok("alice", "collection_query", `{"collection":"gifts","group_by":"occasion","limit":0}`); !strings.Contains(occasions, "- (empty): 3 records") {
		t.Fatalf("occasions:\n%s", occasions)
	}

	h.refused("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"idea","op":"<","value":"b"}]}`, `filter on "idea"`, "< and > work on")
	h.refused("alice", "collection_query", `{"collection":"gifts","sort":{"field":"colour"}}`, `no field called "colour"`, `"person", "idea"`)
	h.refused("alice", "collection_query", `{"collection":"gifts","limit":51}`, "limit is 0 to 50")
}

func TestInvalidValuesAreRefusedWithTheFieldNamed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedGifts()

	h.refused("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","price":12}]}`, `field "price" needs an amount of at most 1000000000000 and a currency`)
	h.refused("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","rating":"high"}]}`, `field "rating" needs a number`)
	h.refused("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","due":"next week"}]}`, `field "due" needs a date, YYYY-MM-DD`)
	h.refused("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","occasion":"wedding"}]}`, `field "occasion" needs one of birthday, holiday`)
	h.refused("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana"},{"idea":"socks"}]}`, `record 2: field "person" is required`)
	h.refused("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","colour":"red"}]}`, `no field called "colour"`)
	h.refused("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","idea":"`+strings.Repeat("x", 501)+`"}]}`, `field "idea" has at most 500 characters`)
	h.refused("alice", "collection_create", `{"name":"Books","fields":[{"name":"status","type":"choice"}]}`, `field "status": a choice field has 1 to 20 options`)
	h.refused("alice", "collection_create", `{"name":"gifts","fields":[{"name":"title","type":"text"}]}`, `A collection called "gifts" already exists`)

	if listed := h.ok("alice", "collection_query", `{"collection":"gifts","limit":0}`); !strings.Contains(listed, "5 of 5 records match.") {
		t.Fatalf("a refused record was stored:\n%s", listed)
	}
}

func TestCollectionLimitsAreEnforced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	fields := make([]string, 0, collection.MaximumFields+1)
	for index := range collection.MaximumFields + 1 {
		fields = append(fields, fmt.Sprintf(`{"name":"field %d","type":"text"}`, index+1))
	}
	h.refused("alice", "collection_create", `{"name":"wide","fields":[`+strings.Join(fields, ",")+`]}`, "a collection has 1 to 20 fields")
	h.ok("alice", "collection_create", `{"name":"wide","fields":[`+strings.Join(fields[:collection.MaximumFields], ",")+`]}`)
	h.refused("alice", "collection_alter", `{"collection":"wide","changes":[{"action":"add","field":"one more","type":"text"}]}`, "a collection has 1 to 20 fields")

	for index := 2; index <= collection.MaximumCollectionsPerPrincipal; index++ {
		h.ok("alice", "collection_create", fmt.Sprintf(`{"name":"list %d","fields":[{"name":"item","type":"text"}]}`, index))
	}
	h.refused("alice", "collection_create", `{"name":"one too many","fields":[{"name":"item","type":"text"}]}`, "You already have 20 collections")
	h.ok("bob", "collection_create", `{"name":"list 2","fields":[{"name":"item","type":"text"}]}`)

	collections, err := h.store.ListCollections(h.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	full := collections[1]
	records := make([]collection.Record, 0, collection.MaximumRecords-1)
	for index := range collection.MaximumRecords - 1 {
		records = append(records, collection.Record{
			ID: fmt.Sprintf("a%015x", index), PrincipalID: "alice", CollectionID: full.ID,
			Values: map[string]json.RawMessage{full.Fields[0].Key: json.RawMessage(fmt.Sprintf(`"item %d"`, index))}, CreatedAt: h.now, UpdatedAt: h.now,
		})
	}
	if created, err := h.store.CreateRecords(h.ctx, records, collection.MaximumRecords); err != nil || created != len(records) {
		t.Fatalf("seeded %d records: %v", created, err)
	}
	h.refused("alice", "collection_add", `{"collection":"list 2","records":[{"item":"a"},{"item":"b"}]}`, "would hold more than 2000 records")
	h.ok("alice", "collection_add", `{"collection":"list 2","records":[{"item":"last"}]}`)
	if _, err := h.store.CreateRecords(h.ctx, []collection.Record{{ID: "b000000000000000", PrincipalID: "alice", CollectionID: full.ID, Values: map[string]json.RawMessage{}, CreatedAt: h.now, UpdatedAt: h.now}}, collection.MaximumRecords); !errors.Is(err, collection.ErrTooManyRecords) {
		t.Fatalf("store past the record limit = %v", err)
	}

	listed := h.ok("alice", "collection_query", `{"collection":"list 2"}`)
	if !strings.Contains(listed, "2000 of 2000 records match.") || !strings.Contains(listed, "Listed 50 of 2000 matches.") || strings.Count(listed, "\n- [") != collection.MaximumQueryRecords {
		t.Fatalf("a full collection lists %d records:\n%.300s", strings.Count(listed, "\n- ["), listed)
	}
}

func TestFieldChangesKeepRenamedValuesAndDropRemovedOnes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedGifts()

	h.ok("alice", "collection_alter", `{"collection":"gifts","changes":[{"action":"rename","field":"person","new_name":"for"},{"action":"add","field":"store","type":"text"}]}`)
	if len(h.confirmer.proposed) != 1 || h.confirmer.proposed[0] != "collection_create" {
		t.Fatalf("adding and renaming were proposed: %v", h.confirmer.proposed)
	}
	renamed := h.ok("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"for","op":"=","value":"lee"}]}`)
	if !strings.Contains(renamed, "for: Lee; idea: scarf") {
		t.Fatalf("after the rename:\n%s", renamed)
	}

	h.ok("alice", "collection_alter", `{"collection":"gifts","changes":[{"action":"remove","field":"rating"},{"action":"add","field":"score","type":"number"}]}`)
	if len(h.confirmer.proposed) != 2 || h.confirmer.proposed[1] != "collection_alter" {
		t.Fatalf("removing a field was not proposed: %v", h.confirmer.proposed)
	}
	after := h.ok("alice", "collection_query", `{"collection":"gifts"}`)
	if strings.Contains(after, "rating") || strings.Contains(after, "score:") {
		t.Fatalf("a removed field's values survived, or the new field took them over:\n%s", after)
	}
	h.refused("alice", "collection_alter", `{"collection":"gifts","changes":[{"action":"add","field":"owner","type":"person","required":true}]}`, `field "owner" cannot be required`)
}

func TestDirectChangesUndo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedGifts()
	undo := func(tool string) string {
		t.Helper()
		outcome, err := h.service.Undo(h.ctx, approval.Record{PrincipalID: "alice", Tool: tool}, h.confirmer.undo[tool])
		if err != nil {
			t.Fatalf("undo %s = %v", tool, err)
		}
		return outcome.Text
	}
	record := func(idea string) string {
		t.Helper()
		found := h.ok("alice", "collection_query", fmt.Sprintf(`{"collection":"gifts","filters":[{"field":"idea","op":"=","value":%q}]}`, idea))
		start := strings.Index(found, "- [")
		if start < 0 {
			t.Fatalf("no %s record:\n%s", idea, found)
		}
		return found[start+3 : start+19]
	}

	scarf := record("scarf")
	h.ok("alice", "collection_update", fmt.Sprintf(`{"collection":"gifts","record_id":%q,"values":{"bought":true,"price":null}}`, scarf))
	if changed := h.ok("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"idea","op":"=","value":"scarf"}]}`); !strings.Contains(changed, "person: Lee; idea: scarf; bought: yes; seen") {
		t.Fatalf("after the update:\n%s", changed)
	}
	h.refused("alice", "collection_update", fmt.Sprintf(`{"collection":"gifts","record_id":%q,"values":{"person":null}}`, scarf), `field "person" is required`)
	undo("collection_update")
	if restored := h.ok("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"idea","op":"=","value":"scarf"}]}`); !strings.Contains(restored, "price: 20.00 USD; bought: no") {
		t.Fatalf("after undoing the update:\n%s", restored)
	}

	plant := record("plant")
	h.ok("alice", "collection_remove", fmt.Sprintf(`{"collection":"gifts","record_id":%q}`, plant))
	if count := h.ok("alice", "collection_query", `{"collection":"gifts","limit":0}`); !strings.Contains(count, "4 of 4") {
		t.Fatalf("after the removal:\n%s", count)
	}
	undo("collection_remove")
	if back := record("plant"); back != plant {
		t.Fatalf("the restored record is %s, not %s", back, plant)
	}

	h.ok("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","idea":"tea"},{"person":"Ana","idea":"mug"}]}`)
	if text := undo("collection_add"); text != "Removed 2 records." {
		t.Fatalf("undo of the add = %q", text)
	}
	if count := h.ok("alice", "collection_query", `{"collection":"gifts","limit":0}`); !strings.Contains(count, "5 of 5") {
		t.Fatalf("after undoing the add:\n%s", count)
	}

	h.ok("alice", "collection_alter", `{"collection":"gifts","changes":[{"action":"add","field":"store","type":"text"}]}`)
	undo("collection_alter")
	if fields := h.ok("alice", "collection_list", `{}`); strings.Contains(fields, "store") || !strings.Contains(fields, "- seen: datetime") {
		t.Fatalf("after undoing the alter:\n%s", fields)
	}
}

func TestCollectionsAreThePrincipalsOwn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedGifts()
	found := h.ok("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"idea","op":"=","value":"scarf"}]}`)
	id := found[strings.Index(found, "- [")+3 : strings.Index(found, "- [")+19]

	h.refused("bob", "collection_query", `{"collection":"gifts"}`, "The user has no collections.")
	if listed := h.ok("bob", "collection_list", `{}`); listed != "The user has no collections." {
		t.Fatalf("bob's list = %q", listed)
	}
	h.ok("bob", "collection_create", `{"name":"gifts","fields":[{"name":"idea","type":"text"}]}`)
	h.refused("bob", "collection_remove", fmt.Sprintf(`{"collection":"gifts","record_id":%q}`, id), "has no record with id")
	if _, removed, err := h.service.RemoveRecord(h.ctx, "bob", id); err != nil || removed {
		t.Fatalf("bob removed alice's record: %v %v", removed, err)
	}
	collections, err := h.service.Collections(h.ctx, "alice")
	if err != nil || len(collections) != 1 {
		t.Fatalf("alice's collections = %+v %v", collections, err)
	}
	if deleted, err := h.service.DeleteCollection(h.ctx, "bob", collections[0].ID); err != nil || deleted {
		t.Fatalf("bob deleted alice's collection: %v %v", deleted, err)
	}
	if line, err := h.service.TurnLine(h.ctx, "bob"); err != nil || line != `The user's collections, read with collection_query: "gifts" (0 records).` {
		t.Fatalf("bob's turn line = %q %v", line, err)
	}
	if line, err := h.service.TurnLine(h.ctx, "alice"); err != nil || line != `The user's collections, read with collection_query: "Gifts" (5 records).` {
		t.Fatalf("alice's turn line = %q %v", line, err)
	}
}

// holdingProposer records proposals and actions without executing them, so a test can change the
// collection before it confirms one.
type holdingProposer struct {
	mu   sync.Mutex
	held []approval.Proposal
}

func (p *holdingProposer) Propose(_ context.Context, _ string, proposal approval.Proposal) (approval.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held = append(p.held, proposal)
	return approval.Record{}, nil
}

func (p *holdingProposer) Act(ctx context.Context, principalID string, proposal approval.Proposal) (string, error) {
	_, err := p.Propose(ctx, principalID, proposal)
	return approval.ProposedText, err
}

func TestAConfirmedChangeIsCheckedAgainstTheCollectionAsItIsNow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("alice", "collection_create", `{"name":"chores","fields":[{"name":"task","type":"text"}]}`)
	holding := &holdingProposer{}
	held, err := collection.NewService(collection.Config{Location: time.UTC}, h.store, holding, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := held.Execute(h.ctx, "alice", tools.Call{Name: "collection_alter", Arguments: json.RawMessage(`{"collection":"chores","changes":[{"action":"add","field":"owner","type":"person","required":true}]}`)}); err != nil || result.IsError {
		t.Fatalf("proposing a required field in an empty collection = %+v %v", result, err)
	}
	h.ok("alice", "collection_add", `{"collection":"chores","records":[{"task":"water the plants"}]}`)

	_, err = held.ExecuteApproved(h.ctx, approval.Record{ID: "c000000000000001", PrincipalID: "alice", Tool: "collection_alter", Arguments: holding.held[0].Arguments})
	var failed *approval.ActionError
	if !errors.As(err, &failed) || !strings.Contains(failed.Text, `field "owner" cannot be required: the collection already has records`) {
		t.Fatalf("confirming a required field after a record was added = %v", err)
	}
	if fields := h.ok("alice", "collection_list", `{}`); strings.Contains(fields, "owner") {
		t.Fatalf("the refused field was added:\n%s", fields)
	}
}

func TestFieldKeysAreNeverGivenOutTwice(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("alice", "collection_create", `{"name":"chores","fields":[{"name":"task","type":"text"},{"name":"note","type":"text"}]}`)
	keys := func() map[string]string {
		t.Helper()
		collections, err := h.store.ListCollections(h.ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]string{}
		for _, field := range collections[0].Fields {
			found[field.Name] = field.Key
		}
		return found
	}
	before := keys()
	h.ok("alice", "collection_add", `{"collection":"chores","records":[{"task":"sweep","note":"old note"}]}`)

	h.ok("alice", "collection_alter", `{"collection":"chores","changes":[{"action":"remove","field":"note"}]}`)
	h.ok("alice", "collection_alter", `{"collection":"chores","changes":[{"action":"add","field":"remark","type":"text"}]}`)
	remark := keys()["remark"]
	h.ok("alice", "collection_alter", `{"collection":"chores","changes":[{"action":"remove","field":"remark"},{"action":"add","field":"comment","type":"text"}]}`)
	after := keys()
	if after["task"] != before["task"] || after["comment"] == "" || remark == before["note"] || after["comment"] == before["note"] || after["comment"] == remark {
		t.Fatalf("keys before %v, remark %s, after %v", before, remark, after)
	}
	h.ok("alice", "collection_add", `{"collection":"chores","records":[{"task":"dust","comment":"new"}]}`)
	if listed := h.ok("alice", "collection_query", `{"collection":"chores"}`); strings.Contains(listed, "old note") || !strings.Contains(listed, "task: sweep\n") {
		t.Fatalf("a removed field's value came back:\n%s", listed)
	}
}

func TestUndoingAFieldAdditionKeepsValuesWrittenSince(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedGifts()
	h.ok("alice", "collection_alter", `{"collection":"gifts","changes":[{"action":"add","field":"store","type":"text"}]}`)
	payload := h.confirmer.undo["collection_alter"]
	h.ok("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","store":"the corner shop"}]}`)

	_, err := h.service.Undo(h.ctx, approval.Record{PrincipalID: "alice", Tool: "collection_alter"}, payload)
	var refusal *approval.UndoRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, `records already have values in the field "store"`) {
		t.Fatalf("undo after a value was written = %v", err)
	}
	if listed := h.ok("alice", "collection_query", `{"collection":"gifts","filters":[{"field":"store","op":"not_empty"}]}`); !strings.Contains(listed, "store: the corner shop") {
		t.Fatalf("the value was dropped:\n%s", listed)
	}
}

func TestBatchUndoSkipsRemovedRecordsAndRefusesChangedOnes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedGifts()
	undoAdd := func() (approval.Outcome, error) {
		return h.service.Undo(h.ctx, approval.Record{PrincipalID: "alice", Tool: "collection_add"}, h.confirmer.undo["collection_add"])
	}
	idOf := func(idea string) string {
		t.Helper()
		found := h.ok("alice", "collection_query", fmt.Sprintf(`{"collection":"gifts","filters":[{"field":"idea","op":"=","value":%q}]}`, idea))
		start := strings.Index(found, "- [")
		return found[start+3 : start+19]
	}

	h.ok("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","idea":"tea"},{"person":"Ana","idea":"mug"}]}`)
	h.ok("alice", "collection_remove", fmt.Sprintf(`{"collection":"gifts","record_id":%q}`, idOf("tea")))
	if outcome, err := undoAdd(); err != nil || outcome.Text != "Removed 1 record." {
		t.Fatalf("undo with one record already removed = %+v %v", outcome, err)
	}

	h.ok("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","idea":"pen"},{"person":"Ana","idea":"ink"}]}`)
	h.ok("alice", "collection_update", fmt.Sprintf(`{"collection":"gifts","record_id":%q,"values":{"bought":true}}`, idOf("ink")))
	var refusal *approval.UndoRefusal
	if _, err := undoAdd(); !errors.As(err, &refusal) {
		t.Fatalf("undo with one record changed = %v", err)
	}
	if count := h.ok("alice", "collection_query", `{"collection":"gifts","limit":0}`); !strings.Contains(count, "7 of 7") {
		t.Fatalf("a refused undo removed records:\n%s", count)
	}
	h.refused("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","rating":1e13}]}`, `field "rating" needs a number between`)
	h.refused("alice", "collection_add", `{"collection":"gifts","records":[{"person":"Ana","price":{"amount":2e12,"currency":"EUR"}}]}`, `field "price" needs an amount of at most`)
}

// currencyCode is the n-th three-letter code: AAA, AAB, and so on.
func currencyCode(n int) string {
	return string([]byte{byte('A' + n/676%26), byte('A' + n/26%26), byte('A' + n%26)})
}

func TestLargeTotalsAndGroupsStayWithinTheResultCap(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("alice", "collection_create", `{"name":"spending","fields":[{"name":"shop","type":"text"},{"name":"paid","type":"money"}]}`)
	h.ok("alice", "collection_create", `{"name":"travel","fields":[{"name":"trip","type":"text"},{"name":"paid","type":"money"}]}`)
	collections, err := h.store.ListCollections(h.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(c collection.Collection, group func(int) string, currency func(int) string) {
		t.Helper()
		records := make([]collection.Record, 0, collection.MaximumRecords)
		for index := range collection.MaximumRecords {
			records = append(records, collection.Record{
				ID: fmt.Sprintf("%s%04d", c.ID[4:], index), PrincipalID: "alice", CollectionID: c.ID, CreatedAt: h.now, UpdatedAt: h.now,
				Values: map[string]json.RawMessage{
					c.Fields[0].Key: json.RawMessage(fmt.Sprintf("%q", group(index))),
					c.Fields[1].Key: json.RawMessage(fmt.Sprintf(`{"amount":%d.25,"currency":%q}`, index, currency(index))),
				},
			})
		}
		if _, err := h.store.CreateRecords(h.ctx, records, collection.MaximumRecords); err != nil {
			t.Fatal(err)
		}
	}
	seed(collections[0], func(n int) string { return "shop " + strings.Repeat("x", 400) + fmt.Sprint(n%50) }, func(n int) string { return currencyCode(n / 50 % 40) })
	seed(collections[1], func(int) string { return "trip" }, currencyCode)

	grouped := h.ok("alice", "collection_query", `{"collection":"spending","group_by":"shop"}`)
	if len(grouped) > 64<<10 || !strings.Contains(grouped, "more groups") || !strings.Contains(grouped, "- paid: 50 with a value") || strings.Contains(grouped, strings.Repeat("x", 120)) {
		t.Fatalf("grouped result of %d bytes:\n%.600s", len(grouped), grouped)
	}
	totals := h.ok("alice", "collection_query", `{"collection":"travel","group_by":"trip"}`)
	if len(totals) > 64<<10 || !strings.Contains(totals, "more totals") || !strings.Contains(totals, "- and 1 more groups") || !strings.Contains(totals, "Listed 0 of 2000 matches.") {
		t.Fatalf("totals result of %d bytes:\n%.300s\n...\n%s", len(totals), totals, totals[max(0, len(totals)-300):])
	}
}

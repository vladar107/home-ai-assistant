package intake_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/intake"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

var recordIDPattern = regexp.MustCompile(`intake record ([0-9a-f]{16})`)

// cleanTurn stands in for the approval service in a chat turn that has read no outside content:
// every change is made at once, as Confirm would make it.
type cleanTurn struct {
	service *intake.Service
	acted   []string
}

func (c *cleanTurn) Act(ctx context.Context, principalID string, proposal approval.Proposal) (string, error) {
	c.acted = append(c.acted, proposal.Tool)
	outcome, err := c.service.ExecuteApproved(ctx, approval.Record{ID: "0000000000000001", PrincipalID: principalID, Tool: proposal.Tool, Arguments: proposal.Arguments})
	if err != nil {
		return "", &approval.FailedError{Text: err.Error()}
	}
	return "Done: " + outcome.Text, nil
}

type fixture struct {
	t       *testing.T
	service *intake.Service
	turn    *cleanTurn
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, now: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), turn: &cleanTurn{}}
	service, err := intake.NewService(intake.Config{Location: time.UTC, Now: func() time.Time {
		f.now = f.now.Add(time.Minute)
		return f.now
	}}, store, f.turn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.service, f.turn.service = service, service
	return f
}

func (f *fixture) call(principalID, name, arguments string) tools.Result {
	f.t.Helper()
	result, err := f.service.Execute(f.t.Context(), principalID, tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *fixture) record(principalID, arguments string) string {
	f.t.Helper()
	result := f.call(principalID, "intake_record", arguments)
	match := recordIDPattern.FindStringSubmatch(result.Text)
	if result.IsError || match == nil {
		f.t.Fatalf("intake_record %s = %+v", arguments, result)
	}
	return match[1]
}

const invoice = `{"source":"file","source_ref":"a1b2c3d4","kind":"Invoice","title":"Electricity for September","sender":"Example Energy (billing@example.com)",` +
	`"dates":[{"label":"payment due","date":"2026-10-15"}],"amounts":[{"label":"total","value":"84.20","currency":"eur"}],` +
	`"references":[{"label":"invoice number","value":"INV-2026-0917"}],"action_required":"Pay 84.20 EUR by 15 October."}`

// A record keeps the facts read from one item, once per item: the same attachment again updates it,
// a forwarded message is its own item each time, and a draft that cannot be kept keeps nothing.
func TestIntakeRecordKeepsOneRecordPerItem(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	id := f.record("alice", invoice)
	if same := f.call("alice", "intake_record", invoice); same.IsError || !strings.Contains(same.Text, "already holds this item exactly") || len(f.turn.acted) != 0 {
		t.Fatalf("the same file again = %+v, acted %v", same, f.turn.acted)
	}
	again := f.call("alice", "intake_record", strings.Replace(invoice, "Pay 84.20 EUR by 15 October.", "Paid already.", 1))
	if again.IsError || !strings.Contains(again.Text, "Done: Updated the intake record "+id) || len(f.turn.acted) != 1 {
		t.Fatalf("second record of the same file = %+v, acted %v", again, f.turn.acted)
	}
	forward := `{"source":"forward","kind":"appointment","title":"Dentist check-up","dates":[{"label":"appointment","date":"2026-10-20 09:30:00"}]}`
	first, second := f.record("alice", forward), f.record("alice", forward)
	if first == second {
		t.Fatalf("two forwarded messages share the record %s", first)
	}
	if shown := f.call("alice", "intake_get", `{"id":"`+first+`"}`); !strings.Contains(shown.Text, "- appointment: 2026-10-20 09:30\n") {
		t.Fatalf("a date with seconds = %q", shown.Text)
	}

	for arguments, want := range map[string]string{
		`{"source":"file","kind":"invoice","title":"x"}`:                                                                                "source_ref is required",
		`{"source":"fax","source_ref":"r","kind":"invoice","title":"x"}`:                                                                "source must be",
		`{"source":"photo","source_ref":"p1","kind":"invoice","title":"x","dates":[{"label":"due","date":"15 Oct"}]}`:                   "a date must be",
		`{"source":"photo","source_ref":"p1","kind":"invoice","title":"x","amounts":[{"label":"total","value":"1","currency":"euro"}]}`: "three-letter",
		`{"source":"photo","source_ref":"p1","kind":"invoice","title":"https://example.com"}`:                                           "title is required",
	} {
		if result := f.call("alice", "intake_record", arguments); !result.IsError || !strings.Contains(result.Text, want) || !strings.Contains(result.Text, "Nothing was kept") {
			t.Fatalf("intake_record %s = %+v, want a refusal with %q", arguments, result, want)
		}
	}

	full := f.call("alice", "intake_get", `{"id":"`+id+`"}`)
	for _, want := range []string{
		"Intake record " + id + ", kept 2026-10-05", "from a file.", "Electricity for September (Invoice), from Example Energy",
		"- payment due: 2026-10-15", "- total: 84.20 EUR", "- invoice number: INV-2026-0917", "To do: Paid already.",
	} {
		if full.IsError || !strings.Contains(full.Text, want) {
			t.Fatalf("intake_get lacks %q:\n%s", want, full.Text)
		}
	}
	if strings.Contains(full.Text, "example.com") {
		t.Fatalf("a web address was kept:\n%s", full.Text)
	}

	invoices := f.call("alice", "intake_list", `{"kind":"INVOICE"}`)
	if invoices.IsError || strings.Count(invoices.Text, "\n- ") != 1 || !strings.Contains(invoices.Text, "["+id+"]") || !strings.Contains(invoices.Text, "payment due 2026-10-15") {
		t.Fatalf("invoices = %q", invoices.Text)
	}
	if newest := f.call("alice", "intake_list", `{"limit":2}`); strings.Count(newest.Text, "\n- ") != 2 || strings.Contains(newest.Text, id) {
		t.Fatalf("the two newest records = %q", newest.Text)
	}
	if none := f.call("alice", "intake_list", `{"kind":"warranty"}`); none.IsError || !strings.Contains(none.Text, `No intake records of the kind "warranty"`) {
		t.Fatalf("no warranties = %+v", none)
	}

	if deleted := f.call("alice", "intake_delete", `{"id":"`+id+`"}`); deleted.IsError {
		t.Fatalf("delete = %+v", deleted)
	}
	if gone := f.call("alice", "intake_get", `{"id":"`+id+`"}`); !gone.IsError {
		t.Fatalf("deleted record = %+v", gone)
	}
}

// Another principal's records are never listed, shown, or deleted.
func TestIntakeRecordsAreThePrincipalsOwn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.record("alice", invoice)

	if listed := f.call("bob", "intake_list", `{}`); listed.IsError || listed.Text != "No intake records yet." {
		t.Fatalf("bob's records = %+v", listed)
	}
	if shown := f.call("bob", "intake_get", `{"id":"`+id+`"}`); !shown.IsError || strings.Contains(shown.Text, "Electricity") {
		t.Fatalf("bob read alice's record: %+v", shown)
	}
	if deleted := f.call("bob", "intake_delete", `{"id":"`+id+`"}`); !deleted.IsError {
		t.Fatalf("bob deleted alice's record: %+v", deleted)
	}
	bobs := f.record("bob", invoice)
	if bobs == id {
		t.Fatal("the same attachment id across principals shares a record")
	}
	if shown := f.call("alice", "intake_get", `{"id":"`+id+`"}`); shown.IsError {
		t.Fatalf("alice's record after bob's calls = %+v", shown)
	}
}

// The read tools carry outside content, so a turn that reads a record is exposed; the record tool
// is a write, so a read-only turn has none.
func TestIntakeToolsMarkTheirResultsAsOutsideContent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	contribution, err := f.service.Contribute(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	risks := map[string]tools.Definition{}
	for _, definition := range contribution.Definitions {
		risks[definition.Name] = definition
	}
	for _, name := range []string{"intake_list", "intake_get"} {
		if definition := risks[name]; definition.Risk != tools.RiskRead || !definition.ThirdParty {
			t.Fatalf("%s = %+v, want a third-party read", name, definition)
		}
	}
	for _, name := range []string{"intake_record", "intake_delete"} {
		if definition := risks[name]; definition.Risk != tools.RiskWrite || definition.ThirdParty {
			t.Fatalf("%s = %+v, want a write", name, definition)
		}
	}
	if !strings.Contains(contribution.WriteGuidance, "collection_add") || !strings.Contains(contribution.WriteGuidance, "at most five") {
		t.Fatalf("write guidance = %q", contribution.WriteGuidance)
	}
}

package notion_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const token = "ntn_connectortoken"

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

// recorder is the approval service as the connector sees it. It keeps every proposal, so a test
// can prove that a refused write records nothing.
type recorder struct {
	proposals []approval.Proposal
}

func (r *recorder) Propose(_ context.Context, principalID string, proposal approval.Proposal) (approval.Record, error) {
	r.proposals = append(r.proposals, proposal)
	return approval.Record{ID: fmt.Sprintf("approval-%d", len(r.proposals)), PrincipalID: principalID, Tool: proposal.Tool, Arguments: proposal.Arguments, Summary: proposal.Summary}, nil
}

type fixture struct {
	t         *testing.T
	ctx       context.Context
	sim       *notionsim.Simulator
	service   *notion.Service
	proposals *recorder
	logs      *lockedBuffer
	// now and displayNames are the clock and the principal names the service reads; a test may
	// change them.
	now          *time.Time
	displayNames map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	credentialsDir := filepath.Join(t.TempDir(), "integrations")
	credentials, err := integration.NewCredentials(credentialsDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.Write("alice", "notion", token); err != nil {
		t.Fatal(err)
	}
	sim := notionsim.New(t)
	sim.AddToken(token, notionsim.Token{Kind: "internal", BotName: "Connector", WorkspaceID: "ws", WorkspaceName: "Workspace"})
	logs := &lockedBuffer{}
	proposals := &recorder{}
	zone, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.October, 3, 20, 0, 0, 0, zone)
	displayNames := map[string]string{"alice": "Alice"}
	service, err := notion.NewService(notion.Config{
		CredentialsDir: credentialsDir, APIBaseURL: sim.URL(), HTTPClient: sim.Client(),
		DisplayNames: displayNames, Location: zone, Now: func() time.Time { return now },
	}, store, proposals, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, ctx: ctx, sim: sim, service: service, proposals: proposals, logs: logs, now: &now, displayNames: displayNames}
}

func (f *fixture) execute(name, arguments string) tools.Result {
	f.t.Helper()
	result, err := f.service.Execute(f.ctx, "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		f.t.Fatal(err)
	}
	if strings.Contains(result.Text, "synthetic-notion-error-text") || strings.Contains(result.Text, token) {
		f.t.Fatalf("result exposes provider text or the token: %+v", result)
	}
	return result
}

func (f *fixture) requests(path string) []notionsim.Request {
	var matching []notionsim.Request
	for _, request := range f.sim.Requests() {
		if request.Path == path {
			matching = append(matching, request)
		}
	}
	return matching
}

// refuse runs a write the schema must reject and proves that nothing was proposed for it.
func (f *fixture) refuse(name, arguments, want string) {
	f.t.Helper()
	before := len(f.proposals.proposals)
	result := f.execute(name, arguments)
	if !result.IsError || !strings.Contains(result.Text, want) {
		f.t.Fatalf("%s = %+v, want a failure naming %q", name, result, want)
	}
	if recorded := len(f.proposals.proposals) - before; recorded != 0 {
		f.t.Fatalf("a refused write recorded %d proposal(s)", recorded)
	}
}

func (f *fixture) proposal() approval.Proposal {
	f.t.Helper()
	if len(f.proposals.proposals) == 0 {
		f.t.Fatal("no proposal was recorded")
	}
	return f.proposals.proposals[len(f.proposals.proposals)-1]
}

// enableWrites adopts the connection and switches the principal's write grant on.
func (f *fixture) enableWrites() {
	f.t.Helper()
	f.contribute()
	if status, err := f.service.SetWrites(f.ctx, "alice", true); err != nil || !status.WritesEnabled {
		f.t.Fatalf("enable writes = %+v %v", status, err)
	}
}

func (f *fixture) contribute() tools.Contribution {
	f.t.Helper()
	contribution, err := f.service.Contribute(f.ctx, "alice")
	if err != nil {
		f.t.Fatal(err)
	}
	return contribution
}

func TestPageSizeCapAndCursor(t *testing.T) {
	f := newFixture(t)
	for index := 1; index <= 27; index++ {
		f.sim.SharePage(token, notionsim.Page{ID: fmt.Sprintf("page-%02d", index), Title: fmt.Sprintf("Item %02d", index), URL: "https://notion.so/p", LastEdited: "2026-09-01T00:00:00.000Z"})
	}
	if contribution := f.contribute(); len(contribution.Definitions) != 5 {
		t.Fatalf("contribution = %+v", contribution)
	}
	first := f.execute("notion_search", `{"limit":50}`)
	if first.IsError || !strings.Contains(first.Text, "25 result(s); more available with cursor cursor-25") || !strings.Contains(first.Text, "Item 25") || strings.Contains(first.Text, "Item 26") {
		t.Fatalf("first page = %+v", first)
	}
	searches := f.requests("/v1/search")
	if len(searches) != 1 || searches[0].Body["page_size"] != float64(25) || searches[0].Token != token {
		t.Fatalf("search requests = %+v", searches)
	}
	second := f.execute("notion_search", `{"cursor":"cursor-25"}`)
	if second.IsError || !strings.Contains(second.Text, "2 result(s)") || !strings.Contains(second.Text, "Item 27") || strings.Contains(second.Text, "more available") {
		t.Fatalf("second page = %+v", second)
	}
	if searches := f.requests("/v1/search"); searches[1].Body["start_cursor"] != "cursor-25" || searches[1].Body["page_size"] != float64(10) {
		t.Fatalf("cursor request = %+v", searches[1])
	}
}

func TestRateLimitRetryAndErrorClasses(t *testing.T) {
	f := newFixture(t)
	f.sim.SharePage(token, notionsim.Page{ID: "page-1", Title: "One", URL: "https://notion.so/1", LastEdited: "2026-09-01T00:00:00.000Z"})
	f.contribute()
	f.sim.FailNext(429, "1")
	if result := f.execute("notion_search", `{}`); result.IsError || !strings.Contains(result.Text, "One") {
		t.Fatalf("retried search = %+v", result)
	}
	if searches := f.requests("/v1/search"); len(searches) != 2 {
		t.Fatalf("expected one retry, got %d search requests", len(searches))
	}
	for _, tc := range []struct {
		status     int
		retryAfter string
		want       string
	}{
		{429, "30", "rate limiting"},
		{529, "", "rate limiting"},
		{401, "", "rejected the connection token"},
		{403, "", "lacks the capability"},
		{500, "", "unavailable"},
		{503, "", "unavailable"},
	} {
		before := len(f.requests("/v1/search"))
		f.sim.FailNext(tc.status, tc.retryAfter)
		result := f.execute("notion_search", `{}`)
		if !result.IsError || !strings.Contains(result.Text, tc.want) {
			t.Fatalf("status %d = %+v, want %q", tc.status, result, tc.want)
		}
		if after := len(f.requests("/v1/search")); after != before+1 {
			t.Fatalf("status %d retried: %d requests", tc.status, after-before)
		}
	}
	if result := f.execute("notion_get_page", `{"page_id":"missing"}`); !result.IsError || !strings.Contains(result.Text, "no shared page") {
		t.Fatalf("missing page = %+v", result)
	}
	if result := f.execute("notion_search", `{"object_type":"user"}`); !result.IsError || !strings.Contains(result.Text, "object_type") {
		t.Fatalf("invalid object type = %+v", result)
	}
	if logs := f.logs.String(); strings.Contains(logs, "synthetic-notion-error-text") || strings.Contains(logs, token) || strings.Contains(logs, "One") {
		t.Fatalf("logs contain provider or page content: %s", logs)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "error_class=unauthorized") || !strings.Contains(logs, "error_class=rate_limited") {
		t.Fatalf("logs lack error classes: %s", logs)
	}
}

func TestPageContentIsFlattenedAndCapped(t *testing.T) {
	f := newFixture(t)
	f.sim.SharePage(token, notionsim.Page{
		ID: "page-notes", Title: "Notes", URL: "https://notion.so/notes", LastEdited: "2026-09-01T00:00:00.000Z",
		Properties: map[string]any{"Tags": notionsim.RichTextProperty("home, weekly"), "Done": notionsim.CheckboxProperty(true)},
		Blocks:     []map[string]any{notionsim.Heading(1, "Title"), notionsim.Paragraph("Intro text"), notionsim.Bullet("one"), notionsim.ToDo("two", true)},
	})
	var blocks []map[string]any
	for index := range 300 {
		blocks = append(blocks, notionsim.Paragraph(fmt.Sprintf("paragraph %03d %s", index, strings.Repeat("x", 90))))
	}
	f.sim.SharePage(token, notionsim.Page{ID: "page-long", Title: "Long", URL: "https://notion.so/long", LastEdited: "2026-09-01T00:00:00.000Z", Blocks: blocks})
	f.contribute()
	page := f.execute("notion_get_page", `{"page_id":"page-notes"}`)
	for _, want := range []string{"Page: Notes", "# Title", "Intro text", "- one", "[x] two", "Tags: home, weekly", "Done: true"} {
		if !strings.Contains(page.Text, want) {
			t.Fatalf("page rendering lacks %q: %s", want, page.Text)
		}
	}
	if withoutContent := f.execute("notion_get_page", `{"page_id":"page-notes","include_content":false}`); strings.Contains(withoutContent.Text, "Content:") || !strings.Contains(withoutContent.Text, "Tags: home, weekly") {
		t.Fatalf("properties-only rendering = %+v", withoutContent)
	}
	long := f.execute("notion_get_page", `{"page_id":"page-long"}`)
	if long.IsError || len(long.Text) > 16<<10 || !strings.Contains(long.Text, "[content truncated]") || !strings.Contains(long.Text, "paragraph 000") {
		t.Fatalf("long page rendering: error=%v bytes=%d truncated=%v", long.IsError, len(long.Text), strings.Contains(long.Text, "[content truncated]"))
	}
}

func TestConnectRejectsUnknownTokensWithoutStoringThem(t *testing.T) {
	f := newFixture(t)
	if status, err := f.service.Status(f.ctx, "alice"); err != nil || !status.Connected || status.TokenKind != "internal" || status.WorkspaceName != "Workspace" {
		t.Fatalf("adopted status = %+v err=%v", status, err)
	}
	if _, err := f.service.Connect(f.ctx, "alice", "ntn_unknown"); !errors.Is(err, notion.ErrInvalidToken) {
		t.Fatalf("unknown token error = %v", err)
	}
	if _, err := f.service.Connect(f.ctx, "alice", "not a token"); !errors.Is(err, notion.ErrInvalidToken) {
		t.Fatalf("malformed token error = %v", err)
	}
	if status, err := f.service.Status(f.ctx, "alice"); err != nil || !status.Connected {
		t.Fatalf("status after rejected connects = %+v err=%v", status, err)
	}
	if calls := f.requests("/v1/users/me"); len(calls) != 2 || calls[1].Token != "ntn_unknown" {
		t.Fatalf("verification requests = %+v", calls)
	}
	if removed, err := f.service.Disconnect(f.ctx, "alice"); err != nil || !removed {
		t.Fatalf("disconnect = %v %v", removed, err)
	}
	if contribution := f.contribute(); len(contribution.Definitions) != 0 {
		t.Fatalf("contribution after disconnect = %+v", contribution)
	}
	if result := f.execute("notion_search", `{}`); !result.IsError || result.Text != notion.NotConnectedText {
		t.Fatalf("execution after disconnect = %+v", result)
	}
}

func TestTableNestedInAPageIsReachableThroughItsDatabaseID(t *testing.T) {
	f := newFixture(t)
	f.sim.ShareDataSource(token, notionsim.DataSource{
		ID: "ds-movies", Title: "Movie nights", URL: "https://notion.so/ds-movies",
		Properties: map[string]any{"Name": notionsim.SchemaProperty("title"), "Week": notionsim.SchemaProperty("rich_text")},
	})
	f.sim.ShareDatabase(token, notionsim.Database{
		ID: "db-movies", Title: "Movie nights", IsInline: true,
		DataSources: []notionsim.DataSourceRef{{ID: "ds-movies", Name: "Movie nights"}},
	})
	f.sim.SharePage(token, notionsim.Page{
		ID: "page-movies", Title: "Кино вечера", URL: "https://notion.so/page-movies", LastEdited: "2026-09-01T00:00:00.000Z",
		Blocks: []map[string]any{notionsim.Paragraph("Расписание"), notionsim.ChildDatabase("db-movies", "Movie nights")},
	})
	f.sim.SharePage(token, notionsim.Page{
		ID: "row-dune", Title: "Dune", URL: "https://notion.so/row-dune", LastEdited: "2026-09-01T00:00:00.000Z",
		DataSourceID: "ds-movies",
		Properties:   map[string]any{"Name": notionsim.TitleProperty("Dune"), "Week": notionsim.RichTextProperty("next")},
	})
	f.contribute()

	page := f.execute("notion_get_page", `{"page_id":"page-movies"}`)
	if page.IsError || !strings.Contains(page.Text, "[database] Movie nights (database id: db-movies)") {
		t.Fatalf("page content hides the nested database: %s", page.Text)
	}

	rows := f.execute("notion_query_data_source", `{"data_source_id":"db-movies"}`)
	if rows.IsError || !strings.Contains(rows.Text, "Dune") || !strings.Contains(rows.Text, "Week: next") {
		t.Fatalf("rows through the database ID = %+v", rows)
	}
	if rejected := f.requests("/v1/data_sources/db-movies/query"); len(rejected) != 1 {
		t.Fatalf("the database ID must be tried as a data source first: %d attempt(s)", len(rejected))
	}
	if lookups := f.requests("/v1/databases/db-movies"); len(lookups) != 1 {
		t.Fatalf("database lookups = %d, want exactly one resolution hop", len(lookups))
	}
	if queries := f.requests("/v1/data_sources/ds-movies/query"); len(queries) != 1 {
		t.Fatalf("data source queries = %d", len(queries))
	}
}

func TestSimpleTableRowsAreReadThroughTheirBlockID(t *testing.T) {
	f := newFixture(t)
	f.sim.SharePage(token, notionsim.Page{
		ID: "page-films", Title: "Кино вечера", URL: "https://notion.so/page-films", LastEdited: "2026-09-01T00:00:00.000Z",
		Blocks: []map[string]any{
			notionsim.Paragraph("Очередь"),
			notionsim.Table(notionsim.TableRow("Фильм", "Неделя"), notionsim.TableRow("Dune", "next")),
		},
	})
	f.contribute()

	page := f.execute("notion_get_page", `{"page_id":"page-films"}`)
	if page.IsError || !strings.Contains(page.Text, "[table] (has nested content; block id: page-films-block-1)") {
		t.Fatalf("page content hides the table's block ID: %s", page.Text)
	}

	rows := f.execute("notion_get_block_children", `{"block_id":"page-films-block-1"}`)
	if rows.IsError || !strings.Contains(rows.Text, "| Фильм | Неделя |") || !strings.Contains(rows.Text, "| Dune | next |") {
		t.Fatalf("table rows = %+v", rows)
	}
	if missing := f.execute("notion_get_block_children", `{"block_id":"page-films-block-9"}`); !missing.IsError {
		t.Fatalf("an unknown block ID must fail: %+v", missing)
	}
	if own := f.execute("notion_get_block_children", `{"block_id":"page-films"}`); own.IsError || !strings.Contains(own.Text, "Очередь") {
		t.Fatalf("a page ID must resolve to its own blocks: %+v", own)
	}
}

func TestDataSourceIDIsQueriedWithoutADatabaseLookup(t *testing.T) {
	f := newFixture(t)
	f.sim.ShareDataSource(token, notionsim.DataSource{
		ID: "ds-movies", Title: "Movie nights", URL: "https://notion.so/ds-movies",
		Properties: map[string]any{"Name": notionsim.SchemaProperty("title")},
	})
	f.sim.SharePage(token, notionsim.Page{
		ID: "row-dune", Title: "Dune", URL: "https://notion.so/row-dune", LastEdited: "2026-09-01T00:00:00.000Z",
		DataSourceID: "ds-movies", Properties: map[string]any{"Name": notionsim.TitleProperty("Dune")},
	})
	f.contribute()

	rows := f.execute("notion_query_data_source", `{"data_source_id":"ds-movies"}`)
	if rows.IsError || !strings.Contains(rows.Text, "Dune") {
		t.Fatalf("direct query = %+v", rows)
	}
	if queries := f.requests("/v1/data_sources/ds-movies/query"); len(queries) != 1 {
		t.Fatalf("data source queries = %d, want one", len(queries))
	}
	for _, request := range f.sim.Requests() {
		if strings.HasPrefix(request.Path, "/v1/databases/") {
			t.Fatalf("a data source ID must not trigger a database lookup: %s", request.Path)
		}
	}

	missing := f.execute("notion_query_data_source", `{"data_source_id":"ds-unknown"}`)
	if !missing.IsError || strings.Contains(missing.Text, "synthetic-notion-error-text") {
		t.Fatalf("unknown ID = %+v", missing)
	}
}

func TestDatabaseWithSeveralDataSourcesAsksWhichOne(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"ds-2026", "ds-2025"} {
		f.sim.ShareDataSource(token, notionsim.DataSource{ID: id, Title: id, URL: "https://notion.so/" + id, Properties: map[string]any{"Name": notionsim.SchemaProperty("title")}})
	}
	f.sim.ShareDatabase(token, notionsim.Database{
		ID: "db-seasons", Title: "Seasons",
		DataSources: []notionsim.DataSourceRef{{ID: "ds-2026", Name: "2026"}, {ID: "ds-2025", Name: "2025"}},
	})
	f.contribute()

	result := f.execute("notion_query_data_source", `{"data_source_id":"db-seasons"}`)
	if result.IsError {
		t.Fatalf("ambiguous database = %+v", result)
	}
	for _, want := range []string{"holds 2 data sources", "id: ds-2026", "id: ds-2025", "2026", "2025"} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("choice rendering lacks %q: %s", want, result.Text)
		}
	}
	for _, request := range f.sim.Requests() {
		if strings.HasPrefix(request.Path, "/v1/data_sources/ds-") {
			t.Fatalf("an ambiguous database must not query a data source: %s", request.Path)
		}
	}
}

func TestDataSourceOptionsAreVisibleInSearchAndOnItsRows(t *testing.T) {
	f := newFixture(t)
	f.sim.ShareDataSource(token, notionsim.DataSource{
		ID: "ds-mentoring", Title: "Mentoring", URL: "https://notion.so/ds-mentoring",
		Properties: map[string]any{
			"Name":   notionsim.SchemaProperty("title"),
			"Week":   notionsim.SchemaProperty("rich_text"),
			"Status": notionsim.SchemaPropertyWithOptions("status", "Not started", "In progress", "Done"),
			"Tags":   notionsim.SchemaPropertyWithOptions("multi_select", "home", "work"),
		},
	})
	f.sim.SharePage(token, notionsim.Page{
		ID: "row-session", Title: "Session", URL: "https://notion.so/row-session", LastEdited: "2026-09-01T00:00:00.000Z",
		DataSourceID: "ds-mentoring",
		Properties:   map[string]any{"Status": notionsim.StatusProperty("In progress")},
	})
	f.contribute()

	found := f.execute("notion_search", `{"object_type":"data_source"}`)
	for _, want := range []string{"Status (status: Not started | In progress | Done)", "Tags (multi_select: home | work)", "Week (rich_text)"} {
		if found.IsError || !strings.Contains(found.Text, want) {
			t.Fatalf("search result lacks %q: %s", want, found.Text)
		}
	}

	row := f.execute("notion_get_page", `{"page_id":"row-session"}`)
	for _, want := range []string{"Page: Session", "Status: In progress", "Structure: ", "Status (status: Not started | In progress | Done)"} {
		if row.IsError || !strings.Contains(row.Text, want) {
			t.Fatalf("row rendering lacks %q: %s", want, row.Text)
		}
	}
	if retrievals := f.requests("/v1/data_sources/ds-mentoring"); len(retrievals) != 1 {
		t.Fatalf("data source retrievals = %d, want one", len(retrievals))
	}
}

func TestRowRendersWhenItsDataSourceCannotBeRead(t *testing.T) {
	f := newFixture(t)
	f.sim.SharePage(token, notionsim.Page{
		ID: "row-private", Title: "Session", URL: "https://notion.so/row-private", LastEdited: "2026-09-01T00:00:00.000Z",
		DataSourceID: "ds-unshared",
		Properties:   map[string]any{"Week": notionsim.RichTextProperty("next")},
	})
	f.contribute()

	row := f.execute("notion_get_page", `{"page_id":"row-private","include_content":false}`)
	if row.IsError || !strings.Contains(row.Text, "Page: Session") || !strings.Contains(row.Text, "Week: next") {
		t.Fatalf("row with an unreadable data source = %+v", row)
	}
	if strings.Contains(row.Text, "Structure:") {
		t.Fatalf("an unreadable data source must not render a structure: %s", row.Text)
	}
	if retrievals := f.requests("/v1/data_sources/ds-unshared"); len(retrievals) != 1 {
		t.Fatalf("data source retrievals = %d, want one attempt", len(retrievals))
	}
}

// shareMentoringTable shares a data source whose properties carry the options a proposed write is
// checked against, together with one row of it.
func shareMentoringTable(f *fixture) {
	f.sim.ShareDataSource(token, notionsim.DataSource{
		ID: "ds-mentoring", Title: "Mentoring", URL: "https://notion.so/ds-mentoring",
		Properties: map[string]any{
			"Week":    notionsim.SchemaProperty("rich_text"),
			"Status":  notionsim.SchemaPropertyWithOptions("status", "Not started", "In progress", "Done"),
			"Tags":    notionsim.SchemaPropertyWithOptions("multi_select", "home", "work"),
			"Updated": notionsim.SchemaProperty("last_edited_time"),
		},
	})
	f.sim.SharePage(token, notionsim.Page{
		ID: "row-session", Title: "Session", URL: "https://notion.so/row-session", LastEdited: "2026-09-01T00:00:00.000Z",
		DataSourceID: "ds-mentoring",
		Properties:   map[string]any{"Status": notionsim.StatusProperty("In progress")},
	})
}

func TestProposedRowValuesAreCheckedAgainstTheSchemaBeforeAnythingIsRecorded(t *testing.T) {
	f := newFixture(t)
	shareMentoringTable(f)
	f.contribute()

	f.refuse("notion_update_page", `{"page_id":"row-session","properties":{"Status":{"status":{"name":"Completed"}}}}`,
		`property "Status" has no option "Completed"; its options are Not started | In progress | Done`)
	f.refuse("notion_update_page", `{"page_id":"row-session","properties":{"Tags":{"multi_select":[{"name":"home"},{"name":"garden"}]}}}`,
		`property "Tags" has no option "garden"; its options are home | work`)
	f.refuse("notion_update_page", `{"page_id":"row-session","properties":{"State":{"status":{"name":"Done"}}}}`,
		`this table has no property "State"; its settable properties are Name, Status, Tags, Week`)
	f.refuse("notion_update_page", `{"page_id":"row-session","properties":{"Updated":{"last_edited_time":"2026-09-07T00:00:00.000Z"}}}`,
		`property "Updated" is last_edited_time, which Notion computes itself and never accepts on a write`)
	f.refuse("notion_update_page", `{"page_id":"row-session","properties":{"Status":{"select":{"name":"Done"}}}}`,
		`property "Status" is status; send it as {"Status":{"status":{"name":"..."}}}`)

	// Clearing a property names no option, so it passes the check.
	if cleared := f.execute("notion_update_page", `{"page_id":"row-session","properties":{"Status":{"status":null}}}`); cleared.IsError {
		t.Fatalf("clearing a property = %+v", cleared)
	}
	for _, request := range f.sim.Requests() {
		if request.Method != "GET" {
			t.Fatalf("a checked proposal reached Notion with %s %s", request.Method, request.Path)
		}
	}
}

func TestProposedRowChangeShowsCurrentValuesAndExecutesOnConfirmation(t *testing.T) {
	f := newFixture(t)
	shareMentoringTable(f)
	f.enableWrites()

	if result := f.execute("notion_update_page", `{"page_id":"row-session","properties":{"Status":{"status":{"name":"Done"}},"Tags":{"multi_select":[{"name":"home"}]},"Week":{"rich_text":[{"text":{"content":"next"}}]}}}`); result.IsError {
		t.Fatalf("valid update = %+v", result)
	}
	proposal := f.proposal()
	want := "Notion · update page\n**[Session](https://notion.so/row-session)**\nStatus: In progress → Done\nTags: (empty) → home\nWeek: (empty) → next"
	if proposal.Summary != want {
		t.Fatalf("proposal summary = %q, want %q", proposal.Summary, want)
	}
	if reads := f.requests("/v1/pages/row-session"); len(reads) != 1 {
		t.Fatalf("page reads for one proposal = %d, want one", len(reads))
	}

	outcome, err := f.service.ExecuteApproved(f.ctx, approval.Record{PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
	if err != nil || outcome.Text != "Updated." {
		t.Fatalf("confirmed update = %+v %v", outcome, err)
	}
	var written notionsim.Request
	for _, request := range f.sim.Requests() {
		if request.Method == "PATCH" {
			written = request
		}
	}
	properties, _ := written.Body["properties"].(map[string]any)
	status, _ := properties["Status"].(map[string]any)
	option, _ := status["status"].(map[string]any)
	if written.Path != "/v1/pages/row-session" || option["name"] != "Done" {
		t.Fatalf("update request = %+v", written)
	}

	if result := f.execute("notion_update_page", `{"page_id":"row-session","properties":{"Status":{"status":{"name":"Done"}}}}`); result.IsError {
		t.Fatalf("repeated update = %+v", result)
	}
	if summary := f.proposal().Summary; !strings.HasSuffix(summary, "\nStatus: Done (no change)") {
		t.Fatalf("proposal of an unchanged value = %q", summary)
	}
}

func TestProposedPageCreationNamesItsDataSourceAndChecksItsOptions(t *testing.T) {
	f := newFixture(t)
	shareMentoringTable(f)
	f.contribute()

	f.refuse("notion_create_page", `{"parent_type":"data_source","parent_id":"ds-mentoring","title":"October review","properties":{"Status":{"status":{"name":"Started"}}}}`,
		`property "Status" has no option "Started"; its options are Not started | In progress | Done`)

	if result := f.execute("notion_create_page", `{"parent_type":"data_source","parent_id":"ds-mentoring","title":"October review","properties":{"Status":{"status":{"name":"Not started"}}},"paragraphs":["Agenda"]}`); result.IsError {
		t.Fatalf("valid create = %+v", result)
	}
	want := "Notion · create page\n**October review** in **Mentoring**\nStatus: Not started\nBody (1 paragraph(s)):\n1. Agenda"
	if summary := f.proposal().Summary; summary != want {
		t.Fatalf("create summary = %q, want %q", summary, want)
	}
}

func TestProposalsCarryOnWhenTheDataSourceCannotBeRead(t *testing.T) {
	f := newFixture(t)
	f.sim.SharePage(token, notionsim.Page{
		ID: "row-private", Title: "Session", URL: "https://notion.so/row-private", LastEdited: "2026-09-01T00:00:00.000Z",
		DataSourceID: "ds-unshared",
	})
	f.contribute()

	if result := f.execute("notion_update_page", `{"page_id":"row-private","properties":{"Anything":{"multi_select":[{"name":"invented"}]}}}`); result.IsError {
		t.Fatalf("update of a row without a readable schema = %+v", result)
	}
	want := "Notion · update page\n**[Session](https://notion.so/row-private)**\nAnything: (empty) → invented"
	if summary := f.proposal().Summary; summary != want {
		t.Fatalf("proposal summary = %q, want %q", summary, want)
	}
	if retrievals := f.requests("/v1/data_sources/ds-unshared"); len(retrievals) != 1 {
		t.Fatalf("data source retrievals = %d, want one attempt", len(retrievals))
	}

	if result := f.execute("notion_append_blocks", `{"page_id":"row-private","paragraphs":["First","Second"]}`); result.IsError {
		t.Fatalf("append = %+v", result)
	}
	want = "Notion · append 2 paragraph(s)\n**[Session](https://notion.so/row-private)**\nBody (2 paragraph(s)):\n1. First\n2. Second"
	if summary := f.proposal().Summary; summary != want {
		t.Fatalf("append summary = %q, want %q", summary, want)
	}
}

func TestARejectedWriteNamesTheOptionThatNoLongerMatches(t *testing.T) {
	f := newFixture(t)
	shareMentoringTable(f)
	f.enableWrites()
	if result := f.execute("notion_update_page", `{"page_id":"row-session","properties":{"Status":{"status":{"name":"Done"}}}}`); result.IsError {
		t.Fatalf("valid update = %+v", result)
	}
	record := approval.Record{PrincipalID: "alice", Tool: f.proposal().Tool, Arguments: f.proposal().Arguments}

	// The option is removed in Notion between the proposal and its confirmation.
	f.sim.SetDataSourceProperties("ds-mentoring", map[string]any{"Status": notionsim.SchemaPropertyWithOptions("status", "Not started", "In progress")})
	f.sim.FailNext(400, "")
	var actionErr *approval.ActionError
	_, err := f.service.ExecuteApproved(f.ctx, record)
	if !errors.As(err, &actionErr) || !strings.Contains(actionErr.Text, `property "Status" has no option "Done"; its options are Not started | In progress`) {
		t.Fatalf("rejected write = %v", err)
	}

	// A rejection the schema does not explain keeps the general sentence.
	f.sim.SetDataSourceProperties("ds-mentoring", map[string]any{"Status": notionsim.SchemaPropertyWithOptions("status", "Not started", "In progress", "Done")})
	f.sim.FailNext(400, "")
	_, err = f.service.ExecuteApproved(f.ctx, record)
	if !errors.As(err, &actionErr) || actionErr.Text != "Notion rejected the request; nothing was written." {
		t.Fatalf("unexplained rejection = %v", err)
	}
	if logs := f.logs.String(); strings.Contains(logs, "synthetic-notion-error-text") || strings.Contains(logs, "Done") {
		t.Fatalf("logs carry provider text or content: %s", logs)
	}
}

func TestQueryResultsStateTheirOrder(t *testing.T) {
	f := newFixture(t)
	f.sim.ShareDataSource(token, notionsim.DataSource{
		ID: "ds-movies", Title: "Movie nights", URL: "https://notion.so/ds-movies",
		Properties: map[string]any{"Name": notionsim.SchemaProperty("title")},
	})
	f.sim.SharePage(token, notionsim.Page{
		ID: "row-dune", Title: "Dune", URL: "https://notion.so/row-dune", LastEdited: "2026-09-01T00:00:00.000Z",
		DataSourceID: "ds-movies", Properties: map[string]any{"Name": notionsim.TitleProperty("Dune")},
	})
	f.contribute()

	unsorted := f.execute("notion_query_data_source", `{"data_source_id":"ds-movies"}`)
	if unsorted.IsError || !strings.HasPrefix(unsorted.Text, "Data source entries (API order, not the view's order; query with sorts for a defined order): 1 result(s)") {
		t.Fatalf("unsorted query = %+v", unsorted)
	}
	sorted := f.execute("notion_query_data_source", `{"data_source_id":"ds-movies","sorts":[{"property":"Date","direction":"ascending"},{"timestamp":"created_time","direction":"descending"}]}`)
	if sorted.IsError || !strings.HasPrefix(sorted.Text, "Data source entries (sorted by Date ascending, then created time descending): 1 result(s)") {
		t.Fatalf("sorted query = %+v", sorted)
	}
}

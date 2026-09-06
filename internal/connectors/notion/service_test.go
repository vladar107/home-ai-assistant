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

type fixture struct {
	t       *testing.T
	ctx     context.Context
	sim     *notionsim.Simulator
	service *notion.Service
	logs    *lockedBuffer
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
	service, err := notion.NewService(notion.Config{CredentialsDir: credentialsDir, APIBaseURL: sim.URL(), HTTPClient: sim.Client()}, store, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, ctx: ctx, sim: sim, service: service, logs: logs}
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
	if contribution := f.contribute(); len(contribution.Definitions) != 3 {
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

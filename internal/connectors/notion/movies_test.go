package notion_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// shareMovieTable mirrors the household table: the title holds the date, the Cyrillic К column
// says who picked the film, and the Cyrillic В and Latin K columns hold each user's score.
func shareMovieTable(f *fixture) {
	f.sim.ShareDataSource(token, notionsim.DataSource{
		ID: "ds-films", Title: "Кино вечера", URL: "https://notion.so/ds-films",
		Properties: map[string]any{
			"Фильм": notionsim.SchemaProperty("rich_text"),
			"К":     notionsim.SchemaProperty("rich_text"),
			"В":     notionsim.SchemaProperty("rich_text"),
			"K":     notionsim.SchemaProperty("rich_text"),
		},
	})
	rows := []struct{ id, date, picker, film, vlad, kim string }{
		{"row-marley", "24.03.2023", "К", "Марли и я", "7", "7"},
		{"row-dune", "26.09.2026", "В", "Дюна", "8", ""},
		{"row-whale", "03.10.2026", "К", "Кит", "", ""},
		{"row-old-men", "10.10.2026", "В", "Старикам тут не место", "", ""},
		{"row-good-person", "", "К", "A good person", "", ""},
		{"row-parasite", "", "В", "Паразиты", "", ""},
	}
	for _, row := range rows {
		f.sim.SharePage(token, notionsim.Page{
			ID: row.id, Title: row.date, URL: "https://notion.so/" + row.id, LastEdited: "2026-09-01T00:00:00.000Z", DataSourceID: "ds-films",
			Properties: map[string]any{
				"Фильм": notionsim.RichTextProperty(row.film),
				"К":     notionsim.RichTextProperty(row.picker),
				"В":     notionsim.RichTextProperty(row.vlad),
				"K":     notionsim.RichTextProperty(row.kim),
			},
		})
	}
}

func proposedProperties(t *testing.T, proposal approval.Proposal) (string, map[string]any) {
	t.Helper()
	var arguments struct {
		PageID     string         `json:"page_id"`
		Properties map[string]any `json:"properties"`
	}
	if proposal.Tool != "notion_update_page" || json.Unmarshal(proposal.Arguments, &arguments) != nil {
		t.Fatalf("proposal = %+v, want a notion_update_page", proposal)
	}
	return arguments.PageID, arguments.Properties
}

func proposedText(t *testing.T, properties map[string]any, name, kind string) string {
	t.Helper()
	value, _ := properties[name].(map[string]any)
	parts, _ := value[kind].([]any)
	if len(parts) != 1 {
		t.Fatalf("property %q = %+v, want one %s part", name, properties[name], kind)
	}
	part, _ := parts[0].(map[string]any)
	text, _ := part["text"].(map[string]any)
	content, _ := text["content"].(string)
	return content
}

func TestNextMovieIsTheEarliestEveningFromTodayInTheHouseholdZone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieTable(f)
	contribution := f.contribute()
	if !slices.ContainsFunc(contribution.Definitions, func(definition tools.Definition) bool { return definition.Name == "movies_next" }) ||
		slices.ContainsFunc(contribution.Definitions, func(definition tools.Definition) bool { return definition.Name == "movies_set_score" }) {
		t.Fatalf("read-only contribution = %+v, want movies_next without the movie writes", contribution.Definitions)
	}

	*f.now = time.Date(2026, time.October, 3, 23, 30, 0, 0, f.now.Location())
	tonight := f.execute("movies_next", `{"data_source_id":"ds-films"}`)
	want := "Next movie: Кит on 03.10.2026 (Saturday), picked by К."
	if tonight.IsError || !strings.Contains(tonight.Text, want) || !strings.Contains(tonight.Text, "Films without a date yet, in table order: A good person, Паразиты.") {
		t.Fatalf("next movie tonight = %+v, want %q and the undated films", tonight, want)
	}

	// Half past midnight in Amsterdam is still the 3rd in UTC; the household's day has moved on.
	*f.now = time.Date(2026, time.October, 4, 0, 30, 0, 0, f.now.Location())
	tomorrow := f.execute("movies_next", `{"data_source_id":"ds-films"}`)
	if tomorrow.IsError || !strings.Contains(tomorrow.Text, "Next movie: Старикам тут не место on 10.10.2026 (Saturday), picked by В.") {
		t.Fatalf("next movie after midnight = %+v", tomorrow)
	}

	*f.now = time.Date(2026, time.November, 1, 12, 0, 0, 0, f.now.Location())
	if ended := f.execute("movies_next", `{"data_source_id":"ds-films"}`); ended.IsError || !strings.Contains(ended.Text, "No upcoming movie night is scheduled. The latest was Старикам тут не место on 10.10.2026 (Saturday).") {
		t.Fatalf("next movie with nothing scheduled = %+v", ended)
	}
}

func TestScoreGoesToTheColumnOfTheUserWhoGaveIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieTable(f)
	f.enableWrites()

	// Kim's Latin K matches the Latin score column, never the Cyrillic picker column К.
	f.displayNames["alice"] = "Kim"
	if result := f.execute("movies_set_score", `{"data_source_id":"ds-films","film":"дюна","score":8.5}`); result.IsError || !strings.Contains(result.Text, "column K") {
		t.Fatalf("Kim's score = %+v", result)
	}
	proposal := f.proposal()
	pageID, properties := proposedProperties(t, proposal)
	if pageID != "row-dune" || len(properties) != 1 || proposedText(t, properties, "K", "rich_text") != "8.5" {
		t.Fatalf("Kim's score proposal = %s %+v", pageID, properties)
	}
	if !strings.HasSuffix(proposal.Summary, "\nK: (empty) → 8.5") {
		t.Fatalf("Kim's score summary = %q", proposal.Summary)
	}
	outcome, err := f.service.ExecuteApproved(f.ctx, approval.Record{PrincipalID: "alice", Tool: proposal.Tool, Arguments: proposal.Arguments})
	if err != nil || outcome.Text != "Updated." {
		t.Fatalf("confirmed score = %+v %v", outcome, err)
	}

	// Vlad's Latin V transliterates to the Cyrillic В column, and an existing score shows as replaced.
	f.displayNames["alice"] = "Vlad"
	if result := f.execute("movies_set_score", `{"data_source_id":"ds-films","film":"Дюна","score":9}`); result.IsError {
		t.Fatalf("Vlad's score = %+v", result)
	}
	if pageID, properties := proposedProperties(t, f.proposal()); pageID != "row-dune" || proposedText(t, properties, "В", "rich_text") != "9" {
		t.Fatalf("Vlad's score proposal = %s %+v", pageID, properties)
	}
	if summary := f.proposal().Summary; !strings.HasSuffix(summary, "\nВ: 8 → 9") {
		t.Fatalf("Vlad's score summary = %q", summary)
	}

	f.displayNames["alice"] = "Alice"
	f.refuse("movies_set_score", `{"data_source_id":"ds-films","film":"Дюна","score":9}`, "no score column matches the initial of Alice; the score columns are K | В")
	f.displayNames["alice"] = "Kim"
	f.refuse("movies_set_score", `{"data_source_id":"ds-films","film":"Дюна","score":9.3}`, "score must be between 0 and 10 in steps of 0.5")
	f.refuse("movies_set_score", `{"data_source_id":"ds-films","film":"Кит 2","score":9}`, `the movie table has no film "Кит 2"; similar titles are Кит`)
}

func TestScheduleContinuesWeeklyFromTheLatestDateInTableOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieTable(f)
	f.enableWrites()

	result := f.execute("movies_schedule", `{"data_source_id":"ds-films"}`)
	if result.IsError || !strings.Contains(result.Text, "Proposed dates: A good person → 17.10.2026 (Saturday), Паразиты → 24.10.2026 (Saturday).") {
		t.Fatalf("schedule of undated films = %+v", result)
	}
	proposals := f.proposals.proposals
	if len(proposals) != 2 {
		t.Fatalf("schedule proposals = %d, want one per undated film", len(proposals))
	}
	for i, want := range []struct{ page, date string }{{"row-good-person", "17.10.2026"}, {"row-parasite", "24.10.2026"}} {
		pageID, properties := proposedProperties(t, proposals[i])
		if pageID != want.page || proposedText(t, properties, "Name", "title") != want.date {
			t.Fatalf("schedule proposal %d = %s %+v, want %s on %s", i, pageID, properties, want.page, want.date)
		}
	}

	if result := f.execute("movies_schedule", `{"data_source_id":"ds-films","film":"Паразиты","date":"2026-10-31"}`); result.IsError {
		t.Fatalf("schedule on a given date = %+v", result)
	}
	if pageID, properties := proposedProperties(t, f.proposal()); pageID != "row-parasite" || proposedText(t, properties, "Name", "title") != "31.10.2026" {
		t.Fatalf("schedule on a given date = %s %+v", pageID, properties)
	}

	// Rescheduling the latest evening continues from the latest other date, not from itself.
	if result := f.execute("movies_schedule", `{"data_source_id":"ds-films","film":"Старикам тут не место"}`); result.IsError || !strings.Contains(result.Text, "→ 10.10.2026 (Saturday)") {
		t.Fatalf("reschedule of the latest evening = %+v", result)
	}

	f.refuse("movies_schedule", `{"data_source_id":"ds-films","date":"2026-10-31"}`, "date requires film")
	f.refuse("movies_schedule", `{"data_source_id":"ds-films","film":"Паразиты","date":"31.10.2026"}`, "date must be YYYY-MM-DD")
}

// Rows are read oldest first, so a table longer than the read leaves out the newest evenings; the
// writes that depend on them refuse instead of acting on a partial table.
func TestMovieWritesRefuseATableTooLongToRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieTable(f)
	f.enableWrites()
	f.displayNames["alice"] = "Kim"
	addFilms := func(from, to int) {
		for index := from; index < to; index++ {
			id := fmt.Sprintf("row-extra-%04d", index)
			f.sim.SharePage(token, notionsim.Page{
				ID: id, URL: "https://notion.so/" + id, LastEdited: "2026-09-01T00:00:00.000Z", DataSourceID: "ds-films",
				Properties: map[string]any{"Фильм": notionsim.RichTextProperty(fmt.Sprintf("Film %04d", index))},
			})
		}
	}

	// A diary of several hundred evenings is still read whole.
	addFilms(0, 494)
	if result := f.execute("movies_set_score", `{"data_source_id":"ds-films","film":"Film 0493","score":8}`); result.IsError {
		t.Fatalf("score in a table of 500 rows = %+v", result)
	}

	addFilms(494, 995)
	if next := f.execute("movies_next", `{"data_source_id":"ds-films"}`); next.IsError || !strings.Contains(next.Text, "later evenings may be missing") {
		t.Fatalf("next movie of a long table = %+v", next)
	}
	f.refuse("movies_schedule", `{"data_source_id":"ds-films"}`, "more rows than can be read (over 1000)")
	f.refuse("movies_schedule", `{"data_source_id":"ds-films","film":"Паразиты","date":"2026-10-31"}`, "more rows than can be read")
	f.refuse("movies_set_score", `{"data_source_id":"ds-films","film":"Дюна","score":8}`, "more rows than can be read")
}

func shareNonMovieDataSource(f *fixture, id, title string) {
	f.sim.ShareDataSource(token, notionsim.DataSource{
		ID: id, Title: title, URL: "https://notion.so/" + id,
		Properties: map[string]any{"Заметки": notionsim.SchemaProperty("rich_text")},
	})
}

func TestNextMovieFindsTheSharedMovieTableWithoutAnID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.contribute()
	shareNonMovieDataSource(f, "ds-recipes", "Рецепты")
	shareMovieTable(f)
	*f.now = time.Date(2026, time.October, 3, 12, 0, 0, 0, f.now.Location())

	explicit := f.execute("movies_next", `{"data_source_id":"ds-films"}`)
	found := f.execute("movies_next", `{}`)
	if want := explicit.Text + "\nThe movie table's data_source_id is ds-films."; explicit.IsError || found.IsError || found.Text != want {
		t.Fatalf("next movie without an ID = %+v, want the answer of the explicit ID %+v and the table's ID", found, explicit)
	}
}

func TestNextMovieWithoutAnIDRefusesWhenNoSharedDataSourceLooksLikeTheMovieTable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.contribute()
	shareNonMovieDataSource(f, "ds-recipes", "Рецепты")

	result := f.execute("movies_next", `{}`)
	if !result.IsError || !strings.Contains(result.Text, "no shared data source looks like the movie table") {
		t.Fatalf("next movie with no movie table shared = %+v, want the no-table error", result)
	}
}

func TestNextMovieWithoutAnIDListsEveryCandidateWhenSeveralLookLikeTheMovieTable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.contribute()
	shareMovieTable(f)
	f.sim.ShareDataSource(token, notionsim.DataSource{
		ID: "ds-watchlist", Title: "Хотим посмотреть", URL: "https://notion.so/ds-watchlist",
		Properties: map[string]any{"Фильм": notionsim.SchemaProperty("rich_text")},
	})

	result := f.execute("movies_next", `{}`)
	if !result.IsError || !strings.Contains(result.Text, "ds-films") || !strings.Contains(result.Text, "ds-watchlist") || !strings.Contains(result.Text, "data_source_id") {
		t.Fatalf("next movie with two candidates = %+v, want an error listing both IDs", result)
	}
}

type movieWhoRow struct{ date, who, film string }

// movieWhoRows is a table with the Кто picker column: Vlad's evening on 10.10 is reserved, and the
// other picker spells the column differently from Vlad's.
var movieWhoRows = []movieWhoRow{
	{"26.09.2026", "В", "Дюна"},
	{"03.10.2026", "Ким", "Марли и я"},
	{"10.10.2026", "В", ""},
}

func shareMovieWhoTable(f *fixture, rows ...movieWhoRow) {
	f.sim.ShareDataSource(token, notionsim.DataSource{
		ID: "ds-who", Title: "Кино вечера", URL: "https://notion.so/ds-who",
		Properties: map[string]any{
			"Фильм": notionsim.SchemaProperty("rich_text"),
			"Кто":   notionsim.SchemaProperty("rich_text"),
		},
	})
	for index, row := range rows {
		shareMovieWhoPage(f, fmt.Sprintf("who-%d", index+1), row)
	}
}

func shareMovieWhoPage(f *fixture, id string, row movieWhoRow) {
	f.sim.SharePage(token, notionsim.Page{
		ID: id, Title: row.date, URL: "https://notion.so/" + id, LastEdited: "2026-09-01T00:00:00.000Z", DataSourceID: "ds-who",
		Properties: map[string]any{
			"Фильм": notionsim.RichTextProperty(row.film),
			"Кто":   notionsim.RichTextProperty(row.who),
		},
	})
}

func movieWhoAdd(films ...string) string {
	encoded, _ := json.Marshal(films)
	return `{"data_source_id":"ds-who","films":` + string(encoded) + `}`
}

// movieCreated decodes a proposed notion_create_page into its title and property texts.
func movieCreated(t *testing.T, proposal approval.Proposal) (string, map[string]string) {
	t.Helper()
	var arguments struct {
		ParentType string                    `json:"parent_type"`
		ParentID   string                    `json:"parent_id"`
		Title      string                    `json:"title"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if proposal.Tool != "notion_create_page" || json.Unmarshal(proposal.Arguments, &arguments) != nil || arguments.ParentType != "data_source" || arguments.ParentID != "ds-who" {
		t.Fatalf("proposal = %+v, want a notion_create_page in the data source", proposal)
	}
	texts := map[string]string{}
	for name := range arguments.Properties {
		texts[name] = proposedText(t, map[string]any{name: arguments.Properties[name]}, name, "rich_text")
	}
	return arguments.Title, texts
}

func TestMoviesAddFillsTheReservedEveningThenAlternatesWithReservedRows(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieWhoTable(f, movieWhoRows...)
	f.enableWrites()

	// Latin V and Cyrillic В are the same initial.
	for _, name := range []string{"Vlad", "Влад"} {
		f.displayNames["alice"] = name
		f.proposals.proposals = nil
		result := f.execute("movies_add", movieWhoAdd("Film A", " Film B "))
		if result.IsError || !strings.Contains(result.Text, "Proposed films: ") {
			t.Fatalf("add for %s = %+v", name, result)
		}
		proposals := f.proposals.proposals
		if len(proposals) != 3 {
			t.Fatalf("add for %s recorded %d proposals, want the fill, the other picker's reserved evening, and the new film", name, len(proposals))
		}
		pageID, properties := proposedProperties(t, proposals[0])
		if pageID != "who-3" || len(properties) != 1 || proposedText(t, properties, "Фильм", "rich_text") != "Film A" {
			t.Fatalf("fill = %s %+v", pageID, properties)
		}
		title, reserved := movieCreated(t, proposals[1])
		if title != "17.10.2026" || len(reserved) != 1 || reserved["Кто"] != "Ким" {
			t.Fatalf("reserved row = %q %+v, want the other picker's own spelling and no film", title, reserved)
		}
		title, created := movieCreated(t, proposals[2])
		if title != "24.10.2026" || len(created) != 2 || created["Кто"] != "В" || created["Фильм"] != "Film B" {
			t.Fatalf("new row = %q %+v", title, created)
		}
	}

	// The other picker is named by an argument and continues straight after the latest evening.
	f.displayNames["alice"] = "Alice"
	f.proposals.proposals = nil
	if result := f.execute("movies_add", `{"data_source_id":"ds-who","films":["Film K"],"picker":"K"}`); result.IsError {
		t.Fatalf("add for the named picker = %+v", result)
	}
	if len(f.proposals.proposals) != 1 {
		t.Fatalf("add for the named picker recorded %d proposals, want one", len(f.proposals.proposals))
	}
	if title, created := movieCreated(t, f.proposal()); title != "17.10.2026" || created["Кто"] != "Ким" || created["Фильм"] != "Film K" {
		t.Fatalf("named picker's row = %q %+v", title, created)
	}
}

type movieActingProposer struct {
	proposed []approval.Proposal
	acted    []approval.Proposal
}

func (p *movieActingProposer) Propose(_ context.Context, principalID string, proposal approval.Proposal) (approval.Record, error) {
	p.proposed = append(p.proposed, proposal)
	return approval.Record{ID: fmt.Sprintf("approval-%d", len(p.proposed)), PrincipalID: principalID, Tool: proposal.Tool, Arguments: proposal.Arguments}, nil
}

func (p *movieActingProposer) ProposeAll(ctx context.Context, principalID string, proposals []approval.Proposal) ([]approval.Record, error) {
	records := make([]approval.Record, 0, len(proposals))
	for _, proposal := range proposals {
		record, _ := p.Propose(ctx, principalID, proposal)
		records = append(records, record)
	}
	return records, nil
}

// Act changes the table at once, as the approval service does in a clean chat turn.
func (p *movieActingProposer) Act(_ context.Context, _ string, proposal approval.Proposal) (string, error) {
	p.acted = append(p.acted, proposal)
	return "Updated.", nil
}

// newMovieActingService connects a second service to the fixture's Notion simulator whose
// approval service acts on updates at once.
func newMovieActingService(t *testing.T, f *fixture) (*notion.Service, *movieActingProposer) {
	t.Helper()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(f.ctx, []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
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
	proposer := &movieActingProposer{}
	now := *f.now
	service, err := notion.NewService(notion.Config{
		CredentialsDir: credentialsDir, APIBaseURL: f.sim.URL(), HTTPClient: f.sim.Client(),
		DisplayNames: map[string]string{"alice": "Vlad"}, Location: now.Location(), Now: func() time.Time { return now },
	}, store, proposer, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Contribute(f.ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if status, err := service.SetWrites(f.ctx, "alice", true); err != nil || !status.WritesEnabled {
		t.Fatalf("enable writes = %+v %v", status, err)
	}
	return service, proposer
}

func TestMovieBatchWithNewRowsProposesItsUpdatesToo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieWhoTable(f, movieWhoRows...)
	service, proposer := newMovieActingService(t, f)

	run := func(name, arguments string) {
		t.Helper()
		result, err := service.Execute(f.ctx, "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil || result.IsError {
			t.Fatalf("%s = %+v %v", name, result, err)
		}
	}

	run("movies_add", movieWhoAdd("Film A", "Film B"))
	if len(proposer.acted) != 0 || len(proposer.proposed) != 3 {
		t.Fatalf("fill and create: acted %d, proposed %d, want everything proposed and nothing acted on", len(proposer.acted), len(proposer.proposed))
	}

	// With nothing to create, the update keeps the path of every other update.
	*proposer = movieActingProposer{}
	run("movies_add", movieWhoAdd("Film C"))
	if len(proposer.acted) != 1 || len(proposer.proposed) != 0 {
		t.Fatalf("fill alone: acted %d, proposed %d, want the one update acted on", len(proposer.acted), len(proposer.proposed))
	}

	// A scheduled film that needs a reserved row before it proposes its date update as well.
	shareMovieWhoPage(f, "who-undated", movieWhoRow{"", "В", "Waiting"})
	*proposer = movieActingProposer{}
	run("movies_schedule", `{"data_source_id":"ds-who"}`)
	if len(proposer.acted) != 0 || len(proposer.proposed) != 2 {
		t.Fatalf("schedule with a reserved row: acted %d, proposed %d", len(proposer.acted), len(proposer.proposed))
	}
}

func TestMoviesAddRefusalsRecordNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieWhoTable(f, movieWhoRows...)
	f.enableWrites()
	f.displayNames["alice"] = "Vlad"

	f.refuse("movies_add", movieWhoAdd("дюна"), `"дюна" is already in the movie table (26.09.2026`)
	f.refuse("movies_add", movieWhoAdd("Film A", "film a "), "more than once")
	f.refuse("movies_add", `{"data_source_id":"ds-who","films":[]}`, "at least one film")
	f.refuse("movies_add", `{"data_source_id":"ds-who"}`, "at least one film")
	f.refuse("movies_add", movieWhoAdd("Film A", "  "), "blank")
	f.displayNames["alice"] = "Alice"
	f.refuse("movies_add", movieWhoAdd("Film A"), "matches neither picker (В | Ким)")
	f.refuse("movies_add", `{"data_source_id":"ds-who","films":["Film A"],"picker":"Zed"}`, "matches neither picker")

	// Six films need eleven changes: the fill, then a reserved row and a new row for each further film.
	f.displayNames["alice"] = "Vlad"
	f.refuse("movies_add", movieWhoAdd("F1", "F2", "F3", "F4", "F5", "F6"), "only the first 5 films fit")
	f.proposals.proposals = nil
	if result := f.execute("movies_add", movieWhoAdd("F1", "F2", "F3", "F4", "F5")); result.IsError || len(f.proposals.proposals) != 9 {
		t.Fatalf("five films = %+v with %d proposals, want nine", result, len(f.proposals.proposals))
	}
}

func TestMoviesAddRefusesTablesWithoutClearTurns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rows []movieWhoRow
		want string
	}{
		{"waiting undated films", append(slices.Clone(movieWhoRows), movieWhoRow{"", "К", "Waiting film"}), "Waiting film"},
		{"no dated rows", nil, "no evening in the movie table has a date"},
		{"latest row without a picker", []movieWhoRow{{"26.09.2026", "В", "Дюна"}, {"03.10.2026", "", "Кит"}}, "has no picker"},
		{"no second picker", []movieWhoRow{{"26.09.2026", "В", "Дюна"}, {"03.10.2026", "В", "Кит"}}, "no dated evening in the movie table has a picker other than"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			shareMovieWhoTable(f, c.rows...)
			f.enableWrites()
			f.displayNames["alice"] = "Vlad"
			f.refuse("movies_add", movieWhoAdd("Film A"), c.want)
		})
	}
}

func TestMoviesAddRefusesATableTooLongToRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieWhoTable(f, movieWhoRows...)
	f.enableWrites()
	f.displayNames["alice"] = "Vlad"
	for index := range 1000 {
		shareMovieWhoPage(f, fmt.Sprintf("row-extra-%04d", index), movieWhoRow{})
	}
	f.refuse("movies_add", movieWhoAdd("Film A"), "more rows than can be read")
}

func TestMoviesAddCatchesUpToTodayKeepingTheAlternation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieWhoTable(f, movieWhoRow{"29.08.2026", "Ким", "Кит"}, movieWhoRow{"05.09.2026", "В", "Дюна"})
	f.enableWrites()

	// The weeks after 05.09 fall on 12.09, 19.09, 26.09, and 03.10, which is today and Vlad's turn.
	f.displayNames["alice"] = "Vlad"
	if result := f.execute("movies_add", movieWhoAdd("Film A")); result.IsError || len(f.proposals.proposals) != 1 {
		t.Fatalf("add on Vlad's turn = %+v with %d proposals", result, len(f.proposals.proposals))
	}
	if title, created := movieCreated(t, f.proposal()); title != "03.10.2026" || created["Кто"] != "В" {
		t.Fatalf("Vlad's row = %q %+v", title, created)
	}

	f.proposals.proposals = nil
	f.displayNames["alice"] = "Kim"
	if result := f.execute("movies_add", movieWhoAdd("Film B")); result.IsError || len(f.proposals.proposals) != 2 {
		t.Fatalf("add on Kim's turn = %+v with %d proposals", result, len(f.proposals.proposals))
	}
	if title, created := movieCreated(t, f.proposals.proposals[0]); title != "03.10.2026" || created["Кто"] != "В" || len(created) != 1 {
		t.Fatalf("reserved row = %q %+v", title, created)
	}
	if title, created := movieCreated(t, f.proposal()); title != "10.10.2026" || created["Кто"] != "Ким" || created["Фильм"] != "Film B" {
		t.Fatalf("Kim's row = %q %+v", title, created)
	}
}

func TestScheduleGivesAFilmItsPickersTurnWithAReservedRowBetween(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieWhoTable(f, movieWhoRows...)
	shareMovieWhoPage(f, "who-undated", movieWhoRow{"", "В", "Waiting"})
	shareMovieWhoPage(f, "who-nobody", movieWhoRow{"", "", "Nobody's"})
	shareMovieWhoPage(f, "who-stray", movieWhoRow{"", "Zed", "Stray"})
	f.enableWrites()

	f.refuse("movies_schedule", `{"data_source_id":"ds-who"}`, `"Nobody's" has no picker`)
	f.refuse("movies_schedule", `{"data_source_id":"ds-who","film":"Nobody's"}`, `"Nobody's" has no picker`)

	f.refuse("movies_schedule", `{"data_source_id":"ds-who","film":"Stray"}`, `neither of the current pickers (В | Ким)`)

	// Waiting is Vlad's, but the week after the latest evening is Kim's.
	result := f.execute("movies_schedule", `{"data_source_id":"ds-who","film":"Waiting"}`)
	if result.IsError || !strings.Contains(result.Text, "17.10.2026 (Saturday) reserved for Ким") || !strings.Contains(result.Text, "Waiting → 24.10.2026 (Saturday)") {
		t.Fatalf("schedule = %+v", result)
	}
	proposals := f.proposals.proposals
	if len(proposals) != 2 {
		t.Fatalf("schedule recorded %d proposals, want the reserved row and the date", len(proposals))
	}
	if title, reserved := movieCreated(t, proposals[0]); title != "17.10.2026" || len(reserved) != 1 || reserved["Кто"] != "Ким" {
		t.Fatalf("reserved row = %q %+v", title, reserved)
	}
	if pageID, properties := proposedProperties(t, proposals[1]); pageID != "who-undated" || proposedText(t, properties, "Name", "title") != "24.10.2026" {
		t.Fatalf("date = %s %+v", pageID, properties)
	}

	// An explicit date moves nothing else and needs no turn.
	f.proposals.proposals = nil
	if result := f.execute("movies_schedule", `{"data_source_id":"ds-who","film":"Waiting","date":"2026-10-31"}`); result.IsError || len(f.proposals.proposals) != 1 {
		t.Fatalf("schedule on a given date = %+v with %d proposals", result, len(f.proposals.proposals))
	}
}

func TestScheduleMovesADatedFilmToItsPickersNextTurnAfterTheLatestOtherDate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieWhoTable(f, movieWhoRow{"26.09.2026", "В", "Дюна"}, movieWhoRow{"03.10.2026", "Ким", "Марли и я"}, movieWhoRow{"10.10.2026", "В", "Старикам тут не место"})
	f.enableWrites()

	result := f.execute("movies_schedule", `{"data_source_id":"ds-who","film":"Старикам тут не место"}`)
	if result.IsError || !strings.Contains(result.Text, "→ 10.10.2026 (Saturday)") || len(f.proposals.proposals) != 1 {
		t.Fatalf("reschedule of the latest evening = %+v with %d proposals", result, len(f.proposals.proposals))
	}
}

func TestScheduleSeveralFilmsContinueFromTheEarlierPlacement(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieWhoTable(f, movieWhoRows...)
	for index, who := range []string{"В", "В", "Ким"} {
		shareMovieWhoPage(f, fmt.Sprintf("who-undated-%d", index), movieWhoRow{"", who, fmt.Sprintf("Waiting %d", index)})
	}
	f.enableWrites()

	// 17.10 Kim's (reserved), 24.10 Vlad's, 31.10 Kim's (reserved), 07.11 Vlad's, 14.11 Kim's.
	if result := f.execute("movies_schedule", `{"data_source_id":"ds-who"}`); result.IsError {
		t.Fatalf("schedule = %+v", result)
	}
	var got []string
	for _, proposal := range f.proposals.proposals {
		if proposal.Tool == "notion_create_page" {
			title, created := movieCreated(t, proposal)
			got = append(got, "create "+title+" "+created["Кто"])
			continue
		}
		pageID, properties := proposedProperties(t, proposal)
		got = append(got, pageID+" "+proposedText(t, properties, "Name", "title"))
	}
	want := []string{"create 17.10.2026 Ким", "who-undated-0 24.10.2026", "create 31.10.2026 Ким", "who-undated-1 07.11.2026", "who-undated-2 14.11.2026"}
	if !slices.Equal(got, want) {
		t.Fatalf("schedule placement = %v, want %v", got, want)
	}
}

func TestNextMovieNamesThePickerAndWhoseTurnAnEmptyEveningIs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	shareMovieWhoTable(f, movieWhoRows...)
	f.contribute()

	if next := f.execute("movies_next", `{"data_source_id":"ds-who"}`); next.IsError || !strings.Contains(next.Text, "Next movie: Марли и я on 03.10.2026 (Saturday), picked by Ким.") {
		t.Fatalf("next movie = %+v", next)
	}
	*f.now = time.Date(2026, time.October, 4, 12, 0, 0, 0, f.now.Location())
	if next := f.execute("movies_next", `{"data_source_id":"ds-who"}`); next.IsError || !strings.Contains(next.Text, "The next movie night is 10.10.2026 (Saturday), but no film is set for it yet. It is В's turn.") {
		t.Fatalf("next evening without a film = %+v", next)
	}
}

func TestMovieGuidanceSendsFilmChangesThroughTheMovieTools(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.enableWrites()
	contribution := f.contribute()
	if !slices.ContainsFunc(contribution.Definitions, func(definition tools.Definition) bool { return definition.Name == "movies_add" }) {
		t.Fatalf("definitions = %+v, want movies_add", contribution.Definitions)
	}
	for _, want := range []string{"movies_add", "movies_schedule", "movies_set_score", "never create rows of the movie table with notion_create_page", "update those rows with notion_update_page"} {
		if !strings.Contains(contribution.WriteGuidance, want) {
			t.Fatalf("write guidance lacks %q: %s", want, contribution.WriteGuidance)
		}
	}
	if strings.Contains(contribution.WriteGuidance, "update rows of the movie table with") {
		t.Fatalf("write guidance still forbids updating movie rows: %s", contribution.WriteGuidance)
	}
}

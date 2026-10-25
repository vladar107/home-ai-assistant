package notion_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
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
	f := newFixture(t)
	f.contribute()
	shareNonMovieDataSource(f, "ds-recipes", "Рецепты")

	result := f.execute("movies_next", `{}`)
	if !result.IsError || !strings.Contains(result.Text, "no shared data source looks like the movie table") {
		t.Fatalf("next movie with no movie table shared = %+v, want the no-table error", result)
	}
}

func TestNextMovieWithoutAnIDListsEveryCandidateWhenSeveralLookLikeTheMovieTable(t *testing.T) {
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

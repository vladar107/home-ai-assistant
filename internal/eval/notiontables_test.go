package eval

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

const filmTableSeed = `{
  "writes": true,
  "data_sources": [{"id": "films", "title": "Film club", "properties": [
      {"name": "Date", "type": "date"}, {"name": "Rating", "type": "number"}, {"name": "Genre", "type": "select", "options": ["Drama", "Comedy"]}],
    "rows": [
      {"id": "row-a", "title": "First Light", "values": {"Date": "2026-03-04", "Rating": 8, "Genre": "Drama"}},
      {"id": "row-b", "title": "Second Wind", "values": {"Date": "2026-03-11"}}]}]
}`

func TestNotionTableIsQueryableAndWritesAreGranted(t *testing.T) {
	t.Parallel()
	w := newTestWorld(t, worldConfig{sections: map[string]any{SectionNotion: json.RawMessage(filmTableSeed)}})

	offered := offeredTools(t, w)
	for _, tool := range []string{"notion_query_data_source", "notion_update_page", "notion_create_page"} {
		if !slices.Contains(offered, tool) {
			t.Errorf("%s is not offered: %v", tool, offered)
		}
	}

	record := w.play(Step{Say: call("notion_update_page", `{"page_id":"row-b","properties":{"Rating":{"number":7}}}`) + "\n" +
		call("notion_query_data_source", `{"data_source_id":"films"}`)})
	query := w.toolResult(record, "notion_query_data_source")
	for _, want := range []string{"First Light", "Second Wind", "Rating", "2026-03-04", "7"} {
		if !strings.Contains(query.Result, want) {
			t.Errorf("the query lacks %q: %s", want, query.Result)
		}
	}
	if update := w.toolResult(record, "notion_update_page"); update.IsError {
		t.Fatalf("the update failed: %s", update.Result)
	}
	for _, page := range w.Notion.Pages() {
		if page.ID == "row-b" {
			rating, _ := page.Properties["Rating"].(map[string]any)
			if rating["number"] != float64(7) {
				t.Errorf("row-b rating = %v, want 7", rating)
			}
			return
		}
	}
	t.Fatal("row-b is missing")
}

func TestNotionWithoutWritesOffersOnlyReads(t *testing.T) {
	t.Parallel()
	w := newTestWorld(t, worldConfig{sections: map[string]any{SectionNotion: json.RawMessage(`{"data_sources": [{"id": "films", "title": "Film club"}]}`)}})
	if offered := offeredTools(t, w); slices.Contains(offered, "notion_update_page") || !slices.Contains(offered, "notion_query_data_source") {
		t.Errorf("offered: %v", offered)
	}
}

func TestNotionTableSeedsAreValidated(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"unknown column":  `{"data_sources": [{"id": "t", "title": "T", "rows": [{"id": "r", "title": "R", "values": {"Nope": 1}}]}]}`,
		"wrong type":      `{"data_sources": [{"id": "t", "title": "T", "properties": [{"name": "Rating", "type": "number"}], "rows": [{"id": "r", "title": "R", "values": {"Rating": "high"}}]}]}`,
		"bad date":        `{"data_sources": [{"id": "t", "title": "T", "properties": [{"name": "Date", "type": "date"}], "rows": [{"id": "r", "title": "R", "values": {"Date": "soon"}}]}]}`,
		"unknown type":    `{"data_sources": [{"id": "t", "title": "T", "properties": [{"name": "X", "type": "formula"}]}]}`,
		"title duplicate": `{"data_sources": [{"id": "t", "title": "T", "properties": [{"name": "Title", "type": "number"}]}]}`,
		"row id clash":    `{"pages": [{"id": "r", "title": "P"}], "data_sources": [{"id": "t", "title": "T", "rows": [{"id": "r", "title": "R"}]}]}`,
	} {
		if err := DefaultRegistry.worlds[SectionNotion](json.RawMessage(raw)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

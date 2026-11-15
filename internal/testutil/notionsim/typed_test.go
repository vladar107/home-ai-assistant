package notionsim

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func typedSim(t *testing.T) *Simulator {
	t.Helper()
	sim := New(t)
	sim.AddToken("tok", Token{Kind: "internal", BotName: "Eval"})
	sim.ShareDataSource("tok", DataSource{ID: "ds", Title: "Films", TitleName: "Title", Properties: map[string]any{
		"Date":   map[string]any{"id": "d", "name": "Date", "type": "date", "date": map[string]any{}},
		"Rating": map[string]any{"id": "r", "name": "Rating", "type": "number", "number": map[string]any{"format": "number"}},
	}})
	for _, row := range []struct {
		id, title, date string
		rating          any
	}{
		{"r1", "Later", "2026-03-19", nil},
		{"r2", "Earlier", "2026-03-05", 8.0},
		{"r3", "Middle", "2026-03-12", 6.0},
	} {
		properties := map[string]any{"Date": DateProperty(row.date), "Rating": map[string]any{"id": "r", "type": "number", "number": row.rating}}
		sim.SharePage("tok", Page{ID: row.id, Title: row.title, TitleName: "Title", DataSourceID: "ds", Properties: properties, LastEdited: "2026-03-01T00:00:00.000Z"})
	}
	return sim
}

func call(t *testing.T, sim *Simulator, method, path string, body any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(body)
	request, _ := http.NewRequest(method, sim.URL()+path, bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer tok")
	request.Header.Set("Notion-Version", Version)
	response, err := sim.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func titles(t *testing.T, result map[string]any) []string {
	t.Helper()
	var out []string
	for _, item := range result["results"].([]any) {
		properties := item.(map[string]any)["properties"].(map[string]any)
		title := properties["Title"].(map[string]any)["title"].([]any)[0].(map[string]any)["plain_text"].(string)
		out = append(out, title)
	}
	return out
}

func TestTypedRowsQueryFilterSortAndUpdate(t *testing.T) {
	sim := typedSim(t)

	got := titles(t, call(t, sim, "POST", "/v1/data_sources/ds/query", map[string]any{"sorts": []any{map[string]any{"property": "Date", "direction": "ascending"}}}))
	if want := []string{"Earlier", "Middle", "Later"}; !equal(got, want) {
		t.Fatalf("sorted by date: %v, want %v", got, want)
	}
	got = titles(t, call(t, sim, "POST", "/v1/data_sources/ds/query", map[string]any{"filter": map[string]any{"property": "Date", "date": map[string]any{"on_or_after": "2026-03-10"}}}))
	if len(got) != 2 {
		t.Fatalf("on or after 10 March: %v", got)
	}
	got = titles(t, call(t, sim, "POST", "/v1/data_sources/ds/query", map[string]any{"filter": map[string]any{"property": "Rating", "number": map[string]any{"greater_than_or_equal_to": 7.0}}}))
	if !equal(got, []string{"Earlier"}) {
		t.Fatalf("rated 7 or more: %v", got)
	}
	got = titles(t, call(t, sim, "POST", "/v1/data_sources/ds/query", map[string]any{"filter": map[string]any{"property": "Rating", "number": map[string]any{"is_empty": true}}}))
	if !equal(got, []string{"Later"}) {
		t.Fatalf("unrated: %v", got)
	}

	call(t, sim, "PATCH", "/v1/pages/r1", map[string]any{"properties": map[string]any{"Rating": map[string]any{"number": 7}}})
	schema := call(t, sim, "GET", "/v1/data_sources/ds", nil)["properties"].(map[string]any)
	if _, ok := schema["Title"]; !ok {
		t.Fatalf("schema lacks the Title property: %v", schema)
	}
	for _, page := range sim.Pages() {
		if page.ID == "r1" && page.Properties["Rating"].(map[string]any)["number"] != float64(7) && page.Properties["Rating"].(map[string]any)["number"] != 7 {
			t.Fatalf("rating after the update: %v", page.Properties["Rating"])
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

package eval

import (
	"path/filepath"
	"strings"
	"testing"
)

func movieCase(t *testing.T, id string) Case {
	t.Helper()
	cases, err := LoadCases(filepath.Join("..", "..", "evals", "cases"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if strings.HasPrefix(c.ID, id+"-") {
			return c
		}
	}
	t.Fatalf("no case %s", id)
	return Case{}
}

// The movie cases seed a production-shaped table, so the movies_* tools must find and use it.
func TestMovieCasesSeedATableTheMovieToolsUse(t *testing.T) {
	w := newCaseWorld(t, movieCase(t, "f1"), "")
	record := w.play(Step{Say: call("movies_next", `{}`)})
	next := w.toolResult(record, "movies_next")
	if next.IsError || !strings.Contains(next.Result, "Северный ветер") || !strings.Contains(next.Result, "12.03.2026") || !strings.Contains(next.Result, "film-club") {
		t.Fatalf("movies_next = %+v", next)
	}

	w = newCaseWorld(t, movieCase(t, "m1"), "")
	record = w.play(Step{Say: call("movies_set_score", `{"data_source_id":"film-club","film":"Тихий берег","score":7}`)})
	score := w.toolResult(record, "movies_set_score")
	if score.IsError || !strings.Contains(score.Result, "column С") {
		t.Fatalf("movies_set_score = %+v", score)
	}

	w = newCaseWorld(t, movieCase(t, "w3"), "")
	record = w.play(Step{Say: call("movies_schedule", `{"data_source_id":"film-club","film":"Тихий берег","date":"2026-03-18"}`)})
	if schedule := w.toolResult(record, "movies_schedule"); schedule.IsError {
		t.Fatalf("movies_schedule = %+v", schedule)
	}
}

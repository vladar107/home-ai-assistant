package dutchphrase

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const phrasesTable = "dutch_phrases"

// Exporter is the Dutch phrases section of a user's export: every phrase they were sent, so a
// restored account never sends one of them again.
func Exporter() export.Exporter { return exporter{} }

type exporter struct{}

func (exporter) Section() export.Section {
	return export.Section{Name: "dutch_phrases", Label: "Dutch phrases of the day", Version: 1, Tables: []export.Table{{Name: phrasesTable}}}
}

func (exporter) Keep(string, export.Row) bool { return true }

// Plan adds each phrase whose date and normalized form the principal holds no phrase for, through
// the same checks a new phrase passes.
func (exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, phrasesTable)
	if err != nil {
		return err
	}
	dates := map[string]bool{}
	held := map[string]bool{}
	for _, row := range current {
		dates[row.Text("local_date")] = true
		held[row.Text("normalized")] = true
	}
	for _, row := range tables[phrasesTable] {
		phrase := Phrase{
			LocalDate: row.Text("local_date"), Phrase: row.Text("phrase"), Normalized: Normalize(row.Text("phrase")),
			Usage: row.Text("usage"), Translation: row.Text("translation"), UsageTranslation: row.Text("usage_translation"), Note: row.Text("note"),
		}
		title := export.Quote(phrase.Phrase)
		_, createdOK := row.Time("created_at")
		if !phrase.Valid() || !createdOK || !textColumns(row) {
			plan.Skip(title, "it could not be read")
			continue
		}
		if held[phrase.Normalized] {
			plan.Present()
			continue
		}
		if dates[phrase.LocalDate] {
			plan.Skip(title, "you already have a phrase for that day")
			continue
		}
		write := export.Row{
			"local_date": phrase.LocalDate, "phrase": phrase.Phrase, "normalized": phrase.Normalized, "usage": phrase.Usage,
			"translation": phrase.Translation, "usage_translation": phrase.UsageTranslation, "note": phrase.Note, "created_at": row.Text("created_at"),
		}
		plan.Add(phrasesTable, write)
		plan.Adding(title)
		dates[phrase.LocalDate] = true
		held[phrase.Normalized] = true
	}
	return nil
}

// textColumns reports whether every text column the file sets holds a string, so a value that
// reads as "" is one the file left out or wrote empty, not a number or another type.
func textColumns(row export.Row) bool {
	for _, column := range []string{"local_date", "phrase", "usage", "translation", "usage_translation", "note", "created_at"} {
		if _, isText := row[column].(string); !isText && !row.Null(column) {
			return false
		}
	}
	return true
}

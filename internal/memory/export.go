package memory

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const (
	entriesTable    = "memory_entries"
	proceduresTable = "memory_procedures"
	settingsTable   = "memory_settings"
)

// Exporter is the memory section of a user's export: every entry and procedure. Each one shapes
// what the assistant does, so a restore preview shows its text in full.
func Exporter() export.Exporter { return memoryExporter{} }

type memoryExporter struct{}

func (memoryExporter) Section() export.Section {
	return export.Section{Name: "memory", Label: "Memory", Version: 1, Tables: []export.Table{{Name: entriesTable}, {Name: proceduresTable}}}
}

func (memoryExporter) Keep(string, export.Row) bool { return true }

// Plan adds each entry and procedure the memory does not hold, judged as a new one would be: the
// same text or title already kept counts as already there, and the memory's caps still hold.
func (memoryExporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, entriesTable)
	if err != nil {
		return err
	}
	entries := make([]Entry, 0, len(current))
	for _, row := range current {
		entries = append(entries, Entry{ID: row.Text("id"), Kind: row.Text("kind"), Text: row.Text("text")})
	}
	rows := tables[entriesTable]
	assigned, err := plan.Assign(ctx, entriesTable, ids(rows))
	if err != nil {
		return err
	}
	for _, row := range rows {
		kind, text := row.Text("kind"), row.Text("text")
		title := "Instruction"
		if kind == KindFact {
			title = "Fact"
		}
		assignment := assigned[row.Text("id")]
		if assignment.Present {
			plan.Present()
			continue
		}
		_, createdOK := row.Time("created_at")
		_, updatedOK := row.Time("updated_at")
		switch source := row.Text("source"); {
		case !validKind(kind) || !createdOK || !updatedOK || (source != SourceModel && source != SourceConfirmed && source != SourceLearned):
			plan.Skip("an entry", "it could not be read")
			continue
		case text == "" || text != normalize(text) || utf8.RuneCountInString(text) > MaximumEntryRunes:
			plan.Skip(title+" "+export.Quote(text), "it is not a single line of at most 300 characters")
			continue
		}
		rejected := check(entries, kind, text, "")
		if rejected.outcome == outcomeDuplicate {
			plan.Present()
			continue
		}
		if rejected.outcome != "" {
			plan.Skip(title+" "+export.Quote(text), "the memory would hold more than it may")
			continue
		}
		write := row.Clone()
		write["id"] = assignment.ID
		plan.Add(entriesTable, write)
		plan.AddingText(title, text)
		entries = append(entries, Entry{ID: assignment.ID, Kind: kind, Text: text})
	}
	return planProcedures(ctx, plan, tables[proceduresTable])
}

func planProcedures(ctx context.Context, plan *export.Planning, rows []export.Row) error {
	current, err := plan.Current(ctx, proceduresTable)
	if err != nil {
		return err
	}
	titles := map[string]bool{}
	for _, row := range current {
		titles[strings.ToLower(row.Text("title"))] = true
	}
	count := len(current)
	plan.Limit(export.Limit{Table: proceduresTable, Max: MaximumProcedures})
	assigned, err := plan.Assign(ctx, proceduresTable, ids(rows))
	if err != nil {
		return err
	}
	for _, row := range rows {
		title, steps := row.Text("title"), row.Text("steps")
		label := "Procedure " + export.Quote(title)
		assignment := assigned[row.Text("id")]
		if assignment.Present {
			plan.Present()
			continue
		}
		_, createdOK := row.Time("created_at")
		_, updatedOK := row.Time("updated_at")
		if source := row.Text("source"); !createdOK || !updatedOK || (source != SourceConfirmed && source != SourceLearned) {
			plan.Skip(label, "it could not be read")
			continue
		}
		if title != normalize(title) || steps != normalizeSteps(steps) {
			plan.Skip(label, "it is not in its saved form")
			continue
		}
		if problem := validProcedure(title, steps); problem != "" {
			plan.Skip(label, problem)
			continue
		}
		if titles[strings.ToLower(title)] {
			plan.Present()
			continue
		}
		if count >= MaximumProcedures {
			plan.Skip(label, "you already keep the most procedures there may be")
			continue
		}
		write := row.Clone()
		write["id"] = assignment.ID
		plan.Add(proceduresTable, write)
		plan.AddingText(label, steps)
		titles[strings.ToLower(title)] = true
		count++
	}
	return nil
}

// SettingsExporter is the settings section of a user's export: whether the assistant keeps a
// memory and learns from feedback and from finished requests. A setting already chosen here is
// left as it is.
func SettingsExporter() export.Exporter { return settingsExporter{} }

type settingsExporter struct{}

func (settingsExporter) Section() export.Section {
	return export.Section{Name: "settings", Label: "Memory settings", Version: 1, Tables: []export.Table{{Name: settingsTable}}}
}

func (settingsExporter) Keep(string, export.Row) bool { return true }

func (settingsExporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, settingsTable)
	if err != nil {
		return err
	}
	for _, row := range tables[settingsTable] {
		if len(current) > 0 {
			plan.Present()
			continue
		}
		valid := true
		for _, column := range []string{"enabled", "learn_from_feedback", "learn_procedures"} {
			if value, ok := row.Int(column); !ok || (value != 0 && value != 1) {
				valid = false
			}
		}
		if !valid || len(row) != 3 {
			plan.Skip("your memory settings", "they could not be read")
			continue
		}
		plan.Add(settingsTable, row.Clone())
		plan.Adding(settingsTitle(row))
		current = append(current, row)
	}
	return nil
}

func settingsTitle(row export.Row) string {
	state := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	return "memory " + state(row.Bool("enabled")) + ", learning from feedback " + state(row.Bool("learn_from_feedback")) + ", learning procedures " + state(row.Bool("learn_procedures"))
}

func ids(rows []export.Row) []string {
	result := make([]string, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.Text("id"))
	}
	return result
}

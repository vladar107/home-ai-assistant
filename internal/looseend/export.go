package looseend

import (
	"context"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const itemsTable = "loose_ends"

// Exporter is the loose ends section of a user's export: every tracked item, tombstones included,
// so a restore never brings back a deadline the user dropped. How far the scan has read is left
// out.
func Exporter() export.Exporter { return exporter{} }

type exporter struct{}

func (exporter) Section() export.Section {
	return export.Section{Name: "looseends", Label: "Loose ends", Version: 1, Tables: []export.Table{{Name: itemsTable}}}
}

func (exporter) Keep(string, export.Row) bool { return true }

// Plan adds each item whose ID and key are both new. An item with the same key already here is the
// file's item, kept again. A chat item given a new ID takes it as its key too.
func (exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, itemsTable)
	if err != nil {
		return err
	}
	keys := map[[4]string]bool{}
	open := 0
	for _, row := range current {
		keys[rowKey(row)] = true
		if row.Text("status") == string(StatusOpen) {
			open++
		}
	}
	plan.Limit(export.Limit{Table: itemsTable, Where: "status = 'open'", Max: MaximumOpen})
	rows := tables[itemsTable]
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, itemsTable, ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id := row.Text("id")
		title := export.Quote(row.Text("title"))
		assignment := assigned[id]
		if assignment.Present {
			plan.Present()
			continue
		}
		if reason := checkRow(row); reason != "" {
			plan.Skip(title, reason)
			continue
		}
		write := row.Clone()
		write["id"] = assignment.ID
		if Source(row.Text("source")) == SourceChat && row.Text("source_ref") == id {
			write["source_ref"] = assignment.ID
		}
		key := rowKey(write)
		if keys[key] {
			plan.Present()
			continue
		}
		isOpen := row.Text("status") == string(StatusOpen)
		if isOpen && open >= MaximumOpen {
			plan.Skip(title, "you already have the most open loose ends there may be")
			continue
		}
		plan.Add(itemsTable, write)
		plan.Adding(title)
		keys[key] = true
		if isOpen {
			open++
		}
	}
	return nil
}

func rowKey(row export.Row) [4]string {
	return [4]string{row.Text("source"), row.Text("account"), row.Text("source_ref"), row.Text("kind")}
}

// checkRow says why a stored item cannot be restored, or returns "" when every field is one the
// service could have written.
func checkRow(row export.Row) string {
	if !Kind(row.Text("kind")).Valid() || !Source(row.Text("source")).Valid() || !Status(row.Text("status")).Valid() {
		return "its kind, source, or status could not be read"
	}
	if cleaned, reason := cleanTitle(row.Text("title")); reason != "" || cleaned != row.Text("title") {
		return "its title could not be read"
	}
	if cleanCounterpart(row.Text("counterpart")) != row.Text("counterpart") {
		return "its counterpart could not be read"
	}
	for _, column := range []string{"account", "source_ref", "thread_ref"} {
		limit := maximumRefRunes
		if column == "account" {
			limit = maximumAccountRunes
		}
		if value := row.Text(column); row.Null(column) || oneLine(value) != value || len([]rune(value)) > limit {
			return "its source could not be read"
		}
	}
	if Source(row.Text("source")) != SourceChat && row.Text("source_ref") == "" {
		return "its source could not be read"
	}
	for _, column := range []string{"due_at", "created_at", "updated_at"} {
		if _, ok := row.Time(column); !ok {
			return "its dates could not be read"
		}
	}
	if !validDate(row.Text("nudge_on")) {
		return "its dates could not be read"
	}
	if nudged := row.Text("nudged_on"); row.Null("nudged_on") || nudged != "" && !validDate(nudged) {
		return "its dates could not be read"
	}
	for _, column := range []string{"due_all_day", "outside"} {
		if value, ok := row.Int(column); !ok || value != 0 && value != 1 {
			return "it could not be read"
		}
	}
	return ""
}

func validDate(value string) bool {
	_, err := time.Parse(DateLayout, value)
	return err == nil
}

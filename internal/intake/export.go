package intake

import (
	"context"
	"encoding/json"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const recordsTable = "intake_records"

// Exporter is the intake records section of a user's export: the facts kept from incoming content.
// The actions a record still proposes are left out, since they belong to the approvals of the day
// they were proposed, and so is how far the inbox check has read.
func Exporter() export.Exporter { return exporter{} }

type exporter struct{}

func (exporter) Section() export.Section {
	return export.Section{Name: "intake", Label: "Kept facts from incoming content", Version: 1, Tables: []export.Table{
		{Name: recordsTable, Omit: []string{"pending_actions"}},
	}}
}

func (exporter) Keep(string, export.Row) bool { return true }

// Plan adds each record whose ID and source are both new. A record of the same source already
// here is the file's record, kept again.
func (exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, recordsTable)
	if err != nil {
		return err
	}
	sources := map[[2]string]string{}
	for _, row := range current {
		sources[[2]string{row.Text("source"), row.Text("source_ref")}] = row.Text("id")
	}
	rows := tables[recordsTable]
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, recordsTable, ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id := row.Text("id")
		title := export.Quote(row.Text("title"))
		assignment := assigned[id]
		if assignment.Present {
			plan.Present()
			plan.Refer(export.RefIntake, id, assignment.ID, true)
			continue
		}
		var facts Facts
		_, createdOK := row.Time("created_at")
		_, updatedOK := row.Time("updated_at")
		if json.Unmarshal([]byte(row.Text("facts")), &facts) != nil || !createdOK || !updatedOK || row.Text("source_ref") == "" {
			plan.Skip(title, "it could not be read")
			continue
		}
		if _, _, reason := validate(Draft{
			Source: row.Text("source"), SourceRef: row.Text("source_ref"), Kind: row.Text("kind"), Title: row.Text("title"), Sender: row.Text("sender"),
			Dates: facts.Dates, Amounts: facts.Amounts, References: facts.References, ActionRequired: row.Text("action_required"),
		}, true); reason != "" {
			plan.Skip(title, reason)
			continue
		}
		key := [2]string{row.Text("source"), row.Text("source_ref")}
		if existing, found := sources[key]; found {
			plan.Present()
			plan.Refer(export.RefIntake, id, existing, true)
			continue
		}
		write := row.Clone()
		write["id"] = assignment.ID
		plan.Add(recordsTable, write)
		plan.Adding(title)
		plan.Refer(export.RefIntake, id, assignment.ID, false)
		sources[key] = assignment.ID
	}
	return nil
}

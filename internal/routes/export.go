package routes

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const routesTable = "routes"

// Exporter is the saved routes section of a user's export. A route watch names its route by name,
// so a route of the same name already here is taken to be the file's.
func Exporter() export.Exporter { return exporter{} }

type exporter struct{}

func (exporter) Section() export.Section {
	return export.Section{Name: "routes", Label: "Saved routes", Version: 1, Tables: []export.Table{{Name: routesTable}}}
}

func (exporter) Keep(string, export.Row) bool { return true }

func (exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, routesTable)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	currentNames := map[string]string{}
	for _, row := range current {
		names[strings.ToLower(row.Text("name"))] = true
		currentNames[row.Text("id")] = row.Text("name")
	}
	count := len(current)
	plan.Limit(export.Limit{Table: routesTable, Max: MaximumRoutesPerPrincipal})
	rows := tables[routesTable]
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, routesTable, ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		title := export.Quote(row.Text("name"))
		key := strings.ToLower(row.Text("name"))
		assignment := assigned[row.Text("id")]
		if assignment.Present {
			plan.Present()
			plan.Refer(export.RefRoute, key, currentNames[assignment.ID], true)
			continue
		}
		var spec Spec
		spec.Name, spec.ArriveBy = row.Text("name"), row.Text("arrive_by")
		_, createdOK := row.Time("created_at")
		_, updatedOK := row.Time("updated_at")
		if json.Unmarshal([]byte(row.Text("days")), &spec.Days) != nil || json.Unmarshal([]byte(row.Text("legs")), &spec.Legs) != nil || !createdOK || !updatedOK {
			plan.Skip(title, "it could not be read")
			continue
		}
		normalized, err := normalize(spec)
		if err != nil {
			plan.Skip(title, err.Error())
			continue
		}
		if normalized.Name != spec.Name {
			plan.Skip(title, "its name is not in its saved form")
			continue
		}
		if names[strings.ToLower(normalized.Name)] {
			plan.Present()
			plan.Refer(export.RefRoute, key, normalized.Name, true)
			continue
		}
		if count >= MaximumRoutesPerPrincipal {
			plan.Skip(title, "you already keep the most routes there may be")
			continue
		}
		days, err := json.Marshal(normalized.Days)
		if err != nil {
			return err
		}
		legs, err := json.Marshal(normalized.Legs)
		if err != nil {
			return err
		}
		write := row.Clone()
		write["id"], write["days"], write["legs"] = assignment.ID, string(days), string(legs)
		write["arrive_by"] = nil
		if normalized.ArriveBy != "" {
			write["arrive_by"] = normalized.ArriveBy
		}
		plan.Add(routesTable, write)
		plan.Adding(title)
		plan.Refer(export.RefRoute, key, normalized.Name, false)
		names[strings.ToLower(normalized.Name)] = true
		count++
	}
	return nil
}

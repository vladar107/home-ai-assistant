package location

import (
	"context"
	"math"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const placesTable = "places"

// PlacesExporter is the saved places section of a user's export: each place not deleted, with its
// point and radius. Presence and fixes are never in it, since they record where the user was.
func PlacesExporter() export.Exporter { return placesExporter{} }

type placesExporter struct{}

func (placesExporter) Section() export.Section {
	return export.Section{Name: "places", Label: "Saved places", Version: 1, Tables: []export.Table{{Name: placesTable}}}
}

func (placesExporter) Keep(_ string, row export.Row) bool { return row.Null("deleted_at") }

// Plan adds each place whose ID and name are both new. A place of the same name is taken to be the
// one already there, and what refers to the file's place follows it.
func (placesExporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, placesTable)
	if err != nil {
		return err
	}
	names := map[string]string{}
	deleted := map[string]bool{}
	count := 0
	for _, row := range current {
		if !row.Null("deleted_at") {
			deleted[row.Text("id")] = true
			continue
		}
		names[NameKey(row.Text("name"))] = row.Text("id")
		count++
	}
	plan.Limit(export.Limit{Table: placesTable, Where: "deleted_at IS NULL", Max: MaximumPlacesPerPrincipal})
	rows := tables[placesTable]
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, placesTable, ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id := row.Text("id")
		title := export.Quote(row.Text("name"))
		assignment := assigned[id]
		if assignment.Present {
			if deleted[assignment.ID] {
				plan.Skip(title, "it was deleted here and can still be brought back with Undo on its deletion")
				continue
			}
			plan.Present()
			plan.Refer(export.RefPlace, id, assignment.ID, true)
			continue
		}
		name, err := normalizePlaceName(row.Text("name"))
		if err != nil {
			plan.Skip(title, err.Error())
			continue
		}
		latitude, latitudeOK := row.Float("latitude")
		longitude, longitudeOK := row.Float("longitude")
		radius, radiusOK := row.Int("radius_m")
		_, createdOK := row.Time("created_at")
		_, updatedOK := row.Time("updated_at")
		switch {
		case !latitudeOK || !longitudeOK || math.IsNaN(latitude) || math.IsNaN(longitude) || math.Abs(latitude) > 90 || math.Abs(longitude) > 180:
			plan.Skip(title, "its point is out of range")
			continue
		case !radiusOK || checkRadius(int(radius)) != nil:
			plan.Skip(title, "its radius is out of range")
			continue
		case !createdOK || !updatedOK:
			plan.Skip(title, "it could not be read")
			continue
		}
		if existing, found := names[NameKey(name)]; found {
			plan.Present()
			plan.Refer(export.RefPlace, id, existing, true)
			continue
		}
		if count >= MaximumPlacesPerPrincipal {
			plan.Skip(title, "you already keep the most saved places there may be")
			continue
		}
		write := row.Clone()
		write["id"], write["name"], write["name_key"] = assignment.ID, name, NameKey(name)
		delete(write, "deleted_at")
		plan.Add(placesTable, write)
		plan.Adding(title)
		plan.Refer(export.RefPlace, id, assignment.ID, false)
		names[NameKey(name)] = assignment.ID
		count++
	}
	return nil
}

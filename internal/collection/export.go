package collection

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const (
	collectionsTable = "collections"
	recordsTable     = "collection_records"
)

// Exporter is the collections section of a user's export: each collection with its fields, field
// keys, and next key, and every record. Records are records of their own, so a record missing
// from a collection that is already here is restored into it, checked against its fields as they
// are now.
func (s *Service) Exporter() export.Exporter { return exporter{service: s} }

type exporter struct{ service *Service }

func (exporter) Section() export.Section {
	return export.Section{Name: "collections", Label: "Collections", Version: 1, Tables: []export.Table{{Name: collectionsTable}, {Name: recordsTable}}}
}

func (exporter) Keep(string, export.Row) bool { return true }

func (e exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, collectionsTable)
	if err != nil {
		return err
	}
	schemas := map[string]Collection{}
	names := map[string]string{}
	for _, row := range current {
		collection, ok := collectionOf(row)
		if !ok {
			continue
		}
		schemas[collection.ID] = collection
		names[strings.ToLower(collection.Name)] = collection.ID
	}
	count := len(current)
	plan.Limit(export.Limit{Table: collectionsTable, Max: MaximumCollectionsPerPrincipal})
	rows := tables[collectionsTable]
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, collectionsTable, ids)
	if err != nil {
		return err
	}
	titles := map[string]string{}
	for _, row := range rows {
		id := row.Text("id")
		title := export.Quote(row.Text("name"))
		titles[id] = title
		assignment := assigned[id]
		if assignment.Present {
			plan.Present()
			plan.Refer(export.RefCollection, id, assignment.ID, true)
			continue
		}
		collection, ok := collectionOf(row)
		if !ok {
			plan.Skip(title, "it could not be read")
			continue
		}
		collection, reason := checkCollection(collection)
		if reason != "" {
			plan.Skip(title, reason)
			continue
		}
		if existing, found := names[strings.ToLower(collection.Name)]; found {
			plan.Present()
			plan.Refer(export.RefCollection, id, existing, true)
			continue
		}
		if count >= MaximumCollectionsPerPrincipal {
			plan.Skip(title, "you already keep the most collections there may be")
			continue
		}
		fields, err := json.Marshal(collection.Fields)
		if err != nil {
			return err
		}
		write := row.Clone()
		write["id"], write["name"], write["description"], write["fields"] = assignment.ID, collection.Name, collection.Description, string(fields)
		plan.Add(collectionsTable, write)
		plan.Adding(title)
		plan.Refer(export.RefCollection, id, assignment.ID, false)
		collection.ID = assignment.ID
		schemas[assignment.ID] = collection
		names[strings.ToLower(collection.Name)] = assignment.ID
		count++
	}
	return e.planRecords(ctx, plan, tables[recordsTable], schemas, titles)
}

func (e exporter) planRecords(ctx context.Context, plan *export.Planning, rows []export.Row, schemas map[string]Collection, titles map[string]string) error {
	if len(rows) == 0 {
		return nil
	}
	current, err := plan.Current(ctx, recordsTable)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, row := range current {
		counts[row.Text("collection_id")]++
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, recordsTable, ids)
	if err != nil {
		return err
	}
	limited := map[string]bool{}
	for _, row := range rows {
		collectionID := row.Text("collection_id")
		title := "a record of " + titles[collectionID]
		if titles[collectionID] == "" {
			title = "a record"
		}
		assignment := assigned[row.Text("id")]
		if assignment.Present {
			plan.Present()
			continue
		}
		target, found := plan.Resolve(export.RefCollection, collectionID)
		collection, known := schemas[target]
		if !found || !known {
			plan.Skip(title, "its collection is not in the file or here")
			continue
		}
		var values map[string]json.RawMessage
		_, createdOK := row.Time("created_at")
		_, updatedOK := row.Time("updated_at")
		if json.Unmarshal([]byte(row.Text("field_values")), &values) != nil || values == nil || !createdOK || !updatedOK {
			plan.Skip(title, "it could not be read")
			continue
		}
		normalized, err := normalizedValues(collection, values, e.service.location)
		if err != nil {
			plan.Skip(title, err.Error())
			continue
		}
		if counts[target] >= MaximumRecords {
			plan.Skip(title, "the collection already holds the most records there may be")
			continue
		}
		if !limited[target] {
			plan.Limit(export.Limit{Table: recordsTable, Where: "collection_id = ?", Args: []any{target}, Max: MaximumRecords})
			limited[target] = true
		}
		write := row.Clone()
		write["id"], write["collection_id"], write["field_values"] = assignment.ID, target, normalized
		plan.Add(recordsTable, write)
		plan.Adding(title)
		counts[target]++
	}
	return nil
}

// collectionOf reads a stored collection's name, description, fields, and next key.
func collectionOf(row export.Row) (Collection, bool) {
	next, ok := row.Int("next_key")
	_, createdOK := row.Time("created_at")
	_, updatedOK := row.Time("updated_at")
	collection := Collection{ID: row.Text("id"), Name: row.Text("name"), Description: row.Text("description"), NextKey: int(next)}
	if !ok || !createdOK || !updatedOK || json.Unmarshal([]byte(row.Text("fields")), &collection.Fields) != nil {
		return Collection{}, false
	}
	return collection, true
}

// checkCollection checks a stored collection as creating it would and returns it in the form
// creating it stores, refusing one whose field keys or next key are not those it was saved with.
func checkCollection(collection Collection) (Collection, string) {
	name, err := normalizeName(collection.Name)
	if err != nil {
		return Collection{}, err.Error()
	}
	description, err := normalizeDescription(collection.Description)
	if err != nil {
		return Collection{}, err.Error()
	}
	for _, field := range collection.Fields {
		if keyNumber(field.Key) == 0 {
			return Collection{}, "a field has a malformed key"
		}
	}
	fields, next, err := normalizeFields(collection.Fields, collection.NextKey)
	if err != nil {
		return Collection{}, err.Error()
	}
	if next != collection.NextKey || len(fields) != len(collection.Fields) {
		return Collection{}, "its field keys are not those it was saved with"
	}
	collection.Name, collection.Description, collection.Fields = name, description, fields
	return collection, ""
}

// normalizedValues are a record's values in the canonical form creating it stores.
func normalizedValues(c Collection, values map[string]json.RawMessage, location *time.Location) (string, error) {
	if err := checkValues(c, values, location); err != nil {
		return "", err
	}
	normalized := make(map[string]json.RawMessage, len(values))
	for _, field := range c.Fields {
		raw, set := values[field.Key]
		if !set {
			continue
		}
		value, err := normalizeValue(field, raw, location)
		if err != nil {
			return "", err
		}
		normalized[field.Key] = value
	}
	encoded, err := json.Marshal(normalized)
	return string(encoded), err
}

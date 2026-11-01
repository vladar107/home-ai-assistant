package events

import (
	"context"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const sourcesTable = "event_sources"

// Exporter is the event sources section of a user's export: each source's name, mode, and switch.
// A source's token is never in it, not even as its hash, so a restored source has a token nobody
// holds until the user presses New token for it.
func Exporter() export.Exporter { return exporter{} }

type exporter struct{}

func (exporter) Section() export.Section {
	return export.Section{Name: "event_sources", Label: "Event sources", Version: 1, Tables: []export.Table{
		{Name: sourcesTable, Omit: []string{"token_hash", "last_used_at", "rotated_at"}},
	}}
}

func (exporter) Keep(string, export.Row) bool { return true }

func (exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, sourcesTable)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, row := range current {
		names[strings.ToLower(row.Text("name"))] = true
	}
	count := len(current)
	plan.Limit(export.Limit{Table: sourcesTable, Max: MaximumSources})
	rows := tables[sourcesTable]
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, sourcesTable, ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		title := export.Quote(row.Text("name"))
		assignment := assigned[row.Text("id")]
		if assignment.Present {
			plan.Present()
			continue
		}
		name, err := NormalizeName(row.Text("name"))
		enabled, enabledOK := row.Int("enabled")
		_, createdOK := row.Time("created_at")
		mode := row.Text("mode")
		if err != nil || name != row.Text("name") || (mode != ModeForward && mode != ModeAssess) || !enabledOK || (enabled != 0 && enabled != 1) || !createdOK {
			plan.Skip(title, "it could not be read")
			continue
		}
		if names[strings.ToLower(name)] {
			plan.Present()
			continue
		}
		if count >= MaximumSources {
			plan.Skip(title, "you already keep the most event sources there may be")
			continue
		}
		// The hash is of a token minted here and dropped at once, so no token reaches the source. A
		// preview writes nothing, so it mints none.
		hash := ""
		if plan.Applying() {
			if _, hash, err = mintToken(); err != nil {
				return err
			}
		}
		write := row.Clone()
		write["id"], write["token_hash"] = assignment.ID, hash
		plan.Add(sourcesTable, write)
		plan.Adding(title + ", with no token until you press New token for it in /settings")
		names[strings.ToLower(name)] = true
		count++
	}
	return nil
}

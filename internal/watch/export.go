package watch

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const watchesTable = "watches"

// Exporter is the watches section of a user's export: every watch of every kind, with its switch,
// mute, and travel pause. Window states and alerts are not in it, since they record what a check
// found. now is the clock a one-off watch's window is judged against, and available reports whether
// a kind can be checked here, which needs its connector. restored is told once a restore added
// watches, so each agent watch gets its tab; nil leaves that to the watch's next alert or change.
func Exporter(now func() time.Time, available func(kind string) bool, restored func(ctx context.Context, principalID string)) export.Exporter {
	if now == nil {
		now = time.Now
	}
	return exporter{now: now, available: available, restored: restored}
}

type exporter struct {
	now       func() time.Time
	available func(kind string) bool
	restored  func(ctx context.Context, principalID string)
}

// Section leaves out the summary message, which means something only in the chat it was sent to.
func (exporter) Section() export.Section {
	return export.Section{Name: "watches", Label: "Watches", Version: 1, Tables: []export.Table{
		{Name: watchesTable, Omit: []string{"summary_message_id"}},
	}}
}

func (e exporter) Restored(ctx context.Context, principalID string) {
	if e.restored != nil {
		e.restored(ctx, principalID)
	}
}

func (exporter) Keep(string, export.Row) bool { return true }

// Plan checks each watch as saving it would, after following a place watch's place, and a route
// watch's route, to what they are once restored, and stores it in the form saving it stores. An
// agent watch's instruction and condition are what a model turn is told on every check, so the
// preview shows them in full.
func (e exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, watchesTable)
	if err != nil {
		return err
	}
	count, agents := len(current), 0
	for _, row := range current {
		if row.Text("kind") == KindAgent {
			agents++
		}
	}
	plan.Limit(export.Limit{Table: watchesTable, Max: MaximumWatchesPerPrincipal})
	plan.Limit(export.Limit{Table: watchesTable, Where: "kind = 'agent'", Max: MaximumAgentWatchesPerPrincipal})
	routes, err := plan.Current(ctx, "routes")
	if err != nil {
		return err
	}
	routeNames := map[string]string{}
	for _, row := range routes {
		routeNames[strings.ToLower(row.Text("name"))] = row.Text("name")
	}
	rows := tables[watchesTable]
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, watchesTable, ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var spec Spec
		readable := json.Unmarshal([]byte(row.Text("config")), &spec) == nil && spec.Kind == row.Text("kind")
		title := export.Quote(spec.Label())
		if !readable {
			title = "a watch"
		}
		assignment := assigned[row.Text("id")]
		if assignment.Present {
			plan.Present()
			continue
		}
		_, createdOK := row.Time("created_at")
		_, updatedOK := row.Time("updated_at")
		if !readable || !createdOK || !updatedOK {
			plan.Skip(title, "it could not be read")
			continue
		}
		if e.available == nil || !e.available(spec.Kind) {
			plan.Skip(title, spec.Kind+" watches are not available here")
			continue
		}
		switch spec.Kind {
		case KindPlace:
			target, found := plan.Resolve(export.RefPlace, spec.Target.Place)
			if !found {
				plan.Skip(title, "its place is not in the file or here")
				continue
			}
			spec.Target.Place = target
		case KindRoute:
			_, restored := plan.Resolve(export.RefRoute, strings.ToLower(spec.Target.Route))
			if _, here := routeNames[strings.ToLower(spec.Target.Route)]; !restored && !here {
				plan.Skip(title, "its route is not in the file or here")
				continue
			}
		}
		normalized, err := normalize(spec, e.now())
		if err != nil {
			plan.Skip(title, err.Error())
			continue
		}
		if normalized.Kind == KindHome && normalized.Target.Entity == "" || normalized.Kind == KindPlace && normalized.Target.Place == "" {
			plan.Skip(title, "what it watches could not be read")
			continue
		}
		if count >= MaximumWatchesPerPrincipal || normalized.Kind == KindAgent && agents >= MaximumAgentWatchesPerPrincipal {
			plan.Skip(title, "you already have the most watches of its kind there may be")
			continue
		}
		config, err := json.Marshal(normalized)
		if err != nil {
			return err
		}
		write := row.Clone()
		write["id"], write["config"] = assignment.ID, string(config)
		plan.Add(watchesTable, write)
		if normalized.Kind == KindPlace {
			plan.Guard(export.RefPlace, normalized.Target.Place)
		}
		title = export.Quote(normalized.Label())
		if normalized.Kind == KindAgent && normalized.Check != nil {
			plan.AddingText(title, "Instruction: "+normalized.Check.Instruction+"\nCondition: "+normalized.Check.Condition)
		} else {
			plan.Adding(title)
		}
		count++
		if normalized.Kind == KindAgent {
			agents++
		}
	}
	return nil
}

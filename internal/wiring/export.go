package wiring

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/vladar107/home-ai-assistant/internal/events"
	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/intake"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	userlocation "github.com/vladar107/home-ai-assistant/internal/location"
	usermemory "github.com/vladar107/home-ai-assistant/internal/memory"
	"github.com/vladar107/home-ai-assistant/internal/project"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	"github.com/vladar107/home-ai-assistant/internal/trip"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// ExcludedFromExport names every table with a principal_id column that no export section holds,
// and why. A test holds each such table to being exported or listed here, so a new table cannot be
// left out of the export silently.
var ExcludedFromExport = map[string]string{
	"model_connections":           "a model subscription's credentials",
	"google_accounts":             "a Google account's credentials",
	"integration_connections":     "a Notion workspace's or an MCP server's credentials",
	"capability_grants":           "access the operator grants",
	"telegram_allowlist":          "identity the operator configures",
	"conversation_sessions":       "conversation history",
	"conversation_requests":       "conversation history",
	"conversation_attachments":    "conversation history",
	"conversation_reply_messages": "messages of one chat",
	"conversation_lane_models":    "a choice made for one chat's tab",
	"reply_feedback":              "reactions to replies in one chat",
	"reply_reconsiders":           "reactions to replies in one chat",
	"approvals":                   "confirmations of the day they were asked",
	"schedule_runs":               "a record of runs",
	"job_reports":                 "a record of runs",
	"job_report_messages":         "messages of one chat",
	"tasks":                       "a record of background work",
	"watch_states":                "what a watch's checks found",
	"watch_alerts":                "what a watch's checks found",
	"watch_alert_messages":        "messages of one chat",
	"turn_traces":                 "a record of what turns did",
	"turn_trace_calls":            "a record of what turns did",
	"turn_messages":               "messages of one chat",
	"tool_outcome_counts":         "telemetry",
	"user_locations":              "where the user was",
	"place_presence":              "where the user was",
	"ignored_live_locations":      "messages of one chat",
	"telegram_tabs":               "the tabs of one chat",
	"inbound_events":              "a record of what the user's systems posted",
	"event_token_messages":        "messages that show a token",
	"intake_scans":                "how far the inbox check has read",
	"trip_scans":                  "how far the trips check has read",
	"trip_updates_sent":           "what the trip update check has sent",
	"trip_update_states":          "what the trip update check has seen",
	"backlog_items":               "development ideas, which /backlog export gives out",
}

// buildExports registers every section of a user's export, in the order they restore: each after
// every section whose records it refers to. Places, routes, trips, collections, and intake records
// come before the projects that link them, and all of them before the jobs and watches.
func (a *Assembly) buildExports() error {
	exports, err := export.NewService(export.Config{
		KeyPath: filepath.Join(a.in.DataDir, export.KeyFileName), Location: a.location, Now: a.in.Core.Now,
		Lifetime: a.in.Core.Approval.Lifetime,
	}, a.in.Store, a.files, a.Approvals, a.in.Logger)
	if err != nil {
		return fmt.Errorf("initialize export: %w", err)
	}
	// A place watch or a job on a place is written while its places cannot be deleted, as saving one
	// in conversation is.
	if a.Locations != nil {
		locations, store := a.Locations, a.in.Store
		exports.SetGuard(export.RefPlace, func(ctx context.Context, principalID string, ids []string, write func() error) error {
			return locations.GuardPlace(ctx, principalID, ids[0], func() error {
				for _, id := range ids[1:] {
					if _, found, err := store.LoadPlace(ctx, principalID, id); err != nil {
						return err
					} else if !found {
						return userlocation.ErrPlaceGone
					}
				}
				return write()
			})
		})
	}
	available := func(kind string) bool { return a.Watches != nil && a.Watches.KindAvailable(kind) }
	var watchesRestored func(context.Context, string)
	if a.Watches != nil {
		watchesRestored = a.Watches.Restored
	}
	for _, exporter := range []export.Exporter{
		userlocation.PlacesExporter(),
		routes.Exporter(),
		trip.Exporter(),
		a.Collections.Exporter(),
		intake.Exporter(),
		project.Exporter(),
		usermemory.Exporter(),
		usermemory.SettingsExporter(),
		switchesExporter{},
		events.Exporter(),
		a.Scheduler.Exporter(),
		watch.Exporter(a.in.Core.Now, available, watchesRestored),
	} {
		if err := exports.Register(exporter); err != nil {
			return err
		}
	}
	if err := a.Approvals.RegisterExecutor(exports); err != nil {
		return err
	}
	a.Exports = exports
	return nil
}

const switchesTable = "integration_switches"

// switchesExporter is the integration switches section of a user's export: each integration the
// user turned on or off, MCP servers included. The connections themselves, and their tokens and
// keys, are never in it. A switch already chosen here is left as it is. A restored mcp-<server>
// switch grants nothing while the catalog has no server of that name, but it applies to a server
// the operator later adds under the same name; a server outside the user's principal list refuses
// its tools where they are offered and run, whatever the switch says. A proposal-widget switch, from
// an export made before it became the Widgets switch, is restored as that. It lives here because
// the integration package sits below the approvals that the export package builds on.
type switchesExporter struct{}

// renamedSwitches maps a switch's former name in an older export to its name now.
var renamedSwitches = map[string]string{"proposal-widget": integration.WidgetsSwitch.Integration}

func (switchesExporter) Section() export.Section {
	return export.Section{Name: "switches", Label: "Integration switches", Version: 1, Tables: []export.Table{{Name: switchesTable}}}
}

func (switchesExporter) Keep(string, export.Row) bool { return true }

func (switchesExporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, switchesTable)
	if err != nil {
		return err
	}
	chosen := map[string]bool{}
	for _, row := range current {
		chosen[row.Text("integration")] = true
	}
	for _, row := range tables[switchesTable] {
		name := row.Text("integration")
		if renamed, found := renamedSwitches[name]; found {
			row = row.Clone()
			row["integration"], name = renamed, renamed
		}
		if chosen[name] {
			plan.Present()
			continue
		}
		enabled, ok := row.Int("enabled")
		_, changedOK := row.Time("changed_at")
		if !integration.ValidName(name) || !ok || (enabled != 0 && enabled != 1) || !changedOK {
			plan.Skip("a switch", "it could not be read")
			continue
		}
		plan.Add(switchesTable, row.Clone())
		state := "off"
		if enabled == 1 {
			state = "on"
		}
		plan.Adding(name + " " + state)
		chosen[name] = true
	}
	return nil
}

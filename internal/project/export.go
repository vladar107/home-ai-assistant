package project

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const (
	projectsTable = "projects"
	itemsTable    = "project_items"
	linksTable    = "project_links"
)

// Exporter is the projects section of a user's export: each project with its checklist and its
// links. A project's summary message belongs to one chat's tab, so it is left out.
func Exporter() export.Exporter { return exporter{} }

type exporter struct{}

func (exporter) Section() export.Section {
	return export.Section{Name: "projects", Label: "Projects", Version: 1, Tables: []export.Table{
		{Name: projectsTable, Omit: []string{"summary_message_id"}},
		{Name: itemsTable},
		{Name: linksTable},
	}}
}

func (exporter) Keep(string, export.Row) bool { return true }

// Plan adds each new project with its items and links, which come only with their project. A link
// to a collection or an intake record that is neither restored nor already here is left out.
func (exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, projectsTable)
	if err != nil {
		return err
	}
	open := 0
	for _, row := range current {
		if Open(row.Text("status")) {
			open++
		}
	}
	plan.Limit(export.Limit{Table: projectsTable, Where: "status IN ('active', 'paused')", Max: MaximumOpenProjects})
	items := map[string][]export.Row{}
	for _, row := range tables[itemsTable] {
		items[row.Text("project_id")] = append(items[row.Text("project_id")], row)
	}
	links := map[string][]export.Row{}
	for _, row := range tables[linksTable] {
		links[row.Text("project_id")] = append(links[row.Text("project_id")], row)
	}
	projects := tables[projectsTable]
	ids := make([]string, 0, len(projects))
	for _, row := range projects {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, projectsTable, ids)
	if err != nil {
		return err
	}
	for _, row := range projects {
		id := row.Text("id")
		title := export.Quote(row.Text("title"))
		assignment := assigned[id]
		if assignment.Present {
			plan.Present()
			plan.Refer(export.RefProject, id, assignment.ID, true)
			continue
		}
		if reason := checkProject(row, items[id]); reason != "" {
			plan.Skip(title, reason)
			continue
		}
		if len(items[id]) > MaximumItems || len(links[id]) > MaximumLinks {
			plan.Skip(title, "it holds more items or links than a project may")
			continue
		}
		if Open(row.Text("status")) && open >= MaximumOpenProjects {
			plan.Skip(title, "you already have the most open projects there may be")
			continue
		}
		write := row.Clone()
		write["id"] = assignment.ID
		plan.Add(projectsTable, write)
		for _, item := range items[id] {
			itemWrite := item.Clone()
			itemWrite["project_id"] = assignment.ID
			plan.Add(itemsTable, itemWrite)
		}
		for _, link := range links[id] {
			normalized, err := normalizeLink(Link{Kind: link.Text("kind"), Reference: link.Text("reference"), Label: link.Text("label")})
			if _, positioned := link.Int("position"); err != nil || !positioned {
				plan.Skip("a link of "+title, "it could not be read")
				continue
			}
			reference := normalized.Reference
			switch normalized.Kind {
			case LinkCollection, LinkIntake:
				kind := export.RefCollection
				if normalized.Kind == LinkIntake {
					kind = export.RefIntake
				}
				target, found := plan.Resolve(kind, reference)
				if !found {
					plan.Skip("a link of "+title, "what it links to is not in the file or here")
					continue
				}
				reference = target
			}
			linkWrite := link.Clone()
			linkWrite["project_id"], linkWrite["reference"] = assignment.ID, reference
			plan.Add(linksTable, linkWrite)
		}
		plan.Adding(title)
		plan.Refer(export.RefProject, id, assignment.ID, false)
		if Open(row.Text("status")) {
			open++
		}
	}
	return nil
}

// checkProject checks a stored project and its items as starting it would.
func checkProject(row export.Row, items []export.Row) string {
	_, createdOK := row.Time("created_at")
	_, updatedOK := row.Time("updated_at")
	if !createdOK || !updatedOK {
		return "it could not be read"
	}
	if _, err := normalizeTitle(row.Text("title")); err != nil {
		return err.Error()
	}
	if _, err := normalizeGoal(row.Text("goal")); err != nil {
		return err.Error()
	}
	if _, err := normalizeStatus(row.Text("status")); err != nil {
		return err.Error()
	}
	seen := map[string]bool{}
	for _, item := range items {
		id := item.Text("id")
		_, positioned := item.Int("position")
		if id == "" || seen[id] || !positioned {
			return "an item could not be read"
		}
		seen[id] = true
		if !item.Null("done_at") {
			if _, ok := item.Time("done_at"); !ok {
				return "an item could not be read"
			}
		}
		if _, err := normalizeItem(Item{ID: id, Text: item.Text("text"), DueOn: item.Text("due_on"), Note: item.Text("note")}); err != nil {
			return err.Error()
		}
	}
	return ""
}

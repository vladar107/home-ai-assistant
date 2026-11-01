package telegram

import (
	"context"
	"strconv"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/project"
)

const (
	ProjectsTitle    = "Projects"
	NoProjectsReply  = "You have no projects. Ask me in conversation, for example: let's make a project for finding a dentist."
	ClosedProjectsOf = "Done or abandoned"

	// listedClosedProjects is how many of the latest done or abandoned projects /projects shows.
	listedClosedProjects = 10
	// projectLineUnits keeps the list within Telegram's 4096 UTF-16 units: 20 open and 10 closed
	// projects of this many units each leave room to spare.
	projectLineUnits = 120
)

// ProjectController is implemented by the project service; nil leaves /projects out.
type ProjectController interface {
	Projects(ctx context.Context, principalID string) ([]project.Project, error)
}

// renderProjects lists the principal's open projects with their status, open items, and next due
// date, and then the latest done or abandoned ones. Each project's tab carries its own summary.
func renderProjects(ctx context.Context, principalID string, controller ProjectController) (string, InlineKeyboard, error) {
	projects, err := controller.Projects(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	if len(projects) == 0 {
		return NoProjectsReply, nil, nil
	}
	lines := []string{ProjectsTitle}
	var closed []project.Project
	number := 0
	for _, found := range projects {
		if !found.Open() {
			closed = append(closed, found)
			continue
		}
		number++
		lines = append(lines, strconv.Itoa(number)+". "+projectLine(found))
	}
	if number == 0 {
		lines = append(lines, "No open projects.")
	}
	if len(closed) > 0 {
		lines = append(lines, "", ClosedProjectsOf)
		for _, found := range closed[max(0, len(closed)-listedClosedProjects):] {
			lines = append(lines, "- "+projectLine(found))
		}
	}
	return clipUnits(strings.Join(lines, "\n"), messageLimit), nil, nil
}

func projectLine(found project.Project) string {
	line := found.Title + ": " + found.Status
	if open := len(found.OpenItems()); len(found.Items) > 0 {
		line += ", " + strconv.Itoa(open) + " of " + strconv.Itoa(len(found.Items)) + " items open"
	}
	if next, due := found.NextDue(); due {
		line += ", next due " + next.DueOn
	}
	return clipUnits(line, projectLineUnits)
}

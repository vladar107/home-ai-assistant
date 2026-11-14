package telegram

import (
	"context"
	"strconv"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/project"
)

const (
	// listedClosedProjects is how many of the latest done or abandoned projects /projects shows.
	listedClosedProjects = 10
	// projectLineUnits keeps the list within Telegram's 4096 UTF-16 units: 20 open and 10 closed
	// projects of this many units each leave room to spare.
	projectLineUnits = 120
)

var (
	ProjectsTitle    = locale.T("Projects", "Проекты")
	NoProjectsReply  = locale.T("You have no projects. Ask me in conversation, for example: let's make a project for finding a dentist.", "У тебя нет проектов. Попроси меня в разговоре, например: давай заведём проект по поиску стоматолога.")
	ClosedProjectsOf = locale.T("Done or abandoned", "Завершённые или заброшенные")
	noOpenProjects   = locale.T("No open projects.", "Открытых проектов нет.")
	projectItemsOpen = locale.N(
		"%[2]d of %[1]d items open",
		"%[2]d of %[1]d items open",
		"открыто %[2]d из %[1]d пункта",
		"открыто %[2]d из %[1]d пунктов",
		"открыто %[2]d из %[1]d пунктов")
	projectNextDue = locale.T("next due %s", "ближайший срок: %s")

	projectStatuses = map[string]locale.Text{
		project.StatusActive:    locale.T("active", "активен"),
		project.StatusPaused:    locale.T("paused", "на паузе"),
		project.StatusDone:      locale.T("done", "завершён"),
		project.StatusAbandoned: locale.T("abandoned", "заброшен"),
	}
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
		return NoProjectsReply.Format(ctx), nil, nil
	}
	lines := []string{ProjectsTitle.Format(ctx)}
	var closed []project.Project
	number := 0
	for _, found := range projects {
		if !found.Open() {
			closed = append(closed, found)
			continue
		}
		number++
		lines = append(lines, strconv.Itoa(number)+". "+projectLine(ctx, found))
	}
	if number == 0 {
		lines = append(lines, noOpenProjects.Format(ctx))
	}
	if len(closed) > 0 {
		lines = append(lines, "", ClosedProjectsOf.Format(ctx))
		for _, found := range closed[max(0, len(closed)-listedClosedProjects):] {
			lines = append(lines, "- "+projectLine(ctx, found))
		}
	}
	return clipUnits(strings.Join(lines, "\n"), messageLimit), nil, nil
}

func projectLine(ctx context.Context, found project.Project) string {
	status := found.Status
	if known, ok := projectStatuses[status]; ok {
		status = known.Format(ctx)
	}
	line := found.Title + ": " + status
	if open := len(found.OpenItems()); len(found.Items) > 0 {
		line += ", " + projectItemsOpen.Format(ctx, len(found.Items), open)
	}
	if next, due := found.NextDue(); due {
		line += ", " + projectNextDue.Format(ctx, next.DueOn)
	}
	return clipUnits(line, projectLineUnits)
}

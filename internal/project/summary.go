package project

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
)

const (
	// summaryRunes keeps a summary well inside Telegram's 4096-character limit.
	summaryRunes = 3500
	// summaryItems and turnItems are how many open items the summary and the turn line show, and
	// turnItemRunes and turnGoalRunes keep each of them and the goal short in the line.
	summaryItems  = 15
	turnItems     = 10
	turnItemRunes = 80
	turnGoalRunes = 200

	summaryDateLayout = "Mon 2 Jan"

	SummaryPaused    = "Paused."
	SummaryDone      = "Done."
	SummaryAbandoned = "Abandoned."
)

// Summary is the plain text pinned in a project's tab: its title, goal, open items with their due
// dates, and the next one due. A project that is not active says so first, where the pinned bar
// shows it.
func Summary(project Project, today string) string {
	var lines []string
	switch project.Status {
	case StatusPaused:
		lines = append(lines, SummaryPaused)
	case StatusDone:
		lines = append(lines, SummaryDone)
	case StatusAbandoned:
		lines = append(lines, SummaryAbandoned)
	}
	lines = append(lines, project.Title)
	if project.Goal != "" {
		lines = append(lines, "Goal: "+project.Goal)
	}
	open := project.OpenItems()
	lines = append(lines, "")
	switch {
	case len(project.Items) == 0:
		lines = append(lines, "No items yet.")
	case len(open) == 0:
		lines = append(lines, fmt.Sprintf("All %d items done.", len(project.Items)))
	default:
		lines = append(lines, fmt.Sprintf("Open items: %d of %d", len(open), len(project.Items)))
	}
	for index, item := range open {
		if index == summaryItems {
			lines = append(lines, fmt.Sprintf("…and %d more.", len(open)-summaryItems))
			break
		}
		lines = append(lines, "- "+item.Text+dueText(item.DueOn, today))
	}
	if next, found := project.NextDue(); found {
		lines = append(lines, "", "Next due: "+dueDate(next.DueOn)+", "+next.Text)
	}
	text := strings.Join(lines, "\n")
	if runes := []rune(text); len(runes) > summaryRunes {
		text = string(runes[:summaryRunes-1]) + "…"
	}
	return text
}

func dueText(due, today string) string {
	switch {
	case due == "":
		return ""
	case due < today:
		return " (overdue, due " + dueDate(due) + ")"
	default:
		return " (due " + dueDate(due) + ")"
	}
}

func dueDate(due string) string {
	parsed, err := time.Parse(dateLayout, due)
	if err != nil {
		return due
	}
	return parsed.Format(summaryDateLayout)
}

// turnLine tells a turn in the project's conversation what the project is: its status, goal, open
// items with their IDs, and the next one due. Text the user or outside content wrote is quoted.
func turnLine(project Project) string {
	parts := []string{"status " + project.Status}
	if project.Goal != "" {
		parts = append(parts, "goal "+strconv.Quote(clip.Runes(project.Goal, turnGoalRunes)))
	}
	open := project.OpenItems()
	if len(open) == 0 {
		parts = append(parts, fmt.Sprintf("no open items of %d", len(project.Items)))
	} else {
		items := make([]string, 0, min(len(open), turnItems))
		for index, item := range open {
			if index == turnItems {
				break
			}
			entry := "[" + item.ID + "] " + strconv.Quote(clip.Runes(item.Text, turnItemRunes))
			if item.DueOn != "" {
				entry += " due " + item.DueOn
			}
			items = append(items, entry)
		}
		list := fmt.Sprintf("open items (%d of %d): %s", len(open), len(project.Items), strings.Join(items, "; "))
		if len(open) > turnItems {
			list += fmt.Sprintf("; and %d more", len(open)-turnItems)
		}
		parts = append(parts, list)
	}
	if next, found := project.NextDue(); found {
		parts = append(parts, "next due "+next.DueOn)
	}
	return prompt.AboutProject(project.Title, project.ID, strings.Join(parts, "; ")+".")
}

// Reader is the read side of the projects for the project tabs, the turn line, and the scheduler:
// it needs the repository, the zone, and the clock, not the project service with its tools.
type Reader struct {
	repository Repository
	location   *time.Location
	now        func() time.Time
}

// NewReader returns the reader over the repository; a nil clock means time.Now.
func NewReader(repository Repository, location *time.Location, now func() time.Time) Reader {
	if now == nil {
		now = time.Now
	}
	return Reader{repository: repository, location: location, now: now}
}

// Project finds one of the principal's projects of any status.
func (r Reader) Project(ctx context.Context, principalID, id string) (Project, bool, error) {
	return r.repository.LoadProject(ctx, principalID, id)
}

// SetSummaryMessage records the message that sums the project up in its tab; zero forgets it.
func (r Reader) SetSummaryMessage(ctx context.Context, principalID, id string, messageID int64) error {
	return r.repository.SetProjectSummary(ctx, principalID, id, messageID)
}

// Summary is the project's pinned summary as of today.
func (r Reader) Summary(project Project) string {
	return Summary(project, r.today())
}

func (r Reader) today() string { return r.now().In(r.location).Format(dateLayout) }

// OpenProject reports whether one of the principal's projects is open, so a job may be routed to
// it.
func (r Reader) OpenProject(ctx context.Context, principalID, id string) (bool, error) {
	found, known, err := r.repository.LoadProject(ctx, principalID, id)
	return known && found.Open(), err
}

// ProjectLine is the line a turn in the project's conversation starts from, or "" when the
// project no longer exists.
func (r Reader) ProjectLine(ctx context.Context, principalID, id string) (string, error) {
	found, known, err := r.repository.LoadProject(ctx, principalID, id)
	if err != nil || !known {
		return "", err
	}
	return turnLine(found), nil
}

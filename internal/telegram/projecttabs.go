package telegram

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/project"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	// projectTabIcon is the emoji of a project tab's icon, used when Telegram's icon set offers it.
	projectTabIcon = "📝"
	// projectTabFallbackName names the tab of a project whose title leaves nothing a tab can show.
	projectTabFallbackName = "Project"
)

// ProjectReader is what the project tabs need of the projects: one project, the record of its
// summary message, and its summary as of today.
type ProjectReader interface {
	Project(ctx context.Context, principalID, id string) (project.Project, bool, error)
	// SetSummaryMessage records the message that sums the project up in its tab.
	SetSummaryMessage(ctx context.Context, principalID, id string, messageID int64) error
	Summary(project.Project) string
}

// ProjectFeeds keeps each project's tab in step with the project; the service's Tabs implement it.
type ProjectFeeds interface {
	// SummarizeProject gives an open project its tab when it has none, and keeps its pinned
	// summary and the tab's name current.
	SummarizeProject(ctx context.Context, principalID string, chatID int64, projectID string)
}

var _ ProjectFeeds = (*Tabs)(nil)

// SummarizeProject keeps a project's tab in step after a write to the project: an open project
// without a tab gets one, its summary is edited, or sent and pinned again when it is gone, and the
// tab is renamed when the title changed. A failure is logged and left to the next write.
func (s *Tabs) SummarizeProject(ctx context.Context, principalID string, chatID int64, projectID string) {
	s.summarize(ctx, thread.ObjectProject, principalID, chatID, projectID)
}

// projectKind is the projects as their tabs see them. A project keeps its tab whatever becomes of
// it, until the user closes it; only an active or paused one is given one.
func projectKind(projects ProjectReader) objectKind {
	return objectKind{
		kind: thread.ObjectProject, icon: projectTabIcon, fallbackName: projectTabFallbackName,
		event: "telegram.project_tab", idKey: "project_id",
		load: func(ctx context.Context, principalID, id string) (tabObject, bool, error) {
			found, known, err := projects.Project(ctx, principalID, id)
			if err != nil || !known {
				return tabObject{}, known, err
			}
			return tabObject{
				ID: found.ID, Title: found.Title, SummaryMessageID: found.SummaryMessageID,
				Open: found.Open(), Summary: clipUnits(projects.Summary(found), messageLimit),
			}, true, nil
		},
		setSummary: projects.SetSummaryMessage,
	}
}

// ProjectNotifier keeps each project's tab in step with the project in the principal's active
// chat. Its messages carry no markup and no link previews.
type ProjectNotifier struct {
	Chats TripChats
	// Feeds keeps the tabs; nil leaves projects without tabs.
	Feeds ProjectFeeds
}

var _ project.Tabs = ProjectNotifier{}

// ProjectChanged keeps the project's tab and its summary in step with a write to the project. A
// failure is logged by the resolver and leaves the next write to try again.
func (n ProjectNotifier) ProjectChanged(ctx context.Context, changed project.Project) {
	if n.Feeds == nil || n.Chats == nil {
		return
	}
	chatID, found, err := n.Chats.ResolveActiveChat(ctx, changed.PrincipalID)
	if err != nil || !found {
		return
	}
	n.Feeds.SummarizeProject(ctx, changed.PrincipalID, chatID, changed.ID)
}

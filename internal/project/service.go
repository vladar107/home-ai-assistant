package project

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/collection"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// Repository stores projects with their items and links, always scoped to their principal.
type Repository interface {
	// CreateProject stores a new project with its items and links unless one with its ID exists,
	// refusing with ErrTooManyProjects when the principal already keeps limit open projects.
	CreateProject(ctx context.Context, project Project, limit int) (bool, error)
	// ListProjects returns the principal's projects, oldest first, with their items and links.
	ListProjects(ctx context.Context, principalID string) ([]Project, error)
	// CountProjects returns how many projects the principal keeps, and how many of them are open.
	CountProjects(ctx context.Context, principalID string) (total, open int, err error)
	LoadProject(ctx context.Context, principalID, projectID string) (Project, bool, error)
	// SaveProject replaces the project's title, goal, status, items, and links only while it was
	// last changed at expectedUpdatedAt. A project that opens again is refused with
	// ErrTooManyProjects when the principal already keeps limit other open projects.
	SaveProject(ctx context.Context, project Project, expectedUpdatedAt time.Time, limit int) (bool, error)
	// SetProjectSummary records the message pinned in the project's tab; zero forgets it.
	SetProjectSummary(ctx context.Context, principalID, projectID string, messageID int64) error
}

// Proposer is the core approval service as projects see it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
	Act(context.Context, string, approval.Proposal) (string, error)
}

// Tabs keeps a project's tab in step with it after a write; with tabs on, an open project without
// a tab gets one with its summary pinned. It logs its own failures.
type Tabs interface {
	ProjectChanged(ctx context.Context, project Project)
}

// Jobs lists the principal's scheduled jobs, so a project can name the ones routed to it.
type Jobs interface {
	ListScheduledJobs(ctx context.Context, principalID string) ([]schedule.Job, error)
}

// Collections lists the principal's collections, which a project can link to.
type Collections interface {
	ListCollections(ctx context.Context, principalID string) ([]collection.Collection, error)
}

type Config struct {
	// Now stamps changes and checked items; nil means time.Now.
	Now func() time.Time
	// Tabs keeps each project's tab; nil leaves projects without tabs.
	Tabs Tabs
	// Jobs names the jobs routed to a project; nil leaves them out.
	Jobs Jobs
	// Collections checks collection links; nil refuses them.
	Collections Collections
}

type Service struct {
	repository  Repository
	proposer    Proposer
	tabs        Tabs
	jobs        Jobs
	collections Collections
	now         func() time.Time
	logger      *slog.Logger
	// writes holds a lock per principal and project, so writes to one project at once, such as
	// items checked off in one batch of tool calls, apply one after another.
	writes sync.Map
}

// NewService builds the project service. A nil proposer leaves out the tools that change projects.
func NewService(cfg Config, repository Repository, proposer Proposer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("project repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("project logger is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		repository: repository, proposer: proposer, tabs: cfg.Tabs, jobs: cfg.Jobs, collections: cfg.Collections,
		now: now, logger: logger,
	}, nil
}

// Projects lists the principal's projects, oldest first.
func (s *Service) Projects(ctx context.Context, principalID string) ([]Project, error) {
	return s.repository.ListProjects(ctx, principalID)
}

// errStale reports that a project changed between being read and being saved; the write is tried
// again on the project as it is then.
var errStale = errors.New("the project changed while it was being saved")

// change applies a change to the principal's project as it is now and saves it, under the
// project's lock. A change that returns an error saves nothing; false means the project is gone.
// It returns the project as it was and as it is now.
func (s *Service) change(ctx context.Context, principalID, projectID string, apply func(*Project) error) (before, after Project, found bool, err error) {
	lock, _ := s.writes.LoadOrStore(principalID+"\x00"+projectID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	for attempt := 0; attempt < 3; attempt++ {
		current, found, err := s.repository.LoadProject(ctx, principalID, projectID)
		if err != nil || !found {
			return Project{}, Project{}, false, err
		}
		next := current.clone()
		if err := apply(&next); err != nil {
			return Project{}, Project{}, true, err
		}
		next.UpdatedAt = s.stampAfter(current.UpdatedAt)
		saved, err := s.repository.SaveProject(ctx, next, current.UpdatedAt, MaximumOpenProjects)
		if err != nil {
			return Project{}, Project{}, true, err
		}
		if saved {
			return current, next, true, nil
		}
	}
	return Project{}, Project{}, true, errStale
}

// stamp is now as the store keeps it, so a time read back compares equal to the one written.
func (s *Service) stamp() time.Time { return s.now().UTC().Truncate(time.Millisecond) }

// stampAfter is a change time later than the one it replaces, so two changes in one millisecond
// still tell apart.
func (s *Service) stampAfter(previous time.Time) time.Time {
	stamp := s.stamp()
	if !stamp.After(previous) {
		stamp = previous.Add(time.Millisecond)
	}
	return stamp
}

// changed tells the tabs about a write. Their own failures are logged by them.
func (s *Service) changed(ctx context.Context, project Project) {
	if s.tabs != nil {
		s.tabs.ProjectChanged(ctx, project)
	}
}

// pendingJobs are the principal's jobs routed to the project that have yet to run.
func (s *Service) pendingJobs(ctx context.Context, principalID, projectID string) ([]schedule.Job, error) {
	if s.jobs == nil {
		return nil, nil
	}
	jobs, err := s.jobs.ListScheduledJobs(ctx, principalID)
	if err != nil {
		return nil, err
	}
	destination := thread.ProjectDestination(projectID)
	var routed []schedule.Job
	for _, job := range jobs {
		if job.Destination == destination && job.FinishedAt.IsZero() {
			routed = append(routed, job)
		}
	}
	return routed, nil
}

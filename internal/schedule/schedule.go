// Package schedule is the durable scheduler. Jobs belong to one principal and carry a cron
// expression, a time zone, and a typed configuration; SQLite holds when each job is next due and
// every occurrence it produced, so an occurrence runs at most once across ticks, restarts, and
// retries. What an occurrence does is decided by its job type, registered in code.
package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusReady     = "ready"
	StatusDelivered = "delivered"
	StatusSkipped   = "skipped"
	StatusFailed    = "failed"

	MaximumJobsPerPrincipal = 10
)

// ErrTooManyJobs reports that the principal already holds the maximum number of jobs.
var ErrTooManyJobs = errors.New("the principal already has the maximum number of scheduled jobs")

type Job struct {
	ID          string
	PrincipalID string
	Name        string
	Type        string
	Cron        string
	Zone        string
	Config      json.RawMessage
	Enabled     bool
	NextRunAt   time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Run is one occurrence of a job. The job's name, type, and configuration are read with it, so a
// run executes what the job said when it ran rather than when it was queued.
type Run struct {
	ID           string
	JobID        string
	PrincipalID  string
	ScheduledFor time.Time
	Manual       bool
	Status       string
	ErrorKind    string
	Reply        string
	JobName      string
	JobType      string
	JobZone      string
	Config       json.RawMessage
}

type Repository interface {
	// CreateScheduledJob stores a new job unless the principal already holds limit jobs, which
	// returns ErrTooManyJobs. A job whose ID already exists is left alone and reported as not
	// created, so executing the same confirmation twice creates one job.
	CreateScheduledJob(ctx context.Context, job Job, limit int) (bool, error)
	ListScheduledJobs(ctx context.Context, principalID string) ([]Job, error)
	LoadScheduledJob(ctx context.Context, principalID, jobID string) (Job, bool, error)
	SetScheduledJobEnabled(ctx context.Context, principalID, jobID string, enabled bool, nextRunAt, now time.Time) (bool, error)
	DeleteScheduledJob(ctx context.Context, principalID, jobID string) (bool, error)
	DueScheduledJobs(ctx context.Context, now time.Time) ([]Job, error)
	// AdvanceScheduledJob moves an enabled job from the occurrence it was due at to next and, when
	// run is set, queues that run in the same transaction. A job changed since it was read is
	// left alone, and an occurrence already queued is not queued again.
	AdvanceScheduledJob(ctx context.Context, jobID string, due, next time.Time, run *Run) (bool, error)
	QueueScheduledRun(ctx context.Context, run Run) error
	// ScheduledRuns returns the runs in the given statuses together with their job's fields.
	ScheduledRuns(ctx context.Context, statuses ...string) ([]Run, error)
	// TransitionScheduledRun moves a run from one status to another. A terminal status clears the
	// stored reply; ready stores it.
	TransitionScheduledRun(ctx context.Context, runID, from, to, reply, errorKind string, now time.Time) (bool, error)
	// RecoverScheduledRuns moves every run left running by a previous process to ready with the
	// given reply, returning how many there were.
	RecoverScheduledRuns(ctx context.Context, reply, errorKind string, now time.Time) (int, error)
	PruneScheduledRuns(ctx context.Context, before time.Time) error
}

// Occurrence is what a job type's handler receives: one run of one principal's job.
type Occurrence struct {
	PrincipalID  string
	JobID        string
	JobName      string
	Config       json.RawMessage
	ScheduledFor time.Time
	Location     *time.Location
}

// Result is the text to deliver to the job's owner. ErrorKind is empty for success and a short
// word for logs otherwise; a failed run still delivers its text, so the owner learns of it.
type Result struct {
	Text      string
	ErrorKind string
}

type Handler interface {
	// Validate checks a job's configuration before it is proposed or stored.
	Validate(config json.RawMessage) error
	// Run performs one occurrence. An error means the run was interrupted, such as by shutdown,
	// and is left for startup recovery rather than delivered.
	Run(ctx context.Context, occurrence Occurrence) (Result, error)
}

// JobType is a registered kind of job and the rules every job of that kind follows.
type JobType struct {
	Name    string
	Handler Handler
	// CatchUp is how old a missed occurrence may be and still run once after downtime.
	CatchUp time.Duration
	// MinimumInterval is the least time allowed between two occurrences of one job.
	MinimumInterval time.Duration
}

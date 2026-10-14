package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// jobUndo reverses a direct job action: After is the job as a create or an update left it, Before
// the job an update replaced, and Removed the whole job a removal deleted.
type jobUndo struct {
	JobID   string         `json:"job_id"`
	After   *jobDefinition `json:"after,omitempty"`
	Before  *Job           `json:"before,omitempty"`
	Removed *Job           `json:"removed,omitempty"`
}

// jobDefinition is what the user can change about a job. Every run moves a job's UpdatedAt, so an
// Undo compares this instead, and only a change the user made refuses it.
type jobDefinition struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Cron    string          `json:"cron,omitempty"`
	RunAt   time.Time       `json:"run_at,omitzero"`
	Zone    string          `json:"zone"`
	Config  json.RawMessage `json:"config"`
	Enabled bool            `json:"enabled"`
}

func definitionOf(job Job) jobDefinition {
	return jobDefinition{Name: job.Name, Type: job.Type, Cron: job.Cron, RunAt: job.RunAt, Zone: job.Zone, Config: job.Config, Enabled: job.Enabled}
}

func (d jobDefinition) same(other jobDefinition) bool {
	var config, otherConfig bytes.Buffer
	if json.Compact(&config, d.Config) != nil || json.Compact(&otherConfig, other.Config) != nil {
		return false
	}
	return d.Name == other.Name && d.Type == other.Type && d.Cron == other.Cron && d.RunAt.Equal(other.RunAt) &&
		d.Zone == other.Zone && d.Enabled == other.Enabled && bytes.Equal(config.Bytes(), otherConfig.Bytes())
}

// undoCreate is the payload that deletes a job just created, read back as stored.
func (s *Service) undoCreate(ctx context.Context, principalID, jobID string) (json.RawMessage, error) {
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, jobID)
	if err != nil || !found {
		return nil, err
	}
	after := definitionOf(job)
	return json.Marshal(jobUndo{JobID: jobID, After: &after})
}

// undoUpdate is the payload that puts back the job an update replaced, with the job as it is now.
func (s *Service) undoUpdate(ctx context.Context, before Job) (json.RawMessage, error) {
	job, found, err := s.repository.LoadScheduledJob(ctx, before.PrincipalID, before.ID)
	if err != nil || !found {
		return nil, err
	}
	after := definitionOf(job)
	return json.Marshal(jobUndo{JobID: before.ID, After: &after, Before: &before})
}

// Undo reverses a direct job action: it deletes a created job, puts back the job an update
// replaced, or recreates a removed job under its own ID. A job the user changed since, one that is
// gone, and a one-time job whose time has passed are refused.
func (s *Service) Undo(ctx context.Context, record approval.Record, raw json.RawMessage) (approval.Outcome, error) {
	var undo jobUndo
	if err := json.Unmarshal(raw, &undo); err != nil || undo.JobID == "" {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "what to undo could not be read."}
	}
	switch {
	case record.Tool == toolPropose && undo.After != nil:
		return s.undoCreated(ctx, record.PrincipalID, undo)
	case record.Tool == toolUpdate && undo.After != nil && undo.Before != nil:
		return s.undoUpdated(ctx, record.PrincipalID, undo)
	case record.Tool == toolRemove && undo.Removed != nil:
		return s.undoRemoved(ctx, record.PrincipalID, *undo.Removed)
	default:
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "this action cannot be undone."}
	}
}

// current loads the job an Undo works on, refusing one that is gone, was changed since, or has
// already run.
func (s *Service) current(ctx context.Context, principalID string, undo jobUndo) (Job, error) {
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, undo.JobID)
	if err != nil {
		return Job{}, err
	}
	switch {
	case !found:
		return Job{}, &approval.UndoRefusal{Reason: "the job no longer exists."}
	case !job.FinishedAt.IsZero():
		return Job{}, &approval.UndoRefusal{Reason: "the job has already run."}
	case !definitionOf(job).same(*undo.After):
		return Job{}, &approval.UndoRefusal{Reason: "the job was changed since; change it in /jobs instead."}
	}
	return job, nil
}

func (s *Service) undoCreated(ctx context.Context, principalID string, undo jobUndo) (approval.Outcome, error) {
	job, err := s.current(ctx, principalID, undo)
	if err != nil {
		return approval.Outcome{}, err
	}
	deleted, err := s.repository.DeleteUnchangedScheduledJob(ctx, principalID, job.ID, job.UpdatedAt)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !deleted {
		return approval.Outcome{}, fmt.Errorf("the job %s changed while it was being deleted", job.ID)
	}
	s.logger.InfoContext(ctx, "schedule.job.deleted", "principal_id", principalID, "job_id", job.ID)
	return approval.Outcome{Text: fmt.Sprintf("Deleted the scheduled job %q.", job.Name)}, nil
}

func (s *Service) undoUpdated(ctx context.Context, principalID string, undo jobUndo) (approval.Outcome, error) {
	job, err := s.current(ctx, principalID, undo)
	if err != nil {
		return approval.Outcome{}, err
	}
	previous := *undo.Before
	prepared, err := s.restorable(previous)
	if err != nil {
		return approval.Outcome{}, err
	}
	updated, err := s.Update(ctx, job, job.UpdatedAt, prepared)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, fmt.Errorf("the job %s changed while it was being restored", job.ID)
	}
	return approval.Outcome{Text: fmt.Sprintf("Restored the scheduled job %q as it was.", previous.Name)}, nil
}

func (s *Service) undoRemoved(ctx context.Context, principalID string, removed Job) (approval.Outcome, error) {
	if removed.PrincipalID != principalID {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the job belongs to someone else."}
	}
	prepared, err := s.restorable(removed)
	if err != nil {
		return approval.Outcome{}, err
	}
	prepared.Job.Enabled, prepared.Job.CreatedAt = removed.Enabled, removed.CreatedAt
	created, err := s.Create(ctx, removed.ID, prepared)
	if errors.Is(err, ErrTooManyJobs) {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: tooManyText(removed.OneTime())}
	}
	if err != nil {
		return approval.Outcome{}, err
	}
	if !created {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the job is already back."}
	}
	return approval.Outcome{Text: fmt.Sprintf("Restored the scheduled job %q; its earlier runs are not.", removed.Name)}, nil
}

// restorable validates a job as it was, to store it again, with its next run worked out from now.
// A one-time job whose time has passed, or that has already run, cannot come back.
func (s *Service) restorable(job Job) (Prepared, error) {
	timing := Timing{Cron: job.Cron, Zone: job.Zone}
	if job.OneTime() {
		location, err := LoadZone(job.Zone)
		if err != nil {
			return Prepared{}, &approval.UndoRefusal{Reason: "its time zone is no longer known."}
		}
		if !job.FinishedAt.IsZero() || !job.RunAt.After(s.now()) {
			return Prepared{}, &approval.UndoRefusal{Reason: fmt.Sprintf("its time, %s, has passed, so it cannot be restored; schedule it again instead.", FormatOccurrence(job.RunAt, location))}
		}
		timing.At = job.RunAt.In(location).Format(localTimeLayouts[0])
	}
	prepared, err := s.Prepare(job.PrincipalID, job.Name, job.Type, timing, job.Config)
	if err != nil {
		return Prepared{}, &approval.UndoRefusal{Reason: "it can no longer be scheduled (" + err.Error() + ")."}
	}
	if job.OneTime() {
		// The stored instant may carry seconds, as a snoozed reminder's does, which the local
		// time above drops.
		prepared.Job.RunAt, prepared.Job.NextRunAt, prepared.Schedule.Once = job.RunAt, job.RunAt, job.RunAt
	}
	return prepared, nil
}

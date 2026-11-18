package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

const (
	InboxName = "Inbox intake"
	// InboxCron is every hour on the hour from 08:00 to 22:00 in the job's zone, the scan type's
	// own minimum interval.
	InboxCron = "0 8-22 * * *"
	// legacyInboxCron is the three-hourly schedule inbox jobs were created with before.
	legacyInboxCron = "0 8-22/3 * * *"
	// InboxScanType is the job type of the inbox check, whose handler lives in the intake package.
	InboxScanType = "intake.scan"
)

// inboxNotesLabel heads the Inbox intake job's notes wherever the model reads them.
const inboxNotesLabel = "Notes (the user's preferences; how Inbox intake checks mail is built in)"

// InboxJobID is the principal's inbox check job ID, so the job is found and created once however
// often the switch is pressed.
func InboxJobID(principalID string) string {
	sum := sha256.Sum256([]byte("inbox-intake\x00" + principalID))
	return hex.EncodeToString(sum[:8])
}

// MigrateInbox moves every inbox job still on the legacy schedule to InboxCron, recomputing its
// next run. A job that is not the principal's built-in inbox job is left alone.
func (s *Service) MigrateInbox(ctx context.Context) error {
	jobs, err := s.repository.ScheduledJobsWithCron(ctx, legacyInboxCron)
	if err != nil {
		return err
	}
	moved := 0
	for _, job := range jobs {
		if job.ID != InboxJobID(job.PrincipalID) {
			continue
		}
		prepared, err := s.Prepare(job.PrincipalID, job.Name, job.Type, Timing{Cron: InboxCron, Zone: job.Zone}, job.Config)
		if err != nil {
			s.logger.WarnContext(ctx, "schedule.inbox.migrate_failed", "principal_id", job.PrincipalID, "job_id", job.ID, "error", err)
			continue
		}
		prepared.Job.TravelPause = job.TravelPause
		updated, err := s.Update(ctx, job, job.UpdatedAt, prepared)
		if err != nil {
			return err
		}
		if updated {
			moved++
		}
	}
	if moved > 0 {
		s.logger.InfoContext(ctx, "schedule.inbox.migrated", "count", moved)
	}
	return nil
}

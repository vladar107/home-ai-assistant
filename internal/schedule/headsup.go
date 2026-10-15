package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

const (
	HeadsUpName = "Heads-up"
	HeadsUpCron = "30 7,18 * * *"
	// legacyHeadsUpCron is the schedule jobs were created with before the Trips scan shared 07:30.
	legacyHeadsUpCron = "0 8,19 * * *"
	// headsUpCallBudget fits the dozen lookups its prompt asks for, which a turn's usual budget
	// would cut short and leave reported as failed checks.
	headsUpCallBudget = 20
)

// HeadsUpPrompt is the built-in instruction of the Heads-up job. It is stored with the job, so a
// later change reaches only jobs created afterwards.
const HeadsUpPrompt = "Look at the user's calendar for today and tomorrow, their jobs, watches, and saved routes, " +
	"transit disruptions on the trips those imply, the weather where they will be, Home Assistant devices that need attention " +
	"(doors left open, errors, low batteries, a full bin), health and energy trends worth knowing, and pending reminders. " +
	"Read the reports already sent today with job_reports and repeat none of them. " +
	"Write only when something needs the user's attention or action: a few short lines, most important first, " +
	"in the language of the user's memory and other jobs. A source that is off or not connected is not worth mentioning. Otherwise reply with exactly " + conversation.NothingToAdd + "."

// HeadsUpJobID is the principal's Heads-up job ID, so the job is found and created once however
// often the switch is pressed.
func HeadsUpJobID(principalID string) string {
	sum := sha256.Sum256([]byte("heads-up\x00" + principalID))
	return hex.EncodeToString(sum[:8])
}

// MigrateHeadsUp moves every Heads-up job still on the legacy schedule to HeadsUpCron, recomputing
// its next run. A job whose schedule the user changed is left alone, and so is one that is not the
// principal's built-in Heads-up job.
func (s *Service) MigrateHeadsUp(ctx context.Context) error {
	jobs, err := s.repository.ScheduledJobsWithCron(ctx, legacyHeadsUpCron)
	if err != nil {
		return err
	}
	moved := 0
	for _, job := range jobs {
		if job.ID != HeadsUpJobID(job.PrincipalID) {
			continue
		}
		prepared, err := s.Prepare(job.PrincipalID, job.Name, job.Type, Timing{Cron: HeadsUpCron, Zone: job.Zone}, job.Config)
		if err != nil {
			s.logger.WarnContext(ctx, "schedule.headsup.migrate_failed", "principal_id", job.PrincipalID, "job_id", job.ID, "error", err)
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
		s.logger.InfoContext(ctx, "schedule.headsup.migrated", "count", moved)
	}
	return nil
}

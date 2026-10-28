package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/vladar107/home-ai-assistant/internal/prompt"
)

const (
	HeadsUpName = "Heads-up"
	HeadsUpCron = "30 7,18 * * *"
	// legacyHeadsUpCron is the schedule jobs were created with before the Trips scan shared 07:30.
	legacyHeadsUpCron = "0 8,19 * * *"
	// headsUpCallBudget fits a check of every trip in the window plus the alternatives for a
	// failed train, which a turn's usual budget would cut short and leave reported as failed
	// checks.
	headsUpCallBudget = 30
)

// HeadsUpNotes are the notes a new Heads-up job starts with. The job stores only the user's
// notes; every run follows headsUpMethod, so improving the method reaches existing jobs.
const HeadsUpNotes = "Write in the language of my memory and my other jobs."

// legacyHeadsUpPrompt is the whole instruction Heads-up jobs stored before the method moved into
// code. A job still holding it word for word gets HeadsUpNotes instead.
const legacyHeadsUpPrompt = "Look at the user's calendar for today and tomorrow, their jobs, watches, and saved routes, " +
	"transit disruptions on the trips those imply, the weather where they will be, Home Assistant devices that need attention " +
	"(doors left open, errors, low batteries, a full bin), health and energy trends worth knowing, and pending reminders. " +
	"Read the reports already sent today with job_reports and repeat none of them. " +
	"Write only when something needs the user's attention or action: a few short lines, most important first, " +
	"in the language of the user's memory and other jobs. A source that is off or not connected is not worth mentioning. Otherwise reply with exactly " + prompt.NothingToAdd + "."

// headsUpMethod is how every Heads-up run works, whatever the user's notes say. It names no
// household detail: what is particular to the user, such as which event means an office day,
// belongs in their notes.
const headsUpMethod = "This is the user's Heads-up: a short briefing on what in the coming hours needs their attention or action. " +
	"Work through these steps with your tools before writing anything; a source that is off or not connected is skipped and not mentioned.\n" +
	"1. Window: from now to the end of today and, when it is 15:00 or later, all of tomorrow as well. Leave out anything already over and every departure that has already left.\n" +
	"2. Timeline: read every calendar event in the window, in order, with its place. An all-day event marks where the user is or what holds that day, not a busy time. " +
	"Take an event only for what its title, its place, and the user's notes say it is.\n" +
	"3. Trips: each change of place on the timeline is a trip, including the first one from home and the last one back home after the day's final event.\n" +
	"4. Check every trip: route_check a saved route that fits it, by name, or plan it with the transit tools, arriving before the next event starts and leaving after the one before it ends. " +
	"A saved route's usual time does not hold when an event at that place ends later.\n" +
	"5. When a check fails, a train is cancelled, or a change between trains is too tight, give the real alternative now with its times and changes, from route_check's alternatives or by planning the trip again, " +
	"for trains with extra transfer time when a change is tight. Never tell the user to check for themselves.\n" +
	"6. Check disruptions on those trips and say whether each affects the user's trip, and the weather where and when the user will be outside. " +
	"When you write, send one weather_card for the window, with each trip's trains.\n" +
	"7. Check home devices that need attention, health and energy trends, and pending reminders. " +
	"Read the last three days of Heads-up reports in full (job_reports with job %q and days 3), and repeat an item that is not travel only when it changed or needs action now.\n" +
	"8. Before replying, check the draft: every trip on the timeline is covered, every event comes from the calendar, every time and status came from a tool in this run, and nothing is in the past.\n" +
	"Write a few short lines, most important first. Reply with exactly " + prompt.NothingToAdd + " only when nothing in the window needs the user's attention.\n\n" +
	"The user's notes, which take precedence over the steps above on language, format, and how to read their calendar:\n"

// headsUpNotesLabel heads the Heads-up job's text wherever the model reads it, so it is edited as
// notes and not taken for the whole instruction.
const headsUpNotesLabel = "Notes (the user's preferences; how Heads-up checks the day is built in)"

// HeadsUpTask is what a Heads-up run is asked to do: the built-in method, then the user's notes.
// Its reports are read by the job's ID, which a rename keeps.
func HeadsUpTask(jobID, notes string) string {
	return fmt.Sprintf(headsUpMethod, jobID) + notes
}

// HeadsUpJobID is the principal's Heads-up job ID, so the job is found and created once however
// often the switch is pressed.
func HeadsUpJobID(principalID string) string {
	sum := sha256.Sum256([]byte("heads-up\x00" + principalID))
	return hex.EncodeToString(sum[:8])
}

// MigrateHeadsUp moves every Heads-up job still on the legacy schedule to HeadsUpCron, recomputing
// its next run, and then gives every one still holding the legacy instruction HeadsUpNotes. A job
// whose schedule or instruction the user changed keeps it, and one that is not the principal's
// built-in Heads-up job is left alone.
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
	return s.migrateHeadsUpNotes(ctx)
}

// migrateHeadsUpNotes replaces the legacy instruction of Heads-up jobs, on whatever schedule, with
// HeadsUpNotes, keeping their schedule, model, and next run.
func (s *Service) migrateHeadsUpNotes(ctx context.Context) error {
	jobs, err := s.repository.ScheduledJobsOfType(ctx, TypeModelTurn)
	if err != nil {
		return err
	}
	replaced := 0
	for _, job := range jobs {
		if job.ID != HeadsUpJobID(job.PrincipalID) || ModelTurnPrompt(job.Config) != legacyHeadsUpPrompt {
			continue
		}
		config, err := ModelTurnConfigWithModel(HeadsUpNotes, ModelTurnModel(job.Config))
		if err != nil {
			return err
		}
		changed := job
		changed.Config, changed.UpdatedAt = config, s.now()
		updated, err := s.repository.UpdateScheduledJob(ctx, changed, job.UpdatedAt)
		if err != nil {
			return err
		}
		if updated {
			replaced++
		}
	}
	if replaced > 0 {
		s.logger.InfoContext(ctx, "schedule.headsup.notes_migrated", "count", replaced)
	}
	return nil
}

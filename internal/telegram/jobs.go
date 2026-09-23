package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

const (
	JobsTitle           = "Scheduled jobs"
	NoJobsReply         = "You have no scheduled jobs. Ask me for one in conversation, for example: every Monday at 9, tell me which of my Notion tasks are overdue."
	JobGoneReply        = "That job no longer exists."
	SchedulePrivacyLine = "A scheduled job's instructions are kept until you delete it in /jobs, and each run's report is removed from the home system once it reaches you."

	jobsPrefix      = "jobs"
	jobPauseAction  = "pause"
	jobResumeAction = "resume"
	jobRunAction    = "run"
	jobDeleteAction = "delete"
)

// ScheduleController is implemented by the scheduler; nil disables /jobs.
type ScheduleController interface {
	Jobs(context.Context, string) ([]schedule.Job, error)
	SetEnabled(context.Context, string, string, bool) (bool, error)
	Delete(context.Context, string, string) (bool, error)
	RunNow(context.Context, string, string) (bool, error)
	DefaultZone() string
}

var errUnknownJobAction = errors.New("unknown job action")

// renderJobs lists the principal's jobs with one row of buttons each. Buttons are numbered like
// the list, because Telegram shows only the button text.
func renderJobs(ctx context.Context, principalID string, schedules ScheduleController) (string, InlineKeyboard, error) {
	jobs, err := schedules.Jobs(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	if len(jobs) == 0 {
		return NoJobsReply, nil, nil
	}
	lines := []string{JobsTitle, ""}
	var keyboard InlineKeyboard
	for index, job := range jobs {
		number := strconv.Itoa(index + 1)
		location, err := schedule.LoadZone(job.Zone)
		if err != nil {
			location, _ = schedule.LoadZone(schedules.DefaultZone())
		}
		state := "next " + schedule.FormatOccurrence(job.NextRunAt, location)
		toggle := InlineButton{Text: "Pause " + number, CallbackData: jobCallback(jobPauseAction, job.ID)}
		if !job.Enabled {
			state = "paused"
			toggle = InlineButton{Text: "Resume " + number, CallbackData: jobCallback(jobResumeAction, job.ID)}
		}
		lines = append(lines, fmt.Sprintf("%s. %s\n%s (%s), %s", number, job.Name, job.Cron, job.Zone, state))
		keyboard = append(keyboard, []InlineButton{
			toggle,
			{Text: "Run " + number + " now", CallbackData: jobCallback(jobRunAction, job.ID)},
			{Text: "Delete " + number, CallbackData: jobCallback(jobDeleteAction, job.ID)},
		})
	}
	return strings.Join(lines, "\n"), keyboard, nil
}

func jobCallback(action, jobID string) string {
	return jobsPrefix + ":" + action + ":" + jobID
}

// parseJobCallback accepts only the fixed vocabulary, so a stale or forged button does nothing
// beyond refreshing the list.
func parseJobCallback(data string) (string, string, bool) {
	if len(data) > maximumCallbackDataBytes {
		return "", "", false
	}
	prefix, rest, found := strings.Cut(data, ":")
	if !found || prefix != jobsPrefix {
		return "", "", false
	}
	action, jobID, found := strings.Cut(rest, ":")
	if !found || jobID == "" {
		return "", "", false
	}
	return action, jobID, true
}

// actOnJob performs one button's action on the principal's own job and returns a message for the
// user when the refreshed list alone would not say what happened.
func actOnJob(ctx context.Context, principalID, action, jobID string, schedules ScheduleController) (string, error) {
	var changed bool
	var err error
	switch action {
	case jobPauseAction, jobResumeAction:
		changed, err = schedules.SetEnabled(ctx, principalID, jobID, action == jobResumeAction)
	case jobDeleteAction:
		changed, err = schedules.Delete(ctx, principalID, jobID)
	case jobRunAction:
		changed, err = schedules.RunNow(ctx, principalID, jobID)
		if err == nil && changed {
			return "Running it now; the report will arrive here.", nil
		}
	default:
		return "", errUnknownJobAction
	}
	if err != nil {
		return "", err
	}
	if !changed {
		return JobGoneReply, nil
	}
	return "", nil
}

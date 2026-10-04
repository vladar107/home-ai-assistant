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
	NoJobsReply         = "You have no reminders or scheduled jobs. Ask me for one in conversation, for example: remind me at 5 to call mom, or every Monday at 9, tell me which of my Notion tasks are overdue."
	JobGoneReply        = "That job no longer exists."
	SchedulePrivacyLine = "A reminder's text and a scheduled job's instructions are kept until you delete them in /jobs, or for 30 days after a one-time job has run, and each run's report is removed from the home system once it reaches you."

	jobsPrefix      = "jobs"
	jobPauseAction  = "pause"
	jobResumeAction = "resume"
	jobRunAction    = "run"
	jobDeleteAction = "delete"
	jobsPageAction  = "page"

	jobsPerPage = 8
)

// ScheduleController is implemented by the scheduler; nil disables /jobs.
type ScheduleController interface {
	Jobs(context.Context, string) ([]schedule.Job, error)
	SetEnabled(context.Context, string, string, bool) (bool, error)
	Delete(context.Context, string, string) (bool, error)
	RunNow(context.Context, string, string, string) (bool, error)
	Snooze(context.Context, string, string, schedule.Snooze) (schedule.Job, bool, error)
	DefaultZone() string
}

var errUnknownJobAction = errors.New("unknown job action")

// renderJobs lists one page of the principal's jobs with one row of buttons each. Buttons are
// numbered like the list, because Telegram shows only the button text, and a page keeps the
// message within Telegram's limits of 100 buttons and 4,096 characters. A page past the end shows
// the last one.
func renderJobs(ctx context.Context, principalID string, schedules ScheduleController, page int) (string, InlineKeyboard, error) {
	jobs, err := schedules.Jobs(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	if len(jobs) == 0 {
		return NoJobsReply, nil, nil
	}
	pages := (len(jobs) + jobsPerPage - 1) / jobsPerPage
	page = min(max(page, 1), pages)
	title := JobsTitle
	if pages > 1 {
		title += fmt.Sprintf(" (page %d of %d)", page, pages)
	}
	lines := []string{title, ""}
	var keyboard InlineKeyboard
	first := (page - 1) * jobsPerPage
	for index, job := range jobs[first:min(first+jobsPerPage, len(jobs))] {
		number := strconv.Itoa(first + index + 1)
		state := schedule.DescribeJob(job, schedules.DefaultZone())
		var row []InlineButton
		if !job.OneTime() {
			location, err := schedule.LoadZone(job.Zone)
			if err != nil {
				location, _ = schedule.LoadZone(schedules.DefaultZone())
			}
			state += ", next " + schedule.FormatOccurrence(job.NextRunAt, location)
			toggle := InlineButton{Text: "Pause " + number, CallbackData: jobCallback(jobPauseAction, job.ID, page)}
			if !job.Enabled {
				state = schedule.DescribeJob(job, schedules.DefaultZone()) + ", paused"
				toggle = InlineButton{Text: "Resume " + number, CallbackData: jobCallback(jobResumeAction, job.ID, page)}
			}
			row = append(row, toggle)
		}
		name := job.Name
		if job.Type == schedule.TypeReminder {
			name = "⏰ " + name
		}
		lines = append(lines, fmt.Sprintf("%s. %s\n%s", number, name, state))
		keyboard = append(keyboard, append(row,
			InlineButton{Text: "Run " + number + " now", CallbackData: jobCallback(jobRunAction, job.ID, page)},
			InlineButton{Text: "Delete " + number, CallbackData: jobCallback(jobDeleteAction, job.ID, page)},
		))
	}
	var navigation []InlineButton
	if page > 1 {
		navigation = append(navigation, InlineButton{Text: "‹ Previous", CallbackData: jobsPageCallback(page - 1)})
	}
	if page < pages {
		navigation = append(navigation, InlineButton{Text: "Next ›", CallbackData: jobsPageCallback(page + 1)})
	}
	if len(navigation) > 0 {
		keyboard = append(keyboard, navigation)
	}
	return strings.Join(lines, "\n"), keyboard, nil
}

func jobCallback(action, jobID string, page int) string {
	return jobsPrefix + ":" + action + ":" + jobID + ":" + strconv.Itoa(page)
}

func jobsPageCallback(page int) string {
	return jobsPrefix + ":" + jobsPageAction + ":" + strconv.Itoa(page)
}

// parseJobCallback accepts only the fixed vocabulary, so a stale or forged button does nothing
// beyond refreshing the list. Data is jobs:<action>:<job id>[:<page>], or jobs:page:<page>; a
// button from before paging carries no page and refreshes the first.
func parseJobCallback(data string) (string, string, int, bool) {
	if len(data) > maximumCallbackDataBytes {
		return "", "", 0, false
	}
	parts := strings.Split(data, ":")
	if len(parts) < 3 || len(parts) > 4 || parts[0] != jobsPrefix || parts[2] == "" {
		return "", "", 0, false
	}
	action, target, page := parts[1], parts[2], 1
	if action == jobsPageAction {
		if len(parts) != 3 {
			return "", "", 0, false
		}
		number, err := strconv.Atoi(target)
		if err != nil {
			return "", "", 0, false
		}
		return action, "", number, true
	}
	if len(parts) == 4 {
		number, err := strconv.Atoi(parts[3])
		if err != nil {
			return "", "", 0, false
		}
		page = number
	}
	return action, target, page, true
}

// actOnJob performs one button's action on the principal's own job and returns a message for the
// user when the refreshed list alone would not say what happened.
// The trigger names the press, so a press the poller retries runs the job once.
func actOnJob(ctx context.Context, principalID, action, jobID, trigger string, schedules ScheduleController) (string, error) {
	var changed bool
	var err error
	switch action {
	case jobsPageAction:
		return "", nil
	case jobPauseAction, jobResumeAction:
		changed, err = schedules.SetEnabled(ctx, principalID, jobID, action == jobResumeAction)
	case jobDeleteAction:
		changed, err = schedules.Delete(ctx, principalID, jobID)
	case jobRunAction:
		changed, err = schedules.RunNow(ctx, principalID, jobID, trigger)
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

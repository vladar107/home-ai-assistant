package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const (
	jobsPrefix         = "jobs"
	jobPauseAction     = "pause"
	jobResumeAction    = "resume"
	jobRunAction       = "run"
	jobDeleteAction    = "delete"
	jobTravelOnAction  = "tpause"
	jobTravelOffAction = "tresume"
	jobsPageAction     = "page"
	// Watches share the /jobs list under their own actions, since a watch ID is not a job ID.
	watchPauseJobAction     = "wpause"
	watchResumeJobAction    = "wresume"
	watchDeleteJobAction    = "wdelete"
	watchTravelOnJobAction  = "wtpause"
	watchTravelOffJobAction = "wtresume"
	routeDeleteJobAction    = "rdelete"

	jobsPerPage = 8
)

var (
	JobsTitle           = locale.T("Scheduled jobs", "Задания по расписанию")
	jobsPagedTitle      = locale.T("Scheduled jobs (page %d of %d)", "Задания по расписанию (страница %d из %d)")
	jobNextState        = locale.T("%s, next %s", "%s, следующий запуск: %s")
	jobPausedState      = locale.T("%s, paused", "%s, приостановлено")
	jobTravelState      = locale.T("%s, pauses while traveling", "%s, приостанавливается на время поездки")
	jobPauseButton      = locale.T("Pause %s", "Пауза %s")
	jobResumeButton     = locale.T("Resume %s", "Возобновить %s")
	jobTravelOnButton   = locale.T("Pause on trip %s", "Пауза в поездке %s")
	jobTravelOffButton  = locale.T("Run on trip %s", "Выполнять в поездке %s")
	jobRunButton        = locale.T("Run %s now", "Запустить %s сейчас")
	deleteNumberButton  = locale.T("Delete %s", "Удалить %s")
	listPreviousButton  = locale.T("‹ Previous", "‹ Назад")
	listNextButton      = locale.T("Next ›", "Далее ›")
	jobRunningReply     = locale.T("Running it now; the report will arrive here.", "Запускаю сейчас; отчёт придёт сюда.")
	NoJobsReply         = locale.T("You have no reminders or scheduled jobs. Ask me for one in conversation, for example: remind me at 5 to call mom, or every Monday at 9, tell me which of my Notion tasks are overdue.", "У тебя нет напоминаний и заданий по расписанию. Попроси меня об этом в разговоре, например: напомни в 5 позвонить маме или каждый понедельник в 9 говори, какие из моих задач в Notion просрочены.")
	JobGoneReply        = locale.T("That job no longer exists.", "Этого задания больше нет.")
	SchedulePrivacyLine = locale.T("Reminders and jobs are kept until you delete them in /jobs; one-time jobs, 30 days after they run.", "Напоминания и задания хранятся, пока ты не удалишь их в /jobs; разовые задания хранятся 30 дней после выполнения.")
)

// ScheduleController is implemented by the scheduler; nil disables /jobs.
type ScheduleController interface {
	Jobs(context.Context, string) ([]schedule.Job, error)
	SetEnabled(context.Context, string, string, bool) (bool, error)
	Delete(context.Context, string, string) (bool, error)
	SetTravelPause(context.Context, string, string, bool) (bool, error)
	RunNow(context.Context, string, string, string) (bool, error)
	Busy(context.Context, string, string) (bool, error)
	Snooze(context.Context, string, string, schedule.Snooze) (schedule.Job, bool, error)
	DefaultZone() string
	Prepare(principalID, name, jobType string, timing schedule.Timing, config json.RawMessage) (schedule.Prepared, error)
	Create(ctx context.Context, id string, prepared schedule.Prepared) (bool, error)
}

var errUnknownJobAction = errors.New("unknown job action")

// renderJobs lists one page of the principal's jobs, then their watches and saved routes, with one
// row of buttons each. Buttons are numbered like the list, because Telegram shows only the button
// text, and a page keeps the message within Telegram's limits of 100 buttons and 4,096 characters.
// A page past the end shows the last one.
func renderJobs(ctx context.Context, principalID string, schedules ScheduleController, watches WatchController, routeList RouteController, page int) (string, InlineKeyboard, error) {
	jobs, err := schedules.Jobs(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	var watchList []watch.Watch
	if watches != nil {
		if watchList, err = watches.Watches(ctx, principalID); err != nil {
			return "", nil, err
		}
	}
	var saved []routes.Route
	if routeList != nil {
		if saved, err = routeList.Routes(ctx, principalID); err != nil {
			return "", nil, err
		}
	}
	total := len(jobs) + len(watchList) + len(saved)
	if total == 0 {
		return NoJobsReply.Format(ctx), nil, nil
	}
	pages := (total + jobsPerPage - 1) / jobsPerPage
	page = min(max(page, 1), pages)
	title := JobsTitle.Format(ctx)
	if pages > 1 {
		title = jobsPagedTitle.Format(ctx, page, pages)
	}
	lines := []string{title, ""}
	var keyboard InlineKeyboard
	first := (page - 1) * jobsPerPage
	for index := first; index < min(first+jobsPerPage, total); index++ {
		number := strconv.Itoa(index + 1)
		if index >= len(jobs)+len(watchList) {
			route := saved[index-len(jobs)-len(watchList)]
			lines = append(lines, fmt.Sprintf("%s. 🧭 %s\n%s", number, route.HeadingFor(ctx), route.OutlineFor(ctx)))
			keyboard = append(keyboard, []InlineButton{{Text: deleteNumberButton.Format(ctx, number), CallbackData: jobCallback(routeDeleteJobAction, route.ID, page)}})
			continue
		}
		if index >= len(jobs) {
			line, row := renderWatchEntry(ctx, watchList[index-len(jobs)], watches, number, page)
			lines = append(lines, line)
			keyboard = append(keyboard, row)
			continue
		}
		job := jobs[index]
		state := schedule.DescribeJobIn(locale.From(ctx), job, schedules.DefaultZone())
		var row []InlineButton
		if !job.OneTime() {
			described := state
			state = jobNextState.Format(ctx, described, schedule.NextRunTextIn(locale.From(ctx), job, schedules.DefaultZone()))
			toggle := InlineButton{Text: jobPauseButton.Format(ctx, number), CallbackData: jobCallback(jobPauseAction, job.ID, page)}
			if !job.Enabled {
				state = jobPausedState.Format(ctx, described)
				toggle = InlineButton{Text: jobResumeButton.Format(ctx, number), CallbackData: jobCallback(jobResumeAction, job.ID, page)}
			}
			row = append(row, toggle)
			if schedule.TravelPausable(job) {
				travel := InlineButton{Text: jobTravelOnButton.Format(ctx, number), CallbackData: jobCallback(jobTravelOnAction, job.ID, page)}
				if job.TravelPause {
					state = jobTravelState.Format(ctx, state)
					travel = InlineButton{Text: jobTravelOffButton.Format(ctx, number), CallbackData: jobCallback(jobTravelOffAction, job.ID, page)}
				}
				row = append(row, travel)
			}
		}
		name := job.Name
		if job.Type == schedule.TypeReminder {
			name = "⏰ " + name
		}
		lines = append(lines, fmt.Sprintf("%s. %s\n%s", number, name, state))
		keyboard = append(keyboard, append(row,
			InlineButton{Text: jobRunButton.Format(ctx, number), CallbackData: jobCallback(jobRunAction, job.ID, page)},
			InlineButton{Text: deleteNumberButton.Format(ctx, number), CallbackData: jobCallback(jobDeleteAction, job.ID, page)},
		))
	}
	var navigation []InlineButton
	if page > 1 {
		navigation = append(navigation, InlineButton{Text: listPreviousButton.Format(ctx), CallbackData: jobsPageCallback(page - 1)})
	}
	if page < pages {
		navigation = append(navigation, InlineButton{Text: listNextButton.Format(ctx), CallbackData: jobsPageCallback(page + 1)})
	}
	if len(navigation) > 0 {
		keyboard = append(keyboard, navigation)
	}
	return strings.Join(lines, "\n"), keyboard, nil
}

// renderWatchEntry is one watch in /jobs, with pause or resume and delete.
func renderWatchEntry(ctx context.Context, w watch.Watch, watches WatchController, number string, page int) (string, []InlineButton) {
	toggle := InlineButton{Text: jobPauseButton.Format(ctx, number), CallbackData: jobCallback(watchPauseJobAction, w.ID, page)}
	if !w.Enabled {
		toggle = InlineButton{Text: jobResumeButton.Format(ctx, number), CallbackData: jobCallback(watchResumeJobAction, w.ID, page)}
	}
	travel := InlineButton{Text: jobTravelOnButton.Format(ctx, number), CallbackData: jobCallback(watchTravelOnJobAction, w.ID, page)}
	if w.TravelPause {
		travel = InlineButton{Text: jobTravelOffButton.Format(ctx, number), CallbackData: jobCallback(watchTravelOffJobAction, w.ID, page)}
	}
	return fmt.Sprintf("%s. 👁 %s\n%s", number, w.LabelIn(locale.From(ctx)), watches.DescribeState(ctx, w)), []InlineButton{
		toggle,
		travel,
		{Text: deleteNumberButton.Format(ctx, number), CallbackData: jobCallback(watchDeleteJobAction, w.ID, page)},
	}
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
func actOnJob(ctx context.Context, principalID, action, jobID, trigger string, schedules ScheduleController, watches WatchController, routeList RouteController) (string, error) {
	var changed bool
	var err error
	switch action {
	case jobsPageAction:
		return "", nil
	case jobPauseAction, jobResumeAction:
		changed, err = schedules.SetEnabled(ctx, principalID, jobID, action == jobResumeAction)
	case jobDeleteAction:
		changed, err = schedules.Delete(ctx, principalID, jobID)
	case jobTravelOnAction, jobTravelOffAction:
		changed, err = schedules.SetTravelPause(ctx, principalID, jobID, action == jobTravelOnAction)
	case watchPauseJobAction, watchResumeJobAction, watchDeleteJobAction, watchTravelOnJobAction, watchTravelOffJobAction:
		if watches == nil {
			return "", errUnknownJobAction
		}
		switch action {
		case watchDeleteJobAction:
			changed, err = watches.Delete(ctx, principalID, jobID)
		case watchTravelOnJobAction, watchTravelOffJobAction:
			changed, err = watches.SetTravelPause(ctx, principalID, jobID, action == watchTravelOnJobAction)
		default:
			changed, err = watches.SetEnabled(ctx, principalID, jobID, action == watchResumeJobAction)
		}
		if err == nil && !changed {
			return WatchGoneReply.Format(ctx), nil
		}
	case routeDeleteJobAction:
		if routeList == nil {
			return "", errUnknownJobAction
		}
		changed, err = routeList.Delete(ctx, principalID, jobID)
		if err == nil && !changed {
			return RouteGoneReply.Format(ctx), nil
		}
	case jobRunAction:
		changed, err = schedules.RunNow(ctx, principalID, jobID, trigger)
		if err == nil && changed {
			return jobRunningReply.Format(ctx), nil
		}
	default:
		return "", errUnknownJobAction
	}
	if err != nil {
		return "", err
	}
	if !changed {
		return JobGoneReply.Format(ctx), nil
	}
	return "", nil
}

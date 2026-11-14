package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/modelcatalog"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolList           = "schedule_list"
	toolGet            = "schedule_get"
	toolPropose        = "schedule_propose"
	toolUpdate         = "schedule_update"
	toolRemove         = "schedule_remove"
	toolReminderSet    = "reminder_set"
	toolReminderCancel = "reminder_cancel"

	upcomingShown = 3
	modelExample  = "gpt-6.1-sol:high"
	// listedInstructionRunes keeps a long list of jobs small in the model's context.
	listedInstructionRunes = 200
	// editQuoteRunes is how much of a failing edit's find text its error repeats.
	editQuoteRunes = 80
	// actTimeout lets a change made at once run as long as one made after Confirm.
	actTimeout = 45 * time.Second
)

// Proposer is the core approval service as the scheduler sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
	Act(context.Context, string, approval.Proposal) (string, error)
}

// ExposureChecker reports whether the calling conversation has read content from outside the
// household, after which reminders wait for the user's confirmation.
type ExposureChecker interface {
	External(ctx context.Context, principalID string) bool
}

var (
	whenRepeating = locale.T("%[1]s; next: %[2]s", "%[1]s; следующий запуск: %[2]s")
	whenAnchored  = locale.T("for %[1]s, %[2]s", "на %[1]s, %[2]s")
	whenAt        = locale.T("for %s", "на %s")
	onlyIf        = locale.T("%[1]s, only if %[2]s", "%[1]s, только если %[2]s")

	describeOnce         = locale.T("once, %s", "один раз, %s")
	describeOnceAnchored = locale.T("once, %[1]s, %[2]s", "один раз, %[1]s, %[2]s")

	setReminderSummary = locale.T("Set a reminder %[1]s:\n%[2]s", "Поставить напоминание %[1]s:\n%[2]s")
	outsideContentNote = locale.T("I read outside content in this conversation, so I need your confirmation.", "В этом разговоре был прочитан внешний контент, поэтому нужно твоё подтверждение.")

	scheduleHeading      = locale.T("Schedule %s", "Запланировать %s")
	changeHeading        = locale.T("Change %s", "Изменить %s")
	changeRenamedHeading = locale.T("Change %[1]s, renamed to %[2]s", "Изменить %[1]s, новое название %[2]s")
	deleteSummary        = locale.T("Delete the scheduled job %[1]s (%[2]s). It will not run again.", "Удалить задание %[1]s (%[2]s). Оно больше не запустится.")

	instructionsStaySummary   = locale.T("%s. Its instructions stay as they are.", "%s. Инструкции остаются прежними.")
	instructionsChangeSummary = locale.T("%s. Its instructions change only here:", "%s. Инструкции меняются только здесь:")
	editRemoved               = locale.T("(removed)", "(удалено)")
	editWasNow                = locale.T("Was: %[1]s\nNow: %[2]s", "Было: %[1]s\nСтало: %[2]s")
	defaultModelNote          = locale.T("It goes back to the default model.", "Задание вернётся к модели по умолчанию.")
	noConditionNote           = locale.T("It no longer has a condition, so it runs every time.", "Условия больше нет, поэтому задание будет запускаться каждый раз.")
	leftProjectNote           = locale.T("Its runs no longer go to the tab of the project it belonged to.", "Его запуски больше не попадают во вкладку проекта, к которому оно относилось.")
	joinedProjectNote         = locale.T("Its runs go to the tab of the project it belongs to.", "Его запуски попадают во вкладку проекта, к которому оно относится.")
	staysPausedNote           = locale.T("It stays paused until you resume it in /jobs.", "Оно остаётся на паузе, пока ты не возобновишь его в /jobs.")

	summaryRuns             = locale.T("Runs: %s", "Запуск: %s")
	summaryRunsCron         = locale.T("Runs: %[1]s (%[2]s)", "Запуск: %[1]s (%[2]s)")
	summaryNext             = locale.T("Next: %s", "Следующий запуск: %s")
	summaryRunsOnce         = locale.T("Runs once: %s", "Один раз: %s")
	summaryRunsOnceAnchored = locale.T("Runs once: %[1]s, %[2]s, and moves with it", "Один раз: %[1]s, %[2]s; переносится вместе с событием")
	summaryTravel           = locale.T("Pauses while you are traveling", "Приостанавливается, пока ты в поездке")
	summaryCondition        = locale.T("Only if: %s, checked when it is due; otherwise it stays silent", "Только если: %s; это проверяется перед запуском, а иначе сообщения не будет")
	summaryModel            = locale.T("Model: %s", "Модель: %s")
	summaryEachRun          = locale.T("Each run asks your model, with read-only tools, and sends you the reply", "Каждый запуск спрашивает твою модель, только с инструментами чтения, и присылает тебе ответ")
	summaryOnceRun          = locale.T("It asks your model, with read-only tools, and sends you the reply", "Задание спрашивает твою модель, только с инструментами чтения, и присылает тебе ответ")

	scheduledAnchored  = locale.T("Scheduled %s.", "Запланировано: %s.")
	scheduledOnce      = locale.T("Scheduled for %s.", "Запланировано на %s.")
	scheduledRecurring = locale.T("Scheduled. First run: %s.", "Запланировано. Первый запуск: %s.")
	reminderSetOutcome = locale.T("Reminder set %s.", "Напоминание поставлено: %s.")
	deletedOutcome     = locale.T("Deleted.", "Удалено.")
	changedPaused      = locale.T("Changed. It stays paused until you resume it in /jobs.", "Изменено. Задание остаётся на паузе, пока ты не возобновишь его в /jobs.")
	changedAnchored    = locale.T("Changed. Runs %s.", "Изменено. Запуск: %s.")
	changedOnce        = locale.T("Changed. Runs once: %s.", "Изменено. Один раз: %s.")
	changedRecurring   = locale.T("Changed. Next run: %s.", "Изменено. Следующий запуск: %s.")

	tooManyOneTime   = locale.T("You already have %d pending reminders and one-time jobs, which is the most allowed; delete one in /jobs first.", "У тебя уже %d напоминаний и разовых заданий, это максимум; сначала удали одно в /jobs.")
	tooManyRecurring = locale.T("You already have %d recurring jobs, which is the most allowed; delete one in /jobs first.", "У тебя уже %d повторяющихся заданий, это максимум; сначала удали одно в /jobs.")

	nothingScheduled     = locale.T("Nothing was scheduled.", "Ничего не запланировано.")
	nothingSet           = locale.T("Nothing was set.", "Ничего не поставлено.")
	nothingScheduledWord = locale.T("scheduled", "запланировано")
	nothingSetWord       = locale.T("set", "поставлено")
	nothingChangedWord   = locale.T("changed", "изменено")

	jobUnreadable               = locale.T("The confirmed job could not be read; nothing was scheduled.", "Не удалось прочитать подтверждённое задание; ничего не запланировано.")
	scheduleInvalidNotScheduled = locale.T("That schedule is no longer valid (%s); nothing was scheduled.", "Это расписание больше не подходит (%s); ничего не запланировано.")
	reminderUnreadable          = locale.T("The confirmed reminder could not be read; nothing was set.", "Не удалось прочитать подтверждённое напоминание; ничего не поставлено.")
	reminderInvalid             = locale.T("That reminder can no longer be set (%s); nothing was set.", "Это напоминание больше нельзя поставить (%s); ничего не поставлено.")
	deletionUnreadable          = locale.T("The confirmed action could not be read; nothing was deleted.", "Не удалось прочитать подтверждённое действие; ничего не удалено.")
	jobGone                     = locale.T("That job no longer exists.", "Этого задания больше нет.")
	actionUnavailable           = locale.T("That action is no longer available.", "Это действие больше недоступно.")
	projectInvalid              = locale.T("That project can no longer take jobs (%[1]s); nothing was %[2]s.", "Этот проект больше не принимает задания (%[1]s); ничего не %[2]s.")
	changeUnreadable            = locale.T("The confirmed change could not be read; nothing was changed.", "Не удалось прочитать подтверждённое изменение; ничего не изменено.")
	jobGoneNotChanged           = locale.T("That job no longer exists; nothing was changed.", "Этого задания больше нет; ничего не изменено.")
	jobStale                    = locale.T("That job changed after this was proposed; nothing was changed. Ask again to change it.", "Задание изменилось после этого предложения; ничего не изменено. Попроси изменить его ещё раз.")
	scheduleInvalidNotChanged   = locale.T("That schedule is no longer valid (%s); nothing was changed.", "Это расписание больше не подходит (%s); ничего не изменено.")
	tripJobNotMovable           = locale.T("That job's runs go where its trip sends them, so it cannot be moved to a project; nothing was changed.", "Запуски этого задания идут туда, куда их направляет поездка, поэтому его нельзя перенести в проект; ничего не изменено.")
)

// actionError is a refusal whose English text the model and the logs read and whose user text
// follows the context's language.
func actionError(ctx context.Context, reason string, message locale.Text, args ...any) *approval.ActionError {
	return &approval.ActionError{Reason: reason, Text: message.FormatIn(locale.English, args...), UserText: message.Format(ctx, args...)}
}

const listGuidance = "Scheduled jobs: schedule_list shows the user's pending reminders and scheduled jobs, each with its id, schedule, next run, and instructions, shortening long instructions; schedule_get shows one of them in full. The user manages them with /jobs as well."

func proposeGuidance(zone string) string {
	return "The user can ask for something to happen on a schedule, such as every Monday at 9 checking their Notion tasks and reporting what is overdue, or once, such as tomorrow at 7:30 checking whether their train is delayed. " +
		"When the user asked for it in this conversation and the conversation has read no outside content, schedule_propose, schedule_update, and schedule_remove make the change at once and the user can undo it; tell the user it is done. " +
		"After outside content, such as a web page or an email, they only propose it: the user sees it with Confirm and Reject buttons, and new or changed instructions also get a Preview button that runs them once without saving, so never claim it was made. " +
		"A recurring job takes cron, a standard five-field cron expression (minute hour day-of-month month day-of-week) in the user's time zone, " + zone + " unless they name another; " +
		"for example 0 9 * * 1 is Mondays at 09:00 and 30 7 * * 1-5 is weekdays at 07:30. A recurring job may run at most once an hour. " +
		"A one-time job takes at instead, a local date and time such as 2026-10-04 07:30 that you work out from the current time. " +
		"Set pause_while_traveling on a recurring job that only matters while the user is home, such as cleaning, bins, the commute, or home devices, so it stays silent during a trip and resumes by itself afterwards; never set it for a one-time job or a reminder, and leave it out for anything that should still arrive while away. " +
		"A job meant to \"only tell me when…\" should say so in its instructions; its runs then stay silent unless the condition holds. " +
		"Each run is a separate turn the user does not watch, with only read-only tools, so the prompt must say exactly what to look at and what to report, on its own, without relying on this conversation. " +
		"schedule_update changes a job by its id from schedule_list, given only the fields that change. " +
		"To change its instructions, first read them with schedule_get, then pass prompt_edits: small find and replace pairs, each find copied exactly from the stored prompt and occurring in it once, that change only what the user asked for; the user sees just those passages, old and new. " +
		"Never rewrite or reword the parts the user did not ask to change, and pass prompt, the complete new instructions, only when the user asks for a real rewrite. " +
		"A new job's prompt or a rewrite must fit one confirmation, about 2000 characters; longer instructions, up to 10000 characters, are grown with prompt_edits once the job exists. " +
		"The Heads-up job's text is only the user's notes: how it checks the day is built in and not shown. When the user corrects a Heads-up report, add or change one short note with prompt_edits that states the lasting preference behind the correction, " +
		"such as what an event in their calendar means or what to leave out, never a step of how to check the day and never one day's mistake. " +
		"Never invent a value the user did not give, such as a time, a buffer, a walking time, or a threshold; ask for it instead. " +
		"A recurring job stays recurring and a one-time job stays one-time; to switch, remove it and propose a new one. " +
		"schedule_remove deletes a job by its id from schedule_list. Make every job change the request needs in this turn, once each. Proposals of one turn are shown together and confirmed or rejected as one batch; withdraw a wrong one with proposal_withdraw, and a proposal in a later turn replaces a batch still waiting."
}

func reminderGuidance(zone string) string {
	return "Reminders: when the user only wants to be told something at a time, such as remind me at 5 to call mom or in 20 minutes tell me to take the laundry out, use reminder_set, not schedule_propose. " +
		"It sends the text as written at that time, without asking a model, and is set at once without confirmation, except after you have read content from outside the household, such as a web page or an email, in this conversation: then reminder_set only proposes the reminder, the user sees it with Confirm and Reject buttons, and you must not claim it is set. " +
		"at is a local date and time such as 2026-10-04 17:00 in the user's time zone, " + zone + " unless they name another; work it out from the current time. " +
		"Write the text as the reminder the user should read, such as Call mom. Repeat the time reminder_set returns to the user exactly as it gives it. " +
		"reminder_cancel cancels a pending reminder by its id from schedule_list, also without confirmation. A reminder that should repeat on a clock schedule is a recurring schedule_propose job instead; one before each matching calendar event takes a calendar_match anchor."
}

// anchorGuidance says when to anchor a job to an event instead of a clock time, naming only the
// kinds of anchor that can be resolved here.
func (s *Service) anchorGuidance() string {
	var kinds []string
	if _, known := s.anchors[AnchorCalendarEvent]; known {
		kinds = append(kinds, "calendar_event with the account, calendar_id, and event_id from calendar_events for one event")
	}
	if _, known := s.anchors[AnchorCalendarMatch]; known {
		kinds = append(kinds, "calendar_match with a title for each upcoming event whose title contains it")
	}
	if _, known := s.anchors[AnchorTrip]; known {
		kinds = append(kinds, "trip with a trip_id from trip_list and edge start or end")
	}
	_, place := s.anchors[AnchorPlace]
	if place {
		kinds = append(kinds, "place with a place_id from place_list and edge arrive or leave, which runs on every arrival or leaving unless once is set")
	}
	if len(kinds) == 0 {
		return ""
	}
	guidance := "Anchors: when the user times a reminder or job by an event rather than a clock time, such as 30 minutes before the dentist, an hour before every meeting titled Planning, or when their trip starts, give anchor instead of cron or at: " +
		strings.Join(kinds, "; ") + ". offset_minutes is negative for before the event and positive for after it. " +
		"Look the event or trip up first rather than guessing its ids. An anchored job follows its event when it moves, and when the event is cancelled it ends and the user is told in one line, so prefer an anchor to a time copied from the event."
	if place {
		guidance += " A place anchor, such as when I get home remind me to water the plants, runs 0 to 240 minutes after the arrival or leaving, and only while the user shares their live location; tell the user so, and that it may notice the arrival late."
	}
	return guidance
}

const conditionGuidance = "Conditions: any reminder or job can take a condition, checked once when it is due; when it does not hold, the run is skipped and nothing is sent, such as at 07:40 on weekdays, tell me only if rain is coming in the next hour, which is a weather condition with within_minutes 60. " +
	"Prefer a code-checked kind, weather, home, departure, or disruptions, which costs no model turn, to agent, which costs one each run; a reminder takes only a weather or home condition. Prefer a condition to writing only tell me when into a job's instructions whenever a code-checked kind can tell. " +
	"A home condition takes no for_minutes and route is not a condition. A check that fails lets the run go ahead with a note. Never invent a threshold the user did not give: weather defaults to 0.2 mm in 15 minutes."

var listDefinition = tools.Definition{
	Name:        toolList,
	Description: "List the user's scheduled jobs with their ids, schedules, next runs, and instructions.",
	Risk:        tools.RiskRead,
	InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
}

var getDefinition = tools.Definition{
	Name:        toolGet,
	Description: "Show one of the user's scheduled jobs or reminders in full, with its complete instructions.",
	Risk:        tools.RiskRead,
	// The job's header and maximumPromptRunes instructions of up to four bytes each.
	MaxResultBytes: 48 << 10,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"job_id":{"type":"string","description":"The id from schedule_list."}},` +
		`"required":["job_id"],"additionalProperties":false}`),
}

// anchorProperty and conditionProperty are the anchor and condition every job tool takes.
const anchorProperty = `"anchor":{"type":"object","description":"Instead of cron or at: run relative to an event, and follow it when it moves.","properties":{` +
	`"kind":{"type":"string","enum":["calendar_event","calendar_match","trip","place"],"description":"calendar_event: one event from calendar_events; calendar_match: each upcoming event whose title contains title; trip: a trip's start or end; place: each arrival at or leaving of a saved place."},` +
	`"account":{"type":"string","description":"calendar_event: the account calendar_events shows for the event. calendar_match: optional, to match only that account's events."},` +
	`"calendar_id":{"type":"string","description":"calendar_event: the event's calendar id. calendar_match: optional, to match only that calendar."},` +
	`"event_id":{"type":"string","description":"calendar_event: the event id from calendar_events."},` +
	`"title":{"type":"string","maxLength":100,"description":"calendar_match: text the event titles contain, such as Planning."},` +
	`"trip_id":{"type":"string","description":"trip: the trip's id from trip_list."},` +
	`"place_id":{"type":"string","description":"place: the saved place's id from place_list."},` +
	`"edge":{"type":"string","enum":["start","end","arrive","leave"],"description":"trip: start or end. place: arrive or leave."},` +
	`"once":{"type":"boolean","description":"place: run on the next arrival or leaving only, then finish; by default it runs on every one."},` +
	`"offset_minutes":{"type":"integer","minimum":-2880,"maximum":1440,"description":"Minutes after the event's start, or the trip's start or end; negative for before, such as -30 for half an hour before. place: 0 to 240 minutes after arriving or leaving."}},` +
	`"required":["kind","offset_minutes"],"additionalProperties":false}`

const conditionProperty = `"condition":{"type":"object","description":"Run only if this holds when the job is due, checked once then; otherwise the run is skipped without a message. ` +
	`kind is weather, home, departure, disruptions, or agent, with the fields watch_propose takes for that kind but no days, date, window, or triggers: ` +
	`weather takes name or current_location, within_minutes, and threshold_mm; home takes name and condition without for_minutes; departure takes mode, its stations or stop and line, departure, and delay_minutes; disruptions takes stations; agent takes name and check."}`

// projectProperty routes a job's runs to one of the user's projects; no other destination can be
// named by the model.
const projectProperty = `"project_id":{"type":"string","description":"Only when this belongs to one of the user's projects: the project's id from project_list, so its runs arrive in the project's tab."}`

// projectUpdateProperty is projectProperty for a change, which can also take a job off its
// project.
const projectUpdateProperty = `"project_id":{"type":"string","description":"Only when the user ties this job to one of their projects: the project's id from project_list, so its runs arrive in the project's tab; none takes the job off its project; omit it to keep the job where it goes."}`

// noProject is the project_id that takes a job off its project.
const noProject = "none"

var reminderDefinitions = []tools.Definition{
	{
		Name:        toolReminderSet,
		Description: "Set a reminder: the text is sent to the user once, at the given local time or relative to an event, without confirmation unless outside content was read in this conversation.",
		Risk:        tools.RiskWrite,
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"text":{"type":"string","maxLength":500,"description":"The reminder as the user should read it, at most 500 characters."},` +
			`"at":{"type":"string","description":"Local date and time, YYYY-MM-DD HH:MM, such as 2026-10-04 17:00; or give anchor instead."},` +
			anchorProperty + `,` + conditionProperty + `,` + projectProperty + `,` +
			`"timezone":{"type":"string","description":"IANA time zone such as Europe/Amsterdam; omit to use the user's own."}},` +
			`"required":["text"],"additionalProperties":false}`),
	},
	{
		Name:        toolReminderCancel,
		Description: "Cancel one of the user's pending reminders, without confirmation.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"job_id":{"type":"string","description":"The reminder's id from schedule_list."}},` +
			`"required":["job_id"],"additionalProperties":false}`),
	},
}

var writeDefinitions = []tools.Definition{
	{
		Name:        toolPropose,
		Description: "Schedule a job that runs a prompt on a cron schedule, or once at a local time, with read-only tools and sends the result to the user. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","maxLength":60,"description":"A short name the user will recognise, such as Overdue Notion tasks."},` +
			`"cron":{"type":"string","description":"For a recurring job: a five-field cron expression, such as 0 9 * * 1 for Mondays at 09:00."},` +
			`"at":{"type":"string","description":"For a one-time job instead of cron: local date and time, YYYY-MM-DD HH:MM."},` +
			`"prompt":{"type":"string","maxLength":2000,"description":"The complete instructions for each run, at most 2000 characters, because the user sees them in full to confirm. Once the job exists, prompt_edits in schedule_update can grow them to 10000 characters."},` +
			`"timezone":{"type":"string","description":"IANA time zone such as Europe/Amsterdam; omit to use the user's own."},` +
			`"pause_while_traveling":{"type":"boolean","description":"Recurring jobs only: stay silent while the user is on a trip."},` +
			anchorProperty + `,` + conditionProperty + `,` + projectProperty + `,` +
			`"model":{"type":"string","maxLength":200,"description":"The model and effort the job runs on, as id:effort such as ` + modelExample + `. Set it only when the user asks for a different model for this job; omit it to use the default."}},` +
			`"required":["name","prompt"],"additionalProperties":false}`),
	},
	{
		Name:        toolUpdate,
		Description: "Change one of the user's scheduled jobs; omitted fields keep their current values. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"job_id":{"type":"string","description":"The id from schedule_list."},` +
			`"name":{"type":"string","maxLength":60,"description":"A new short name."},` +
			`"cron":{"type":"string","description":"For a recurring job: a new five-field cron expression."},` +
			`"at":{"type":"string","description":"For a one-time job: a new local date and time, YYYY-MM-DD HH:MM."},` +
			`"prompt":{"type":"string","maxLength":2000,"description":"The complete new instructions, replacing the old ones, at most 2000 characters, because the user sees them in full to confirm; prompt_edits can grow a job's instructions to 10000 characters. Only for a real rewrite; not together with prompt_edits."},` +
			`"prompt_edits":{"type":"array","minItems":1,"maxItems":10,"description":"Changes to the current instructions, applied in order. Prefer these to prompt.","items":{"type":"object","properties":{` +
			`"find":{"type":"string","minLength":1,"maxLength":1000,"description":"A passage copied exactly from the current instructions; it must occur there exactly once."},` +
			`"replace":{"type":"string","maxLength":1000,"description":"The text that replaces it; empty to delete the passage."}},` +
			`"required":["find","replace"],"additionalProperties":false}},` +
			`"timezone":{"type":"string","description":"A new IANA time zone such as Europe/Amsterdam."},` +
			`"pause_while_traveling":{"type":"boolean","description":"Recurring jobs only: true to stay silent while the user is on a trip, false to stop doing so; omit to keep it as it is."},` +
			anchorProperty + `,` + conditionProperty + `,` + projectUpdateProperty + `,` +
			`"remove_condition":{"type":"boolean","description":"true to drop the job's condition, so it runs every time again."},` +
			`"model":{"type":"string","maxLength":200,"description":"The model and effort the job runs on, as id:effort such as ` + modelExample + `. Set it only when the user asks for a different model for this job; an empty string returns it to the default, and omitting it keeps the current one."}},` +
			`"required":["job_id"],"additionalProperties":false}`),
	},
	{
		Name:        toolRemove,
		Description: "Delete one of the user's scheduled jobs. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"job_id":{"type":"string","description":"The id from schedule_list."}},` +
			`"required":["job_id"],"additionalProperties":false}`),
	},
}

var _ tools.Provider = (*Service)(nil)
var _ approval.Executor = (*Service)(nil)
var _ approval.Previewer = (*Service)(nil)
var _ approval.Undoer = (*Service)(nil)

// Contribute offers the list and get tools, the reminder tools when the reminder type is
// registered, and the proposal tools when there is an approval service and the model.turn type is
// registered, and job_reports when the principal has a stored report. A scheduled run itself
// receives only the read tools, because the broker offers it read tools alone.
func (s *Service) Contribute(ctx context.Context, principalID string) (tools.Contribution, error) {
	contribution := tools.Contribution{Guidance: listGuidance, Definitions: []tools.Definition{listDefinition, getDefinition}}
	if err := s.contributeReports(ctx, principalID, &contribution); err != nil {
		return tools.Contribution{}, err
	}
	var guidance []string
	if _, known := s.types[TypeReminder]; known {
		guidance = append(guidance, reminderGuidance(s.zone))
		contribution.Definitions = append(contribution.Definitions, reminderDefinitions...)
	}
	if _, known := s.types[TypeModelTurn]; known && s.proposer != nil {
		guidance = append(guidance, proposeGuidance(s.zone))
		contribution.Definitions = append(contribution.Definitions, writeDefinitions...)
	}
	if len(guidance) > 0 {
		if text := s.anchorGuidance(); text != "" {
			guidance = append(guidance, text)
		}
		if s.conditions != nil {
			guidance = append(guidance, conditionGuidance)
		}
	}
	contribution.WriteGuidance = strings.Join(guidance, "\n")
	return contribution, nil
}

func (s *Service) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case toolList:
		jobs, err := s.repository.ListScheduledJobs(ctx, principalID)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Text: s.renderJobs(ctx, jobs)}, nil
	case toolGet:
		return s.getJob(ctx, principalID, call)
	case toolReports:
		return s.executeReports(ctx, principalID, call)
	case toolPropose:
		return s.proposeJob(ctx, principalID, call)
	case toolUpdate:
		return s.proposeUpdate(ctx, principalID, call)
	case toolRemove:
		return s.proposeRemoval(ctx, principalID, call)
	case toolReminderSet:
		return s.setReminder(ctx, principalID, call)
	case toolReminderCancel:
		return s.cancelReminder(ctx, principalID, call)
	default:
		return tools.Result{Text: "unknown scheduling tool", IsError: true}, nil
	}
}

type proposeArguments struct {
	Name                string          `json:"name"`
	Cron                string          `json:"cron"`
	At                  string          `json:"at"`
	Prompt              string          `json:"prompt"`
	Timezone            string          `json:"timezone"`
	PauseWhileTraveling bool            `json:"pause_while_traveling"`
	Model               string          `json:"model"`
	Anchor              *Anchor         `json:"anchor"`
	Condition           json.RawMessage `json:"condition"`
	ProjectID           string          `json:"project_id"`
}

type confirmedReminder struct {
	Text      string     `json:"text"`
	At        string     `json:"at"`
	Zone      string     `json:"zone"`
	Anchor    *Anchor    `json:"anchor,omitempty"`
	Condition *Condition `json:"condition,omitempty"`
	ProjectID string     `json:"project_id,omitempty"`
}

// confirmedJob is the canonical form the user confirms and the executor stores. At is the local
// time as parsed, so it is validated again when the user presses Confirm.
type confirmedJob struct {
	Name   string `json:"name"`
	Cron   string `json:"cron,omitempty"`
	At     string `json:"at,omitempty"`
	Zone   string `json:"zone"`
	Prompt string `json:"prompt"`
	// TravelPause is nil unless the model set the flag, so Confirm keeps the job's current value
	// instead of one read when the proposal was made.
	TravelPause *bool `json:"pause_while_traveling,omitempty"`
	// Model is the job's own "id:effort", empty for its default.
	Model string `json:"model,omitempty"`
	// Anchor and Condition are as stored, with the anchor's label as its resolver read it.
	Anchor    *Anchor    `json:"anchor,omitempty"`
	Condition *Condition `json:"condition,omitempty"`
	// ProjectID is the project the job's runs go to, or "".
	ProjectID string `json:"project_id,omitempty"`
}

// config is the job's stored configuration.
func (c confirmedJob) config() (json.RawMessage, error) {
	handler, err := ModelTurnConfigWithModel(c.Prompt, c.Model)
	if err != nil {
		return nil, err
	}
	return JoinConfig(handler, c.Anchor, c.Condition)
}

func (c confirmedJob) same(other confirmedJob) bool {
	encoded, errA := json.Marshal(c)
	otherEncoded, errB := json.Marshal(other)
	return errA == nil && errB == nil && string(encoded) == string(otherEncoded)
}

type updateArguments struct {
	JobID       string       `json:"job_id"`
	Name        string       `json:"name"`
	Cron        string       `json:"cron"`
	At          string       `json:"at"`
	Prompt      string       `json:"prompt"`
	PromptEdits []promptEdit `json:"prompt_edits"`
	Timezone    string       `json:"timezone"`
	// PauseWhileTraveling is nil when the model left the flag alone.
	PauseWhileTraveling *bool `json:"pause_while_traveling"`
	// Model is nil when the model left the job's model alone, and empty to return it to the default.
	Model           *string         `json:"model"`
	Anchor          *Anchor         `json:"anchor"`
	Condition       json.RawMessage `json:"condition"`
	RemoveCondition bool            `json:"remove_condition"`
	ProjectID       string          `json:"project_id"`
}

// promptEdit replaces the one occurrence of Find in a job's instructions with Replace.
type promptEdit struct {
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

// confirmedUpdate is the job as it will be after the change, with the job's last change as it was
// proposed, so a job changed before the press is not overwritten.
type confirmedUpdate struct {
	JobID     string    `json:"job_id"`
	UpdatedAt time.Time `json:"updated_at"`
	confirmedJob
}

type removeArguments struct {
	JobID string `json:"job_id"`
}

type reminderArguments struct {
	Text      string          `json:"text"`
	At        string          `json:"at"`
	Timezone  string          `json:"timezone"`
	Anchor    *Anchor         `json:"anchor"`
	Condition json.RawMessage `json:"condition"`
	ProjectID string          `json:"project_id"`
}

// projectDestination checks a project the model named for a job against the principal's own open
// projects and returns its id; "" names none. A failure is a refusal for the model to read.
func (s *Service) projectDestination(ctx context.Context, principalID, projectID string) (string, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		return "", nil
	}
	if s.projects == nil {
		return "", fmt.Errorf("projects are not available here, so leave project_id out")
	}
	open, err := s.projects.OpenProject(ctx, principalID, id)
	if err != nil {
		return "", fmt.Errorf("the project could not be checked; try again")
	}
	if !open {
		return "", fmt.Errorf("no open project has the id %q; find it with project_list", id)
	}
	return id, nil
}

// triggers reads the anchor and condition the model gave a new job; light refuses a condition that
// needs a model turn. A failure is a refusal for the model to read.
func (s *Service) triggers(ctx context.Context, principalID string, rawAnchor *Anchor, rawCondition json.RawMessage, light bool) (*Anchor, *Condition, error) {
	var anchor *Anchor
	if rawAnchor != nil {
		normalized, err := normalizeAnchor(*rawAnchor)
		if err != nil {
			return nil, nil, err
		}
		if _, known := s.anchors[normalized.Kind]; !known {
			return nil, nil, fmt.Errorf("%s anchors are not available, because that integration is not set up", normalized.Kind)
		}
		anchor = &normalized
	}
	var condition *Condition
	if given(rawCondition) {
		var err error
		if condition, err = s.prepareCondition(ctx, principalID, rawCondition, light); err != nil {
			return nil, nil, err
		}
	}
	return anchor, condition, nil
}

// given reports whether the model passed a JSON value other than null.
func given(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func (s *Service) setReminder(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments reminderArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	anchor, condition, err := s.triggers(ctx, principalID, arguments.Anchor, arguments.Condition, true)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	text, err := ReminderConfig(arguments.Text)
	if err != nil {
		return tools.Result{}, err
	}
	config, err := JoinConfig(text, anchor, condition)
	if err != nil {
		return tools.Result{}, err
	}
	projectID, err := s.projectDestination(ctx, principalID, arguments.ProjectID)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	timing := Timing{At: arguments.At, Zone: arguments.Timezone}
	prepared, err := s.prepareJob(ctx, principalID, ReminderName(arguments.Text), TypeReminder, timing, config)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if projectID != "" {
		prepared.Job.Destination = thread.ProjectDestination(projectID)
	}
	if s.exposure != nil && s.exposure.External(ctx, principalID) {
		return s.proposeReminder(ctx, principalID, call, ReminderText(config), prepared, projectID)
	}
	id, err := newRunID()
	if err != nil {
		return tools.Result{}, err
	}
	if _, err := s.Create(ctx, id, prepared); err != nil {
		var refusal *approval.ActionError
		switch {
		case errors.Is(err, ErrTooManyJobs):
			return tools.Result{Text: tooManyText(true), IsError: true}, nil
		case errors.As(err, &refusal):
			return tools.Result{Text: refusal.Text, IsError: true}, nil
		}
		return tools.Result{}, err
	}
	return tools.Result{Text: withNote(fmt.Sprintf("Reminder set %s (id %s): %s", reminderWhen(prepared), id, ReminderText(config)), prepared)}, nil
}

// reminderWhen is when a prepared reminder goes off, as "for Mon 5 Oct 17:00" or, for an anchored
// one, with the event it follows and its condition.
func reminderWhen(prepared Prepared) string { return reminderWhenIn(locale.English, prepared) }

func reminderWhenIn(language locale.Language, prepared Prepared) string {
	job := prepared.Job
	var when string
	switch {
	case prepared.Anchor != nil && prepared.Anchor.Repeats():
		when = whenRepeating.FormatIn(language, prepared.Anchor.DescribeIn(language), NextRunTextIn(language, job, job.Zone))
	case prepared.Anchor != nil:
		when = whenAnchored.FormatIn(language, FormatOccurrenceIn(language, job.RunAt, prepared.Schedule.Location), prepared.Anchor.DescribeIn(language))
	default:
		when = whenAt.FormatIn(language, FormatOccurrenceIn(language, job.RunAt, prepared.Schedule.Location))
	}
	if condition := JobCondition(job); condition != nil {
		when = onlyIf.FormatIn(language, when, condition.Description)
	}
	return when
}

// withNote adds what the anchor's resolver tells the user, such as that arrivals need a live
// location shared, after a line about a saved job.
func withNote(text string, prepared Prepared) string {
	if prepared.note == "" {
		return text
	}
	return text + " " + prepared.note
}

func (s *Service) proposeReminder(ctx context.Context, principalID string, call tools.Call, text string, prepared Prepared, projectID string) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "I read outside content in this conversation, so reminders need the user's confirmation, and confirming is not available. Tell the user the reminder was not set.", IsError: true}, nil
	}
	proposal, err := reminderProposal(locale.From(ctx), text, prepared, projectID)
	if err != nil {
		return tools.Result{}, err
	}
	proposal.Summary += "\n" + outsideContentNote.Format(ctx)
	if _, err := s.proposer.Propose(ctx, principalID, proposal); err != nil {
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		s.logger.WarnContext(ctx, "schedule.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return tools.Result{Text: "the proposal could not be recorded", IsError: true}, nil
	}
	return tools.Result{Text: approval.ProposedText}, nil
}

// reminderProposal is the proposal that sets the prepared reminder of text once confirmed.
func reminderProposal(language locale.Language, text string, prepared Prepared, projectID string) (approval.Proposal, error) {
	job := prepared.Job
	confirmed := confirmedReminder{Text: text, Zone: job.Zone, Anchor: prepared.Anchor, Condition: JobCondition(job), ProjectID: projectID}
	if prepared.Anchor == nil {
		confirmed.At = job.RunAt.In(prepared.Schedule.Location).Format(localTimeLayouts[0])
	}
	canonical, err := json.Marshal(confirmed)
	if err != nil {
		return approval.Proposal{}, err
	}
	return approval.Proposal{
		Tool: toolReminderSet, Arguments: canonical,
		Summary: setReminderSummary.FormatIn(language, reminderWhenIn(language, prepared), text),
		Intent:  approval.Intent{Verb: "set", TargetKind: "reminder", TargetName: job.Name},
	}, nil
}

// ReminderProposal builds the proposal of a one-time reminder of text at the local time at,
// YYYY-MM-DD HH:MM in the principal's default zone, for a caller outside a chat turn to offer, such
// as the inbox check. It is the proposal reminder_set makes after outside content, without the line
// about the conversation; an error says why the reminder cannot be set.
func (s *Service) ReminderProposal(ctx context.Context, principalID, text, at string) (approval.Proposal, error) {
	config, err := ReminderConfig(text)
	if err != nil {
		return approval.Proposal{}, err
	}
	prepared, err := s.prepareJob(ctx, principalID, ReminderName(text), TypeReminder, Timing{At: at}, config)
	if err != nil {
		return approval.Proposal{}, err
	}
	return reminderProposal(locale.From(ctx), ReminderText(config), prepared, "")
}

func (s *Service) cancelReminder(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments removeArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, strings.TrimSpace(arguments.JobID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found || job.Type != TypeReminder || !job.FinishedAt.IsZero() {
		return tools.Result{Text: "No pending reminder has that id. Use schedule_list to see the user's reminders; other jobs are removed with schedule_remove.", IsError: true}, nil
	}
	if _, err := s.Delete(ctx, principalID, job.ID); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Text: "Cancelled the reminder: " + ReminderText(job.Config)}, nil
}

func (s *Service) proposeJob(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "Scheduling is not available.", IsError: true}, nil
	}
	var arguments proposeArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	model, refusal, err := s.offeredModel(ctx, principalID, arguments.Model)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return tools.Result{Text: refusal, IsError: true}, nil
	}
	anchor, condition, err := s.triggers(ctx, principalID, arguments.Anchor, arguments.Condition, false)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	projectID, err := s.projectDestination(ctx, principalID, arguments.ProjectID)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	config, err := confirmedJob{Prompt: arguments.Prompt, Model: model, Anchor: anchor, Condition: condition}.config()
	if err != nil {
		return tools.Result{}, err
	}
	timing := Timing{Cron: arguments.Cron, At: arguments.At, Zone: arguments.Timezone}
	prepared, err := s.prepareJob(ctx, principalID, arguments.Name, TypeModelTurn, timing, config)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if arguments.PauseWhileTraveling && prepared.Job.OneTime() {
		return tools.Result{Text: oneTimeTravelPauseText, IsError: true}, nil
	}
	prepared.Job.TravelPause = arguments.PauseWhileTraveling
	job := prepared.Job
	existing, err := s.repository.ListScheduledJobs(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if countKind(existing, job.OneTime()) >= job.Limit() {
		return tools.Result{Text: tooManyText(job.OneTime()), IsError: true}, nil
	}

	confirmed := confirmedJob{Name: job.Name, Cron: job.Cron, Zone: job.Zone, Prompt: ModelTurnPrompt(job.Config), Model: ModelTurnModel(job.Config), Anchor: prepared.Anchor, Condition: JobCondition(job), ProjectID: projectID}
	if job.TravelPause {
		on := true
		confirmed.TravelPause = &on
	}
	targetKind := "recurring job"
	switch {
	case prepared.Anchor != nil:
		targetKind = "anchored job"
	case job.OneTime():
		confirmed.At = job.RunAt.In(prepared.Schedule.Location).Format(localTimeLayouts[0])
		targetKind = "one-time job"
	}
	canonical, err := json.Marshal(confirmed)
	if err != nil {
		return tools.Result{}, err
	}
	language := locale.From(ctx)
	summary := proposalSummary(language, scheduleHeading.FormatIn(language, quotedIn(language, job.Name)), prepared)
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolPropose, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "schedule", TargetKind: targetKind, TargetName: job.Name}}, longNewJobText)
}

// act makes the change at once in a clean turn and proposes it otherwise. tooLong answers a change
// too long to show for confirmation, saying how to make it fit; empty keeps the generic refusal.
func (s *Service) act(ctx context.Context, principalID string, call tools.Call, proposal approval.Proposal, tooLong string) (tools.Result, error) {
	text, err := s.proposer.Act(ctx, principalID, proposal)
	if err != nil {
		if errors.Is(err, approval.ErrSummaryTooLong) && tooLong != "" {
			return tools.Result{Text: tooLong, IsError: true}, nil
		}
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		s.logger.WarnContext(ctx, "schedule.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return tools.Result{Text: "the change could not be recorded", IsError: true}, nil
	}
	return tools.Result{Text: text}, nil
}

func (s *Service) getJob(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments removeArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, strings.TrimSpace(arguments.JobID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found || !job.FinishedAt.IsZero() {
		return tools.Result{Text: "No pending job has that id. Use schedule_list to see the user's jobs.", IsError: true}, nil
	}
	return tools.Result{Text: renderJob(job, s.zone, maximumPromptRunes, s.describeModel(ctx, job, true))}, nil
}

func (s *Service) proposeUpdate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "Scheduling is not available.", IsError: true}, nil
	}
	var arguments updateArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, strings.TrimSpace(arguments.JobID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found || !job.FinishedAt.IsZero() {
		return tools.Result{Text: "No pending job has that id. Use schedule_list to see the user's jobs.", IsError: true}, nil
	}
	if job.Type != TypeModelTurn {
		return tools.Result{Text: "Reminders cannot be changed; cancel it with reminder_cancel and set a new one with reminder_set.", IsError: true}, nil
	}
	current, err := currentJob(job)
	if err != nil {
		return tools.Result{Text: "That job's stored schedule can no longer be read; remove it and propose a new one.", IsError: true}, nil
	}
	anchored := current.Anchor != nil
	switch {
	case anchored && (strings.TrimSpace(arguments.Cron) != "" || strings.TrimSpace(arguments.At) != ""):
		return tools.Result{Text: "This job follows an event, so it takes a new anchor, not cron or at; to run it on a clock time, remove it and propose a new job.", IsError: true}, nil
	case !anchored && arguments.Anchor != nil:
		return tools.Result{Text: "This job runs on a clock time; to make it follow an event, remove it and propose a new job with an anchor.", IsError: true}, nil
	case job.OneTime() && strings.TrimSpace(arguments.Cron) != "":
		return tools.Result{Text: "This is a one-time job, so it takes at, not cron; to make it recurring, remove it and propose a new job.", IsError: true}, nil
	case !job.OneTime() && strings.TrimSpace(arguments.At) != "":
		return tools.Result{Text: "This is a recurring job, so it takes cron, not at; to make it run once, remove it and propose a new job.", IsError: true}, nil
	case arguments.RemoveCondition && given(arguments.Condition):
		return tools.Result{Text: "Give either condition or remove_condition, not both; nothing was proposed.", IsError: true}, nil
	case strings.TrimSpace(arguments.ProjectID) != "" && job.Destination != "" && current.ProjectID == "":
		return tools.Result{Text: "This job's runs go where its trip sends them, so it cannot be moved to a project; nothing was proposed.", IsError: true}, nil
	}
	if strings.TrimSpace(arguments.Prompt) != "" && len(arguments.PromptEdits) > 0 {
		return tools.Result{Text: "Give either prompt, the complete new instructions, or prompt_edits, not both; nothing was proposed.", IsError: true}, nil
	}
	next := current
	travel := job.TravelPause
	if arguments.PauseWhileTraveling != nil {
		if *arguments.PauseWhileTraveling && !TravelPausable(job) {
			return tools.Result{Text: unpausableTravelText, IsError: true}, nil
		}
		travel = *arguments.PauseWhileTraveling
	}
	override(&next.Name, arguments.Name)
	override(&next.Cron, arguments.Cron)
	override(&next.At, arguments.At)
	override(&next.Prompt, arguments.Prompt)
	override(&next.Zone, arguments.Timezone)
	if strings.EqualFold(strings.TrimSpace(arguments.ProjectID), noProject) {
		next.ProjectID = ""
	} else {
		projectID, err := s.projectDestination(ctx, principalID, arguments.ProjectID)
		if err != nil {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		override(&next.ProjectID, projectID)
	}
	if arguments.Model != nil {
		model, refusal, err := s.offeredModel(ctx, principalID, *arguments.Model)
		if err != nil {
			return tools.Result{}, err
		}
		if refusal != "" {
			return tools.Result{Text: refusal, IsError: true}, nil
		}
		next.Model = model
	}
	if len(arguments.PromptEdits) > 0 {
		edited, err := applyPromptEdits(current.Prompt, arguments.PromptEdits)
		if err != nil {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		next.Prompt = edited
	}
	anchor, condition, err := s.triggers(ctx, principalID, arguments.Anchor, arguments.Condition, false)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if anchor != nil {
		next.Anchor = anchor
	} else if next.Anchor != nil {
		// A copy, since resolving the job refreshes its label.
		copied := *next.Anchor
		next.Anchor = &copied
	}
	switch {
	case condition != nil:
		next.Condition = condition
	case arguments.RemoveCondition:
		next.Condition = nil
	}
	config, err := next.config()
	if err != nil {
		return tools.Result{}, err
	}
	prepared, err := s.prepareJob(ctx, principalID, next.Name, TypeModelTurn, Timing{Cron: next.Cron, At: next.At, Zone: next.Zone}, config)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	prepared.Job.TravelPause = travel
	changed := prepared.Job
	confirmed := confirmedUpdate{JobID: job.ID, UpdatedAt: job.UpdatedAt, confirmedJob: confirmedJob{Name: changed.Name, Cron: changed.Cron, Zone: changed.Zone, Prompt: ModelTurnPrompt(changed.Config), TravelPause: arguments.PauseWhileTraveling, Model: ModelTurnModel(changed.Config),
		Anchor: prepared.Anchor, Condition: JobCondition(changed), ProjectID: next.ProjectID}}
	if changed.OneTime() && prepared.Anchor == nil {
		confirmed.At = changed.RunAt.In(prepared.Schedule.Location).Format(localTimeLayouts[0])
	}
	unflagged := confirmed.confirmedJob
	unflagged.TravelPause = nil
	if unflagged.same(current) && travel == job.TravelPause {
		return tools.Result{Text: "That changes nothing; the job already reads exactly like this.", IsError: true}, nil
	}
	canonical, err := json.Marshal(confirmed)
	if err != nil {
		return tools.Result{}, err
	}
	language := locale.From(ctx)
	heading := changeHeading.FormatIn(language, quotedIn(language, job.Name))
	if changed.Name != job.Name {
		heading = changeRenamedHeading.FormatIn(language, quotedIn(language, job.Name), quotedIn(language, changed.Name))
	}
	// Unchanged instructions are left out, so a job grown past one message by prompt_edits can still
	// be renamed or rescheduled.
	summary, tooLong := instructionsStaySummary.FormatIn(language, scheduleSummary(language, heading, prepared)), ""
	switch {
	case len(arguments.PromptEdits) > 0:
		summary, tooLong = patchSummary(language, heading, prepared, arguments.PromptEdits), longEditsText
	case confirmed.Prompt != current.Prompt:
		summary, tooLong = proposalSummary(language, heading, prepared), longRewriteText
	}
	if next.Model == "" && current.Model != "" {
		summary += "\n" + defaultModelNote.In(language)
	}
	if next.Condition == nil && current.Condition != nil {
		summary += "\n" + noConditionNote.In(language)
	}
	switch {
	case next.ProjectID != current.ProjectID && next.ProjectID == "":
		summary += "\n" + leftProjectNote.In(language)
	case next.ProjectID != current.ProjectID:
		summary += "\n" + joinedProjectNote.In(language)
	}
	if !job.Enabled {
		summary += "\n" + staysPausedNote.In(language)
	}
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolUpdate, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "change", TargetKind: "scheduled job", TargetName: job.Name, NewName: changed.Name}}, tooLong)
}

// currentJob is a stored model.turn job in the canonical form a proposal carries, with a one-time
// job's instant as a local time in its own zone; an anchored job's instant follows its anchor.
func currentJob(job Job) (confirmedJob, error) {
	current := confirmedJob{Name: job.Name, Cron: job.Cron, Zone: job.Zone, Prompt: ModelTurnPrompt(job.Config), Model: ModelTurnModel(job.Config), Anchor: JobAnchor(job), Condition: JobCondition(job)}
	current.ProjectID, _ = thread.ProjectOf(job.Destination)
	if job.OneTime() && current.Anchor == nil {
		location, err := LoadZone(job.Zone)
		if err != nil {
			return confirmedJob{}, err
		}
		current.At = job.RunAt.In(location).Format(localTimeLayouts[0])
	}
	return current, nil
}

// applyPromptEdits applies the edits in order, each to the result of the ones before it. Every find
// must occur exactly once, so an edit never lands somewhere the model did not mean.
func applyPromptEdits(prompt string, edits []promptEdit) (string, error) {
	for index, edit := range edits {
		if edit.Find == "" {
			return "", fmt.Errorf("Edit %d has an empty find; nothing was proposed.", index+1)
		}
		switch count := strings.Count(prompt, edit.Find); count {
		case 1:
			prompt = strings.Replace(prompt, edit.Find, edit.Replace, 1)
		case 0:
			return "", fmt.Errorf("Edit %d: its find text %q does not occur in the instructions as they are after the edits before it; nothing was proposed. Copy the passage exactly from schedule_get.", index+1, clip.Runes(edit.Find, editQuoteRunes))
		default:
			return "", fmt.Errorf("Edit %d: its find text %q occurs %d times in the instructions; nothing was proposed. Include more of the surrounding text so it occurs once.", index+1, clip.Runes(edit.Find, editQuoteRunes), count)
		}
	}
	return prompt, nil
}

func override(field *string, value string) {
	if strings.TrimSpace(value) != "" {
		*field = value
	}
}

func (s *Service) proposeRemoval(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "Scheduling is not available.", IsError: true}, nil
	}
	var arguments removeArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, strings.TrimSpace(arguments.JobID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return tools.Result{Text: "No scheduled job has that id. Use schedule_list to see the user's jobs.", IsError: true}, nil
	}
	canonical, err := json.Marshal(removeArguments{JobID: job.ID})
	if err != nil {
		return tools.Result{}, err
	}
	language := locale.From(ctx)
	summary := deleteSummary.FormatIn(language, quotedIn(language, job.Name), DescribeJobIn(language, job, s.zone))
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolRemove, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "remove", TargetKind: "scheduled job", TargetName: job.Name}}, "")
}

func (s *Service) ActionTools() []string {
	return []string{toolPropose, toolUpdate, toolRemove, toolReminderSet}
}

// ExecuteApproved stores or deletes the confirmed job. The schedule is validated again, because
// the limits and the principal's job count can change between the proposal and the press, and the
// approval's own ID becomes the job ID so a second execution cannot create a second job.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	switch record.Tool {
	case toolPropose:
		var confirmed confirmedJob
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, actionError(ctx, "malformed", jobUnreadable)
		}
		config, err := confirmed.config()
		if err != nil {
			return approval.Outcome{}, err
		}
		timing := Timing{Cron: confirmed.Cron, At: confirmed.At, Zone: confirmed.Zone}
		prepared, err := s.prepareJob(ctx, record.PrincipalID, confirmed.Name, TypeModelTurn, timing, config)
		if err != nil {
			return approval.Outcome{}, actionError(ctx, "invalid", scheduleInvalidNotScheduled, err.Error())
		}
		prepared.Job.TravelPause = confirmed.TravelPause != nil && *confirmed.TravelPause && !prepared.Job.OneTime()
		if err := s.confirmedProject(ctx, record.PrincipalID, confirmed.ProjectID, &prepared, "scheduled", nothingScheduledWord); err != nil {
			return approval.Outcome{}, err
		}
		if _, err := s.Create(ctx, record.ID, prepared); err != nil {
			if errors.Is(err, ErrTooManyJobs) {
				return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: tooManyText(prepared.Job.OneTime()) + " Nothing was scheduled.", UserText: tooManyUserText(ctx, prepared.Job.OneTime()) + " " + nothingScheduled.Format(ctx)}
			}
			return approval.Outcome{}, err
		}
		undo, err := s.undoCreate(ctx, record.PrincipalID, record.ID)
		if err != nil {
			return approval.Outcome{}, err
		}
		language := locale.From(ctx)
		at, userAt := FormatOccurrence(prepared.Job.NextRunAt, prepared.Schedule.Location), FormatOccurrenceIn(language, prepared.Job.NextRunAt, prepared.Schedule.Location)
		switch {
		case prepared.Anchor != nil:
			return approval.Outcome{Text: withNote("Scheduled "+reminderWhen(prepared)+".", prepared), UserText: withNote(scheduledAnchored.FormatIn(language, reminderWhenIn(language, prepared)), prepared), Undo: undo}, nil
		case prepared.Job.OneTime():
			return approval.Outcome{Text: "Scheduled for " + at + ".", UserText: scheduledOnce.FormatIn(language, userAt), Undo: undo}, nil
		}
		return approval.Outcome{Text: "Scheduled. First run: " + at + ".", UserText: scheduledRecurring.FormatIn(language, userAt), Undo: undo}, nil
	case toolReminderSet:
		var confirmed confirmedReminder
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, actionError(ctx, "malformed", reminderUnreadable)
		}
		text, err := ReminderConfig(confirmed.Text)
		if err != nil {
			return approval.Outcome{}, err
		}
		config, err := JoinConfig(text, confirmed.Anchor, confirmed.Condition)
		if err != nil {
			return approval.Outcome{}, err
		}
		prepared, err := s.prepareJob(ctx, record.PrincipalID, ReminderName(confirmed.Text), TypeReminder, Timing{At: confirmed.At, Zone: confirmed.Zone}, config)
		if err != nil {
			return approval.Outcome{}, actionError(ctx, "invalid", reminderInvalid, err.Error())
		}
		if err := s.confirmedProject(ctx, record.PrincipalID, confirmed.ProjectID, &prepared, "set", nothingSetWord); err != nil {
			return approval.Outcome{}, err
		}
		if _, err := s.Create(ctx, record.ID, prepared); err != nil {
			if errors.Is(err, ErrTooManyJobs) {
				return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: tooManyText(true) + " Nothing was set.", UserText: tooManyUserText(ctx, true) + " " + nothingSet.Format(ctx)}
			}
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: withNote("Reminder set "+reminderWhen(prepared)+".", prepared), UserText: withNote(reminderSetOutcome.Format(ctx, reminderWhenIn(locale.From(ctx), prepared)), prepared)}, nil
	case toolUpdate:
		return s.executeUpdate(ctx, record)
	case toolRemove:
		var confirmed removeArguments
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, actionError(ctx, "malformed", deletionUnreadable)
		}
		// The whole job is read before it goes, so an Undo can put it back.
		job, found, err := s.repository.LoadScheduledJob(ctx, record.PrincipalID, confirmed.JobID)
		if err != nil {
			return approval.Outcome{}, err
		}
		deleted := false
		if found {
			if deleted, err = s.Delete(ctx, record.PrincipalID, confirmed.JobID); err != nil {
				return approval.Outcome{}, err
			}
		}
		if !deleted {
			return approval.Outcome{}, actionError(ctx, "not_found", jobGone)
		}
		undo, err := json.Marshal(jobUndo{JobID: job.ID, Removed: &job})
		if err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: "Deleted.", UserText: deletedOutcome.Format(ctx), Undo: undo}, nil
	default:
		return approval.Outcome{}, actionError(ctx, "unavailable", actionUnavailable)
	}
}

// confirmedProject checks again the project a confirmed job goes to and routes the prepared job
// there; "" leaves it as it is. what says what was not done, such as scheduled.
func (s *Service) confirmedProject(ctx context.Context, principalID, projectID string, prepared *Prepared, what string, done locale.Text) error {
	if projectID == "" {
		return nil
	}
	id, err := s.projectDestination(ctx, principalID, projectID)
	if err != nil {
		return &approval.ActionError{Reason: "invalid", Text: "That project can no longer take jobs (" + err.Error() + "); nothing was " + what + ".", UserText: projectInvalid.Format(ctx, err.Error(), done.Format(ctx))}
	}
	prepared.Job.Destination = thread.ProjectDestination(id)
	return nil
}

func (s *Service) executeUpdate(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedUpdate
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, actionError(ctx, "malformed", changeUnreadable)
	}
	job, found, err := s.repository.LoadScheduledJob(ctx, record.PrincipalID, confirmed.JobID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found || !job.FinishedAt.IsZero() || job.Type != TypeModelTurn {
		return approval.Outcome{}, actionError(ctx, "not_found", jobGoneNotChanged)
	}
	stale := actionError(ctx, "stale", jobStale)
	if !job.UpdatedAt.Equal(confirmed.UpdatedAt) {
		return approval.Outcome{}, stale
	}
	config, err := confirmed.config()
	if err != nil {
		return approval.Outcome{}, err
	}
	timing := Timing{Cron: confirmed.Cron, At: confirmed.At, Zone: confirmed.Zone}
	prepared, err := s.prepareJob(ctx, record.PrincipalID, confirmed.Name, TypeModelTurn, timing, config)
	if err != nil {
		return approval.Outcome{}, actionError(ctx, "invalid", scheduleInvalidNotChanged, err.Error())
	}
	prepared.Job.TravelPause = job.TravelPause
	if confirmed.TravelPause != nil {
		prepared.Job.TravelPause = *confirmed.TravelPause && TravelPausable(job)
	}
	if current, _ := thread.ProjectOf(job.Destination); confirmed.ProjectID != current {
		if job.Destination != "" && current == "" {
			return approval.Outcome{}, actionError(ctx, "invalid", tripJobNotMovable)
		}
		if err := s.confirmedProject(ctx, record.PrincipalID, confirmed.ProjectID, &prepared, "changed", nothingChangedWord); err != nil {
			return approval.Outcome{}, err
		}
		// Taken off its project, the job goes to the feed of its kind again.
		prepared.exactDestination = confirmed.ProjectID == ""
	}
	updated, err := s.Update(ctx, job, confirmed.UpdatedAt, prepared)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, stale
	}
	undo, err := s.undoUpdate(ctx, job, prepared.Job.TravelPause != job.TravelPause)
	if err != nil {
		return approval.Outcome{}, err
	}
	language := locale.From(ctx)
	if !job.Enabled {
		return approval.Outcome{Text: "Changed. It stays paused until you resume it in /jobs.", UserText: changedPaused.FormatIn(language), Undo: undo}, nil
	}
	at, userAt := FormatOccurrence(prepared.Job.NextRunAt, prepared.Schedule.Location), FormatOccurrenceIn(language, prepared.Job.NextRunAt, prepared.Schedule.Location)
	switch {
	case prepared.Anchor != nil:
		return approval.Outcome{Text: withNote("Changed. Runs "+reminderWhen(prepared)+".", prepared), UserText: withNote(changedAnchored.FormatIn(language, reminderWhenIn(language, prepared)), prepared), Undo: undo}, nil
	case prepared.Job.OneTime():
		return approval.Outcome{Text: "Changed. Runs once: " + at + ".", UserText: changedOnce.FormatIn(language, userAt), Undo: undo}, nil
	}
	return approval.Outcome{Text: "Changed. Next run: " + at + ".", UserText: changedRecurring.FormatIn(language, userAt), Undo: undo}, nil
}

// Previewable reports whether a proposal runs instructions that are new or changed: a proposed job,
// or a change to a job's prompt.
func (s *Service) Previewable(ctx context.Context, record approval.Record) bool {
	switch record.Tool {
	case toolPropose:
		return true
	case toolUpdate:
		var confirmed confirmedUpdate
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return false
		}
		job, found, err := s.repository.LoadScheduledJob(ctx, record.PrincipalID, confirmed.JobID)
		return err == nil && found && ModelTurnPrompt(job.Config) != confirmed.Prompt
	default:
		return false
	}
}

// Preview runs a proposed job's instructions once, now, through the same handler as a scheduled
// run. Nothing is stored: no job and no run record, and a card the turn staged is dropped.
func (s *Service) Preview(ctx context.Context, record approval.Record) (approval.Preview, error) {
	var confirmed confirmedJob
	jobID := ""
	switch record.Tool {
	case toolPropose:
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Preview{}, err
		}
	case toolUpdate:
		var update confirmedUpdate
		if err := json.Unmarshal(record.Arguments, &update); err != nil {
			return approval.Preview{}, err
		}
		// The job's ID makes a preview of the Heads-up job run with its method and default model.
		confirmed, jobID = update.confirmedJob, update.JobID
	default:
		return approval.Preview{}, fmt.Errorf("preview %s: not a model turn", record.Tool)
	}
	jobType, known := s.types[TypeModelTurn]
	if !known {
		return approval.Preview{}, fmt.Errorf("preview: the model turn type is not registered")
	}
	config, err := ModelTurnConfigWithModel(confirmed.Prompt, confirmed.Model)
	if err != nil {
		return approval.Preview{}, err
	}
	location, err := LoadZone(confirmed.Zone)
	if err != nil {
		location, _ = LoadZone(s.zone)
	}
	runID := "preview-" + record.ID
	result, err := jobType.Handler.Run(ctx, Occurrence{
		RunID:        runID,
		JobID:        jobID,
		PrincipalID:  record.PrincipalID,
		JobName:      confirmed.Name,
		Config:       config,
		ScheduledFor: s.now(),
		Location:     location,
	})
	if s.cards != nil {
		s.cards.Remove(record.PrincipalID, outbox.ScheduledTurn(runID))
	}
	if err != nil {
		return approval.Preview{}, err
	}
	text := result.Text
	if strings.TrimSpace(text) == "" {
		text = FailedText
	}
	return approval.Preview{Name: confirmed.Name, Text: text}, nil
}

const (
	oneTimeTravelPauseText = "Only recurring jobs can pause while traveling; a one-time job or reminder is never paused, so leave pause_while_traveling out."
	unpausableTravelText   = "This job cannot pause while traveling: only the user's own recurring jobs can, not one-time jobs, reminders, or built-in ones such as Heads-up. Leave pause_while_traveling out."
)

// What a change too long to show the user in full for confirmation is told, by how it was made.
const (
	longNewJobText  = "The instructions are too long to show the user in full for confirmation; nothing was proposed. Keep them under about 2000 characters, and once the job exists, after the user confirms it if it is only proposed, extend them with prompt_edits."
	longRewriteText = "The new instructions are too long to show the user in full for confirmation; nothing was changed. Keep the rewrite under about 2000 characters, and once it is made, after the user confirms it if it is only proposed, extend it with prompt_edits."
	longEditsText   = "The edits together are too long to show the user in full for confirmation; nothing was changed. Make fewer or shorter edits in this call, and the rest once this change is made, after the user confirms it if it is only proposed."
)

// TravelPausable reports whether a job may be flagged to pause while its owner travels.
func TravelPausable(job Job) bool {
	return job.Type == TypeModelTurn && !job.OneTime() &&
		job.ID != HeadsUpJobID(job.PrincipalID) && job.ID != TripsJobID(job.PrincipalID)
}

func tooManyText(oneTime bool) string {
	if oneTime {
		return fmt.Sprintf("You already have %d pending reminders and one-time jobs, which is the most allowed; delete one in /jobs first.", MaximumOneTimeJobsPerPrincipal)
	}
	return fmt.Sprintf("You already have %d recurring jobs, which is the most allowed; delete one in /jobs first.", MaximumJobsPerPrincipal)
}

func tooManyUserText(ctx context.Context, oneTime bool) string {
	if oneTime {
		return tooManyOneTime.Format(ctx, MaximumOneTimeJobsPerPrincipal)
	}
	return tooManyRecurring.Format(ctx, MaximumJobsPerPrincipal)
}

func countKind(jobs []Job, oneTime bool) int {
	count := 0
	for _, job := range jobs {
		if job.OneTime() == oneTime {
			count++
		}
	}
	return count
}

// proposalSummary is the confirmation the user reads. It is rendered from the parsed schedule, not
// from the model's description of it, so the next run times are the ones that will happen.
func proposalSummary(language locale.Language, heading string, prepared Prepared) string {
	return scheduleSummary(language, heading, prepared) + ":\n" + ModelTurnPrompt(prepared.Job.Config)
}

// patchSummary is the confirmation of a change made by prompt edits: the schedule as it will be and
// only the passages that change, so the user reads the change instead of the whole prompt again.
func patchSummary(language locale.Language, heading string, prepared Prepared, edits []promptEdit) string {
	var builder strings.Builder
	builder.WriteString(instructionsChangeSummary.FormatIn(language, scheduleSummary(language, heading, prepared)))
	for index, edit := range edits {
		replacement := edit.Replace
		if strings.TrimSpace(replacement) == "" {
			replacement = editRemoved.In(language)
		}
		if len(edits) > 1 {
			fmt.Fprintf(&builder, "\n%d.", index+1)
		}
		builder.WriteString("\n" + editWasNow.FormatIn(language, edit.Find, replacement))
	}
	return builder.String()
}

// scheduleSummary is the heading and the parsed schedule of a proposal, ending with the sentence
// that introduces its instructions.
func scheduleSummary(language locale.Language, heading string, prepared Prepared) string {
	job := prepared.Job
	var builder strings.Builder
	builder.WriteString(heading + "\n")
	model := ModelTurnModel(job.Config)
	line := func(text string) { builder.WriteString(text + "\n") }
	if prepared.Anchor != nil {
		if prepared.Anchor.Repeats() {
			line(summaryRuns.FormatIn(language, prepared.Anchor.DescribeIn(language)))
			line(summaryNext.FormatIn(language, NextRunTextIn(language, job, job.Zone)))
		} else {
			line(summaryRunsOnceAnchored.FormatIn(language, FormatOccurrenceIn(language, job.RunAt, prepared.Schedule.Location), prepared.Anchor.DescribeIn(language)))
		}
		writeCondition(&builder, language, job)
		if model != "" {
			line(summaryModel.FormatIn(language, model))
		}
		builder.WriteString(summaryEachRun.In(language))
		return builder.String()
	}
	if job.OneTime() {
		line(summaryRunsOnce.FormatIn(language, FormatOccurrenceIn(language, job.RunAt, prepared.Schedule.Location)))
		writeCondition(&builder, language, job)
		if model != "" {
			line(summaryModel.FormatIn(language, model))
		}
		builder.WriteString(summaryOnceRun.In(language))
		return builder.String()
	}
	upcoming := prepared.Schedule.Upcoming(job.CreatedAt, upcomingShown)
	times := make([]string, 0, len(upcoming))
	for _, at := range upcoming {
		times = append(times, FormatOccurrenceIn(language, at, prepared.Schedule.Location))
	}
	line(summaryRunsCron.FormatIn(language, job.Cron, job.Zone))
	line(summaryNext.FormatIn(language, strings.Join(times, "; ")))
	if job.TravelPause {
		line(summaryTravel.In(language))
	}
	writeCondition(&builder, language, job)
	if model != "" {
		line(summaryModel.FormatIn(language, model))
	}
	builder.WriteString(summaryEachRun.In(language))
	return builder.String()
}

// writeCondition adds the job's condition to a confirmation.
func writeCondition(builder *strings.Builder, language locale.Language, job Job) {
	if condition := JobCondition(job); condition != nil {
		builder.WriteString(summaryCondition.FormatIn(language, condition.Description) + "\n")
	}
}

func (s *Service) renderJobs(ctx context.Context, jobs []Job) string {
	if len(jobs) == 0 {
		return "The user has no pending reminders or scheduled jobs."
	}
	rendered := make([]string, 0, len(jobs))
	for _, job := range jobs {
		rendered = append(rendered, renderJob(job, s.zone, listedInstructionRunes, s.describeModel(ctx, job, false)))
	}
	return strings.Join(rendered, "\n\n")
}

// renderJob is one job as the model reads it, with its instructions cut to limit runes and the
// model a model.turn job runs on.
func renderJob(job Job, zone string, limit int, model string) string {
	if job.Type == TypeReminder {
		return fmt.Sprintf("id %s: reminder, %s\nText: %s", job.ID, DescribeJob(job, zone), clip.Runes(ReminderText(job.Config), limit))
	}
	state := DescribeJob(job, zone)
	if !job.OneTime() {
		state += ", next run " + NextRunText(job, zone)
	}
	if !job.Enabled {
		state += ", paused"
	}
	if job.TravelPause {
		state += ", pauses while traveling"
	}
	label := "Instructions"
	if job.ID == HeadsUpJobID(job.PrincipalID) {
		label = headsUpNotesLabel
	}
	return fmt.Sprintf("id %s: %q, %s\nModel: %s\n%s: %s", job.ID, job.Name, state, model, label, clip.Runes(ModelTurnPrompt(job.Config), limit))
}

// DescribeJob is a job's schedule as the user reads it, with the event it follows and its
// condition, falling back to the default zone for a zone the binary no longer knows.
func DescribeJob(job Job, zone string) string { return DescribeJobIn(locale.English, job, zone) }

// DescribeJobIn is DescribeJob in a language.
func DescribeJobIn(language locale.Language, job Job, zone string) string {
	location, err := LoadZone(job.Zone)
	if err != nil {
		location, _ = LoadZone(zone)
	}
	var described string
	switch anchor := JobAnchor(job); {
	case anchor != nil && job.OneTime():
		described = describeOnceAnchored.FormatIn(language, FormatOccurrenceIn(language, job.RunAt, location), anchor.DescribeIn(language))
	case anchor != nil:
		described = anchor.DescribeIn(language)
	default:
		schedule, err := JobSchedule(job)
		if err != nil {
			schedule = Schedule{Expression: job.Cron, Once: job.RunAt, Location: location}
		}
		if schedule.Once.IsZero() {
			described = schedule.Describe()
		} else {
			described = describeOnce.FormatIn(language, FormatOccurrenceIn(language, schedule.Once, schedule.Location))
		}
	}
	if condition := JobCondition(job); condition != nil {
		described = onlyIf.FormatIn(language, described, condition.Description)
	}
	return described
}

// FormatOccurrence renders an occurrence in the job's own zone, as the user reads it.
func FormatOccurrence(at time.Time, location *time.Location) string {
	return FormatOccurrenceIn(locale.English, at, location)
}

// FormatOccurrenceIn is FormatOccurrence in a language.
func FormatOccurrenceIn(language locale.Language, at time.Time, location *time.Location) string {
	return locale.Date(language, at.In(location), locale.DayTimeZone)
}

// offeredModel checks a model chat asked for, "id:effort", against the principal's catalog as
// /model checks a choice, and returns it with the level the model runs it as. An empty request is
// the default and returns "". A model the catalog does not offer comes back as a refusal for the
// model to read, naming the models that are.
func (s *Service) offeredModel(ctx context.Context, principalID, requested string) (string, string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", "", nil
	}
	var models []modelcatalog.Model
	var err error
	if s.models.catalog != nil {
		models, err = s.models.catalog.Models(ctx, principalID)
	}
	switch {
	case s.models.catalog == nil || errors.Is(err, modelcatalog.ErrUnavailable):
		models = modelcatalog.Fallback(s.models.chat, s.models.task, s.models.headsUp)
	case err != nil:
		return "", "", fmt.Errorf("read model catalog: %w", err)
	}
	choice, err := agentruntime.ParseModelEffort(requested)
	if err == nil && modelcatalog.ValidChoice(choice) {
		if canonical, ok := modelcatalog.Offered(models, choice); ok {
			choice.Effort = canonical
			return choice.String(), "", nil
		}
	}
	offered := make([]string, 0, len(models))
	for _, model := range models {
		offered = append(offered, model.ID+" ("+strings.Join(model.Efforts, ", ")+")")
	}
	return "", fmt.Sprintf("The model %q is not available to the user; nothing was proposed. Write it as id:effort with one of these models and efforts: %s.", requested, strings.Join(offered, "; ")), nil
}

// describeModel names the model a model.turn job runs on and where that choice comes from. With
// check, a choice the principal's subscription no longer offers is named with the chat default
// it falls back to.
func (s *Service) describeModel(ctx context.Context, job Job, check bool) string {
	if job.Type != TypeModelTurn {
		return ""
	}
	want, source := s.jobModel(job.PrincipalID, job.ID, ModelTurnModel(job.Config))
	name := want.String()
	if want.Model == "" {
		name = "the runtime's default"
	}
	described := fmt.Sprintf("%s (%s)", name, source)
	if !check || source == modelSourceChat {
		return described
	}
	if model, fallback := s.effectiveModel(ctx, job.PrincipalID, want); fallback != "" {
		described += fmt.Sprintf(", which the user's subscription does not offer now, so it runs on the chat default %s", model)
	}
	return described
}

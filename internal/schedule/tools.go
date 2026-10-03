package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolList           = "schedule_list"
	toolPropose        = "schedule_propose"
	toolRemove         = "schedule_remove"
	toolReminderSet    = "reminder_set"
	toolReminderCancel = "reminder_cancel"

	upcomingShown = 3
	// listedInstructionRunes keeps a long list of jobs small in the model's context.
	listedInstructionRunes = 200
)

// Proposer is the core approval service as the scheduler sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

const listGuidance = "Scheduled jobs: schedule_list shows the user's pending reminders and scheduled jobs, each with its id, schedule, next run, and instructions. The user manages them with /jobs as well."

func proposeGuidance(zone string) string {
	return "The user can ask for something to happen on a schedule, such as every Monday at 9 checking their Notion tasks and reporting what is overdue, or once, such as tomorrow at 7:30 checking whether their train is delayed. " +
		"schedule_propose only proposes the job: after your reply the user sees its exact schedule, next runs, and instructions with Confirm and Reject buttons, so never claim it was scheduled. " +
		"A recurring job takes cron, a standard five-field cron expression (minute hour day-of-month month day-of-week) in the user's time zone, " + zone + " unless they name another; " +
		"for example 0 9 * * 1 is Mondays at 09:00 and 30 7 * * 1-5 is weekdays at 07:30. A recurring job may run at most once an hour. " +
		"A one-time job takes at instead, a local date and time such as 2026-10-04 07:30 that you work out from the current time. " +
		"Each run is a separate turn the user does not watch, with only read-only tools, so the prompt must say exactly what to look at and what to report, on its own, without relying on this conversation. " +
		"schedule_remove proposes deleting a job by its id from schedule_list. Only one proposal can wait at a time."
}

func reminderGuidance(zone string) string {
	return "Reminders: when the user only wants to be told something at a time, such as remind me at 5 to call mom or in 20 minutes tell me to take the laundry out, use reminder_set, not schedule_propose. " +
		"It sends the text as written at that time, without asking a model, and is set at once without confirmation. " +
		"at is a local date and time such as 2026-10-04 17:00 in the user's time zone, " + zone + " unless they name another; work it out from the current time. " +
		"Write the text as the reminder the user should read, such as Call mom. Repeat the time reminder_set returns to the user exactly as it gives it. " +
		"reminder_cancel cancels a pending reminder by its id from schedule_list, also without confirmation. A reminder that should repeat is a recurring schedule_propose job instead."
}

var listDefinition = tools.Definition{
	Name:        toolList,
	Description: "List the user's scheduled jobs with their ids, schedules, next runs, and instructions.",
	Risk:        tools.RiskRead,
	InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
}

var reminderDefinitions = []tools.Definition{
	{
		Name:        toolReminderSet,
		Description: "Set a reminder: the text is sent to the user once, at the given local time, without confirmation.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"text":{"type":"string","description":"The reminder as the user should read it, at most 500 characters."},` +
			`"at":{"type":"string","description":"Local date and time, YYYY-MM-DD HH:MM, such as 2026-10-04 17:00."},` +
			`"timezone":{"type":"string","description":"IANA time zone such as Europe/Amsterdam; omit to use the user's own."}},` +
			`"required":["text","at"],"additionalProperties":false}`),
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
		Description: "Propose a job that runs a prompt on a cron schedule, or once at a local time, with read-only tools and sends the result to the user. The user must confirm.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"A short name the user will recognise, such as Overdue Notion tasks."},` +
			`"cron":{"type":"string","description":"For a recurring job: a five-field cron expression, such as 0 9 * * 1 for Mondays at 09:00."},` +
			`"at":{"type":"string","description":"For a one-time job instead of cron: local date and time, YYYY-MM-DD HH:MM."},` +
			`"prompt":{"type":"string","description":"The complete instructions for each run, at most 1000 characters."},` +
			`"timezone":{"type":"string","description":"IANA time zone such as Europe/Amsterdam; omit to use the user's own."}},` +
			`"required":["name","prompt"],"additionalProperties":false}`),
	},
	{
		Name:        toolRemove,
		Description: "Propose deleting one of the user's scheduled jobs. The user must confirm.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"job_id":{"type":"string","description":"The id from schedule_list."}},` +
			`"required":["job_id"],"additionalProperties":false}`),
	},
}

var _ tools.Provider = (*Service)(nil)
var _ approval.Executor = (*Service)(nil)

// Contribute offers the list tool, the reminder tools when the reminder type is registered, and
// the proposal tools when there is an approval service and the model.turn type is registered. A
// scheduled run itself receives only the list tool, because the broker offers it read tools alone.
func (s *Service) Contribute(context.Context, string) (tools.Contribution, error) {
	contribution := tools.Contribution{Guidance: listGuidance, Definitions: []tools.Definition{listDefinition}}
	var guidance []string
	if _, known := s.types[TypeReminder]; known {
		guidance = append(guidance, reminderGuidance(s.zone))
		contribution.Definitions = append(contribution.Definitions, reminderDefinitions...)
	}
	if _, known := s.types[TypeModelTurn]; known && s.proposer != nil {
		guidance = append(guidance, proposeGuidance(s.zone))
		contribution.Definitions = append(contribution.Definitions, writeDefinitions...)
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
		return tools.Result{Text: renderJobs(jobs, s.zone)}, nil
	case toolPropose:
		return s.proposeJob(ctx, principalID, call)
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
	Name     string `json:"name"`
	Cron     string `json:"cron"`
	At       string `json:"at"`
	Prompt   string `json:"prompt"`
	Timezone string `json:"timezone"`
}

// confirmedJob is the canonical form the user confirms and the executor stores. At is the local
// time as parsed, so it is validated again when the user presses Confirm.
type confirmedJob struct {
	Name   string `json:"name"`
	Cron   string `json:"cron,omitempty"`
	At     string `json:"at,omitempty"`
	Zone   string `json:"zone"`
	Prompt string `json:"prompt"`
}

type removeArguments struct {
	JobID string `json:"job_id"`
}

type reminderArguments struct {
	Text     string `json:"text"`
	At       string `json:"at"`
	Timezone string `json:"timezone"`
}

func (s *Service) setReminder(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments reminderArguments
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	config, err := ReminderConfig(arguments.Text)
	if err != nil {
		return tools.Result{}, err
	}
	timing := Timing{At: arguments.At, Zone: arguments.Timezone}
	prepared, err := s.Prepare(principalID, ReminderName(arguments.Text), TypeReminder, timing, config)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	id, err := newRunID()
	if err != nil {
		return tools.Result{}, err
	}
	if _, err := s.Create(ctx, id, prepared); err != nil {
		if errors.Is(err, ErrTooManyJobs) {
			return tools.Result{Text: tooManyText(true), IsError: true}, nil
		}
		return tools.Result{}, err
	}
	at := FormatOccurrence(prepared.Job.RunAt, prepared.Schedule.Location)
	return tools.Result{Text: fmt.Sprintf("Reminder set for %s (id %s): %s", at, id, ReminderText(config))}, nil
}

func (s *Service) cancelReminder(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments removeArguments
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
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
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	config, err := ModelTurnConfig(arguments.Prompt)
	if err != nil {
		return tools.Result{}, err
	}
	timing := Timing{Cron: arguments.Cron, At: arguments.At, Zone: arguments.Timezone}
	prepared, err := s.Prepare(principalID, arguments.Name, TypeModelTurn, timing, config)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	job := prepared.Job
	existing, err := s.repository.ListScheduledJobs(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if countKind(existing, job.OneTime()) >= job.Limit() {
		return tools.Result{Text: tooManyText(job.OneTime()), IsError: true}, nil
	}

	confirmed := confirmedJob{Name: job.Name, Cron: job.Cron, Zone: job.Zone, Prompt: ModelTurnPrompt(job.Config)}
	targetKind := "recurring job"
	if job.OneTime() {
		confirmed.At = job.RunAt.In(prepared.Schedule.Location).Format(localTimeLayouts[0])
		targetKind = "one-time job"
	}
	canonical, err := json.Marshal(confirmed)
	if err != nil {
		return tools.Result{}, err
	}
	summary := proposalSummary(prepared)
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: toolPropose, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "schedule", TargetKind: targetKind, TargetName: job.Name}}); err != nil {
		s.logger.WarnContext(ctx, "schedule.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return tools.Result{Text: "the proposal could not be recorded", IsError: true}, nil
	}
	return tools.Result{Text: approval.ProposedText}, nil
}

func (s *Service) proposeRemoval(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "Scheduling is not available.", IsError: true}, nil
	}
	var arguments removeArguments
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
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
	summary := fmt.Sprintf("Delete the scheduled job %q (%s). It will not run again.", job.Name, DescribeJob(job, s.zone))
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: toolRemove, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "remove", TargetKind: "scheduled job", TargetName: job.Name}}); err != nil {
		s.logger.WarnContext(ctx, "schedule.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return tools.Result{Text: "the proposal could not be recorded", IsError: true}, nil
	}
	return tools.Result{Text: approval.ProposedText}, nil
}

func (s *Service) ActionTools() []string {
	return []string{toolPropose, toolRemove}
}

// ExecuteApproved stores or deletes the confirmed job. The schedule is validated again, because
// the limits and the principal's job count can change between the proposal and the press, and the
// approval's own ID becomes the job ID so a second execution cannot create a second job.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	switch record.Tool {
	case toolPropose:
		var confirmed confirmedJob
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed job could not be read; nothing was scheduled."}
		}
		config, err := ModelTurnConfig(confirmed.Prompt)
		if err != nil {
			return approval.Outcome{}, err
		}
		timing := Timing{Cron: confirmed.Cron, At: confirmed.At, Zone: confirmed.Zone}
		prepared, err := s.Prepare(record.PrincipalID, confirmed.Name, TypeModelTurn, timing, config)
		if err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That schedule is no longer valid (" + err.Error() + "); nothing was scheduled."}
		}
		if _, err := s.Create(ctx, record.ID, prepared); err != nil {
			if errors.Is(err, ErrTooManyJobs) {
				return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: tooManyText(prepared.Job.OneTime()) + " Nothing was scheduled."}
			}
			return approval.Outcome{}, err
		}
		at := FormatOccurrence(prepared.Job.NextRunAt, prepared.Schedule.Location)
		if prepared.Job.OneTime() {
			return approval.Outcome{Text: "Scheduled for " + at + "."}, nil
		}
		return approval.Outcome{Text: "Scheduled. First run: " + at + "."}, nil
	case toolRemove:
		var confirmed removeArguments
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed action could not be read; nothing was deleted."}
		}
		deleted, err := s.Delete(ctx, record.PrincipalID, confirmed.JobID)
		if err != nil {
			return approval.Outcome{}, err
		}
		if !deleted {
			return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That job no longer exists."}
		}
		return approval.Outcome{Text: "Deleted."}, nil
	default:
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That action is no longer available."}
	}
}

func tooManyText(oneTime bool) string {
	if oneTime {
		return fmt.Sprintf("You already have %d pending reminders and one-time jobs, which is the most allowed; delete one in /jobs first.", MaximumOneTimeJobsPerPrincipal)
	}
	return fmt.Sprintf("You already have %d recurring jobs, which is the most allowed; delete one in /jobs first.", MaximumJobsPerPrincipal)
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
func proposalSummary(prepared Prepared) string {
	job := prepared.Job
	var builder strings.Builder
	fmt.Fprintf(&builder, "Schedule %q\n", job.Name)
	if job.OneTime() {
		fmt.Fprintf(&builder, "Runs once: %s\n", FormatOccurrence(job.RunAt, prepared.Schedule.Location))
		builder.WriteString("It asks your model, with read-only tools, and sends you the reply:\n")
		builder.WriteString(ModelTurnPrompt(job.Config))
		return builder.String()
	}
	upcoming := prepared.Schedule.Upcoming(job.CreatedAt, upcomingShown)
	times := make([]string, 0, len(upcoming))
	for _, at := range upcoming {
		times = append(times, FormatOccurrence(at, prepared.Schedule.Location))
	}
	fmt.Fprintf(&builder, "Runs: %s (%s)\n", job.Cron, job.Zone)
	fmt.Fprintf(&builder, "Next: %s\n", strings.Join(times, "; "))
	builder.WriteString("Each run asks your model, with read-only tools, and sends you the reply:\n")
	builder.WriteString(ModelTurnPrompt(job.Config))
	return builder.String()
}

func renderJobs(jobs []Job, zone string) string {
	if len(jobs) == 0 {
		return "The user has no pending reminders or scheduled jobs."
	}
	var builder strings.Builder
	for index, job := range jobs {
		if index > 0 {
			builder.WriteString("\n\n")
		}
		if job.Type == TypeReminder {
			fmt.Fprintf(&builder, "id %s: reminder, %s\nText: %s", job.ID, DescribeJob(job, zone), truncateRunes(ReminderText(job.Config), listedInstructionRunes))
			continue
		}
		state := DescribeJob(job, zone)
		if !job.OneTime() {
			location, err := LoadZone(job.Zone)
			if err != nil {
				location, _ = LoadZone(zone)
			}
			state += ", next run " + FormatOccurrence(job.NextRunAt, location)
		}
		if !job.Enabled {
			state += ", paused"
		}
		fmt.Fprintf(&builder, "id %s: %q, %s\nInstructions: %s", job.ID, job.Name, state, truncateRunes(ModelTurnPrompt(job.Config), listedInstructionRunes))
	}
	return builder.String()
}

// DescribeJob is a job's schedule as the user reads it, falling back to the default zone for a
// zone the binary no longer knows.
func DescribeJob(job Job, zone string) string {
	schedule, err := JobSchedule(job)
	if err != nil {
		location, _ := LoadZone(zone)
		schedule = Schedule{Expression: job.Cron, Once: job.RunAt, Location: location}
	}
	return schedule.Describe()
}

// FormatOccurrence renders an occurrence in the job's own zone, as the user reads it.
func FormatOccurrence(at time.Time, location *time.Location) string {
	return at.In(location).Format("Mon 2 Jan 15:04 MST")
}

func decodeArguments(raw json.RawMessage, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("the arguments do not match the tool's schema")
	}
	return nil
}

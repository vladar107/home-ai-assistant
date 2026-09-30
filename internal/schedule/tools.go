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
	toolList    = "schedule_list"
	toolPropose = "schedule_propose"
	toolRemove  = "schedule_remove"

	upcomingShown = 3
)

// Proposer is the core approval service as the scheduler sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

const listGuidance = "Scheduled jobs: schedule_list shows the user's scheduled jobs, each with its id, schedule, next run, and instructions. The user manages them with /jobs as well."

func proposeGuidance(zone string) string {
	return "The user can ask for something to happen on a schedule, such as every Monday at 9 checking their Notion tasks and reporting what is overdue. " +
		"schedule_propose only proposes the job: after your reply the user sees its exact schedule, next runs, and instructions with Confirm and Reject buttons, so never claim it was scheduled. " +
		"The schedule is a standard five-field cron expression (minute hour day-of-month month day-of-week) in the user's time zone, " + zone + " unless they name another; " +
		"for example 0 9 * * 1 is Mondays at 09:00 and 30 7 * * 1-5 is weekdays at 07:30. A job may run at most once an hour. " +
		"Each run is a separate turn the user does not watch, with only read-only tools, so the prompt must say exactly what to look at and what to report, on its own, without relying on this conversation. " +
		"schedule_remove proposes deleting a job by its id from schedule_list. Only one proposal can wait at a time."
}

var listDefinition = tools.Definition{
	Name:        toolList,
	Description: "List the user's scheduled jobs with their ids, schedules, next runs, and instructions.",
	Risk:        tools.RiskRead,
	InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
}

var writeDefinitions = []tools.Definition{
	{
		Name:        toolPropose,
		Description: "Propose a job that runs a prompt on a cron schedule with read-only tools and sends the result to the user. The user must confirm.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"A short name the user will recognise, such as Overdue Notion tasks."},` +
			`"cron":{"type":"string","description":"Five-field cron expression, such as 0 9 * * 1 for Mondays at 09:00."},` +
			`"prompt":{"type":"string","description":"The complete instructions for each run, at most 1000 characters."},` +
			`"timezone":{"type":"string","description":"IANA time zone such as Europe/Amsterdam; omit to use the user's own."}},` +
			`"required":["name","cron","prompt"],"additionalProperties":false}`),
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

// Contribute offers the list tool, and the proposal tools when there is an approval service and
// the model.turn type is registered. A scheduled run itself receives only the list tool, because
// the broker offers it read tools alone.
func (s *Service) Contribute(context.Context, string) (tools.Contribution, error) {
	contribution := tools.Contribution{Guidance: listGuidance, Definitions: []tools.Definition{listDefinition}}
	if _, known := s.types[TypeModelTurn]; known && s.proposer != nil {
		contribution.WriteGuidance = proposeGuidance(s.zone)
		contribution.Definitions = append(contribution.Definitions, writeDefinitions...)
	}
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
	default:
		return tools.Result{Text: "unknown scheduling tool", IsError: true}, nil
	}
}

type proposeArguments struct {
	Name     string `json:"name"`
	Cron     string `json:"cron"`
	Prompt   string `json:"prompt"`
	Timezone string `json:"timezone"`
}

// confirmedJob is the canonical form the user confirms and the executor stores.
type confirmedJob struct {
	Name   string `json:"name"`
	Cron   string `json:"cron"`
	Zone   string `json:"zone"`
	Prompt string `json:"prompt"`
}

type removeArguments struct {
	JobID string `json:"job_id"`
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
	prepared, err := s.Prepare(principalID, arguments.Name, TypeModelTurn, arguments.Cron, arguments.Timezone, config)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	existing, err := s.repository.ListScheduledJobs(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if len(existing) >= MaximumJobsPerPrincipal {
		return tools.Result{Text: tooManyText(), IsError: true}, nil
	}

	job := prepared.Job
	canonical, err := json.Marshal(confirmedJob{Name: job.Name, Cron: job.Cron, Zone: job.Zone, Prompt: ModelTurnPrompt(job.Config)})
	if err != nil {
		return tools.Result{}, err
	}
	summary := proposalSummary(prepared)
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: toolPropose, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "schedule", TargetKind: "recurring job", TargetName: job.Name}}); err != nil {
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
	summary := fmt.Sprintf("Delete the scheduled job %q (%s, %s). It will not run again.", job.Name, job.Cron, job.Zone)
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
		prepared, err := s.Prepare(record.PrincipalID, confirmed.Name, TypeModelTurn, confirmed.Cron, confirmed.Zone, config)
		if err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That schedule is no longer valid; nothing was scheduled."}
		}
		if _, err := s.Create(ctx, record.ID, prepared); err != nil {
			if errors.Is(err, ErrTooManyJobs) {
				return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: tooManyText() + " Nothing was scheduled."}
			}
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: "Scheduled. First run: " + FormatOccurrence(prepared.Job.NextRunAt, prepared.Schedule.Location) + "."}, nil
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

func tooManyText() string {
	return fmt.Sprintf("You already have %d scheduled jobs, which is the most allowed; delete one in /jobs first.", MaximumJobsPerPrincipal)
}

// proposalSummary is the confirmation the user reads. It is rendered from the parsed schedule, not
// from the model's description of it, so the next run times are the ones that will happen.
func proposalSummary(prepared Prepared) string {
	job := prepared.Job
	upcoming := prepared.Schedule.Upcoming(job.CreatedAt, upcomingShown)
	times := make([]string, 0, len(upcoming))
	for _, at := range upcoming {
		times = append(times, FormatOccurrence(at, prepared.Schedule.Location))
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Schedule %q\n", job.Name)
	fmt.Fprintf(&builder, "Runs: %s (%s)\n", job.Cron, job.Zone)
	fmt.Fprintf(&builder, "Next: %s\n", strings.Join(times, "; "))
	builder.WriteString("Each run asks your model, with read-only tools, and sends you the reply:\n")
	builder.WriteString(ModelTurnPrompt(job.Config))
	return builder.String()
}

func renderJobs(jobs []Job, zone string) string {
	if len(jobs) == 0 {
		return "The user has no scheduled jobs."
	}
	var builder strings.Builder
	for index, job := range jobs {
		if index > 0 {
			builder.WriteString("\n\n")
		}
		location, err := LoadZone(job.Zone)
		if err != nil {
			location, _ = LoadZone(zone)
		}
		state := "next run " + FormatOccurrence(job.NextRunAt, location)
		if !job.Enabled {
			state = "paused"
		}
		fmt.Fprintf(&builder, "id %s: %q, %s (%s), %s\nInstructions: %s", job.ID, job.Name, job.Cron, job.Zone, state, ModelTurnPrompt(job.Config))
	}
	return builder.String()
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

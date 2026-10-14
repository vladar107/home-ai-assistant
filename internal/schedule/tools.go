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
	"github.com/vladar107/home-ai-assistant/internal/outbox"
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

const listGuidance = "Scheduled jobs: schedule_list shows the user's pending reminders and scheduled jobs, each with its id, schedule, next run, and instructions, shortening long instructions; schedule_get shows one of them in full. The user manages them with /jobs as well."

func proposeGuidance(zone string) string {
	return "The user can ask for something to happen on a schedule, such as every Monday at 9 checking their Notion tasks and reporting what is overdue, or once, such as tomorrow at 7:30 checking whether their train is delayed. " +
		"When the user asked for it in this conversation and the conversation has read no outside content, schedule_propose, schedule_update, and schedule_remove make the change at once and the user can undo it; tell the user it is done. " +
		"After outside content, such as a web page or an email, they only propose it: the user sees it with Confirm and Reject buttons, and new or changed instructions also get a Preview button that runs them once without saving, so never claim it was made. " +
		"A recurring job takes cron, a standard five-field cron expression (minute hour day-of-month month day-of-week) in the user's time zone, " + zone + " unless they name another; " +
		"for example 0 9 * * 1 is Mondays at 09:00 and 30 7 * * 1-5 is weekdays at 07:30. A recurring job may run at most once an hour. " +
		"A one-time job takes at instead, a local date and time such as 2026-10-04 07:30 that you work out from the current time. " +
		"Each run is a separate turn the user does not watch, with only read-only tools, so the prompt must say exactly what to look at and what to report, on its own, without relying on this conversation. " +
		"schedule_update changes a job by its id from schedule_list, given only the fields that change. " +
		"To change its instructions, first read them with schedule_get, then pass prompt_edits: small find and replace pairs, each find copied exactly from the stored prompt and occurring in it once, that change only what the user asked for; the user sees just those passages, old and new. " +
		"Never rewrite or reword the parts the user did not ask to change, and pass prompt, the complete new instructions, only when the user asks for a real rewrite. " +
		"Never invent a value the user did not give, such as a time, a buffer, a walking time, or a threshold; ask for it instead. " +
		"A recurring job stays recurring and a one-time job stays one-time; to switch, remove it and propose a new one. " +
		"schedule_remove deletes a job by its id from schedule_list. Make every job change the request needs in this turn, once each. Proposals of one turn are shown together and confirmed or rejected as one batch; withdraw a wrong one with proposal_withdraw, and a proposal in a later turn replaces a batch still waiting."
}

func reminderGuidance(zone string) string {
	return "Reminders: when the user only wants to be told something at a time, such as remind me at 5 to call mom or in 20 minutes tell me to take the laundry out, use reminder_set, not schedule_propose. " +
		"It sends the text as written at that time, without asking a model, and is set at once without confirmation, except after you have read content from outside the household, such as a web page or an email, in this conversation: then reminder_set only proposes the reminder, the user sees it with Confirm and Reject buttons, and you must not claim it is set. " +
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

var getDefinition = tools.Definition{
	Name:        toolGet,
	Description: "Show one of the user's scheduled jobs or reminders in full, with its complete instructions.",
	Risk:        tools.RiskRead,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"job_id":{"type":"string","description":"The id from schedule_list."}},` +
		`"required":["job_id"],"additionalProperties":false}`),
}

var reminderDefinitions = []tools.Definition{
	{
		Name:        toolReminderSet,
		Description: "Set a reminder: the text is sent to the user once, at the given local time, without confirmation unless outside content was read in this conversation.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"text":{"type":"string","maxLength":500,"description":"The reminder as the user should read it, at most 500 characters."},` +
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
		Description: "Schedule a job that runs a prompt on a cron schedule, or once at a local time, with read-only tools and sends the result to the user. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","maxLength":60,"description":"A short name the user will recognise, such as Overdue Notion tasks."},` +
			`"cron":{"type":"string","description":"For a recurring job: a five-field cron expression, such as 0 9 * * 1 for Mondays at 09:00."},` +
			`"at":{"type":"string","description":"For a one-time job instead of cron: local date and time, YYYY-MM-DD HH:MM."},` +
			`"prompt":{"type":"string","maxLength":1000,"description":"The complete instructions for each run, at most 1000 characters."},` +
			`"timezone":{"type":"string","description":"IANA time zone such as Europe/Amsterdam; omit to use the user's own."}},` +
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
			`"prompt":{"type":"string","maxLength":1000,"description":"The complete new instructions, replacing the old ones, at most 1000 characters. Only for a real rewrite; not together with prompt_edits."},` +
			`"prompt_edits":{"type":"array","minItems":1,"maxItems":10,"description":"Changes to the current instructions, applied in order. Prefer these to prompt.","items":{"type":"object","properties":{` +
			`"find":{"type":"string","minLength":1,"maxLength":1000,"description":"A passage copied exactly from the current instructions; it must occur there exactly once."},` +
			`"replace":{"type":"string","maxLength":1000,"description":"The text that replaces it; empty to delete the passage."}},` +
			`"required":["find","replace"],"additionalProperties":false}},` +
			`"timezone":{"type":"string","description":"A new IANA time zone such as Europe/Amsterdam."}},` +
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
	Name     string `json:"name"`
	Cron     string `json:"cron"`
	At       string `json:"at"`
	Prompt   string `json:"prompt"`
	Timezone string `json:"timezone"`
}

type confirmedReminder struct {
	Text string `json:"text"`
	At   string `json:"at"`
	Zone string `json:"zone"`
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

type updateArguments struct {
	JobID       string       `json:"job_id"`
	Name        string       `json:"name"`
	Cron        string       `json:"cron"`
	At          string       `json:"at"`
	Prompt      string       `json:"prompt"`
	PromptEdits []promptEdit `json:"prompt_edits"`
	Timezone    string       `json:"timezone"`
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
	if s.exposure != nil && s.exposure.External(ctx, principalID) {
		return s.proposeReminder(ctx, principalID, call, ReminderText(config), prepared)
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

func (s *Service) proposeReminder(ctx context.Context, principalID string, call tools.Call, text string, prepared Prepared) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "I read outside content in this conversation, so reminders need the user's confirmation, and confirming is not available. Tell the user the reminder was not set.", IsError: true}, nil
	}
	job := prepared.Job
	confirmed := confirmedReminder{Text: text, At: job.RunAt.In(prepared.Schedule.Location).Format(localTimeLayouts[0]), Zone: job.Zone}
	canonical, err := json.Marshal(confirmed)
	if err != nil {
		return tools.Result{}, err
	}
	summary := fmt.Sprintf("Set a reminder for %s:\n%s\nI read outside content in this conversation, so I need your confirmation.", FormatOccurrence(job.RunAt, prepared.Schedule.Location), text)
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: toolReminderSet, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "set", TargetKind: "reminder", TargetName: job.Name}}); err != nil {
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		s.logger.WarnContext(ctx, "schedule.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return tools.Result{Text: "the proposal could not be recorded", IsError: true}, nil
	}
	return tools.Result{Text: approval.ProposedText}, nil
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
	summary := proposalSummary(fmt.Sprintf("Schedule %q", job.Name), prepared)
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolPropose, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "schedule", TargetKind: targetKind, TargetName: job.Name}})
}

// act makes the change at once in a clean turn and proposes it otherwise.
func (s *Service) act(ctx context.Context, principalID string, call tools.Call, proposal approval.Proposal) (tools.Result, error) {
	text, err := s.proposer.Act(ctx, principalID, proposal)
	if err != nil {
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
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, strings.TrimSpace(arguments.JobID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found || !job.FinishedAt.IsZero() {
		return tools.Result{Text: "No pending job has that id. Use schedule_list to see the user's jobs.", IsError: true}, nil
	}
	return tools.Result{Text: renderJob(job, s.zone, maximumPromptRunes)}, nil
}

func (s *Service) proposeUpdate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "Scheduling is not available.", IsError: true}, nil
	}
	var arguments updateArguments
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
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
	switch {
	case job.OneTime() && strings.TrimSpace(arguments.Cron) != "":
		return tools.Result{Text: "This is a one-time job, so it takes at, not cron; to make it recurring, remove it and propose a new job.", IsError: true}, nil
	case !job.OneTime() && strings.TrimSpace(arguments.At) != "":
		return tools.Result{Text: "This is a recurring job, so it takes cron, not at; to make it run once, remove it and propose a new job.", IsError: true}, nil
	}
	if strings.TrimSpace(arguments.Prompt) != "" && len(arguments.PromptEdits) > 0 {
		return tools.Result{Text: "Give either prompt, the complete new instructions, or prompt_edits, not both; nothing was proposed.", IsError: true}, nil
	}
	next := current
	override(&next.Name, arguments.Name)
	override(&next.Cron, arguments.Cron)
	override(&next.At, arguments.At)
	override(&next.Prompt, arguments.Prompt)
	override(&next.Zone, arguments.Timezone)
	if len(arguments.PromptEdits) > 0 {
		edited, err := applyPromptEdits(current.Prompt, arguments.PromptEdits)
		if err != nil {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		next.Prompt = edited
	}
	config, err := ModelTurnConfig(next.Prompt)
	if err != nil {
		return tools.Result{}, err
	}
	prepared, err := s.Prepare(principalID, next.Name, TypeModelTurn, Timing{Cron: next.Cron, At: next.At, Zone: next.Zone}, config)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	changed := prepared.Job
	confirmed := confirmedUpdate{JobID: job.ID, UpdatedAt: job.UpdatedAt, confirmedJob: confirmedJob{Name: changed.Name, Cron: changed.Cron, Zone: changed.Zone, Prompt: ModelTurnPrompt(changed.Config)}}
	if changed.OneTime() {
		confirmed.At = changed.RunAt.In(prepared.Schedule.Location).Format(localTimeLayouts[0])
	}
	if confirmed.confirmedJob == current {
		return tools.Result{Text: "That changes nothing; the job already reads exactly like this.", IsError: true}, nil
	}
	canonical, err := json.Marshal(confirmed)
	if err != nil {
		return tools.Result{}, err
	}
	heading := fmt.Sprintf("Change %q", job.Name)
	if changed.Name != job.Name {
		heading += fmt.Sprintf(", renamed to %q", changed.Name)
	}
	summary := proposalSummary(heading, prepared)
	if len(arguments.PromptEdits) > 0 {
		summary = patchSummary(heading, prepared, arguments.PromptEdits)
	}
	if !job.Enabled {
		summary += "\nIt stays paused until you resume it in /jobs."
	}
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolUpdate, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "change", TargetKind: "scheduled job", TargetName: job.Name, NewName: changed.Name}})
}

// currentJob is a stored model.turn job in the canonical form a proposal carries, with a one-time
// job's instant as a local time in its own zone.
func currentJob(job Job) (confirmedJob, error) {
	current := confirmedJob{Name: job.Name, Cron: job.Cron, Zone: job.Zone, Prompt: ModelTurnPrompt(job.Config)}
	if job.OneTime() {
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
			return "", fmt.Errorf("Edit %d: its find text %q does not occur in the instructions as they are after the edits before it; nothing was proposed. Copy the passage exactly from schedule_get.", index+1, truncateRunes(edit.Find, editQuoteRunes))
		default:
			return "", fmt.Errorf("Edit %d: its find text %q occurs %d times in the instructions; nothing was proposed. Include more of the surrounding text so it occurs once.", index+1, truncateRunes(edit.Find, editQuoteRunes), count)
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
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolRemove, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "remove", TargetKind: "scheduled job", TargetName: job.Name}})
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
		undo, err := s.undoCreate(ctx, record.PrincipalID, record.ID)
		if err != nil {
			return approval.Outcome{}, err
		}
		at := FormatOccurrence(prepared.Job.NextRunAt, prepared.Schedule.Location)
		if prepared.Job.OneTime() {
			return approval.Outcome{Text: "Scheduled for " + at + ".", Undo: undo}, nil
		}
		return approval.Outcome{Text: "Scheduled. First run: " + at + ".", Undo: undo}, nil
	case toolReminderSet:
		var confirmed confirmedReminder
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed reminder could not be read; nothing was set."}
		}
		config, err := ReminderConfig(confirmed.Text)
		if err != nil {
			return approval.Outcome{}, err
		}
		prepared, err := s.Prepare(record.PrincipalID, ReminderName(confirmed.Text), TypeReminder, Timing{At: confirmed.At, Zone: confirmed.Zone}, config)
		if err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That reminder can no longer be set (" + err.Error() + "); nothing was set."}
		}
		if _, err := s.Create(ctx, record.ID, prepared); err != nil {
			if errors.Is(err, ErrTooManyJobs) {
				return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: tooManyText(true) + " Nothing was set."}
			}
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: "Reminder set for " + FormatOccurrence(prepared.Job.RunAt, prepared.Schedule.Location) + "."}, nil
	case toolUpdate:
		return s.executeUpdate(ctx, record)
	case toolRemove:
		var confirmed removeArguments
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed action could not be read; nothing was deleted."}
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
			return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That job no longer exists."}
		}
		undo, err := json.Marshal(jobUndo{JobID: job.ID, Removed: &job})
		if err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: "Deleted.", Undo: undo}, nil
	default:
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That action is no longer available."}
	}
}

func (s *Service) executeUpdate(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedUpdate
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed change could not be read; nothing was changed."}
	}
	job, found, err := s.repository.LoadScheduledJob(ctx, record.PrincipalID, confirmed.JobID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found || !job.FinishedAt.IsZero() || job.Type != TypeModelTurn {
		return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That job no longer exists; nothing was changed."}
	}
	stale := &approval.ActionError{Reason: "stale", Text: "That job changed after this was proposed; nothing was changed. Ask again to change it."}
	if !job.UpdatedAt.Equal(confirmed.UpdatedAt) {
		return approval.Outcome{}, stale
	}
	config, err := ModelTurnConfig(confirmed.Prompt)
	if err != nil {
		return approval.Outcome{}, err
	}
	timing := Timing{Cron: confirmed.Cron, At: confirmed.At, Zone: confirmed.Zone}
	prepared, err := s.Prepare(record.PrincipalID, confirmed.Name, TypeModelTurn, timing, config)
	if err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That schedule is no longer valid (" + err.Error() + "); nothing was changed."}
	}
	updated, err := s.Update(ctx, job, confirmed.UpdatedAt, prepared)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, stale
	}
	undo, err := s.undoUpdate(ctx, job)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !job.Enabled {
		return approval.Outcome{Text: "Changed. It stays paused until you resume it in /jobs.", Undo: undo}, nil
	}
	at := FormatOccurrence(prepared.Job.NextRunAt, prepared.Schedule.Location)
	if prepared.Job.OneTime() {
		return approval.Outcome{Text: "Changed. Runs once: " + at + ".", Undo: undo}, nil
	}
	return approval.Outcome{Text: "Changed. Next run: " + at + ".", Undo: undo}, nil
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
		confirmed = update.confirmedJob
	default:
		return approval.Preview{}, fmt.Errorf("preview %s: not a model turn", record.Tool)
	}
	jobType, known := s.types[TypeModelTurn]
	if !known {
		return approval.Preview{}, fmt.Errorf("preview: the model turn type is not registered")
	}
	config, err := ModelTurnConfig(confirmed.Prompt)
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
func proposalSummary(heading string, prepared Prepared) string {
	return scheduleSummary(heading, prepared) + ":\n" + ModelTurnPrompt(prepared.Job.Config)
}

// patchSummary is the confirmation of a change made by prompt edits: the schedule as it will be and
// only the passages that change, so the user reads the change instead of the whole prompt again.
func patchSummary(heading string, prepared Prepared, edits []promptEdit) string {
	var builder strings.Builder
	builder.WriteString(scheduleSummary(heading, prepared) + ". Its instructions change only here:")
	for index, edit := range edits {
		replacement := edit.Replace
		if strings.TrimSpace(replacement) == "" {
			replacement = "(removed)"
		}
		if len(edits) > 1 {
			fmt.Fprintf(&builder, "\n%d.", index+1)
		}
		builder.WriteString("\nWas: " + edit.Find + "\nNow: " + replacement)
	}
	return builder.String()
}

// scheduleSummary is the heading and the parsed schedule of a proposal, ending with the sentence
// that introduces its instructions.
func scheduleSummary(heading string, prepared Prepared) string {
	job := prepared.Job
	var builder strings.Builder
	builder.WriteString(heading + "\n")
	if job.OneTime() {
		fmt.Fprintf(&builder, "Runs once: %s\n", FormatOccurrence(job.RunAt, prepared.Schedule.Location))
		builder.WriteString("It asks your model, with read-only tools, and sends you the reply")
		return builder.String()
	}
	upcoming := prepared.Schedule.Upcoming(job.CreatedAt, upcomingShown)
	times := make([]string, 0, len(upcoming))
	for _, at := range upcoming {
		times = append(times, FormatOccurrence(at, prepared.Schedule.Location))
	}
	fmt.Fprintf(&builder, "Runs: %s (%s)\n", job.Cron, job.Zone)
	fmt.Fprintf(&builder, "Next: %s\n", strings.Join(times, "; "))
	builder.WriteString("Each run asks your model, with read-only tools, and sends you the reply")
	return builder.String()
}

func renderJobs(jobs []Job, zone string) string {
	if len(jobs) == 0 {
		return "The user has no pending reminders or scheduled jobs."
	}
	rendered := make([]string, 0, len(jobs))
	for _, job := range jobs {
		rendered = append(rendered, renderJob(job, zone, listedInstructionRunes))
	}
	return strings.Join(rendered, "\n\n")
}

// renderJob is one job as the model reads it, with its instructions cut to limit runes.
func renderJob(job Job, zone string, limit int) string {
	if job.Type == TypeReminder {
		return fmt.Sprintf("id %s: reminder, %s\nText: %s", job.ID, DescribeJob(job, zone), truncateRunes(ReminderText(job.Config), limit))
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
	return fmt.Sprintf("id %s: %q, %s\nInstructions: %s", job.ID, job.Name, state, truncateRunes(ModelTurnPrompt(job.Config), limit))
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

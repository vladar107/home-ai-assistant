package task

import (
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
	toolStart  = "task_start"
	toolResult = "task_result"

	// maximumResultBytes keeps a full report within the broker's result limit, beside the
	// untrusted-data prefix the broker adds.
	maximumResultBytes = 15 << 10

	// startGuidance is write guidance, so runs without task_start, such as scheduled runs and the
	// tasks themselves, are never told to call it.
	startGuidance = "Background tasks: when a request needs many lookups or extensive reading, such as going through weeks of mail, " +
		"comparing many web pages, or researching a purchase, call task_start instead of working through it in this turn, then tell " +
		"the user in one sentence that you started it and will report back. The task runs beside this chat with read-only tools and a " +
		"much larger budget, but it cannot see this conversation or ask questions, so its brief must say everything it needs: what to " +
		"look at, the period, what matters to the user, and what the report should contain. Answer directly whatever a few lookups " +
		"can answer. After you have read content from outside the household, such as a web page or an email, in this conversation, task_start only " +
		"proposes the task: the user sees it with Confirm and Reject buttons, so never say it started."
	tooManyText    = "The user already has a background task running and three waiting. Tell them to wait for one to finish or cancel one in /tasks."
	resultGuidance = "When a background task's report comes up, task_result returns its full text."
)

// Proposer is the core approval service as the task service sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

// ExposureChecker reports whether the calling conversation has read content from outside the
// household, after which a task waits for the user's confirmation.
type ExposureChecker interface {
	External(ctx context.Context, principalID string) bool
}

type confirmedTask struct {
	Title     string `json:"title"`
	Brief     string `json:"brief"`
	RequestID string `json:"request_id"`
}

var _ approval.Executor = (*Service)(nil)

var (
	startSchema  = json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","maxLength":80,"description":"A short title the user will recognise, such as 'September mail that needs action'."},"brief":{"type":"string","maxLength":4000,"description":"Self-contained instructions: what to look at, the period, what matters to the user, and what the report should contain. The task cannot see this conversation."}},"required":["title","brief"],"additionalProperties":false}`)
	resultSchema = json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string","description":"The task's id; leave it out to list the user's recent tasks."}},"additionalProperties":false}`)
)

// Contribute offers the task tools to every principal: task_start in chat turns only, since it is
// a write tool and background runs are read-only, and task_result everywhere.
func (s *Service) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{
		Guidance:      resultGuidance,
		WriteGuidance: startGuidance,
		Definitions: []tools.Definition{
			{
				Name: toolStart,
				Description: "Start a background task for a request that needs many lookups or long reading. It runs beside the chat, " +
					"reports back as a reply to the user's message, and returns at once with the task's id.",
				InputSchema:   startSchema,
				Risk:          tools.RiskWrite,
				OutsideBudget: true,
			},
			{
				Name:        toolResult,
				Description: "Return the full report of one of the user's background tasks by id, or list their recent tasks without an id.",
				InputSchema: resultSchema,
				Risk:        tools.RiskRead,
				// A report retells the mail and pages the task read.
				ThirdParty: true,
				Private:    true,
			},
		},
	}, nil
}

func (s *Service) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case toolStart:
		return s.executeStart(ctx, principalID, call.Arguments)
	case toolResult:
		return s.executeResult(ctx, principalID, call.Arguments)
	default:
		return tools.Result{}, fmt.Errorf("unknown task tool")
	}
}

func (s *Service) executeStart(ctx context.Context, principalID string, arguments json.RawMessage) (tools.Result, error) {
	var input struct {
		Title string `json:"title"`
		Brief string `json:"brief"`
	}
	if err := tools.DecodeArguments(arguments, &input); err != nil {
		return tools.Result{Text: "The arguments could not be read; give a title and a brief.", IsError: true}, nil
	}
	requestID, fromChat := strings.CutPrefix(tools.TurnFrom(ctx), outbox.ChatTurn(""))
	if !fromChat || requestID == "" {
		return tools.Result{Text: "A background task can only be started from a chat message.", IsError: true}, nil
	}
	if s.exposure != nil && s.exposure.External(ctx, principalID) {
		return s.proposeStart(ctx, principalID, requestID, input.Title, input.Brief)
	}
	task, err := s.Create(ctx, principalID, requestID, input.Title, input.Brief)
	switch {
	case errors.Is(err, ErrTooManyTasks):
		return tools.Result{Text: tooManyText, IsError: true}, nil
	case isInput(err):
		return tools.Result{Text: err.Error(), IsError: true}, nil
	case err != nil:
		return tools.Result{}, err
	}
	return tools.Result{Text: fmt.Sprintf(
		"Started background task %s, %q. A status message with a Cancel button is in the chat, and the report will arrive as a reply to "+
			"the user's message within %s. Tell the user briefly that you are on it and will report back; do not do the task in this turn.",
		task.ID, task.Title, Duration(s.config.Timeout),
	)}, nil
}

func (s *Service) proposeStart(ctx context.Context, principalID, requestID, title, brief string) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "I read outside content in this conversation, so a background task needs the user's confirmation, and confirming is not available. Tell the user the task was not started.", IsError: true}, nil
	}
	title, brief, err := validateBrief(title, brief)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	canonical, err := json.Marshal(confirmedTask{Title: title, Brief: brief, RequestID: requestID})
	if err != nil {
		return tools.Result{}, err
	}
	summary := fmt.Sprintf("Start the background task %q with this brief:\n%s\nI read outside content in this conversation, so I need your confirmation.", title, brief)
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: toolStart, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "start", TargetKind: "background task", TargetName: title}}); err != nil {
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		s.logger.WarnContext(ctx, "task.proposal.failed", "principal_id", principalID, "error", err)
		return tools.Result{Text: "the proposal could not be recorded", IsError: true}, nil
	}
	return tools.Result{Text: "The task is waiting for the user's confirmation and has not started: after your reply the user sees it with Confirm and Reject buttons. Do not claim it started; tell them it starts once they confirm."}, nil
}

func (s *Service) ActionTools() []string {
	return []string{toolStart}
}

// ExecuteApproved starts the confirmed task from the chat request it answered; the request's
// origin is resolved under the record's principal, so a record cannot reach another user's chat.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	if record.Tool != toolStart {
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That action is no longer available."}
	}
	var confirmed confirmedTask
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed task could not be read; nothing was started."}
	}
	task, err := s.Create(ctx, record.PrincipalID, confirmed.RequestID, confirmed.Title, confirmed.Brief)
	switch {
	case errors.Is(err, ErrTooManyTasks):
		return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: "You already have a background task running and three waiting, so the task was not started. Wait for one to finish or cancel one in /tasks."}
	case isInput(err):
		return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: err.Error() + " Nothing was started."}
	case err != nil:
		return approval.Outcome{}, err
	}
	return approval.Outcome{Text: fmt.Sprintf("Started the background task %q. The report will arrive as a reply to your message within %s.", task.Title, Duration(s.config.Timeout))}, nil
}

func (s *Service) executeResult(ctx context.Context, principalID string, arguments json.RawMessage) (tools.Result, error) {
	var input struct {
		TaskID string `json:"task_id"`
	}
	if err := tools.DecodeArguments(arguments, &input); err != nil {
		return tools.Result{Text: "The arguments could not be read.", IsError: true}, nil
	}
	now := time.Now()
	if strings.TrimSpace(input.TaskID) == "" {
		tasks, err := s.Tasks(ctx, principalID)
		if err != nil {
			return tools.Result{}, err
		}
		if len(tasks) == 0 {
			return tools.Result{Text: "The user has no background tasks from the last week."}, nil
		}
		lines := make([]string, 0, len(tasks))
		for _, task := range tasks {
			lines = append(lines, fmt.Sprintf("%s: %q, %s", task.ID, task.Title, Summary(task, now)))
		}
		return tools.Result{Text: strings.Join(lines, "\n")}, nil
	}
	task, found, err := s.Task(ctx, principalID, strings.TrimSpace(input.TaskID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return tools.Result{Text: "There is no such background task.", IsError: true}, nil
	}
	if task.Open() || task.Result == "" {
		return tools.Result{Text: fmt.Sprintf("Background task %q is %s and has no report.", task.Title, Summary(task, now))}, nil
	}
	report := task.Result
	if len(report) > maximumResultBytes {
		report = strings.ToValidUTF8(report[:maximumResultBytes], "") + "\n[The report continues beyond this point.]"
	}
	heading := fmt.Sprintf("Background task %q, %s:\n\n", task.Title, Summary(task, now))
	return tools.Result{Text: heading + report}, nil
}

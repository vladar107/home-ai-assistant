package conversation

import (
	"cmp"
	"context"
	"strings"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// TaskRun is one model run of a background task: its first run, a resumption after a restart, or
// the request for a report once a limit is reached. All of them use the task's own session.
type TaskRun struct {
	PrincipalID string
	// Turn names the task, so what its tools stage goes out with its report.
	Turn      string
	SessionID string
	Message   string
	// Instruction is the trusted text the run may open links from, the confirmed brief; empty means
	// Message.
	Instruction string
	// Continued marks a run in a session that earlier runs may have filled with outside content,
	// such as a resumed task, so the run starts as having read it.
	Continued bool
	// CallBudget is how many broker tool calls this run may make; zero still offers the tools but
	// refuses every call, so a report run can only write up what the session already holds.
	CallBudget int
	// Report asks only for a write-up of what the session holds, so the runtime's own web search,
	// which no call budget reaches, is left off as well.
	Report bool
	// Sink hears the run's events, in addition to the turn's own tool call log.
	Sink agentruntime.EventSink
}

// TaskReply is what a task run produced: the text for the principal, and a short failure word when
// the text explains a failure instead of reporting.
type TaskReply struct {
	Text      string
	ErrorKind string
}

// RunTask runs one stretch of a background task. Like RunScheduled it uses the principal's own
// subscription, reads memory without being able to change it, and has read tools only, but it does not take the principal's turn slot, so chat turns
// and scheduled runs go on beside it, and its deadline is whatever ctx carries rather than the turn
// timeout. Failures come back as the reply a chat turn would have given. An error is returned only
// when ctx ends, and the caller then decides what the task became.
func (s *Service) RunTask(ctx context.Context, run TaskRun) (TaskReply, error) {
	record := tools.NewRun(cmp.Or(run.Instruction, run.Message))
	if run.Continued {
		record.MarkExternal("")
	}
	runCtx := tools.WithRun(tools.WithCallBudget(tools.WithTurn(ctx, run.Turn), run.CallBudget), record)
	connected, err := s.gate.Ready(runCtx, run.PrincipalID)
	switch {
	case ctx.Err() != nil:
		return TaskReply{}, ctx.Err()
	case err != nil:
		return TaskReply{Text: UnavailableReply, ErrorKind: ErrorKindRuntime}, nil
	case !connected:
		return TaskReply{Text: NotConnectedReply, ErrorKind: ErrorKindNotConnected}, nil
	}
	toolSet, err := s.toolProvider.ReadOnlyToolsFor(runCtx, run.PrincipalID)
	var webSearch bool
	if err == nil && !run.Report {
		webSearch, err = s.webSearchEnabled(runCtx, run.PrincipalID)
	}
	var memory string
	if err == nil {
		memory, err = s.memoryText(runCtx, run.PrincipalID)
	}
	if err != nil {
		if ctx.Err() != nil {
			return TaskReply{}, ctx.Err()
		}
		return TaskReply{Text: UnavailableReply, ErrorKind: ErrorKindToolProvider}, nil
	}
	message := s.withUser(run.PrincipalID, withPlace(s.locationLine(runCtx, run.PrincipalID), run.Message))
	if s.config.Location != nil {
		now := s.config.Now().In(s.config.Location).Format("Monday 2 January 2006, 15:04 MST")
		message = "[system] Current local time: " + now + " (" + s.config.Location.String() + ").\n\n" + message
	}
	var sink agentruntime.EventSink = s.newCountingSink(run.PrincipalID, run.Turn, record)
	if run.Sink != nil {
		sink = sinks{sink, run.Sink}
	}
	result, runErr := s.runtime.Run(runCtx, agentruntime.RunRequest{
		PrincipalID: run.PrincipalID,
		SessionID:   run.SessionID,
		Message:     message,
		Tools:       toolSet,
		Memory:      memory,
		WebSearch:   webSearch,
	}, sink)
	switch {
	case ctx.Err() != nil:
		return TaskReply{}, ctx.Err()
	case runErr != nil || strings.TrimSpace(result.Final) == "":
		reply, kind := failureReply(runErr)
		return TaskReply{Text: reply, ErrorKind: kind}, nil
	default:
		return TaskReply{Text: result.Final}, nil
	}
}

// sinks passes each event to every sink in order and stops at the first that refuses it.
type sinks []agentruntime.EventSink

func (s sinks) Emit(ctx context.Context, event agentruntime.Event) error {
	for _, sink := range s {
		if err := sink.Emit(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

package turn

import (
	"context"
	"errors"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// ScheduledReply is what a scheduled turn produced: the text for the principal, and a short
// failure word when the text explains a failure instead of answering.
type ScheduledReply struct {
	Text      string
	ErrorKind string
	// Private is true when the text may retell Notion page content or NAS file contents the turn
	// read.
	Private bool
	// External is true when the turn read outside content such as web pages or third-party tool
	// results.
	External bool
}

// RunScheduled runs one turn nobody is watching: on the principal's own subscription, in a fresh
// session so chat history stays out of it, with read tools only, and after any chat turn of the
// same principal has finished. Failures come back as the reply a chat turn would have given. An
// error is returned only when ctx ends, and then nothing should be delivered. The turn key names
// the run, so what its tools stage goes out with its reply.
func (e *Engine) RunScheduled(ctx context.Context, principalID, turn, message string) (ScheduledReply, error) {
	release, err := e.Acquire(ctx, Lane{PrincipalID: principalID})
	if err != nil {
		return ScheduledReply{}, err
	}
	defer release()
	return e.runUnwatched(ctx, principalID, turn, message)
}

// RunCheck runs one check of an agent watch as RunScheduled runs a scheduled turn, but under no
// turn key: nothing goes out with the reply but the watch's own alert, so its tools stage nothing.
// It takes no turn slot, as a background task does not: checks run all day, and the user's chat
// turn must not wait behind one. The caller reads the verdict from the final text.
func (e *Engine) RunCheck(ctx context.Context, principalID, message string) (ScheduledReply, error) {
	return e.runUnwatched(ctx, principalID, "", message)
}

func (e *Engine) runUnwatched(ctx context.Context, principalID, turn, message string) (ScheduledReply, error) {
	run := tools.NewRun(message)
	turnCtx, cancel := context.WithTimeout(tools.WithRun(tools.WithTurn(ctx, turn), run), e.config.TurnTimeout)
	defer cancel()
	connected, err := e.gate.Ready(turnCtx, principalID)
	switch {
	case ctx.Err() != nil:
		return ScheduledReply{}, ctx.Err()
	case err != nil:
		return ScheduledReply{Text: UnavailableReply, ErrorKind: "runtime"}, nil
	case !connected:
		return ScheduledReply{Text: NotConnectedReply, ErrorKind: "not_connected"}, nil
	}
	tools, err := e.tools.ReadOnlyToolsFor(turnCtx, principalID)
	var webSearch bool
	if err == nil {
		webSearch, err = e.webSearchEnabled(turnCtx, principalID)
	}
	var memory string
	if err == nil {
		memory, err = e.memoryText(turnCtx, principalID)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ScheduledReply{}, ctx.Err()
		}
		return ScheduledReply{Text: UnavailableReply, ErrorKind: "tool_provider"}, nil
	}
	sessionID, err := NewID()
	if err != nil {
		return ScheduledReply{}, err
	}
	result, runErr := e.runtime.Run(turnCtx, agentruntime.RunRequest{
		PrincipalID: principalID,
		SessionID:   sessionID,
		Message:     e.withTime(e.withUser(principalID, withPlace(e.placeLines(turnCtx, principalID), message))),
		Tools:       tools,
		Memory:      memory,
		WebSearch:   webSearch,
		SearchCheck: e.searchCheck(principalID, webSearch, false),
	}, e.newCountingSink(principalID, turn, run))
	switch {
	case ctx.Err() != nil:
		return ScheduledReply{}, ctx.Err()
	case errors.Is(turnCtx.Err(), context.DeadlineExceeded) || errors.Is(runErr, agentruntime.ErrStalled):
		return ScheduledReply{Text: TimeoutReply, ErrorKind: "timeout"}, nil
	case runErr != nil || strings.TrimSpace(result.Final) == "":
		reply, kind := FailureReply(runErr)
		return ScheduledReply{Text: reply, ErrorKind: kind}, nil
	default:
		return ScheduledReply{Text: result.Final, Private: run.Private(), External: run.External()}, nil
	}
}

func withPlace(place, message string) string {
	return prompt.Prepend(prompt.Place(place), message)
}

func (e *Engine) withUser(principalID, message string) string {
	return prompt.Prepend(prompt.User(e.config.DisplayNames[principalID]), message)
}

// withTime starts a turn's message with the current local time in the household zone.
func (e *Engine) withTime(message string) string {
	if e.config.Location == nil {
		return message
	}
	return prompt.Prepend(prompt.Time(e.config.Now(), e.config.Location), message)
}

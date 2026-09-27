package conversation

import (
	"context"
	"errors"
	"strings"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// ScheduledReply is what a scheduled turn produced: the text for the principal, and a short
// failure word when the text explains a failure instead of answering.
type ScheduledReply struct {
	Text      string
	ErrorKind string
}

// RunScheduled runs one turn nobody is watching: on the principal's own subscription, in a fresh
// session so chat history stays out of it, with read tools only, and after any chat turn of the
// same principal has finished. Failures come back as the reply a chat turn would have given. An
// error is returned only when ctx ends, and then nothing should be delivered.
func (s *Service) RunScheduled(ctx context.Context, principalID, message string) (ScheduledReply, error) {
	release, err := s.acquireTurn(ctx, principalID)
	if err != nil {
		return ScheduledReply{}, err
	}
	defer release()

	turnCtx, cancel := context.WithTimeout(ctx, s.config.TurnTimeout)
	defer cancel()
	connected, err := s.gate.Ready(turnCtx, principalID)
	switch {
	case ctx.Err() != nil:
		return ScheduledReply{}, ctx.Err()
	case err != nil:
		return ScheduledReply{Text: UnavailableReply, ErrorKind: "runtime"}, nil
	case !connected:
		return ScheduledReply{Text: NotConnectedReply, ErrorKind: "not_connected"}, nil
	}
	tools, err := s.toolProvider.ReadOnlyToolsFor(turnCtx, principalID)
	var webSearch bool
	if err == nil {
		webSearch, err = s.webSearchEnabled(turnCtx, principalID)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ScheduledReply{}, ctx.Err()
		}
		return ScheduledReply{Text: UnavailableReply, ErrorKind: "tool_provider"}, nil
	}
	sessionID, err := newID()
	if err != nil {
		return ScheduledReply{}, err
	}
	result, runErr := s.runtime.Run(turnCtx, agentruntime.RunRequest{
		PrincipalID: principalID,
		SessionID:   sessionID,
		Message:     message,
		Tools:       tools,
		WebSearch:   webSearch,
	}, s.turnSink(principalID, webSearch))
	switch {
	case ctx.Err() != nil:
		return ScheduledReply{}, ctx.Err()
	case errors.Is(turnCtx.Err(), context.DeadlineExceeded):
		return ScheduledReply{Text: TimeoutReply, ErrorKind: "timeout"}, nil
	case runErr != nil || strings.TrimSpace(result.Response) == "":
		reply, kind := failureReply(runErr)
		return ScheduledReply{Text: reply, ErrorKind: kind}, nil
	default:
		return ScheduledReply{Text: result.Response}, nil
	}
}

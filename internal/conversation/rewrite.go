package conversation

import (
	"context"
	"errors"
	"strings"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// errNotConnected means the principal has no model subscription connected.
var errNotConnected = errors.New("the principal has no model connection")

// RewriteAlert rewrites a message the application wrote, such as a watch alert, into language on
// the principal's own subscription. The run has no tools, memory, or web search and a fresh
// session, and it does not take the principal's turn slot, so a chat turn does not hold it back.
// Its deadline is whatever ctx carries.
func (s *Service) RewriteAlert(ctx context.Context, principalID, language, text string) (string, error) {
	connected, err := s.gate.Ready(ctx, principalID)
	if err != nil {
		return "", err
	}
	if !connected {
		return "", errNotConnected
	}
	sessionID, err := newID()
	if err != nil {
		return "", err
	}
	result, err := s.runtime.Run(ctx, agentruntime.RunRequest{
		PrincipalID: principalID,
		SessionID:   sessionID,
		Message: "[system] Rewrite the message below in " + language + " for the user. The application wrote it from live data, so keep its meaning exactly: " +
			"change no time, date, number, line, or platform, keep station, stop, and place names as they are, and add or leave out nothing. " +
			"Keep its emoji and its **bold** markers. Reply with the rewritten message only.\n\n" + text,
	}, discardEvents{})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Final), nil
}

type discardEvents struct{}

func (discardEvents) Emit(context.Context, agentruntime.Event) error { return nil }

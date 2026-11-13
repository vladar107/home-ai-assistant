package turn

import (
	"context"
	"errors"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// errNotConnected means the principal has no model subscription connected.
var errNotConnected = errors.New("the principal has no model connection")

// rewriteRules is what every rewrite is held to, whatever names its language.
const rewriteRules = "The application wrote it from live data, so keep its meaning exactly: " +
	"change no time, date, number, line, or platform, keep station, stop, and place names as they are, and add or leave out nothing. " +
	"Keep its emoji and its **bold** markers. Reply with the rewritten message only."

// RewriteAlert rewrites a message the application wrote, such as a watch alert, into language on
// the principal's own subscription. The run has no tools, memory, or web search and a fresh
// session, and it does not take the principal's turn slot, so a chat turn does not hold it back.
// Its deadline is whatever ctx carries.
func (e *Engine) RewriteAlert(ctx context.Context, principalID, language, text string) (string, error) {
	return e.rewrite(ctx, principalID, "[system] Rewrite the message below in "+language+" for the user. "+rewriteRules+"\n\n"+text)
}

// RewriteAlertLike is RewriteAlert with the language named by a sample of it, a message the user
// wrote: the run is told to read the sample for its language and for nothing else. A blank sample
// names no language, and the text comes back as it is.
func (e *Engine) RewriteAlertLike(ctx context.Context, principalID, sample, text string) (string, error) {
	quoted := prompt.LanguageSample(sample)
	if quoted == "" {
		return text, nil
	}
	return e.rewrite(ctx, principalID, "[system] Rewrite the message between the lines of dashes for the user, in the language of the language sample that follows it. "+
		"Keep each line a line, and keep every title, name, and booking reference as it is written. "+rewriteRules+"\n\n"+
		"---\n"+text+"\n---\n\n"+quoted)
}

func (e *Engine) rewrite(ctx context.Context, principalID, message string) (string, error) {
	connected, err := e.gate.Ready(ctx, principalID)
	if err != nil {
		return "", err
	}
	if !connected {
		return "", errNotConnected
	}
	sessionID, err := NewID()
	if err != nil {
		return "", err
	}
	result, err := e.runtime.Run(ctx, agentruntime.RunRequest{PrincipalID: principalID, SessionID: sessionID, Message: message}, discardEvents{})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Final), nil
}

type discardEvents struct{}

func (discardEvents) Emit(context.Context, agentruntime.Event) error { return nil }

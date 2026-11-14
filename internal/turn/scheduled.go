package turn

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
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
// the run, so what its tools stage goes out with its reply. An empty model runs on Pi's default,
// and the run has the scheduled turn's own time limit.
func (e *Engine) RunScheduled(ctx context.Context, principalID, turn, message string, model agentruntime.ModelEffort) (ScheduledReply, error) {
	release, err := e.Acquire(ctx, Lane{PrincipalID: principalID})
	if err != nil {
		return ScheduledReply{}, err
	}
	defer release()
	return e.runUnwatched(ctx, principalID, turn, message, model, e.config.ScheduledTurnTimeout, TraceScheduled, turn)
}

// RunCheck runs one check of an agent watch as RunScheduled runs a scheduled turn, but under no
// turn key: nothing goes out with the reply but the watch's own alert, so its tools stage nothing.
// It takes no turn slot, as a background task does not: checks run all day, and the user's chat
// turn must not wait behind one. The caller reads the verdict from the final text. The check keeps
// a trace under the key ctx names, if any.
func (e *Engine) RunCheck(ctx context.Context, principalID, message string) (ScheduledReply, error) {
	return e.runUnwatched(ctx, principalID, "", message, agentruntime.ModelEffort{}, e.config.TurnTimeout, TraceCheck, TraceKeyFrom(ctx))
}

// RunReview runs the turn of a weekly review on the principal's own subscription and the given
// model: with no tools, memory, or web search, since the message carries everything the review
// reads, and in a fresh session. Like RunCheck it takes no turn slot. Failures come back as the
// reply a chat turn would have given; an error is returned only when ctx ends.
func (e *Engine) RunReview(ctx context.Context, principalID, message string, model agentruntime.ModelEffort) (ScheduledReply, error) {
	return e.runToolless(ctx, principalID, message, model, false, "", agentruntime.RunBackground)
}

// RunAssess runs the turn that decides whether an inbound event is worth telling the user about:
// like RunReview with no tools or web search, but with the principal's memory, which says what they
// care about, and their name. Like RunCheck it takes no turn slot, and the caller reads the
// decision from the final text. The turn keeps a trace under the key ctx names, if any.
func (e *Engine) RunAssess(ctx context.Context, principalID, message string) (ScheduledReply, error) {
	return e.runToolless(ctx, principalID, message, agentruntime.ModelEffort{}, true, TraceAssess, agentruntime.RunBackground)
}

// RunExplain runs the turn that explains what one of the assistant's messages was based on, from
// the record the message's turn kept: like RunReview with no tools, memory, or web search, on the
// runtime's default model. The user is waiting on it, so it runs as a chat turn and never queues
// behind the shared background slots or takes one; the caller runs at most one per principal. It
// keeps no trace of its own.
func (e *Engine) RunExplain(ctx context.Context, principalID, message string) (ScheduledReply, error) {
	return e.runToolless(ctx, principalID, message, agentruntime.ModelEffort{}, false, "", agentruntime.RunChat)
}

// runToolless runs a turn without tools; a kind traces it under the key ctx names.
func (e *Engine) runToolless(ctx context.Context, principalID, message string, model agentruntime.ModelEffort, withMemory bool, kind string, runKind agentruntime.RunKind) (ScheduledReply, error) {
	started := e.config.Now()
	// A failure reply is stored as the run's reply, which the model reads later, so it stays English
	// whatever language the run carries.
	english := locale.With(ctx, locale.English)
	turnCtx, cancel := context.WithTimeout(ctx, e.config.TurnTimeout)
	defer cancel()
	if reply, ready, err := e.unwatchedGate(english, turnCtx, principalID); !ready {
		return reply, err
	}
	var memory string
	if withMemory {
		var err error
		if memory, err = e.memoryText(turnCtx, principalID); err != nil {
			if ctx.Err() != nil {
				return ScheduledReply{}, ctx.Err()
			}
			return ScheduledReply{Text: UnavailableReply.Format(english), ErrorKind: "tool_provider"}, nil
		}
		message = e.withUser(principalID, message)
	}
	sessionID, err := NewID()
	if err != nil {
		return ScheduledReply{}, err
	}
	result, runErr := e.runtime.Run(turnCtx, agentruntime.RunRequest{
		PrincipalID: principalID,
		SessionID:   sessionID,
		Message:     e.withTime(withLanguageFor(ctx, runKind, message)),
		Memory:      memory,
		Kind:        runKind,
		Model:       model.Model,
		Effort:      model.Effort,
	}, discardEvents{})
	if kind != "" {
		e.saveTrace(ctx, tracedRun{principalID: principalID, key: TraceKeyFrom(ctx), kind: kind, model: model.Model, effort: model.Effort, started: started, result: result,
			// An assessed event's text is the turn's message, written by the user's own system, which may
			// repeat text from anywhere.
			external: kind == TraceAssess})
	}
	return unwatchedReply(english, turnCtx, result, runErr, nil)
}

func (e *Engine) runUnwatched(ctx context.Context, principalID, turn, message string, model agentruntime.ModelEffort, timeout time.Duration, kind, traceKey string) (ScheduledReply, error) {
	started := e.config.Now()
	english := locale.With(ctx, locale.English)
	run := tools.NewRun(message)
	turnCtx, cancel := context.WithTimeout(tools.WithRun(tools.WithTurn(ctx, turn), run), timeout)
	defer cancel()
	if reply, ready, err := e.unwatchedGate(english, turnCtx, principalID); !ready {
		return reply, err
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
		return ScheduledReply{Text: UnavailableReply.Format(english), ErrorKind: "tool_provider"}, nil
	}
	sessionID, err := NewID()
	if err != nil {
		return ScheduledReply{}, err
	}
	sink := e.newCountingSink(principalID, turn, run)
	result, runErr := e.runtime.Run(turnCtx, agentruntime.RunRequest{
		PrincipalID: principalID,
		SessionID:   sessionID,
		Message:     e.withTime(withLanguage(ctx, e.withUser(principalID, withPlace(e.placeLines(turnCtx, principalID), e.withCollections(turnCtx, principalID, message))))),
		Tools:       tools,
		Memory:      memory,
		WebSearch:   webSearch,
		SearchCheck: e.searchCheck(principalID, webSearch, false),
		Model:       model.Model,
		Effort:      model.Effort,
	}, sink)
	e.saveTrace(ctx, tracedRun{
		principalID: principalID, key: traceKey, kind: kind, model: model.Model, effort: model.Effort, started: started,
		result: result, executor: tools.Executor, run: run, recorder: sink.trace,
	})
	reply, err := unwatchedReply(english, turnCtx, result, runErr, run)
	// A check's reply is read by code, not shown, so only a scheduled reply names the sources the
	// user reads; one with nothing to add sends nothing, so it is left as it is.
	if err == nil && kind == TraceScheduled && reply.ErrorKind == "" && !strings.Contains(reply.Text, prompt.NothingToAdd) {
		reply.Text = run.Attributed(reply.Text)
	}
	return reply, err
}

// unwatchedGate checks the principal's own connection before an unwatched turn. When the turn
// must not run, ready is false and the reply or the ended context is what to return instead.
func (e *Engine) unwatchedGate(ctx, turnCtx context.Context, principalID string) (reply ScheduledReply, ready bool, err error) {
	connected, err := e.gate.Ready(turnCtx, principalID)
	switch {
	case ctx.Err() != nil:
		return ScheduledReply{}, false, ctx.Err()
	case err != nil:
		return ScheduledReply{Text: UnavailableReply.Format(ctx), ErrorKind: "runtime"}, false, nil
	case !connected:
		return ScheduledReply{Text: NotConnectedReply.Format(ctx), ErrorKind: "not_connected"}, false, nil
	}
	return ScheduledReply{}, true, nil
}

// unwatchedReply turns an unwatched run's result into its reply; run, when set, says what the
// turn's tools read.
func unwatchedReply(ctx, turnCtx context.Context, result agentruntime.RunResult, runErr error, run *tools.Run) (ScheduledReply, error) {
	switch {
	case ctx.Err() != nil:
		return ScheduledReply{}, ctx.Err()
	case errors.Is(turnCtx.Err(), context.DeadlineExceeded) || errors.Is(runErr, agentruntime.ErrStalled):
		return ScheduledReply{Text: TimeoutReply.Format(ctx), ErrorKind: "timeout"}, nil
	case runErr != nil || strings.TrimSpace(result.Final) == "":
		reply, kind := FailureReply(ctx, runErr)
		return ScheduledReply{Text: reply, ErrorKind: kind}, nil
	case run == nil:
		return ScheduledReply{Text: result.Final}, nil
	default:
		return ScheduledReply{Text: result.Final, Private: run.Private(), External: run.External()}, nil
	}
}

func withPlace(place, message string) string {
	return prompt.Prepend(prompt.Place(place), message)
}

// withLanguage names the language the user chose, which the caller of an unattended turn put in the
// context; chat turns are never told, and keep answering in the language the user writes in.
func withLanguage(ctx context.Context, message string) string {
	return prompt.Prepend(prompt.InterfaceLanguage(locale.From(ctx)), message)
}

func withLanguageFor(ctx context.Context, kind agentruntime.RunKind, message string) string {
	if kind == agentruntime.RunChat {
		return message
	}
	return withLanguage(ctx, message)
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

package placeholder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// Runtime is the layer nearest the model. It hides the request's message, memory, jobs and tool
// guidance, and every tool result, behind tokens, resolves the tokens in the model's tool
// arguments before a tool runs, and resolves them in the text the model streams and returns. It
// fails the run when the vault cannot be read or written, so raw text never goes out in place of
// tokens, and logs placeholder.vault.failed with the stage that failed.
type Runtime struct {
	inner  agentruntime.AgentRuntime
	vault  Vault
	hide   bool
	logger *slog.Logger
}

// NewRuntime wraps inner. With hide false nothing is hidden, but tokens are still resolved, so a
// token already in a resumed session keeps reaching tools and users as its real value. A nil
// logger logs nothing.
func NewRuntime(inner agentruntime.AgentRuntime, vault Vault, hide bool, logger *slog.Logger) *Runtime {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Runtime{inner: inner, vault: vault, hide: hide, logger: logger}
}

// toolGuidance tells the model how to use placeholders, so it never rebuilds an address from one.
const toolGuidance = "Email addresses, phone numbers and street addresses reach you as placeholders: ⟦email-… domain⟧, ⟦phone-…⟧ or ⟦address-…⟧. " +
	"The user sees the real value wherever you write one, in your reply and in tool arguments alike. " +
	"So when the user asks for an address, number or place, answer with its placeholder exactly as written, brackets included; " +
	"never say it is hidden or that you cannot tell it. Never rewrite a placeholder as an address or number yourself."

const (
	stageHide    = "hide"
	stageResolve = "resolve"
)

func (r *Runtime) Run(ctx context.Context, request agentruntime.RunRequest, sink agentruntime.EventSink) (agentruntime.RunResult, error) {
	mapper := New(r.vault, request.PrincipalID)
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	failed := &failure{cancel: cancel, logger: r.logger, principalID: request.PrincipalID}
	if r.hide {
		for _, text := range []*string{&request.Message, &request.Memory, &request.Jobs, &request.Tools.Guidance} {
			if *text == "" {
				continue
			}
			hidden, err := mapper.Hide(ctx, *text, nil)
			if err != nil {
				return agentruntime.RunResult{}, failed.fail(ctx, stageHide, err)
			}
			*text = hidden
		}
		if !request.Tools.Empty() {
			request.Tools.Guidance = strings.TrimSpace(request.Tools.Guidance + "\n\n" + toolGuidance)
		}
	}
	if request.Tools.Executor != nil {
		request.Tools.Executor = &executor{inner: request.Tools.Executor, mapper: mapper, hide: r.hide, failed: failed}
	}
	if request.SearchCheck != nil {
		request.SearchCheck = searchCheck(request.SearchCheck, mapper, failed)
	}
	events := &resolvingSink{inner: sink, mapper: mapper, stream: mapper.Stream(runCtx), failed: failed}
	result, err := r.inner.Run(runCtx, request, events)
	if err == nil {
		err = events.flush(runCtx)
	}
	if cause := failed.cause(); cause != nil {
		return agentruntime.RunResult{Usage: result.Usage, Model: result.Model, Tokens: result.Tokens}, cause
	}
	for _, text := range []*string{&result.Response, &result.Final} {
		resolved, resolveErr := mapper.ResolveText(ctx, *text)
		if resolveErr != nil {
			return agentruntime.RunResult{Usage: result.Usage, Model: result.Model, Tokens: result.Tokens}, failed.fail(ctx, stageResolve, resolveErr)
		}
		*text = resolved
	}
	return result, err
}

// ForgetImages forwards to the inner runtime when it keeps transcripts and does nothing otherwise.
func (r *Runtime) ForgetImages(ctx context.Context, principalID, runtimeSessionID string) error {
	if forgetter, ok := r.inner.(interface {
		ForgetImages(context.Context, string, string) error
	}); ok {
		return forgetter.ForgetImages(ctx, principalID, runtimeSessionID)
	}
	return nil
}

// failure keeps the first vault error of a run, logs it, and stops the run with it. A tool
// executor's error only reaches the model as a failed call, so the run is cancelled to fail the
// turn instead.
type failure struct {
	cancel      context.CancelCauseFunc
	logger      *slog.Logger
	principalID string

	mu  sync.Mutex
	err error
}

func (f *failure) fail(ctx context.Context, stage string, err error) error {
	f.mu.Lock()
	first := f.err == nil
	if first {
		f.err = err
	}
	f.mu.Unlock()
	if first {
		f.logger.WarnContext(ctx, "placeholder.vault.failed", "principal_id", f.principalID, "stage", stage, "error", err)
	}
	f.cancel(err)
	return err
}

func (f *failure) cause() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

type executor struct {
	inner  agentruntime.ToolExecutor
	mapper *Mapper
	hide   bool
	failed *failure
}

// undecodableArguments is what the model reads for a call whose arguments are not one JSON value.
const undecodableArguments = "The tool arguments could not be read; send them as one JSON object."

func (e *executor) Execute(ctx context.Context, call agentruntime.ToolCall) (agentruntime.ToolResult, error) {
	if len(bytes.TrimSpace(call.Arguments)) > 0 && !json.Valid(call.Arguments) {
		return agentruntime.ToolResult{Text: undecodableArguments, IsError: true}, nil
	}
	arguments, err := e.mapper.ResolveArguments(ctx, call.Arguments)
	var refused *ResolveError
	if errors.As(err, &refused) {
		return agentruntime.ToolResult{Text: refused.Error(), IsError: true}, nil
	}
	if err != nil {
		return agentruntime.ToolResult{}, e.failed.fail(ctx, stageResolve, err)
	}
	call.Arguments = arguments
	result, err := e.inner.Execute(ctx, call)
	if err != nil {
		return agentruntime.ToolResult{}, err
	}
	if e.hide {
		if result.Text, err = e.mapper.Hide(ctx, result.Text, result.Sensitive); err != nil {
			return agentruntime.ToolResult{}, e.failed.fail(ctx, stageHide, err)
		}
	}
	result.Sensitive = nil
	return result, nil
}

// hiddenSearch is what the model reads for a web search whose query holds a placeholder.
const hiddenSearch = "The search was not run: its query holds a placeholder, and hidden values cannot be searched. Search without them."

// searchCheck denies a web search whose query holds a placeholder, which the search would send out
// in place of the value, before check is asked, so no card offers it; other queries go to check.
func searchCheck(check agentruntime.SearchCheck, mapper *Mapper, failed *failure) agentruntime.SearchCheck {
	return func(ctx context.Context, query string) agentruntime.SearchDecision {
		held, err := mapper.HoldsPlaceholder(ctx, query)
		if err != nil {
			failed.fail(ctx, stageResolve, err)
		}
		if err != nil || held {
			return agentruntime.SearchDecision{Verdict: agentruntime.SearchDeny, Reason: "placeholder", Text: hiddenSearch}
		}
		return check(ctx, query)
	}
}

// resolvingSink resolves the tokens in the model's streamed text and in tool call arguments. Its stream holds a token split
// across deltas until it is whole, and releases what it holds before any other event, since a
// token never spans two messages.
type resolvingSink struct {
	inner  agentruntime.EventSink
	mapper *Mapper
	failed *failure

	mu     sync.Mutex
	stream *StreamResolver
}

func (s *resolvingSink) Emit(ctx context.Context, event agentruntime.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.Type == "text_delta" {
		text, err := s.stream.Write(event.Text)
		if err != nil {
			return s.failed.fail(ctx, stageResolve, err)
		}
		if text == "" {
			return nil
		}
		event.Text = text
		return s.emit(ctx, event)
	}
	if err := s.release(ctx); err != nil {
		return err
	}
	if event.Type == "tool_call" && len(event.Arguments) > 0 {
		resolved, err := s.mapper.ResolveText(ctx, string(event.Arguments))
		if err != nil {
			return s.failed.fail(ctx, stageResolve, err)
		}
		if json.Valid([]byte(resolved)) {
			event.Arguments = json.RawMessage(resolved)
		}
	}
	return s.emit(ctx, event)
}

func (s *resolvingSink) flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.release(ctx)
}

func (s *resolvingSink) release(ctx context.Context) error {
	held, err := s.stream.Flush()
	if err != nil {
		return s.failed.fail(ctx, stageResolve, err)
	}
	if held == "" {
		return nil
	}
	return s.emit(ctx, agentruntime.Event{Type: "text_delta", Text: held})
}

func (s *resolvingSink) emit(ctx context.Context, event agentruntime.Event) error {
	if s.inner == nil {
		return nil
	}
	return s.inner.Emit(ctx, event)
}

package turn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// The kinds of turn a trace records.
const (
	TraceChat      = "chat"
	TraceScheduled = "scheduled"
	TraceTask      = "task"
	TraceCheck     = "check"
	TraceAssess    = "assess"
	// TraceCode records a message that code decided on with no model turn, such as a watch alert
	// a code check raised; its Detail says what the code checked.
	TraceCode = "code"
)

const (
	// maximumTraceCalls bounds the calls one trace keeps; a run's budget allows far fewer.
	// MaximumTraceDetailRunes bounds what a code trace says the code checked.
	MaximumTraceDetailRunes = 4000
	maximumTraceCalls       = 100
	traceStoreTimeout       = 5 * time.Second
	// maximumCallIDBytes bounds the runtime's call id a trace keeps; a longer one still matches the
	// call's events and the broker's record, but is not stored.
	maximumCallIDBytes = 1000
)

// traceToolPattern is the shape of a tool name; a model-chosen name of any other shape is kept as
// "invalid".
var traceToolPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Trace is what one turn did, kept so the user can later ask what a message it delivered was based
// on: which tools ran with what outcome, what each looked at, and whether outside or private content
// reached it. It holds no arguments and no results. Calls of runs that share a key, such as a
// background task's run and the report run after it, add up to one trace.
type Trace struct {
	PrincipalID string
	Key         string
	Kind        string
	// Model and Effort answered the turn; empty means the runtime's default.
	Model  string
	Effort string
	// Detail is what code checked, for a TraceCode trace.
	Detail     string
	StartedAt  time.Time
	FinishedAt time.Time
	External   bool
	Private    bool
	Calls      []TraceCall
}

// TraceCall is one tool call of a traced turn, in the order the runtime started them. Label names
// what the call looked at, such as a page's address or a search's query; it may be private, so it is
// kept only in its owner's database and never logged or exported.
type TraceCall struct {
	Sequence    int
	CallID      string
	Tool        string
	Outcome     string
	Duration    time.Duration
	ResultBytes int
	Label       string
}

// TraceStore keeps the traces of turns and the messages each delivered.
type TraceStore interface {
	// SaveTurnTrace stores a turn's trace, adding its calls to those of an earlier trace under the
	// same key.
	SaveTurnTrace(ctx context.Context, trace Trace) error
	MessageRecorder
}

// MessageRecorder links a message a turn delivered to the turn's trace. A message whose key has no
// trace, such as a reminder's, is not recorded.
type MessageRecorder interface {
	RecordTurnMessage(ctx context.Context, principalID, key string, chatID, messageID int64) error
}

// RecordMessage links a delivered message to its turn's trace. A nil recorder, no key, or no
// message records nothing; the message is already in the chat, so a failure, which costs only its
// explanation, is logged rather than returned.
func RecordMessage(ctx context.Context, recorder MessageRecorder, logger *slog.Logger, principalID, key string, chatID, messageID int64) {
	if recorder == nil || key == "" || messageID <= 0 {
		return
	}
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), traceStoreTimeout)
	defer cancel()
	if err := recorder.RecordTurnMessage(storeCtx, principalID, key, chatID, messageID); err != nil {
		logger.WarnContext(ctx, "turn.trace.message_failed", "principal_id", principalID, "error", err)
	}
}

type traceKey struct{}

// WithTraceKey names the trace a turn run under ctx keeps, for an entry point whose turns have no
// key of their own, such as a check or an assessment; without one such a turn keeps no trace.
func WithTraceKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, traceKey{}, key)
}

// TraceKeyFrom returns the trace key ctx names, or "".
func TraceKeyFrom(ctx context.Context) string {
	key, _ := ctx.Value(traceKey{}).(string)
	return key
}

// The outcomes a trace records for a call the broker did not report on.
const (
	callSucceeded  = "success"
	callFailed     = "failed"
	callUnfinished = "unfinished"
)

// traceRecorder follows a run's tool calls from its events. The runtime's own web search never
// reaches the broker, so the events are the only record of it.
type traceRecorder struct {
	now func() time.Time

	mu      sync.Mutex
	calls   []TraceCall
	started []time.Time
	ended   []bool
	// names are the tool names as the start events gave them, which the end events repeat even when
	// the kept name is "invalid".
	names []string
}

func (r *traceRecorder) start(event agentruntime.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) >= maximumTraceCalls {
		return
	}
	call := TraceCall{Sequence: len(r.calls) + 1, CallID: event.CallID, Tool: event.Text}
	if !traceToolPattern.MatchString(call.Tool) {
		call.Tool = "invalid"
	}
	if call.Tool == WebSearchTool {
		call.Label = searchLabel(event.Arguments)
	}
	r.calls = append(r.calls, call)
	r.started = append(r.started, r.now())
	r.ended = append(r.ended, false)
	r.names = append(r.names, event.Text)
}

// end matches a result to its call by the runtime's call id, or, from a runtime without ids, to
// the oldest unfinished call of the same tool, by the name the start event gave.
func (r *traceRecorder) end(event agentruntime.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.calls {
		call := &r.calls[index]
		if r.ended[index] || call.CallID != event.CallID || (event.CallID == "" && r.names[index] != event.Tool) {
			continue
		}
		r.ended[index] = true
		call.Duration = r.now().Sub(r.started[index])
		call.ResultBytes = len(event.Text)
		call.Outcome = callSucceeded
		if event.IsError {
			call.Outcome = callFailed
		}
		return
	}
}

// finish returns the calls with what the broker learned of each: its own outcome, which is finer
// than the runtime's error flag, and the label of what the call looked at.
func (r *traceRecorder) finish(log *tools.CallLog) []TraceCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := make([]TraceCall, len(r.calls))
	copy(calls, r.calls)
	for index := range calls {
		call := &calls[index]
		if log != nil && call.CallID != "" {
			if record, found := log.Call(call.CallID); found {
				call.Outcome = record.Outcome
				if record.Label != "" {
					call.Label = record.Label
				}
			}
		}
		if call.Outcome == "" {
			call.Outcome = callUnfinished
		}
		if len(call.CallID) > maximumCallIDBytes {
			call.CallID = ""
		}
	}
	return calls
}

// searchLabel is a web search's query, read from the call's arguments in memory only.
func searchLabel(arguments json.RawMessage) string {
	var search struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(arguments, &search); err != nil {
		return ""
	}
	return tools.CleanLabel(strings.TrimSpace(search.Query))
}

// callLogOf returns the log the executor keeps of its calls, or nil for an executor without one.
func callLogOf(executor agentruntime.ToolExecutor) *tools.CallLog {
	logged, ok := executor.(interface{ CallLog() *tools.CallLog })
	if !ok {
		return nil
	}
	return logged.CallLog()
}

// tracedRun is what a run's trace is made of besides its calls.
type tracedRun struct {
	principalID string
	key         string
	kind        string
	model       string
	effort      string
	started     time.Time
	result      agentruntime.RunResult
	executor    agentruntime.ToolExecutor
	run         *tools.Run
	recorder    *traceRecorder
	// external marks a run whose message is itself outside content, such as an inbound event's
	// assessment.
	external bool
}

// traceOf makes a finished run's trace from what its record and the executor's call log hold now.
func (e *Engine) traceOf(traced tracedRun) Trace {
	trace := Trace{
		PrincipalID: traced.principalID,
		Key:         traced.key,
		Kind:        traced.kind,
		Model:       traced.result.Model,
		Effort:      traced.effort,
		StartedAt:   traced.started,
		FinishedAt:  e.config.Now(),
		External:    traced.external,
	}
	if trace.Model == "" {
		trace.Model = traced.model
	}
	if traced.run != nil {
		trace.External, trace.Private = trace.External || traced.run.External(), traced.run.Private()
	}
	if traced.recorder != nil {
		trace.Calls = traced.recorder.finish(callLogOf(traced.executor))
	}
	return trace
}

// saveTrace keeps a finished run's trace when it has a key and the engine a store. A failure is
// logged, without any label, and never fails the turn.
func (e *Engine) saveTrace(ctx context.Context, traced tracedRun) {
	if e.config.Traces == nil || traced.key == "" {
		return
	}
	e.storeTrace(ctx, e.traceOf(traced))
}

// saveTraceLater keeps a chat turn's trace off the path to its reply. A link made through
// RecordTurnMessage waits for the write, so a delivered reply is never linked before its trace is
// there; once the engine has shut down, a trace is written at once.
func (e *Engine) saveTraceLater(ctx context.Context, traced tracedRun) {
	if e.config.Traces == nil || traced.key == "" {
		return
	}
	trace := e.traceOf(traced)
	id := pendingID(trace.PrincipalID, trace.Key)
	e.pendingMu.Lock()
	if e.pendingClosed {
		e.pendingMu.Unlock()
		e.storeTrace(ctx, trace)
		return
	}
	done := make(chan struct{})
	e.pending[id] = done
	e.pendingWG.Add(1)
	e.pendingMu.Unlock()
	go func() {
		defer e.pendingWG.Done()
		e.storeTrace(ctx, trace)
		e.pendingMu.Lock()
		if e.pending[id] == done {
			delete(e.pending, id)
		}
		e.pendingMu.Unlock()
		close(done)
	}()
}

func pendingID(principalID, key string) string { return principalID + "\x00" + key }

func (e *Engine) storeTrace(ctx context.Context, trace Trace) {
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), traceStoreTimeout)
	defer cancel()
	if err := e.config.Traces.SaveTurnTrace(storeCtx, trace); err != nil {
		e.telemetry.ContextLogger(ctx, e.logger).WarnContext(ctx, "turn.trace.failed", "principal_id", trace.PrincipalID, "kind", trace.Kind, "error", err)
	}
}

// RecordTurnMessage links a delivered message to its turn's trace once a write of that trace still
// on its way has ended; without a store it links nothing.
func (e *Engine) RecordTurnMessage(ctx context.Context, principalID, key string, chatID, messageID int64) error {
	if e.config.Traces == nil {
		return nil
	}
	e.pendingMu.Lock()
	done := e.pending[pendingID(principalID, key)]
	e.pendingMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return e.config.Traces.RecordTurnMessage(ctx, principalID, key, chatID, messageID)
}

// Shutdown waits for the chat traces still being written; a trace after it is written at once.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.pendingMu.Lock()
	e.pendingClosed = true
	e.pendingMu.Unlock()
	done := make(chan struct{})
	go func() {
		e.pendingWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown turn traces: %w", ctx.Err())
	}
}

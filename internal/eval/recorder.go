package eval

import (
	"context"
	"encoding/json"
	"sync"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// ToolCall is one call the model made during a run and the answer the application gave.
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error,omitempty"`
}

// RunRecord is one run of the model runtime: what it was asked, what it streamed and how it ended.
type RunRecord struct {
	Request   agentruntime.RunRequest
	Events    []agentruntime.Event
	ToolCalls []ToolCall
	Result    agentruntime.RunResult
	Err       error
}

// Recorder wraps the model runtime and keeps every run, so a case's checks and the cassette read
// what the model was sent and what it did. It forwards the optional interfaces the wiring looks
// for on a runtime.
type Recorder struct {
	inner agentruntime.AgentRuntime

	mu   sync.Mutex
	runs []RunRecord
}

// NewRecorder wraps a runtime.
func NewRecorder(inner agentruntime.AgentRuntime) *Recorder { return &Recorder{inner: inner} }

// Mark returns a position that Since reads the runs after.
func (r *Recorder) Mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

// Since returns the runs recorded after the mark.
func (r *Recorder) Since(mark int) []RunRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RunRecord(nil), r.runs[mark:]...)
}

func (r *Recorder) Run(ctx context.Context, request agentruntime.RunRequest, sink agentruntime.EventSink) (agentruntime.RunResult, error) {
	record := &runLog{}
	if request.Tools.Executor != nil {
		request.Tools.Executor = &recordingExecutor{inner: request.Tools.Executor, log: record}
	}
	result, err := r.inner.Run(ctx, request, &recordingSink{inner: sink, log: record})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, RunRecord{Request: request, Events: record.events, ToolCalls: record.calls, Result: result, Err: err})
	return result, err
}

// ForgetImages forwards to the runtime when it keeps transcripts and does nothing otherwise.
func (r *Recorder) ForgetImages(ctx context.Context, principalID, runtimeSessionID string) error {
	if forgetter, ok := r.inner.(interface {
		ForgetImages(context.Context, string, string) error
	}); ok {
		return forgetter.ForgetImages(ctx, principalID, runtimeSessionID)
	}
	return nil
}

// runLog gathers one run's events and calls; a run's tools may be called in parallel.
type runLog struct {
	mu     sync.Mutex
	events []agentruntime.Event
	calls  []ToolCall
}

type recordingSink struct {
	inner agentruntime.EventSink
	log   *runLog
}

func (s *recordingSink) Emit(ctx context.Context, event agentruntime.Event) error {
	s.log.mu.Lock()
	s.log.events = append(s.log.events, event)
	s.log.mu.Unlock()
	if s.inner == nil {
		return nil
	}
	return s.inner.Emit(ctx, event)
}

type recordingExecutor struct {
	inner agentruntime.ToolExecutor
	log   *runLog
}

func (e *recordingExecutor) Execute(ctx context.Context, call agentruntime.ToolCall) (agentruntime.ToolResult, error) {
	result, err := e.inner.Execute(ctx, call)
	recorded := ToolCall{Name: call.Name, Arguments: call.Arguments, Result: result.Text, IsError: result.IsError}
	if err != nil {
		recorded.IsError = true
	}
	e.log.mu.Lock()
	e.log.calls = append(e.log.calls, recorded)
	e.log.mu.Unlock()
	return result, err
}

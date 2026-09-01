package runtime

import (
	"context"
	"encoding/json"
)

type RunRequest struct {
	PrincipalID string
	SessionID   string
	Message     string
	Tools       []ToolDefinition
	Metadata    map[string]string
}

type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type Event struct {
	Type string
	Text string
}

type EventSink interface {
	Emit(context.Context, Event) error
}

type RunResult struct {
	Response string
}

type AgentRuntime interface {
	Run(context.Context, RunRequest, EventSink) (RunResult, error)
}

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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

// ProfileDirectoryName returns the per-principal directory name for runtime profiles.
// Principal IDs are free-form configuration, so only a conservative subset may name a directory.
func ProfileDirectoryName(principalID string) (string, error) {
	if !profileDirectoryPattern.MatchString(principalID) {
		return "", fmt.Errorf("principal ID cannot name a runtime profile directory")
	}
	return principalID, nil
}

var profileDirectoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type FailureCategory string

const (
	FailureQuota FailureCategory = "provider_quota"
	FailureAuth  FailureCategory = "provider_auth"
)

// ProviderError carries only the category so provider text never reaches an error, a log, or a reply.
type ProviderError struct {
	Category FailureCategory
}

func (e *ProviderError) Error() string {
	switch e.Category {
	case FailureQuota:
		return "model provider reported that the subscription usage limit was reached"
	case FailureAuth:
		return "model provider rejected the subscription credentials"
	default:
		return "model provider reported a failure"
	}
}

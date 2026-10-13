package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

type RunRequest struct {
	PrincipalID string
	SessionID   string
	Message     string
	Images      []Image
	Tools       ToolSet
	// Memory is the principal's rendered memory, which the runtime adds to the system prompt; empty
	// leaves the prompt without it.
	Memory string
	// Jobs describes the principal's scheduled jobs, which the runtime adds to the system prompt
	// after memory; empty leaves it out.
	Jobs string
	// WebSearch means the principal has web access turned on, so the runtime offers its own web
	// search alongside the broker's tools.
	WebSearch bool
	// SearchCheck decides each web search of the turn before the runtime runs it; a nil check
	// refuses every search.
	SearchCheck SearchCheck
	Metadata    map[string]string
}

// SearchVerdict is what a search check decided about one query.
type SearchVerdict string

const (
	SearchAllow   SearchVerdict = "allow"
	SearchConfirm SearchVerdict = "confirm"
	SearchDeny    SearchVerdict = "deny"
)

// SearchDecision is the answer to one query. Text is what the model reads in place of results when
// the search does not run; Reason is a short word for metrics.
type SearchDecision struct {
	Verdict SearchVerdict `json:"verdict"`
	Reason  string        `json:"reason"`
	Text    string        `json:"text"`
}

// SearchCheck is called under the turn's context for each query the runtime's web search is about
// to send out.
type SearchCheck func(ctx context.Context, query string) SearchDecision

// Image is a picture the turn's message carried. It is the only non-text input a runtime accepts.
type Image struct {
	Name     string
	MimeType string
	Data     []byte
}

// ToolSet is what one turn may call: the definitions offered to the model, prompt guidance for
// them, and the executor that runs every call under the turn's principal. An empty set runs the
// turn without tools.
type ToolSet struct {
	Definitions []ToolDefinition
	Guidance    string
	Executor    ToolExecutor
	// Activities maps an offered tool's name to its activity key, for showing the user what the
	// turn is doing. A runtime never sends it to the model.
	Activities map[string]string
}

func (t ToolSet) Empty() bool {
	return len(t.Definitions) == 0
}

type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolExecutor runs the calls a model makes during one turn. The result is what the model may
// read; an error means the call could not be attempted at all.
type ToolExecutor interface {
	Execute(context.Context, ToolCall) (ToolResult, error)
}

type ToolCall struct {
	Name      string
	Arguments json.RawMessage
}

type ToolResult struct {
	Text    string
	IsError bool
}

type Event struct {
	Type string
	Text string
	// Tool names the tool of a "tool_result" event, whose Text is the result the model read.
	Tool string
	// Usage is set on a "model_call" event, sent once per finished assistant message, so a long
	// run can be measured and bounded while it is still going.
	Usage Usage
}

type EventSink interface {
	Emit(context.Context, Event) error
}

type RunResult struct {
	// Response joins the text of every assistant message in the turn.
	Response string
	// Final is the text of the last assistant message alone, empty when that message had none.
	Final string
	Usage Usage
}

// Usage is what the model reported for one turn; zero values mean the runtime did not report it.
type Usage struct {
	ModelCalls   int
	PromptTokens int64
	OutputTokens int64
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

// ErrStalled means the runtime heard nothing from the model for too long and gave up on the turn.
var ErrStalled = errors.New("model produced no output")

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

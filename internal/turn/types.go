package turn

import (
	"context"
	"log/slog"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// Error kinds a failed turn reports to telemetry. Startup creates each kind's series at zero from
// TurnErrorKinds, so a new kind belongs there too.
const (
	ErrorKindTimeout       = "timeout"
	ErrorKindRuntime       = "runtime"
	ErrorKindToolProvider  = "tool_provider"
	ErrorKindAttachments   = "attachments"
	ErrorKindNotConnected  = "not_connected"
	ErrorKindProviderQuota = string(agentruntime.FailureQuota)
	ErrorKindProviderAuth  = string(agentruntime.FailureAuth)
)

func TurnErrorKinds() []string {
	return []string{
		ErrorKindTimeout,
		ErrorKindRuntime,
		ErrorKindToolProvider,
		ErrorKindAttachments,
		ErrorKindNotConnected,
		ErrorKindProviderQuota,
		ErrorKindProviderAuth,
	}
}

// The replies a failed turn gives the user.
const (
	TimeoutReply      = "That response took too long. Please try again with a smaller request."
	UnavailableReply  = "I could not complete that response. /status and /privacy are still available."
	QuotaReply        = "Your model subscription has reached its usage limit. Please try again later."
	ReconnectReply    = "Your model subscription is not connected or has expired. Reconnect it and try again."
	NotConnectedReply = "Your model subscription is not connected. Use /settings to connect it."
)

// Lane is one conversation place of a principal: a Telegram tab, or the chat's main view at thread
// zero, which is the only lane while the bot has tabs off. Each lane holds its own active session
// and runs its turns apart from the others.
type Lane struct {
	PrincipalID string
	ThreadID    int64
}

// ConnectionGate reports whether a principal's own model connection is usable before a turn starts.
type ConnectionGate interface {
	Ready(context.Context, string) (bool, error)
}

// ToolProvider returns the tools a principal may use in one turn, with the executor bound to that
// principal; an empty set runs the turn without tools.
type ToolProvider interface {
	ToolsFor(context.Context, string) (agentruntime.ToolSet, error)
	// ReadOnlyToolsFor is the same set limited to read tools, for a turn nobody is watching.
	ReadOnlyToolsFor(context.Context, string) (agentruntime.ToolSet, error)
}

// Telemetry is what the engine asks of the metrics: the logger of a context's trace.
type Telemetry interface {
	ContextLogger(context.Context, *slog.Logger) *slog.Logger
}

// Jobs describes the principal's scheduled jobs for the system prompt of chat turns and background
// tasks, or returns "" when there are none or they cannot be read.
type Jobs interface {
	PromptSection(ctx context.Context, principalID string) string
}

// WebSearch reports whether a principal has web access on, so the runtime offers its own web
// search in their turns; nil never offers it.
type WebSearch interface {
	WebSearchEnabled(ctx context.Context, principalID string) (bool, error)
}

// Locations describes a principal's shared position for a turn, or returns "" when there is none.
type Locations interface {
	TurnLine(ctx context.Context, principalID string) (string, error)
}

// Trips describes a principal's active or next trip for a turn, or returns "" when there is none.
type Trips interface {
	TurnLine(ctx context.Context, principalID string) (string, error)
}

// Memory is the principal's remembered instructions and facts, which every turn starts from.
type Memory interface {
	// Render is the memory as prompt text, or "" when it is off or empty.
	Render(ctx context.Context, principalID string) (string, error)
	// TakeNotes returns, and forgets, the lines the turn's memory writes owe its reply.
	TakeNotes(principalID, turn string) []string
}

// SearchChecks decides one web search query of a run. A run with nobody to confirm a search, such
// as a scheduled job or a background task, is not interactive.
type SearchChecks interface {
	Check(ctx context.Context, principalID, query string, interactive bool) agentruntime.SearchDecision
}

// Config is what the engine reads besides the runtime, the gate, and the tools.
type Config struct {
	// TurnTimeout bounds an unattended run; a chat turn's own deadline is the coordinator's.
	TurnTimeout time.Duration
	// Location is the household's time zone; each turn then tells the model the current local
	// time. Nil leaves the time out.
	Location *time.Location
	// Now is the clock that time is read from; nil means time.Now.
	Now func() time.Time
	// DisplayNames tells each turn which household member its user is; a principal without a
	// name goes unnamed.
	DisplayNames map[string]string
	// Memory goes into every turn's system prompt; nil leaves memory out.
	Memory Memory
	// Jobs goes into the system prompt of chat turns and background tasks beside memory; nil
	// leaves it out.
	Jobs Jobs
	// WebSearch decides whether a turn offers the runtime's web search; nil never offers it.
	WebSearch WebSearch
	// SearchChecks decides each query of a turn's web search; nil refuses every search.
	SearchChecks SearchChecks
	// Exposure records a turn whose web search read text from outside the household; nil records
	// nothing.
	Exposure tools.TurnExposureRecorder
	// Locations tells each turn where its user is while they share a location; nil leaves it out.
	Locations Locations
	// Trips tells each turn about the user's active or next trip; nil leaves it out.
	Trips Trips
}

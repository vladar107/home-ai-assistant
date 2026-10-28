// Package turn runs one model turn for a principal: the connection gate, the tools, the switches
// that go into the system prompt, the message the model receives, and the run itself. The chat
// coordinator in internal/conversation and the unattended callers (scheduled runs, watch checks,
// background tasks, rewrites, tab names) all run their turns through an Engine.
package turn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// Engine runs model turns. It starts no goroutines and keeps no state beyond the per-lane turn
// slots, so it has nothing to start or shut down.
type Engine struct {
	runtime   agentruntime.AgentRuntime
	gate      ConnectionGate
	tools     ToolProvider
	config    Config
	logger    *slog.Logger
	telemetry Telemetry

	mu sync.Mutex
	// slots holds one slot per lane, so a lane's chat turns take turns with each other and the main
	// lane's also with scheduled runs. It once guarded a whole principal against a credential-refresh
	// race that Pi 1.0.0 removed by locking auth.json, which is why tabs and background tasks run
	// beside each other.
	slots map[Lane]chan struct{}
}

// TurnTimeout is how long a turn may run.
func (e *Engine) TurnTimeout() time.Duration { return e.config.TurnTimeout }

// Location is the household's time zone, nil when turns carry no time line.
func (e *Engine) Location() *time.Location { return e.config.Location }

func New(config Config, runtime agentruntime.AgentRuntime, gate ConnectionGate, toolProvider ToolProvider, logger *slog.Logger, telemetry Telemetry) (*Engine, error) {
	if config.TurnTimeout <= 0 {
		return nil, fmt.Errorf("turn timeout must be positive")
	}
	if runtime == nil {
		return nil, fmt.Errorf("turn runtime is required")
	}
	if gate == nil {
		return nil, fmt.Errorf("turn connection gate is required")
	}
	if toolProvider == nil {
		return nil, fmt.Errorf("turn tool provider is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("turn logger is required")
	}
	if telemetry == nil {
		return nil, fmt.Errorf("turn telemetry is required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.ScheduledTurnTimeout <= 0 {
		config.ScheduledTurnTimeout = config.TurnTimeout
	}
	return &Engine{
		runtime:   runtime,
		gate:      gate,
		tools:     toolProvider,
		config:    config,
		logger:    logger,
		telemetry: telemetry,
		slots:     make(map[Lane]chan struct{}),
	}, nil
}

// Ready reports whether the principal's own model connection is usable.
func (e *Engine) Ready(ctx context.Context, principalID string) (bool, error) {
	return e.gate.Ready(ctx, principalID)
}

// Tools returns every tool the principal may use in a chat turn.
func (e *Engine) Tools(ctx context.Context, principalID string) (agentruntime.ToolSet, error) {
	return e.tools.ToolsFor(ctx, principalID)
}

// Switches is what a turn reads about the principal at its start besides the tools.
type Switches struct {
	WebSearch bool
	Memory    string
}

// Switches reads the web search switch and then the principal's memory. An error fails the turn
// instead of guessing either way.
func (e *Engine) Switches(ctx context.Context, principalID string) (Switches, error) {
	var switches Switches
	var err error
	switches.WebSearch, err = e.webSearchEnabled(ctx, principalID)
	if err == nil {
		switches.Memory, err = e.memoryText(ctx, principalID)
	}
	return switches, err
}

// Parts is what the coordinator owns of a chat turn's message; Compose adds the rest.
type Parts struct {
	Message string
	// Forwarded marks a message the user forwarded, whose text is someone else's.
	Forwarded bool
	// Hint is the tool the message most likely needs, or "".
	Hint string
	// Notice is what became of the principal's earlier requests, or "".
	Notice string
	// Subject is the line a conversation about a job, watch, or trip starts from, or "".
	Subject string
	// Attachments describes the files the message carried, or "".
	Attachments string
}

// Compose builds the message the model receives for a chat turn: the user's text with the lines
// each turn starts from, and the attachments' description after it.
func (e *Engine) Compose(ctx context.Context, principalID string, parts Parts) string {
	message := parts.Message
	if parts.Forwarded {
		message = prompt.Prepend(prompt.Forwarded, message)
	}
	message = prompt.Prepend(prompt.ToolHint(parts.Hint), message)
	message = prompt.Prepend(prompt.Notice(parts.Notice), message)
	message = e.withUser(principalID, withPlace(e.placeLines(ctx, principalID), message))
	message = prompt.Prepend(parts.Subject, message)
	message = e.withTime(message)
	if parts.Attachments == "" {
		return message
	}
	if strings.TrimSpace(message) == "" {
		return parts.Attachments
	}
	return strings.TrimRight(message, "\n") + "\n\n" + parts.Attachments
}

// Run is one model run of a chat turn, set up by the caller. A user is there to confirm a web
// search, and the principal's scheduled jobs are part of the system prompt.
type Run struct {
	PrincipalID string
	SessionID   string
	// Turn names the run, so what its tools stage goes out with its reply.
	Turn    string
	Message string
	Images  []agentruntime.Image
	Tools   agentruntime.ToolSet
	Memory  string
	// WebSearch offers the runtime's own web search.
	WebSearch bool
	Kind      agentruntime.RunKind
	// Model and Effort are the pair the run uses; both empty leaves the runtime's default.
	Model  string
	Effort string
	// Run is the record the turn's tools mark; a web search result marks it as having read
	// outside content.
	Run *tools.Run
	// Sink hears the run's events beside the engine's own counting.
	Sink agentruntime.EventSink
}

// Result is what a run produced and which tools it called, by name.
type Result struct {
	agentruntime.RunResult
	Calls  int
	Called map[string]bool
}

// Run runs the model once. The result is filled in whether or not the run failed.
func (e *Engine) Run(ctx context.Context, run Run) (Result, error) {
	counting := e.newCountingSink(run.PrincipalID, run.Turn, run.Run)
	var sink agentruntime.EventSink = counting
	if run.Sink != nil {
		sink = sinks{counting, run.Sink}
	}
	request := agentruntime.RunRequest{
		PrincipalID: run.PrincipalID,
		SessionID:   run.SessionID,
		Message:     run.Message,
		Images:      run.Images,
		Tools:       run.Tools,
		Memory:      run.Memory,
		WebSearch:   run.WebSearch,
		SearchCheck: e.searchCheck(run.PrincipalID, run.WebSearch, true),
		Jobs:        e.jobsText(ctx, run.PrincipalID),
		Kind:        run.Kind,
		Model:       run.Model,
		Effort:      run.Effort,
	}
	result, err := e.runtime.Run(ctx, request, sink)
	calls, called := counting.snapshot()
	return Result{RunResult: result, Calls: calls, Called: called}, err
}

// MemoryNotes returns, and forgets, the lines the turn's memory writes owe its reply.
func (e *Engine) MemoryNotes(principalID, turn string) []string {
	if e.config.Memory == nil {
		return nil
	}
	return e.config.Memory.TakeNotes(principalID, turn)
}

// WithMemoryNotes ends the reply with a line for each memory write the turn made, so the user
// does not depend on the model mentioning them.
func WithMemoryNotes(reply string, notes []string) string {
	if len(notes) == 0 {
		return reply
	}
	return strings.TrimRight(reply, "\n") + "\n\n" + strings.Join(notes, "\n")
}

// TryAcquire takes the lane's turn slot if it is free.
func (e *Engine) TryAcquire(lane Lane) (release func(), ok bool) {
	slot := e.slot(lane)
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	default:
		return nil, false
	}
}

// Acquire waits for the lane's turn slot until ctx ends.
func (e *Engine) Acquire(ctx context.Context, lane Lane) (release func(), err error) {
	slot := e.slot(lane)
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *Engine) slot(lane Lane) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	slot, exists := e.slots[lane]
	if !exists {
		slot = make(chan struct{}, 1)
		e.slots[lane] = slot
	}
	return slot
}

// webSearchEnabled reads the principal's switch at turn start; an error fails the turn instead of
// guessing either way.
func (e *Engine) webSearchEnabled(ctx context.Context, principalID string) (bool, error) {
	if e.config.WebSearch == nil {
		return false, nil
	}
	return e.config.WebSearch.WebSearchEnabled(ctx, principalID)
}

// searchCheck binds the search check to the principal for one run, or returns nil when the run
// has no web search. Chat turns can ask the user to confirm a search; scheduled runs and tasks
// cannot.
func (e *Engine) searchCheck(principalID string, webSearch, interactive bool) agentruntime.SearchCheck {
	if !webSearch || e.config.SearchChecks == nil {
		return nil
	}
	return func(ctx context.Context, query string) agentruntime.SearchDecision {
		return e.config.SearchChecks.Check(ctx, principalID, query, interactive)
	}
}

// memoryText reads the principal's memory at turn start; an error fails the turn instead of
// running it without what the user asked it to remember.
func (e *Engine) memoryText(ctx context.Context, principalID string) (string, error) {
	if e.config.Memory == nil {
		return "", nil
	}
	return e.config.Memory.Render(ctx, principalID)
}

func (e *Engine) jobsText(ctx context.Context, principalID string) string {
	if e.config.Jobs == nil {
		return ""
	}
	return e.config.Jobs.PromptSection(ctx, principalID)
}

// FailureReply maps a failed run to the reply the user sees and the category telemetry reports.
func FailureReply(err error) (string, string) {
	if errors.Is(err, agentruntime.ErrStalled) {
		return TimeoutReply, ErrorKindTimeout
	}
	var providerErr *agentruntime.ProviderError
	if errors.As(err, &providerErr) {
		switch providerErr.Category {
		case agentruntime.FailureQuota:
			return QuotaReply, ErrorKindProviderQuota
		case agentruntime.FailureAuth:
			return ReconnectReply, ErrorKindProviderAuth
		}
	}
	return UnavailableReply, ErrorKindRuntime
}

// WebSearchTool runs inside the runtime rather than through the broker, so its calls get no
// broker audit line; the turn's sink records each by name instead.
const WebSearchTool = "web_search"

// countingSink remembers which tools a turn called, by name only, and logs each web search call.
// A web search result also marks the turn as having read third-party content, which the broker
// cannot do for a tool that runs inside the runtime. The mark waits for the result, so the search
// check of a call still in flight never sees that call's own exposure.
type countingSink struct {
	principalID string
	turn        string
	logger      *slog.Logger
	telemetry   Telemetry
	exposure    tools.TurnExposureRecorder
	run         *tools.Run

	mu     sync.Mutex
	calls  int
	called map[string]bool
}

func (e *Engine) newCountingSink(principalID, turn string, run *tools.Run) *countingSink {
	return &countingSink{principalID: principalID, turn: turn, logger: e.logger, telemetry: e.telemetry, exposure: e.config.Exposure, run: run, called: make(map[string]bool)}
}

func (c *countingSink) Emit(ctx context.Context, event agentruntime.Event) error {
	if event.Type == "tool_result" && event.Tool == WebSearchTool {
		if c.run != nil {
			c.run.MarkExternal(event.Text)
		}
		if c.exposure != nil {
			c.exposure.MarkTurnExposed(ctx, c.principalID, c.turn, true)
		}
		return nil
	}
	if event.Type != "tool_call" {
		return nil
	}
	c.mu.Lock()
	c.calls++
	c.called[event.Text] = true
	c.mu.Unlock()
	if event.Text == WebSearchTool {
		c.telemetry.ContextLogger(ctx, c.logger).InfoContext(ctx, "conversation.web_search.called", "principal_id", c.principalID, "tool", WebSearchTool)
	}
	return nil
}

func (c *countingSink) snapshot() (int, map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	called := make(map[string]bool, len(c.called))
	for name := range c.called {
		called[name] = true
	}
	return c.calls, called
}

// NewID returns a random version 4 UUID.
func NewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate conversation identifier: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

// locationLine is where the principal is, for the turn's message. A failed lookup leaves the
// line out rather than failing the turn.
func (e *Engine) locationLine(ctx context.Context, principalID string) string {
	if e.config.Locations == nil {
		return ""
	}
	line, err := e.config.Locations.TurnLine(ctx, principalID)
	if err != nil {
		e.telemetry.ContextLogger(ctx, e.logger).WarnContext(ctx, "conversation.location.failed", "principal_id", principalID, "error", err)
		return ""
	}
	return line
}

// tripLine is the principal's active or next trip, for the turn's message. A failed lookup
// leaves the line out rather than failing the turn.
func (e *Engine) tripLine(ctx context.Context, principalID string) string {
	if e.config.Trips == nil {
		return ""
	}
	line, err := e.config.Trips.TurnLine(ctx, principalID)
	if err != nil {
		e.telemetry.ContextLogger(ctx, e.logger).WarnContext(ctx, "conversation.trip.failed", "principal_id", principalID, "error", err)
		return ""
	}
	return line
}

// placeLines is where the principal is and where they are going, for the turn's message.
func (e *Engine) placeLines(ctx context.Context, principalID string) string {
	var lines []string
	for _, line := range []string{e.locationLine(ctx, principalID), e.tripLine(ctx, principalID)} {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " ")
}

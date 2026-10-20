package wiring

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

const (
	// toolOutcomeRetention is how long the weekly review's tool outcome counts are kept.
	toolOutcomeRetention = 30 * 24 * time.Hour
	// toolOutcomeBuffer is how many outcomes wait for the writer before a call's outcome is dropped.
	toolOutcomeBuffer = 256
)

// toolOutcome is one call's outcome waiting to be counted.
type toolOutcome struct {
	principal, tool, outcome string
	at                       time.Time
}

// toolOutcomeLog counts each tool call's outcome in the store for the weekly review. A call only
// hands its outcome to one writer goroutine, so a busy database never holds a call up; when the
// writer falls behind, outcomes are dropped and the next write logs how many. The writer prunes
// counts past their retention on the first outcome of each day. It keeps the wall clock, as the
// replies and reactions the review reads beside it do.
type toolOutcomeLog struct {
	store    *sqlite.Store
	logger   *slog.Logger
	outcomes chan toolOutcome
	dropped  atomic.Int64

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	// pruned is the UTC day of the last prune; only the writer reads and writes it.
	pruned string
}

func newToolOutcomeLog(store *sqlite.Store, logger *slog.Logger) *toolOutcomeLog {
	return &toolOutcomeLog{store: store, logger: logger, outcomes: make(chan toolOutcome, toolOutcomeBuffer)}
}

func (l *toolOutcomeLog) RecordToolOutcome(_ context.Context, principal, tool, outcome string) {
	select {
	case l.outcomes <- toolOutcome{principal: principal, tool: tool, outcome: outcome, at: time.Now()}:
	default:
		l.dropped.Add(1)
	}
}

// Start begins the writer.
func (l *toolOutcomeLog) Start(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done != nil {
		return fmt.Errorf("tool outcome log is already running")
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l.cancel, l.done = cancel, make(chan struct{})
	go l.run(runCtx, l.done)
	return nil
}

// Shutdown stops the writer once it has written the outcomes already waiting.
func (l *toolOutcomeLog) Shutdown(ctx context.Context) error {
	l.mu.Lock()
	cancel, done := l.cancel, l.done
	l.cancel, l.done = nil, nil
	l.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown tool outcome log: %w", ctx.Err())
	}
}

func (l *toolOutcomeLog) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case outcome := <-l.outcomes:
			l.write(ctx, outcome)
		case <-ctx.Done():
			drainCtx := context.WithoutCancel(ctx)
			for {
				select {
				case outcome := <-l.outcomes:
					l.write(drainCtx, outcome)
				default:
					l.reportDropped(drainCtx)
					return
				}
			}
		}
	}
}

func (l *toolOutcomeLog) write(ctx context.Context, outcome toolOutcome) {
	if err := l.store.RecordToolOutcome(ctx, outcome.principal, outcome.tool, outcome.outcome, outcome.at); err != nil {
		l.logger.WarnContext(ctx, "tools.outcome.unrecorded", "principal_id", outcome.principal, "tool", outcome.tool, "error", err)
	}
	l.reportDropped(ctx)
	if day := outcome.at.UTC().Format(time.DateOnly); day != l.pruned {
		l.pruned = day
		if err := l.store.PruneToolOutcomes(ctx, outcome.at.Add(-toolOutcomeRetention)); err != nil {
			l.logger.WarnContext(ctx, "tools.outcome.prune_failed", "error", err)
		}
	}
}

func (l *toolOutcomeLog) reportDropped(ctx context.Context) {
	if dropped := l.dropped.Swap(0); dropped > 0 {
		l.logger.WarnContext(ctx, "tools.outcome.dropped", "count", dropped)
	}
}

// reviewChecker runs the weekly review's turn on the chat default model.
type reviewChecker struct {
	engine *turn.Engine
	model  agentruntime.ModelEffort
}

func (c reviewChecker) RunReview(ctx context.Context, principalID, message string) (string, string, error) {
	reply, err := c.engine.RunReview(ctx, principalID, message, c.model)
	return reply.Text, reply.ErrorKind, err
}

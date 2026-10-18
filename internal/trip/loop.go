package trip

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	loopModuleVersion = "0.1.0"

	DefaultLoopInterval = time.Minute

	loopPruneInterval = time.Hour
	// maximumListedNames bounds how many paused or resumed jobs a notice names.
	maximumListedNames = 10
)

// Notices is the part of the trip service the loop drives.
type Notices interface {
	// Settle runs once before the first cycle.
	Settle(ctx context.Context) error
	DueNotices(ctx context.Context, now time.Time) ([]Notice, error)
	MarkStarted(ctx context.Context, principalID, id string, at time.Time) (bool, error)
	MarkEnded(ctx context.Context, principalID, id string, at time.Time) (bool, error)
	Prune(ctx context.Context, now time.Time) error
	ActiveTrips(ctx context.Context, principalID string, at time.Time) ([]Trip, error)
}

type LoopConfig struct {
	// Interval is the time between cycles; zero means a minute.
	Interval time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Loop sends the start and end notices trips owe and prunes old trips. A notice is marked sent
// only after it was delivered, so a failed send is retried on the next cycle and a crash between
// the send and the mark can repeat a notice once, which beats losing it.
type Loop struct {
	notices  Notices
	paused   []Paused
	sender   Sender
	logger   *slog.Logger
	interval time.Duration
	now      func() time.Time

	cycleMu    sync.Mutex
	lastPruned time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var _ module.Module = (*Loop)(nil)

func NewLoop(cfg LoopConfig, notices Notices, paused []Paused, sender Sender, logger *slog.Logger) (*Loop, error) {
	if notices == nil {
		return nil, fmt.Errorf("trip notices are required")
	}
	if sender == nil {
		return nil, fmt.Errorf("trip sender is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("trip logger is required")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultLoopInterval
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Loop{notices: notices, paused: paused, sender: sender, logger: logger, interval: interval, now: now}, nil
}

func (l *Loop) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "trip", Version: loopModuleVersion, Kind: module.KindSystem}
}

func (l *Loop) Start(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done != nil {
		return fmt.Errorf("trip loop is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel, l.done = cancel, make(chan struct{})
	go l.loop(ctx, l.done)
	return nil
}

func (l *Loop) Ready(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done == nil {
		return fmt.Errorf("trip loop is not running")
	}
	return nil
}

func (l *Loop) Shutdown(ctx context.Context) error {
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
		return fmt.Errorf("shutdown trip loop: %w", ctx.Err())
	}
}

func (l *Loop) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	if err := l.notices.Settle(ctx); err != nil && ctx.Err() == nil {
		l.logger.WarnContext(ctx, "trip.loop.failed", "stage", "settle", "error", err)
	}
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		l.Cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Cycle sends every notice that is due and prunes once an hour. The loop runs it on every tick; it
// is safe to call directly.
func (l *Loop) Cycle(ctx context.Context) {
	l.cycleMu.Lock()
	defer l.cycleMu.Unlock()
	now := l.now()
	due, err := l.notices.DueNotices(ctx, now)
	if err != nil && ctx.Err() == nil {
		l.logger.WarnContext(ctx, "trip.loop.failed", "stage", "due_notices", "error", err)
	}
	for _, notice := range due {
		if ctx.Err() != nil {
			return
		}
		l.deliver(ctx, notice, now)
	}
	if now.Sub(l.lastPruned) >= loopPruneInterval && ctx.Err() == nil {
		l.lastPruned = now
		if err := l.notices.Prune(ctx, now); err != nil && ctx.Err() == nil {
			l.logger.WarnContext(ctx, "trip.loop.failed", "stage", "prune", "error", err)
		}
	}
}

func (l *Loop) deliver(ctx context.Context, notice Notice, now time.Time) {
	trip := notice.Trip
	text := l.text(ctx, notice)
	if err := l.sender.Send(ctx, trip.PrincipalID, trip.ID, text); err != nil {
		if ctx.Err() == nil {
			l.logger.WarnContext(ctx, "trip.notice.send_failed", "trip_id", trip.ID, "kind", notice.Kind, "error", err)
		}
		return
	}
	mark := l.notices.MarkStarted
	if notice.Kind == NoticeEnd {
		mark = l.notices.MarkEnded
	}
	if _, err := mark(ctx, trip.PrincipalID, trip.ID, now); err != nil {
		l.logger.WarnContext(ctx, "trip.notice.mark_failed", "trip_id", trip.ID, "kind", notice.Kind, "error", err)
		return
	}
	l.logger.InfoContext(ctx, "trip.notice.sent", "trip_id", trip.ID, "kind", notice.Kind)
}

// text writes the notice in plain text. The job names come from the owner's own jobs when the
// notice is sent; when they cannot be read the notice says nothing about them.
func (l *Loop) text(ctx context.Context, notice Notice) string {
	trip := notice.Trip
	names, known := l.pausedNames(ctx, trip.PrincipalID)
	if notice.Kind == NoticeEnd {
		head := fmt.Sprintf("Trip %s has ended.", trip.Title)
		if trip.Status == StatusCancelled {
			head = fmt.Sprintf("Trip %s was cancelled.", trip.Title)
		}
		if !known || len(names) == 0 {
			return head
		}
		if other := l.otherActive(ctx, trip); other != "" {
			return head + fmt.Sprintf(" Still traveling for %q, so these stay paused: %s.", other, listNames(names))
		}
		if trip.Status == StatusCancelled {
			return strings.TrimSuffix(head, ".") + "; resumed: " + listNames(names) + "."
		}
		return head + " Resumed: " + listNames(names) + "."
	}
	head := fmt.Sprintf("Trip %s to %s has started (until %s).", trip.Title, trip.Destination, trip.EndLabel("Mon 2006-01-02 15:04", "Mon 2006-01-02"))
	switch {
	case !known:
		return head
	case len(names) == 0:
		return head + " Nothing is paused."
	}
	return head + " Paused while traveling: " + listNames(names) + "."
}

// otherActive names another trip of the owner that is still under way, whose travel keeps their
// jobs paused after this one ended.
func (l *Loop) otherActive(ctx context.Context, ended Trip) string {
	active, err := l.notices.ActiveTrips(ctx, ended.PrincipalID, l.now())
	if err != nil {
		if ctx.Err() == nil {
			l.logger.WarnContext(ctx, "trip.notice.active_failed", "error", err)
		}
		return ""
	}
	for _, trip := range active {
		if trip.ID != ended.ID {
			return trip.Title
		}
	}
	return ""
}

func (l *Loop) pausedNames(ctx context.Context, principalID string) ([]string, bool) {
	var names []string
	for _, source := range l.paused {
		found, err := source.TravelPausedNames(ctx, principalID)
		if err != nil {
			if ctx.Err() == nil {
				l.logger.WarnContext(ctx, "trip.notice.paused_failed", "error", err)
			}
			return nil, false
		}
		names = append(names, found...)
	}
	slices.Sort(names)
	return slices.Compact(names), true
}

func listNames(names []string) string {
	if len(names) <= maximumListedNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maximumListedNames], ", "), len(names)-maximumListedNames)
}

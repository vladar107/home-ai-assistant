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
	// Updates checks trips near or under way for news; nil checks none.
	Updates *Updates
	// Flights follows the flights of trips on their day of travel; nil follows none.
	Flights *Flights
}

// Loop sends the start and end notices trips owe, starts the checks for news about trips, and
// prunes old trips. A notice is marked sent only after it was delivered, so a failed send is
// retried on the next cycle and a crash between the send and the mark can repeat a notice once,
// which beats losing it.
type Loop struct {
	notices  Notices
	paused   []Paused
	sender   Sender
	updates  *Updates
	flights  *Flights
	logger   *slog.Logger
	interval time.Duration
	now      func() time.Time

	cycleMu    sync.Mutex
	lastPruned time.Time

	// checkDone is closed when the running check for news ends; nil before the first.
	checkMu   sync.Mutex
	checkDone chan struct{}
	// flightDone is closed when the running flight lookup ends; flightSends counts the messages
	// being sent after one, which never hold up the next lookup.
	flightDone  chan struct{}
	flightSends sync.WaitGroup
	// lastSend is closed when the latest flight send ends; the next one waits for it.
	lastSend chan struct{}

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
	return &Loop{notices: notices, paused: paused, sender: sender, updates: cfg.Updates, flights: cfg.Flights, logger: logger, interval: interval, now: now}, nil
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
	defer l.Wait()
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

// Cycle sends every notice that is due, starts the check for news about a trip that is due unless
// one is running, and prunes once an hour. The loop runs it on every tick; it is safe to call
// directly. The check runs under ctx after Cycle returns; Wait waits for it.
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
	l.startCheck(ctx, now)
	l.startFlight(ctx, now)
	if now.Sub(l.lastPruned) >= loopPruneInterval && ctx.Err() == nil {
		l.lastPruned = now
		if err := l.notices.Prune(ctx, now); err != nil && ctx.Err() == nil {
			l.logger.WarnContext(ctx, "trip.loop.failed", "stage", "prune", "error", err)
		}
		if l.flights != nil {
			if err := l.flights.prune(ctx, now); err != nil && ctx.Err() == nil {
				l.logger.WarnContext(ctx, "trip.loop.failed", "stage", "flights_prune", "error", err)
			}
		}
	}
}

// startFlight starts the flight lookup that is due, in its own goroutine, unless one is running.
// What it found is sent from another goroutine once it is stored, since a message's rewrite into
// the trip's language is a model turn that must not hold up the next lookup.
func (l *Loop) startFlight(ctx context.Context, now time.Time) {
	if l.flights == nil || ctx.Err() != nil {
		return
	}
	l.checkMu.Lock()
	running := l.flightDone
	l.checkMu.Unlock()
	if running != nil {
		select {
		case <-running:
		default:
			return
		}
	}
	legs, job, found, err := l.flights.due(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			l.logger.WarnContext(ctx, "trip.loop.failed", "stage", "flights_due", "error", err)
		}
		return
	}
	if !found {
		return
	}
	done := make(chan struct{})
	l.checkMu.Lock()
	l.flightDone = done
	l.checkMu.Unlock()
	go func() {
		defer close(done)
		messages := l.flights.run(ctx, job, legs, now)
		if len(messages) == 0 {
			return
		}
		// Each send waits for the one before it, so a leg's messages arrive in order.
		l.checkMu.Lock()
		previous, sent := l.lastSend, make(chan struct{})
		l.lastSend = sent
		l.checkMu.Unlock()
		l.flightSends.Add(1)
		go func() {
			defer l.flightSends.Done()
			defer close(sent)
			if previous != nil {
				<-previous
			}
			// The lookup's state is stored, so what it found is not found again: its messages are
			// sent even after the loop was told to stop, within a bound.
			sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flightSendTimeout)
			defer cancel()
			l.flights.send(sendCtx, messages)
		}()
	}()
}

// startCheck starts the check for news about the trip that is due, in its own goroutine so the
// notices of later cycles are not held up by a model turn. At most one runs at a time.
func (l *Loop) startCheck(ctx context.Context, now time.Time) {
	if l.updates == nil || ctx.Err() != nil {
		return
	}
	l.checkMu.Lock()
	running := l.checkDone
	l.checkMu.Unlock()
	if running != nil {
		select {
		case <-running:
		default:
			return
		}
	}
	item, found, err := l.updates.due(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			l.logger.WarnContext(ctx, "trip.loop.failed", "stage", "updates_due", "error", err)
		}
		return
	}
	if !found {
		return
	}
	if err := l.updates.begin(ctx, item, now); err != nil {
		if ctx.Err() == nil {
			l.logger.WarnContext(ctx, "trip.loop.failed", "stage", "updates_attempt", "trip_id", item.ID, "error", err)
		}
		return
	}
	done := make(chan struct{})
	l.checkMu.Lock()
	l.checkDone = done
	l.checkMu.Unlock()
	go func() {
		defer close(done)
		l.updates.check(ctx, item, now)
	}()
}

// Wait returns once the running check for news and flight lookup, if any, have ended, and the
// flight messages being sent with them.
func (l *Loop) Wait() {
	l.checkMu.Lock()
	running, flight := l.checkDone, l.flightDone
	l.checkMu.Unlock()
	if running != nil {
		<-running
	}
	if flight != nil {
		<-flight
	}
	l.flightSends.Wait()
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

package pi

import (
	"context"
	"sync"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// maximumProcesses bounds the Pi processes running at once: each is a Node program of its own,
// and the container has one CPU and 1 GB of memory for all of them.
const maximumProcesses = 5

// processCaps bounds the kinds nobody waits on turn by turn, so a chat turn always finds a slot
// once one ends. A task can run for an hour, so tasks have their own share and never hold up
// scheduled runs and checks, and those never hold up tasks.
var processCaps = map[agentruntime.RunKind]int{
	agentruntime.RunTask:       2,
	agentruntime.RunBackground: 2,
}

// processPriority is the order in which waiting kinds get a slot that frees.
var processPriority = []agentruntime.RunKind{agentruntime.RunChat, agentruntime.RunTask, agentruntime.RunBackground}

// processKind is the label a kind is reported under.
func processKind(kind agentruntime.RunKind) string {
	switch kind {
	case agentruntime.RunChat, agentruntime.RunTask:
		return string(kind)
	default:
		return "background"
	}
}

// processLimiter hands out the right to run a Pi process: in priority order when a slot frees, and
// to the longest waiting within a kind.
type processLimiter struct {
	total int
	caps  map[agentruntime.RunKind]int
	// report receives each change of a kind's running or waiting count; nil reports nothing.
	report func(kind, state string, delta int64)

	mu      sync.Mutex
	running map[agentruntime.RunKind]int
	waiting map[agentruntime.RunKind][]chan struct{}
}

func newProcessLimiter(total int, caps map[agentruntime.RunKind]int) *processLimiter {
	return &processLimiter{
		total:   total,
		caps:    caps,
		running: map[agentruntime.RunKind]int{},
		waiting: map[agentruntime.RunKind][]chan struct{}{},
	}
}

// acquire waits for a slot for the kind and returns the function that gives it back.
func (l *processLimiter) acquire(ctx context.Context, kind agentruntime.RunKind) (func(), error) {
	switch kind {
	case agentruntime.RunChat, agentruntime.RunTask:
	default:
		kind = agentruntime.RunBackground
	}
	l.mu.Lock()
	// A waiter of another kind that fits would already have its slot, so only this kind's queue
	// can be ahead.
	if len(l.waiting[kind]) == 0 && l.fits(kind) {
		l.start(kind)
		l.mu.Unlock()
		return l.releaser(kind), nil
	}
	granted := make(chan struct{})
	l.waiting[kind] = append(l.waiting[kind], granted)
	l.changed(kind, "waiting", 1)
	l.mu.Unlock()

	select {
	case <-granted:
		return l.releaser(kind), nil
	case <-ctx.Done():
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-granted:
		// The slot arrived with the cancellation; it goes to the next waiter.
		l.finish(kind)
	default:
		queue := l.waiting[kind]
		for index, waiter := range queue {
			if waiter == granted {
				l.waiting[kind] = append(queue[:index:index], queue[index+1:]...)
				l.changed(kind, "waiting", -1)
				break
			}
		}
	}
	return nil, ctx.Err()
}

func (l *processLimiter) releaser(kind agentruntime.RunKind) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.finish(kind)
		})
	}
}

// fits reports whether a process of the kind may start now. The caller holds l.mu.
func (l *processLimiter) fits(kind agentruntime.RunKind) bool {
	total := 0
	for _, running := range l.running {
		total += running
	}
	if total >= l.total {
		return false
	}
	limit, capped := l.caps[kind]
	return !capped || l.running[kind] < limit
}

func (l *processLimiter) start(kind agentruntime.RunKind) {
	l.running[kind]++
	l.changed(kind, "running", 1)
}

// finish ends one running process of the kind and starts the waiters that now fit. The caller
// holds l.mu.
func (l *processLimiter) finish(kind agentruntime.RunKind) {
	l.running[kind]--
	l.changed(kind, "running", -1)
	for _, next := range processPriority {
		for len(l.waiting[next]) > 0 && l.fits(next) {
			granted := l.waiting[next][0]
			l.waiting[next] = l.waiting[next][1:]
			l.changed(next, "waiting", -1)
			l.start(next)
			close(granted)
		}
	}
}

func (l *processLimiter) changed(kind agentruntime.RunKind, state string, delta int64) {
	if l.report != nil {
		l.report(processKind(kind), state, delta)
	}
}

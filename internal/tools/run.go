package tools

import (
	"context"
	"regexp"
	"strings"
	"sync"
)

// maximumSeenURLs bounds what one run remembers; a run that reads more links than this can still
// open the ones it saw first.
const maximumSeenURLs = 512

var urlPattern = regexp.MustCompile(`https?://[^\s<>"'\x60]+`)

// Run is what one model run has read: whether text from outside the household or private content
// reached it, and the addresses that text and the run's own instruction named. It is created when
// the run starts and shared by every tool call of that run.
type Run struct {
	mu       sync.Mutex
	external bool
	private  bool
	seen     map[string]struct{}
}

// NewRun starts the record of a run whose trusted instruction is the given text: the user's own
// message, a confirmed task brief, or a scheduled job's prompt, never text a tool returned.
func NewRun(instruction string) *Run {
	run := &Run{seen: make(map[string]struct{})}
	run.remember(instruction)
	return run
}

type runKey struct{}

// WithRun attaches a run record to the context tools are collected and called under.
func WithRun(ctx context.Context, run *Run) context.Context {
	return context.WithValue(ctx, runKey{}, run)
}

// RunFrom returns the run record of a call, or nil when the call belongs to no run.
func RunFrom(ctx context.Context) *Run {
	run, _ := ctx.Value(runKey{}).(*Run)
	return run
}

// MarkExternal records that text from outside the household reached the run, and remembers the
// addresses it names.
func (r *Run) MarkExternal(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.external = true
	r.remember(text)
}

func (r *Run) remember(text string) {
	for _, address := range urlPattern.FindAllString(text, -1) {
		if len(r.seen) >= maximumSeenURLs {
			return
		}
		r.seen[trimURL(address)] = struct{}{}
	}
}

// External reports whether text from outside the household has reached the run.
func (r *Run) External() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.external
}

// MarkPrivate records that Notion page content or NAS file contents reached the run.
func (r *Run) MarkPrivate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.private = true
}

// Private reports whether Notion page content or NAS file contents have reached the run, so what
// the run reports carries them on.
func (r *Run) Private() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.private
}

// Seen reports whether the address appeared whole in the run's instruction or in outside text the
// run read.
func (r *Run) Seen(address string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, seen := r.seen[address]
	return seen
}

// trimURL drops punctuation that ends the sentence around an address rather than the address. A
// closing bracket stays when the address opened it, as in a wiki title such as Mercury_(planet).
func trimURL(address string) string {
	for address != "" {
		last := address[len(address)-1]
		switch {
		case strings.IndexByte(".,;:!?*", last) >= 0:
		case last == ')' && strings.Count(address, "(") < strings.Count(address, ")"):
		case last == ']' && strings.Count(address, "[") < strings.Count(address, "]"):
		case last == '}' && strings.Count(address, "{") < strings.Count(address, "}"):
		default:
			return address
		}
		address = address[:len(address)-1]
	}
	return address
}

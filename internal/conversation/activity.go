package conversation

import (
	"context"
	"hash/fnv"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	defaultTypingInterval = 4 * time.Second
	defaultDraftDelay     = 8 * time.Second
	defaultDraftInterval  = 20 * time.Second

	thinkingLabel = "Thinking…"
)

// activityLabels is what a draft says for each activity key; a tool without a known key shows
// thinkingLabel.
var activityLabels = map[string]string{
	tools.ActivityWeb:      "Searching the web…",
	tools.ActivityWeather:  "Checking the weather…",
	tools.ActivityMail:     "Reading your mail…",
	tools.ActivityCalendar: "Checking your calendar…",
	tools.ActivityFiles:    "Looking through your files…",
	tools.ActivityHome:     "Checking your home…",
	tools.ActivityTransit:  "Checking transit…",
	tools.ActivityNotion:   "Looking in Notion…",
	tools.ActivityGrafana:  "Checking the dashboards…",
	tools.ActivityPlaces:   "Looking up places nearby…",
	tools.ActivityImage:    "Drawing…",
}

// turnActivity shows a chat that its request is being worked on: typing every TypingInterval and,
// for a turn with a draft, once it has run for DraftDelay, a draft naming what it is doing. It is
// best effort: a failed call skips that tick. stop cancels any call in flight and returns only once
// the keeper's goroutine has, so nothing it sends lands after the caller moves on.
type turnActivity struct {
	service     *Service
	principalID string
	chatID      int64
	// draftID is zero for a keeper that only types, such as one waiting for the turn slot.
	draftID int64
	started time.Time

	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once

	mu      sync.Mutex
	current string
}

func (s *Service) startActivity(ctx context.Context, principalID string, chatID, draftID int64) *turnActivity {
	activityCtx, cancel := context.WithCancel(ctx)
	a := &turnActivity{
		service:     s,
		principalID: principalID,
		chatID:      chatID,
		draftID:     draftID,
		started:     s.config.Now(),
		cancel:      cancel,
		done:        make(chan struct{}),
	}
	go a.run(activityCtx)
	return a
}

// show sets the turn's current activity key, "" for thinking. It never blocks, because the
// runtime's event loop calls it; a change reaches the draft on the next typing tick, so drafts
// never go out faster than typing does.
func (a *turnActivity) show(key string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.current = key
	a.mu.Unlock()
}

func (a *turnActivity) stop() {
	if a == nil {
		return
	}
	a.stopOnce.Do(func() {
		a.cancel()
		<-a.done
	})
}

func (a *turnActivity) run(ctx context.Context) {
	defer close(a.done)
	config := a.service.config
	typing := time.NewTicker(config.TypingInterval)
	defer typing.Stop()
	a.sendTyping(ctx)

	var draftDue, refreshDue <-chan time.Time
	if a.draftID != 0 {
		delay := time.NewTimer(config.DraftDelay)
		defer delay.Stop()
		draftDue = delay.C
	}
	var refresh *time.Ticker
	defer func() {
		if refresh != nil {
			refresh.Stop()
		}
	}()
	shown := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-typing.C:
			a.sendTyping(ctx)
			if refresh != nil && a.label() != shown {
				shown = a.sendDraft(ctx)
				refresh.Reset(config.DraftInterval)
			}
		case <-draftDue:
			draftDue = nil
			shown = a.sendDraft(ctx)
			refresh = time.NewTicker(config.DraftInterval)
			refreshDue = refresh.C
		case <-refreshDue:
			shown = a.sendDraft(ctx)
		}
	}
}

func (a *turnActivity) label() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if label, known := activityLabels[a.current]; known {
		return label
	}
	return thinkingLabel
}

func (a *turnActivity) sendTyping(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	a.report(ctx, "typing", a.service.deliverer.SendTyping(ctx, a.chatID))
}

// sendDraft shows the current activity and returns the label it showed.
func (a *turnActivity) sendDraft(ctx context.Context) string {
	label := a.label()
	if ctx.Err() != nil {
		return label
	}
	text := label + " · " + elapsedText(a.service.config.Now().Sub(a.started))
	a.report(ctx, "draft", a.service.deliverer.SendMessageDraft(ctx, a.chatID, a.draftID, text, true))
	return label
}

// report counts and logs a failed call by method alone, never with the draft's text. The call is
// not retried, and one cut short by stop is not a failure.
func (a *turnActivity) report(ctx context.Context, method string, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	a.service.telemetry.RecordTurnIndicatorFailure(ctx, a.principalID, method)
	a.service.telemetry.ContextLogger(ctx, a.service.logger).WarnContext(ctx, "conversation.indicator.failed", "principal_id", a.principalID, "method", method, "error", err)
}

// elapsedText is whole seconds under a minute and whole minutes after.
func elapsedText(elapsed time.Duration) string {
	if elapsed < time.Minute {
		return strconv.Itoa(int(max(elapsed, 0)/time.Second)) + "s"
	}
	return strconv.Itoa(int(elapsed/time.Minute)) + "m"
}

// draftID derives a turn's draft from its request, so each turn has its own and a stop press can
// be matched to the turn without remembering past drafts.
func draftID(requestID string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(requestID))
	if id := int64(hash.Sum64() & math.MaxInt64); id != 0 {
		return id
	}
	return 1
}

// activityKey is the activity of a tool call: the runtime's own web search, or the key the broker
// offered the tool with.
func activityKey(tool string, activities map[string]string) string {
	if tool == webSearchTool {
		return tools.ActivityWeb
	}
	return activities[tool]
}

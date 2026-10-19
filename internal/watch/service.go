package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/localtime"
	"github.com/vladar107/home-ai-assistant/internal/module"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	moduleVersion = "0.1.0"

	DefaultTickInterval = 30 * time.Second

	historyRetention   = 30 * 24 * time.Hour
	pruneInterval      = time.Hour
	maximumNoticeRunes = 1000

	// checkSlack lets a check run when it is due within the jitter of the tick that finds it.
	checkSlack = time.Second
	// carryGap is how long before a window opens the previous day's last check may be for the
	// watch to have been watching without a break: two intervals and the slack.
	carryGap = 2*HomeCheckInterval + checkSlack

	rewriteTimeout       = 30 * time.Second
	deliveryStoreTimeout = 5 * time.Second
	// maximumMessageRunes is what one Telegram message holds.
	maximumMessageRunes = 4096
)

// Check outcomes and alert outcomes recorded in metrics.
const (
	checkNormal    = "normal"
	checkTriggered = "triggered"
	checkFailed    = "failed"

	alertDelivered     = "delivered"
	alertDropped       = "dropped"
	alertUndeliverable = "undeliverable"

	rewriteRewritten = "rewritten"
	rewriteRejected  = "rejected"
	rewriteFailed    = "failed"
	rewriteTimedOut  = "timeout"
)

// Chats resolves the principal's own private chat, the only place a watch sends to.
type Chats interface {
	ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
}

// Proposer is the core approval service as the watch service sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
	Act(context.Context, string, approval.Proposal) (string, error)
}

// Sender delivers one alert to its owner's chat with buttons that mute or pause the watch, and
// returns the sent message's ID and the tab it landed in.
type Sender interface {
	SendAlert(ctx context.Context, chatID int64, watchID, text string) (int64, int64, error)
}

// Rewriter rewrites a message the code wrote into the owner's language, with the owner's own model
// subscription.
type Rewriter interface {
	RewriteAlert(ctx context.Context, principalID, language, text string) (string, error)
}

// Telemetry records watch metrics.
type Telemetry interface {
	RecordWatchCheck(ctx context.Context, principal, kind, outcome string)
	RecordWatchAlert(ctx context.Context, principal, kind, alertType, outcome string)
	RecordWatchUpstreamError(ctx context.Context, kind, service string)
	RecordWatchRewrite(ctx context.Context, principal, outcome string)
}

type Config struct {
	// Zone is the IANA time zone a watch gets when none is named.
	Zone         string
	TickInterval time.Duration
	// CheckTimeout bounds one evaluator call.
	CheckTimeout time.Duration
	Now          func() time.Time
	// Travel is the trip lookup that watches flagged to pause while traveling consult; without it
	// nobody is ever traveling.
	Travel Travel
	// Telemetry records watch metrics.
	Telemetry Telemetry
	// Feeds are the tabs that alerts are delivered to while the bot has tabs on; without them every
	// alert goes to the main view.
	Feeds thread.Feeds
}

type Service struct {
	repository Repository
	chats      Chats
	sender     Sender
	proposer   Proposer
	rewriter   Rewriter
	telemetry  Telemetry
	travel     Travel
	feeds      thread.Feeds
	logger     *slog.Logger
	zone       string
	tick       time.Duration
	timeout    time.Duration
	now        func() time.Time
	evaluators map[string]Evaluator

	cycleMu sync.Mutex
	pruned  time.Time

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	wake     chan struct{}
	notices  map[string][]notice
	unserved map[string]bool
}

// notice is a delivered message kept for its owner's next turn. Agent watches' messages are kept
// apart, since a model wrote them from what it read outside the household.
type notice struct {
	text    string
	agent   bool
	private bool
	// messageID is the message the alert was sent as, so a reply that quotes it drops the notice.
	messageID int64
}

// Stopper is an evaluator with work of its own running between checks, which the service stops
// when it shuts down.
type Stopper interface {
	Stop(ctx context.Context) error
}

var _ module.Module = (*Service)(nil)

// NewService builds the watch service. A nil proposer leaves out the tools that create, change,
// and remove watches; /jobs still manages existing ones.
func NewService(cfg Config, repository Repository, chats Chats, sender Sender, proposer Proposer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("watch repository is required")
	}
	if chats == nil {
		return nil, fmt.Errorf("watch chats are required")
	}
	if sender == nil {
		return nil, fmt.Errorf("watch sender is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("watch logger is required")
	}
	if _, err := localtime.LoadZone(cfg.Zone); err != nil {
		return nil, fmt.Errorf("watch default zone: %w", err)
	}
	tick := cfg.TickInterval
	if tick <= 0 {
		tick = DefaultTickInterval
	}
	timeout := cfg.CheckTimeout
	if timeout <= 0 {
		timeout = defaultCheckDeadline
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		repository: repository,
		chats:      chats,
		sender:     sender,
		proposer:   proposer,
		travel:     cfg.Travel,
		feeds:      cfg.Feeds,
		telemetry:  cfg.Telemetry,
		logger:     logger,
		zone:       cfg.Zone,
		tick:       tick,
		timeout:    timeout,
		now:        now,
		evaluators: make(map[string]Evaluator),
		wake:       make(chan struct{}, 1),
		notices:    make(map[string][]notice),
		unserved:   make(map[string]bool),
	}, nil
}

// Register adds the evaluator of one kind at wiring time, before the service starts. A kind
// without one is not offered and its stored watches are not checked.
func (s *Service) Register(kind string, evaluator Evaluator) error {
	if _, known := kindTriggers[kind]; !known || evaluator == nil {
		return fmt.Errorf("register watch evaluator: unknown kind %q or no evaluator", kind)
	}
	if _, exists := s.evaluators[kind]; exists {
		return fmt.Errorf("register watch evaluator: %q is already registered", kind)
	}
	s.evaluators[kind] = evaluator
	return nil
}

// SetRewriter lets watches with a language send their messages in it, at wiring time before the
// service starts; without one they go out in English.
func (s *Service) SetRewriter(rewriter Rewriter) {
	s.rewriter = rewriter
}

// Travel reports whether a principal is on a trip at an instant.
type Travel interface {
	Traveling(ctx context.Context, principalID string, at time.Time) (bool, error)
}

// SetTravel sets the trip lookup that watches flagged to pause while traveling consult, at wiring
// time; without it nobody is ever traveling.
func (s *Service) SetTravel(travel Travel) {
	s.travel = travel
}

// travelingAt reports whether the watch's owner is on a trip at the instant, asking once per
// principal and instant within the cache's cycle. A failed lookup counts as not traveling, so an
// alert is never lost to it.
func (s *Service) travelingAt(ctx context.Context, cache *Cache, principalID, watchID string, at time.Time) bool {
	if s.travel == nil {
		return false
	}
	traveling, err := Cached(ctx, cache, "travel|"+principalID+"|"+at.UTC().Format(time.RFC3339Nano), func(ctx context.Context) (bool, error) {
		return s.travel.Traveling(ctx, principalID, at)
	})
	if err != nil {
		s.logger.WarnContext(ctx, "watch.travel.failed", "principal_id", principalID, "watch_id", watchID, "error", err)
		return false
	}
	return traveling
}

// feed is the tab of the alert's kind: Home Assistant watches have their own, and the others share one.
func feed(alert Alert) string {
	if alert.Kind == KindHome {
		return thread.FeedHome
	}
	return thread.FeedWatches
}

// inFeed returns a context whose sends go to the alert's feed tab. A tab that cannot be had leaves
// the alert in the main view, the resolver logs why, and the next alert tries again.
func (s *Service) inFeed(ctx context.Context, alert Alert, chatID int64) context.Context {
	if s.feeds == nil {
		return ctx
	}
	threadID, err := s.feeds.Thread(ctx, alert.PrincipalID, chatID, feed(alert))
	if err != nil {
		threadID = 0
	}
	return thread.WithDelivery(ctx, threadID)
}

// SetTelemetry sets where watch metrics are recorded, at wiring time before the service starts.
func (s *Service) SetTelemetry(telemetry Telemetry) {
	s.telemetry = telemetry
}

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "watch", Version: moduleVersion, Kind: module.KindSystem}
}

// Start begins the check loop. Alerts a previous process left ready are sent, or dropped once
// their window is over, by the first cycle.
func (s *Service) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		return fmt.Errorf("watch service is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go s.loop(ctx, s.done)
	return nil
}

func (s *Service) Ready(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		return fmt.Errorf("watch service is not running")
	}
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("shutdown watch service: %w", ctx.Err())
	}
	for kind, evaluator := range s.evaluators {
		if stopper, ok := evaluator.(Stopper); ok {
			if err := stopper.Stop(ctx); err != nil {
				return fmt.Errorf("shutdown %s watches: %w", kind, err)
			}
		}
	}
	return nil
}

// Wake asks for a cycle now instead of at the next tick.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		s.Cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// Cycle checks every watch that is due, sends the alerts that are ready, and expires finished
// one-off watches. The loop runs it on every tick; it is safe to call directly.
func (s *Service) Cycle(ctx context.Context) {
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()
	now := s.now()
	watches, err := s.repository.AllWatches(ctx)
	if err != nil && ctx.Err() == nil {
		s.logger.WarnContext(ctx, "watch.cycle.failed", "stage", "load_watches", "error", err)
	}
	cache := NewCache()
	for _, watch := range watches {
		if ctx.Err() != nil {
			return
		}
		s.check(ctx, watch, now, cache)
	}
	s.deliver(ctx)
	for _, watch := range watches {
		if !watch.Expired(now) || ctx.Err() != nil {
			continue
		}
		if _, err := s.repository.DeleteWatch(ctx, watch.PrincipalID, watch.ID); err != nil {
			s.logger.WarnContext(ctx, "watch.cycle.failed", "stage", "expire", "error", err)
			continue
		}
		s.logger.InfoContext(ctx, "watch.expired", "principal_id", watch.PrincipalID, "watch_id", watch.ID)
	}
	if now.Sub(s.pruned) >= pruneInterval {
		if err := s.repository.PruneWatchHistory(ctx, now.Add(-historyRetention)); err != nil && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "watch.cycle.failed", "stage", "prune", "error", err)
		} else {
			s.pruned = now
		}
	}
}

// check runs one watch's check when it is inside its window and due, and stores what changed,
// queueing at most one message.
func (s *Service) check(ctx context.Context, watch Watch, now time.Time, cache *Cache) {
	if !watch.Enabled {
		return
	}
	occurrence, active := watch.OccurrenceAt(now)
	if !active || now.Before(occurrence.Start) || now.After(occurrence.End) || watch.MutedOn == occurrence.Date {
		return
	}
	if watch.TravelPause && s.travelingAt(ctx, cache, watch.PrincipalID, watch.ID, now) {
		return
	}
	evaluator, known := s.evaluators[watch.Kind]
	if !known {
		s.mu.Lock()
		logged := s.unserved[watch.Kind]
		s.unserved[watch.Kind] = true
		s.mu.Unlock()
		if !logged {
			s.logger.WarnContext(ctx, "watch.kind.unavailable", "kind", watch.Kind)
		}
		return
	}
	state, found, err := s.repository.LoadWatchState(ctx, watch.ID, occurrence.Date)
	if err != nil {
		s.logWatchError(ctx, watch, "load_state", err)
		return
	}
	if found && now.Sub(state.CheckedAt) < interval(watch.Kind, now, occurrence.End)-checkSlack {
		return
	}
	checked := state.CheckedAt
	if !found {
		state = State{WatchID: watch.ID, PrincipalID: watch.PrincipalID, Occurrence: occurrence.Date}
		sent, err := s.repository.LastWatchAlertSequence(ctx, watch.ID, occurrence.Date)
		if err != nil {
			s.logWatchError(ctx, watch, "load_state", err)
			return
		}
		state.Messages = sent
		if watch.Kind == KindHome && watch.Start == startOfDay && watch.End == endOfDay {
			earlier, carried, err := s.repository.LatestWatchStateBefore(ctx, watch.ID, occurrence.Date)
			if err != nil {
				s.logWatchError(ctx, watch, "load_state", err)
				return
			}
			if carried && followsDirectly(earlier.Occurrence, occurrence.Date) && occurrence.Start.Sub(earlier.CheckedAt) <= carryGap {
				state.Key, state.Alerted, state.Since = earlier.Key, earlier.Alerted, earlier.Since
			}
		}
		if watch.Kind == KindAgent {
			earlier, carried, err := s.repository.LatestWatchStateBefore(ctx, watch.ID, occurrence.Date)
			if err != nil {
				s.logWatchError(ctx, watch, "load_state", err)
				return
			}
			if carried {
				state.Key, state.Alerted, state.Since, checked = earlier.Key, earlier.Alerted, earlier.Since, earlier.CheckedAt
			}
		}
	}

	occurrence.Cache, occurrence.Previous, occurrence.Since, occurrence.Checked = cache, state.Key, state.Since, checked
	checkCtx, cancel := context.WithTimeout(ctx, s.timeout)
	observation, err := evaluator.Evaluate(checkCtx, watch.PrincipalID, watch, occurrence)
	cancel()
	if ctx.Err() != nil {
		return
	}
	if errors.Is(err, ErrSkip) {
		return
	}
	if err == nil && strings.TrimSpace(observation.State) == "" {
		err = fmt.Errorf("the evaluator returned no state")
	}

	var alert *Alert
	outcome := checkNormal
	if err != nil {
		outcome = checkFailed
		state.Failures++
		var upstream *UpstreamError
		service := "none"
		if errors.As(err, &upstream) {
			service = upstream.Service
			if s.telemetry != nil {
				s.telemetry.RecordWatchUpstreamError(ctx, watch.Kind, upstream.Service)
			}
		}
		s.logger.WarnContext(ctx, "watch.check.failed", "principal_id", watch.PrincipalID, "watch_id", watch.ID,
			"kind", watch.Kind, "failures", state.Failures, "service", service, "error", err)
		if state.Failures >= FailureNoticeAfter && !state.FailureNoticed {
			state.FailureNoticed = true
			alert = queue(&state, watch, occurrence, now, alertTypeFailure, failureText(watch))
		}
	} else {
		state.Failures = 0
		if observation.Triggered {
			outcome = checkTriggered
		}
		state.Since = observation.Since
		if observation.State != state.Key {
			switch {
			case observation.Silent:
			case observation.Triggered:
				trigger := TriggerEnter
				if watch.Kind == KindAgent && state.Alerted {
					trigger = TriggerChange
				}
				if wanted(watch, trigger) {
					alert = queue(&state, watch, occurrence, now, alertTypeAlert, alertText(watch, observation))
				}
				state.Alerted = true
			case state.Alerted:
				if wanted(watch, TriggerClear) {
					alert = queue(&state, watch, occurrence, now, alertTypeRecovered, recoveredText(watch, observation))
				}
				state.Alerted = false
			}
			state.Key = observation.State
		}
		if alert != nil {
			alert.Private = observation.Private
		}
	}
	state.CheckedAt = now
	if err := s.repository.SaveWatchCheck(ctx, state, alert, now); err != nil {
		s.logWatchError(ctx, watch, "store_check", err)
		return
	}
	if s.telemetry != nil {
		s.telemetry.RecordWatchCheck(ctx, watch.PrincipalID, watch.Kind, outcome)
	}
	if alert != nil {
		s.logger.InfoContext(ctx, "watch.alert.queued", "principal_id", watch.PrincipalID, "watch_id", watch.ID, "type", alert.Type, "sequence", alert.Sequence)
	}
}

// followsDirectly reports whether date is the calendar day after earlier.
func followsDirectly(earlier, date string) bool {
	day, err := time.Parse(dateLayout, earlier)
	return err == nil && day.AddDate(0, 0, 1).Format(dateLayout) == date
}

// wanted reports whether the watch messages on enter, change, or clear. Only a home or agent watch
// can leave one of them out; the other kinds message on every change their evaluator reports.
func wanted(watch Watch, trigger string) bool {
	return !allDay(watch.Kind) || slices.Contains(watch.Triggers, trigger)
}

// interval is how long after a check the next one is due: a home watch is checked at a fixed
// pace, an agent watch's evaluator is asked every AgentPollInterval and decides itself, and the
// others are checked more often near the end of the window.
func interval(kind string, now, end time.Time) time.Duration {
	switch kind {
	case KindHome:
		return HomeCheckInterval
	case KindAgent:
		return AgentPollInterval
	}
	if end.Sub(now) <= FinalStretch {
		return FinalCheckInterval
	}
	return CheckInterval
}

func maximumMessages(kind string) int {
	if allDay(kind) {
		return MaximumHomeMessages
	}
	return MaximumMessages
}

// queue counts one more message for the window and returns it, or nil once the window has sent
// its maximum. A home or agent alert expires HomeAlertLifetime after now; the others with their
// window.
func queue(state *State, watch Watch, occurrence Occurrence, now time.Time, alertType, text string) *Alert {
	if state.Messages >= maximumMessages(watch.Kind) {
		return nil
	}
	expires := occurrence.End
	if allDay(watch.Kind) {
		expires = now.Add(HomeAlertLifetime)
	}
	state.Messages++
	digest := sha256.Sum256([]byte(watch.ID + "\x00" + occurrence.Date + "\x00" + strconv.Itoa(state.Messages)))
	return &Alert{
		ID:          hex.EncodeToString(digest[:8]),
		WatchID:     watch.ID,
		PrincipalID: watch.PrincipalID,
		Occurrence:  occurrence.Date,
		Sequence:    state.Messages,
		Type:        alertType,
		Text:        text,
		ExpiresAt:   expires,
	}
}

func alertText(watch Watch, observation Observation) string {
	lines := []string{"**⚠️ " + watch.Label() + "**", ""}
	if headline := strings.TrimSpace(observation.Headline); headline != "" {
		lines = append(lines, headline)
	}
	for _, detail := range observation.Details {
		if detail = strings.TrimSpace(detail); detail != "" {
			lines = append(lines, detail)
		}
	}
	return strings.Join(lines, "\n")
}

func recoveredText(watch Watch, observation Observation) string {
	ended := "Back to normal."
	if watch.Kind == KindAgent {
		ended = "No longer the case."
	}
	text := "**✅ " + watch.Label() + "**\n\n" + ended
	if headline := strings.TrimSpace(observation.Headline); headline != "" {
		text += " " + headline
	}
	return text
}

func failureText(watch Watch) string {
	return "**❔ " + watch.Label() + "**\n\nI can't check this right now. I'll keep trying and tell you if something goes wrong."
}

// deliver sends the ready alerts to their owners. An alert past its window, or of a watch paused
// or muted since it was queued, or of a watch that pauses while its owner travels, is dropped
// rather than sent late; a transient failure leaves it ready for the next cycle.
func (s *Service) deliver(ctx context.Context) {
	alerts, err := s.repository.ReadyWatchAlerts(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "watch.cycle.failed", "stage", "load_alerts", "error", err)
		}
		return
	}
	cache, started := NewCache(), s.now()
	for _, alert := range alerts {
		if ctx.Err() != nil {
			return
		}
		switch {
		case s.now().After(alert.ExpiresAt):
			s.finish(ctx, alert, statusDropped, "late", alertDropped)
			continue
		case !alert.Enabled:
			s.finish(ctx, alert, statusDropped, "paused", alertDropped)
			continue
		case alert.MutedOn == alert.Occurrence:
			s.finish(ctx, alert, statusDropped, "muted", alertDropped)
			continue
		case alert.TravelPause && s.travelingAt(ctx, cache, alert.PrincipalID, alert.WatchID, started):
			s.finish(ctx, alert, statusDropped, "travel_paused", alertDropped)
			continue
		}
		chatID, found, err := s.chats.ResolveActiveChat(ctx, alert.PrincipalID)
		if err != nil {
			s.logAlertError(ctx, alert, "resolve_chat", err)
			continue
		}
		if !found {
			s.finish(ctx, alert, statusDropped, "no_chat", alertDropped)
			continue
		}
		text := s.localize(ctx, alert)
		if ctx.Err() != nil {
			return
		}
		messageID, threadID, err := s.sender.SendAlert(s.inFeed(ctx, alert, chatID), chatID, alert.WatchID, text)
		if err != nil {
			if isPermanent(err) {
				s.finish(ctx, alert, statusFailed, "undeliverable", alertUndeliverable)
			} else if ctx.Err() == nil {
				s.logAlertError(ctx, alert, "deliver", err)
			}
			continue
		}
		// The alert is in the chat, so recording it outlives a shutdown that has begun; otherwise
		// the next process sends it again.
		storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryStoreTimeout)
		if s.finish(storeCtx, alert, statusDelivered, "", alertDelivered) {
			s.remember(alert, text, messageID)
			s.keepMessage(storeCtx, alert, text, messageID, threadID)
		}
		cancel()
	}
}

// localize returns the alert in its watch's language, or its English text when it has none or the
// rewrite fails, is late, or loses one of the text's numbers, so a message is never lost or held
// back by its translation.
func (s *Service) localize(ctx context.Context, alert Alert) string {
	if alert.Language == "" || s.rewriter == nil {
		return alert.Text
	}
	started := s.now()
	rewriteCtx, cancel := context.WithTimeout(ctx, rewriteTimeout)
	rewritten, err := s.rewriter.RewriteAlert(rewriteCtx, alert.PrincipalID, alert.Language, alert.Text)
	cancel()
	outcome := rewriteRewritten
	switch {
	case ctx.Err() != nil:
		return alert.Text
	case errors.Is(rewriteCtx.Err(), context.DeadlineExceeded):
		outcome = rewriteTimedOut
	case err != nil:
		outcome = rewriteFailed
	case !faithful(alert.Text, rewritten):
		outcome = rewriteRejected
	}
	if s.telemetry != nil {
		s.telemetry.RecordWatchRewrite(ctx, alert.PrincipalID, outcome)
	}
	attributes := []any{"principal_id", alert.PrincipalID, "watch_id", alert.WatchID, "outcome", outcome,
		"duration_ms", s.now().Sub(started).Milliseconds()}
	if outcome != rewriteRewritten {
		if err != nil {
			attributes = append(attributes, "error", err)
		}
		s.logger.WarnContext(ctx, "watch.alert.rewrite", attributes...)
		return alert.Text
	}
	s.logger.InfoContext(ctx, "watch.alert.rewrite", attributes...)
	return strings.TrimSpace(rewritten)
}

var numberPattern = regexp.MustCompile(`\d+`)

// faithful reports whether a rewrite can stand in for the original: it fits one message and keeps
// every number, which carry the times, delays, lines, and platforms.
func faithful(original, rewritten string) bool {
	rewritten = strings.TrimSpace(rewritten)
	if rewritten == "" || utf8.RuneCountInString(rewritten) > maximumMessageRunes {
		return false
	}
	kept := make(map[string]int)
	for _, number := range numberPattern.FindAllString(rewritten, -1) {
		kept[number]++
	}
	for _, number := range numberPattern.FindAllString(original, -1) {
		if kept[number] == 0 {
			return false
		}
		kept[number]--
	}
	return true
}

func (s *Service) finish(ctx context.Context, alert Alert, status, reason, outcome string) bool {
	finished, err := s.repository.FinishWatchAlert(ctx, alert.ID, status, reason, s.now())
	if err != nil {
		s.logAlertError(ctx, alert, "store_delivery", err)
		return false
	}
	if !finished {
		return false
	}
	if s.telemetry != nil {
		s.telemetry.RecordWatchAlert(ctx, alert.PrincipalID, alert.Kind, alert.Type, outcome)
	}
	s.logger.InfoContext(ctx, "watch.alert.finished", "principal_id", alert.PrincipalID, "watch_id", alert.WatchID,
		"type", alert.Type, "status", status, "reason", reason)
	return true
}

func (s *Service) logWatchError(ctx context.Context, watch Watch, stage string, err error) {
	if ctx.Err() == nil {
		s.logger.WarnContext(ctx, "watch.check.retry", "principal_id", watch.PrincipalID, "watch_id", watch.ID, "stage", stage, "error", err)
	}
}

func (s *Service) logAlertError(ctx context.Context, alert Alert, stage string, err error) {
	s.logger.WarnContext(ctx, "watch.alert.retry", "principal_id", alert.PrincipalID, "watch_id", alert.WatchID, "stage", stage, "error", err)
}

// remember keeps the delivered message for the owner's next conversation turn, so a reply to it has
// something to refer to.
func (s *Service) remember(alert Alert, sent string, messageID int64) {
	text := []rune(strings.TrimSpace(sent))
	if len(text) > maximumNoticeRunes {
		text = append(text[:maximumNoticeRunes], '…')
	}
	kept := notice{
		text:      fmt.Sprintf("One of the user's watches (id %s) just sent them this message:\n%s", alert.WatchID, string(text)),
		agent:     alert.Kind == KindAgent,
		private:   alert.Private,
		messageID: messageID,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	notices := append(s.notices[alert.PrincipalID], kept)
	if len(notices) > 2 {
		notices = notices[len(notices)-2:]
	}
	s.notices[alert.PrincipalID] = notices
}

// take returns and forgets the principal's kept messages of agent watches, or of the other kinds,
// and whether any of them may retell private content.
func (s *Service) take(principalID string, agent bool) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var texts []string
	var rest []notice
	private := false
	for _, kept := range s.notices[principalID] {
		if kept.agent != agent {
			rest = append(rest, kept)
			continue
		}
		texts = append(texts, kept.text)
		private = private || kept.private
	}
	if len(rest) == 0 {
		delete(s.notices, principalID)
	} else {
		s.notices[principalID] = rest
	}
	return strings.Join(texts, "\n\n"), private
}

// PendingNotice returns and forgets the messages of code-checked watches delivered since the
// principal's last turn; AgentNotices hands out those of agent watches.
func (s *Service) PendingNotice(principalID string) string {
	text, _ := s.take(principalID, false)
	return text
}

// AgentNotices is the source of the agent watches' delivered messages for the next turn. A model
// wrote them from web pages or mail, so the conversation should take them as outside content.
func (s *Service) AgentNotices() AgentNotices { return AgentNotices{service: s} }

// AgentNotices returns and forgets the agent watches' messages delivered since a principal's last
// turn.
type AgentNotices struct{ service *Service }

func (n AgentNotices) PendingNotice(principalID string) string {
	text, _ := n.TakeNotice(principalID)
	return text
}

// TakeNotice is PendingNotice that also reports whether any of the messages may retell private
// content the check read.
func (n AgentNotices) TakeNotice(principalID string) (string, bool) {
	return n.service.take(principalID, true)
}

// Watches lists the principal's own watches.
func (s *Service) Watches(ctx context.Context, principalID string) ([]Watch, error) {
	return s.repository.ListWatches(ctx, principalID)
}

// SetEnabled pauses or resumes one of the principal's watches.
func (s *Service) SetEnabled(ctx context.Context, principalID, watchID string, enabled bool) (bool, error) {
	changed, err := s.repository.SetWatchEnabled(ctx, principalID, watchID, enabled, s.now())
	if err == nil && changed {
		s.logger.InfoContext(ctx, "watch.changed", "principal_id", principalID, "watch_id", watchID, "enabled", enabled)
	}
	return changed, err
}

// SetTravelPause flags or unflags one of the principal's watches to stay silent while the
// principal is traveling.
func (s *Service) SetTravelPause(ctx context.Context, principalID, watchID string, on bool) (bool, error) {
	changed, err := s.repository.SetWatchTravelPause(ctx, principalID, watchID, on)
	if err == nil && changed {
		s.logger.InfoContext(ctx, "watch.changed", "principal_id", principalID, "watch_id", watchID, "travel_pause", on)
	}
	return changed, err
}

// TravelPausedNames returns the labels of the principal's enabled watches flagged to pause while
// traveling.
func (s *Service) TravelPausedNames(ctx context.Context, principalID string) ([]string, error) {
	watches, err := s.repository.ListWatches(ctx, principalID)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, watch := range watches {
		if watch.TravelPause && watch.Enabled {
			names = append(names, watch.Label())
		}
	}
	return names, nil
}

// Mute silences one of the principal's watches for the rest of its local day; it checks again the
// next day it runs.
func (s *Service) Mute(ctx context.Context, principalID, watchID string) (bool, error) {
	watch, found, err := s.repository.LoadWatch(ctx, principalID, watchID)
	if err != nil || !found {
		return false, err
	}
	location, err := localtime.LoadZone(watch.Zone)
	if err != nil {
		return false, err
	}
	now := s.now()
	muted, err := s.repository.MuteWatch(ctx, principalID, watchID, now.In(location).Format(dateLayout))
	if err == nil && muted {
		s.logger.InfoContext(ctx, "watch.muted", "principal_id", principalID, "watch_id", watchID)
	}
	return muted, err
}

func (s *Service) Delete(ctx context.Context, principalID, watchID string) (bool, error) {
	deleted, err := s.repository.DeleteWatch(ctx, principalID, watchID)
	if err == nil && deleted {
		s.logger.InfoContext(ctx, "watch.deleted", "principal_id", principalID, "watch_id", watchID)
	}
	return deleted, err
}

// DescribeState is a watch's days and window with its pause and mute state, for /jobs.
func (s *Service) DescribeState(watch Watch) string {
	state := watch.DescribeDays() + ", " + watch.DescribeWindow()
	if !watch.Enabled {
		return state + ", paused"
	}
	if occurrence, active := watch.OccurrenceAt(s.now()); active && watch.MutedOn == occurrence.Date {
		state += ", muted today"
	}
	if watch.TravelPause {
		state += ", pauses while traveling"
	}
	return state
}

// Available reports whether any kind of watch can be checked, so it can be offered.
func (s *Service) Available() bool { return len(s.evaluators) > 0 }

func isPermanent(err error) bool {
	var permanent interface {
		Permanent() bool
	}
	return errors.As(err, &permanent) && permanent.Permanent()
}

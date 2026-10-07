package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	moduleVersion = "0.1.0"

	DefaultTickInterval = 30 * time.Second

	historyRetention   = 30 * 24 * time.Hour
	pruneInterval      = time.Hour
	maximumNoticeRunes = 1000
)

// Check outcomes and alert outcomes recorded in metrics.
const (
	checkNormal    = "normal"
	checkTriggered = "triggered"
	checkFailed    = "failed"

	alertDelivered     = "delivered"
	alertDropped       = "dropped"
	alertUndeliverable = "undeliverable"
)

// Chats resolves the principal's own private chat, the only place a watch sends to.
type Chats interface {
	ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
}

// Proposer is the core approval service as the watch service sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

// Sender delivers one alert to its owner's chat with buttons that mute or pause the watch.
type Sender interface {
	SendAlert(ctx context.Context, chatID int64, watchID, text string) error
}

// Telemetry records watch metrics.
type Telemetry interface {
	RecordWatchCheck(ctx context.Context, principal, kind, outcome string)
	RecordWatchAlert(ctx context.Context, principal, kind, alertType, outcome string)
	RecordWatchUpstreamError(ctx context.Context, kind, service string)
}

type Config struct {
	// Zone is the IANA time zone a watch gets when none is named.
	Zone         string
	TickInterval time.Duration
	// CheckTimeout bounds one evaluator call.
	CheckTimeout time.Duration
	Now          func() time.Time
}

type Service struct {
	repository Repository
	chats      Chats
	sender     Sender
	proposer   Proposer
	telemetry  Telemetry
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
	notices  map[string][]string
	unserved map[string]bool
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
	if _, err := loadZone(cfg.Zone); err != nil {
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
		logger:     logger,
		zone:       cfg.Zone,
		tick:       tick,
		timeout:    timeout,
		now:        now,
		evaluators: make(map[string]Evaluator),
		wake:       make(chan struct{}, 1),
		notices:    make(map[string][]string),
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

// SetTelemetry sets where watch metrics are recorded, at wiring time before the service starts.
func (s *Service) SetTelemetry(telemetry Telemetry) {
	s.telemetry = telemetry
}

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "watch", Version: moduleVersion, Kind: module.KindAutomation}
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
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown watch service: %w", ctx.Err())
	}
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
	if found && now.Sub(state.CheckedAt) < interval(now, occurrence.End) {
		return
	}
	if !found {
		state = State{WatchID: watch.ID, PrincipalID: watch.PrincipalID, Occurrence: occurrence.Date}
	}

	occurrence.Cache = cache
	checkCtx, cancel := context.WithTimeout(ctx, s.timeout)
	observation, err := evaluator.Evaluate(checkCtx, watch.PrincipalID, watch, occurrence)
	cancel()
	if ctx.Err() != nil {
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
			alert = queue(&state, watch, occurrence, alertTypeFailure, failureText(watch))
		}
	} else {
		state.Failures = 0
		if observation.Triggered {
			outcome = checkTriggered
		}
		if observation.State != state.Key {
			switch {
			case observation.Triggered:
				alert = queue(&state, watch, occurrence, alertTypeAlert, alertText(watch, observation))
				state.Alerted = true
			case state.Alerted:
				alert = queue(&state, watch, occurrence, alertTypeRecovered, recoveredText(watch, observation))
				state.Alerted = false
			}
			state.Key = observation.State
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

// interval is how long after a check the next one is due: shorter near the end of the window.
func interval(now, end time.Time) time.Duration {
	if end.Sub(now) <= FinalStretch {
		return FinalCheckInterval
	}
	return CheckInterval
}

// queue counts one more message for the window and returns it, or nil once the window has sent
// its maximum.
func queue(state *State, watch Watch, occurrence Occurrence, alertType, text string) *Alert {
	if state.Messages >= MaximumMessages {
		return nil
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
		ExpiresAt:   occurrence.End,
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
	text := "**✅ " + watch.Label() + "**\n\nBack to normal."
	if headline := strings.TrimSpace(observation.Headline); headline != "" {
		text += " " + headline
	}
	return text
}

func failureText(watch Watch) string {
	return "**❔ " + watch.Label() + "**\n\nI can't check this right now. I'll keep trying and tell you if something goes wrong."
}

// deliver sends the ready alerts to their owners. An alert past its window, or of a watch paused
// or muted since it was queued, is dropped rather than sent late; a transient failure leaves it
// ready for the next cycle.
func (s *Service) deliver(ctx context.Context) {
	alerts, err := s.repository.ReadyWatchAlerts(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "watch.cycle.failed", "stage", "load_alerts", "error", err)
		}
		return
	}
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
		if err := s.sender.SendAlert(ctx, chatID, alert.WatchID, alert.Text); err != nil {
			if isPermanent(err) {
				s.finish(ctx, alert, statusFailed, "undeliverable", alertUndeliverable)
			} else if ctx.Err() == nil {
				s.logAlertError(ctx, alert, "deliver", err)
			}
			continue
		}
		if s.finish(ctx, alert, statusDelivered, "", alertDelivered) {
			s.remember(alert)
		}
	}
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

// remember keeps the delivered alert for the owner's next conversation turn, so a reply to it has
// something to refer to.
func (s *Service) remember(alert Alert) {
	text := []rune(strings.TrimSpace(alert.Text))
	if len(text) > maximumNoticeRunes {
		text = append(text[:maximumNoticeRunes], '…')
	}
	notice := fmt.Sprintf("One of the user's watches (id %s) just sent them this message:\n%s", alert.WatchID, string(text))
	s.mu.Lock()
	defer s.mu.Unlock()
	notices := append(s.notices[alert.PrincipalID], notice)
	if len(notices) > 2 {
		notices = notices[len(notices)-2:]
	}
	s.notices[alert.PrincipalID] = notices
}

// PendingNotice returns and forgets the alerts delivered since the principal's last turn.
func (s *Service) PendingNotice(principalID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	notices := s.notices[principalID]
	delete(s.notices, principalID)
	return strings.Join(notices, "\n\n")
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

// Mute silences one of the principal's watches for the rest of its local day; it checks again the
// next day it runs.
func (s *Service) Mute(ctx context.Context, principalID, watchID string) (bool, error) {
	watch, found, err := s.repository.LoadWatch(ctx, principalID, watchID)
	if err != nil || !found {
		return false, err
	}
	location, err := loadZone(watch.Zone)
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

// Available reports whether any kind of watch can be checked, so it can be offered.
func (s *Service) Available() bool { return len(s.evaluators) > 0 }

func isPermanent(err error) bool {
	var permanent interface {
		Permanent() bool
	}
	return errors.As(err, &permanent) && permanent.Permanent()
}

// Package events lets the user's own systems, such as a backup script or a home automation, message
// them. Each user keeps up to ten event sources, each with its own bearer token; an event a source
// posts to POST /api/events reaches only that source's owner, forwarded as it is or assessed by one
// model turn on the owner's own subscription. Event text is outside content wherever it goes next.
package events

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

const (
	ModeForward = "forward"
	ModeAssess  = "assess"

	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"

	OutcomePending    = "pending"
	OutcomeDelivered  = "delivered"
	OutcomeSuppressed = "suppressed"
	OutcomeFailed     = "failed"
	OutcomeDropped    = "dropped"
	OutcomeDuplicate  = "duplicate"

	// What an event's assessment decided, kept on the event so a retried delivery never assesses
	// it again.
	AssessmentNotify = "notify"
	AssessmentQuiet  = "quiet"
	AssessmentFailed = "failed"

	MaximumSources     = 10
	MaximumNameRunes   = 40
	MaximumTitleRunes  = 200
	MaximumTextRunes   = 4000
	MaximumDedupeRunes = 100
	// HourlyLimit is how many events one source may post in an hour, duplicates included.
	HourlyLimit = 60
	// Retention is how long events are kept, for the rate limit, the dedupe, and conversation.
	Retention = 30 * 24 * time.Hour
	// TokenMessageLifetime is how long the message that shows a token stays in the chat.
	TokenMessageLifetime = 10 * time.Minute

	tokenPrefix = "hae_"
	tokenBytes  = 32

	dedupeWindow = time.Hour
	// pendingLifetime is how long an event waits for a delivery that keeps failing before it is
	// dropped; it would be stale news after that.
	pendingLifetime = time.Hour
	// defaultAssessTimeout bounds an assessment well inside a turn's limit: a slow one forwards
	// the event instead of holding its owner's later events back.
	defaultAssessTimeout = 90 * time.Second
	pruneInterval        = time.Hour
	defaultTick          = time.Minute
	storeTimeout         = 10 * time.Second
	noticesKept          = 2
	maximumNoticeRunes   = 1000
	maximumMessage       = 1000
)

// tokenLength is the length of a token: the prefix and 32 bytes in unpadded base64url.
var tokenLength = len(tokenPrefix) + base64.RawURLEncoding.EncodedLen(tokenBytes)

var (
	ErrFull        = errors.New("the user already has the most event sources")
	ErrInvalidName = errors.New("invalid event source name")
	ErrNoChat      = errors.New("the user has no active chat")
)

// Source is one of a user's systems that may post events. TokenHash is the SHA-256 of its token in
// hex; the token itself is never kept.
type Source struct {
	ID          string
	PrincipalID string
	Name        string
	Mode        string
	TokenHash   string
	Enabled     bool
	CreatedAt   time.Time
	LastUsedAt  time.Time
	RotatedAt   time.Time
}

// Event is one event a source posted, with its source's name, mode, and switch as they are now.
type Event struct {
	ID          string
	PrincipalID string
	SourceID    string
	ReceivedAt  time.Time
	Title       string
	Text        string
	Severity    string
	DedupeKey   string
	Outcome     string
	// Assessment is what the event's assessment decided, or "" before it ran, and
	// AssessedMessage the message it worded.
	Assessment      string
	AssessedMessage string

	SourceName    string
	SourceMode    string
	SourceEnabled bool
}

// Input is the body of a posted event, validated.
type Input struct {
	Title     string
	Text      string
	Severity  string
	DedupeKey string
}

// Admission is what became of a posted event.
type Admission int

const (
	// Admitted events are recorded and wait for delivery.
	Admitted Admission = iota
	// Duplicated events are recorded, and so counted, but are not delivered again.
	Duplicated
	// Limited events are over the source's hourly limit and recorded nowhere.
	Limited
)

// TokenMessage is a message that showed a token, and when it is to be deleted.
type TokenMessage struct {
	PrincipalID string
	ChatID      int64
	MessageID   int64
	DeleteAt    time.Time
}

// Message is what reaches the chat: the source and severity, which the sender renders itself, and
// the title and text, which it shows as they are, never as formatting.
type Message struct {
	Source   string
	Severity string
	Title    string
	Text     string
}

type Repository interface {
	EventSources(ctx context.Context, principalID string) ([]Source, error)
	EventSource(ctx context.Context, principalID, id string) (Source, bool, error)
	EventSourceByTokenHash(ctx context.Context, hash string) (Source, bool, error)
	// AddEventSource adds the source unless the principal already keeps limit of them.
	AddEventSource(ctx context.Context, source Source, limit int) (bool, error)
	RotateEventSourceToken(ctx context.Context, principalID, id, hash string, at time.Time) (bool, error)
	SetEventSourceEnabled(ctx context.Context, principalID, id string, enabled bool) (bool, error)
	SetEventSourceMode(ctx context.Context, principalID, id, mode string) (bool, error)
	RemoveEventSource(ctx context.Context, principalID, id string) (bool, error)
	// RecordInboundEvent records the event unless its source posted limit events since the instant;
	// one whose dedupe key the source posted since then is recorded as a duplicate. It also marks
	// the source used.
	RecordInboundEvent(ctx context.Context, event Event, since time.Time, limit int) (Admission, error)
	// PendingInboundEvents lists the events that wait for delivery, oldest first.
	PendingInboundEvents(ctx context.Context) ([]Event, error)
	FinishInboundEvent(ctx context.Context, principalID, id, outcome string) (bool, error)
	// SaveInboundAssessment keeps a pending event's assessment.
	SaveInboundAssessment(ctx context.Context, principalID, id, assessment, message string) (bool, error)
	// RecentInboundEvents lists up to limit of the principal's events received since the instant,
	// newest first, of the named source alone when source is not empty.
	RecentInboundEvents(ctx context.Context, principalID, source string, since time.Time, limit int) ([]Event, error)
	PruneInboundEvents(ctx context.Context, before time.Time) error
	AddEventTokenMessage(ctx context.Context, message TokenMessage) error
	DueEventTokenMessages(ctx context.Context, at time.Time) ([]TokenMessage, error)
	RemoveEventTokenMessage(ctx context.Context, message TokenMessage) error
}

// Chats resolves the private chat of a principal who is still allowlisted and enabled.
type Chats interface {
	ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
}

// Sender is the chat as events see it.
type Sender interface {
	SendEvent(ctx context.Context, chatID int64, message Message) (int64, error)
	// SendToken shows a source's token in a message of its own and returns its ID.
	SendToken(ctx context.Context, chatID int64, source, token string) (int64, error)
	DeleteMessage(ctx context.Context, chatID, messageID int64) error
}

// Assessor runs the turn that decides whether an event is worth telling its owner about.
type Assessor interface {
	RunAssess(ctx context.Context, principalID, message string) (turn.ScheduledReply, error)
}

// Telemetry counts events by mode and outcome; no source name is ever a label.
type Telemetry interface {
	RecordInboundEvent(ctx context.Context, principal, mode, outcome string)
}

type Config struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Location is the household's time zone, the zone event times are shown in; nil means UTC.
	Location *time.Location
	// Tick is how often due token messages are deleted and failed deliveries retried; zero means
	// a minute.
	Tick time.Duration
	// BodyTimeout bounds reading a posted event's body; zero means ten seconds.
	BodyTimeout time.Duration
	// AssessTimeout bounds an assessment turn, after which the event is forwarded; zero means 90
	// seconds.
	AssessTimeout time.Duration
	// Feeds are the tabs events are delivered to while the bot has tabs on; nil delivers every
	// event to the main view.
	Feeds thread.Feeds
	// Telemetry counts events; nil counts nothing.
	Telemetry Telemetry
}

type Service struct {
	repository    Repository
	chats         Chats
	sender        Sender
	assessor      Assessor
	feeds         thread.Feeds
	telemetry     Telemetry
	logger        *slog.Logger
	now           func() time.Time
	location      *time.Location
	tick          time.Duration
	bodyTimeout   time.Duration
	assessTimeout time.Duration

	// admit serialises admissions, so the hourly limit holds under concurrent posts.
	admit sync.Mutex
	// cycle serialises cycles' housekeeping.
	cycle  sync.Mutex
	pruned time.Time
	// workers are the deliveries running, one per principal at most.
	workers sync.WaitGroup

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	wake    chan struct{}
	notices map[string][]string
	// delivering names the principals whose events a worker is delivering, and again those a
	// cycle found more events for meanwhile.
	delivering map[string]bool
	again      map[string]bool
	// decisions keeps each pending event's assessment in memory too, so a retry in this process
	// never assesses it again even when storing the decision failed.
	decisions map[string]decision
}

func NewService(cfg Config, repository Repository, chats Chats, sender Sender, assessor Assessor, logger *slog.Logger) (*Service, error) {
	switch {
	case repository == nil:
		return nil, errors.New("event repository is required")
	case chats == nil:
		return nil, errors.New("event chats are required")
	case sender == nil:
		return nil, errors.New("event sender is required")
	case assessor == nil:
		return nil, errors.New("event assessor is required")
	case logger == nil:
		return nil, errors.New("event logger is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	location := cfg.Location
	if location == nil {
		location = time.UTC
	}
	tick := cfg.Tick
	if tick <= 0 {
		tick = defaultTick
	}
	bodyTimeout := cfg.BodyTimeout
	if bodyTimeout <= 0 {
		bodyTimeout = 10 * time.Second
	}
	assessTimeout := cfg.AssessTimeout
	if assessTimeout <= 0 {
		assessTimeout = defaultAssessTimeout
	}
	return &Service{
		repository: repository, chats: chats, sender: sender, assessor: assessor,
		feeds: cfg.Feeds, telemetry: cfg.Telemetry, logger: logger,
		now: now, location: location, tick: tick, bodyTimeout: bodyTimeout, assessTimeout: assessTimeout,
		wake: make(chan struct{}, 1), notices: make(map[string][]string),
		delivering: make(map[string]bool), again: make(map[string]bool), decisions: make(map[string]decision),
	}, nil
}

// Sources lists the principal's sources, oldest first.
func (s *Service) Sources(ctx context.Context, principalID string) ([]Source, error) {
	return s.repository.EventSources(ctx, principalID)
}

// NormalizeName makes a source name one trimmed line, or reports why it cannot be one.
func NormalizeName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaximumNameRunes {
		return "", ErrInvalidName
	}
	return name, nil
}

// Create adds a source in forward mode and shows its token once, in a message deleted after
// TokenMessageLifetime. A source whose token could not be shown is removed again.
func (s *Service) Create(ctx context.Context, principalID, name string) (Source, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Source{}, err
	}
	token, hash, err := mintToken()
	if err != nil {
		return Source{}, err
	}
	id, err := newID()
	if err != nil {
		return Source{}, err
	}
	source := Source{ID: id, PrincipalID: principalID, Name: name, Mode: ModeForward, TokenHash: hash, Enabled: true, CreatedAt: s.now()}
	added, err := s.repository.AddEventSource(ctx, source, MaximumSources)
	if err != nil {
		return Source{}, err
	}
	if !added {
		return Source{}, ErrFull
	}
	if err := s.showToken(ctx, principalID, name, token); err != nil {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
		defer cancel()
		if _, removeErr := s.repository.RemoveEventSource(removeCtx, principalID, id); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
		return Source{}, err
	}
	s.logger.InfoContext(ctx, "events.source.created", "principal_id", principalID, "source_id", id)
	return source, nil
}

// Rotate gives the source a new token and shows it once; the old token stops working at once.
// False means the principal has no such source.
func (s *Service) Rotate(ctx context.Context, principalID, id string) (bool, error) {
	source, found, err := s.repository.EventSource(ctx, principalID, id)
	if err != nil || !found {
		return false, err
	}
	token, hash, err := mintToken()
	if err != nil {
		return false, err
	}
	rotated, err := s.repository.RotateEventSourceToken(ctx, principalID, id, hash, s.now())
	if err != nil || !rotated {
		return false, err
	}
	if err := s.showToken(ctx, principalID, source.Name, token); err != nil {
		return true, err
	}
	s.logger.InfoContext(ctx, "events.source.rotated", "principal_id", principalID, "source_id", id)
	return true, nil
}

// showToken sends the token to the principal's chat and records the message for deletion. A
// message whose deletion cannot be recorded is deleted at once.
func (s *Service) showToken(ctx context.Context, principalID, name, token string) error {
	chatID, found, err := s.chats.ResolveActiveChat(ctx, principalID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNoChat
	}
	messageID, err := s.sender.SendToken(ctx, chatID, name, token)
	if err != nil {
		return err
	}
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	message := TokenMessage{PrincipalID: principalID, ChatID: chatID, MessageID: messageID, DeleteAt: s.now().Add(TokenMessageLifetime)}
	if err := s.repository.AddEventTokenMessage(storeCtx, message); err != nil {
		if deleteErr := s.sender.DeleteMessage(storeCtx, chatID, messageID); deleteErr != nil {
			err = errors.Join(err, deleteErr)
		}
		return err
	}
	return nil
}

func (s *Service) SetEnabled(ctx context.Context, principalID, id string, enabled bool) (bool, error) {
	return s.repository.SetEventSourceEnabled(ctx, principalID, id, enabled)
}

func (s *Service) SetMode(ctx context.Context, principalID, id, mode string) (bool, error) {
	if mode != ModeForward && mode != ModeAssess {
		return false, fmt.Errorf("unknown event mode %q", mode)
	}
	return s.repository.SetEventSourceMode(ctx, principalID, id, mode)
}

// Delete removes the source and its events; its token stops working at once.
func (s *Service) Delete(ctx context.Context, principalID, id string) (bool, error) {
	removed, err := s.repository.RemoveEventSource(ctx, principalID, id)
	if removed {
		s.logger.InfoContext(ctx, "events.source.deleted", "principal_id", principalID, "source_id", id)
	}
	return removed, err
}

// Authenticate finds the source a bearer token belongs to. A token that names no source, or whose
// owner is no longer allowlisted or enabled, finds nothing. Sources are looked up by the token's
// SHA-256, so no comparison ever touches a stored token.
func (s *Service) Authenticate(ctx context.Context, token string) (Source, bool, error) {
	if len(token) != tokenLength || !strings.HasPrefix(token, tokenPrefix) {
		return Source{}, false, nil
	}
	hash := hashToken(token)
	source, found, err := s.repository.EventSourceByTokenHash(ctx, hash)
	if err != nil || !found {
		return Source{}, false, err
	}
	if _, active, err := s.chats.ResolveActiveChat(ctx, source.PrincipalID); err != nil || !active {
		return Source{}, false, err
	}
	return source, true, nil
}

// Accept records an event the source posted and wakes its delivery.
func (s *Service) Accept(ctx context.Context, source Source, input Input) (Event, Admission, error) {
	id, err := newID()
	if err != nil {
		return Event{}, Limited, err
	}
	s.admit.Lock()
	defer s.admit.Unlock()
	now := s.now()
	event := Event{
		ID: id, PrincipalID: source.PrincipalID, SourceID: source.ID, ReceivedAt: now,
		Title: input.Title, Text: input.Text, Severity: input.Severity, DedupeKey: input.DedupeKey,
	}
	admission, err := s.repository.RecordInboundEvent(ctx, event, now.Add(-dedupeWindow), HourlyLimit)
	if err != nil {
		return Event{}, Limited, err
	}
	switch admission {
	case Admitted:
		event.Outcome = OutcomePending
		s.Wake()
	case Duplicated:
		event.Outcome = OutcomeDuplicate
		s.record(ctx, source.PrincipalID, source.Mode, OutcomeDuplicate)
	}
	return event, admission, nil
}

// Start begins the delivery loop. Its first cycle deletes the token messages that fell due while
// the service was down and delivers the events a previous process left pending.
func (s *Service) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		return errors.New("event service is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go s.loop(ctx, s.done)
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
		return fmt.Errorf("shutdown event service: %w", ctx.Err())
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
	defer s.workers.Wait()
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

// Cycle deletes the token messages that are due, prunes old events, and hands each principal's
// pending events to a delivery of their own, so one user's slow assessment never holds another's
// events back.
func (s *Service) Cycle(ctx context.Context) {
	s.cycle.Lock()
	defer s.cycle.Unlock()
	s.deleteTokenMessages(ctx)
	now := s.now()
	if now.Sub(s.pruned) >= pruneInterval {
		if err := s.repository.PruneInboundEvents(ctx, now.Add(-Retention)); err != nil {
			if ctx.Err() == nil {
				s.logger.WarnContext(ctx, "events.prune.failed", "error", err)
			}
		} else {
			s.pruned = now
		}
	}
	pending, err := s.repository.PendingInboundEvents(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "events.cycle.failed", "stage", "load_events", "error", err)
		}
		return
	}
	seen := make(map[string]bool)
	for _, event := range pending {
		if !seen[event.PrincipalID] {
			seen[event.PrincipalID] = true
			s.startDelivery(ctx, event.PrincipalID)
		}
	}
}

// startDelivery delivers the principal's events in the background, unless a delivery of theirs is
// already running; that one then asks for another cycle when it ends.
func (s *Service) startDelivery(ctx context.Context, principalID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delivering[principalID] {
		s.again[principalID] = true
		return
	}
	s.delivering[principalID] = true
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		for _, event := range s.pendingFor(ctx, principalID) {
			if ctx.Err() != nil {
				break
			}
			s.deliver(ctx, event)
		}
		s.mu.Lock()
		delete(s.delivering, principalID)
		again := s.again[principalID]
		delete(s.again, principalID)
		s.mu.Unlock()
		if again {
			s.Wake()
		}
	}()
}

// pendingFor reads the principal's pending events once the delivery slot is held. The cycle's own
// list can be stale by then: a delivery that just ended may have sent some of them already.
func (s *Service) pendingFor(ctx context.Context, principalID string) []Event {
	pending, err := s.repository.PendingInboundEvents(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "events.cycle.failed", "stage", "load_events", "error", err)
		}
		return nil
	}
	var own []Event
	for _, event := range pending {
		if event.PrincipalID == principalID {
			own = append(own, event)
		}
	}
	return own
}

// deleteTokenMessages deletes the due token messages. One Telegram will never delete, because it is
// gone or too old, is forgotten; a transient failure leaves it for the next cycle.
func (s *Service) deleteTokenMessages(ctx context.Context) {
	due, err := s.repository.DueEventTokenMessages(ctx, s.now())
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "events.token_message.failed", "stage", "load", "error", err)
		}
		return
	}
	for _, message := range due {
		if err := s.sender.DeleteMessage(ctx, message.ChatID, message.MessageID); err != nil && !isPermanent(err) {
			if ctx.Err() == nil {
				s.logger.WarnContext(ctx, "events.token_message.failed", "principal_id", message.PrincipalID, "stage", "delete", "error", err)
			}
			continue
		}
		if err := s.repository.RemoveEventTokenMessage(ctx, message); err != nil {
			if ctx.Err() == nil {
				s.logger.WarnContext(ctx, "events.token_message.failed", "principal_id", message.PrincipalID, "stage", "forget", "error", err)
			}
			continue
		}
		s.logger.InfoContext(ctx, "events.token_message.deleted", "principal_id", message.PrincipalID)
	}
}

// deliver sends one pending event to its owner's Events feed, as it is or as its assessment
// words it. A transient failure leaves it pending for the next cycle, until it is too old to send.
func (s *Service) deliver(ctx context.Context, event Event) {
	switch {
	case s.now().Sub(event.ReceivedAt) > pendingLifetime:
		s.finish(ctx, event, OutcomeDropped, "late")
		return
	case !event.SourceEnabled:
		s.finish(ctx, event, OutcomeDropped, "paused")
		return
	}
	chatID, found, err := s.chats.ResolveActiveChat(ctx, event.PrincipalID)
	if err != nil {
		s.logRetry(ctx, event, "resolve_chat", err)
		return
	}
	if !found {
		s.finish(ctx, event, OutcomeDropped, "no_chat")
		return
	}
	message := Message{Source: event.SourceName, Severity: event.Severity, Title: event.Title, Text: event.Text}
	if event.SourceMode == ModeAssess {
		decision := s.decide(ctx, event)
		switch {
		case ctx.Err() != nil:
			return
		case decision.failed:
		case !decision.notify && event.Severity != SeverityCritical:
			s.finish(ctx, event, OutcomeSuppressed, "assessed")
			return
		case decision.message != "":
			message.Text = decision.message
		}
	}
	messageID, err := s.sender.SendEvent(s.inFeed(ctx, event.PrincipalID, chatID), chatID, message)
	if err != nil {
		if isPermanent(err) {
			s.finish(ctx, event, OutcomeFailed, "undeliverable")
		} else {
			s.logRetry(ctx, event, "deliver", err)
		}
		return
	}
	// The event is in the chat, so recording it outlives a shutdown that has begun; otherwise the
	// next process sends it again.
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if s.finish(storeCtx, event, OutcomeDelivered, "") {
		s.remember(event, message, messageID)
	}
}

// inFeed returns a context whose sends go to the Events tab. A tab that cannot be had leaves the
// event in the main view, the resolver logs why, and the next event tries again.
func (s *Service) inFeed(ctx context.Context, principalID string, chatID int64) context.Context {
	if s.feeds == nil {
		return ctx
	}
	threadID, err := s.feeds.Thread(ctx, principalID, chatID, thread.FeedEvents)
	if err != nil {
		threadID = 0
	}
	return thread.WithDelivery(ctx, threadID)
}

func (s *Service) finish(ctx context.Context, event Event, outcome, reason string) bool {
	finished, err := s.repository.FinishInboundEvent(ctx, event.PrincipalID, event.ID, outcome)
	if err != nil {
		s.logRetry(ctx, event, "store_outcome", err)
		return false
	}
	s.mu.Lock()
	delete(s.decisions, event.ID)
	s.mu.Unlock()
	if !finished {
		return false
	}
	s.record(ctx, event.PrincipalID, event.SourceMode, outcome)
	s.logger.InfoContext(ctx, "events.event.finished", "principal_id", event.PrincipalID, "source_id", event.SourceID,
		"event_id", event.ID, "outcome", outcome, "reason", reason)
	return true
}

func (s *Service) record(ctx context.Context, principalID, mode, outcome string) {
	if s.telemetry != nil {
		s.telemetry.RecordInboundEvent(ctx, principalID, mode, outcome)
	}
}

func (s *Service) logRetry(ctx context.Context, event Event, stage string, err error) {
	if ctx.Err() == nil {
		s.logger.WarnContext(ctx, "events.event.retry", "principal_id", event.PrincipalID, "source_id", event.SourceID,
			"event_id", event.ID, "stage", stage, "error", err)
	}
}

// remember keeps the delivered message for its owner's next conversation turn. Its text came from
// outside the conversation, so the turn takes it as outside content.
func (s *Service) remember(event Event, message Message, messageID int64) {
	body := strings.TrimSpace(message.Title + "\n" + message.Text)
	text := fmt.Sprintf("The user's event source %q just sent them this message (event id %s):\n%s",
		event.SourceName, event.ID, clip.Runes(body, maximumNoticeRunes))
	s.mu.Lock()
	defer s.mu.Unlock()
	notices := append(s.notices[event.PrincipalID], text)
	if len(notices) > noticesKept {
		notices = notices[len(notices)-noticesKept:]
	}
	s.notices[event.PrincipalID] = notices
}

// PendingNotice returns and forgets the events delivered since the principal's last turn.
func (s *Service) PendingNotice(principalID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := strings.Join(s.notices[principalID], "\n\n")
	delete(s.notices, principalID)
	return text
}

func isPermanent(err error) bool {
	var permanent interface {
		Permanent() bool
	}
	return errors.As(err, &permanent) && permanent.Permanent()
}

// mintToken returns a new token and the hash that is kept of it.
func mintToken() (string, string, error) {
	var value [tokenBytes]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", "", fmt.Errorf("generate event token: %w", err)
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(value[:])
	return token, hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// LooksLikeToken reports whether text carries an event token, so a pasted one never reaches the
// model.
func LooksLikeToken(text string) bool {
	for index := strings.Index(text, tokenPrefix); index >= 0; {
		candidate := text[index:]
		if len(candidate) >= tokenLength && isTokenBody(candidate[len(tokenPrefix):tokenLength]) {
			return true
		}
		next := strings.Index(text[index+1:], tokenPrefix)
		if next < 0 {
			return false
		}
		index += next + 1
	}
	return false
}

func isTokenBody(text string) bool {
	for _, r := range text {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func newID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate event identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

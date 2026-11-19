// Package assistantmail is the household's one assistant mailbox: a Gmail account that belongs to
// the assistant, with a plus-tagged address for each principal. It polls the mailbox's history and
// routes each new message to the principals it was addressed to. It reads with the mailbox's own
// OAuth client, never with a principal's Google connection.
package assistantmail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	Integration = "assistant-mail"

	accessRefreshMargin = time.Minute
	maximumJitter       = time.Minute
)

// Switch is each principal's Assistant mailbox switch, on by default for a principal the operator
// gave a tag.
var Switch = integration.Switch{Integration: Integration, Name: "Assistant mailbox", DefaultOn: true}

// ErrUnavailable means Google rejected the mailbox's refresh token, or the token reads another
// account than the configured address; nothing is read until the operator authorizes again and
// restarts.
var ErrUnavailable = errors.New("the assistant mailbox is unavailable")

// Telemetry counts polls, messages, failed token refreshes, and notices.
type Telemetry interface {
	RecordAssistantMailPoll(ctx context.Context, outcome string)
	RecordAssistantMailMessage(ctx context.Context, result string)
	RecordAssistantMailRefreshFailure(ctx context.Context, reason string)
	// RecordAssistantMailNotice counts a principal's notice of new mail by outcome: sent, failed,
	// mark_failed, no_chat, blocked, or abandoned.
	RecordAssistantMailNotice(ctx context.Context, outcome string)
}

// Chats resolves the private chat of a principal who is still allowlisted and enabled, the only
// place a notice goes.
type Chats interface {
	ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
}

// Sender delivers a notice as plain text, so nothing in it is read as formatting.
type Sender interface {
	SendPlainReply(ctx context.Context, chatID, replyToMessageID int64, text string) error
}

// TokenRefresher exchanges the mailbox's refresh token for an access token, with the mailbox's own
// OAuth client.
type TokenRefresher interface {
	Refresh(ctx context.Context, refresh string) (google.Tokens, error)
}

type Config struct {
	Mailbox    config.AssistantMailConfig
	Repository Repository
	Gmail      *gmail.Client
	Tokens     TokenRefresher
	Switches   integration.SwitchReader
	Chats      Chats
	Sender     Sender
	Logger     *slog.Logger
	// Languages gives each principal's interface language for their notices; nil writes English.
	Languages locale.Preferences
	// Location is the household's time zone, the zone mail times are shown in; nil means UTC.
	Location *time.Location
	// Notices keeps each delivered notice for the principal's next turn; nil keeps none.
	Notices *Notices
	// Proposer records sends for the user's confirmation; nil offers no send tool.
	Proposer Proposer
	// Exposure tells a send proposal that its conversation has read outside content; nil never
	// marks it.
	Exposure ExposureChecker
	// DisplayNames are the principals' full names, which the assistant signs its mail for.
	DisplayNames map[string]string
	// Now is the clock of the routes, the switch states, and the cached access token; nil means
	// time.Now.
	Now func() time.Time
	// Jitter returns the extra wait before each poll; nil draws up to a minute at random.
	Jitter func() time.Duration
}

type Service struct {
	mailbox config.AssistantMailConfig
	// base is the mailbox's address as Gmail delivers to it, which tagged addresses are matched on.
	base       string
	repository Repository
	gmail      *gmail.Client
	tokens     TokenRefresher
	switches   integration.SwitchReader
	chats      Chats
	sender     Sender
	languages  locale.Preferences
	location   *time.Location
	notices    *Notices
	proposer   Proposer
	exposure   ExposureChecker
	names      map[string]string
	logger     *slog.Logger
	telemetry  Telemetry
	now        func() time.Time
	jitter     func() time.Duration

	// rejected is set once Google refused the refresh token, and wrongAccount once the token turned
	// out to read another account than the configured address; no Google call is made after either.
	rejected     atomic.Bool
	wrongAccount atomic.Bool
	// accountChecked is set once a profile read confirmed the token reads the configured address.
	accountChecked atomic.Bool

	tokenMu sync.Mutex
	access  string
	expiry  time.Time

	// pollMu keeps the loop and a direct Poll from running at once, and guards failures,
	// relistAfter, observed, and noticeFailures.
	pollMu sync.Mutex
	// failures counts the polls in a row that failed on each message.
	failures map[string]int
	// relistAfter is the start of the last recovery's window, listed again by the next poll; zero
	// when there is none.
	relistAfter time.Time
	// observed is each tagged principal's switch as the running poll found it; a principal
	// missing from it counts as off.
	observed map[string]SwitchState
	// noticeFailures counts the polls in a row whose notice to each principal failed to deliver.
	noticeFailures map[string]int

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var _ module.Module = (*Service)(nil)

func NewService(cfg Config) (*Service, error) {
	switch {
	case !cfg.Mailbox.Enabled:
		return nil, fmt.Errorf("assistant mailbox is not enabled")
	case cfg.Mailbox.Address == "" || cfg.Mailbox.RefreshToken == "":
		return nil, fmt.Errorf("assistant mailbox address and refresh token are required")
	case cfg.Mailbox.PollInterval <= 0:
		return nil, fmt.Errorf("assistant mailbox poll interval must be positive")
	case cfg.Repository == nil:
		return nil, fmt.Errorf("assistant mailbox repository is required")
	case cfg.Gmail == nil:
		return nil, fmt.Errorf("assistant mailbox Gmail client is required")
	case cfg.Tokens == nil:
		return nil, fmt.Errorf("assistant mailbox token client is required")
	case cfg.Switches == nil:
		return nil, fmt.Errorf("assistant mailbox switches are required")
	case cfg.Chats == nil || cfg.Sender == nil:
		return nil, fmt.Errorf("assistant mailbox chats and sender are required")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("assistant mailbox logger is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	jitter := cfg.Jitter
	if jitter == nil {
		jitter = func() time.Duration { return rand.N(maximumJitter) }
	}
	location := cfg.Location
	if location == nil {
		location = time.UTC
	}
	local, domain, _ := strings.Cut(strings.ToLower(cfg.Mailbox.Address), "@")
	return &Service{
		mailbox: cfg.Mailbox, base: gmailBase(local, domain), repository: cfg.Repository, gmail: cfg.Gmail,
		tokens: cfg.Tokens, switches: cfg.Switches, chats: cfg.Chats, sender: cfg.Sender, languages: cfg.Languages,
		location: location, notices: cfg.Notices, proposer: cfg.Proposer, exposure: cfg.Exposure, names: cfg.DisplayNames, logger: cfg.Logger, telemetry: noTelemetry{}, now: now,
		jitter: jitter, failures: make(map[string]int), noticeFailures: make(map[string]int),
	}, nil
}

// SetTelemetry counts polls, messages, and failed refreshes from now on.
func (s *Service) SetTelemetry(telemetry Telemetry) {
	if telemetry != nil {
		s.telemetry = telemetry
	}
}

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: Integration, Version: "0.1.0", Kind: module.KindConnector}
}

// Start begins polling: once after the jitter, then every poll interval plus the jitter. It
// refuses while a loop a timed-out Shutdown left behind is still running.
func (s *Service) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		select {
		case <-s.done:
		default:
			return fmt.Errorf("assistant mailbox is already running")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go s.loop(ctx, s.done)
	return nil
}

func (s *Service) Ready(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel == nil {
		return fmt.Errorf("assistant mailbox is not running")
	}
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel = nil
	s.mu.Unlock()
	if done == nil {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("shutdown assistant mailbox: %w", ctx.Err())
	}
	s.tokenMu.Lock()
	s.access, s.expiry = "", time.Time{}
	s.tokenMu.Unlock()
	return nil
}

// Available reports whether the mailbox can be read: false once Google rejected its refresh token
// or the token turned out to read another account.
func (s *Service) Available() bool {
	return !s.rejected.Load() && !s.wrongAccount.Load()
}

// Unavailable says why the mailbox cannot be read until the operator acts, or "" while it can.
func (s *Service) Unavailable() string {
	switch {
	case s.wrongAccount.Load():
		return "The assistant mailbox is unavailable: its authorization is for another Google account than the configured address, so no mail is read until the operator authorizes it again and restarts."
	case s.rejected.Load():
		return "The assistant mailbox is unavailable: Google rejected its authorization, so no mail is read until the operator authorizes it again and restarts."
	}
	return ""
}

func (s *Service) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	wait := s.jitter()
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		err := s.Poll(ctx)
		if errors.Is(err, ErrUnavailable) {
			return
		}
		if err != nil && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "assistant_mail.poll.failed", "error", err)
		}
		wait = s.mailbox.PollInterval + s.jitter()
	}
}

// accessToken returns the cached access token, refreshing it within a minute of its expiry or
// when forced. A refresh token Google rejects makes the mailbox unavailable until restart.
func (s *Service) accessToken(ctx context.Context, force bool) (string, error) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if !s.Available() {
		return "", ErrUnavailable
	}
	if !force && s.access != "" && s.now().Add(accessRefreshMargin).Before(s.expiry) {
		return s.access, nil
	}
	tokens, err := s.tokens.Refresh(ctx, s.mailbox.RefreshToken)
	if err != nil {
		var googleErr *google.Error
		if errors.As(err, &googleErr) && googleErr.Class == google.ErrorInvalidGrant {
			s.rejected.Store(true)
			s.access, s.expiry = "", time.Time{}
			s.telemetry.RecordAssistantMailRefreshFailure(ctx, "rejected")
			s.logger.ErrorContext(ctx, "assistant_mail.refresh.rejected")
			return "", ErrUnavailable
		}
		s.telemetry.RecordAssistantMailRefreshFailure(ctx, "failed")
		return "", fmt.Errorf("refresh assistant mailbox token: %w", err)
	}
	// The token client dates the expiry on the wall clock; the cache compares it on the service's.
	s.access, s.expiry = tokens.Access, s.now().Add(time.Until(tokens.Expiry))
	return s.access, nil
}

// call runs one Gmail request with the access token, refreshing the token once and repeating the
// request when Gmail answers 401.
func (s *Service) call(ctx context.Context, request func(access string) error) error {
	access, err := s.accessToken(ctx, false)
	if err != nil {
		return err
	}
	err = request(access)
	var googleErr *google.Error
	if !errors.As(err, &googleErr) || googleErr.Class != google.ErrorUnauthorized {
		return err
	}
	if access, err = s.accessToken(ctx, true); err != nil {
		return err
	}
	return request(access)
}

// principals returns the principals the operator gave a tag, in order.
func (s *Service) principals() []string {
	principals := make([]string, 0, len(s.mailbox.Tags))
	for principal := range s.mailbox.Tags {
		principals = append(principals, principal)
	}
	slices.Sort(principals)
	return principals
}

// address returns the principal's current address on the mailbox, or false without a tag.
func (s *Service) address(principalID string) (string, bool) {
	tag, found := s.mailbox.CurrentTag(principalID)
	if !found {
		return "", false
	}
	local, domain, _ := strings.Cut(s.mailbox.Address, "@")
	return local + "+" + tag + "@" + domain, true
}

// Address returns the principal's current address on the mailbox, or false without a tag.
func (s *Service) Address(principalID string) (string, bool) { return s.address(principalID) }

type noTelemetry struct{}

func (noTelemetry) RecordAssistantMailPoll(context.Context, string)           {}
func (noTelemetry) RecordAssistantMailMessage(context.Context, string)        {}
func (noTelemetry) RecordAssistantMailRefreshFailure(context.Context, string) {}
func (noTelemetry) RecordAssistantMailNotice(context.Context, string)         {}

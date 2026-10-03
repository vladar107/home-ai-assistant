// Package notion is the Notion connector: a per-user token, a typed client bound to that token,
// read tools contributed to the core broker, and the connection lifecycle behind /settings.
package notion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	Integration    = "notion"
	intakeLifetime = 10 * time.Minute
	maximumNameLen = 200
)

var (
	ErrInvalidToken = errors.New("Notion rejected the token")

	tokenPattern = regexp.MustCompile(`^(ntn_|secret_)[A-Za-z0-9]+$`)
)

type Config struct {
	CredentialsDir string
	APIBaseURL     string
	ChatConnect    bool
	HTTPClient     *http.Client
	// IntakeLifetime bounds how long a Connect Notion press waits for the token; zero means ten minutes.
	IntakeLifetime time.Duration
	// DisplayNames maps principal IDs to their configured display names, whose initials pick each
	// user's score column in the movie table.
	DisplayNames map[string]string
	// Location is the household time zone that movie night dates are read in; nil means UTC.
	Location *time.Location
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Status is the non-secret view of a principal's Notion connection.
type Status struct {
	Connected     bool
	Problem       Problem
	TokenKind     string
	WorkspaceName string
	BotName       string
	WritesEnabled bool
}

type Problem string

const (
	ProblemInvalidToken Problem = "invalid_token"
	ProblemUnavailable  Problem = "unavailable"
)

type Service struct {
	client         *Client
	credentials    *integration.Credentials
	repository     integration.Repository
	proposer       Proposer
	chatConnect    bool
	intakeLifetime time.Duration
	logger         *slog.Logger
	now            func() time.Time
	displayNames   map[string]string
	location       *time.Location

	mu      sync.Mutex
	intakes map[string]time.Time
	running atomic.Bool
}

var _ module.Module = (*Service)(nil)

// NewService builds the connector; a nil proposer leaves the write tools out entirely.
func NewService(cfg Config, repository integration.Repository, proposer Proposer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("Notion repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("Notion logger is required")
	}
	credentials, err := integration.NewCredentials(cfg.CredentialsDir)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(ClientConfig{BaseURL: cfg.APIBaseURL, HTTPClient: cfg.HTTPClient})
	if err != nil {
		return nil, err
	}
	lifetime := cfg.IntakeLifetime
	if lifetime <= 0 {
		lifetime = intakeLifetime
	}
	location := cfg.Location
	if location == nil {
		location = time.UTC
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		client:         client,
		credentials:    credentials,
		repository:     repository,
		proposer:       proposer,
		chatConnect:    cfg.ChatConnect,
		intakeLifetime: lifetime,
		logger:         logger,
		now:            now,
		displayNames:   cfg.DisplayNames,
		location:       location,
		intakes:        make(map[string]time.Time),
	}, nil
}

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: Integration, Version: "0.1.0", Kind: module.KindConnector}
}

func (s *Service) Start(context.Context) error {
	s.running.Store(true)
	return nil
}

func (s *Service) Ready(context.Context) error {
	if !s.running.Load() {
		return fmt.Errorf("Notion connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	s.mu.Lock()
	s.intakes = make(map[string]time.Time)
	s.mu.Unlock()
	return nil
}

func (s *Service) ChatConnectEnabled() bool {
	return s.chatConnect
}

// LooksLikeToken reports whether a chat message is a Notion token, so it is never forwarded to
// the model or kept in the chat.
func LooksLikeToken(text string) bool {
	return tokenPattern.MatchString(strings.TrimSpace(text))
}

// Status reports the connection and adopts a token file the operator placed: the file is verified
// against Notion and the metadata row created. An invalid file is reported and left in place.
func (s *Service) Status(ctx context.Context, principalID string) (Status, error) {
	token, found, err := s.credentials.Read(principalID, Integration)
	if err != nil {
		s.logger.WarnContext(ctx, "notion.credentials.unreadable", "principal_id", principalID)
		return Status{Problem: ProblemInvalidToken}, nil
	}
	record, bound, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return Status{}, err
	}
	switch {
	case !found && bound:
		if _, err := s.repository.DeleteIntegrationConnection(ctx, principalID, Integration); err != nil {
			return Status{}, err
		}
		return Status{}, nil
	case !found:
		return Status{}, nil
	case !bound:
		record, err = s.adopt(ctx, principalID, token)
		if err != nil {
			var notionErr *Error
			if errors.As(err, &notionErr) && notionErr.Class == ErrorUnauthorized {
				return Status{Problem: ProblemInvalidToken}, nil
			}
			if errors.As(err, &notionErr) {
				return Status{Problem: ProblemUnavailable}, nil
			}
			return Status{}, err
		}
	}
	return statusOf(record), nil
}

func (s *Service) adopt(ctx context.Context, principalID, token string) (integration.Connection, error) {
	record, err := s.verify(ctx, principalID, token)
	if err != nil {
		s.logger.WarnContext(ctx, "notion.connection.adopt_failed", "principal_id", principalID)
		return integration.Connection{}, err
	}
	if err := s.repository.SaveIntegrationConnection(ctx, record); err != nil {
		return integration.Connection{}, err
	}
	s.logger.InfoContext(ctx, "notion.connection.adopted", "principal_id", principalID, "token_kind", record.TokenKind)
	return record, nil
}

func (s *Service) verify(ctx context.Context, principalID, token string) (integration.Connection, error) {
	bot, err := s.client.Me(ctx, token)
	if err != nil {
		return integration.Connection{}, err
	}
	return integration.Connection{
		PrincipalID:   principalID,
		Integration:   Integration,
		TokenKind:     bot.TokenKind(),
		WorkspaceID:   truncateRunes(bot.WorkspaceID, 128),
		WorkspaceName: truncateRunes(bot.WorkspaceName, maximumNameLen),
		BotName:       truncateRunes(bot.Name, maximumNameLen),
	}, nil
}

// Connect verifies a token the user supplied, stores it, and records the connection.
func (s *Service) Connect(ctx context.Context, principalID, token string) (Status, error) {
	token = strings.TrimSpace(token)
	if !LooksLikeToken(token) {
		return Status{}, ErrInvalidToken
	}
	record, err := s.verify(ctx, principalID, token)
	if err != nil {
		var notionErr *Error
		if errors.As(err, &notionErr) && notionErr.Class == ErrorUnauthorized {
			s.logger.InfoContext(ctx, "notion.connection.rejected", "principal_id", principalID)
			return Status{}, ErrInvalidToken
		}
		s.logger.WarnContext(ctx, "notion.connection.verify_failed", "principal_id", principalID)
		return Status{}, err
	}
	if err := s.credentials.Write(principalID, Integration, token); err != nil {
		return Status{}, err
	}
	if err := s.repository.SaveIntegrationConnection(ctx, record); err != nil {
		_, _ = s.credentials.Remove(principalID, Integration)
		return Status{}, err
	}
	s.logger.InfoContext(ctx, "notion.connection.connected", "principal_id", principalID, "token_kind", record.TokenKind)
	return statusOf(record), nil
}

func (s *Service) Disconnect(ctx context.Context, principalID string) (bool, error) {
	removed, err := s.credentials.Remove(principalID, Integration)
	if err != nil {
		return false, err
	}
	deleted, err := s.repository.DeleteIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return false, err
	}
	if removed || deleted {
		s.logger.InfoContext(ctx, "notion.connection.disconnected", "principal_id", principalID)
	}
	return removed || deleted, nil
}

// SetWrites switches the principal's write grant; it reports a disconnected status when there is
// no connection to switch.
func (s *Service) SetWrites(ctx context.Context, principalID string, enabled bool) (Status, error) {
	changed, err := s.repository.SetIntegrationWrites(ctx, principalID, Integration, enabled)
	if err != nil {
		return Status{}, err
	}
	if !changed {
		return Status{}, nil
	}
	return s.Status(ctx, principalID)
}

// BeginIntake marks the principal's next chat message as a token, replacing any pending intake.
func (s *Service) BeginIntake(principalID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intakes[principalID] = s.now().Add(s.intakeLifetime)
}

// ConsumeIntake reports whether a pending, unexpired intake existed for the principal and clears it.
func (s *Service) ConsumeIntake(principalID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, pending := s.intakes[principalID]
	if !pending {
		return false
	}
	delete(s.intakes, principalID)
	return s.now().Before(expiry)
}

// token returns the principal's token only while both the file and the metadata row exist.
func (s *Service) token(ctx context.Context, principalID string) (string, bool, error) {
	token, found, err := s.credentials.Read(principalID, Integration)
	if err != nil || !found {
		return "", false, nil
	}
	_, bound, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return "", false, err
	}
	return token, bound, nil
}

func statusOf(record integration.Connection) Status {
	return Status{
		Connected:     true,
		TokenKind:     record.TokenKind,
		WorkspaceName: record.WorkspaceName,
		BotName:       record.BotName,
		WritesEnabled: record.WritesEnabled,
	}
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit])
}

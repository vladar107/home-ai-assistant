// Package gmail is the Gmail connector: each principal connects their own mailbox with the
// read-only Gmail scope through a sign-in they complete from the chat, and their turns can then
// search it and read a message's text. Nothing here can send, draft, label, or delete mail,
// because the credential cannot.
package gmail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	Integration          = "gmail"
	intakeLifetime       = 10 * time.Minute
	accessRefreshMargin  = time.Minute
	maximumAddressLength = 128
)

var (
	// ErrNoSignIn means no sign-in is waiting for this principal, or it expired.
	ErrNoSignIn = errors.New("no Gmail sign-in is pending")
	// ErrNotRedirect means the message does not carry an authorization code.
	ErrNotRedirect = errors.New("the message is not a Gmail sign-in address")
	// ErrDenied means the user declined, or did not grant read access to Gmail.
	ErrDenied = errors.New("Gmail access was not granted")
	// ErrRejected means Google refused the authorization code.
	ErrRejected = errors.New("Google refused the sign-in")
	// ErrMailboxTaken means another principal has already connected this mailbox.
	ErrMailboxTaken = errors.New("the mailbox is connected to another user")

	redirectPattern = regexp.MustCompile(`https?://(localhost|127\.0\.0\.1)(:[0-9]+)?/\S*`)
)

type Config struct {
	CredentialsDir string
	ClientID       string
	ClientSecret   string
	RedirectURL    string
	AuthURL        string
	TokenURL       string
	RevokeURL      string
	APIBaseURL     string
	HTTPClient     *http.Client
	// IntakeLifetime bounds how long a sign-in link stays usable; zero means ten minutes.
	IntakeLifetime time.Duration
}

// Accounts finds which principal already holds a connection to a given account.
type Accounts interface {
	IntegrationAccountOwner(ctx context.Context, integration, accountID string) (string, bool, error)
}

// Status is the non-secret view of a principal's Gmail connection.
type Status struct {
	Connected bool
	// NeedsReconnect means Google no longer accepts the stored grant.
	NeedsReconnect bool
	Address        string
}

type signIn struct {
	state    string
	verifier string
	expires  time.Time
}

type accessToken struct {
	refresh string
	access  string
	expiry  time.Time
}

type Service struct {
	client         *Client
	credentials    *integration.Credentials
	repository     integration.Repository
	accounts       Accounts
	intakeLifetime time.Duration
	logger         *slog.Logger
	now            func() time.Time

	mu       sync.Mutex
	signIns  map[string]signIn
	tokens   map[string]accessToken
	rejected map[string]string
	running  atomic.Bool
}

var _ module.Module = (*Service)(nil)

func NewService(cfg Config, repository integration.Repository, accounts Accounts, logger *slog.Logger) (*Service, error) {
	if repository == nil || accounts == nil {
		return nil, fmt.Errorf("Gmail repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("Gmail logger is required")
	}
	credentials, err := integration.NewCredentials(cfg.CredentialsDir)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(ClientConfig{
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
		AuthURL: cfg.AuthURL, TokenURL: cfg.TokenURL, RevokeURL: cfg.RevokeURL, APIBaseURL: cfg.APIBaseURL,
		HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	lifetime := cfg.IntakeLifetime
	if lifetime <= 0 {
		lifetime = intakeLifetime
	}
	return &Service{
		client:         client,
		credentials:    credentials,
		repository:     repository,
		accounts:       accounts,
		intakeLifetime: lifetime,
		logger:         logger,
		now:            time.Now,
		signIns:        make(map[string]signIn),
		tokens:         make(map[string]accessToken),
		rejected:       make(map[string]string),
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
		return fmt.Errorf("Gmail connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	s.mu.Lock()
	s.signIns = make(map[string]signIn)
	s.tokens = make(map[string]accessToken)
	s.mu.Unlock()
	return nil
}

// Status reports the connection from the stored grant and its metadata row; it does not call
// Google, so /settings stays fast. A grant Google has rejected since shows as needing a reconnect.
func (s *Service) Status(ctx context.Context, principalID string) (Status, error) {
	refresh, found, err := s.credentials.Read(principalID, Integration)
	if err != nil {
		s.logger.WarnContext(ctx, "gmail.credentials.unreadable", "principal_id", principalID)
		return Status{NeedsReconnect: true}, nil
	}
	record, bound, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return Status{}, err
	}
	if !found || !bound {
		return Status{}, nil
	}
	s.mu.Lock()
	rejected := s.rejected[principalID] == refresh
	s.mu.Unlock()
	return Status{Connected: !rejected, NeedsReconnect: rejected, Address: record.WorkspaceName}, nil
}

// BeginSignIn starts a sign-in for the principal and returns the consent link, replacing any
// sign-in already pending for them.
func (s *Service) BeginSignIn(principalID string) (string, error) {
	state, err := randomString(16)
	if err != nil {
		return "", err
	}
	verifier, err := randomString(48)
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	s.mu.Lock()
	s.signIns[principalID] = signIn{state: state, verifier: verifier, expires: s.now().Add(s.intakeLifetime)}
	s.mu.Unlock()
	return s.client.AuthorizationURL(state, base64.RawURLEncoding.EncodeToString(challenge[:])), nil
}

// LooksLikeRedirect reports whether a chat message carries a loopback address with an
// authorization code, so it is never forwarded to the model or kept in the chat.
func LooksLikeRedirect(text string) bool {
	for _, candidate := range redirectPattern.FindAllString(text, -1) {
		parsed, err := url.Parse(candidate)
		if err != nil {
			continue
		}
		query := parsed.Query()
		if query.Get("code") != "" || query.Get("error") != "" {
			return true
		}
	}
	return false
}

// CompleteSignIn takes the address the browser was sent to after consent, checks it belongs to
// this principal's pending sign-in, and stores the grant. The pending sign-in is used up either
// way, so a code can be tried once.
func (s *Service) CompleteSignIn(ctx context.Context, principalID, pasted string) (Status, error) {
	code, state, denied, ok := parseRedirect(pasted)
	if !ok {
		return Status{}, ErrNotRedirect
	}
	s.mu.Lock()
	pending, exists := s.signIns[principalID]
	delete(s.signIns, principalID)
	s.mu.Unlock()
	if !exists || !s.now().Before(pending.expires) || state != pending.state {
		s.logger.InfoContext(ctx, "gmail.signin.unmatched", "principal_id", principalID)
		return Status{}, ErrNoSignIn
	}
	if denied {
		return Status{}, ErrDenied
	}

	tokens, err := s.client.Exchange(ctx, code, pending.verifier)
	if err != nil {
		var gmailErr *Error
		if errors.As(err, &gmailErr) && (gmailErr.Class == ErrorInvalidGrant || gmailErr.Class == ErrorInvalid) {
			s.logger.InfoContext(ctx, "gmail.signin.rejected", "principal_id", principalID)
			return Status{}, ErrRejected
		}
		return Status{}, err
	}
	// Google's consent screen lets the user untick the Gmail box, which still returns a code.
	if !slices.Contains(tokens.Scopes, Scope) || tokens.Refresh == "" {
		s.revoke(ctx, principalID, tokens.Access)
		return Status{}, ErrDenied
	}
	address, err := s.client.Profile(ctx, tokens.Access)
	if err != nil {
		s.revoke(ctx, principalID, tokens.Refresh)
		return Status{}, err
	}
	address = strings.ToLower(strings.TrimSpace(address))
	if !utf8.ValidString(address) || len(address) > maximumAddressLength {
		s.revoke(ctx, principalID, tokens.Refresh)
		return Status{}, &Error{Class: ErrorUnavailable}
	}
	owner, taken, err := s.accounts.IntegrationAccountOwner(ctx, Integration, address)
	if err != nil {
		return Status{}, err
	}
	if taken && owner != principalID {
		s.revoke(ctx, principalID, tokens.Refresh)
		s.logger.InfoContext(ctx, "gmail.signin.taken", "principal_id", principalID)
		return Status{}, ErrMailboxTaken
	}

	if err := s.credentials.Write(principalID, Integration, tokens.Refresh); err != nil {
		return Status{}, err
	}
	record := integration.Connection{
		PrincipalID:   principalID,
		Integration:   Integration,
		TokenKind:     integration.TokenKindPersonal,
		WorkspaceID:   address,
		WorkspaceName: address,
	}
	if err := s.repository.SaveIntegrationConnection(ctx, record); err != nil {
		_, _ = s.credentials.Remove(principalID, Integration)
		return Status{}, err
	}
	s.mu.Lock()
	s.tokens[principalID] = accessToken{refresh: tokens.Refresh, access: tokens.Access, expiry: tokens.Expiry}
	delete(s.rejected, principalID)
	s.mu.Unlock()
	s.logger.InfoContext(ctx, "gmail.connection.connected", "principal_id", principalID)
	return Status{Connected: true, Address: address}, nil
}

// parseRedirect finds the loopback address in a message and returns its code and state, or
// reports that the user declined.
func parseRedirect(text string) (code, state string, denied, ok bool) {
	for _, candidate := range redirectPattern.FindAllString(text, -1) {
		parsed, err := url.Parse(candidate)
		if err != nil {
			continue
		}
		query := parsed.Query()
		state = query.Get("state")
		if query.Get("error") != "" {
			return "", state, true, true
		}
		if code = query.Get("code"); code != "" {
			return code, state, false, true
		}
	}
	return "", "", false, false
}

// Disconnect deletes the grant here and revokes it at Google; a revocation that fails is logged,
// and the grant is gone from this system either way.
func (s *Service) Disconnect(ctx context.Context, principalID string) (bool, error) {
	refresh, found, _ := s.credentials.Read(principalID, Integration)
	removed, err := s.credentials.Remove(principalID, Integration)
	if err != nil {
		return false, err
	}
	deleted, err := s.repository.DeleteIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	delete(s.tokens, principalID)
	delete(s.rejected, principalID)
	delete(s.signIns, principalID)
	s.mu.Unlock()
	if found {
		s.revoke(ctx, principalID, refresh)
	}
	if removed || deleted {
		s.logger.InfoContext(ctx, "gmail.connection.disconnected", "principal_id", principalID)
	}
	return removed || deleted, nil
}

func (s *Service) revoke(ctx context.Context, principalID, token string) {
	if token == "" {
		return
	}
	if err := s.client.Revoke(ctx, token); err != nil {
		s.logger.WarnContext(ctx, "gmail.revoke.failed", "principal_id", principalID, "error", err)
	}
}

var errNotConnected = errors.New("Gmail is not connected")

// access returns a usable access token for the principal, refreshing it when it is about to
// expire. Both the stored grant and its metadata row must exist, so a disconnect during a turn
// fails closed. A refresh token Google rejects is remembered until the user reconnects.
func (s *Service) access(ctx context.Context, principalID string, forceRefresh bool) (string, error) {
	refresh, found, err := s.credentials.Read(principalID, Integration)
	if err != nil || !found {
		return "", errNotConnected
	}
	if _, bound, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration); err != nil {
		return "", err
	} else if !bound {
		return "", errNotConnected
	}
	s.mu.Lock()
	cached := s.tokens[principalID]
	rejected := s.rejected[principalID] == refresh
	s.mu.Unlock()
	if rejected {
		return "", errNotConnected
	}
	if !forceRefresh && cached.refresh == refresh && s.now().Add(accessRefreshMargin).Before(cached.expiry) {
		return cached.access, nil
	}
	tokens, err := s.client.Refresh(ctx, refresh)
	if err != nil {
		var gmailErr *Error
		if errors.As(err, &gmailErr) && gmailErr.Class == ErrorInvalidGrant {
			s.mu.Lock()
			s.rejected[principalID] = refresh
			delete(s.tokens, principalID)
			s.mu.Unlock()
			s.logger.WarnContext(ctx, "gmail.grant.rejected", "principal_id", principalID)
			return "", errNotConnected
		}
		return "", err
	}
	s.mu.Lock()
	s.tokens[principalID] = accessToken{refresh: refresh, access: tokens.Access, expiry: tokens.Expiry}
	s.mu.Unlock()
	return tokens.Access, nil
}

func randomString(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate Gmail sign-in secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

const secretFileSizeLimit = 4 << 10

// LoadClientSecretFile reads the OAuth client secret from a file only its owner can read.
func LoadClientSecretFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("Gmail client secret file metadata could not be read")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > secretFileSizeLimit {
		return "", fmt.Errorf("Gmail client secret file must be a small regular file without group or other permissions")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("Gmail client secret file could not be read")
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" || !utf8.ValidString(secret) || strings.ContainsAny(secret, " \t\r\n") {
		return "", fmt.Errorf("Gmail client secret file does not contain a secret")
	}
	return secret, nil
}

// Package google holds each principal's Google accounts: the sign-in they complete from the chat,
// the grant of each account, which products they switched on for it, and access tokens. The Gmail
// and Calendar connectors ask it for accounts and tokens; neither holds a credential itself.
package google

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
	Integration          = "google"
	intakeLifetime       = 10 * time.Minute
	accessRefreshMargin  = time.Minute
	maximumAddressLength = 128
	// legacyIntegration is 0025's single Gmail connection, whose grant is revoked at startup.
	legacyIntegration = "gmail"
)

var (
	// ErrNoSignIn means no sign-in is waiting for this principal, or it expired.
	ErrNoSignIn = errors.New("no Google sign-in is pending")
	// ErrNotRedirect means the message does not carry an authorization code.
	ErrNotRedirect = errors.New("the message is not a Google sign-in address")
	// ErrDenied means the user declined, or granted neither Gmail nor Calendar.
	ErrDenied = errors.New("Google access was not granted")
	// ErrRejected means Google refused the authorization code.
	ErrRejected = errors.New("Google refused the sign-in")
	// ErrAccountTaken means another principal has already connected this Google account.
	ErrAccountTaken = errors.New("the Google account is connected to another user")
	// ErrNotGranted means the product cannot be switched on because its grant does not cover it.
	ErrNotGranted = errors.New("the Google account did not grant this product")

	redirectPattern = regexp.MustCompile(`https?://(localhost|127\.0\.0\.1)(:[0-9]+)?/\S*`)
	subjectPattern  = regexp.MustCompile(`^[0-9]{1,40}$`)
)

type Config struct {
	CredentialsDir string
	ClientID       string
	ClientSecret   string
	RedirectURL    string
	AuthURL        string
	TokenURL       string
	RevokeURL      string
	UserInfoURL    string
	HTTPClient     *http.Client
	// IntakeLifetime bounds how long a sign-in link stays usable; zero means ten minutes.
	IntakeLifetime time.Duration
}

// AccountStatus is the non-secret view of one connected account for /settings.
type AccountStatus struct {
	Account
	MailGranted     bool
	CalendarGranted bool
	MailEnabled     bool
	CalendarEnabled bool
	// NeedsReconnect means Google no longer accepts the stored grant, or it is missing.
	NeedsReconnect bool
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
	credentialsDir string
	accounts       integration.GoogleAccounts
	intakeLifetime time.Duration
	logger         *slog.Logger
	now            func() time.Time

	mu      sync.Mutex
	signIns map[string]signIn
	// tokens and rejected are keyed by account id, which belongs to one principal only.
	tokens   map[string]accessToken
	rejected map[string]string
	running  atomic.Bool
}

var _ module.Module = (*Service)(nil)

func NewService(cfg Config, accounts integration.GoogleAccounts, logger *slog.Logger) (*Service, error) {
	if accounts == nil {
		return nil, fmt.Errorf("Google accounts repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("Google logger is required")
	}
	credentials, err := integration.NewCredentials(cfg.CredentialsDir)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(ClientConfig{
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
		AuthURL: cfg.AuthURL, TokenURL: cfg.TokenURL, RevokeURL: cfg.RevokeURL, UserInfoURL: cfg.UserInfoURL,
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
		credentialsDir: cfg.CredentialsDir,
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
	return module.Descriptor{ID: Integration, Version: "0.2.0", Kind: module.KindConnector}
}

func (s *Service) Start(ctx context.Context) error {
	s.removeLegacyGrants(ctx)
	s.running.Store(true)
	return nil
}

func (s *Service) Ready(context.Context) error {
	if !s.running.Load() {
		return fmt.Errorf("Google connector is not running")
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

// removeLegacyGrants revokes and deletes the single Gmail grant 0025 stored per principal; its
// metadata row is removed by the migration that introduced Google accounts.
func (s *Service) removeLegacyGrants(ctx context.Context) {
	entries, err := os.ReadDir(s.credentialsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		principalID := entry.Name()
		token, found, err := s.credentials.Read(principalID, legacyIntegration)
		if err != nil || !found {
			continue
		}
		s.revoke(ctx, principalID, token)
		if _, err := s.credentials.Remove(principalID, legacyIntegration); err != nil {
			s.logger.WarnContext(ctx, "google.legacy_grant.remove_failed", "principal_id", principalID, "error", err)
			continue
		}
		s.logger.InfoContext(ctx, "google.legacy_grant.removed", "principal_id", principalID)
	}
}

// Status lists the principal's accounts from their records and stored grants; it does not call
// Google, so /settings stays fast. A grant Google has rejected since shows as needing a reconnect.
func (s *Service) Status(ctx context.Context, principalID string) ([]AccountStatus, error) {
	records, err := s.accounts.ListGoogleAccounts(ctx, principalID)
	if err != nil {
		return nil, err
	}
	statuses := make([]AccountStatus, 0, len(records))
	for _, record := range records {
		status := AccountStatus{
			Account:         Account{ID: record.AccountID, Address: record.Address},
			MailGranted:     record.MailGranted,
			CalendarGranted: record.CalendarGranted,
			MailEnabled:     record.MailEnabled,
			CalendarEnabled: record.CalendarEnabled,
		}
		refresh, found, err := s.credentials.Read(principalID, credentialName(record.AccountID))
		if err != nil {
			s.logger.WarnContext(ctx, "google.credentials.unreadable", "principal_id", principalID)
		}
		s.mu.Lock()
		rejected := found && s.rejected[record.AccountID] == refresh
		s.mu.Unlock()
		status.NeedsReconnect = err != nil || !found || rejected
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// Accounts lists the principal's accounts that the product can use now: granted, switched on, and
// with a grant Google is not known to have rejected.
func (s *Service) Accounts(ctx context.Context, principalID string, product Product) ([]Account, error) {
	statuses, err := s.Status(ctx, principalID)
	if err != nil {
		return nil, err
	}
	var accounts []Account
	for _, status := range statuses {
		if status.usable(product) {
			accounts = append(accounts, status.Account)
		}
	}
	return accounts, nil
}

func (s AccountStatus) usable(product Product) bool {
	if s.NeedsReconnect {
		return false
	}
	switch product {
	case ProductMail:
		return s.MailGranted && s.MailEnabled
	case ProductCalendar:
		return s.CalendarGranted && s.CalendarEnabled
	default:
		return false
	}
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
// this principal's pending sign-in, and stores the account's grant. The pending sign-in is used up
// either way, so a code can be tried once.
//
// Google keeps one grant per account and OAuth client, so revoking any of its tokens ends it for
// whoever else holds it. A grant is therefore revoked here only when no principal has the account.
func (s *Service) CompleteSignIn(ctx context.Context, principalID, pasted string) (AccountStatus, error) {
	code, state, denied, ok := parseRedirect(pasted)
	if !ok {
		return AccountStatus{}, ErrNotRedirect
	}
	s.mu.Lock()
	pending, exists := s.signIns[principalID]
	delete(s.signIns, principalID)
	s.mu.Unlock()
	if !exists || !s.now().Before(pending.expires) || state != pending.state {
		s.logger.InfoContext(ctx, "google.signin.unmatched", "principal_id", principalID)
		return AccountStatus{}, ErrNoSignIn
	}
	if denied {
		return AccountStatus{}, ErrDenied
	}

	tokens, err := s.client.Exchange(ctx, code, pending.verifier)
	if err != nil {
		var googleErr *Error
		if errors.As(err, &googleErr) && (googleErr.Class == ErrorInvalidGrant || googleErr.Class == ErrorInvalid) {
			s.logger.InfoContext(ctx, "google.signin.rejected", "principal_id", principalID)
			return AccountStatus{}, ErrRejected
		}
		return AccountStatus{}, err
	}
	identity, err := s.client.UserInfo(ctx, tokens.Access)
	if err != nil {
		return AccountStatus{}, err
	}
	address := strings.ToLower(strings.TrimSpace(identity.Email))
	if !subjectPattern.MatchString(identity.Subject) || address == "" || !utf8.ValidString(address) || len(address) > maximumAddressLength {
		return AccountStatus{}, &Error{Class: ErrorUnavailable}
	}
	owner, taken, err := s.accounts.GoogleAccountOwner(ctx, identity.Subject)
	if err != nil {
		return AccountStatus{}, err
	}
	if taken && owner != principalID {
		s.logger.InfoContext(ctx, "google.signin.taken", "principal_id", principalID)
		return AccountStatus{}, ErrAccountTaken
	}
	// Google's consent screen lets the user untick each product, which still returns a code.
	mailGranted := slices.Contains(tokens.Scopes, MailScope)
	calendarGranted := slices.Contains(tokens.Scopes, CalendarScope)
	if (!mailGranted && !calendarGranted) || tokens.Refresh == "" {
		if !taken {
			s.revoke(ctx, principalID, tokens.Access)
		}
		return AccountStatus{}, ErrDenied
	}

	record := integration.GoogleAccount{
		PrincipalID:     principalID,
		AccountID:       identity.Subject,
		Address:         address,
		MailGranted:     mailGranted,
		CalendarGranted: calendarGranted,
		MailEnabled:     mailGranted,
		CalendarEnabled: calendarGranted,
	}
	if previous, found, err := s.account(ctx, principalID, identity.Subject); err != nil {
		return AccountStatus{}, err
	} else if found {
		// A reconnect keeps a switch the user turned off, unless the product was not granted before.
		record.MailEnabled = mailGranted && (previous.MailEnabled || !previous.MailGranted)
		record.CalendarEnabled = calendarGranted && (previous.CalendarEnabled || !previous.CalendarGranted)
	}
	if err := s.credentials.Write(principalID, credentialName(identity.Subject), tokens.Refresh); err != nil {
		return AccountStatus{}, err
	}
	if err := s.accounts.SaveGoogleAccount(ctx, record); err != nil {
		if !taken {
			_, _ = s.credentials.Remove(principalID, credentialName(identity.Subject))
		}
		return AccountStatus{}, err
	}
	s.mu.Lock()
	s.tokens[identity.Subject] = accessToken{refresh: tokens.Refresh, access: tokens.Access, expiry: tokens.Expiry}
	delete(s.rejected, identity.Subject)
	s.mu.Unlock()
	s.logger.InfoContext(ctx, "google.account.connected", "principal_id", principalID,
		"mail", mailGranted, "calendar", calendarGranted)
	return AccountStatus{
		Account:     Account{ID: identity.Subject, Address: address},
		MailGranted: mailGranted, CalendarGranted: calendarGranted,
		MailEnabled: record.MailEnabled, CalendarEnabled: record.CalendarEnabled,
	}, nil
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

// SetProduct switches a product on or off for one of the principal's accounts and reports whether
// the account exists; a product the grant does not cover cannot be switched on.
func (s *Service) SetProduct(ctx context.Context, principalID, accountID string, product Product, enabled bool) (bool, error) {
	record, found, err := s.account(ctx, principalID, accountID)
	if err != nil || !found {
		return false, err
	}
	switch product {
	case ProductMail:
		if enabled && !record.MailGranted {
			return true, ErrNotGranted
		}
		record.MailEnabled = enabled
	case ProductCalendar:
		if enabled && !record.CalendarGranted {
			return true, ErrNotGranted
		}
		record.CalendarEnabled = enabled
	default:
		return false, fmt.Errorf("unknown Google product %q", product)
	}
	if err := s.accounts.SaveGoogleAccount(ctx, record); err != nil {
		return true, err
	}
	s.logger.InfoContext(ctx, "google.account.product_switched", "principal_id", principalID,
		"product", string(product), "enabled", enabled)
	return true, nil
}

// Disconnect deletes one account's grant here and revokes it at Google; a revocation that fails is
// logged, and the grant is gone from this system either way.
func (s *Service) Disconnect(ctx context.Context, principalID, accountID string) (bool, error) {
	if !subjectPattern.MatchString(accountID) {
		return false, nil
	}
	if _, found, err := s.account(ctx, principalID, accountID); err != nil || !found {
		return false, err
	}
	name := credentialName(accountID)
	refresh, found, _ := s.credentials.Read(principalID, name)
	removed, err := s.credentials.Remove(principalID, name)
	if err != nil {
		return false, err
	}
	deleted, err := s.accounts.DeleteGoogleAccount(ctx, principalID, accountID)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	delete(s.tokens, accountID)
	delete(s.rejected, accountID)
	s.mu.Unlock()
	if found {
		s.revoke(ctx, principalID, refresh)
	}
	if removed || deleted {
		s.logger.InfoContext(ctx, "google.account.disconnected", "principal_id", principalID)
	}
	return removed || deleted, nil
}

func (s *Service) account(ctx context.Context, principalID, accountID string) (integration.GoogleAccount, bool, error) {
	records, err := s.accounts.ListGoogleAccounts(ctx, principalID)
	if err != nil {
		return integration.GoogleAccount{}, false, err
	}
	for _, record := range records {
		if record.AccountID == accountID {
			return record, true, nil
		}
	}
	return integration.GoogleAccount{}, false, nil
}

func (s *Service) revoke(ctx context.Context, principalID, token string) {
	if token == "" {
		return
	}
	if err := s.client.Revoke(ctx, token); err != nil {
		s.logger.WarnContext(ctx, "google.revoke.failed", "principal_id", principalID, "error", err)
	}
}

// AccessToken returns a usable access token for one of the principal's accounts, refreshing it
// when it is about to expire. The account must be usable for the product right now, so a
// disconnect or a switch turned off during a turn fails closed. A refresh token Google rejects is
// remembered until the user reconnects.
func (s *Service) AccessToken(ctx context.Context, principalID, accountID string, product Product, forceRefresh bool) (string, error) {
	accounts, err := s.Accounts(ctx, principalID, product)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(accounts, func(account Account) bool { return account.ID == accountID }) {
		return "", ErrNotConnected
	}
	refresh, found, err := s.credentials.Read(principalID, credentialName(accountID))
	if err != nil || !found {
		return "", ErrNotConnected
	}
	s.mu.Lock()
	cached := s.tokens[accountID]
	s.mu.Unlock()
	if !forceRefresh && cached.refresh == refresh && s.now().Add(accessRefreshMargin).Before(cached.expiry) {
		return cached.access, nil
	}
	tokens, err := s.client.Refresh(ctx, refresh)
	if err != nil {
		var googleErr *Error
		if errors.As(err, &googleErr) && googleErr.Class == ErrorInvalidGrant {
			s.mu.Lock()
			s.rejected[accountID] = refresh
			delete(s.tokens, accountID)
			s.mu.Unlock()
			s.logger.WarnContext(ctx, "google.grant.rejected", "principal_id", principalID)
			return "", ErrNotConnected
		}
		return "", err
	}
	s.mu.Lock()
	s.tokens[accountID] = accessToken{refresh: refresh, access: tokens.Access, expiry: tokens.Expiry}
	s.mu.Unlock()
	return tokens.Access, nil
}

func credentialName(accountID string) string {
	return Integration + "-" + accountID
}

func randomString(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate Google sign-in secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

const secretFileSizeLimit = 4 << 10

// LoadClientSecretFile reads the OAuth client secret from a file only its owner can read.
func LoadClientSecretFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("Google client secret file metadata could not be read")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > secretFileSizeLimit {
		return "", fmt.Errorf("Google client secret file must be a small regular file without group or other permissions")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("Google client secret file could not be read")
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" || !utf8.ValidString(secret) || strings.ContainsAny(secret, " \t\r\n") {
		return "", fmt.Errorf("Google client secret file does not contain a secret")
	}
	return secret, nil
}

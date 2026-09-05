package connection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	DefaultProvider    = "openai-codex"
	DefaultAuthBaseURL = "https://auth.openai.com"
	DefaultClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultCodeTimeout = 15 * time.Minute
	defaultHTTPTimeout = 30 * time.Second
	deliveryTimeout    = 30 * time.Second
)

type Config struct {
	ProfilesDir string
	Provider    string
	AuthBaseURL string
	ClientID    string
	HTTPClient  *http.Client
	CodeTimeout time.Duration
	Principals  []string
}

type Service struct {
	profilesDir string
	provider    string
	codeTimeout time.Duration
	client      *deviceClient
	repository  Repository
	deliverer   Deliverer
	logger      *slog.Logger

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	pending map[string]*ceremony
	wg      sync.WaitGroup
}

type ceremony struct {
	cancel context.CancelFunc
}

func NewService(cfg Config, repository Repository, deliverer Deliverer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("connection repository is required")
	}
	if deliverer == nil {
		return nil, fmt.Errorf("connection deliverer is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("connection logger is required")
	}
	if strings.TrimSpace(cfg.ProfilesDir) == "" {
		return nil, fmt.Errorf("connection profiles directory is required")
	}
	profilesDir, err := filepath.Abs(cfg.ProfilesDir)
	if err != nil {
		return nil, fmt.Errorf("resolve connection profiles directory: %w", err)
	}
	if err := preparePrivateDirectory(profilesDir); err != nil {
		return nil, err
	}
	for _, principalID := range cfg.Principals {
		if _, err := agentruntime.ProfileDirectoryName(principalID); err != nil {
			return nil, fmt.Errorf("principal %q: %w", principalID, err)
		}
	}
	baseURL, err := normalizeAuthBaseURL(cfg.AuthBaseURL)
	if err != nil {
		return nil, err
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	codeTimeout := cfg.CodeTimeout
	if codeTimeout <= 0 {
		codeTimeout = DefaultCodeTimeout
	}
	provider := cfg.Provider
	if provider == "" {
		provider = DefaultProvider
	}
	clientID := cfg.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}

	return &Service{
		profilesDir: profilesDir,
		provider:    provider,
		codeTimeout: codeTimeout,
		client:      &deviceClient{baseURL: baseURL, clientID: clientID, httpClient: httpClient},
		repository:  repository,
		deliverer:   deliverer,
		logger:      logger,
		pending:     make(map[string]*ceremony),
	}, nil
}

func normalizeAuthBaseURL(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return DefaultAuthBaseURL, nil
	}
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("connection auth base URL must be a plain HTTP or HTTPS URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && !(scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", fmt.Errorf("connection auth base URL must use HTTPS, or HTTP for a loopback address")
	}
	parsed.Scheme = scheme
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return fmt.Errorf("connection service is already running")
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	if cancel == nil {
		s.mu.Unlock()
		return nil
	}
	s.cancel = nil
	s.mu.Unlock()
	cancel()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.mu.Lock()
		s.ctx = nil
		s.pending = make(map[string]*ceremony)
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown connection service: %w", ctx.Err())
	}
}

func (s *Service) ProfileDir(principalID string) (string, error) {
	name, err := agentruntime.ProfileDirectoryName(principalID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.profilesDir, name), nil
}

// Ready satisfies the conversation gate: Pi must have a refreshable credential and the
// application must know which account it belongs to.
func (s *Service) Ready(ctx context.Context, principalID string) (bool, error) {
	profileDir, err := s.ProfileDir(principalID)
	if err != nil {
		return false, err
	}
	_, found, err := readCredential(profileDir, s.provider)
	if err != nil || !found {
		return false, err
	}
	_, bound, err := s.repository.LoadModelConnection(ctx, principalID)
	if err != nil {
		return false, err
	}
	return bound, nil
}

func (s *Service) Status(ctx context.Context, principalID string) (Status, error) {
	profileDir, err := s.ProfileDir(principalID)
	if err != nil {
		return Status{}, err
	}
	credential, found, err := readCredential(profileDir, s.provider)
	if err != nil {
		return Status{}, err
	}
	record, bound, err := s.repository.LoadModelConnection(ctx, principalID)
	if err != nil {
		return Status{}, err
	}
	return Status{
		Connected: found && bound,
		Provider:  s.provider,
		AccountID: record.AccountID,
		ExpiresAt: credential.expiresAt(),
	}, nil
}

func (s *Service) Connect(ctx context.Context, principalID string, chatID int64) (Ceremony, error) {
	if _, err := s.ProfileDir(principalID); err != nil {
		return Ceremony{}, err
	}
	s.mu.Lock()
	running := s.ctx
	s.mu.Unlock()
	if running == nil {
		return Ceremony{}, fmt.Errorf("connection service is not running")
	}

	device, err := s.client.start(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "connection.ceremony.failed", "principal_id", principalID, "reason", "start")
		return Ceremony{}, err
	}
	expiresAt := time.Now().Add(s.codeTimeout)

	s.mu.Lock()
	if s.ctx == nil {
		s.mu.Unlock()
		return Ceremony{}, fmt.Errorf("connection service is not running")
	}
	if previous := s.pending[principalID]; previous != nil {
		previous.cancel()
	}
	runCtx, cancel := context.WithDeadline(s.ctx, expiresAt)
	current := &ceremony{cancel: cancel}
	s.pending[principalID] = current
	s.wg.Add(1)
	s.mu.Unlock()

	go s.complete(runCtx, current, principalID, chatID, device)
	s.logger.InfoContext(ctx, "connection.ceremony.started", "principal_id", principalID)
	return Ceremony{UserCode: device.userCode, VerificationURL: s.client.verificationURL(), ExpiresAt: expiresAt}, nil
}

func (s *Service) complete(ctx context.Context, current *ceremony, principalID string, chatID int64, device deviceAuthorization) {
	defer s.wg.Done()
	defer func() {
		current.cancel()
		s.mu.Lock()
		if s.pending[principalID] == current {
			delete(s.pending, principalID)
		}
		s.mu.Unlock()
	}()

	reply, outcome := s.finish(ctx, principalID, device)
	if reply == "" {
		s.logger.InfoContext(ctx, "connection.ceremony.completed", "principal_id", principalID, "outcome", outcome)
		return
	}
	s.mu.Lock()
	base := s.ctx
	s.mu.Unlock()
	if base == nil || base.Err() != nil {
		return
	}
	deliveryCtx, cancel := context.WithTimeout(base, deliveryTimeout)
	defer cancel()
	if err := s.deliverer.SendMessage(deliveryCtx, chatID, reply); err != nil {
		s.logger.WarnContext(deliveryCtx, "connection.ceremony.undeliverable", "principal_id", principalID, "outcome", outcome)
	}
	event := "connection.ceremony.completed"
	if outcome != "connected" {
		event = "connection.ceremony.failed"
	}
	s.logger.InfoContext(deliveryCtx, event, "principal_id", principalID, "outcome", outcome)
}

// finish returns the reply for the user and a content-free outcome word; an empty reply means
// the ceremony was superseded or the service stopped, so nothing should be sent.
func (s *Service) finish(ctx context.Context, principalID string, device deviceAuthorization) (string, string) {
	code, err := s.client.poll(ctx, device)
	switch {
	case errors.Is(err, context.Canceled):
		return "", "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return ConnectExpiredReply, "expired"
	case errors.Is(err, errDenied):
		return ConnectDeniedReply, "denied"
	case err != nil:
		return ConnectFailedReply, "failed"
	}
	credential, err := s.client.exchange(ctx, code)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", "cancelled"
		}
		return ConnectFailedReply, "failed"
	}
	profileDir, err := s.ProfileDir(principalID)
	if err != nil {
		return ConnectFailedReply, "failed"
	}
	if err := preparePrivateDirectory(profileDir); err != nil {
		return ConnectFailedReply, "failed"
	}
	bound, err := s.repository.BindModelConnection(ctx, principalID, s.provider, credential.AccountID)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", "cancelled"
		}
		return ConnectFailedReply, "failed"
	}
	if !bound {
		return AccountAlreadyBoundReply, "already_bound"
	}
	if err := writeCredential(profileDir, s.provider, credential); err != nil {
		_, _ = s.repository.DeleteModelConnection(ctx, principalID)
		return ConnectFailedReply, "failed"
	}
	// A disconnect or replacement that raced the final write must not leave credentials behind.
	if ctx.Err() != nil {
		_, _ = removeCredential(profileDir)
		_, _ = s.repository.DeleteModelConnection(context.WithoutCancel(ctx), principalID)
		return "", "cancelled"
	}
	return ConnectedReply, "connected"
}

func (s *Service) Disconnect(ctx context.Context, principalID string) (bool, error) {
	profileDir, err := s.ProfileDir(principalID)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	if pending := s.pending[principalID]; pending != nil {
		pending.cancel()
		delete(s.pending, principalID)
	}
	s.mu.Unlock()

	removed, err := removeCredential(profileDir)
	if err != nil {
		return false, err
	}
	deleted, err := s.repository.DeleteModelConnection(ctx, principalID)
	if err != nil {
		return false, err
	}
	return removed || deleted, nil
}

// Package homeassistant is the Home Assistant connector: for a principal whose home switch is on,
// their turns reach the devices the household exposed to Assist through Home Assistant's own MCP
// tools, read energy statistics, and propose confirmed service calls for devices labelled as
// needing confirmation, all with one household token.
package homeassistant

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	Integration = "homeassistant"
	// A row for the integration records that the principal turned the home off; without one it is
	// on, since the home is shared wherever the operator configured a token.
	optOutWorkspaceID   = "homeassistant-off"
	optOutWorkspaceName = "Home off"

	DefaultBaseURL = "https://ha.home"
)

type Config struct {
	// Token is the household's long-lived access token, created by a non-admin Home Assistant user.
	Token string
	// BaseURL is Home Assistant's address; empty means DefaultBaseURL.
	BaseURL string
	// Now is the clock for the snapshot's age; nil means time.Now.
	Now func() time.Time
}

// Status is a principal's home switch.
type Status struct {
	Enabled bool
}

// Proposer is the core approval service as the connector sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

// ExposureChecker reports whether the calling conversation or run has read content written outside
// the household.
type ExposureChecker interface {
	External(ctx context.Context, principalID string) bool
}

type Service struct {
	client     *client
	repository integration.Repository
	proposer   Proposer
	exposure   ExposureChecker
	logger     *slog.Logger
	now        func() time.Time

	snapshotMu sync.Mutex
	snapshot   *snapshot

	running atomic.Bool
}

var _ module.Module = (*Service)(nil)

func NewService(cfg Config, repository integration.Repository, proposer Proposer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("home assistant repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("home assistant logger is required")
	}
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return nil, fmt.Errorf("home assistant token is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Path != "" {
		return nil, fmt.Errorf("home assistant URL must be an http or https origin without credentials, path, or query")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		client:     newClient(parsed, token),
		repository: repository,
		proposer:   proposer,
		logger:     logger,
		now:        now,
	}, nil
}

// SetExposure makes immediate home actions need confirmation once the checker reports outside
// content; nil, the default, never gates them. Call it before the service handles turns.
func (s *Service) SetExposure(checker ExposureChecker) {
	s.exposure = checker
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
		return fmt.Errorf("home assistant connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	s.client.http.CloseIdleConnections()
	return nil
}

// Status reports the principal's switch. The home is on unless the principal saved the opt-out row,
// which carries no credential, since the token belongs to the household.
func (s *Service) Status(ctx context.Context, principalID string) (Status, error) {
	_, optedOut, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return Status{}, err
	}
	return Status{Enabled: !optedOut}, nil
}

// SetEnabled turns the principal's home on or off. Turning it off takes effect on the next tool
// call, including one later in a turn already running.
func (s *Service) SetEnabled(ctx context.Context, principalID string, enabled bool) (Status, error) {
	if enabled {
		deleted, err := s.repository.DeleteIntegrationConnection(ctx, principalID, Integration)
		if err != nil {
			return Status{}, err
		}
		if deleted {
			s.logger.InfoContext(ctx, "homeassistant.home.enabled", "principal_id", principalID)
		}
		return Status{Enabled: true}, nil
	}
	status, err := s.Status(ctx, principalID)
	if err != nil || !status.Enabled {
		return status, err
	}
	if err := s.repository.SaveIntegrationConnection(ctx, integration.Connection{
		PrincipalID:   principalID,
		Integration:   Integration,
		TokenKind:     integration.TokenKindInternal,
		WorkspaceID:   optOutWorkspaceID,
		WorkspaceName: optOutWorkspaceName,
	}); err != nil {
		return Status{}, err
	}
	s.logger.InfoContext(ctx, "homeassistant.home.disabled", "principal_id", principalID)
	return Status{}, nil
}

// Exposure is what the exposure check found: whether it could run, and the exposed devices that
// need confirmation, which withhold every immediate home action while they stay exposed.
type Exposure struct {
	Checked   bool
	Conflicts []string
}

// Exposure reports the check for the settings page; an unreachable Home Assistant is reported as
// unchecked rather than as an error.
func (s *Service) Exposure(ctx context.Context) Exposure {
	snap, err := s.current(ctx)
	if err != nil || !snap.guarded {
		return Exposure{}
	}
	return Exposure{Checked: true, Conflicts: snap.conflicts}
}

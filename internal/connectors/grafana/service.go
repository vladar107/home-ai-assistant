// Package grafana is the Grafana connector: for the principals the operator named, their turns
// read the household's dashboards, query Prometheus metrics, and search Loki logs through
// Grafana's data source proxy, all with one read-only service-account token.
package grafana

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	Integration = "grafana"
	// A row for the integration records that the principal turned Grafana off; without one it is
	// on for every principal the operator allowed.
	optOutWorkspaceID   = "grafana-off"
	optOutWorkspaceName = "Grafana off"

	DefaultBaseURL = "http://grafana:3000"

	// The data sources home-monitoring provisions, by UID.
	prometheusUID = "prometheus"
	lokiUID       = "loki"
)

type Config struct {
	// Token is a Grafana service-account token with the Viewer role.
	Token string
	// BaseURL is Grafana's address; empty means DefaultBaseURL.
	BaseURL string
	// Principals may use Grafana; nobody else is offered it.
	Principals []string
	// Location is the zone times are shown in; nil means UTC.
	Location *time.Location
	// Now is the clock that open-ended ranges end at; nil means time.Now.
	Now func() time.Time
}

// Status is a principal's Grafana switch. Allowed is false for anyone the operator did not name,
// who then has no switch at all.
type Status struct {
	Allowed bool
	Enabled bool
}

type Service struct {
	client     *client
	repository integration.Repository
	principals []string
	location   *time.Location
	logger     *slog.Logger
	now        func() time.Time

	running atomic.Bool
}

var _ module.Module = (*Service)(nil)

func NewService(cfg Config, repository integration.Repository, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("grafana repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("grafana logger is required")
	}
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return nil, fmt.Errorf("grafana token is required")
	}
	if len(cfg.Principals) == 0 {
		return nil, fmt.Errorf("grafana needs at least one allowed principal")
	}
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Path != "" {
		return nil, fmt.Errorf("grafana URL must be an http or https origin without credentials, path, or query")
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
		client:     newClient(parsed, token),
		repository: repository,
		principals: slices.Clone(cfg.Principals),
		location:   location,
		logger:     logger,
		now:        now,
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
		return fmt.Errorf("grafana connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	s.client.http.CloseIdleConnections()
	return nil
}

// Allowed reports whether the operator named the principal; nobody else sees Grafana anywhere.
func (s *Service) Allowed(principalID string) bool {
	return slices.Contains(s.principals, principalID)
}

// Status reports the principal's switch. Grafana is on for an allowed principal unless they saved
// the opt-out row, which carries no credential, since the token belongs to the operator.
func (s *Service) Status(ctx context.Context, principalID string) (Status, error) {
	if !s.Allowed(principalID) {
		return Status{}, nil
	}
	_, optedOut, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return Status{}, err
	}
	return Status{Allowed: true, Enabled: !optedOut}, nil
}

// SetEnabled turns Grafana on or off for an allowed principal. Turning it off takes effect on the
// next tool call, including one later in a turn already running.
func (s *Service) SetEnabled(ctx context.Context, principalID string, enabled bool) (Status, error) {
	if !s.Allowed(principalID) {
		return Status{}, nil
	}
	if enabled {
		deleted, err := s.repository.DeleteIntegrationConnection(ctx, principalID, Integration)
		if err != nil {
			return Status{}, err
		}
		if deleted {
			s.logger.InfoContext(ctx, "grafana.enabled", "principal_id", principalID)
		}
		return Status{Allowed: true, Enabled: true}, nil
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
	s.logger.InfoContext(ctx, "grafana.disabled", "principal_id", principalID)
	return Status{Allowed: true}, nil
}

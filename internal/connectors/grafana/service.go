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
	Integration    = "grafana"
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

type Service struct {
	client     *client
	principals []string
	location   *time.Location
	logger     *slog.Logger
	now        func() time.Time

	running atomic.Bool
}

var (
	_ module.Module        = (*Service)(nil)
	_ integration.Switched = (*Service)(nil)
)

func NewService(cfg Config, logger *slog.Logger) (*Service, error) {
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

// Switch is each principal's Grafana switch, on by default for every principal the operator
// allowed, since the token belongs to the operator.
var Switch = integration.Switch{Integration: Integration, Name: "Grafana", DefaultOn: true}

func (s *Service) Switch() integration.Switch { return Switch }

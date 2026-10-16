// Package ns is the NS train information connector: for a principal whose trains switch is on,
// their turns can plan trips between Dutch stations, list departures, and read disruptions from the
// NS travel information API, and count OV-fiets rental bikes from the NS places API, using one
// operator key for the whole household.
package ns

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	Integration = "ns"
	// A row for the integration records that the principal turned trains off; without one they are
	// on, since trains are on by default wherever the operator configured a key.
	optOutWorkspaceID   = "ns-trains-off"
	optOutWorkspaceName = "Trains off"

	defaultBaseURL       = "https://gateway.apiportal.ns.nl/reisinformatie-api/api"
	defaultPlacesBaseURL = "https://gateway.apiportal.ns.nl/places-api"
	timeZone             = "Europe/Amsterdam"
)

type Config struct {
	// APIKey is the operator's NS API Portal subscription key.
	APIKey string
	// BaseURL overrides the NS travel information gateway; empty means the public one.
	BaseURL string
	// PlacesBaseURL overrides the NS places gateway, which reports OV-fiets; empty means the public
	// one.
	PlacesBaseURL string
	// Now is the clock for default times, the station list's age, and the result cache; nil means
	// time.Now.
	Now func() time.Time
}

// Status is a principal's trains switch.
type Status struct {
	Enabled bool
}

type Service struct {
	client     *client
	places     *client
	repository integration.Repository
	logger     *slog.Logger
	now        func() time.Time
	location   *time.Location

	stations *directory

	cacheMu sync.Mutex
	cache   map[string]cachedResult

	running atomic.Bool
}

var _ module.Module = (*Service)(nil)

func NewService(cfg Config, repository integration.Repository, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("ns repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("ns logger is required")
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, fmt.Errorf("ns API key is required")
	}
	parsed, err := gatewayURL(cfg.BaseURL, defaultBaseURL)
	if err != nil {
		return nil, fmt.Errorf("ns base URL must be an http or https address without credentials or query")
	}
	places, err := gatewayURL(cfg.PlacesBaseURL, defaultPlacesBaseURL)
	if err != nil {
		return nil, fmt.Errorf("ns places base URL must be an http or https address without credentials or query")
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return nil, fmt.Errorf("load %s time zone: %w", timeZone, err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	service := &Service{
		client:     newClient(parsed, key),
		places:     newClient(places, key),
		repository: repository,
		logger:     logger,
		now:        now,
		location:   location,
		cache:      make(map[string]cachedResult),
	}
	service.stations = &directory{fetch: service.client.stations, now: now}
	return service, nil
}

func gatewayURL(value, fallback string) (*url.URL, error) {
	if value == "" {
		value = fallback
	}
	parsed, err := url.Parse(strings.TrimRight(value, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" {
		return nil, fmt.Errorf("invalid gateway URL")
	}
	return parsed, nil
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
		return fmt.Errorf("ns connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	s.client.http.CloseIdleConnections()
	s.places.http.CloseIdleConnections()
	return nil
}

// Status reports the principal's switch. Trains are on unless the principal saved the opt-out row,
// which carries no credential, since the key belongs to the operator.
func (s *Service) Status(ctx context.Context, principalID string) (Status, error) {
	_, optedOut, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return Status{}, err
	}
	return Status{Enabled: !optedOut}, nil
}

// SetEnabled turns the principal's trains on or off. Turning them off takes effect on the next tool
// call, including one later in a turn already running.
func (s *Service) SetEnabled(ctx context.Context, principalID string, enabled bool) (Status, error) {
	if enabled {
		deleted, err := s.repository.DeleteIntegrationConnection(ctx, principalID, Integration)
		if err != nil {
			return Status{}, err
		}
		if deleted {
			s.logger.InfoContext(ctx, "ns.trains.enabled", "principal_id", principalID)
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
	s.logger.InfoContext(ctx, "ns.trains.disabled", "principal_id", principalID)
	return Status{}, nil
}

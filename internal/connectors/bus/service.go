// Package bus is the bus information connector: for a principal whose buses switch is on, their
// turns can read live departures and direct trips from OVapi, plan trips at any time through
// Transitous, and see the operators' disruption notices, with no account at any of them.
package bus

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
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	Integration = "bus"
	// A row for the integration records that the principal turned buses off; without one they are
	// on, since buses are on by default wherever the operator enabled them.
	optOutWorkspaceID   = "bus-off"
	optOutWorkspaceName = "Buses off"

	moduleVersion = "0.1.0"

	defaultOVapiURL      = "http://v0.ovapi.nl"
	defaultGTFSURL       = "http://gtfs.ovapi.nl/nl"
	defaultTransitousURL = "https://api.transitous.org/api"
	timeZone             = "Europe/Amsterdam"

	maximumContactRunes = 200
)

type Config struct {
	// HomeStop is a timing point code of the household's home stop; the tools default to all
	// platforms of the stop it belongs to.
	HomeStop string
	// Contact is how Transitous can reach the operator, sent in the User-Agent once it is set.
	Contact string
	// OVapiURL, GTFSURL, and TransitousURL override the public services; empty means those.
	OVapiURL      string
	GTFSURL       string
	TransitousURL string
	// Now is the clock for default times, the directory's age, and the caches; nil means time.Now.
	Now func() time.Time
}

// Status is a principal's buses switch.
type Status struct {
	Enabled bool
}

type Service struct {
	ovapi      *client
	gtfs       *client
	transitous *client
	repository integration.Repository
	logger     *slog.Logger
	now        func() time.Time
	location   *time.Location
	homeStop   string

	stops  *transit.Directory[*stopIndex]
	alerts *alertFeed

	cacheMu sync.Mutex
	cache   map[string]cachedResult

	running atomic.Bool
}

var (
	_ module.Module      = (*Service)(nil)
	_ transit.Departures = (*Service)(nil)
)

func NewService(cfg Config, repository integration.Repository, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("bus repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("bus logger is required")
	}
	homeStop := strings.TrimSpace(cfg.HomeStop)
	if homeStop != "" && !digitsOnly(homeStop) {
		return nil, fmt.Errorf("bus home stop must be a timing point code made of digits")
	}
	contact := strings.TrimSpace(cfg.Contact)
	if len([]rune(contact)) > maximumContactRunes || strings.ContainsAny(contact, "\r\n()") {
		return nil, fmt.Errorf("bus contact must be one line of at most %d characters without parentheses", maximumContactRunes)
	}
	ovapiBase, err := baseURL(cfg.OVapiURL, defaultOVapiURL)
	if err != nil {
		return nil, err
	}
	gtfsBase, err := baseURL(cfg.GTFSURL, defaultGTFSURL)
	if err != nil {
		return nil, err
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return nil, fmt.Errorf("load %s time zone: %w", timeZone, err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	agent := "HomeAgent/" + moduleVersion + " (private household assistant)"
	service := &Service{
		ovapi:      newClient(ovapiBase, agent),
		gtfs:       newClient(gtfsBase, agent),
		repository: repository,
		logger:     logger,
		now:        now,
		location:   location,
		homeStop:   homeStop,
		cache:      make(map[string]cachedResult),
	}
	transitousBase, err := baseURL(cfg.TransitousURL, defaultTransitousURL)
	if err != nil {
		return nil, err
	}
	transitousAgent := agent
	if contact != "" {
		transitousAgent = "HomeAgent/" + moduleVersion + " (+" + contact + ")"
	}
	service.transitous = newClient(transitousBase, transitousAgent)
	service.stops = &transit.Directory[*stopIndex]{
		Fetch:       service.fetchStops,
		Now:         now,
		Lifetime:    directoryLifetime,
		RetryDelay:  directoryRetryDelay,
		Interrupted: transportError,
	}
	service.alerts = &alertFeed{fetch: service.gtfs.alerts, now: now}
	return service, nil
}

func baseURL(value, fallback string) (*url.URL, error) {
	if value == "" {
		value = fallback
	}
	parsed, err := url.Parse(strings.TrimRight(value, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" {
		return nil, fmt.Errorf("bus service URL must be an http or https address without credentials or query")
	}
	return parsed, nil
}

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: Integration, Version: moduleVersion, Kind: module.KindConnector}
}

func (s *Service) Start(context.Context) error {
	s.running.Store(true)
	return nil
}

func (s *Service) Ready(context.Context) error {
	if !s.running.Load() {
		return fmt.Errorf("bus connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	for _, c := range []*client{s.ovapi, s.gtfs, s.transitous} {
		if c != nil {
			c.http.CloseIdleConnections()
		}
	}
	return nil
}

// Status reports the principal's switch. Buses are on unless the principal saved the opt-out row,
// which carries no credential, since none of the services needs one.
func (s *Service) Status(ctx context.Context, principalID string) (Status, error) {
	_, optedOut, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return Status{}, err
	}
	return Status{Enabled: !optedOut}, nil
}

// SetEnabled turns the principal's buses on or off. Turning them off takes effect on the next tool
// call, including one later in a turn already running.
func (s *Service) SetEnabled(ctx context.Context, principalID string, enabled bool) (Status, error) {
	if enabled {
		deleted, err := s.repository.DeleteIntegrationConnection(ctx, principalID, Integration)
		if err != nil {
			return Status{}, err
		}
		if deleted {
			s.logger.InfoContext(ctx, "bus.enabled", "principal_id", principalID)
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
	s.logger.InfoContext(ctx, "bus.disabled", "principal_id", principalID)
	return Status{}, nil
}

func digitsOnly(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

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
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	Integration = "ns"

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

type Service struct {
	client   *client
	places   *client
	switches integration.SwitchReader
	logger   *slog.Logger
	now      func() time.Time
	location *time.Location

	stations *transit.Directory[[]station]

	cacheMu sync.Mutex
	cache   map[string]cachedResult

	running atomic.Bool
}

var (
	_ module.Module        = (*Service)(nil)
	_ integration.Switched = (*Service)(nil)
	_ transit.Departures   = (*Service)(nil)
	_ transit.Alternatives = (*Service)(nil)
	_ transit.Disruptions  = (*Service)(nil)
	_ transit.Trips        = (*Service)(nil)
)

func NewService(cfg Config, switches integration.SwitchReader, logger *slog.Logger) (*Service, error) {
	if switches == nil {
		return nil, fmt.Errorf("ns switches are required")
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
		client:   newClient(parsed, key),
		places:   newClient(places, key),
		switches: switches,
		logger:   logger,
		now:      now,
		location: location,
		cache:    make(map[string]cachedResult),
	}
	service.stations = &transit.Directory[[]station]{
		Fetch:       service.fetchStations,
		Now:         now,
		Lifetime:    stationListLifetime,
		RetryDelay:  stationRetryDelay,
		Interrupted: transportError,
	}
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

// Switch is each principal's trains switch. Trains are on by default wherever the operator
// configured a key, since the key is the household's.
var Switch = integration.Switch{Integration: Integration, Name: "Train information", DefaultOn: true}

func (s *Service) Switch() integration.Switch { return Switch }

// Enabled reports whether the principal has trains on.
func (s *Service) Enabled(ctx context.Context, principalID string) (bool, error) {
	return s.switches.Enabled(ctx, principalID, Switch)
}

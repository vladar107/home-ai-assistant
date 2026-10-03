// Package places is the nearby-places connector: it finds pharmacies, shops, chargers, and other
// amenities around a point in OpenStreetMap through Overpass, and names a point through
// Transitous reverse geocoding, with no account at either.
package places

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	moduleVersion = "0.1.0"

	defaultOverpassURL   = "https://overpass-api.de/api/interpreter"
	defaultTransitousURL = "https://api.transitous.org/api"

	maximumContactRunes = 200
)

type Config struct {
	// OverpassURL and TransitousURL override the public services; empty means those.
	OverpassURL   string
	TransitousURL string
	// Contact is how the services can reach the operator, sent in the User-Agent once it is set.
	Contact string
	// HTTPClient replaces the default client, for tests.
	HTTPClient *http.Client
	// Now is the clock for the caches; nil means time.Now.
	Now func() time.Time
}

type Service struct {
	overpass   *url.URL
	transitous *url.URL
	agent      string
	http       *http.Client
	logger     *slog.Logger
	now        func() time.Time

	cacheMu sync.Mutex
	cache   map[string]cachedResult

	running atomic.Bool
}

var _ module.Module = (*Service)(nil)

func NewService(cfg Config, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		return nil, fmt.Errorf("places logger is required")
	}
	contact := strings.TrimSpace(cfg.Contact)
	if len([]rune(contact)) > maximumContactRunes || strings.ContainsAny(contact, "\r\n()") {
		return nil, fmt.Errorf("places contact must be one line of at most %d characters without parentheses", maximumContactRunes)
	}
	overpass, err := baseURL(cfg.OverpassURL, defaultOverpassURL)
	if err != nil {
		return nil, err
	}
	transitous, err := baseURL(cfg.TransitousURL, defaultTransitousURL)
	if err != nil {
		return nil, err
	}
	agent := "HomeAgent/" + moduleVersion + " (private household assistant)"
	if contact != "" {
		agent = "HomeAgent/" + moduleVersion + " (+" + contact + ")"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = newHTTPClient()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		overpass:   overpass,
		transitous: transitous,
		agent:      agent,
		http:       client,
		logger:     logger,
		now:        now,
		cache:      make(map[string]cachedResult),
	}, nil
}

func baseURL(value, fallback string) (*url.URL, error) {
	if value == "" {
		value = fallback
	}
	parsed, err := url.Parse(strings.TrimRight(value, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" {
		return nil, fmt.Errorf("places service URL must be an http or https address without credentials or query")
	}
	return parsed, nil
}

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "places", Version: moduleVersion, Kind: module.KindConnector}
}

func (s *Service) Start(context.Context) error {
	s.running.Store(true)
	return nil
}

func (s *Service) Ready(context.Context) error {
	if !s.running.Load() {
		return fmt.Errorf("places connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	s.http.CloseIdleConnections()
	return nil
}

type cachedResult struct {
	value   any
	expires time.Time
}

func (s *Service) cached(key string) (any, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entry, found := s.cache[key]
	if !found || !s.now().Before(entry.expires) {
		return nil, false
	}
	return entry.value, true
}

func (s *Service) store(key string, value any, lifetime time.Duration) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	now := s.now()
	for existing, entry := range s.cache {
		if !now.Before(entry.expires) {
			delete(s.cache, existing)
		}
	}
	s.cache[key] = cachedResult{value: value, expires: now.Add(lifetime)}
}

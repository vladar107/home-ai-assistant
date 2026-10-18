// Package web is the web connector: for a principal whose web access is on, their turns can
// read a public page or PDF by its address and get a weather forecast for a named place. Every
// request goes through one guarded client that refuses the home network, so no address the model
// is given or redirected to can reach the NAS, the router, or anything else inside the house.
package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/daycard"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	Integration = "web"
	// A row for the integration records that the principal turned web access off; without one it
	// is on, since web access is on by default wherever the operator enabled it.
	optOutWorkspaceID   = "web-access-off"
	optOutWorkspaceName = "Web access off"

	defaultGeocodingURL = "https://geocoding-api.open-meteo.com/v1/search"
	defaultForecastURL  = "https://api.open-meteo.com/v1/forecast"
	maximumPlaceRunes   = 200
)

type Config struct {
	// WeatherHome is the household's place name, used when the user names none; empty means the
	// model asks.
	WeatherHome string
	// GeocodingURL and ForecastURL override the Open-Meteo endpoints; empty means the public ones.
	GeocodingURL string
	ForecastURL  string
	// Resolver and Dial replace name resolution and the final connection; nil means the system
	// resolver and a plain dialer. The guard vets every resolved address before Dial sees it, so
	// a test can route approved public addresses to a local server without widening the guard.
	Resolver Resolver
	Dial     DialFunc
	// Now is the clock that bounds an hourly forecast's date; nil means time.Now.
	Now func() time.Time
	// Location is the household's time zone, the calendar that bounds an hourly forecast's date
	// for coordinates, which come without a zone of their own; nil means UTC.
	Location *time.Location
	// Cards keeps a day card for the reply of the turn that drew it; nil leaves weather_card out.
	Cards Cards
	// Trains reports train statuses for a day card's trains; nil leaves trains off the card.
	Trains Trains
}

// Cards stages a PNG for the reply of a principal's turn.
type Cards interface {
	Stage(principalID, turn string, png []byte) error
}

// Trains reports how the train a principal would take in one direction is running, under their
// own train information switch. A *TrainRefusal is told to the model; any other error fails the
// call.
type Trains interface {
	// Enabled reports whether the principal's train information switch is on.
	Enabled(ctx context.Context, principalID string) (bool, error)
	TripStatus(ctx context.Context, principalID string, query TrainQuery) (TrainReport, error)
}

// TrainQuery is one travel direction. At's wall clock is Dutch local time.
type TrainQuery struct {
	From     string
	To       string
	At       time.Time
	ArriveBy bool
}

// TrainReport is the train a principal would take, classified. An actual time is zero when there
// is no live time for it.
type TrainReport struct {
	Level daycard.TrainStatus
	Cause TrainCause
	// From and To are the resolved station names.
	From      string
	To        string
	Found     bool
	Cancelled bool

	PlannedDeparture time.Time
	ActualDeparture  time.Time
	PlannedArrival   time.Time
	ActualArrival    time.Time
	DelayMinutes     int
	ExtraMinutes     int
	Transfers        int
	// Reason says in a few English words why the train has its level.
	Reason string
}

// TrainCause is the dominant reason a train has its level.
type TrainCause int

const (
	TrainCauseNone TrainCause = iota
	TrainCauseDelay
	TrainCauseLonger
	TrainCauseCancelled
	TrainCauseAlternative
	TrainCauseMissedConnection
	TrainCauseDisruption
	TrainCauseNoTrip
)

// TrainRefusal is a sentence the model is told instead of a train status, such as train
// information being off for the principal.
type TrainRefusal struct {
	Text string
	// Unavailable marks a refusal about train information itself, such as it being off or NS
	// failing, rather than about the requested stations; retrying without trains is the way out.
	Unavailable bool
}

func (e *TrainRefusal) Error() string { return e.Text }

// Status is a principal's web access switch.
type Status struct {
	Enabled bool
}

type Service struct {
	guard        *guard
	repository   integration.Repository
	weatherHome  string
	geocodingURL *url.URL
	forecastURL  *url.URL
	logger       *slog.Logger
	now          func() time.Time
	location     *time.Location
	cards        Cards
	trains       Trains
	telemetry    Telemetry
	exposure     ExposureChecker

	running atomic.Bool
}

var _ module.Module = (*Service)(nil)

// Telemetry records day-card render metrics.
type Telemetry interface {
	RecordDayCardRender(ctx context.Context, outcome string, duration time.Duration)
}

// ExposureChecker reports whether the principal's conversation has read content from outside the
// household.
type ExposureChecker interface {
	External(ctx context.Context, principalID string) bool
}

// SetExposure sets the check that limits fetch_page to addresses the run has seen, at wiring time
// before turns start. Without one, fetch_page opens any public address.
func (s *Service) SetExposure(exposure ExposureChecker) {
	s.exposure = exposure
}

// SetTelemetry sets where render metrics are recorded, at wiring time before turns start.
func (s *Service) SetTelemetry(telemetry Telemetry) {
	s.telemetry = telemetry
}

func NewService(cfg Config, repository integration.Repository, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("web repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("web logger is required")
	}
	home := strings.TrimSpace(cfg.WeatherHome)
	if len([]rune(home)) > maximumPlaceRunes {
		return nil, fmt.Errorf("weather home must be at most %d characters", maximumPlaceRunes)
	}
	geocoding, err := endpoint(cfg.GeocodingURL, defaultGeocodingURL)
	if err != nil {
		return nil, fmt.Errorf("weather geocoding URL: %w", err)
	}
	forecast, err := endpoint(cfg.ForecastURL, defaultForecastURL)
	if err != nil {
		return nil, fmt.Errorf("weather forecast URL: %w", err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	location := cfg.Location
	if location == nil {
		location = time.UTC
	}
	return &Service{
		now:          now,
		location:     location,
		cards:        cfg.Cards,
		trains:       cfg.Trains,
		guard:        newGuard(cfg.Resolver, cfg.Dial),
		repository:   repository,
		weatherHome:  home,
		geocodingURL: geocoding,
		forecastURL:  forecast,
		logger:       logger,
	}, nil
}

func endpoint(configured, fallback string) (*url.URL, error) {
	if configured == "" {
		configured = fallback
	}
	parsed, err := url.Parse(configured)
	if err != nil {
		return nil, err
	}
	if err := checkURL(parsed); err != nil {
		return nil, err
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
		return fmt.Errorf("web connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	s.guard.client.CloseIdleConnections()
	return nil
}

// Status reports the principal's switch. Web access is on unless the principal saved the opt-out
// row, which carries no credential, since nothing here signs in anywhere.
func (s *Service) Status(ctx context.Context, principalID string) (Status, error) {
	_, optedOut, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return Status{}, err
	}
	return Status{Enabled: !optedOut}, nil
}

// SetEnabled turns the principal's web access on or off. Turning it off takes effect on the next
// tool call, including one later in a turn already running.
func (s *Service) SetEnabled(ctx context.Context, principalID string, enabled bool) (Status, error) {
	if enabled {
		deleted, err := s.repository.DeleteIntegrationConnection(ctx, principalID, Integration)
		if err != nil {
			return Status{}, err
		}
		if deleted {
			s.logger.InfoContext(ctx, "web.access.enabled", "principal_id", principalID)
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
	s.logger.InfoContext(ctx, "web.access.disabled", "principal_id", principalID)
	return Status{}, nil
}

// WebSearchEnabled tells the conversation whether the principal's turns load the search extension;
// it follows the same switch as the tools here.
func (s *Service) WebSearchEnabled(ctx context.Context, principalID string) (bool, error) {
	status, err := s.Status(ctx, principalID)
	return status.Enabled, err
}

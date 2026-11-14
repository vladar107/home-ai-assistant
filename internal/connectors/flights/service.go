// Package flights is the flight status connector: for a principal whose Flight status switch is
// on, their turns can look a flight up by its number and day, and the trips loop follows the
// flights of their tracked trips, both through AeroDataBox on RapidAPI with one operator key for
// the whole household and one budget of units per quota period.
package flights

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/flightnumber"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	Integration = "flights"

	// cacheLifetime lets the tool, every user's tracking, and every trip share one lookup of a
	// flight within a few minutes.
	cacheLifetime = 5 * time.Minute
	// maximumCached bounds the cache; expired entries are dropped first.
	maximumCached = 256

	// Attribution is the line that names the source wherever a user reads flight data, as the
	// free plan's terms ask; AttributionMark is what a reply must already hold to need no line.
	Attribution     = "Flight data: AeroDataBox (aerodatabox.com)"
	AttributionMark = "aerodatabox.com"

	pausedDayLayout = "Mon 2 Jan"
)

// Switch is each principal's Flight status switch, on by default wherever the operator configured
// a key, since the key is the household's.
var Switch = integration.Switch{Integration: Integration, Name: "Flight status", DefaultOn: true}

// Telemetry counts lookups and the period's units.
type Telemetry interface {
	RecordFlightLookup(ctx context.Context, purpose, outcome string)
	RecordFlightUnits(ctx context.Context, used, budget int64)
}

type Config struct {
	// APIKey is the operator's RapidAPI key for AeroDataBox.
	APIKey string
	// MonthlyUnits is what one quota period may spend, and QuotaResetDay the day of the month the
	// period starts on.
	MonthlyUnits  int
	QuotaResetDay int
	// Zone is the household's, whose today is the tool's default date.
	Zone *time.Location
	// BaseURL overrides the AeroDataBox address on RapidAPI; empty means the public one.
	BaseURL string
	// Now is the clock of the periods, the cache, and the limiter; nil means time.Now.
	Now func() time.Time
	// Sleep waits between requests; nil waits on a timer. Tests step a fake clock with it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Provider replaces AeroDataBox; nil uses it. The budget is then not applied.
	Provider Provider
}

type Service struct {
	provider  Provider
	units     Units
	budget    Budget
	switches  integration.SwitchReader
	logger    *slog.Logger
	telemetry Telemetry
	zone      *time.Location
	now       func() time.Time

	// refused is set when the provider refused the key; every lookup is skipped until restart.
	refused atomic.Bool

	cacheMu sync.Mutex
	cache   map[string]cached
	// inflight holds the lookup under way for each cache key, so callers asking for the same
	// flight at once share one request.
	inflight map[string]*flight

	running atomic.Bool
}

type cached struct {
	legs []Leg
	err  error
	at   time.Time
}

type flight struct {
	done chan struct{}
	legs []Leg
	err  error
}

var (
	_ module.Module        = (*Service)(nil)
	_ integration.Switched = (*Service)(nil)
)

func NewService(cfg Config, units Units, switches integration.SwitchReader, logger *slog.Logger) (*Service, error) {
	switch {
	case units == nil:
		return nil, fmt.Errorf("flight units are required")
	case switches == nil:
		return nil, fmt.Errorf("flight switches are required")
	case logger == nil:
		return nil, fmt.Errorf("flight logger is required")
	case cfg.Zone == nil:
		return nil, fmt.Errorf("flight zone is required")
	case cfg.MonthlyUnits < UnitsPerLookup:
		return nil, fmt.Errorf("flight monthly units must be at least %d", UnitsPerLookup)
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" && cfg.Provider == nil {
		return nil, fmt.Errorf("flight API key is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	wait := cfg.Sleep
	if wait == nil {
		wait = sleep
	}
	service := &Service{
		units: units, budget: NewBudget(cfg.MonthlyUnits, cfg.QuotaResetDay), switches: switches, logger: logger,
		zone: cfg.Zone, now: now, cache: make(map[string]cached), inflight: make(map[string]*flight),
	}
	service.provider = cfg.Provider
	if service.provider == nil {
		base, err := baseURL(cfg.BaseURL)
		if err != nil {
			return nil, err
		}
		metered := &meteredTransport{units: units, budget: service.budget, now: now, used: service.recordUnits,
			limiter: &limiter{gap: requestSpacing, now: now, sleep: wait}}
		service.provider = newAeroDataBox(base, key, metered)
	}
	return service, nil
}

func baseURL(value string) (*url.URL, error) {
	if value == "" {
		value = defaultBaseURL
	}
	parsed, err := url.Parse(strings.TrimRight(value, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" {
		return nil, fmt.Errorf("flight base URL must be an http or https address without credentials or query")
	}
	return parsed, nil
}

// SetTelemetry counts lookups and units from now on.
func (s *Service) SetTelemetry(telemetry Telemetry) { s.telemetry = telemetry }

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: Integration, Version: "0.1.0", Kind: module.KindConnector}
}

func (s *Service) Start(context.Context) error {
	s.running.Store(true)
	return nil
}

func (s *Service) Ready(context.Context) error {
	if !s.running.Load() {
		return fmt.Errorf("flight connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	if provider, ok := s.provider.(*aeroDataBox); ok {
		provider.http.CloseIdleConnections()
	}
	return nil
}

func (s *Service) Switch() integration.Switch { return Switch }

// Enabled reports whether the principal has Flight status on.
func (s *Service) Enabled(ctx context.Context, principalID string) (bool, error) {
	return s.switches.Enabled(ctx, principalID, Switch)
}

// Request is one lookup: the flight's number, its local day as YYYY-MM-DD, whether that day is
// its departure's or either end's, and whom it is for.
type Request struct {
	Designator  string
	Day         string
	Role        Role
	Purpose     Purpose
	PrincipalID string
}

// SpentError is a lookup refused because a budget is spent: the household's, the tool's share,
// or the asking user's. Until is when the next quota period starts.
type SpentError struct {
	Scope string
	Until time.Time
}

func (e *SpentError) Error() string {
	until := e.Until.Format(pausedDayLayout)
	switch e.Scope {
	case ScopeTool:
		return fmt.Sprintf("The household's flight questions have used this month's share until %s; tracked trips are still followed. Check the airline's app.", until)
	case ScopeUser:
		return fmt.Sprintf("You have used your share of flight questions until %s; tracked trips are still followed. Check the airline's app.", until)
	}
	return PausedText(e.Until)
}

// ScopeUser names a user's share in a SpentError.
const ScopeUser = "user"

// PausedText is the line for a spent household allowance, which pauses every lookup until the
// period ends.
func PausedText(until time.Time) string {
	return fmt.Sprintf("Flight tracking is paused until %s: the household's free flight-status allowance for this month is used up. Check the airline's app.", until.Format(pausedDayLayout))
}

// Lookup returns the legs under the flight number on the day. A malformed number, a refused key,
// and a spent budget send nothing. A repeat within a few minutes is served from the cache,
// whoever asks.
func (s *Service) Lookup(ctx context.Context, request Request) ([]Leg, error) {
	designator, ok := flightnumber.Normalize(request.Designator)
	if !ok {
		return nil, fmt.Errorf("a flight number is an airline code and a number, such as EX123 or Z0 4567")
	}
	if _, err := time.Parse(time.DateOnly, request.Day); err != nil {
		return nil, fmt.Errorf("the day must be YYYY-MM-DD")
	}
	if request.Role != RoleDeparture && request.Role != RoleBoth {
		request.Role = RoleDeparture
	}
	if request.Purpose != PurposeTool {
		request.Purpose = PurposeTracking
	}
	if s.refused.Load() {
		s.record(ctx, request.Purpose, "unauthorized")
		return nil, ErrUnauthorized
	}
	if ctx.Err() != nil {
		return nil, errTimeout
	}
	key := designator + "|" + request.Day + "|" + string(request.Role)
	for {
		if legs, err, found := s.cached(key, s.now()); found {
			s.record(ctx, request.Purpose, "cached")
			return legs, err
		}
		s.cacheMu.Lock()
		running, waiting := s.inflight[key]
		if !waiting {
			running = &flight{done: make(chan struct{})}
			s.inflight[key] = running
		}
		s.cacheMu.Unlock()
		if !waiting {
			running.legs, running.err = s.request(ctx, request, designator, key)
			s.cacheMu.Lock()
			delete(s.inflight, key)
			s.cacheMu.Unlock()
			close(running.done)
			return running.legs, running.err
		}
		select {
		case <-running.done:
		case <-ctx.Done():
			return nil, errTimeout
		}
		// The shared answer is reused through the cache; one that failed is not cached, so this
		// caller asks again, under its own purpose and budget.
	}
}

// request sends one lookup and caches what it found.
func (s *Service) request(ctx context.Context, request Request, designator, key string) ([]Leg, error) {
	now := s.now()
	legs, err := s.provider.Flights(withRequest(ctx, request.Purpose, request.PrincipalID), designator, request.Day, request.Role)
	outcome := "ok"
	var spent *spentError
	var budget *budgetError
	switch {
	case err == nil:
	case errors.As(err, &spent):
		_, until := s.budget.Period(now)
		scope := spent.scope
		switch {
		case scope == ScopeTool:
		case strings.HasPrefix(scope, scopeUser):
			scope = ScopeUser
		default:
			scope = ScopeAll
			s.logger.WarnContext(ctx, "flights.allowance.spent", "purpose", request.Purpose, "until", until)
		}
		s.record(ctx, request.Purpose, "spent_"+scope)
		return nil, &SpentError{Scope: scope, Until: until}
	case errors.As(err, &budget):
		s.logger.WarnContext(ctx, "flights.budget.failed", "error", budget.err)
		s.record(ctx, request.Purpose, "budget_failed")
		return nil, errUnavailable
	case errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrNotSubscribed):
		s.refused.Store(true)
		outcome = ErrorClass(err)
		s.logger.WarnContext(ctx, "flights.request.failed", "error_class", outcome)
	default:
		outcome = ErrorClass(err)
		if outcome != "not_found" && ctx.Err() == nil {
			s.logger.InfoContext(ctx, "flights.request.failed", "purpose", request.Purpose, "error_class", outcome)
		}
	}
	s.record(ctx, request.Purpose, outcome)
	if err == nil || errors.Is(err, ErrNotFound) {
		s.store(key, legs, err, now)
	}
	return legs, err
}

// Unavailable says why no lookup can be made until restart, when the provider refused the
// operator's key or the key is not subscribed to the API, and "" otherwise.
func (s *Service) Unavailable() string {
	if s.refused.Load() {
		return "Flight status is unavailable: the flight status provider refused the operator's key, so nothing is looked up until the operator fixes it and restarts."
	}
	return ""
}

// PeriodEnd is when the quota period around the instant ends and the next begins.
func (s *Service) PeriodEnd(at time.Time) time.Time {
	_, end := s.budget.Period(at)
	return end
}

// UnitsLeft is what the household budget has left in the period around now, zero once the
// marketplace said it was used up.
func (s *Service) UnitsLeft(ctx context.Context) (int, time.Time, error) {
	start, end := s.budget.Period(s.now())
	period := periodKey(start)
	spent, err := s.units.FlightPeriodSpent(ctx, period)
	if err != nil || spent {
		return 0, end, err
	}
	used, err := s.units.FlightUnitsUsed(ctx, period, ScopeAll)
	if err != nil {
		return 0, end, err
	}
	return max(s.budget.Monthly-used, 0), end, nil
}

func (s *Service) cached(key string, now time.Time) ([]Leg, error, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entry, found := s.cache[key]
	if !found || now.Sub(entry.at) >= cacheLifetime || now.Before(entry.at) {
		return nil, nil, false
	}
	return entry.legs, entry.err, true
}

func (s *Service) store(key string, legs []Leg, err error, now time.Time) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if len(s.cache) >= maximumCached {
		for old, entry := range s.cache {
			if now.Sub(entry.at) >= cacheLifetime {
				delete(s.cache, old)
			}
		}
		if len(s.cache) >= maximumCached {
			clear(s.cache)
		}
	}
	s.cache[key] = cached{legs: legs, err: err, at: now}
}

func (s *Service) record(ctx context.Context, purpose Purpose, outcome string) {
	if s.telemetry != nil {
		s.telemetry.RecordFlightLookup(ctx, string(purpose), outcome)
	}
}

func (s *Service) recordUnits(ctx context.Context, used int) {
	if s.telemetry != nil {
		s.telemetry.RecordFlightUnits(ctx, int64(used), int64(s.budget.Monthly))
	}
}

// ErrorClass names a failed lookup in one word for logs and metrics.
func ErrorClass(err error) string {
	var failure *apiError
	var spent *SpentError
	switch {
	case errors.As(err, &failure):
		return failure.class
	case errors.As(err, &spent):
		return "spent_" + spent.Scope
	}
	return "other"
}

// ModelText is the sentence the model or the user sees for a failed lookup.
func ModelText(err error) string {
	var failure *apiError
	var spent *SpentError
	switch {
	case errors.As(err, &spent):
		return spent.Error()
	case errors.As(err, &failure):
		return failure.text
	}
	return errUnavailable.text
}

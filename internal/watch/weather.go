package watch

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/localtime"
)

const (
	serviceWeather = "weather"

	weatherRain = "rain"
	weatherDry  = "dry"
	// weatherSlot is the length of one slot of the 15-minute forecast.
	weatherSlot = 15 * time.Minute
)

// Place is a named place as the forecast service found it.
type Place struct {
	Name      string
	Latitude  float64
	Longitude float64
}

// PrecipitationSlot is the precipitation forecast for the 15 minutes from Start.
type PrecipitationSlot struct {
	Start time.Time
	MM    float64
}

// Forecasts is the forecast service a weather watch reads, under its owner's web access switch.
type Forecasts interface {
	WeatherEnabled(ctx context.Context, principalID string) (bool, error)
	FindPlace(ctx context.Context, principalID, name string) (Place, error)
	// Precipitation is the 15-minute precipitation forecast at the coordinates for the next hours.
	Precipitation(ctx context.Context, principalID string, latitude, longitude float64) ([]PrecipitationSlot, error)
}

// Positions is the user's shared location, which a weather watch on current_location reads.
type Positions interface {
	CurrentPosition(ctx context.Context, principalID string) (latitude, longitude float64, found bool, err error)
}

// WeatherEvaluator checks weather watches and conditions in code against the 15-minute forecast:
// rain is coming when one slot of the window holds the threshold or more. Identical requests within
// WeatherCheckInterval share one forecast, whichever watch or job asks.
type WeatherEvaluator struct {
	forecasts Forecasts
	positions Positions
	now       func() time.Time

	mu     sync.Mutex
	cached map[string]cachedForecast
}

type cachedForecast struct {
	fetched time.Time
	slots   []PrecipitationSlot
}

var (
	_ Evaluator      = (*WeatherEvaluator)(nil)
	_ Resolver       = (*WeatherEvaluator)(nil)
	_ InstantChecker = (*WeatherEvaluator)(nil)
)

// NewWeatherEvaluator reads forecasts from forecasts and, for current_location, the user's shared
// location from positions, which may be nil to offer named places only.
func NewWeatherEvaluator(forecasts Forecasts, positions Positions, now func() time.Time) (*WeatherEvaluator, error) {
	if forecasts == nil {
		return nil, fmt.Errorf("weather watches need a forecast service")
	}
	if now == nil {
		now = time.Now
	}
	return &WeatherEvaluator{forecasts: forecasts, positions: positions, now: now, cached: make(map[string]cachedForecast)}, nil
}

// Resolve finds a named place once, when the watch or condition is saved, so each check asks only
// for its forecast.
func (e *WeatherEvaluator) Resolve(ctx context.Context, principalID string, spec Spec) (Spec, error) {
	if err := e.enabled(ctx, principalID); err != nil {
		return Spec{}, err
	}
	if spec.Target.Current {
		if e.positions == nil {
			return Spec{}, fmt.Errorf("the user's shared location is not available here; name the place instead")
		}
		return spec, nil
	}
	place, err := e.forecasts.FindPlace(ctx, principalID, spec.Target.Name)
	if err != nil {
		return Spec{}, err
	}
	latitude, longitude := place.Latitude, place.Longitude
	spec.Target.Name, spec.Target.Latitude, spec.Target.Longitude = place.Name, &latitude, &longitude
	return spec, nil
}

// Evaluate is rain or dry for the window ahead. A watch on current_location is skipped while the
// user shares no location, and every weather watch while their web access is off.
func (e *WeatherEvaluator) Evaluate(ctx context.Context, principalID string, watch Watch, occurrence Occurrence) (Observation, error) {
	enabled, err := e.forecasts.WeatherEnabled(ctx, principalID)
	if err != nil {
		return Observation{}, err
	}
	if !enabled {
		return Observation{}, fmt.Errorf("web access is switched off: %w", ErrSkip)
	}
	latitude, longitude, found, err := e.position(ctx, principalID, watch.Spec)
	if err != nil {
		return Observation{}, err
	}
	if !found {
		return Observation{}, fmt.Errorf("no shared location: %w", ErrSkip)
	}
	rain, slot, err := e.rain(ctx, principalID, watch.Spec, latitude, longitude, e.now())
	if err != nil {
		return Observation{}, err
	}
	if !rain {
		return Observation{State: weatherDry, Headline: dryHeadline(watch.Spec)}, nil
	}
	return Observation{State: weatherRain, Triggered: true, Headline: rainHeadline(watch.Spec, slot)}, nil
}

// CheckAt tells whether rain is coming at the instant, for a job's condition.
func (e *WeatherEvaluator) CheckAt(ctx context.Context, principalID string, spec Spec, at time.Time) (Verdict, error) {
	if err := e.enabled(ctx, principalID); err != nil {
		return Verdict{}, err
	}
	latitude, longitude, found, err := e.position(ctx, principalID, spec)
	if err != nil {
		return Verdict{}, err
	}
	if !found {
		return Verdict{}, fmt.Errorf("the user shares no location now")
	}
	rain, slot, err := e.rain(ctx, principalID, spec, latitude, longitude, at)
	if err != nil {
		return Verdict{}, err
	}
	if !rain {
		return Verdict{Value: dryHeadline(spec)}, nil
	}
	return Verdict{Met: true, Value: rainHeadline(spec, slot)}, nil
}

func (e *WeatherEvaluator) enabled(ctx context.Context, principalID string) error {
	enabled, err := e.forecasts.WeatherEnabled(ctx, principalID)
	if err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("web access is switched off in /settings, so the forecast cannot be read")
	}
	return nil
}

func (e *WeatherEvaluator) position(ctx context.Context, principalID string, spec Spec) (float64, float64, bool, error) {
	if !spec.Target.Current {
		if spec.Target.Latitude == nil || spec.Target.Longitude == nil {
			return 0, 0, false, fmt.Errorf("the place has no coordinates")
		}
		return *spec.Target.Latitude, *spec.Target.Longitude, true, nil
	}
	if e.positions == nil {
		return 0, 0, false, fmt.Errorf("the user's shared location is not available here")
	}
	return e.positions.CurrentPosition(ctx, principalID)
}

// rain reports the first slot in the window from at with the threshold or more, and whether
// there is one.
func (e *WeatherEvaluator) rain(ctx context.Context, principalID string, spec Spec, latitude, longitude float64, at time.Time) (bool, PrecipitationSlot, error) {
	weather := spec.Weather
	if weather == nil {
		return false, PrecipitationSlot{}, fmt.Errorf("the weather watch has no threshold")
	}
	slots, err := e.forecast(ctx, principalID, latitude, longitude)
	if err != nil {
		return false, PrecipitationSlot{}, &UpstreamError{Service: serviceWeather, Err: err}
	}
	until := at.Add(time.Duration(weather.WithinMinutes) * time.Minute)
	var covered time.Time
	for _, slot := range slots {
		if !slot.Start.Add(weatherSlot).After(at) || !slot.Start.Before(until) {
			continue
		}
		if slot.MM >= weather.ThresholdMM {
			return true, slot, nil
		}
		if end := slot.Start.Add(weatherSlot); end.After(covered) {
			covered = end
		}
	}
	// A forecast that stops short of the window cannot say it stays dry.
	if covered.Before(until) {
		return false, PrecipitationSlot{}, &UpstreamError{Service: serviceWeather, Err: fmt.Errorf("the forecast does not cover the next %d minutes", weather.WithinMinutes)}
	}
	return false, PrecipitationSlot{}, nil
}

// forecast is the forecast at the coordinates, fetched at most once per WeatherCheckInterval. The
// forecast is public data, so the cache is shared across principals; each still needs web access.
func (e *WeatherEvaluator) forecast(ctx context.Context, principalID string, latitude, longitude float64) ([]PrecipitationSlot, error) {
	key := strconv.FormatFloat(latitude, 'f', 3, 64) + "," + strconv.FormatFloat(longitude, 'f', 3, 64)
	now := e.now()
	e.mu.Lock()
	for cachedKey, entry := range e.cached {
		if now.Sub(entry.fetched) >= WeatherCheckInterval {
			delete(e.cached, cachedKey)
		}
	}
	entry, found := e.cached[key]
	e.mu.Unlock()
	if found {
		return entry.slots, nil
	}
	slots, err := e.forecasts.Precipitation(ctx, principalID, latitude, longitude)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.cached[key] = cachedForecast{fetched: now, slots: slots}
	e.mu.Unlock()
	return slots, nil
}

func rainHeadline(spec Spec, slot PrecipitationSlot) string {
	location := localtime.SavedZone(spec.Zone)
	return fmt.Sprintf("Rain expected at %s from %s: %s mm in 15 minutes, by the forecast.",
		spec.placeName(), slot.Start.In(location).Format("15:04"), strconv.FormatFloat(slot.MM, 'f', 1, 64))
}

func dryHeadline(spec Spec) string {
	within := DefaultWithinMinutes
	if spec.Weather != nil {
		within = spec.Weather.WithinMinutes
	}
	return fmt.Sprintf("No rain expected at %s in the next %d minutes, by the forecast.", spec.placeName(), within)
}

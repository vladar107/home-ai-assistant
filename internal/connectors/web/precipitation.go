package web

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// precipitationSlots is how many 15-minute slots one precipitation forecast covers: the six
	// hours a weather watch may look ahead, the slot the request falls in, and the 10 minutes a
	// forecast is reused, with a slot to spare.
	precipitationSlots = 27
	minutelyLayout     = "2006-01-02T15:04"
)

// Place is a named place as geocoding found it.
type Place struct {
	Name      string
	Latitude  float64
	Longitude float64
}

// PrecipitationSlot is the forecast precipitation in the 15 minutes from Start.
type PrecipitationSlot struct {
	Start time.Time
	MM    float64
}

// WeatherEnabled reports whether the principal's web access, which the forecast goes through, is
// on.
func (s *Service) WeatherEnabled(ctx context.Context, principalID string) (bool, error) {
	return s.switches.Enabled(ctx, principalID, Switch)
}

// FindPlace finds a place name as weather_forecast does: one of the principal's saved places, or
// else geocoded; an empty name is the household's home.
func (s *Service) FindPlace(ctx context.Context, principalID, name string) (Place, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = s.weatherHome
	}
	if name == "" {
		return Place{}, fmt.Errorf("name the place to watch the weather at")
	}
	if len([]rune(name)) > maximumPlaceRunes {
		return Place{}, fmt.Errorf("the place must be at most %d characters", maximumPlaceRunes)
	}
	if saved, found, err := s.savedPlace(ctx, principalID, name); err != nil || found {
		return saved, err
	}
	found, ok, err := s.geocode(ctx, name)
	if err != nil {
		s.logger.InfoContext(ctx, "web.weather.failed", "principal_id", principalID, "error_class", errorClass(err))
		return Place{}, fmt.Errorf("%s", weatherUnavailableText)
	}
	if !ok {
		return Place{}, fmt.Errorf("no place called %q was found; ask the user for a nearby town or a more precise name", name)
	}
	return Place{Name: placeName(found), Latitude: found.Latitude, Longitude: found.Longitude}, nil
}

// savedPlace is the principal's saved place with the name, if there is one.
func (s *Service) savedPlace(ctx context.Context, principalID, name string) (Place, bool, error) {
	if s.places == nil {
		return Place{}, false, nil
	}
	return s.places.FindSavedPlace(ctx, principalID, name)
}

// Precipitation is Open-Meteo's 15-minute precipitation forecast at the coordinates for the next
// six hours, in slots that start on the quarter hour.
func (s *Service) Precipitation(ctx context.Context, principalID string, latitude, longitude float64) ([]PrecipitationSlot, error) {
	if !validCoordinates(latitude, longitude) {
		return nil, fmt.Errorf("the coordinates are out of range")
	}
	query := url.Values{
		"latitude":             {strconv.FormatFloat(latitude, 'f', 4, 64)},
		"longitude":            {strconv.FormatFloat(longitude, 'f', 4, 64)},
		"minutely_15":          {"precipitation"},
		"forecast_minutely_15": {strconv.Itoa(precipitationSlots)},
		"timezone":             {"GMT"},
	}
	var result struct {
		Minutely *struct {
			Time          []string   `json:"time"`
			Precipitation []*float64 `json:"precipitation"`
		} `json:"minutely_15"`
	}
	if err := s.getJSON(ctx, s.forecastURL, query, &result); err != nil {
		// The request's own error names its URL, which carries the coordinates, so only its class
		// leaves here.
		class := errorClass(err)
		if errors.Is(err, errWeatherMalformed) {
			class = "unexpected_response"
		}
		s.logger.InfoContext(ctx, "web.weather.failed", "principal_id", principalID, "error_class", class)
		return nil, fmt.Errorf("the weather service failed: %s", class)
	}
	if result.Minutely == nil || len(result.Minutely.Time) == 0 || len(result.Minutely.Time) != len(result.Minutely.Precipitation) {
		return nil, errWeatherMalformed
	}
	slots := make([]PrecipitationSlot, 0, len(result.Minutely.Time))
	for index, value := range result.Minutely.Time {
		start, err := time.ParseInLocation(minutelyLayout, value, time.UTC)
		if err != nil {
			return nil, errWeatherMalformed
		}
		amount := result.Minutely.Precipitation[index]
		if amount == nil || math.IsNaN(*amount) || *amount < 0 {
			continue
		}
		slots = append(slots, PrecipitationSlot{Start: start, MM: *amount})
	}
	return slots, nil
}

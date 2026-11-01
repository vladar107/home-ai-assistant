package wiring

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
	userlocation "github.com/vladar107/home-ai-assistant/internal/location"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// weatherForecasts hands the web connector's forecast to weather watches and conditions.
type weatherForecasts struct {
	web *web.Service
}

func (f weatherForecasts) WeatherEnabled(ctx context.Context, principalID string) (bool, error) {
	return f.web.WeatherEnabled(ctx, principalID)
}

func (f weatherForecasts) FindPlace(ctx context.Context, principalID, name string) (watch.Place, error) {
	place, err := f.web.FindPlace(ctx, principalID, name)
	return watch.Place{Name: place.Name, Latitude: place.Latitude, Longitude: place.Longitude}, err
}

func (f weatherForecasts) Precipitation(ctx context.Context, principalID string, latitude, longitude float64) ([]watch.PrecipitationSlot, error) {
	slots, err := f.web.Precipitation(ctx, principalID, latitude, longitude)
	if err != nil {
		return nil, err
	}
	converted := make([]watch.PrecipitationSlot, 0, len(slots))
	for _, slot := range slots {
		converted = append(converted, watch.PrecipitationSlot{Start: slot.Start, MM: slot.MM})
	}
	return converted, nil
}

// sharedPositions hands the user's shared location to weather watches on current_location.
type sharedPositions struct {
	locations *userlocation.Service
}

func (p sharedPositions) CurrentPosition(ctx context.Context, principalID string) (float64, float64, bool, error) {
	fix, found, err := p.locations.Current(ctx, principalID)
	if err != nil || !found {
		return 0, 0, false, err
	}
	return fix.Latitude, fix.Longitude, true, nil
}

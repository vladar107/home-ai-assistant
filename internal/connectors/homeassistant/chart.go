package homeassistant

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chart"
)

// hourlyPeriods are the periods of at most 90 days, which a chart may read by the hour.
var hourlyPeriods = []string{"today", "yesterday", "this_week", "last_week", "this_month", "last_month", "last_7_days", "last_30_days", "last_90_days"}

var errNoStatistics = errors.New("no statistics for that period; a sensor keeps long-term statistics only when it has a state class")

var _ chart.HomeSource = (*Service)(nil)

// ChartSeries reads one exposed sensor's statistics for a chart: one value per bucket of the
// period, the period moved back by the query's shift, with the sensor's unit. Like ha_history it
// reaches exposed sensors only, and unlike it a failure never quotes another device's name, so
// the chart's result carries no text from the household's devices.
func (s *Service) ChartSeries(ctx context.Context, query chart.HomeQuery) (chart.Fetched, error) {
	if _, known := historyPeriods[query.Period]; !known {
		return chart.Fetched{}, errors.New("period is not one of the listed values")
	}
	if !slices.Contains(chart.HomeResolutions, query.Resolution) {
		return chart.Fetched{}, errors.New("resolution is not one of the listed values")
	}
	if query.Resolution == "hour" && !slices.Contains(hourlyPeriods, query.Period) {
		return chart.Fetched{}, errors.New("hour resolution covers periods of at most 90 days; use day for this period")
	}
	snap, err := s.current(ctx)
	if err != nil {
		return chart.Fetched{}, err
	}
	entityID, candidates := snap.resolveExposed(query.Sensor, "sensor")
	if entityID == "" && len(candidates) == 0 {
		if fresh := s.refreshOnMiss(ctx, snap); fresh != snap {
			snap = fresh
			entityID, candidates = snap.resolveExposed(query.Sensor, "sensor")
		}
	}
	if entityID == "" {
		if len(candidates) > 0 {
			return chart.Fetched{}, errors.New("several exposed sensors match that name; use one exact name from " + liveContextToolName)
		}
		return chart.Fetched{}, errors.New("no exposed sensor has that name; use a sensor name from " + liveContextToolName)
	}
	location := snap.location
	if location == nil {
		location = time.UTC
	}
	now := s.now()
	start, end := historyBounds(query.Period, now, location)
	// A period still under way ends now, and a shifted one at the same point of its own period, so
	// the two cover the same share of their periods.
	if end.After(now) {
		end = now.In(location)
	}
	start, end = query.Shift.Back(start), query.Shift.Back(end)
	statistics, err := s.statistics(ctx, entityID, start, end, query.Resolution)
	if err != nil {
		return chart.Fetched{}, err
	}
	if len(statistics) == 0 {
		return chart.Fetched{}, errNoStatistics
	}
	pick, changes := statisticOf(statistics)
	points := make([]chart.Point, 0, len(statistics))
	for _, statistic := range statistics {
		if value := pick(statistic); value != nil {
			points = append(points, chart.Point{At: time.UnixMilli(int64(statistic.Start)).In(location), Value: *value})
		}
	}
	if len(points) == 0 {
		return chart.Fetched{}, errNoStatistics
	}
	if query.Period == "all" {
		start = points[0].At
	}
	return chart.Fetched{Points: points, Unit: s.unitOf(ctx, entityID), Start: start, End: end, Additive: changes}, nil
}

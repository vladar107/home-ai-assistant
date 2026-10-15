package homeassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const maximumHistoryPoints = 60

// historyBuckets runs finest to coarsest: a period with too many points moves to the next one.
var historyBuckets = []string{"day", "week", "month"}

// historyEpoch precedes any recorded statistic, so "all" starts from the first one.
var historyEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// historyBounds converts a named period to explicit times in Home Assistant's time zone, because
// the per-bucket statistics command takes no calendar periods. Weeks start on Monday.
func historyBounds(period string, now time.Time, location *time.Location) (time.Time, time.Time) {
	now = now.In(location)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	week := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location)
	year := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, location)
	switch period {
	case "today":
		return day, day.AddDate(0, 0, 1)
	case "yesterday":
		return day.AddDate(0, 0, -1), day
	case "this_week":
		return week, week.AddDate(0, 0, 7)
	case "last_week":
		return week.AddDate(0, 0, -7), week
	case "this_month":
		return month, month.AddDate(0, 1, 0)
	case "last_month":
		return month.AddDate(0, -1, 0), month
	case "this_year":
		return year, year.AddDate(1, 0, 0)
	case "last_year":
		return year.AddDate(-1, 0, 0), year
	case "last_7_days":
		return now.AddDate(0, 0, -7), now
	case "last_30_days":
		return now.AddDate(0, 0, -30), now
	case "last_90_days":
		return now.AddDate(0, 0, -90), now
	case "last_365_days":
		return now.AddDate(0, 0, -365), now
	}
	return historyEpoch, now
}

type statisticPoint struct {
	Start  float64  `json:"start"`
	Mean   *float64 `json:"mean"`
	Change *float64 `json:"change"`
	State  *float64 `json:"state"`
}

// historyPoints answers a trend: one value per bucket from the statistics of the period, using the
// next coarser bucket while a bucket would give more than maximumHistoryPoints.
func (s *Service) historyPoints(ctx context.Context, snap *snapshot, arguments historyArguments, entityID string) tools.Result {
	location := snap.location
	if location == nil {
		location = time.UTC
	}
	start, end := historyBounds(arguments.Period, s.now(), location)
	label := strings.ReplaceAll(arguments.Period, "_", " ")

	var points []statisticPoint
	bucket := arguments.Points
	for index := slices.Index(historyBuckets, bucket); ; index++ {
		bucket = historyBuckets[index]
		results, err := s.client.websocketCommands(ctx, map[string]any{
			"type":          "recorder/statistics_during_period",
			"start_time":    start.Format(time.RFC3339),
			"end_time":      end.Format(time.RFC3339),
			"statistic_ids": []string{entityID},
			"period":        bucket,
			"types":         []string{"mean", "change", "state"},
		})
		if err != nil {
			return failure(err)
		}
		var series map[string][]statisticPoint
		if err := json.Unmarshal(results[0], &series); err != nil {
			return failure(errMalformed)
		}
		points = series[entityID]
		if len(points) <= maximumHistoryPoints || index == len(historyBuckets)-1 {
			break
		}
	}
	if len(points) == 0 {
		return tools.Result{Text: fmt.Sprintf("%s has no statistics for %s.", arguments.Name, label)}
	}

	pick := func(point statisticPoint) *float64 { return point.Mean }
	switch {
	case points[0].Mean != nil:
	case points[0].Change != nil:
		pick = func(point statisticPoint) *float64 { return point.Change }
	default:
		pick = func(point statisticPoint) *float64 { return point.State }
	}
	unit := ""
	var state stateObject
	if err := s.client.getJSON(ctx, "/api/states/"+entityID, &state); err == nil {
		unit = state.unit()
	}

	var lines []string
	var values []float64
	var days []string
	for _, point := range points {
		value := pick(point)
		if value == nil {
			continue
		}
		values = append(values, *value)
		days = append(days, time.UnixMilli(int64(point.Start)).In(location).Format("2006-01-02"))
	}
	if len(values) == 0 {
		return tools.Result{Text: fmt.Sprintf("%s has no statistics for %s.", arguments.Name, label)}
	}
	trimmed := false
	if len(values) > maximumHistoryPoints {
		values, days, trimmed = values[len(values)-maximumHistoryPoints:], days[len(days)-maximumHistoryPoints:], true
	}
	format := func(value float64) string { return strings.TrimSpace(formatNumber(value) + " " + unit) }
	for index, value := range values {
		lines = append(lines, days[index]+": "+format(value))
	}
	first, last := values[0], values[len(values)-1]
	lines = append(lines, "first: "+format(first), "last: "+format(last), "change: "+format(last-first))
	header := fmt.Sprintf("%s, %s by %s:", arguments.Name, label, bucket)
	if trimmed {
		header = fmt.Sprintf("%s, %s by %s (only the latest %d are shown):", arguments.Name, label, bucket, maximumHistoryPoints)
	}
	return tools.Result{Text: header + "\n" + strings.Join(lines, "\n")}
}

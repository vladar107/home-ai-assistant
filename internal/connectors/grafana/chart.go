package grafana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chart"
)

// maximumChartRange is longer than grafana_metrics allows, since a chart reads raw points rather
// than passing them to the model.
const maximumChartRange = 30 * 24 * time.Hour

var _ chart.MetricsSource = (*Service)(nil)

var errChartQuery = errors.New("the data source rejected the query; test it with " + toolMetrics)

// ChartSeries runs one PromQL range query for a chart over at most 30 days, at most
// maximumPoints samples, ending at the query's end or now moved back by its shift. The query must
// return one series, so the chart's labels stay the model's own rather than the metric's.
func (s *Service) ChartSeries(ctx context.Context, principalID string, query chart.MetricsQuery) (chart.Fetched, error) {
	if !s.Allowed(principalID) {
		return chart.Fetched{}, errors.New("the grafana source is not available")
	}
	expression, err := checkQuery(query.Query)
	if err != nil {
		return chart.Fetched{}, err
	}
	end, err := s.parseEnd(query.End)
	if err != nil {
		return chart.Fetched{}, err
	}
	w, err := s.windowWithin(query.Range, query.Shift.Back(end), query.Step, maximumChartRange, maximumPoints)
	if err != nil {
		return chart.Fetched{}, err
	}
	var response queryResponse
	if err := s.client.get(ctx, "/api/datasources/proxy/uid/"+prometheusUID+"/api/v1/query_range", url.Values{
		"query": {expression},
		"start": {unixSeconds(w.start)},
		"end":   {unixSeconds(w.end)},
		"step":  {strconv.FormatFloat(w.step.Seconds(), 'f', -1, 64)},
	}, &response); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			s.logger.InfoContext(ctx, "grafana.request.failed", "principal_id", principalID, "tool", "chart", "error_class", apiErr.class)
			// The data source's message can quote label values from devices and services, which a
			// chart's result never carries.
			if apiErr.class == "bad_query" {
				return chart.Fetched{}, errChartQuery
			}
		}
		return chart.Fetched{}, err
	}
	var found []series
	if response.Data.ResultType != "matrix" || json.Unmarshal(response.Data.Result, &found) != nil {
		return chart.Fetched{}, errMalformed
	}
	switch {
	case len(found) == 0:
		return chart.Fetched{}, errors.New("the query matched no series in that range; check the metric and label names against a dashboard panel's query")
	case len(found) > 1:
		return chart.Fetched{}, fmt.Errorf("the query returned %d series; aggregate them into one, such as with sum(), or chart each as its own series", len(found))
	}
	points := make([]chart.Point, 0, len(found[0].Values))
	for _, raw := range found[0].Values {
		at, value, ok := raw.decode()
		if !ok {
			return chart.Fetched{}, errMalformed
		}
		points = append(points, chart.Point{At: at.In(s.location), Value: value})
	}
	return chart.Fetched{Points: points, Start: w.start, End: w.end}, nil
}

package chart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolName = "chart"
	// toolTimeout covers up to four series fetched at once, each bounded by its source's own
	// request limit, and the rendering.
	toolTimeout = time.Minute

	maximumTitleRunes = 80
	maximumLabelRunes = 40
	maximumUnitRunes  = 12
	// MaximumValues bounds the points the model passes for one values series.
	MaximumValues = 200
	// maximumPoints bounds what one chart draws across its series, after fetching.
	maximumPoints  = 2000
	outliersShown  = 5
	dateLayout     = "Mon 2 Jan 2006"
	dateTimeLayout = "Mon 2 Jan 2006 15:04"

	SourceHome    = "ha"
	SourceMetrics = "grafana"
	SourceValues  = "values"

	noTurnText = "A chart can only be attached during a conversation turn."
)

// HomePeriods are the periods a Home Assistant series covers, in Home Assistant's time zone.
var HomePeriods = []string{"today", "yesterday", "this_week", "last_week", "this_month", "last_month", "this_year", "last_year", "last_7_days", "last_30_days", "last_90_days", "last_365_days", "all"}

// HomeResolutions are the statistics buckets a Home Assistant series is read at.
var HomeResolutions = []string{"hour", "day", "week", "month"}

// defaultResolutions keeps an unnamed resolution to between a few dozen and a few hundred points.
var defaultResolutions = map[string]string{
	"today": "hour", "yesterday": "hour", "this_week": "hour", "last_week": "hour", "last_7_days": "hour",
	"this_month": "day", "last_month": "day", "last_30_days": "day", "last_90_days": "day",
	"this_year": "week", "last_year": "week", "last_365_days": "week", "all": "month",
}

// HomeQuery names one exposed sensor's statistics over a period, the whole period moved back by
// the shift.
type HomeQuery struct {
	Sensor, Period, Resolution string
	Shift                      Shift
}

// MetricsQuery is one PromQL range query ending at End, or now, moved back by the shift.
type MetricsQuery struct {
	Query, Range, End, Step string
	Shift                   Shift
}

// Fetched is one series as its source returned it, at the times its values fell.
type Fetched struct {
	Points     []Point
	Unit       string
	Start, End time.Time
	// Additive marks values that are each what their bucket added, such as energy per day, whose
	// figure for the period is their total.
	Additive bool
}

// HomeSource reads Home Assistant's long-term statistics. Its errors are written for the model.
type HomeSource interface {
	integration.Switched
	ChartSeries(ctx context.Context, query HomeQuery) (Fetched, error)
}

// MetricsSource runs Prometheus range queries through Grafana for the principals the operator
// allowed. Its errors are written for the model.
type MetricsSource interface {
	integration.Switched
	Allowed(principalID string) bool
	ChartSeries(ctx context.Context, principalID string, query MetricsQuery) (Fetched, error)
}

// Images stages the chart for the reply of the run's turn, once per run.
type Images interface {
	StageOnce(once *outbox.Once, principalID, turn string, png []byte) error
}

// Telemetry records chart render metrics.
type Telemetry interface {
	RecordChartRender(ctx context.Context, outcome string, duration time.Duration)
}

type Config struct {
	Images   Images
	Switches integration.SwitchReader
	// Home and Metrics are the sources besides the model's own values; nil leaves one out.
	Home    HomeSource
	Metrics MetricsSource
	// Location is the zone the chart and its statistics show times in; nil means UTC.
	Location *time.Location
}

// Service is the chart tool's provider.
type Service struct {
	images    Images
	switches  integration.SwitchReader
	home      HomeSource
	metrics   MetricsSource
	location  *time.Location
	logger    *slog.Logger
	telemetry Telemetry
}

var _ tools.Provider = (*Service)(nil)

func NewService(cfg Config, logger *slog.Logger) (*Service, error) {
	switch {
	case cfg.Images == nil:
		return nil, errors.New("chart images are required")
	case cfg.Switches == nil:
		return nil, errors.New("chart switches are required")
	case logger == nil:
		return nil, errors.New("chart logger is required")
	}
	location := cfg.Location
	if location == nil {
		location = time.UTC
	}
	return &Service{images: cfg.Images, switches: cfg.Switches, home: cfg.Home, metrics: cfg.Metrics, location: location, logger: logger}, nil
}

// SetTelemetry sets where render metrics are recorded, at wiring time before turns start.
func (s *Service) SetTelemetry(telemetry Telemetry) {
	s.telemetry = telemetry
}

// Contribute offers the chart tool with the sources the principal may read now: their own values
// always, Home Assistant while their home switch is on, and Grafana when the operator allowed them
// and their Grafana switch is on.
func (s *Service) Contribute(ctx context.Context, principalID string) (tools.Contribution, error) {
	home, metrics, err := s.offered(ctx, principalID)
	if err != nil {
		return tools.Contribution{}, err
	}
	return tools.Contribution{Guidance: guidance(home, metrics), Definitions: []tools.Definition{definition(home, metrics)}}, nil
}

func (s *Service) offered(ctx context.Context, principalID string) (home, metrics bool, err error) {
	if s.home != nil {
		if home, err = s.switches.Enabled(ctx, principalID, s.home.Switch()); err != nil {
			return false, false, fmt.Errorf("read chart home switch: %w", err)
		}
	}
	if s.metrics != nil && s.metrics.Allowed(principalID) {
		if metrics, err = s.switches.Enabled(ctx, principalID, s.metrics.Switch()); err != nil {
			return false, false, fmt.Errorf("read chart grafana switch: %w", err)
		}
	}
	return home, metrics, nil
}

func guidance(home, metrics bool) string {
	var sources []string
	if home {
		sources = append(sources, SourceHome+" (an exposed sensor's name from ha_get_live_context, a period, and a resolution)")
	}
	if metrics {
		sources = append(sources, SourceMetrics+" (a PromQL query that returns one series, and a range of up to 30d)")
	}
	sources = append(sources, SourceValues+" (up to 200 points a tool returned in this turn, such as a table on a web page or rows from Notion)")
	text := "Charts: when the user asks to show, plot, or compare something over time, or what was unusual in it, call " + toolName + " instead of listing numbers; "
	if home || metrics {
		text += "keep "
		switch {
		case home && metrics:
			text += "ha_history and grafana_metrics"
		case home:
			text += "ha_history"
		default:
			text += "grafana_metrics"
		}
		text += " for a single figure. "
	} else {
		text = strings.TrimSuffix(text, "; ") + ". "
	}
	return text + toolName + " fetches each series itself from " + strings.Join(sources, ", or ") + ". " +
		"It draws up to four series, on two value axes when their units differ; give a series shift to draw the same series a week, month, or year earlier over the current one to compare. " +
		"Its result holds the numbers computed from every point: state them from there, never read off the picture, and keep the reply to about 800 characters, since it becomes the photo's caption. " +
		"A reply carries one image, so put everything in one chart, and never in a turn that also sends a day card."
}

func enum(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = `"` + value + `"`
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func definition(home, metrics bool) tools.Definition {
	sources := []string{SourceValues}
	properties := `"label":{"type":"string","maxLength":40,"description":"The series' name in the legend, in the language of your reply."},` +
		`"unit":{"type":"string","maxLength":12,"description":"The values' unit, such as °C or kWh; a Home Assistant sensor's own unit is used when omitted. Series with different units go on two axes, at most two units per chart."},` +
		`"shift":{"type":"string","enum":["1w","1M","1y"],"description":"Fetch this series one week, month, or year earlier and draw it dashed over the current period, to compare with an unshifted series."},`
	if home {
		sources = append([]string{SourceHome}, sources...)
		properties += `"sensor":{"type":"string","description":"ha: the sensor's name exactly as the live context gives it."},` +
			`"period":{"type":"string","enum":` + enum(HomePeriods) + `,"description":"ha: the period, in Home Assistant's time zone."},` +
			`"resolution":{"type":"string","enum":` + enum(HomeResolutions) + `,"description":"ha: one value per hour (periods up to 90 days), day, week, or month; omit for one that suits the period."},`
	}
	if metrics {
		position := 0
		if home {
			position = 1
		}
		sources = slices.Insert(sources, position, SourceMetrics)
		properties += `"query":{"type":"string","description":"grafana: a PromQL expression that returns one series; aggregate with sum or similar."},` +
			`"range":{"type":"string","description":"grafana: how far back from end, such as 6h, 7d, or 30d, at most 30d."},` +
			`"end":{"type":"string","description":"grafana: RFC 3339 time the range ends at; omit for now."},` +
			`"step":{"type":"string","description":"grafana: resolution such as 5m or 1h; omit for at most 500 points."},`
	}
	properties += `"points":{"type":"array","minItems":1,"maxItems":200,"description":"values: the points, from a tool result of this turn.","items":{"type":"object","properties":{` +
		`"time":{"type":"string","description":"RFC 3339, YYYY-MM-DDTHH:MM, YYYY-MM-DD, or YYYY-MM, in the household's time zone unless it has an offset."},` +
		`"value":{"type":"number"}},"required":["time","value"],"additionalProperties":false}},` +
		`"source":{"type":"string","enum":` + enum(sources) + `,"description":"Where the series comes from; give only that source's fields."}`
	return tools.Definition{
		Name:        toolName,
		Description: "Draw a chart of up to four time series as a picture sent with your reply, and compute each series' first and last value, change, minimum and maximum with their times, mean, total, gaps, and outliers.",
		Risk:        tools.RiskRead,
		Activity:    tools.ActivityImage,
		SourceLabel: tools.ArgumentLabel("title"),
		Timeout:     toolTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"title":{"type":"string","maxLength":80,"description":"The chart's title, in the language of your reply."},` +
			`"type":{"type":"string","enum":["line","step","bar"],"description":"line (the default) for measurements, step for set values such as a thermostat, bar for amounts per bucket such as energy per day."},` +
			`"series":{"type":"array","minItems":1,"maxItems":4,"items":{"type":"object","properties":{` + properties + `},` +
			`"required":["label","source"],"additionalProperties":false}}},` +
			`"required":["title","series"],"additionalProperties":false}`),
	}
}

type chartRequest struct {
	Title  string          `json:"title"`
	Type   string          `json:"type"`
	Series []seriesRequest `json:"series"`
}

type seriesRequest struct {
	Label      string         `json:"label"`
	Unit       string         `json:"unit"`
	Shift      string         `json:"shift"`
	Source     string         `json:"source"`
	Sensor     string         `json:"sensor"`
	Period     string         `json:"period"`
	Resolution string         `json:"resolution"`
	Query      string         `json:"query"`
	Range      string         `json:"range"`
	End        string         `json:"end"`
	Step       string         `json:"step"`
	Points     []valueRequest `json:"points"`
}

type valueRequest struct {
	Time  string   `json:"time"`
	Value *float64 `json:"value"`
}

// series is one requested series through fetching and summarising.
type series struct {
	request seriesRequest
	label   string
	unit    string
	shift   Shift
	fetched Fetched
	// points are the fetched points with a number, at the times they fell.
	points  []Point
	summary Summary
	drawn   int
}

// totals reports whether the series' values add up to something: amounts per bucket from Home
// Assistant, or values the model passed. A total of temperatures would mean nothing.
func (one *series) totals() bool {
	return one.fetched.Additive || one.request.Source == SourceValues
}

// key is what a shifted series shares with the unshifted one it is compared with: the same
// sensor or query, or the model's own values.
func (one *series) key() string {
	switch one.request.Source {
	case SourceHome:
		return SourceHome + "\x00" + strings.ToLower(strings.TrimSpace(one.request.Sensor))
	case SourceMetrics:
		return SourceMetrics + "\x00" + strings.Join(strings.Fields(one.request.Query), " ")
	default:
		return SourceValues
	}
}

func errorResult(text string) tools.Result {
	return tools.Result{Text: text, IsError: true}
}

func (s *Service) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if call.Name != toolName {
		return errorResult("This chart tool is not available."), nil
	}
	turn, run := tools.TurnFrom(ctx), tools.RunFrom(ctx)
	if turn == "" || run == nil {
		return errorResult(noTurnText), nil
	}
	var request chartRequest
	if err := tools.DecodeArguments(call.Arguments, &request); err != nil {
		return errorResult(err.Error()), nil
	}
	title := strings.TrimSpace(request.Title)
	if title == "" || len([]rune(title)) > maximumTitleRunes {
		return errorResult(fmt.Sprintf("A chart needs a title of at most %d characters.", maximumTitleRunes)), nil
	}
	kind := Kind(request.Type)
	if kind == "" {
		kind = Line
	}
	if kind != Line && kind != Step && kind != Bar {
		return errorResult(`type must be "line", "step", or "bar".`), nil
	}
	if len(request.Series) == 0 || len(request.Series) > MaximumSeries {
		return errorResult(fmt.Sprintf("A chart needs 1 to %d series.", MaximumSeries)), nil
	}
	all := make([]*series, len(request.Series))
	for index, requested := range request.Series {
		one, err := s.checkSeries(ctx, principalID, requested)
		if err != nil {
			return errorResult(fmt.Sprintf("Series %d: %s", index+1, err)), nil
		}
		all[index] = one
	}
	if failed, err := s.fetchAll(ctx, principalID, all); err != nil {
		s.logger.InfoContext(ctx, "chart.source.failed", "principal_id", principalID, "source", all[failed].request.Source)
		return errorResult(fmt.Sprintf("Series %d (%s): %s", failed+1, all[failed].label, err)), nil
	}

	c := Chart{Title: title, Kind: kind, Location: s.location}
	budget := maximumPoints / len(all)
	for index, one := range all {
		if one.unit == "" {
			one.unit = sourceUnit(one.fetched.Unit)
		}
		points := finite(one.fetched.Points)
		if len(points) == 0 {
			return errorResult(fmt.Sprintf("Series %d (%s): no history with a number in that period.", index+1, one.label)), nil
		}
		for _, point := range points {
			if !Chartable(point.Value) {
				return errorResult(fmt.Sprintf("Series %d (%s): %s.", index+1, one.label, ErrValueRange)), nil
			}
		}
		one.points = points
		one.summary, _ = Summarize(points)
		drawn := Downsample(points, budget)
		one.drawn = len(drawn)
		shown := make([]Point, len(drawn))
		for position, point := range drawn {
			shown[position] = Point{At: one.shift.Forward(point.At.In(s.location)), Value: point.Value}
		}
		c.Series = append(c.Series, Series{Label: one.label, Unit: one.unit, Points: shown, Dashed: one.shift != NoShift})
	}
	if len(c.units()) > MaximumUnits {
		return errorResult("A chart shows at most two units; chart the series with a third unit separately."), nil
	}

	started := time.Now()
	png, err := Render(c)
	if s.telemetry != nil {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		s.telemetry.RecordChartRender(ctx, outcome, time.Since(started))
	}
	if err != nil {
		return tools.Result{}, fmt.Errorf("render chart: %w", err)
	}
	if err := s.images.StageOnce(run.Image(), principalID, turn, png); err != nil {
		if errors.Is(err, outbox.ErrImageStaged) {
			return errorResult(err.Error()), nil
		}
		return tools.Result{}, fmt.Errorf("stage chart: %w", err)
	}
	s.logger.InfoContext(ctx, "chart.staged", "principal_id", principalID, "series", len(all), "kind", string(kind), "bytes", len(png))
	return tools.Result{Text: s.report(all)}, nil
}

// checkSeries validates one series and that its source is open to the principal now; the switch
// is read again here because it may have changed since the turn's tools were collected.
func (s *Service) checkSeries(ctx context.Context, principalID string, requested seriesRequest) (*series, error) {
	one := &series{request: requested, label: strings.Join(strings.Fields(requested.Label), " "), unit: strings.TrimSpace(requested.Unit), shift: Shift(requested.Shift)}
	if one.label == "" || len([]rune(one.label)) > maximumLabelRunes {
		return nil, fmt.Errorf("give the series a label of at most %d characters", maximumLabelRunes)
	}
	if len([]rune(one.unit)) > maximumUnitRunes {
		return nil, fmt.Errorf("unit must be at most %d characters", maximumUnitRunes)
	}
	if one.shift != NoShift && !slices.Contains(Shifts, one.shift) {
		return nil, errors.New(`shift must be "1w", "1M", or "1y"`)
	}
	given := map[string]bool{
		"sensor": requested.Sensor != "", "period": requested.Period != "", "resolution": requested.Resolution != "",
		"query": requested.Query != "", "range": requested.Range != "", "end": requested.End != "", "step": requested.Step != "",
		"points": len(requested.Points) > 0,
	}
	fields := map[string][]string{
		SourceHome:    {"sensor", "period", "resolution"},
		SourceMetrics: {"query", "range", "end", "step"},
		SourceValues:  {"points"},
	}
	own, known := fields[requested.Source]
	if !known {
		return nil, fmt.Errorf("source must be one of %s", strings.Join(s.sourceNames(ctx, principalID), ", "))
	}
	for _, field := range slices.Sorted(maps.Keys(given)) {
		if given[field] && !slices.Contains(own, field) {
			return nil, fmt.Errorf("%s does not belong to the %s source", field, requested.Source)
		}
	}
	switch requested.Source {
	case SourceHome:
		if s.home == nil {
			return nil, errors.New("the ha source is not available")
		}
		if err := s.switchedOn(ctx, principalID, s.home.Switch()); err != nil {
			return nil, err
		}
		if strings.TrimSpace(requested.Sensor) == "" || requested.Period == "" {
			return nil, errors.New("the ha source needs a sensor and a period")
		}
		if !slices.Contains(HomePeriods, requested.Period) {
			return nil, errors.New("period is not one of the listed values")
		}
		if requested.Resolution != "" && !slices.Contains(HomeResolutions, requested.Resolution) {
			return nil, errors.New("resolution is not one of the listed values")
		}
		if requested.Resolution == "" {
			one.request.Resolution = defaultResolutions[requested.Period]
		}
	case SourceMetrics:
		if s.metrics == nil || !s.metrics.Allowed(principalID) {
			return nil, errors.New("the grafana source is not available")
		}
		if err := s.switchedOn(ctx, principalID, s.metrics.Switch()); err != nil {
			return nil, err
		}
		if strings.TrimSpace(requested.Query) == "" || requested.Range == "" {
			return nil, errors.New("the grafana source needs a query and a range")
		}
	case SourceValues:
		if len(requested.Points) == 0 || len(requested.Points) > MaximumValues {
			return nil, fmt.Errorf("the values source needs 1 to %d points", MaximumValues)
		}
	}
	return one, nil
}

func (s *Service) sourceNames(ctx context.Context, principalID string) []string {
	home, metrics, err := s.offered(ctx, principalID)
	names := []string{SourceValues}
	if err == nil && metrics {
		names = append([]string{SourceMetrics}, names...)
	}
	if err == nil && home {
		names = append([]string{SourceHome}, names...)
	}
	return names
}

func (s *Service) switchedOn(ctx context.Context, principalID string, sw integration.Switch) error {
	enabled, err := s.switches.Enabled(ctx, principalID, sw)
	if err != nil {
		return fmt.Errorf("%s cannot be checked right now", sw.Name)
	}
	if !enabled {
		return errors.New(strings.TrimSuffix(sw.OffText(), "."))
	}
	return nil
}

// fetchAll fetches every series at once, so the slowest bounds the wait, and reports the first
// failure in the chart's order.
func (s *Service) fetchAll(ctx context.Context, principalID string, all []*series) (int, error) {
	failures := make([]error, len(all))
	var group sync.WaitGroup
	for index, one := range all {
		group.Go(func() {
			one.fetched, failures[index] = s.fetch(ctx, principalID, one)
		})
	}
	group.Wait()
	for index, err := range failures {
		if err != nil {
			return index, err
		}
	}
	return 0, nil
}

func (s *Service) fetch(ctx context.Context, principalID string, one *series) (Fetched, error) {
	requested := one.request
	switch requested.Source {
	case SourceHome:
		return s.home.ChartSeries(ctx, HomeQuery{Sensor: strings.TrimSpace(requested.Sensor), Period: requested.Period, Resolution: requested.Resolution, Shift: one.shift})
	case SourceMetrics:
		return s.metrics.ChartSeries(ctx, principalID, MetricsQuery{Query: requested.Query, Range: requested.Range, End: requested.End, Step: requested.Step, Shift: one.shift})
	default:
		return s.values(requested.Points)
	}
}

var valueLayouts = []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02", "2006-01"}

func (s *Service) values(requested []valueRequest) (Fetched, error) {
	points := make([]Point, 0, len(requested))
	for index, value := range requested {
		if value.Value == nil {
			return Fetched{}, fmt.Errorf("point %d has no value", index+1)
		}
		if !Chartable(*value.Value) {
			return Fetched{}, fmt.Errorf("point %d: a value must be 0 or between %g and %g in size", index+1, MinimumMagnitude, MaximumMagnitude)
		}
		at, ok := s.parseTime(strings.TrimSpace(value.Time))
		if !ok {
			return Fetched{}, fmt.Errorf("point %d: time must be RFC 3339, YYYY-MM-DDTHH:MM, YYYY-MM-DD, or YYYY-MM", index+1)
		}
		points = append(points, Point{At: at, Value: *value.Value})
	}
	slices.SortStableFunc(points, func(a, b Point) int { return a.At.Compare(b.At) })
	return Fetched{Points: points, Start: points[0].At, End: points[len(points)-1].At}, nil
}

func (s *Service) parseTime(value string) (time.Time, bool) {
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at, true
	}
	for _, layout := range valueLayouts {
		if at, err := time.ParseInLocation(layout, value, s.location); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

// sourceUnit keeps a source's unit to a few printable characters, since it is the one text a
// source adds to the result.
func sourceUnit(unit string) string {
	var kept []rune
	for _, r := range strings.TrimSpace(unit) {
		if unicode.IsPrint(r) && len(kept) < maximumUnitRunes {
			kept = append(kept, r)
		}
	}
	return strings.TrimSpace(string(kept))
}

func finite(points []Point) []Point {
	kept := make([]Point, 0, len(points))
	for _, point := range points {
		if !math.IsNaN(point.Value) && !math.IsInf(point.Value, 0) {
			kept = append(kept, point)
		}
	}
	return kept
}

// report is the tool result: every number the reply may state, computed from all fetched points.
func (s *Service) report(all []*series) string {
	var out strings.Builder
	out.WriteString("The chart is ready and goes out as a photo with your reply, which becomes its caption; it is this reply's one image. " +
		"Every number below comes from all the points fetched, with times in " + s.location.String() + ":\n")
	for index, one := range all {
		s.describe(&out, index, one)
	}
	for _, one := range all {
		if one.shift == NoShift {
			continue
		}
		for _, base := range all {
			if base.shift == NoShift && base.key() == one.key() {
				s.compare(&out, base, one)
				break
			}
		}
	}
	out.WriteString("State these numbers in the reply rather than reading them off the chart, and keep the reply to about 800 characters.")
	return out.String()
}

// compare sets a shifted series against the unshifted one over only the span both cover once
// shifted, so a period still under way is not set against the whole of an earlier one.
func (s *Service) compare(out *strings.Builder, base, earlier *series) {
	from := base.points[0].At
	if start := earlier.shift.Forward(earlier.points[0].At); start.After(from) {
		from = start
	}
	to := base.points[len(base.points)-1].At
	if end := earlier.shift.Forward(earlier.points[len(earlier.points)-1].At); end.Before(to) {
		to = end
	}
	covered := func(points []Point, shift Shift) []Point {
		var kept []Point
		for _, point := range points {
			if at := shift.Forward(point.At); !at.Before(from) && !at.After(to) {
				kept = append(kept, point)
			}
		}
		return kept
	}
	current, past := covered(base.points, NoShift), covered(earlier.points, earlier.shift)
	fmt.Fprintf(out, "Comparison of %q with %q, %s earlier", base.label, earlier.label, earlier.shift.words())
	if len(current) == 0 || len(past) == 0 {
		out.WriteString(": the two share no span to compare.\n")
		return
	}
	now, then := summaryOf(current), summaryOf(past)
	fmt.Fprintf(out, ", over the span both cover, %s to %s, with %d and %d points: mean %s against %s, %s",
		from.In(s.location).Format(dateTimeLayout), to.In(s.location).Format(dateTimeLayout), len(current), len(past),
		formatNumber(now.Mean), formatNumber(then.Mean), difference(now.Mean, then.Mean))
	if base.totals() && earlier.totals() {
		fmt.Fprintf(out, "; total %s against %s, %s", formatNumber(now.Total), formatNumber(then.Total), difference(now.Total, then.Total))
	}
	out.WriteString(".\n")
}

func summaryOf(points []Point) Summary {
	summary, _ := Summarize(points)
	return summary
}

func (s *Service) describe(out *strings.Builder, index int, one *series) {
	summary := one.summary
	layout := dateLayout
	for _, point := range one.fetched.Points {
		local := point.At.In(s.location)
		if local.Hour() != 0 || local.Minute() != 0 {
			layout = dateTimeLayout
			break
		}
	}
	at := func(point Point) string { return point.At.In(s.location).Format(layout) }
	value := func(number float64) string { return strings.TrimSpace(formatNumber(number) + " " + one.unit) }

	fmt.Fprintf(out, "%d. %q", index+1, one.label)
	switch one.request.Source {
	case SourceHome:
		fmt.Fprintf(out, " from the sensor %q, %s by %s", one.request.Sensor, strings.ReplaceAll(one.request.Period, "_", " "), one.request.Resolution)
	case SourceMetrics:
		out.WriteString(" from Prometheus")
	default:
		out.WriteString(" from the values given")
	}
	if one.shift != NoShift {
		fmt.Fprintf(out, ", %s earlier, drawn dashed %s later over the current period", one.shift.words(), one.shift.words())
	}
	if !one.fetched.Start.IsZero() && !one.fetched.End.IsZero() && one.request.Source != SourceValues {
		fmt.Fprintf(out, " (%s to %s)", one.fetched.Start.In(s.location).Format(dateTimeLayout), one.fetched.End.In(s.location).Format(dateTimeLayout))
	}
	fmt.Fprintf(out, ": %d points", summary.Count)
	if one.drawn < summary.Count {
		fmt.Fprintf(out, ", drawn as %d bucket means", one.drawn)
	}
	out.WriteString(".\n   ")
	if one.fetched.Additive {
		fmt.Fprintf(out, "Each point is what its %s added: total %s; per %s mean %s, min %s on %s, max %s on %s; first %s on %s, last %s on %s.",
			one.request.Resolution, value(summary.Total), one.request.Resolution, value(summary.Mean),
			value(summary.Min.Value), at(summary.Min), value(summary.Max.Value), at(summary.Max),
			value(summary.First.Value), at(summary.First), value(summary.Last.Value), at(summary.Last))
	} else {
		fmt.Fprintf(out, "First %s on %s, last %s on %s, change %s; min %s on %s, max %s on %s; mean %s",
			value(summary.First.Value), at(summary.First), value(summary.Last.Value), at(summary.Last), signed(summary.Last.Value-summary.First.Value),
			value(summary.Min.Value), at(summary.Min), value(summary.Max.Value), at(summary.Max),
			value(summary.Mean))
		if one.totals() {
			fmt.Fprintf(out, "; total %s", value(summary.Total))
		}
		out.WriteString(".")
	}
	switch summary.Gaps {
	case 0:
	case 1:
		out.WriteString(" 1 gap where points are missing.")
	default:
		fmt.Fprintf(out, " %d gaps where points are missing.", summary.Gaps)
	}
	if len(summary.Outliers) == 0 {
		out.WriteString(" No outliers.\n")
		return
	}
	shown := make([]string, 0, outliersShown)
	for _, point := range summary.Outliers[:min(outliersShown, len(summary.Outliers))] {
		shown = append(shown, value(point.Value)+" on "+at(point))
	}
	more := ""
	if extra := len(summary.Outliers) - len(shown); extra > 0 {
		more = fmt.Sprintf(" and %d more", extra)
	}
	spread := "median absolute deviations"
	if summary.MeanDeviation {
		spread = "times the mean absolute deviation scaled by 1.2533, as most points equal the median,"
	}
	fmt.Fprintf(out, " Outliers, more than %d %s (%s) from the median %s: %s%s.\n",
		outlierDeviations, spread, formatNumber(summary.Deviation), value(summary.Median), strings.Join(shown, ", "), more)
}

func signed(value float64) string {
	text := formatNumber(value)
	if value > 0 && text != "0" {
		return "+" + text
	}
	return text
}

// difference is current minus earlier, and as a share of earlier when that is not zero.
func difference(current, earlier float64) string {
	text := "a difference of " + signed(current-earlier)
	if earlier != 0 {
		text += fmt.Sprintf(" (%s%%)", signed(math.Round((current-earlier)/math.Abs(earlier)*1000)/10))
	}
	return text
}

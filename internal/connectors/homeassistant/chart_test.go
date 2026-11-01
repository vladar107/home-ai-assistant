package homeassistant_test

import (
	"bytes"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chart"
	"github.com/vladar107/home-ai-assistant/internal/connectors/homeassistant"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	chartTurnID       = "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
	hallTemperature   = "- names: Hall temperature\n  domain: sensor\n  state: '21.2'\n  attributes:\n    unit_of_measurement: °C\n"
	hallTemperatureID = "sensor.hall_temperature"
)

// within keeps the statistics that start in [start, end), as Home Assistant answers a period.
func within(points []map[string]any, start, end string) []map[string]any {
	from, errFrom := time.Parse(time.RFC3339, start)
	to, errTo := time.Parse(time.RFC3339, end)
	if errFrom != nil || errTo != nil {
		return nil
	}
	var kept []map[string]any
	for _, point := range points {
		at := time.UnixMilli(point["start"].(int64))
		if !at.Before(from) && at.Before(to) {
			kept = append(kept, point)
		}
	}
	return kept
}

func newChartFixture(t *testing.T) (*fixture, *outbox.Box) {
	t.Helper()
	f := newFixture(t, newSimulator(livingTV, solarToday, hallTemperature))
	f.sim.set(func(s *simulator) {
		s.states = append(s.states, map[string]any{"entity_id": hallTemperatureID, "state": "21.2", "attributes": map[string]any{"friendly_name": "Hall temperature", "unit_of_measurement": "°C"}})
	})
	box, err := outbox.New(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	charts, err := chart.NewService(chart.Config{Images: box, Switches: f.switches, Home: f.service}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.broker.Register(charts); err != nil {
		t.Fatal(err)
	}
	return f, box
}

func (f *fixture) chartSet(principalID string) agentruntime.ToolSet {
	f.t.Helper()
	set, err := f.broker.ToolsFor(tools.WithRun(tools.WithTurn(f.t.Context(), outbox.ChatTurn(chartTurnID)), tools.NewRun("")), principalID)
	if err != nil {
		f.t.Fatal(err)
	}
	return set
}

func stagedChart(t *testing.T, box *outbox.Box, principalID string) {
	t.Helper()
	card, found, err := box.Card(principalID, outbox.ChatTurn(chartTurnID))
	if err != nil || !found {
		t.Fatalf("staged chart = %v %v", found, err)
	}
	decoded, err := png.Decode(bytes.NewReader(card.PNG))
	if err != nil {
		t.Fatal(err)
	}
	if size := decoded.Bounds().Size(); size.X != chart.Width || size.Y != chart.Height {
		t.Fatalf("chart size = %v", size)
	}
}

func TestChartFetchesHourlyStatisticsDownsamplesThemAndStagesOnePNG(t *testing.T) {
	f, box := newChartFixture(t)
	first := start.AddDate(0, 0, -90)
	var hours []map[string]any
	for hour := range 90 * 24 {
		if hour >= 1500 && hour < 1530 {
			continue
		}
		value := 20 + float64(hour%24)/10
		if hour == 1000 {
			value = 60
		}
		hours = append(hours, map[string]any{"start": first.Add(time.Duration(hour) * time.Hour).UnixMilli(), "mean": value})
	}
	f.sim.set(func(s *simulator) {
		s.series = map[string]map[string][]map[string]any{hallTemperatureID: {"hour": hours}}
	})

	result := f.call(f.chartSet("alice"), "chart", map[string]any{
		"title":  "Hall temperature, last 90 days",
		"series": []map[string]any{{"label": "Hall", "source": "ha", "sensor": "Hall temperature", "period": "last_90_days", "resolution": "hour"}},
	})
	if result.IsError {
		t.Fatalf("result = %+v", result)
	}
	for _, want := range []string{
		`1. "Hall" from the sensor "Hall temperature", last 90 days by hour (Sat 4 Jul 2026 08:00 to Fri 2 Oct 2026 08:00): 2130 points, drawn as `,
		"min 20 °C on Sat 4 Jul 2026 08:00, max 60 °C on Sat 15 Aug 2026 00:00",
		"1 gap where points are missing.",
		"(0.6) from the median 21.1 °C: 60 °C on Sat 15 Aug 2026 00:00.",
	} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("result lacks %q:\n%s", want, result.Text)
		}
	}
	stagedChart(t, box, "alice")
	if _, found, _ := box.Card("bob", outbox.ChatTurn(chartTurnID)); found {
		t.Fatal("another principal's turn has a chart")
	}
	f.sim.mu.Lock()
	command := f.sim.statCommands[0]
	f.sim.mu.Unlock()
	if command["period"] != "hour" || command["start_time"] != "2026-07-04T08:00:00Z" || command["end_time"] != "2026-10-02T08:00:00Z" {
		t.Fatalf("command = %+v", command)
	}
}

// TestChartComparesTheElapsedPartOfAMonth compares this month, two days in, with the same two
// days of the month before, and compares a shifted series only with its own sensor.
func TestChartComparesTheElapsedPartOfAMonth(t *testing.T) {
	f, box := newChartFixture(t)
	var days []map[string]any
	for day := range 30 {
		days = append(days, map[string]any{"start": time.Date(2026, 9, 1+day, 0, 0, 0, 0, time.UTC).UnixMilli(), "change": 8.0})
	}
	for day, change := range []float64{10, 12} {
		days = append(days, map[string]any{"start": time.Date(2026, 10, 1+day, 0, 0, 0, 0, time.UTC).UnixMilli(), "change": change})
	}
	f.sim.set(func(s *simulator) {
		s.windowed = true
		s.series = map[string]map[string][]map[string]any{
			"sensor.solar_production_today": {"day": days},
			hallTemperatureID:               {"day": weightSeries(5, func(i int) time.Time { return time.Date(2026, 9, 1+i, 0, 0, 0, 0, time.UTC) })},
		}
		// A unit is the one text a sensor adds to the result, so it is cut to a few printable
		// characters.
		s.states[len(s.states)-1]["attributes"] = map[string]any{"friendly_name": "Hall temperature", "unit_of_measurement": "°C\u0007 and some long text"}
	})

	result := f.call(f.chartSet("alice"), "chart", map[string]any{
		"title": "Solar production",
		"type":  "bar",
		"series": []map[string]any{
			{"label": "October", "source": "ha", "sensor": "Solar production today", "period": "this_month"},
			{"label": "September", "source": "ha", "sensor": "Solar production today", "period": "this_month", "shift": "1M"},
			{"label": "Hall in September", "source": "ha", "sensor": "Hall temperature", "period": "this_month", "shift": "1M"},
		},
	})
	if result.IsError {
		t.Fatalf("result = %+v", result)
	}
	for _, want := range []string{
		"Each point is what its day added: total 22 kWh;",
		`2. "September" from the sensor "Solar production today", this month by day, 1 month earlier, drawn dashed 1 month later over the current period (Tue 1 Sep 2026 00:00 to Wed 2 Sep 2026 08:00): 2 points.`,
		"Each point is what its day added: total 16 kWh;",
		"mean 79.75 °C and some. No outliers.",
		`Comparison of "October" with "September", 1 month earlier, over the span both cover, Thu 1 Oct 2026 00:00 to Fri 2 Oct 2026 00:00, with 2 and 2 points: mean 11 against 8, a difference of +3 (+37.5%); total 22 against 16, a difference of +6 (+37.5%).`,
	} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("result lacks %q:\n%s", want, result.Text)
		}
	}
	if strings.Count(result.Text, "Comparison of") != 1 || strings.Contains(result.Text, "long text") || strings.Contains(result.Text, "\u0007") {
		t.Fatalf("result = %s", result.Text)
	}
	if hall := result.Text[strings.Index(result.Text, "3. "):strings.Index(result.Text, "Comparison of")]; strings.Contains(hall, "total") {
		t.Fatalf("a temperature has a total: %s", result.Text)
	}
	stagedChart(t, box, "alice")
	f.sim.mu.Lock()
	commands := slices.Clone(f.sim.statCommands)
	f.sim.mu.Unlock()
	var windows []string
	for _, command := range commands {
		windows = append(windows, command["start_time"].(string)+" "+command["end_time"].(string))
	}
	slices.Sort(windows)
	if want := []string{"2026-09-01T00:00:00Z 2026-09-02T08:00:00Z", "2026-09-01T00:00:00Z 2026-09-02T08:00:00Z", "2026-10-01T00:00:00Z 2026-10-02T08:00:00Z"}; !slices.Equal(windows, want) {
		t.Fatalf("windows = %v", windows)
	}
}

func TestChartUsesOnlyTheCallingPrincipalsHomeSwitch(t *testing.T) {
	f, box := newChartFixture(t)
	f.sim.set(func(s *simulator) {
		s.series = map[string]map[string][]map[string]any{hallTemperatureID: {"day": weightSeries(5, func(i int) time.Time { return start.AddDate(0, 0, i-5) })}}
	})
	set := f.chartSet("alice")
	if err := f.switches.SetEnabled(t.Context(), "alice", homeassistant.Switch, false); err != nil {
		t.Fatal(err)
	}
	arguments := map[string]any{
		"title":  "Hall",
		"series": []map[string]any{{"label": "Hall", "source": "ha", "sensor": "Hall temperature", "period": "last_7_days", "resolution": "day"}},
	}
	wantError(t, f.call(set, "chart", arguments), "Series 1: The home is off for this user")
	if _, found, _ := box.Card("alice", outbox.ChatTurn(chartTurnID)); found {
		t.Fatal("a chart was staged with the home off")
	}
	f.sim.mu.Lock()
	asked := len(f.sim.statCommands)
	f.sim.mu.Unlock()
	if asked != 0 {
		t.Fatalf("statistics were read with the home off: %d", asked)
	}
	for _, definition := range f.chartSet("alice").Definitions {
		if definition.Name == "chart" && strings.Contains(string(definition.InputSchema), `"sensor"`) {
			t.Fatalf("the ha source is offered with the home off: %s", definition.InputSchema)
		}
	}

	if result := f.call(f.chartSet("bob"), "chart", arguments); result.IsError {
		t.Fatalf("bob's chart = %+v", result)
	}
	stagedChart(t, box, "bob")
}

func TestChartRefusesHourlyStatisticsOverAYear(t *testing.T) {
	f, _ := newChartFixture(t)
	result := f.call(f.chartSet("alice"), "chart", map[string]any{
		"title":  "Hall",
		"series": []map[string]any{{"label": "Hall", "source": "ha", "sensor": "Hall temperature", "period": "last_year", "resolution": "hour"}},
	})
	wantError(t, result, "hour resolution covers periods of at most 90 days")
	wantError(t, f.call(f.chartSet("alice"), "chart", map[string]any{
		"title":  "Hall",
		"series": []map[string]any{{"label": "Hall", "source": "ha", "sensor": "Nowhere", "period": "today"}},
	}), "no exposed sensor has that name")
}

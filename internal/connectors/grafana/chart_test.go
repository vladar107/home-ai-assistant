package grafana_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chart"
	"github.com/vladar107/home-ai-assistant/internal/connectors/grafana"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const chartTurnID = "6b5a4c3d-2e1f-4b0a-9d8c-7f6e5d4c3b2a"

// samples answers a range query with one series, or with one per instance when the query groups
// by instance, at the query's start and every step after it.
func samples(values ...string) func(url.Values) string {
	return func(query url.Values) string {
		var start, step float64
		_, _ = fmt.Sscan(query.Get("start"), &start)
		_, _ = fmt.Sscan(query.Get("step"), &step)
		var points []string
		for index, value := range values {
			points = append(points, fmt.Sprintf(`[%g,"%s"]`, start+float64(index)*step, value))
		}
		one := `{"metric":{"instance":"%s"},"values":[` + strings.Join(points, ",") + `]}`
		result := fmt.Sprintf(one, "nas")
		if strings.Contains(query.Get("query"), "by (instance)") {
			result += "," + fmt.Sprintf(one, "router")
		}
		return envelope("matrix", "["+result+"]")
	}
}

func newChartFixture(t *testing.T) (*fixture, *outbox.Box) {
	t.Helper()
	sim := newSimulator()
	sim.prometheus = samples("1", "3", "2", "NaN")
	f := newFixture(t, sim)
	box, err := outbox.New(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	charts, err := chart.NewService(chart.Config{Images: box, Switches: f.switches, Metrics: f.service, Location: amsterdam}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func metricChart(series ...map[string]any) map[string]any {
	return map[string]any{"title": "NAS load", "series": series}
}

func TestChartReadsOneSeriesOverThirtyDaysAtMostFiveHundredPoints(t *testing.T) {
	f, box := newChartFixture(t)
	result := f.call(f.chartSet("alice"), "chart", metricChart(
		map[string]any{"label": "Load", "source": "grafana", "query": `max(node_load1{instance="nas"})`, "range": "30d"},
	))
	if result.IsError {
		t.Fatalf("result = %+v", result)
	}
	for _, want := range []string{
		`1. "Load" from Prometheus (Wed 2 Sep 2026 10:00 to Fri 2 Oct 2026 10:00): 3 points.`,
		"First 1 on Wed 2 Sep 2026 10:00, last 2 on Wed 2 Sep 2026 12:52, change +1; min 1 on Wed 2 Sep 2026 10:00, max 3 on Wed 2 Sep 2026 11:26; mean 2. No outliers.",
	} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("result lacks %q:\n%s", want, result.Text)
		}
	}
	request := f.sim.last()
	if request.path != "/api/datasources/proxy/uid/prometheus/api/v1/query_range" ||
		request.query.Get("start") != fmt.Sprint(now.Add(-30*24*time.Hour).Unix()) || request.query.Get("end") != fmt.Sprint(now.Unix()) ||
		request.query.Get("step") != "5184" {
		t.Fatalf("request = %+v", request)
	}
	if _, found, err := box.Card("alice", outbox.ChatTurn(chartTurnID)); !found || err != nil {
		t.Fatalf("staged = %v %v", found, err)
	}

	f.call(f.chartSet("alice"), "chart", metricChart(
		map[string]any{"label": "Last week", "source": "grafana", "query": "node_load1", "range": "1d", "shift": "1w"},
	))
	if request := f.sim.last(); request.query.Get("end") != fmt.Sprint(now.AddDate(0, 0, -7).Unix()) {
		t.Fatalf("a shifted range ends at %s", request.query.Get("end"))
	}
}

func TestChartRefusesWhatGrafanaMetricsWouldNotChart(t *testing.T) {
	f, _ := newChartFixture(t)
	set := f.chartSet("alice")
	before := f.sim.count()
	wantError(t, f.call(set, "chart", metricChart(map[string]any{"label": "Load", "source": "grafana", "query": "node_load1", "range": "31d"})), "between 1m and 30d")
	if f.sim.count() != before {
		t.Fatal("an over-long range reached Prometheus")
	}
	wantError(t, f.call(set, "chart", metricChart(map[string]any{"label": "Load", "source": "grafana", "query": "max by (instance) (node_load1)", "range": "1d"})),
		"the query returned 2 series; aggregate them into one")
	// The data source's own message can quote labels, so the chart gives a fixed one.
	rejected := f.call(set, "chart", metricChart(map[string]any{"label": "Load", "source": "grafana", "query": "bad(node_load1", "range": "1d"}))
	wantError(t, rejected, "Series 1 (Load): the data source rejected the query; test it with grafana_metrics")
	if strings.Contains(rejected.Text, "parse error") {
		t.Fatalf("the data source's message reached the result: %s", rejected.Text)
	}
	wantError(t, f.call(set, "chart", metricChart(map[string]any{"label": "Load", "source": "grafana", "query": "node_load1", "range": "1d", "sensor": "Hall"})),
		"sensor does not belong to the grafana source")
}

func TestChartKeepsGrafanaToItsPrincipalsAndTheirSwitch(t *testing.T) {
	f, box := newChartFixture(t)
	arguments := metricChart(map[string]any{"label": "Load", "source": "grafana", "query": "node_load1", "range": "1d"})

	bob := f.chartSet("bob")
	for _, definition := range bob.Definitions {
		if definition.Name == "chart" && strings.Contains(string(definition.InputSchema)+bob.Guidance, "grafana") {
			t.Fatalf("bob is offered Grafana: %s\n%s", definition.InputSchema, bob.Guidance)
		}
	}
	wantError(t, f.call(bob, "chart", arguments), "Series 1: the grafana source is not available")
	if f.sim.count() != 0 {
		t.Fatal("a principal outside the allowlist reached Grafana")
	}
	if _, found, _ := box.Card("bob", outbox.ChatTurn(chartTurnID)); found {
		t.Fatal("bob's turn has a chart")
	}

	set := f.chartSet("alice")
	if err := f.switches.SetEnabled(t.Context(), "alice", grafana.Switch, false); err != nil {
		t.Fatal(err)
	}
	wantError(t, f.call(set, "chart", arguments), "Series 1: Grafana is off for this user")
	if f.sim.count() != 0 {
		t.Fatal("a chart read Grafana with the switch off")
	}
}

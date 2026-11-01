package chart_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chart"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const turnID = "7c6b5a4d-3e2f-4c1b-8a9d-0e1f2a3b4c5d"

type allOn struct{}

func (allOn) Enabled(context.Context, string, integration.Switch) (bool, error) { return true, nil }

type renders struct {
	mu       sync.Mutex
	outcomes []string
}

func (r *renders) RecordChartRender(_ context.Context, outcome string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
}

type toolFixture struct {
	t       *testing.T
	box     *outbox.Box
	broker  *tools.Broker
	renders *renders
}

func newToolFixture(t *testing.T) *toolFixture {
	t.Helper()
	box, err := outbox.New(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	charts, err := chart.NewService(chart.Config{Images: box, Switches: allOn{}, Location: amsterdam}, logger)
	if err != nil {
		t.Fatal(err)
	}
	recorded := &renders{}
	charts.SetTelemetry(recorded)
	broker, err := tools.NewBroker(logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(charts); err != nil {
		t.Fatal(err)
	}
	return &toolFixture{t: t, box: box, broker: broker, renders: recorded}
}

func (f *toolFixture) set(ctx context.Context) agentruntime.ToolSet {
	f.t.Helper()
	set, err := f.broker.ToolsFor(ctx, "alice")
	if err != nil {
		f.t.Fatal(err)
	}
	return set
}

func (f *toolFixture) inRun() agentruntime.ToolSet {
	return f.set(tools.WithRun(tools.WithTurn(f.t.Context(), outbox.ChatTurn(turnID)), tools.NewRun("")))
}

func (f *toolFixture) call(set agentruntime.ToolSet, arguments any) agentruntime.ToolResult {
	f.t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		f.t.Fatal(err)
	}
	result, err := set.Executor.Execute(f.t.Context(), agentruntime.ToolCall{Name: "chart", Arguments: raw})
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *toolFixture) staged() ([]byte, bool) {
	f.t.Helper()
	card, found, err := f.box.Card("alice", outbox.ChatTurn(turnID))
	if err != nil {
		f.t.Fatal(err)
	}
	return card.PNG, found
}

func valuesChart(series ...map[string]any) map[string]any {
	return map[string]any{"title": "Prices", "series": series}
}

func prices(label string, values ...float64) map[string]any {
	var points []map[string]any
	for index, value := range values {
		points = append(points, map[string]any{"time": "2026-10-0" + string(rune('1'+index)), "value": value})
	}
	return map[string]any{"label": label, "unit": "EUR", "source": "values", "points": points}
}

func wantResult(t *testing.T, result agentruntime.ToolResult, isError bool, fragments ...string) {
	t.Helper()
	if result.IsError != isError {
		t.Fatalf("result = %+v, want error %v", result, isError)
	}
	for _, fragment := range fragments {
		if !strings.Contains(result.Text, fragment) {
			t.Fatalf("result lacks %q:\n%s", fragment, result.Text)
		}
	}
}

func TestChartOfValuesStatesItsNumbersAndStagesOnce(t *testing.T) {
	f := newToolFixture(t)
	set := f.inRun()
	if len(set.Definitions) != 1 || strings.Contains(string(set.Definitions[0].InputSchema), `"grafana"`) || strings.Contains(set.Guidance, "ha_history") {
		t.Fatalf("a chart without sources offers them: %s\n%s", set.Definitions[0].InputSchema, set.Guidance)
	}
	if set.Definitions[0].Name != "chart" {
		t.Fatalf("tool = %s", set.Definitions[0].Name)
	}

	wantResult(t, f.call(set, valuesChart(prices("Shop A", 10, 12, 11, 30, 12), prices("Shop B", 9, 9.5, 10, 10, 10.5))), false,
		`1. "Shop A" from the values given: 5 points.`,
		"First 10 EUR on Thu 1 Oct 2026, last 12 EUR on Mon 5 Oct 2026, change +2; min 10 EUR on Thu 1 Oct 2026, max 30 EUR on Sun 4 Oct 2026; mean 15 EUR; total 75 EUR.",
		"Outliers, more than 3 median absolute deviations (1) from the median 12 EUR: 30 EUR on Sun 4 Oct 2026.",
		`2. "Shop B" from the values given: 5 points.`,
		"Europe/Amsterdam",
	)
	first, found := f.staged()
	if !found {
		t.Fatal("no chart was staged")
	}
	if !slices.Equal(f.renders.outcomes, []string{"success"}) {
		t.Fatalf("renders recorded = %v", f.renders.outcomes)
	}

	wantResult(t, f.call(set, valuesChart(prices("Shop A", 1, 2))), true, "This reply already has its one image, so this one was not added")
	if again, _ := f.staged(); !bytes.Equal(again, first) {
		t.Fatal("a second chart in the run replaced the first")
	}

	wantResult(t, f.call(f.inRun(), valuesChart(prices("Shop A", 1, 2))), false)
	if again, _ := f.staged(); bytes.Equal(again, first) {
		t.Fatal("a new run of the turn could not stage its own chart")
	}
}

func TestChartRefusals(t *testing.T) {
	f := newToolFixture(t)
	for name, test := range map[string]struct {
		arguments map[string]any
		want      string
	}{
		"no series":    {valuesChart(), "1 to 4 series"},
		"five series":  {valuesChart(prices("a", 1), prices("b", 1), prices("c", 1), prices("d", 1), prices("e", 1)), "1 to 4 series"},
		"no label":     {valuesChart(map[string]any{"source": "values", "points": []map[string]any{{"time": "2026-10-01", "value": 1}}}), "Series 1: give the series a label"},
		"bad kind":     {map[string]any{"title": "x", "type": "pie", "series": []any{prices("a", 1)}}, `type must be "line", "step", or "bar"`},
		"bad time":     {valuesChart(map[string]any{"label": "a", "source": "values", "points": []map[string]any{{"time": "yesterday", "value": 1}}}), "point 1: time must be"},
		"no value":     {valuesChart(map[string]any{"label": "a", "source": "values", "points": []map[string]any{{"time": "2026-10-01"}}}), "point 1 has no value"},
		"bad shift":    {valuesChart(map[string]any{"label": "a", "source": "values", "shift": "2d", "points": []map[string]any{{"time": "2026-10-01", "value": 1}}}), `shift must be "1w", "1M", or "1y"`},
		"no home":      {valuesChart(map[string]any{"label": "a", "source": "ha", "sensor": "x", "period": "today"}), "Series 1: the ha source is not available"},
		"no source":    {valuesChart(map[string]any{"label": "a", "source": "csv"}), "Series 1: source must be one of values"},
		"wrong field":  {valuesChart(map[string]any{"label": "a", "source": "values", "query": "up", "points": []map[string]any{{"time": "2026-10-01", "value": 1}}}), "query does not belong to the values source"},
		"three units":  {valuesChart(withUnit(prices("a", 1), "EUR"), withUnit(prices("b", 1), "USD"), withUnit(prices("c", 1), "GBP")), "at most two units"},
		"untitled":     {map[string]any{"title": " ", "series": []any{prices("a", 1)}}, "A chart needs a title"},
		"long title":   {map[string]any{"title": strings.Repeat("x", 81), "series": []any{prices("a", 1)}}, "A chart needs a title"},
		"many values":  {valuesChart(map[string]any{"label": "a", "source": "values", "points": make([]map[string]any, 201)}), "1 to 200 points"},
		"huge value":   {valuesChart(map[string]any{"label": "a", "source": "values", "points": []map[string]any{{"time": "2026-10-01", "value": 1e308}, {"time": "2026-10-02", "value": -1e308}}}), "point 1: a value must be 0 or between 1e-09 and 1e+15 in size"},
		"tiny value":   {valuesChart(map[string]any{"label": "a", "source": "values", "points": []map[string]any{{"time": "2026-10-01", "value": 5e-324}}}), "point 1: a value must be 0 or between"},
		"extra fields": {map[string]any{"title": "x", "series": []any{prices("a", 1)}, "colour": "red"}, "do not match the tool's schema"},
	} {
		t.Run(name, func(t *testing.T) {
			wantResult(t, f.call(f.inRun(), test.arguments), true, test.want)
		})
	}
	if _, found := f.staged(); found {
		t.Fatal("a refused chart was staged")
	}

	// Watch checks and other calls outside a turn's run have nowhere to send a picture.
	wantResult(t, f.call(f.set(t.Context()), valuesChart(prices("a", 1))), true, "A chart can only be attached during a conversation turn.")
	wantResult(t, f.call(f.set(tools.WithTurn(t.Context(), outbox.ChatTurn(turnID))), valuesChart(prices("a", 1))), true, "A chart can only be attached during a conversation turn.")
}

func withUnit(series map[string]any, unit string) map[string]any {
	series["unit"] = unit
	return series
}

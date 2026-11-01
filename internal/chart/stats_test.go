package chart_test

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chart"
)

func TestSummarizeAgainstAHandComputedSeries(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	hour := func(n int) time.Time { return at.Add(time.Duration(n) * time.Hour) }
	// Hourly values with a three-hour gap before the last one and one far-off value: the median
	// is 4, the deviations from it 3, 2, 1, 0, 96, 1, 2, so their median is 2 and only 100 lies
	// more than 3 × 2 from 4.
	points := []chart.Point{{hour(0), 1}, {hour(1), 2}, {hour(2), 3}, {hour(3), 4}, {hour(4), 100}, {hour(5), 5}, {hour(8), 6}}
	summary, ok := chart.Summarize(points)
	if !ok {
		t.Fatal("no summary")
	}
	want := chart.Summary{
		Count: 7, First: points[0], Last: points[6], Min: points[0], Max: points[4],
		Mean: 121.0 / 7, Total: 121, Median: 4, Deviation: 2, Outliers: []chart.Point{points[4]}, Gaps: 1,
	}
	if math.Abs(summary.Mean-want.Mean) > 1e-9 {
		t.Fatalf("mean = %v, want %v", summary.Mean, want.Mean)
	}
	summary.Mean = want.Mean
	if !summaryEqual(summary, want) {
		t.Fatalf("summary = %+v\nwant %+v", summary, want)
	}

	// Most points equal the median, so the median spread is 0; the mean spread, 2 / 4 × 1.2533,
	// still finds the one spike.
	spike, _ := chart.Summarize([]chart.Point{{hour(0), 5}, {hour(1), 5}, {hour(2), 5}, {hour(3), 7}})
	if !spike.MeanDeviation || math.Abs(spike.Deviation-0.5*1.2533) > 1e-9 || !slices.Equal(spike.Outliers, []chart.Point{{hour(3), 7}}) {
		t.Fatalf("a flat series with a spike = %+v", spike)
	}
	flat, _ := chart.Summarize([]chart.Point{{hour(0), 5}, {hour(1), 5}, {hour(2), 5}})
	if flat.Deviation != 0 || len(flat.Outliers) != 0 || flat.Median != 5 {
		t.Fatalf("a series without spread = %+v", flat)
	}
	even, _ := chart.Summarize([]chart.Point{{hour(0), 1}, {hour(1), 4}, {hour(2), 2}, {hour(3), 3}})
	if even.Median != 2.5 || even.Gaps != 0 {
		t.Fatalf("an even series = %+v", even)
	}
	if _, ok := chart.Summarize(nil); ok {
		t.Fatal("an empty series has a summary")
	}
}

func TestShiftKeepsTheMonthEnd(t *testing.T) {
	day := func(year int, month time.Month, day int) time.Time {
		return time.Date(year, month, day, 10, 0, 0, 0, time.UTC)
	}
	for _, test := range []struct {
		shift    chart.Shift
		at, want time.Time
		forward  bool
	}{
		{chart.OneMonth, day(2026, 10, 31), day(2026, 11, 30), true},
		{chart.OneMonth, day(2026, 3, 31), day(2026, 2, 28), false},
		{chart.OneYear, day(2028, 2, 29), day(2029, 2, 28), true},
		{chart.OneWeek, day(2026, 10, 29), day(2026, 11, 5), true},
	} {
		got := test.shift.Back(test.at)
		if test.forward {
			got = test.shift.Forward(test.at)
		}
		if !got.Equal(test.want) {
			t.Errorf("%s from %s = %s, want %s", test.shift, test.at, got, test.want)
		}
	}
}

func summaryEqual(a, b chart.Summary) bool {
	return a.Count == b.Count && a.First == b.First && a.Last == b.Last && a.Min == b.Min && a.Max == b.Max &&
		a.Mean == b.Mean && a.Total == b.Total && a.Median == b.Median && a.Deviation == b.Deviation &&
		slices.Equal(a.Outliers, b.Outliers) && a.Gaps == b.Gaps
}

func TestDownsampleKeepsTheExtremesAndTheGaps(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var points []chart.Point
	for minute := range 10000 {
		if minute >= 4000 && minute < 5000 {
			continue
		}
		points = append(points, chart.Point{At: at.Add(time.Duration(minute) * time.Minute), Value: math.Sin(float64(minute) / 300)})
	}
	points[1234].Value, points[7777].Value = -5, 9
	low, high := points[1234], points[7777]

	reduced := chart.Downsample(points, 500)
	if len(reduced) > 500 || len(reduced) < 400 {
		t.Fatalf("%d points remain", len(reduced))
	}
	if !slices.Contains(reduced, low) || !slices.Contains(reduced, high) {
		t.Fatal("the extremes were averaged away")
	}
	for index, point := range reduced {
		if index > 0 && !point.At.After(reduced[index-1].At) {
			t.Fatalf("points out of order at %d", index)
		}
		if minute := point.At.Sub(at).Minutes(); minute > 4001 && minute < 4999 {
			t.Fatalf("a point at minute %.0f fills the gap", minute)
		}
	}
	if len(chart.Downsample(points[:300], 500)) != 300 {
		t.Fatal("a short series was reduced")
	}
}

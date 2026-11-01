package chart

import (
	"errors"
	"testing"
	"time"
)

// TestValueAxisFailsRatherThanLoops builds axes straight from values Chart.check would refuse,
// which once made the tick loop run without end.
func TestValueAxisFailsRatherThanLoops(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		kind   Kind
		values []float64
	}{
		"both extremes": {Line, []float64{1e308, -1e308}},
		"huge bar":      {Bar, []float64{1.7e308}},
		"subnormal":     {Line, []float64{5e-324, 1e-323}},
	} {
		t.Run(name, func(t *testing.T) {
			series := Series{Label: name}
			for index, value := range test.values {
				series.Points = append(series.Points, Point{At: at.Add(time.Duration(index) * time.Hour), Value: value})
			}
			if axis, err := newValueAxis(Chart{Kind: test.kind, Series: []Series{series}}, ""); !errors.Is(err, ErrValueRange) {
				t.Fatalf("axis = %+v, error = %v", axis, err)
			}
		})
	}
}

func TestBarsStayAtLeastTwoPixelsWide(t *testing.T) {
	area := plot{slot: 1}
	for index := range MaximumSeries {
		offset, width := area.barOffset(index, MaximumSeries)
		if width < minimumBarWidth || offset != -2*minimumBarWidth+float64(index)*minimumBarWidth {
			t.Fatalf("bar %d at %v, %v wide", index, offset, width)
		}
	}
}

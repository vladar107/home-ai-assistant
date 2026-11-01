package chart_test

import (
	"bytes"
	"errors"
	"flag"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chart"
)

var update = flag.Bool("update", false, "rewrite the golden images in testdata")

// Rendering differs slightly between architectures where the compiler fuses multiply-adds, so a
// golden image matches when few pixels differ, and those only a little.
const (
	pixelTolerance  = 48
	changedPixelMax = 0.001
)

var amsterdam = func() *time.Location {
	location, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		panic(err)
	}
	return location
}()

func hourly(from time.Time, hours int, value func(int) float64) []chart.Point {
	points := make([]chart.Point, 0, hours)
	for hour := range hours {
		points = append(points, chart.Point{At: from.Add(time.Duration(hour) * time.Hour), Value: value(hour)})
	}
	return points
}

func daily(from time.Time, days int, value func(int) float64) []chart.Point {
	points := make([]chart.Point, 0, days)
	for day := range days {
		points = append(points, chart.Point{At: from.AddDate(0, 0, day), Value: value(day)})
	}
	return points
}

func goldenCharts() map[string]chart.Chart {
	week := time.Date(2026, 10, 26, 0, 0, 0, 0, amsterdam)
	temperature := func(hour int) float64 { return 20 + 2*math.Sin(float64(hour)/24*2*math.Pi) + float64(hour%7)/10 }
	withGap := hourly(week, 168, temperature)
	withGap = append(withGap[:60], withGap[80:]...)
	october := time.Date(2026, 10, 1, 0, 0, 0, 0, amsterdam)
	production := func(day int) float64 { return 8 + float64((day*7)%11) }
	return map[string]chart.Chart{
		"line": {
			Title:    "Living room temperature this week",
			Kind:     chart.Line,
			Location: amsterdam,
			Series:   []chart.Series{{Label: "Living room", Unit: "°C", Points: withGap}},
		},
		"step": {
			Title:    "Heating setpoint",
			Kind:     chart.Step,
			Location: amsterdam,
			Series: []chart.Series{{Label: "Setpoint", Unit: "°C", Points: []chart.Point{
				{At: week, Value: 17}, {At: week.Add(6 * time.Hour), Value: 20.5}, {At: week.Add(9 * time.Hour), Value: 18},
				{At: week.Add(17 * time.Hour), Value: 21}, {At: week.Add(23 * time.Hour), Value: 16}, {At: week.Add(24 * time.Hour), Value: 16},
			}}},
		},
		"bar": {
			Title:    "Energy by day",
			Kind:     chart.Bar,
			Location: amsterdam,
			Series: []chart.Series{
				{Label: "Produced", Unit: "kWh", Points: daily(october, 14, production)},
				{Label: "Used", Unit: "kWh", Points: daily(october, 14, func(day int) float64 { return 9 + float64(day%4) })},
			},
		},
		"two-axes": {
			Title:    "Temperature and humidity",
			Kind:     chart.Line,
			Location: amsterdam,
			Series: []chart.Series{
				{Label: "Temperature", Unit: "°C", Points: hourly(week, 72, temperature)},
				{Label: "Humidity", Unit: "%", Points: hourly(week, 72, func(hour int) float64 { return 55 + 10*math.Cos(float64(hour)/24*2*math.Pi) })},
			},
		},
		"shifted": {
			Title:    "Solar production: October against September",
			Kind:     chart.Line,
			Location: amsterdam,
			Series: []chart.Series{
				{Label: "October", Unit: "kWh", Points: daily(october, 31, production)},
				{Label: "September", Unit: "kWh", Dashed: true, Points: daily(october, 30, func(day int) float64 { return 11 + float64((day*5)%9) })},
			},
		},
	}
}

func TestRenderMatchesGoldenImages(t *testing.T) {
	for name, c := range goldenCharts() {
		t.Run(name, func(t *testing.T) {
			rendered, err := chart.Render(c)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", name+".png")
			if *update {
				if err := os.WriteFile(path, rendered, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			golden, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v; run go test ./internal/chart -update to create it", err)
			}
			compareImages(t, decode(t, rendered), decode(t, golden))
		})
	}
}

func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func compareImages(t *testing.T, got, want image.Image) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("size %v, want %v", got.Bounds(), want.Bounds())
	}
	if got.Bounds().Dx() != chart.Width || got.Bounds().Dy() != chart.Height {
		t.Fatalf("size %v, want %dx%d", got.Bounds(), chart.Width, chart.Height)
	}
	changed := 0
	bounds := got.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			a := color.NRGBAModel.Convert(got.At(x, y)).(color.NRGBA)
			b := color.NRGBAModel.Convert(want.At(x, y)).(color.NRGBA)
			if channelDistance(a, b) > pixelTolerance {
				changed++
			}
		}
	}
	if share := float64(changed) / float64(bounds.Dx()*bounds.Dy()); share > changedPixelMax {
		t.Fatalf("%d pixels (%.2f%%) differ from the golden image; review the change and rerun with -update", changed, 100*share)
	}
}

func channelDistance(a, b color.NRGBA) int {
	distance := 0
	for _, pair := range [][2]uint8{{a.R, b.R}, {a.G, b.G}, {a.B, b.B}, {a.A, b.A}} {
		distance = max(distance, int(max(pair[0], pair[1])-min(pair[0], pair[1])))
	}
	return distance
}

func TestRenderRefusesWhatItCannotDraw(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	one := []chart.Point{{At: at, Value: 1}}
	series := func(unit string) chart.Series { return chart.Series{Label: unit, Unit: unit, Points: one} }
	for name, test := range map[string]struct {
		chart chart.Chart
		want  error
	}{
		"no series":     {chart.Chart{Kind: chart.Line}, chart.ErrSeriesCount},
		"five series":   {chart.Chart{Kind: chart.Line, Series: []chart.Series{series("a"), series("a"), series("a"), series("a"), series("a")}}, chart.ErrSeriesCount},
		"three units":   {chart.Chart{Kind: chart.Line, Series: []chart.Series{series("a"), series("b"), series("c")}}, chart.ErrUnitCount},
		"not a number":  {chart.Chart{Kind: chart.Bar, Series: []chart.Series{{Label: "x", Points: []chart.Point{{At: at, Value: math.NaN()}}}}}, chart.ErrValueRange},
		"both extremes": {chart.Chart{Kind: chart.Line, Series: []chart.Series{{Label: "x", Points: []chart.Point{{At: at, Value: 1e308}, {At: at.Add(time.Hour), Value: -1e308}}}}}, chart.ErrValueRange},
		"huge bar":      {chart.Chart{Kind: chart.Bar, Series: []chart.Series{{Label: "x", Points: []chart.Point{{At: at, Value: 1.7e308}}}}}, chart.ErrValueRange},
		"subnormal":     {chart.Chart{Kind: chart.Line, Series: []chart.Series{{Label: "x", Points: []chart.Point{{At: at, Value: 5e-324}, {At: at.Add(time.Hour), Value: 1e-323}}}}}, chart.ErrValueRange},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := chart.Render(test.chart)
			if err == nil || (test.want != nil && !errors.Is(err, test.want)) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

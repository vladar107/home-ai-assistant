// Package chart draws time series as a PNG for a reply, computes the statistics the reply states
// about them, and offers the chart tool that fetches the series itself, so neither hundreds of
// points nor the arithmetic on them pass through the model.
package chart

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

const (
	Width  = 1200
	Height = 700

	MaximumSeries = 4
	// MaximumUnits is how many value axes a chart has: one on each side.
	MaximumUnits = 2
)

// Kind is how a chart draws its series.
type Kind string

const (
	Line Kind = "line"
	Step Kind = "step"
	Bar  Kind = "bar"
)

// Series is one drawn series. Its points are in the time they are drawn at, which for a shifted
// comparison is the time they fell plus the shift.
type Series struct {
	Label string
	// Unit decides the value axis; series with the same unit share one.
	Unit   string
	Points []Point
	// Dashed draws a comparison, such as the same series a period earlier, as a dashed line or a
	// lighter bar.
	Dashed bool
}

// Chart is the content of one chart image. The renderer adds no words of its own: the title and
// labels come from the caller, and the axes show numbers and dates.
type Chart struct {
	Title  string
	Kind   Kind
	Series []Series
	// Location is the zone the time axis is shown in; nil means UTC.
	Location *time.Location
}

var (
	ErrSeriesCount = errors.New("a chart needs 1 to " + strconv.Itoa(MaximumSeries) + " series")
	ErrUnitCount   = errors.New("a chart has at most " + strconv.Itoa(MaximumUnits) + " units")
	ErrValueRange  = errors.New("a value is not a number, or is too large or too small to chart")
)

// The magnitudes a value may have besides zero. Beyond them the axis arithmetic loses its
// precision: ticks no longer step apart, or the span between them overflows.
const (
	MaximumMagnitude = 1e15
	MinimumMagnitude = 1e-9
)

// Chartable reports whether a value can be drawn: zero, or a finite number whose magnitude lies
// between MinimumMagnitude and MaximumMagnitude.
func Chartable(value float64) bool {
	magnitude := math.Abs(value)
	return value == 0 || (magnitude >= MinimumMagnitude && magnitude <= MaximumMagnitude)
}

func (c Chart) check() error {
	switch c.Kind {
	case Line, Step, Bar:
	default:
		return fmt.Errorf("unknown chart kind %q", c.Kind)
	}
	if len(c.Series) == 0 || len(c.Series) > MaximumSeries {
		return ErrSeriesCount
	}
	if len(c.units()) > MaximumUnits {
		return ErrUnitCount
	}
	for index, series := range c.Series {
		if len(series.Points) == 0 {
			return fmt.Errorf("series %d has no points", index+1)
		}
		for _, point := range series.Points {
			if !Chartable(point.Value) {
				return fmt.Errorf("series %d: %w", index+1, ErrValueRange)
			}
		}
	}
	return nil
}

// units lists the distinct units in the order the series bring them; the first is the left axis.
func (c Chart) units() []string {
	var units []string
	for _, series := range c.Series {
		known := false
		for _, unit := range units {
			known = known || unit == series.Unit
		}
		if !known {
			units = append(units, series.Unit)
		}
	}
	return units
}

// Shift moves a series by a calendar period, to compare it with the same series earlier.
type Shift string

const (
	NoShift  Shift = ""
	OneWeek  Shift = "1w"
	OneMonth Shift = "1M"
	OneYear  Shift = "1y"
)

// Shifts are the shifts a series may take.
var Shifts = []Shift{OneWeek, OneMonth, OneYear}

// Back is the time one shift earlier.
func (s Shift) Back(at time.Time) time.Time {
	years, months, days := s.period()
	return addClamped(at, -years, -months, -days)
}

// Forward is the time one shift later.
func (s Shift) Forward(at time.Time) time.Time {
	years, months, days := s.period()
	return addClamped(at, years, months, days)
}

// addClamped is AddDate that keeps a day past the end of a shorter month on its last day, so the
// 31st a month later is the 30th of the next month rather than the 1st of the one after.
func addClamped(at time.Time, years, months, days int) time.Time {
	if months == 0 && years == 0 {
		return at.AddDate(0, 0, days)
	}
	first := time.Date(at.Year()+years, at.Month()+time.Month(months), 1, at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), at.Location())
	last := first.AddDate(0, 1, -1).Day()
	return time.Date(first.Year(), first.Month(), min(at.Day(), last), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), at.Location()).AddDate(0, 0, days)
}

func (s Shift) period() (years, months, days int) {
	switch s {
	case OneWeek:
		return 0, 0, 7
	case OneMonth:
		return 0, 1, 0
	case OneYear:
		return 1, 0, 0
	}
	return 0, 0, 0
}

func (s Shift) words() string {
	switch s {
	case OneWeek:
		return "1 week"
	case OneMonth:
		return "1 month"
	case OneYear:
		return "1 year"
	}
	return ""
}

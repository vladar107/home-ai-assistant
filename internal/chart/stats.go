package chart

import (
	"math"
	"slices"
	"time"
)

// outlierDeviations is how many deviations from the median make a point an outlier.
const outlierDeviations = 3

// meanDeviationScale brings the mean absolute deviation to the median absolute deviation's scale
// for normally distributed values, so the fallback flags points about as far out.
const meanDeviationScale = 1.2533

// Point is one value of a series at one time.
type Point struct {
	At    time.Time
	Value float64
}

// Summary is what a reply says about one series, computed from every point fetched rather than
// from what the chart draws.
type Summary struct {
	Count       int
	First, Last Point
	Min, Max    Point
	Mean, Total float64
	// Median and Deviation decide the outliers. Deviation is the median absolute deviation, or,
	// when MeanDeviation is set because more than half the points equal the median, the mean
	// absolute deviation scaled to match it.
	Median, Deviation float64
	MeanDeviation     bool
	Outliers          []Point
	// Gaps counts the spaces between points that are more than twice the usual spacing.
	Gaps int
}

// Summarize describes points ordered by time; it reports false when there are none.
func Summarize(points []Point) (Summary, bool) {
	if len(points) == 0 {
		return Summary{}, false
	}
	summary := Summary{Count: len(points), First: points[0], Last: points[len(points)-1], Min: points[0], Max: points[0]}
	values := make([]float64, 0, len(points))
	for _, point := range points {
		if point.Value < summary.Min.Value {
			summary.Min = point
		}
		if point.Value > summary.Max.Value {
			summary.Max = point
		}
		summary.Total += point.Value
		values = append(values, point.Value)
	}
	summary.Mean = summary.Total / float64(len(points))
	summary.Median = median(values)
	deviations := make([]float64, len(values))
	for index, value := range values {
		deviations[index] = math.Abs(value - summary.Median)
	}
	summary.Deviation = median(deviations)
	// A mostly flat series has no median spread, which would hide a single spike in it; the mean
	// spread still sees it. With no spread at all nothing stands out.
	if summary.Deviation == 0 {
		total := 0.0
		for _, deviation := range deviations {
			total += deviation
		}
		summary.Deviation = total / float64(len(deviations)) * meanDeviationScale
		summary.MeanDeviation = summary.Deviation > 0
	}
	if summary.Deviation > 0 {
		for _, point := range points {
			if math.Abs(point.Value-summary.Median) > outlierDeviations*summary.Deviation {
				summary.Outliers = append(summary.Outliers, point)
			}
		}
	}
	for _, gap := range gapsAfter(points) {
		if gap {
			summary.Gaps++
		}
	}
	return summary, true
}

func median(values []float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// gapsAfter marks each point followed by a space more than twice the median spacing, where a line
// breaks rather than bridging missing data. Fewer than three points have no usual spacing.
func gapsAfter(points []Point) []bool {
	gaps := make([]bool, len(points))
	if len(points) < 3 {
		return gaps
	}
	spacings := make([]float64, 0, len(points)-1)
	for index := 1; index < len(points); index++ {
		spacings = append(spacings, float64(points[index].At.Sub(points[index-1].At)))
	}
	usual := median(spacings)
	if usual <= 0 {
		return gaps
	}
	for index, spacing := range spacings {
		gaps[index] = spacing > 2*usual
	}
	return gaps
}

// Downsample reduces points ordered by time to at most limit, by the mean of equal stretches of
// time, so a stretch without data stays a gap. The lowest and highest points are kept as they are,
// so the line still reaches the extremes the summary reports.
func Downsample(points []Point, limit int) []Point {
	if len(points) <= limit || limit < 3 {
		return points
	}
	buckets := limit - 1
	start := points[0].At
	span := points[len(points)-1].At.Sub(start)
	if span <= 0 {
		return points[:limit]
	}
	type bucket struct {
		offset, value float64
		count         int
		points        []Point
	}
	filled := make([]bucket, buckets)
	low, high := 0, 0
	indexOf := func(at time.Time) int {
		return min(buckets-1, int(float64(at.Sub(start))/float64(span)*float64(buckets)))
	}
	for index, point := range points {
		b := &filled[indexOf(point.At)]
		b.offset += float64(point.At.Sub(start))
		b.value += point.Value
		b.count++
		if point.Value < points[low].Value {
			low = index
		}
		if point.Value > points[high].Value {
			high = index
		}
	}
	for _, extreme := range []int{low, high} {
		b := &filled[indexOf(points[extreme].At)]
		if !slices.Contains(b.points, points[extreme]) {
			b.points = append(b.points, points[extreme])
		}
	}
	reduced := make([]Point, 0, limit)
	for _, b := range filled {
		switch {
		case b.count == 0:
		case len(b.points) > 0:
			slices.SortFunc(b.points, func(a, b Point) int { return a.At.Compare(b.At) })
			reduced = append(reduced, b.points...)
		default:
			reduced = append(reduced, Point{
				At:    start.Add(time.Duration(b.offset / float64(b.count))),
				Value: b.value / float64(b.count),
			})
		}
	}
	return reduced
}

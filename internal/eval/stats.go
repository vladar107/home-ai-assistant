package eval

import "math"

const (
	// zInterval is the normal quantile of a two-sided 95% interval.
	zInterval = 1.96
	// zPower is the normal quantile of 80% power; with zInterval it gives the smallest true
	// difference a paired comparison detects four times in five.
	zPower = 0.8416
)

// Paired is the paired comparison of two runs over the same cases: each case contributes the
// difference of its two pass rates, and the cases, not the trials, are the sample.
type Paired struct {
	N int
	// Mean is the mean per-case difference, second run minus first.
	Mean float64
	// SE is the standard error of Mean, from the sample standard deviation of the differences.
	SE float64
	// Low and High bound the 95% interval, Mean ± 1.96·SE.
	Low, High float64
	// MDD is the minimum detectable difference at 5% significance and 80% power, (1.96+0.84)·SE.
	MDD float64
	// Better, Worse and Same count the cases the second run did better, worse or equally on.
	Better, Worse, Same int
}

// Detected reports whether the interval excludes zero.
func (p Paired) Detected() bool { return p.N >= 2 && (p.Low > 0 || p.High < 0) }

const rateTolerance = 1e-9

// PairedDifferences compares first and second, two equal-length lists of per-case pass rates.
// With fewer than two cases there is no standard error and the interval is left at the mean.
func PairedDifferences(first, second []float64) Paired {
	n := min(len(first), len(second))
	p := Paired{N: n}
	if n == 0 {
		return p
	}
	diffs := make([]float64, n)
	var sum float64
	for i := range diffs {
		diffs[i] = second[i] - first[i]
		sum += diffs[i]
		switch {
		case diffs[i] > rateTolerance:
			p.Better++
		case diffs[i] < -rateTolerance:
			p.Worse++
		default:
			p.Same++
		}
	}
	p.Mean = sum / float64(n)
	p.Low, p.High = p.Mean, p.Mean
	if n < 2 {
		return p
	}
	var squares float64
	for _, d := range diffs {
		squares += (d - p.Mean) * (d - p.Mean)
	}
	p.SE = math.Sqrt(squares/float64(n-1)) / math.Sqrt(float64(n))
	p.Low, p.High = p.Mean-zInterval*p.SE, p.Mean+zInterval*p.SE
	p.MDD = (zInterval + zPower) * p.SE
	return p
}

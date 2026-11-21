package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Comparison is a paired comparison of two runs over the cases both played.
type Comparison struct {
	First, Second *RunDetail
	// Shared lists the cases both runs played, sorted; OnlyFirst and OnlySecond are the rest.
	Shared     []string
	OnlyFirst  []string
	OnlySecond []string
	Paired     Paired
	// Rates are each run's pass rates over the shared cases alone.
	FirstRate, SecondRate   Rate
	FirstPassK, SecondPassK float64
	FirstHappy, SecondHappy Rate
	// FirstCost and SecondCost add up the shared cases' trials.
	FirstCost, SecondCost cost
	// ModelChanged is set when the second run is on a different model from the first, as under a
	// variant that names one. NotSensitive then lists the shared cases whose own model changed too,
	// with at least minSensitivityTrials trials in each run, that passed every first-run trial and
	// whose pass rate did not drop: cases that may not be testing the model.
	ModelChanged bool
	NotSensitive []string
}

// minSensitivityTrials is the fewest trials a case needs in each run to count as not sensitive.
const minSensitivityTrials = 3

type cost struct {
	Trials               int
	Calls, Input, Output int64
}

func (c *cost) add(r CaseRate) {
	c.Trials += r.Ran
	c.Calls += r.ModelCalls
	c.Input += r.Input
	c.Output += r.Output
}

// Compare pairs two stored runs by case. Only the cases both ran count; the rest are listed.
func Compare(ctx context.Context, store *ResultsStore, first, second string) (*Comparison, error) {
	a, err := store.Detail(ctx, first)
	if err != nil {
		return nil, err
	}
	b, err := store.Detail(ctx, second)
	if err != nil {
		return nil, err
	}
	c := &Comparison{First: a, Second: b}
	for id := range a.Cases {
		if _, ok := b.Cases[id]; ok {
			c.Shared = append(c.Shared, id)
		} else {
			c.OnlyFirst = append(c.OnlyFirst, id)
		}
	}
	for id := range b.Cases {
		if _, ok := a.Cases[id]; !ok {
			c.OnlySecond = append(c.OnlySecond, id)
		}
	}
	sort.Strings(c.Shared)
	sort.Strings(c.OnlyFirst)
	sort.Strings(c.OnlySecond)
	if len(c.Shared) == 0 {
		return nil, errors.New("the runs share no case, so there is nothing to compare")
	}
	var firstRates, secondRates []float64
	for _, id := range c.Shared {
		x, y := a.Cases[id], b.Cases[id]
		firstRates = append(firstRates, x.Rate())
		secondRates = append(secondRates, y.Rate())
		accumulate(&c.FirstRate, &c.FirstHappy, &c.FirstPassK, x)
		accumulate(&c.SecondRate, &c.SecondHappy, &c.SecondPassK, y)
		c.FirstCost.add(x)
		c.SecondCost.add(y)
	}
	c.FirstPassK /= float64(len(c.Shared))
	c.SecondPassK /= float64(len(c.Shared))
	c.Paired = PairedDifferences(firstRates, secondRates)
	c.ModelChanged = a.Run.Model != b.Run.Model
	if c.ModelChanged {
		for _, id := range c.Shared {
			x, y := a.Cases[id], b.Cases[id]
			if x.Model != y.Model && x.Ran >= minSensitivityTrials && y.Ran >= minSensitivityTrials && x.Passed == x.Ran && y.Rate() >= x.Rate()-rateTolerance {
				c.NotSensitive = append(c.NotSensitive, id)
			}
		}
	}
	return c, nil
}

// accumulate adds one case to a run's pooled rate, happy-path rate and count of cases whose every
// trial passed.
func accumulate(overall, happy *Rate, allPassed *float64, c CaseRate) {
	overall.Ran += c.Ran
	overall.Passed += c.Passed
	if c.Happy {
		happy.Ran += c.Ran
		happy.Passed += c.Passed
	}
	if c.Ran == c.Passed {
		*allPassed++
	}
}

// Verdict is the comparison's conclusion in one sentence.
func (c *Comparison) Verdict() string {
	p := c.Paired
	switch {
	case p.N < 2:
		return "Too few shared cases for an interval; no conclusion."
	case p.Detected() && p.Mean > 0:
		return fmt.Sprintf("The second run is better: %+.1f points per case on average, 95%% interval %+.1f to %+.1f.", 100*p.Mean, 100*p.Low, 100*p.High)
	case p.Detected():
		return fmt.Sprintf("The second run is worse: %+.1f points per case on average, 95%% interval %+.1f to %+.1f.", 100*p.Mean, 100*p.Low, 100*p.High)
	case p.SE == 0:
		return "No difference was detected: every shared case has the same pass rate in both runs."
	default:
		return fmt.Sprintf("No difference was detected: %+.1f points per case on average, 95%% interval %+.1f to %+.1f. "+
			"With %d shared cases this comparison could detect a difference of about %.1f points; a smaller one would not show.",
			100*p.Mean, 100*p.Low, 100*p.High, p.N, 100*p.MDD)
	}
}

func perTrial(total int64, trials int) string {
	if trials == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", float64(total)/float64(trials))
}

func describeRun(run StoredRun) string {
	variant := "none"
	if run.Variant != "" {
		variant = "`" + run.Variant + "`"
	}
	fingerprint := run.Fingerprint
	if len(fingerprint) > 12 {
		fingerprint = fingerprint[:12]
	}
	text := fmt.Sprintf("model %s (%s), Pi %s, variant %s, fingerprint `%s`, commit %s", run.Model, run.Effort, run.PiVersion, variant, fingerprint, run.Commit)
	if run.Replayed {
		text += ", replayed from " + run.ReplayOf
	}
	return text
}

// Markdown renders the comparison.
func (c *Comparison) Markdown() string {
	var b strings.Builder
	a, z := c.First, c.Second
	fmt.Fprintf(&b, "# Eval comparison: %s and %s\n\n", a.Run.RunID, z.Run.RunID)
	fmt.Fprintf(&b, "%s\n\n", c.Verdict())
	fmt.Fprintf(&b, "- First run `%s`: %s\n- Second run `%s`: %s\n", a.Run.RunID, describeRun(a.Run), z.Run.RunID, describeRun(z.Run))
	fmt.Fprintf(&b, "- Compared on %d shared case(s). The comparison is paired by case: each case's difference is its second pass rate minus its first.\n", len(c.Shared))
	if len(c.OnlyFirst)+len(c.OnlySecond) > 0 {
		fmt.Fprintf(&b, "- Not compared: %d case(s) only in the first run (%s) and %d only in the second (%s).\n",
			len(c.OnlyFirst), strings.Join(c.OnlyFirst, ", "), len(c.OnlySecond), strings.Join(c.OnlySecond, ", "))
	}
	if a.Run.Replayed || z.Run.Replayed {
		b.WriteString("- A replayed run re-scores old cassettes with the current checks; it says nothing about a prompt, since no model ran.\n")
	}
	if c.ModelChanged {
		fmt.Fprintf(&b, "- The second run is on a different model, %s instead of %s: see \"Not sensitive to the model\".\n", z.Run.Model, a.Run.Model)
	}

	p := c.Paired
	b.WriteString("\n## Overall, shared cases\n\n| | First | Second |\n| --- | --- | --- |\n")
	fmt.Fprintf(&b, "| Pass rate (trials) | %s | %s |\n", c.FirstRate, c.SecondRate)
	fmt.Fprintf(&b, "| pass^k (every trial of a case passes) | %.1f%% | %.1f%% |\n", 100*c.FirstPassK, 100*c.SecondPassK)
	fmt.Fprintf(&b, "| Happy-path pass rate | %s | %s |\n", c.FirstHappy, c.SecondHappy)
	fmt.Fprintf(&b, "| Model calls per trial | %s | %s |\n", perTrial(c.FirstCost.Calls, c.FirstCost.Trials), perTrial(c.SecondCost.Calls, c.SecondCost.Trials))
	fmt.Fprintf(&b, "| Input tokens per trial | %s | %s |\n", perTrial(c.FirstCost.Input, c.FirstCost.Trials), perTrial(c.SecondCost.Input, c.SecondCost.Trials))
	fmt.Fprintf(&b, "| Output tokens per trial | %s | %s |\n", perTrial(c.FirstCost.Output, c.FirstCost.Trials), perTrial(c.SecondCost.Output, c.SecondCost.Trials))

	b.WriteString("\n## Paired difference\n\n")
	fmt.Fprintf(&b, "- Mean per-case difference: %+.1f points\n", 100*p.Mean)
	if p.N >= 2 {
		fmt.Fprintf(&b, "- Standard error: %.1f points; 95%% interval %+.1f to %+.1f\n", 100*p.SE, 100*p.Low, 100*p.High)
		fmt.Fprintf(&b, "- Minimum detectable difference (5%% significance, 80%% power): %.1f points\n", 100*p.MDD)
	}
	fmt.Fprintf(&b, "- Cases better: %d, worse: %d, unchanged: %d\n", p.Better, p.Worse, p.Same)

	b.WriteString("\n## Per case\n\n| Case | First | Second | Difference |\n| --- | --- | --- | --- |\n")
	ids := slices.Clone(c.Shared)
	slices.SortStableFunc(ids, func(x, y string) int {
		dx := c.Second.Cases[x].Rate() - c.First.Cases[x].Rate()
		dy := c.Second.Cases[y].Rate() - c.First.Cases[y].Rate()
		switch {
		case dx < dy:
			return -1
		case dx > dy:
			return 1
		}
		return strings.Compare(x, y)
	})
	for _, id := range ids {
		x, y := c.First.Cases[id], c.Second.Cases[id]
		fmt.Fprintf(&b, "| %s | %d/%d | %d/%d | %+.0f points |\n", id, x.Passed, x.Ran, y.Passed, y.Ran, 100*(y.Rate()-x.Rate()))
	}
	if c.ModelChanged {
		b.WriteString("\n## Not sensitive to the model\n\n")
		fmt.Fprintf(&b, "Cases whose model changed between the runs, with at least %d trials in each run, that passed every trial of the first run and whose pass rate did not drop. "+
			"A case that pins its own model is not counted. A case listed here may not be testing the model; consider tightening its checks.\n\n", minSensitivityTrials)
		if len(c.NotSensitive) == 0 {
			b.WriteString("None.\n")
		}
		for _, id := range c.NotSensitive {
			fmt.Fprintf(&b, "- %s\n", id)
		}
	}
	return b.String()
}

// WriteComparison writes compare.md under <results>/compare-<first>-<second> and returns its path.
func WriteComparison(resultsDir string, c *Comparison) (string, error) {
	dir := filepath.Join(resultsDir, fmt.Sprintf("compare-%s-%s", c.First.Run.RunID, c.Second.Run.RunID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("write comparison: %w", err)
	}
	path := filepath.Join(dir, "compare.md")
	if err := os.WriteFile(path, []byte(c.Markdown()), 0o600); err != nil {
		return "", fmt.Errorf("write comparison: %w", err)
	}
	return path, nil
}

// Summary is the few lines the compare command prints.
func (c *Comparison) Summary() string {
	p := c.Paired
	summary := fmt.Sprintf("%s\nshared cases %d, better %d, worse %d, unchanged %d; pass rate %s -> %s; pass^k %.1f%% -> %.1f%%",
		c.Verdict(), len(c.Shared), p.Better, p.Worse, p.Same, c.FirstRate, c.SecondRate, 100*c.FirstPassK, 100*c.SecondPassK)
	if c.ModelChanged {
		summary += fmt.Sprintf("\nmodel %s -> %s: %d case(s) not sensitive to the model", c.First.Run.Model, c.Second.Run.Model, len(c.NotSensitive))
	}
	return summary
}

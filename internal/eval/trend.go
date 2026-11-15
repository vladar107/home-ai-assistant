package eval

import (
	"fmt"
	"io"
	"strings"
)

type trendKey struct{ fingerprint, model, effort, piVersion string }

func shortPrint(fingerprint string) string {
	switch {
	case fingerprint == "":
		return "none"
	case len(fingerprint) > 12:
		return fingerprint[:12]
	}
	return fingerprint
}

// WriteTrend prints each run's pass rate, pass^k and happy-path rate, split into groups by prompt
// fingerprint, model, effort and Pi version, so a change in any of them starts a new group. Groups
// follow the order their first run started in and each lists its runs oldest first.
func WriteTrend(w io.Writer, runs []StoredRun) {
	if len(runs) == 0 {
		fmt.Fprintln(w, "no runs in the results store")
		return
	}
	var order []trendKey
	groups := map[trendKey][]StoredRun{}
	for _, run := range runs {
		key := trendKey{run.Fingerprint, run.Model, run.Effort, run.PiVersion}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], run)
	}
	for i, key := range order {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "prompt %s, model %s (%s), Pi %s\n", shortPrint(key.fingerprint), key.model, key.effort, key.piVersion)
		fmt.Fprintf(w, "  %-16s %-20s %-20s %-14s %-12s %s\n", "run", "pass rate", "pass^k", "happy", "variant", "notes")
		for _, run := range groups[key] {
			fmt.Fprintf(w, "  %-16s %-20s %-20s %-14s %-12s %s\n", run.RunID, run.Overall, passK(run), happy(run), run.Variant, notes(run))
		}
	}
}

func passK(run StoredRun) string {
	if run.K == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% (k=%d)", 100*run.PassK, run.K)
}

func happy(run StoredRun) string {
	if run.Happy.Ran == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(run.Happy.Passed)/float64(run.Happy.Ran))
}

func notes(run StoredRun) string {
	var parts []string
	if run.Replayed {
		parts = append(parts, "replayed from "+run.ReplayOf)
	}
	if run.Stopped != "" {
		parts = append(parts, "stopped: "+run.Stopped)
	}
	if run.Reviewed > 0 {
		parts = append(parts, fmt.Sprintf("%d reviewed", run.Reviewed))
	}
	return strings.Join(parts, ", ")
}

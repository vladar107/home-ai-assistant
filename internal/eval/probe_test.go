package eval

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProbesFindLooseChecksWithoutAModel(t *testing.T) {
	cases, err := LoadCases(filepath.Join("testdata", "probes"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunProbes(t.Context(), cases)
	if err != nil {
		t.Fatal(err)
	}
	listed := func(probe, id string) bool { return slices.Contains(report.Passed[probe], id) }

	for _, probe := range []string{ProbeEcho, ProbeAcknowledgement} {
		if !listed(probe, "probe-loose") {
			t.Errorf("the loose case is not listed under %s: %+v", probe, report.Passed)
		}
	}
	if listed(ProbeEmpty, "probe-loose") {
		t.Error("an empty reply passed a check that the reply is not empty")
	}
	for _, probe := range TrivialProbes {
		if listed(probe.Name, "probe-strict") {
			t.Errorf("the strict case is listed under %s", probe.Name)
		}
	}
	if listed(ProbeRefusal, "probe-injection") || listed(ProbeNoTools, "probe-injection") {
		t.Errorf("the injection case is listed under a probe it declares expected: %+v", report.Passed)
	}
	if !listed(ProbeAcknowledgement, "probe-injection") || listed(ProbeEcho, "probe-injection") {
		t.Errorf("the injection case is not scored by its own checks under the probes it does not expect: %+v", report.Passed)
	}
	// The Confirm step has nothing to press and its store check reads the store as seeded.
	if !listed(ProbeEcho, "probe-confirm") || listed(ProbeEmpty, "probe-confirm") {
		t.Errorf("the confirm case was not probed through its Confirm step: %+v", report.Passed)
	}
	if !slices.Equal(report.NotProbeable, []string{"probe-rubric"}) {
		t.Errorf("not probeable = %v, want the rubric-only case", report.NotProbeable)
	}
	if !slices.Equal(report.NotPassed[ProbeAcknowledgement], []string{"probe-stale"}) || len(report.NotPassed) != 1 {
		t.Errorf("expected but not passed = %v, want only the stale case's acknowledgement", report.NotPassed)
	}
	// A stale expected probe is reported but does not count as flagged, which sets the exit status.
	if report.Probed != 5 || report.Flagged() != 3 {
		t.Errorf("probed %d, flagged %d; want 5 and 3", report.Probed, report.Flagged())
	}

	markdown := report.Markdown()
	for _, want := range []string{
		"## echo: the user's message echoed\n\n- probe-confirm\n- probe-loose\n", "## Not probeable", "- probe-rubric",
		"## Expected but not passed", "- probe-stale: acknowledgement\n",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("report lacks %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "probe-strict") {
		t.Errorf("report names the strict case:\n%s", markdown)
	}
}

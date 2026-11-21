package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// Probe names, as a case's expected_probes names them.
const (
	ProbeEmpty           = "empty"
	ProbeRefusal         = "refusal"
	ProbeEcho            = "echo"
	ProbeAcknowledgement = "acknowledgement"
	ProbeNoTools         = "no-tools"
)

// TrivialProbe is a synthetic answer no model gave: every step gets its reply, and no step calls a
// tool or offers a change.
type TrivialProbe struct {
	Name        string
	Description string
	// reply answers a step; said is the last message the user said up to it.
	reply func(said string) string
}

// TrivialProbes are the probes run against every case, in report order.
var TrivialProbes = []TrivialProbe{
	{Name: ProbeEmpty, Description: "an empty reply", reply: func(string) string { return "" }},
	{Name: ProbeRefusal, Description: "a refusal", reply: func(string) string { return "Sorry, I can't help with that." }},
	{Name: ProbeEcho, Description: "the user's message echoed", reply: func(said string) string { return said }},
	{Name: ProbeAcknowledgement, Description: "a generic acknowledgement", reply: func(string) string { return "Okay, got it." }},
	{Name: ProbeNoTools, Description: "a confident answer that claims the work was done without calling a tool",
		reply: func(string) string { return "Done: I checked everything and took care of it for you." }},
}

func knownProbe(name string) bool {
	return slices.ContainsFunc(TrivialProbes, func(p TrivialProbe) bool { return p.Name == name })
}

// ProbeReport is what the probes found: for each probe, the cases it passed although they do not
// declare it expected, and the cases that declare it expected although it failed.
type ProbeReport struct {
	Probed int
	// Passed maps a probe name to the sorted IDs of the cases it unexpectedly passed.
	Passed map[string][]string
	// NotPassed maps a probe name to the sorted IDs of the cases that expect it but failed it: a
	// stale expected_probes entry. It is reported only.
	NotPassed map[string][]string
	// NotProbeable lists the cases with no deterministic check, only a rubric or nothing.
	NotProbeable []string
}

// Flagged counts the cases at least one probe unexpectedly passed.
func (r *ProbeReport) Flagged() int {
	seen := map[string]bool{}
	for _, ids := range r.Passed {
		for _, id := range ids {
			seen[id] = true
		}
	}
	return len(seen)
}

// RunProbes runs every probe against each case's checks, over the world the case seeds and with no
// model: a world whose runtime refuses every run, so nothing reaches Pi or the network.
func RunProbes(ctx context.Context, cases []Case) (*ProbeReport, error) {
	profiles, err := os.MkdirTemp("", "eval-probe-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(profiles)
	report := &ProbeReport{Passed: map[string][]string{}, NotPassed: map[string][]string{}}
	for _, c := range cases {
		if !hasChecks(c) {
			report.NotProbeable = append(report.NotProbeable, c.ID)
			continue
		}
		passed, err := probeCase(ctx, c, profiles)
		if err != nil {
			return nil, fmt.Errorf("probe case %s: %w", c.ID, err)
		}
		report.Probed++
		for name, ok := range passed {
			switch expected := c.expectsProbe(name); {
			case ok && !expected:
				report.Passed[name] = append(report.Passed[name], c.ID)
			case !ok && expected:
				report.NotPassed[name] = append(report.NotPassed[name], c.ID)
			}
		}
	}
	for _, found := range []map[string][]string{report.Passed, report.NotPassed} {
		for _, ids := range found {
			slices.Sort(ids)
		}
	}
	slices.Sort(report.NotProbeable)
	return report, nil
}

// probeCase reports, for each probe it runs, whether the probe passes the case. The probes change
// nothing, so one seeded world serves them all.
func probeCase(ctx context.Context, c Case, profiles string) (_ map[string]bool, err error) {
	world, err := NewWorld(ctx, c, WorldOptions{
		Runtime: noModel{}, ProfilesDir: profiles, AccountID: "probe",
		ChatModel: agentruntime.ModelEffort{Model: "probe", Effort: agentruntime.DefaultEffort},
	})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, world.Close()) }()
	env := CheckEnv{Store: world.Store, Location: world.Location}
	saysAnything := slices.ContainsFunc(c.Steps, func(s Step) bool { return s.Say != "" })
	passed := map[string]bool{}
	for _, probe := range TrivialProbes {
		// With no user message to echo, the echo probe is the empty one.
		if probe.Name == ProbeEcho && !saysAnything {
			continue
		}
		said, ok := "", true
		for _, step := range c.Steps {
			if step.Say != "" {
				said = step.Say
			}
			if len(CheckStep(ctx, step.Expect, probeTurn(step, probe.reply(said)), env)) > 0 {
				ok = false
				break
			}
		}
		passed[probe.Name] = ok
	}
	return passed, nil
}

// probeTurn is what a step would record had the model answered with the reply and done nothing
// else. No probe offers a change, so a Confirm or Reject step has nothing to press and records
// nothing; a scheduled run or a triage with an empty reply is quiet.
func probeTurn(step Step, reply string) TurnRecord {
	switch {
	case step.Confirm:
		return TurnRecord{Kind: "confirm"}
	case step.Reject:
		return TurnRecord{Kind: "reject"}
	case step.ThumbsDown:
		return TurnRecord{Kind: "thumbs_down", Reply: reply}
	case step.RunJob != "":
		return TurnRecord{Kind: "run_job", Reply: reply, Quiet: reply == ""}
	case step.TriageMail:
		return TurnRecord{Kind: "triage_mail", Reply: reply, Quiet: reply == ""}
	}
	return TurnRecord{Kind: "say", Message: step.Say, Reply: reply}
}

func hasChecks(c Case) bool {
	return slices.ContainsFunc(c.Steps, func(s Step) bool { return s.Expect.hasChecks() })
}

func (e *Expect) hasChecks() bool {
	if e == nil {
		return false
	}
	r := e.Reply
	return len(e.Calls) > 0 || len(e.NotCalled) > 0 || e.MaxCalls != nil || e.Proposals != nil || e.NoWrites || len(e.Store) > 0 ||
		r != nil && (len(r.Contains) > 0 || len(r.NotContains) > 0 || len(r.Matches) > 0 || len(r.NotMatches) > 0 || r.Language != "" || r.Quiet)
}

func (c Case) expectsProbe(name string) bool {
	return slices.ContainsFunc(c.ExpectedProbes, func(p ExpectedProbe) bool { return p.Probe == name })
}

// Markdown renders the report.
func (r *ProbeReport) Markdown() string {
	var b strings.Builder
	b.WriteString("# Trivial-answer probes\n\n")
	fmt.Fprintf(&b, "%d case(s) probed with no model, %d passed by at least one unexpected probe, %d not probeable. "+
		"A case a probe passes has a check too loose to trust, unless it declares that probe in expected_probes with a reason.\n",
		r.Probed, r.Flagged(), len(r.NotProbeable))
	for _, probe := range TrivialProbes {
		ids := r.Passed[probe.Name]
		if len(ids) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s: %s\n\n", probe.Name, probe.Description)
		for _, id := range ids {
			fmt.Fprintf(&b, "- %s\n", id)
		}
	}
	var stale []string
	for _, probe := range TrivialProbes {
		for _, id := range r.NotPassed[probe.Name] {
			stale = append(stale, fmt.Sprintf("- %s: %s\n", id, probe.Name))
		}
	}
	if len(stale) > 0 {
		b.WriteString("\n## Expected but not passed\n\nThese cases declare a probe expected that their checks now fail; the expected_probes entry may be stale.\n\n")
		for _, line := range stale {
			b.WriteString(line)
		}
	}
	if len(r.NotProbeable) > 0 {
		b.WriteString("\n## Not probeable\n\nThese cases have no deterministic check, only a rubric or nothing.\n\n")
		for _, id := range r.NotProbeable {
			fmt.Fprintf(&b, "- %s\n", id)
		}
	}
	return b.String()
}

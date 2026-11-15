package eval

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	VerdictGood   = "good"
	VerdictBad    = "bad"
	VerdictUnsure = "unsure"

	reviewFile = "review.json"

	reviewWidth      = 100
	reviewArgsRunes  = 160
	reviewReplyLimit = 4000
)

// ReviewEntry is the operator's own verdict on one trial.
type ReviewEntry struct {
	Case       string    `json:"case"`
	Trial      int       `json:"trial"`
	Verdict    string    `json:"verdict"`
	Note       string    `json:"note,omitempty"`
	ReviewedAt time.Time `json:"reviewed_at"`
}

// Key names the trial the entry belongs to.
func (e ReviewEntry) Key() string { return trialKey(e.Case, e.Trial) }

// ReviewFile is a run's review.json.
type ReviewFile struct {
	RunID   string        `json:"run_id"`
	Reviews []ReviewEntry `json:"reviews"`
}

func trialKey(caseID string, n int) string { return fmt.Sprintf("%s#%d", caseID, n) }

func (f *ReviewFile) find(key string) (int, bool) {
	for i, entry := range f.Reviews {
		if entry.Key() == key {
			return i, true
		}
	}
	return 0, false
}

// LoadResult reads a run's result.json from its directory.
func LoadResult(dir string) (*Result, error) {
	contents, err := os.ReadFile(filepath.Join(dir, resultFile))
	if err != nil {
		return nil, fmt.Errorf("load result: %w", err)
	}
	var result Result
	if err := json.Unmarshal(contents, &result); err != nil {
		return nil, fmt.Errorf("load result: %w", err)
	}
	result.Dir = dir
	return &result, nil
}

// LoadReview reads a run's review.json; a run with none has an empty review and found is false.
func LoadReview(dir string) (review *ReviewFile, found bool, err error) {
	contents, err := os.ReadFile(filepath.Join(dir, reviewFile))
	if errors.Is(err, os.ErrNotExist) {
		return &ReviewFile{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load review: %w", err)
	}
	var file ReviewFile
	if err := json.Unmarshal(contents, &file); err != nil {
		return nil, false, fmt.Errorf("load review: %w", err)
	}
	return &file, true, nil
}

func saveReview(dir string, file *ReviewFile) error {
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("save review: %w", err)
	}
	temp, err := os.CreateTemp(dir, reviewFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("save review: %w", err)
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(append(encoded, '\n')); err != nil {
		temp.Close()
		return fmt.Errorf("save review: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("save review: %w", err)
	}
	if err := os.Rename(temp.Name(), filepath.Join(dir, reviewFile)); err != nil {
		return fmt.Errorf("save review: %w", err)
	}
	return nil
}

// ReviewOptions configures a review session over one run's directory.
type ReviewOptions struct {
	Dir string
	// All shows trials that already have a verdict too.
	All bool
	In  io.Reader
	Out io.Writer
}

// Review steps through the run's trials, records the operator's verdicts after every answer and
// rewrites the report when the session ends. Quitting, or the input ending, leaves the reviewed
// trials stored, so the next session resumes at the first unreviewed one.
func Review(opts ReviewOptions) error {
	result, err := LoadResult(opts.Dir)
	if err != nil {
		return err
	}
	review, _, err := LoadReview(opts.Dir)
	if err != nil {
		return err
	}
	review.RunID = result.RunID
	in := bufio.NewReader(opts.In)
	out := opts.Out

	type pending struct {
		c     CaseResult
		trial TrialResult
	}
	var queue []pending
	for _, c := range sortedCases(result.Cases) {
		for _, trial := range sortedTrials(c.Trials) {
			if !trial.Ran() {
				continue
			}
			if _, done := review.find(trialKey(c.ID, trial.N)); done && !opts.All {
				continue
			}
			queue = append(queue, pending{c, trial})
		}
	}
	if len(queue) == 0 {
		fmt.Fprintln(out, "nothing to review: every trial of the run has a verdict")
		return writeReviewedReport(result)
	}

loop:
	for i, item := range queue {
		fmt.Fprintf(out, "\n%s\n[%d/%d] ", strings.Repeat("=", reviewWidth), i+1, len(queue))
		showTrial(out, result, item.c, item.trial)
		key := trialKey(item.c.ID, item.trial.N)
		if index, done := review.find(key); done {
			fmt.Fprintf(out, "previous verdict: %s", review.Reviews[index].Verdict)
			if note := review.Reviews[index].Note; note != "" {
				fmt.Fprintf(out, " (%s)", note)
			}
			fmt.Fprintln(out)
		}
		for {
			fmt.Fprint(out, "\nverdict [g]ood [b]ad [u]nsure [s]kip [q]uit: ")
			line, err := readLine(in)
			if err != nil {
				fmt.Fprintln(out)
				break loop
			}
			var verdict string
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "g", "good":
				verdict = VerdictGood
			case "b", "bad":
				verdict = VerdictBad
			case "u", "unsure":
				verdict = VerdictUnsure
			case "s", "skip":
				continue loop
			case "q", "quit":
				break loop
			default:
				continue
			}
			fmt.Fprint(out, "note (optional): ")
			note, _ := readLine(in)
			entry := ReviewEntry{Case: item.c.ID, Trial: item.trial.N, Verdict: verdict, Note: strings.TrimSpace(note), ReviewedAt: time.Now().UTC()}
			if index, done := review.find(key); done {
				review.Reviews[index] = entry
			} else {
				review.Reviews = append(review.Reviews, entry)
			}
			if err := saveReview(opts.Dir, review); err != nil {
				return err
			}
			continue loop
		}
	}
	return writeReviewedReport(result)
}

// RewriteReport rebuilds report.md from result.json and, when present, review.json.
func RewriteReport(dir string) error {
	result, err := LoadResult(dir)
	if err != nil {
		return err
	}
	return writeReviewedReport(result)
}

func writeReviewedReport(result *Result) error {
	review, found, err := LoadReview(result.Dir)
	if err != nil {
		return err
	}
	if found {
		result.Review = review
	}
	if err := os.WriteFile(filepath.Join(result.Dir, reportFile), []byte(result.Markdown()), 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

// LatestRun names the newest run directory under the results directory; run IDs are timestamps.
func LatestRun(resultsDir string) (string, error) {
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return "", fmt.Errorf("find the latest run: %w", err)
	}
	var latest string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(resultsDir, entry.Name(), resultFile)); err == nil && entry.Name() > latest {
			latest = entry.Name()
		}
	}
	if latest == "" {
		return "", fmt.Errorf("no runs found in %s", resultsDir)
	}
	return latest, nil
}

func readLine(in *bufio.Reader) (string, error) {
	line, err := in.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

func sortedCases(cases []CaseResult) []CaseResult {
	sorted := append([]CaseResult(nil), cases...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return sorted
}

func sortedTrials(trials []TrialResult) []TrialResult {
	sorted := append([]TrialResult(nil), trials...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].N < sorted[j].N })
	return sorted
}

func showTrial(out io.Writer, result *Result, c CaseResult, trial TrialResult) {
	fmt.Fprintf(out, "%s trial %d: %s\n", c.ID, trial.N, c.Title)
	fmt.Fprintf(out, "kind: %s", c.Kind)
	if len(c.Tags) > 0 {
		fmt.Fprintf(out, "   tags: %s", strings.Join(c.Tags, ", "))
	}
	fmt.Fprintln(out)

	var cassette Cassette
	loaded := false
	if trial.Cassette != "" {
		if contents, err := os.ReadFile(filepath.Join(result.Dir, trial.Cassette)); err == nil {
			loaded = json.Unmarshal(contents, &cassette) == nil
		}
	}
	if !loaded {
		fmt.Fprintln(out, "(cassette not available: only the automatic verdict is shown)")
	}
	for _, step := range cassette.Steps {
		fmt.Fprintf(out, "\n-- step %d: %s\n", step.Index+1, describeStep(step.Step))
		for _, call := range step.ToolCalls {
			status := "ok"
			if call.IsError {
				status = "ERROR"
			}
			fmt.Fprintf(out, "  tool %s(%s) -> %s\n", call.Name, excerptTo(string(call.Arguments), reviewArgsRunes), status)
		}
		for _, proposal := range step.Proposals {
			fmt.Fprintf(out, "  proposal %s: %s [%s]\n", proposal.Tool, excerptTo(proposal.Summary, reviewArgsRunes), proposal.Status)
		}
		for _, proposal := range step.Resolved {
			fmt.Fprintf(out, "  resolved %s: %s [%s]\n", proposal.Tool, excerptTo(proposal.Summary, reviewArgsRunes), proposal.Status)
		}
		switch {
		case step.Quiet:
			fmt.Fprintln(out, "  reply: (quiet: nothing to say)")
		case strings.TrimSpace(step.Reply) == "":
			fmt.Fprintln(out, "  reply: (empty)")
		default:
			fmt.Fprintln(out, "  reply:")
			fmt.Fprintln(out, wrap(clipRunes(step.Reply, reviewReplyLimit), reviewWidth-4, "    "))
		}
	}

	fmt.Fprintf(out, "\nchecks: %s\n", strings.ToUpper(trial.Status))
	if trial.Error != "" {
		fmt.Fprintf(out, "  error: %s\n", trial.Error)
	}
	for _, failure := range trial.Failures {
		fmt.Fprintf(out, "  step %d, %s: %s\n", failure.Step, failure.Check, failure.Message)
	}
}

func describeStep(step Step) string {
	switch {
	case step.Say != "":
		return "user: " + step.Say
	case step.RunJob != "":
		return "job: " + step.RunJob
	case step.ThumbsDown:
		return "user gives a thumbs-down"
	case step.Confirm:
		return "user confirms the proposals"
	case step.Reject:
		return "user rejects the proposals"
	default:
		return "(empty step)"
	}
}

func clipRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}

// wrap breaks every line of text at width, indenting each output line.
func wrap(text string, width int, indent string) string {
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		line := indent + words[0]
		for _, word := range words[1:] {
			if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > width {
				lines = append(lines, line)
				line = indent + word
				continue
			}
			line += " " + word
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// agreement counts the trials where the operator and the checks gave the same verdict.
type agreement struct{ Agree, Total int }

func (a agreement) String() string {
	if a.Total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(a.Agree)/float64(a.Total), a.Agree, a.Total)
}

func (r *Result) reviewMarkdown(b *strings.Builder) {
	review := r.Review
	b.WriteString("\n## Operator review\n\n")
	var overall agreement
	perCase := map[string]*agreement{}
	var caseOrder, missed, falseAlarms, unsure []string
	reviewed := 0
	for _, c := range sortedCases(r.Cases) {
		for _, trial := range sortedTrials(c.Trials) {
			index, done := review.find(trialKey(c.ID, trial.N))
			if !done || !trial.Ran() {
				continue
			}
			reviewed++
			entry := review.Reviews[index]
			label := fmt.Sprintf("%s, trial %d", c.ID, trial.N)
			if entry.Note != "" {
				label += ": " + entry.Note
			}
			if entry.Verdict == VerdictUnsure {
				unsure = append(unsure, label)
				continue
			}
			if perCase[c.ID] == nil {
				perCase[c.ID] = &agreement{}
				caseOrder = append(caseOrder, c.ID)
			}
			passed := trial.Status == TrialPassed
			overall.Total++
			perCase[c.ID].Total++
			switch {
			case passed == (entry.Verdict == VerdictGood):
				overall.Agree++
				perCase[c.ID].Agree++
			case passed:
				missed = append(missed, label)
			default:
				falseAlarms = append(falseAlarms, label)
			}
		}
	}
	fmt.Fprintf(b, "- Reviewed trials: %d (unsure, excluded from agreement: %d)\n", reviewed, len(unsure))
	fmt.Fprintf(b, "- Agreement between the checks and the operator: %s\n", overall)
	fmt.Fprintf(b, "- Missed failures (operator bad, checks passed): %d\n", len(missed))
	fmt.Fprintf(b, "- False alarms (operator good, checks failed): %d\n", len(falseAlarms))
	if len(caseOrder) > 0 {
		b.WriteString("\n| Case | Agreement |\n| --- | --- |\n")
		for _, id := range caseOrder {
			fmt.Fprintf(b, "| %s | %s |\n", id, perCase[id])
		}
	}
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(b, "\n### %s\n\n", title)
		for _, item := range items {
			fmt.Fprintf(b, "- %s\n", item)
		}
	}
	list("Missed failures (operator bad, checks passed)", missed)
	list("False alarms (operator good, checks failed)", falseAlarms)
}

package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

type storeEvaluator func(ctx context.Context, raw json.RawMessage, env CheckEnv) error

var storeEvaluators = map[string]storeEvaluator{}

// registerStoreCheck registers a store check's shape with the case loader and its evaluator with
// the checks.
func registerStoreCheck(name string, validate func(json.RawMessage) error, evaluate storeEvaluator) {
	DefaultRegistry.RegisterStoreCheck(name, validate)
	storeEvaluators[name] = evaluate
}

func init() {
	registerStoreCheck("jobs", validateJobsCheck, checkJobs)
	registerStoreCheck("memory", validateMemoryCheck, checkMemory)
	registerStoreCheck("backlog", validateBacklogCheck, checkBacklog)
}

func storeCheckName(raw json.RawMessage) (string, error) {
	var head struct {
		Check string `json:"check"`
	}
	if err := json.Unmarshal(raw, &head); err != nil || head.Check == "" {
		return "", errors.New("want an object with a \"check\" name")
	}
	return head.Check, nil
}

// bounds is how many matching rows a check wants: exactly Count, or between Min and Max.
type bounds struct {
	Count *int `json:"count,omitempty"`
	Min   *int `json:"min,omitempty"`
	Max   *int `json:"max,omitempty"`
}

func (b bounds) set() bool { return b.Count != nil || b.Min != nil || b.Max != nil }

func (b bounds) validate() error {
	for name, value := range map[string]*int{"count": b.Count, "min": b.Min, "max": b.Max} {
		if value != nil && *value < 0 {
			return fmt.Errorf("%s: must not be negative", name)
		}
	}
	if b.Count != nil && (b.Min != nil || b.Max != nil) {
		return errors.New("count cannot be combined with min or max")
	}
	if b.Min != nil && b.Max != nil && *b.Max < *b.Min {
		return errors.New("max: must not be below min")
	}
	return nil
}

// verify holds when n is within the bounds; without any bounds a filtered check wants at least one
// match.
func (b bounds) verify(what string, n int, filtered bool) error {
	switch {
	case b.Count != nil:
		if n != *b.Count {
			return fmt.Errorf("%d %s, expected exactly %d", n, what, *b.Count)
		}
	case b.Min != nil || b.Max != nil:
		if b.Min != nil && n < *b.Min {
			return fmt.Errorf("%d %s, expected at least %d", n, what, *b.Min)
		}
		if b.Max != nil && n > *b.Max {
			return fmt.Errorf("%d %s, expected at most %d", n, what, *b.Max)
		}
	case filtered && n == 0:
		return fmt.Errorf("no %s", what)
	}
	return nil
}

// jobsCheck counts the principal's scheduled jobs that match every filter it sets. Seeded jobs and
// the Heads-up job count too. At is a local time in the job's zone, as 2026-03-10T15:00; Cron is
// compared as written; Recurring selects jobs with a cron schedule or without one.
type jobsCheck struct {
	Check          string `json:"check"`
	Name           string `json:"name,omitempty"`
	Cron           string `json:"cron,omitempty"`
	At             string `json:"at,omitempty"`
	PromptContains string `json:"prompt_contains,omitempty"`
	PromptEquals   string `json:"prompt_equals,omitempty"`
	Recurring      *bool  `json:"recurring,omitempty"`
	bounds
}

func (c jobsCheck) filtered() bool {
	return c.Name != "" || c.Cron != "" || c.At != "" || c.PromptContains != "" || c.PromptEquals != "" || c.Recurring != nil
}

func validateJobsCheck(raw json.RawMessage) error {
	var check jobsCheck
	if err := decodeStrict(raw, &check); err != nil {
		return err
	}
	if !check.filtered() && !check.set() {
		return errors.New("want a filter (name, cron, at, prompt_contains, prompt_equals, recurring) or count, min or max")
	}
	if check.At != "" {
		if _, err := time.Parse(ClockLayout, check.At); err != nil {
			return fmt.Errorf("at %q: want %s", check.At, ClockLayout)
		}
	}
	return check.validate()
}

func checkJobs(ctx context.Context, raw json.RawMessage, env CheckEnv) error {
	var check jobsCheck
	if err := decodeStrict(raw, &check); err != nil {
		return err
	}
	jobs, err := env.Store.ListScheduledJobs(ctx, PrincipalID)
	if err != nil {
		return fmt.Errorf("read the jobs: %w", err)
	}
	var matching int
	var seen []string
	for _, job := range jobs {
		seen = append(seen, describeJob(job, env.Location))
		if jobMatches(check, job, env.Location) {
			matching++
		}
	}
	if err := check.verify("matching job(s)", matching, check.filtered()); err != nil {
		return fmt.Errorf("%w; jobs are [%s]", err, excerpt(strings.Join(seen, "; ")))
	}
	return nil
}

func jobText(job schedule.Job) string {
	if job.Type == schedule.TypeReminder {
		return schedule.ReminderText(job.Config)
	}
	return schedule.ModelTurnPrompt(job.Config)
}

func jobLocation(job schedule.Job, fallback *time.Location) *time.Location {
	if job.Zone != "" {
		if location, err := time.LoadLocation(job.Zone); err == nil {
			return location
		}
	}
	return fallback
}

func jobMatches(check jobsCheck, job schedule.Job, fallback *time.Location) bool {
	text := strings.ToLower(jobText(job))
	switch {
	case check.Name != "" && !strings.EqualFold(job.Name, check.Name):
		return false
	case check.Cron != "" && strings.TrimSpace(job.Cron) != strings.TrimSpace(check.Cron):
		return false
	case check.At != "" && (job.RunAt.IsZero() || job.RunAt.In(jobLocation(job, fallback)).Format(ClockLayout) != check.At):
		return false
	case check.PromptContains != "" && !strings.Contains(text, strings.ToLower(check.PromptContains)):
		return false
	case check.PromptEquals != "" && strings.TrimSpace(jobText(job)) != strings.TrimSpace(check.PromptEquals):
		return false
	case check.Recurring != nil && (job.Cron != "") != *check.Recurring:
		return false
	}
	return true
}

func describeJob(job schedule.Job, fallback *time.Location) string {
	when := job.Cron
	if when == "" && !job.RunAt.IsZero() {
		when = "at " + job.RunAt.In(jobLocation(job, fallback)).Format(ClockLayout)
	}
	return fmt.Sprintf("%q %s", job.Name, when)
}

// memoryCheck counts the principal's memory entries, those containing Contains when it is set
// (compared without case) and of Kind when it is set.
type memoryCheck struct {
	Check    string `json:"check"`
	Contains string `json:"contains,omitempty"`
	Kind     string `json:"kind,omitempty"`
	bounds
}

func validateMemoryCheck(raw json.RawMessage) error {
	var check memoryCheck
	if err := decodeStrict(raw, &check); err != nil {
		return err
	}
	if check.Contains == "" && check.Kind == "" && !check.set() {
		return errors.New("want contains, kind, or count, min or max")
	}
	return check.validate()
}

func checkMemory(ctx context.Context, raw json.RawMessage, env CheckEnv) error {
	var check memoryCheck
	if err := decodeStrict(raw, &check); err != nil {
		return err
	}
	entries, err := env.Store.ListMemory(ctx, PrincipalID)
	if err != nil {
		return fmt.Errorf("read memory: %w", err)
	}
	var matching int
	for _, entry := range entries {
		if check.Kind != "" && entry.Kind != check.Kind {
			continue
		}
		if check.Contains != "" && !strings.Contains(strings.ToLower(entry.Text), strings.ToLower(check.Contains)) {
			continue
		}
		matching++
	}
	return check.verify("matching memory entr(ies)", matching, check.Contains != "" || check.Kind != "")
}

// backlogCheck counts the principal's backlog items: Count of every item, Open of the open ones.
type backlogCheck struct {
	Check string `json:"check"`
	Count *int   `json:"count,omitempty"`
	Open  *int   `json:"open,omitempty"`
}

func validateBacklogCheck(raw json.RawMessage) error {
	var check backlogCheck
	if err := decodeStrict(raw, &check); err != nil {
		return err
	}
	if check.Count == nil && check.Open == nil {
		return errors.New("want count or open")
	}
	if (check.Count != nil && *check.Count < 0) || (check.Open != nil && *check.Open < 0) {
		return errors.New("count and open must not be negative")
	}
	return nil
}

func checkBacklog(ctx context.Context, raw json.RawMessage, env CheckEnv) error {
	var check backlogCheck
	if err := decodeStrict(raw, &check); err != nil {
		return err
	}
	open, err := env.Store.ListBacklog(ctx, PrincipalID, backlog.StatusOpen)
	if err != nil {
		return fmt.Errorf("read the backlog: %w", err)
	}
	done, err := env.Store.ListBacklog(ctx, PrincipalID, backlog.StatusDone)
	if err != nil {
		return fmt.Errorf("read the backlog: %w", err)
	}
	if check.Open != nil && len(open) != *check.Open {
		return fmt.Errorf("%d open backlog item(s), expected %d", len(open), *check.Open)
	}
	if total := len(open) + len(done); check.Count != nil && total != *check.Count {
		return fmt.Errorf("%d backlog item(s), expected %d", total, *check.Count)
	}
	return nil
}

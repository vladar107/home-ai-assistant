// Package eval defines the agent evaluation cases and loads them from disk.
package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	ClockLayout     = "2006-01-02T15:04"
	timeOfDayLayout = "15:04"

	KindHappy     = "happy"
	DefaultTrials = 3
	MaxTrials     = 10
)

var (
	kebabPattern    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	languagePattern = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]+)*$`)
)

type Case struct {
	ID     string                     `json:"id"`
	Title  string                     `json:"title"`
	Tags   []string                   `json:"tags,omitempty"`
	Kind   string                     `json:"kind"`
	Clock  Clock                      `json:"clock"`
	User   User                       `json:"user"`
	World  map[string]json.RawMessage `json:"world,omitempty"`
	Steps  []Step                     `json:"steps"`
	Rubric []string                   `json:"rubric,omitempty"`
	Trials int                        `json:"trials,omitempty"`

	// File is the path the case was loaded from.
	File string `json:"-"`
}

type Clock struct {
	Local string `json:"local"`
	Zone  string `json:"zone"`
}

// Time returns the clock's instant in its zone.
func (c Clock) Time() (time.Time, error) {
	loc, err := time.LoadLocation(c.Zone)
	if err != nil {
		return time.Time{}, fmt.Errorf("clock.zone %q: %w", c.Zone, err)
	}
	t, err := time.ParseInLocation(ClockLayout, c.Local, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("clock.local %q: want %s: %w", c.Local, ClockLayout, err)
	}
	return t, nil
}

type User struct {
	Name     string          `json:"name,omitempty"`
	Switches map[string]bool `json:"switches,omitempty"`
	Memory   []string        `json:"memory,omitempty"`
	Model    string          `json:"model,omitempty"`
	Effort   string          `json:"effort,omitempty"`
	// Language is the user's interface language, such as en or ru; empty leaves the default.
	Language string `json:"language,omitempty"`
}

// Step holds exactly one action; Expect is checked after that action's turn.
type Step struct {
	Say        string  `json:"say,omitempty"`
	Confirm    bool    `json:"confirm,omitempty"`
	Reject     bool    `json:"reject,omitempty"`
	ThumbsDown bool    `json:"thumbs_down,omitempty"`
	RunJob     string  `json:"run_job,omitempty"`
	Expect     *Expect `json:"expect,omitempty"`
}

type Expect struct {
	Calls     []Call            `json:"calls,omitempty"`
	NotCalled []string          `json:"not_called,omitempty"`
	MaxCalls  *int              `json:"max_calls,omitempty"`
	Ordered   bool              `json:"ordered,omitempty"`
	Proposals *Proposals        `json:"proposals,omitempty"`
	NoWrites  bool              `json:"no_writes,omitempty"`
	Reply     *ReplyExpect      `json:"reply,omitempty"`
	Store     []json.RawMessage `json:"store,omitempty"`
}

// Call expects Min..Max calls of Tool; the loader sets an absent Min to 1.
type Call struct {
	Tool string    `json:"tool"`
	Min  *int      `json:"min,omitempty"`
	Max  *int      `json:"max,omitempty"`
	Args []ArgExpr `json:"args,omitempty"`
}

type ArgExpr struct {
	Path     string          `json:"path"`
	Equals   json.RawMessage `json:"equals,omitempty"`
	Contains *string         `json:"contains,omitempty"`
	Matches  *string         `json:"matches,omitempty"`
	Within   *Within         `json:"within,omitempty"`
}

// Within bounds a local time argument. Both ends are either ClockLayout
// date-times or "15:04" times of day.
type Within struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Proposals struct {
	Min   int      `json:"min,omitempty"`
	Max   *int     `json:"max,omitempty"`
	Tools []string `json:"tools,omitempty"`
}

type ReplyExpect struct {
	Contains    []string `json:"contains,omitempty"`
	NotContains []string `json:"not_contains,omitempty"`
	Matches     []string `json:"matches,omitempty"`
	Language    string   `json:"language,omitempty"`
	Quiet       bool     `json:"quiet,omitempty"`
}

// Registry holds the world sections and store checks that cases may use.
// Seeders and checkers register their name with a validator for the shape of
// their JSON.
type Registry struct {
	worlds map[string]func(json.RawMessage) error
	checks map[string]func(json.RawMessage) error
}

func NewRegistry() *Registry {
	return &Registry{
		worlds: map[string]func(json.RawMessage) error{},
		checks: map[string]func(json.RawMessage) error{},
	}
}

// DefaultRegistry is the registry LoadCases uses; world seeders register into it.
var DefaultRegistry = NewRegistry()

func (r *Registry) RegisterWorldSection(name string, validate func(json.RawMessage) error) {
	register(r.worlds, "world section", name, validate)
}

func (r *Registry) RegisterStoreCheck(name string, validate func(json.RawMessage) error) {
	register(r.checks, "store check", name, validate)
}

func register(into map[string]func(json.RawMessage) error, what, name string, validate func(json.RawMessage) error) {
	if name == "" || validate == nil {
		panic("eval: " + what + " needs a name and a validator")
	}
	if _, dup := into[name]; dup {
		panic("eval: duplicate " + what + " " + name)
	}
	into[name] = validate
}

// LoadCases reads every *.json file directly in dir, validating against DefaultRegistry.
func LoadCases(dir string) ([]Case, error) {
	return DefaultRegistry.LoadCases(dir)
}

// LoadCases reads every *.json file directly in dir in file name order.
func (r *Registry) LoadCases(dir string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read cases directory: %w", err)
	}
	var cases []Case
	seen := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		c, err := r.loadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if first, dup := seen[c.ID]; dup {
			return nil, fmt.Errorf("%s: duplicate case id %q, first used in %s", path, c.ID, first)
		}
		seen[c.ID] = path
		cases = append(cases, c)
	}
	return cases, nil
}

func (r *Registry) loadFile(path string) (Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Case{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Case
	if err := dec.Decode(&c); err != nil {
		return Case{}, fmt.Errorf("decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Case{}, errors.New("decode: unexpected data after the case object")
	}
	c.File = path
	if err := r.validate(&c); err != nil {
		return Case{}, err
	}
	return c, nil
}

func (r *Registry) validate(c *Case) error {
	if !kebabPattern.MatchString(c.ID) {
		return fmt.Errorf("id %q: want lowercase kebab case", c.ID)
	}
	if c.Title == "" {
		return errors.New("title is required")
	}
	if c.Kind != KindHappy && !kebabPattern.MatchString(c.Kind) {
		return fmt.Errorf("kind %q: want a lowercase kebab failure kind or %q", c.Kind, KindHappy)
	}
	if _, err := c.Clock.Time(); err != nil {
		return err
	}
	if c.User.Language != "" && !languagePattern.MatchString(c.User.Language) {
		return fmt.Errorf("user.language %q: want a BCP-47 tag such as en or ru", c.User.Language)
	}
	if c.Trials == 0 {
		c.Trials = DefaultTrials
	}
	if c.Trials < 1 || c.Trials > MaxTrials {
		return fmt.Errorf("trials %d: want 1..%d", c.Trials, MaxTrials)
	}
	for _, name := range sortedKeys(c.World) {
		validate, ok := r.worlds[name]
		if !ok {
			return fmt.Errorf("world.%s: unknown world section", name)
		}
		if err := validate(c.World[name]); err != nil {
			return fmt.Errorf("world.%s: %w", name, err)
		}
	}
	if len(c.Steps) == 0 {
		return errors.New("steps: at least one step is required")
	}
	for i := range c.Steps {
		if err := r.validateStep(&c.Steps[i], i); err != nil {
			return fmt.Errorf("steps[%d]: %w", i, err)
		}
	}
	return nil
}

func (r *Registry) validateStep(s *Step, i int) error {
	var actions []string
	if s.Say != "" {
		actions = append(actions, "say")
	}
	if s.Confirm {
		actions = append(actions, "confirm")
	}
	if s.Reject {
		actions = append(actions, "reject")
	}
	if s.ThumbsDown {
		actions = append(actions, "thumbs_down")
	}
	if s.RunJob != "" {
		actions = append(actions, "run_job")
	}
	switch len(actions) {
	case 0:
		return errors.New("no action: want one of say, confirm, reject, thumbs_down, run_job")
	case 1:
	default:
		return fmt.Errorf("want exactly one action, got %v", actions)
	}
	if i == 0 && actions[0] != "say" && actions[0] != "run_job" {
		return fmt.Errorf("%s cannot be the first step: it needs a previous turn", actions[0])
	}
	if s.Expect == nil {
		return nil
	}
	if err := r.validateExpect(s.Expect); err != nil {
		return fmt.Errorf("expect.%w", err)
	}
	return nil
}

func (r *Registry) validateExpect(e *Expect) error {
	for i := range e.Calls {
		if err := validateCall(&e.Calls[i]); err != nil {
			return fmt.Errorf("calls[%d]: %w", i, err)
		}
	}
	for i, tool := range e.NotCalled {
		if tool == "" {
			return fmt.Errorf("not_called[%d]: empty tool name", i)
		}
	}
	if e.MaxCalls != nil && *e.MaxCalls < 0 {
		return errors.New("max_calls: must not be negative")
	}
	if p := e.Proposals; p != nil {
		if p.Min < 0 || (p.Max != nil && *p.Max < p.Min) {
			return errors.New("proposals: want 0 <= min <= max")
		}
		if e.NoWrites && p.Min > 0 {
			return errors.New("proposals.min and no_writes contradict each other")
		}
	}
	if rep := e.Reply; rep != nil {
		if err := validateReply(rep); err != nil {
			return fmt.Errorf("reply.%w", err)
		}
	}
	for i, raw := range e.Store {
		var head struct {
			Check string `json:"check"`
		}
		if err := json.Unmarshal(raw, &head); err != nil || head.Check == "" {
			return fmt.Errorf("store[%d]: want an object with a \"check\" name", i)
		}
		validate, ok := r.checks[head.Check]
		if !ok {
			return fmt.Errorf("store[%d]: unknown check %q", i, head.Check)
		}
		if err := validate(raw); err != nil {
			return fmt.Errorf("store[%d] (%s): %w", i, head.Check, err)
		}
	}
	return nil
}

func validateCall(c *Call) error {
	if c.Tool == "" {
		return errors.New("tool is required")
	}
	if c.Min == nil {
		one := 1
		c.Min = &one
	}
	if *c.Min < 0 {
		return errors.New("min: must not be negative")
	}
	if c.Max != nil && *c.Max < *c.Min {
		return errors.New("max: must not be below min")
	}
	for i, a := range c.Args {
		if err := validateArg(a); err != nil {
			return fmt.Errorf("args[%d]: %w", i, err)
		}
	}
	return nil
}

func validateArg(a ArgExpr) error {
	if a.Path == "" {
		return errors.New("path is required")
	}
	n := 0
	if len(a.Equals) > 0 {
		n++
	}
	if a.Contains != nil {
		n++
	}
	if a.Matches != nil {
		n++
		if _, err := regexp.Compile(*a.Matches); err != nil {
			return fmt.Errorf("matches: %w", err)
		}
	}
	if a.Within != nil {
		n++
		if err := validateWithin(*a.Within); err != nil {
			return fmt.Errorf("within: %w", err)
		}
	}
	if n != 1 {
		return errors.New("want exactly one of equals, contains, matches, within")
	}
	return nil
}

func validateWithin(w Within) error {
	for _, layout := range []string{ClockLayout, timeOfDayLayout} {
		from, errFrom := time.Parse(layout, w.From)
		to, errTo := time.Parse(layout, w.To)
		if errFrom != nil || errTo != nil {
			continue
		}
		if layout == ClockLayout && to.Before(from) {
			return errors.New("to is before from")
		}
		return nil
	}
	return fmt.Errorf("from %q and to %q: want both as %s or both as %s", w.From, w.To, ClockLayout, timeOfDayLayout)
}

func validateReply(r *ReplyExpect) error {
	for i, expr := range r.Matches {
		if _, err := regexp.Compile(expr); err != nil {
			return fmt.Errorf("matches[%d]: %w", i, err)
		}
	}
	if r.Language != "" && !languagePattern.MatchString(r.Language) {
		return fmt.Errorf("language %q: want a BCP-47 tag such as en or ru", r.Language)
	}
	if r.Quiet && (len(r.Contains) > 0 || len(r.Matches) > 0 || r.Language != "") {
		return errors.New("quiet cannot be combined with checks on the reply text")
	}
	return nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

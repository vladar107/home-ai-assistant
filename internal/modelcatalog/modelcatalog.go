// Package modelcatalog reads the models a principal can choose from Pi's own cache. Pi refreshes
// that cache per profile, so the list follows what the subscription offers without a copy here.
package modelcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	storeFileName = "models-store.json"
	// lockStale matches the proper-lockfile setting Pi uses for the store, so a lock either side
	// abandons is reclaimed by the other after the same interval.
	lockStale = 30 * time.Second
	// lockWait is short because a person is waiting on the answer.
	lockWait     = 2 * time.Second
	lockInterval = 10 * time.Millisecond
	maximumStore = 8 << 20
)

// ErrUnavailable means the principal's catalog is missing, locked too long, or unreadable.
var ErrUnavailable = errors.New("model catalog is unavailable")

// Model is one entry of the catalog.
type Model struct {
	ID            string
	Name          string
	ContextWindow int
	// InputCost and OutputCost are per million tokens, in the catalog's currency.
	InputCost  float64
	OutputCost float64
	// Efforts are the levels to offer, least to most, one per distinct effort the model maps to.
	Efforts []string
	// mapped sends every level Pi accepts on this model, aliases included, to the effort it runs.
	mapped map[string]string
}

// Supports reports whether the model runs on the effort, aliases that Pi maps onto another
// level included.
func (m Model) Supports(effort string) bool {
	_, ok := m.Canonical(effort)
	return ok
}

// Canonical returns the level to use for the requested one: the entry of Efforts that runs the
// same effort, always a name Pi accepts and never the provider's own string. It returns false
// when the model does not support the level.
func (m Model) Canonical(effort string) (string, bool) {
	if m.mapped != nil {
		runs, ok := m.mapped[effort]
		if !ok {
			return "", false
		}
		for _, level := range m.Efforts {
			if m.mapped[level] == runs {
				return level, true
			}
		}
		return "", false
	}
	return effort, slices.Contains(m.Efforts, effort)
}

// Reader lists the models a principal can choose from.
type Reader interface {
	Models(ctx context.Context, principalID string) ([]Model, error)
}

// Catalog reads the catalog of one provider from each principal's Pi profile.
type Catalog struct {
	profilesDir string
	provider    string
}

var _ Reader = (*Catalog)(nil)

func New(profilesDir, provider string) *Catalog {
	return &Catalog{profilesDir: profilesDir, provider: provider}
}

// Models returns the principal's models in the catalog's order, or ErrUnavailable.
func (c *Catalog) Models(ctx context.Context, principalID string) ([]Model, error) {
	name, err := agentruntime.ProfileDirectoryName(principalID)
	if err != nil {
		return nil, ErrUnavailable
	}
	path := filepath.Join(c.profilesDir, name, storeFileName)
	release, err := lockStore(ctx, path)
	if err != nil {
		return nil, ErrUnavailable
	}
	data, err := readStore(path)
	release()
	if err != nil {
		return nil, ErrUnavailable
	}
	return parse(data, c.provider)
}

// Fallback is what can be offered without a catalog: the models the defaults name, each on the
// efforts its defaults name, since nothing says what else it supports.
func Fallback(defaults ...agentruntime.ModelEffort) []Model {
	var models []Model
	for _, d := range defaults {
		if d.Model == "" {
			continue
		}
		index := slices.IndexFunc(models, func(m Model) bool { return m.ID == d.Model })
		if index < 0 {
			models = append(models, Model{ID: d.Model, Name: d.Model, Efforts: []string{d.Effort}})
			continue
		}
		if !slices.Contains(models[index].Efforts, d.Effort) {
			efforts := append(models[index].Efforts, d.Effort)
			slices.SortStableFunc(efforts, func(a, b string) int {
				return slices.Index(agentruntime.Efforts, a) - slices.Index(agentruntime.Efforts, b)
			})
			models[index].Efforts = efforts
		}
	}
	return models
}

type storeModel struct {
	ID            string                          `json:"id"`
	Name          string                          `json:"name"`
	Reasoning     bool                            `json:"reasoning"`
	ContextWindow int                             `json:"contextWindow"`
	Cost          struct{ Input, Output float64 } `json:"cost"`
	LevelMap      map[string]json.RawMessage      `json:"thinkingLevelMap"`
}

func parse(data []byte, provider string) ([]Model, error) {
	var store map[string]struct {
		Models []storeModel `json:"models"`
	}
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, ErrUnavailable
	}
	entry, ok := store[provider]
	if !ok {
		return nil, ErrUnavailable
	}
	var models []Model
	for _, m := range entry.Models {
		if m.ID == "" {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		efforts, mapped := supportedEfforts(m.Reasoning, m.LevelMap)
		models = append(models, Model{
			ID: m.ID, Name: name, ContextWindow: m.ContextWindow,
			InputCost: m.Cost.Input, OutputCost: m.Cost.Output,
			Efforts: efforts, mapped: mapped,
		})
	}
	if len(models) == 0 {
		return nil, ErrUnavailable
	}
	return models, nil
}

// supportedEfforts follows Pi: a null entry hides a level, xhigh and max need an entry to exist,
// and a model that does not reason has only "off". Levels that map to the same effort are one
// choice, kept under the name that matches the effort when there is one. The second result maps
// every accepted level, aliases included, to the effort it runs.
func supportedEfforts(reasoning bool, levels map[string]json.RawMessage) ([]string, map[string]string) {
	if !reasoning {
		return []string{"off"}, map[string]string{"off": "off"}
	}
	mappedLevels := map[string]string{}
	var order []string
	groups := map[string][]string{}
	for _, level := range agentruntime.Efforts {
		raw, present := levels[level]
		mapped := level
		if present {
			if string(raw) == "null" {
				continue
			}
			var text string
			if json.Unmarshal(raw, &text) == nil && text != "" {
				mapped = text
			}
		} else if level == "xhigh" || level == "max" {
			continue
		}
		mappedLevels[level] = mapped
		if _, seen := groups[mapped]; !seen {
			order = append(order, mapped)
		}
		groups[mapped] = append(groups[mapped], level)
	}
	efforts := make([]string, 0, len(order))
	for _, mapped := range order {
		group := groups[mapped]
		if slices.Contains(group, mapped) {
			efforts = append(efforts, mapped)
		} else {
			efforts = append(efforts, group[0])
		}
	}
	slices.SortStableFunc(efforts, func(a, b string) int {
		return slices.Index(agentruntime.Efforts, a) - slices.Index(agentruntime.Efforts, b)
	})
	return efforts, mappedLevels
}

func readStore(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximumStore {
		return nil, fmt.Errorf("model catalog is not a readable file")
	}
	data := make([]byte, info.Size())
	if _, err := file.ReadAt(data, 0); err != nil && info.Size() > 0 {
		return nil, err
	}
	return data, nil
}

// lockStore takes the lock Pi takes around the store: a directory beside the file whose
// modification time marks it alive.
func lockStore(ctx context.Context, path string) (func(), error) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > lockStale {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("model catalog is locked")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockInterval):
		}
	}
}

var idPattern = regexp.MustCompile(`^(?:[a-z]+-)?(\d+(?:\.\d+)*)(?:-(.+))?$`)

// family splits "gpt-6.1-sol" into the family "sol" and the version [6 1]; an id with no family
// such as "gpt-5.4" has an empty one.
func family(id string) (string, []int, bool) {
	match := idPattern.FindStringSubmatch(strings.ToLower(id))
	if match == nil {
		return "", nil, false
	}
	var version []int
	for _, part := range strings.Split(match[1], ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return "", nil, false
		}
		version = append(version, n)
	}
	return match[2], version, true
}

// ResolveError says why a name did not pick one model, with the choices that would have.
type ResolveError struct {
	Ambiguous bool
	// Options are the ids that tie, or every id when the name matches none.
	Options []string
}

func (e *ResolveError) Error() string {
	if e.Ambiguous {
		return "model name is ambiguous"
	}
	return "model is not in the catalog"
}

// Resolve finds the model a name means: an exact id, or a family name such as "sol", which is the
// model of that family with the highest version. A name that fits several models equally is
// refused.
func Resolve(models []Model, name string) (Model, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, m := range models {
		if strings.ToLower(m.ID) == name {
			return m, nil
		}
	}
	var best []Model
	var bestVersion []int
	for _, m := range models {
		fam, version, ok := family(m.ID)
		if !ok || name == "" || fam != name {
			continue
		}
		switch cmp := slices.Compare(version, bestVersion); {
		case best == nil || cmp > 0:
			best, bestVersion = []Model{m}, version
		case cmp == 0:
			best = append(best, m)
		}
	}
	switch len(best) {
	case 0:
		return Model{}, &ResolveError{Options: ids(models)}
	case 1:
		return best[0], nil
	default:
		return Model{}, &ResolveError{Ambiguous: true, Options: ids(best)}
	}
}

func ids(models []Model) []string {
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = m.ID
	}
	return out
}

// Describe returns one line per model, in order: its name, context size, and cost relative to the
// cheapest model of the list.
func Describe(models []Model) []string {
	cheapest := 0.0
	for _, m := range models {
		if m.OutputCost > 0 && (cheapest == 0 || m.OutputCost < cheapest) {
			cheapest = m.OutputCost
		}
	}
	lines := make([]string, len(models))
	for i, m := range models {
		parts := []string{m.Name}
		if m.ContextWindow > 0 {
			parts = append(parts, contextSize(m.ContextWindow)+" context")
		}
		if cheapest > 0 && m.OutputCost > 0 {
			if ratio := m.OutputCost / cheapest; ratio < 1.05 {
				parts = append(parts, "lowest cost")
			} else {
				parts = append(parts, strconv.FormatFloat(round1(ratio), 'f', -1, 64)+"x cost")
			}
		}
		lines[i] = strings.Join(parts, ", ")
	}
	return lines
}

func contextSize(tokens int) string {
	if tokens >= 1_000_000 {
		return strconv.FormatFloat(round1(float64(tokens)/1_000_000), 'f', -1, 64) + "M"
	}
	return strconv.Itoa(tokens/1000) + "k"
}

func round1(value float64) float64 {
	return float64(int(value*10+0.5)) / 10
}

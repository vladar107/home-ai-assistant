package homeassistant

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const (
	statesCacheKey   = "homeassistant:states"
	maximumCandidate = 5

	watchPending = "pending"
	watchHolding = "holding"
	watchOK      = "ok"
)

var (
	_ watch.Evaluator = (*Service)(nil)
	_ watch.Resolver  = (*Service)(nil)
)

// visible returns the entities the assistant can see, by entity ID with their friendly names: the
// ones labelled ConfirmLabel and the ones exposed to Assist. The live context names exposed
// entities without IDs, so a name counts only when, within the entry's domain, it belongs to
// exactly one entity; an ambiguous name makes none of them visible.
func (s *snapshot) visible() map[string]string {
	s.visibleOnce.Do(func() {
		s.visibleIDs = make(map[string]string)
		for id, entity := range s.confirmed {
			s.visibleIDs[id] = entity.name
		}
		for _, exposed := range s.exposed {
			for _, name := range exposed.names {
				var matches []string
				for _, id := range s.entityIDs[strings.ToLower(name)] {
					if exposed.domain == "" || domainOfEntity(id) == exposed.domain {
						matches = append(matches, id)
					}
				}
				if len(matches) == 1 {
					s.visibleIDs[matches[0]] = name
				}
			}
		}
	})
	return s.visibleIDs
}

// Resolve checks that a home watch looks at one entity the assistant can see and that its
// condition fits the entity's state, and returns the spec with the entity ID and friendly name.
func (s *Service) Resolve(ctx context.Context, principalID string, spec watch.Spec) (watch.Spec, error) {
	status, err := s.Status(ctx, principalID)
	if err != nil {
		return watch.Spec{}, err
	}
	if !status.Enabled {
		return watch.Spec{}, errors.New("the home is switched off for this user, so its devices cannot be watched")
	}
	snap, err := s.current(ctx)
	if err != nil {
		return watch.Spec{}, err
	}
	visible := snap.visible()
	target := &spec.Target

	id := target.Entity
	if _, ok := visible[id]; !ok {
		id = ""
	}
	if id == "" {
		name := strings.TrimSpace(target.Name)
		if name == "" {
			return watch.Spec{}, errors.New("name the device to watch")
		}
		var matches []string
		for candidate, friendly := range visible {
			if strings.EqualFold(friendly, name) {
				matches = append(matches, candidate)
			}
		}
		sort.Strings(matches)
		switch {
		case len(matches) == 1:
			id = matches[0]
		case len(matches) > 1:
			return watch.Spec{}, fmt.Errorf("%q means several devices (%s); ask which one", name, strings.Join(matches, ", "))
		default:
			return watch.Spec{}, unknownDevice(name, visible)
		}
	}
	target.Entity = id
	target.Name = visible[id]

	if spec.Condition != nil && domainOfEntity(id) == "binary_sensor" {
		if len(spec.Condition.State) == 0 {
			return watch.Spec{}, fmt.Errorf("%s is a binary sensor; watch for it being on or off, not for a number", target.Name)
		}
		states := make([]string, 0, len(spec.Condition.State))
		for _, wanted := range spec.Condition.State {
			mapped, ok := binaryState(wanted)
			if !ok {
				return watch.Spec{}, fmt.Errorf("binary sensors report on or off, not %q; for a door, open is on", wanted)
			}
			states = append(states, mapped)
		}
		condition := *spec.Condition
		condition.State = states
		spec.Condition = &condition
	}
	if spec.Condition != nil && (spec.Condition.Above != nil || spec.Condition.Below != nil) {
		var current stateObject
		if err := s.client.getJSON(ctx, "/api/states/"+url.PathEscape(id), &current); err != nil {
			return watch.Spec{}, err
		}
		if unusable(current.State) {
			return watch.Spec{}, fmt.Errorf("%s is %s right now, so its value cannot be checked yet", target.Name, current.State)
		}
		if _, err := strconv.ParseFloat(current.State, 64); err != nil {
			return watch.Spec{}, fmt.Errorf("%s reports %q, not a number; use a state condition instead", target.Name, current.State)
		}
	}
	return spec, nil
}

// unknownDevice refuses a name that matches no visible device, listing the closest ones.
func unknownDevice(name string, visible map[string]string) error {
	lowered := strings.ToLower(name)
	var near []string
	for _, friendly := range visible {
		other := strings.ToLower(friendly)
		if strings.Contains(other, lowered) || strings.Contains(lowered, other) || sharesWord(other, lowered) {
			near = append(near, friendly)
		}
	}
	sort.Strings(near)
	if len(near) > maximumCandidate {
		near = near[:maximumCandidate]
	}
	if len(near) == 0 {
		return fmt.Errorf("no device the assistant can see is called %q", name)
	}
	return fmt.Errorf("no device the assistant can see is called %q; did you mean %s?", name, strings.Join(near, ", "))
}

func sharesWord(a, b string) bool {
	words := make(map[string]bool)
	for _, word := range strings.Fields(a) {
		if len(word) > 2 {
			words[word] = true
		}
	}
	for _, word := range strings.Fields(b) {
		if words[word] {
			return true
		}
	}
	return false
}

// Evaluate checks one home watch against Home Assistant's current states, which all of a cycle's
// home watches read once. A state Home Assistant cannot report, or an entity the assistant no
// longer sees, fails the check rather than counting as normal, so it never clears an alert.
func (s *Service) Evaluate(ctx context.Context, principalID string, w watch.Watch, occ watch.Occurrence) (watch.Observation, error) {
	condition := w.Condition
	if condition == nil {
		return watch.Observation{}, errors.New("the watch has no condition")
	}
	status, err := s.Status(ctx, principalID)
	if err != nil {
		return watch.Observation{}, err
	}
	if !status.Enabled {
		return watch.Observation{}, fmt.Errorf("the home is switched off: %w", watch.ErrSkip)
	}
	snap, err := s.current(ctx)
	if err != nil {
		return watch.Observation{}, &watch.UpstreamError{Service: Integration, Err: err}
	}
	if _, ok := snap.visible()[w.Target.Entity]; !ok {
		return watch.Observation{}, fmt.Errorf("%s is no longer exposed to the assistant", w.Target.Entity)
	}
	states, err := watch.Cached(ctx, occ.Cache, statesCacheKey, s.fetchStates)
	if err != nil {
		return watch.Observation{}, &watch.UpstreamError{Service: Integration, Err: err}
	}
	entity, found := states[w.Target.Entity]
	if !found {
		return watch.Observation{}, fmt.Errorf("Home Assistant no longer reports %s", w.Target.Entity)
	}
	name := entity.friendlyName()
	if name == "" {
		name = w.Target.Name
	}
	if name == "" {
		name = w.Target.Entity
	}

	holds, err := conditionHolds(*condition, entity)
	if err != nil {
		return watch.Observation{}, fmt.Errorf("%s: %w", name, err)
	}
	if !holds {
		return watch.Observation{State: watchOK, Headline: normalHeadline(name, *condition, entity)}, nil
	}

	now := s.now()
	since := heldSince(*condition, entity, occ, now)
	if now.Sub(since) < time.Duration(condition.ForMinutes)*time.Minute {
		return watch.Observation{State: watchPending, Silent: true, Since: since}, nil
	}
	return watch.Observation{State: watchHolding, Triggered: true, Headline: holdingHeadline(name, *condition, entity), Since: since}, nil
}

func (s *Service) fetchStates(ctx context.Context) (map[string]stateObject, error) {
	var states []stateObject
	if err := s.client.getJSON(ctx, "/api/states", &states); err != nil {
		return nil, err
	}
	byID := make(map[string]stateObject, len(states))
	for _, state := range states {
		byID[state.EntityID] = state
	}
	return byID, nil
}

func unusable(state string) bool {
	return state == "unavailable" || state == "unknown"
}

func conditionHolds(condition watch.Condition, entity stateObject) (bool, error) {
	if len(condition.State) > 0 {
		for _, wanted := range condition.State {
			if strings.EqualFold(wanted, entity.State) {
				return true, nil
			}
		}
		if unusable(entity.State) {
			return false, fmt.Errorf("the state is %s", entity.State)
		}
		return false, nil
	}
	if unusable(entity.State) {
		return false, fmt.Errorf("the state is %s", entity.State)
	}
	value, err := strconv.ParseFloat(entity.State, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return false, fmt.Errorf("the state %q is not a number", entity.State)
	}
	if condition.Above != nil {
		return value > *condition.Above, nil
	}
	return value < *condition.Below, nil
}

// heldSince is when the condition started holding. Once it was holding, the stored start stays:
// a Home Assistant restart moves every last_changed. A numeric value carries no useful change
// time, so its start is the first check that saw it hold; a state is seeded from the entity's own
// last_changed.
func heldSince(condition watch.Condition, entity stateObject, occ watch.Occurrence, now time.Time) time.Time {
	if !occ.Since.IsZero() && (occ.Previous == watchPending || occ.Previous == watchHolding) {
		return occ.Since
	}
	if len(condition.State) == 0 {
		return now
	}
	changed, err := time.Parse(time.RFC3339Nano, entity.LastChanged)
	if err != nil || changed.After(now) {
		return now
	}
	return changed
}

func holdingHeadline(name string, condition watch.Condition, entity stateObject) string {
	duration := ""
	if condition.ForMinutes > 0 {
		duration = fmt.Sprintf(" for %d %s", condition.ForMinutes, plural(condition.ForMinutes, "minute"))
	}
	if len(condition.State) > 0 {
		if duration == "" {
			return fmt.Sprintf("%s is %s.", name, displayState(entity))
		}
		return fmt.Sprintf("%s has been %s%s.", name, displayState(entity), duration)
	}
	limit, relation := 0.0, "below"
	if condition.Above != nil {
		limit, relation = *condition.Above, "above"
	} else {
		limit = *condition.Below
	}
	return fmt.Sprintf("%s is %s, %s %s%s.", name, withUnit(entity.State, entity.unit()), relation, withUnit(strconv.FormatFloat(limit, 'f', -1, 64), entity.unit()), duration)
}

func normalHeadline(name string, condition watch.Condition, entity stateObject) string {
	if len(condition.State) > 0 {
		return fmt.Sprintf("%s is %s.", name, displayState(entity))
	}
	return fmt.Sprintf("%s is %s.", name, withUnit(entity.State, entity.unit()))
}

func withUnit(value, unit string) string {
	switch unit {
	case "":
		return value
	case "%":
		return value + unit
	}
	return value + " " + unit
}

func plural(count int, word string) string {
	if count == 1 {
		return word
	}
	return word + "s"
}

// binaryState maps the words people use for a binary sensor to the on or off Home Assistant
// reports.
func binaryState(word string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "on", "open", "opened", "detected", "wet", "motion":
		return "on", true
	case "off", "closed", "clear", "dry", "not detected":
		return "off", true
	}
	return "", false
}

var binaryWords = map[string][2]string{
	"door":        {"open", "closed"},
	"window":      {"open", "closed"},
	"opening":     {"open", "closed"},
	"garage_door": {"open", "closed"},
	"moisture":    {"wet", "dry"},
	"motion":      {"detected", "clear"},
	"occupancy":   {"detected", "clear"},
	"presence":    {"detected", "clear"},
}

// displayState is the entity's state as a person says it: a binary sensor's on and off become
// open and closed, wet and dry, or detected and clear by its device class.
func displayState(entity stateObject) string {
	if domainOfEntity(entity.EntityID) != "binary_sensor" {
		return entity.State
	}
	class, _ := entity.Attributes["device_class"].(string)
	if words, ok := binaryWords[class]; ok {
		switch entity.State {
		case "on":
			return words[0]
		case "off":
			return words[1]
		}
	}
	return entity.State
}

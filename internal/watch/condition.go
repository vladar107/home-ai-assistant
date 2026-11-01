package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/localtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// InstantChecker is an evaluator that can also tell whether a spec's condition holds at one
// instant, for a scheduled job's condition. Evaluate is stateful and windowed, and an agent watch's
// finishes in the background, so a condition never calls it.
type InstantChecker interface {
	CheckAt(ctx context.Context, principalID string, spec Spec, at time.Time) (Verdict, error)
}

// Verdict is what one instant check found: whether the condition holds, and a short value to tell
// the user, such as when the rain starts. Private and External are set when the value may retell
// private content or content from outside the household, such as a train service's notice.
type Verdict struct {
	Met      bool
	Value    string
	Private  bool
	External bool
}

// conditionArguments is a condition as the model gives it: a kind and the fields watch_propose
// takes for it.
type conditionArguments struct {
	Kind string `json:"kind"`
	specArguments
}

// PrepareCondition validates a job's condition as the model gives it and returns it as the spec
// the job stores, with its description as the user reads it. A route, a home condition that must
// hold for some minutes, and, with light, an agent check, which costs a model turn, are refused.
func (s *Service) PrepareCondition(ctx context.Context, principalID string, raw json.RawMessage, light bool) (json.RawMessage, string, error) {
	var arguments conditionArguments
	if err := tools.DecodeArguments(raw, &arguments); err != nil {
		return nil, "", fmt.Errorf("the condition takes kind and the fields watch_propose takes for that kind")
	}
	kind := strings.TrimSpace(arguments.Kind)
	switch {
	case kind == KindRoute:
		return nil, "", fmt.Errorf("a route cannot be a condition, because whether a route works depends on the legs it would take; use a departure condition for the train or bus instead")
	case kind == KindPlace:
		return nil, "", fmt.Errorf("place conditions are not available yet")
	case light && kind != KindWeather && kind != KindHome:
		return nil, "", fmt.Errorf("a reminder is sent as written, so its condition can only be weather or home, whose values code writes; for a departure, disruptions, or agent condition, make it a scheduled job")
	case kind == KindHome && arguments.Condition != nil && arguments.Condition.ForMinutes > 0:
		return nil, "", fmt.Errorf("a condition is checked once, when the job is due, so it cannot wait for something to hold for_minutes; leave for_minutes out")
	case len(arguments.Days) > 0 || arguments.Date != "" || arguments.WindowFrom != "" || arguments.WindowUntil != "" || arguments.MinutesBefore != 0 || arguments.PauseWhileTraveling != nil:
		return nil, "", fmt.Errorf("a condition is checked when the job is due, so it takes no days, date, window, minutes_before, or pause_while_traveling")
	}
	evaluator, known := s.evaluators[kind]
	if !known {
		return nil, "", fmt.Errorf("%s conditions are not available here", kind)
	}
	if _, ok := evaluator.(InstantChecker); !ok {
		return nil, "", fmt.Errorf("a %s watch cannot be a condition", kind)
	}
	spec, err := s.conditionSpec(kind, arguments.specArguments)
	if err != nil {
		return nil, "", err
	}
	if resolver, ok := evaluator.(Resolver); ok {
		resolved, err := resolver.Resolve(ctx, principalID, spec)
		if err != nil {
			return nil, "", err
		}
		if spec, err = normalizeWhat(resolved); err != nil {
			return nil, "", err
		}
	}
	if kind == KindHome && spec.Target.Entity == "" {
		return nil, "", fmt.Errorf("the device could not be matched to one entity")
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, "", err
	}
	return encoded, describeCondition(spec), nil
}

// conditionSpec is a condition's spec: what it checks, and for a departure the planned departure
// as its End, with no days or window.
func (s *Service) conditionSpec(kind string, arguments specArguments) (Spec, error) {
	spec := Spec{
		Kind: kind,
		Target: Target{
			Mode: arguments.Mode, From: arguments.From, To: arguments.To, Via: arguments.Via,
			Stop: arguments.Stop, Line: arguments.Line, Stations: arguments.Stations, Route: arguments.Route,
			Entity: arguments.Entity, Name: arguments.Name, Current: arguments.CurrentLocation,
		},
		Condition:    arguments.Condition,
		Check:        arguments.Check,
		Weather:      weatherOf(kind, arguments),
		Triggers:     arguments.Triggers,
		DelayMinutes: arguments.DelayMinutes,
		Zone:         strings.TrimSpace(arguments.Timezone),
		Language:     arguments.Language,
	}
	if spec.Zone == "" {
		spec.Zone = s.zone
	}
	if kind == KindDeparture {
		departure, err := time.Parse(clockLayout, strings.TrimSpace(arguments.Departure))
		if err != nil {
			return Spec{}, fmt.Errorf("a departure condition needs departure, the planned local time, HH:MM, such as 07:42")
		}
		spec.End = departure.Format(clockLayout)
	} else if arguments.Departure != "" {
		return Spec{}, fmt.Errorf("only a departure condition takes departure")
	}
	return normalizeWhat(spec)
}

// CheckCondition checks a job's stored condition once at the instant.
func (s *Service) CheckCondition(ctx context.Context, principalID string, raw json.RawMessage, at time.Time) (schedule.ConditionResult, error) {
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return schedule.ConditionResult{}, fmt.Errorf("the condition could not be read")
	}
	evaluator, known := s.evaluators[spec.Kind]
	if !known {
		return schedule.ConditionResult{}, fmt.Errorf("%s conditions are not available now", spec.Kind)
	}
	checker, ok := evaluator.(InstantChecker)
	if !ok {
		return schedule.ConditionResult{}, fmt.Errorf("a %s watch cannot be a condition", spec.Kind)
	}
	if spec.Kind != KindAgent {
		// An agent check is a model turn bounded by the turn's own time limit.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	verdict, err := checker.CheckAt(ctx, principalID, spec, at)
	if err != nil {
		return schedule.ConditionResult{}, err
	}
	return schedule.ConditionResult{Met: verdict.Met, Value: verdict.Value, Private: verdict.Private, External: verdict.External}, nil
}

// describeCondition is a condition as the user reads it after "only if".
func describeCondition(spec Spec) string {
	switch spec.Kind {
	case KindWeather:
		text := "rain is expected at " + spec.placeName()
		if spec.Weather != nil {
			text += fmt.Sprintf(" within %d min", spec.Weather.WithinMinutes)
			if spec.Weather.ThresholdMM != DefaultThresholdMM {
				text += fmt.Sprintf(" (%s)", spec.Weather.Describe())
			}
		}
		return text
	case KindHome:
		name := spec.Target.Name
		if name == "" {
			name = spec.Target.Entity
		}
		if spec.Condition == nil {
			return name + " is as watched"
		}
		return name + " is " + spec.Condition.Describe()
	case KindDeparture:
		return spec.Label() + " has " + spec.DescribeTriggers()
	case KindDisruptions:
		return "there are disruptions or engineering works at " + strings.Join(spec.Target.Stations, ", ")
	case KindAgent:
		if spec.Check != nil {
			return spec.Check.Condition
		}
	}
	return spec.Label()
}

// departureAt is the planned departure of a departure condition on the local day of the instant.
func departureAt(spec Spec, at time.Time) (time.Time, error) {
	location, err := localtime.LoadZone(spec.Zone)
	if err != nil {
		return time.Time{}, err
	}
	return clockOn(at.In(location), spec.End)
}

func weatherOf(kind string, arguments specArguments) *Weather {
	if kind != KindWeather && arguments.WithinMinutes == 0 && arguments.ThresholdMM == nil {
		return nil
	}
	weather := &Weather{WithinMinutes: arguments.WithinMinutes}
	if arguments.ThresholdMM != nil {
		weather.ThresholdMM = *arguments.ThresholdMM
	}
	return weather
}

// CheckAt checks the condition's one planned departure on the instant's day: it holds when one of
// its triggers does, such as the train being cancelled or late.
func (e *DepartureEvaluator) CheckAt(ctx context.Context, principalID string, spec Spec, at time.Time) (Verdict, error) {
	planned, err := departureAt(spec, at)
	if err != nil {
		return Verdict{}, err
	}
	departure, err := e.check(ctx, principalID, spec.Target, Occurrence{End: planned})
	if err != nil {
		return Verdict{}, err
	}
	observation := departureObservation(spec, departure, planned.Location())
	// The status phrase and its note are the train or bus service's own text.
	return Verdict{Met: observation.Triggered, Value: observation.Headline, External: true}, nil
}

// CheckAt holds when NS reports a disruption or engineering work in effect now at any of the
// stations.
func (e *DisruptionsEvaluator) CheckAt(ctx context.Context, principalID string, spec Spec, at time.Time) (Verdict, error) {
	var titles []string
	seen := make(map[string]bool)
	for _, station := range spec.Target.Stations {
		entries, err := e.trains.Disruptions(ctx, principalID, station, time.Time{})
		if err != nil {
			return Verdict{}, connectorError(serviceNS, err)
		}
		for _, entry := range entries {
			if !seen[entry.ID] {
				seen[entry.ID] = true
				titles = append(titles, entry.Title)
			}
		}
	}
	if len(titles) == 0 {
		return Verdict{Value: "NS reports no disruptions or engineering works at " + strings.Join(spec.Target.Stations, ", ") + "."}, nil
	}
	return Verdict{Met: true, Value: strings.Join(titles, "; "), External: true}, nil
}

// CheckAt runs one check turn now and waits for it, which costs a model turn on the owner's own
// subscription.
func (e *AgentEvaluator) CheckAt(ctx context.Context, principalID string, spec Spec, at time.Time) (Verdict, error) {
	if spec.Check == nil {
		return Verdict{}, fmt.Errorf("the agent condition has no check")
	}
	watch := Watch{PrincipalID: principalID, Spec: spec}
	reply, err := e.runner.RunCheck(ctx, principalID, checkMessage(watch, "", at))
	if err != nil {
		return Verdict{}, err
	}
	observation, err := observe(watch, "", reply)
	if err != nil {
		return Verdict{}, err
	}
	return Verdict{Met: observation.Triggered, Value: observation.Headline, Private: reply.Private, External: reply.External}, nil
}

var (
	_ InstantChecker = (*DepartureEvaluator)(nil)
	_ InstantChecker = (*DisruptionsEvaluator)(nil)
	_ InstantChecker = (*AgentEvaluator)(nil)
)

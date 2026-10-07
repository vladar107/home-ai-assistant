package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/ns"
)

const noDisruptions = "none"

// DisruptionsEvaluator checks disruption watches: the NS disruptions and engineering works that
// touch the watch's stations, under the owner's own trains switch.
type DisruptionsEvaluator struct {
	trains *ns.Service
}

var (
	_ Evaluator = (*DisruptionsEvaluator)(nil)
	_ Resolver  = (*DisruptionsEvaluator)(nil)
)

func NewDisruptionsEvaluator(trains *ns.Service) (*DisruptionsEvaluator, error) {
	if trains == nil {
		return nil, fmt.Errorf("disruption watches need the NS connector")
	}
	return &DisruptionsEvaluator{trains: trains}, nil
}

// Resolve refuses a watch for an owner with trains off and a name that is not one station, and
// returns the stations with their full names.
func (e *DisruptionsEvaluator) Resolve(ctx context.Context, principalID string, spec Spec) (Spec, error) {
	stations := make([]string, 0, len(spec.Target.Stations))
	for _, query := range spec.Target.Stations {
		name, err := e.trains.ResolveStation(ctx, principalID, "station", query)
		if err != nil {
			return Spec{}, err
		}
		if !slices.Contains(stations, name) {
			stations = append(stations, name)
		}
	}
	spec.Target.Stations = stations
	return spec, nil
}

// Evaluate asks NS, per station, what is in effect now and what will be at the end of the window,
// so engineering works starting before the user leaves are told in advance. The per-station
// queries are smaller than the national list and leave matching stations to NS. Each entry counts
// by its ID and a fingerprint of what the user reads, so a new expected end or advice is a change
// and reordering or spacing is not.
func (e *DisruptionsEvaluator) Evaluate(ctx context.Context, principalID string, watch Watch, occurrence Occurrence) (Observation, error) {
	var current []ns.Disruption
	seen := make(map[string]bool)
	// What is in effect now comes first, so an entry is described by its current timespan.
	for _, moment := range []time.Time{{}, occurrence.End} {
		for _, station := range watch.Target.Stations {
			key := strings.Join([]string{KindDisruptions, principalID, station, moment.Format(time.RFC3339)}, "\x00")
			entries, err := Cached(ctx, occurrence.Cache, key, func(ctx context.Context) ([]ns.Disruption, error) {
				return e.trains.Disruptions(ctx, principalID, station, moment)
			})
			if err != nil {
				return Observation{}, connectorError(serviceNS, err)
			}
			for _, entry := range entries {
				if !seen[entry.ID] {
					seen[entry.ID] = true
					current = append(current, entry)
				}
			}
		}
	}
	return disruptionObservation(watch.Spec, current, previousFingerprints(occurrence.Previous), occurrence.End.Location()), nil
}

func disruptionObservation(spec Spec, current []ns.Disruption, previous map[string]string, location *time.Location) Observation {
	if len(current) == 0 {
		return Observation{State: noDisruptions, Headline: "NS reports no disruptions or engineering works at " + strings.Join(spec.Target.Stations, ", ") + "."}
	}
	watched := func(trigger string) bool { return slices.Contains(spec.Triggers, trigger) }
	states := make([]string, 0, len(current))
	var told, others []string
	for _, entry := range current {
		mark := fingerprint(entry)
		states = append(states, entry.ID+"="+mark)
		known, found := previous[entry.ID]
		switch {
		case !found && watched(TriggerNew):
			told = append(told, describeDisruption("New", entry, location)...)
		case found && known != mark && watched(TriggerChanged):
			told = append(told, describeDisruption("Changed", entry, location)...)
		default:
			others = append(others, entry.Title)
		}
	}
	slices.Sort(states)
	observation := Observation{State: strings.Join(states, ","), Triggered: true}
	if len(told) == 0 {
		observation.Silent = true
		return observation
	}
	observation.Headline, observation.Details = told[0], told[1:]
	if len(others) > 0 {
		observation.Details = append(observation.Details, "Still in effect: "+strings.Join(others, "; "))
	}
	return observation
}

// describeDisruption is one entry as alert lines: what it is, then NS's situation, advice, and
// expected end.
func describeDisruption(change string, entry ns.Disruption, location *time.Location) []string {
	what := "disruption"
	if entry.Kind == "maintenance" {
		what = "engineering work"
	}
	lines := []string{fmt.Sprintf("%s %s: %s", change, what, entry.Title)}
	if entry.Situation != "" && entry.Situation != entry.Title {
		lines = append(lines, entry.Situation)
	}
	if entry.Advice != "" {
		lines = append(lines, "Advice: "+entry.Advice)
	}
	switch {
	case entry.ExpectedEnd != "":
		lines = append(lines, "Expected end: "+entry.ExpectedEnd)
	case !entry.End.IsZero():
		lines = append(lines, "Until "+entry.End.In(location).Format("Mon 2 Jan 15:04"))
	}
	return lines
}

// fingerprint covers what an alert tells about an entry, compared without case or spacing.
func fingerprint(entry ns.Disruption) string {
	var end string
	if !entry.End.IsZero() {
		end = entry.End.UTC().Format(time.RFC3339)
	}
	fields := []string{entry.Kind, entry.Title, entry.Situation, entry.Advice, entry.ExpectedEnd, end}
	for index, field := range fields {
		fields[index] = strings.ToLower(strings.Join(strings.Fields(field), " "))
	}
	digest := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return hex.EncodeToString(digest[:6])
}

// previousFingerprints reads the entries of the last State.
func previousFingerprints(state string) map[string]string {
	known := make(map[string]string)
	if state == "" || state == noDisruptions {
		return known
	}
	for _, item := range strings.Split(state, ",") {
		if id, mark, found := strings.Cut(item, "="); found {
			known[id] = mark
		}
	}
	return known
}

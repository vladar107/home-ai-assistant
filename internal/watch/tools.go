package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolList    = "watch_list"
	toolPropose = "watch_propose"
	toolUpdate  = "watch_update"
	toolRemove  = "watch_remove"
)

const listGuidance = "Watches: watch_list shows the user's watches with their ids. A watch checks a train or bus departure, a saved route, NS disruptions at some stations, or the state of a device or sensor at home during a daily window, in code and without a model, and messages the user only when something changes. The user manages watches with /jobs as well, and each alert has Mute today and Pause buttons."

func proposeGuidance(zone string, kinds []string) string {
	transit := slices.ContainsFunc(kinds, func(kind string) bool { return kind != KindHome })
	guidance := "Suggest a watch instead of a scheduled report when the user wants to hear only when something goes wrong, such as tell me if my 07:42 is cancelled or more than 5 minutes late, or warn me about disruptions before I leave: a watch stays silent while all is fine, costs no model turn, and alerts within minutes, while a scheduled report runs the model at a fixed time and always sends a message. A report remains right for a summary the user wants every time. " +
		"The kinds available now are: " + strings.Join(kinds, ", ") + ". " +
		"A watch runs on days, such as mon to fri, or once on a date (YYYY-MM-DD) and then ends; times are local HH:MM in " + zone + " unless the user names another time zone. " +
		"Set language to the language the user writes to you in, such as Russian, so the watch's messages are written in it; leave it out for English, and pass English to switch a watch back. " +
		"watch_propose only proposes: after your reply the user sees the watch with Confirm and Reject buttons, so never claim it is set. watch_update proposes changing a watch by its id from watch_list, given only the fields that change; watch_remove proposes deleting one. Each user may have " + fmt.Sprint(MaximumWatchesPerPrincipal) + " watches."
	if transit {
		guidance += " A departure watch is one train (mode train, from, to, optional via) or bus (mode bus, stop, line) with its planned departure time; checking starts minutes_before the departure, 90 by default, and ends at it. Its triggers are cancelled, partly_cancelled, delay (delay_minutes or more, 5 by default), and platform. " +
			"A route watch names a saved route and the time the user leaves; its triggers are leg and arrival. A disruptions watch takes one or more stations and a fixed window, window_from to window_until; its triggers are new and changed. Omitting triggers means all of the kind's triggers. " +
			"Never invent a departure time, station, stop, or line: ask the user for one they did not give. Transit watches are checked every 5 minutes in the window and every 2 in its last 30, so tell the user it may take that long to notice a change."
	}
	if slices.Contains(kinds, KindHome) {
		guidance += " A home watch tells the user when a device or sensor at home enters a condition, such as the washing machine being done, the balcony door being open for more than 10 minutes, or the freezer going above -15. " +
			"When the user asks to be told when something at home starts, finishes, opens, or changes, propose a home watch at once rather than reading a schedule or the current state. " +
			"Prefer the device's own entity, such as the vacuum itself with Home Assistant's standard states like cleaning, docked, or returning, over a vendor status sensor. " +
			"Give the device's name as the user says it in name (or its entity id in entity) and a condition with exactly one of state (the states that count, such as on, off, or idle; a door, window, motion, or leak sensor reports on when open, moving, or wet, and open or detected are mapped to on), above, or below (a number), and optionally for_minutes (1 to 240) for how long it must hold first. " +
			"Its triggers are enter, when the condition starts to hold, and clear, when it ends after an alert; a numeric condition uses both by default and a state condition only enter, so ask for clear only when the user wants to hear it ends. " +
			"A home watch runs every day unless the user names days or one date, and checks all day by default, every minute, so a message can come up to a minute late; give window_from and window_until, which may be 24:00 for the end of the day, only to keep it quiet outside certain hours. " +
			"Do not ask for a departure for it, and never guess a device: if the name is unknown or matches several devices, tell the user what the tool reports."
	}
	return guidance
}

var listDefinition = tools.Definition{
	Name:        toolList,
	Description: "List the user's watches with their ids, targets, triggers, days, and windows.",
	Risk:        tools.RiskRead,
	InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
}

const specProperties = `"mode":{"type":"string","enum":["train","bus"],"description":"For a departure watch: train or bus."},` +
	`"from":{"type":"string","description":"Train: the departure station."},` +
	`"to":{"type":"string","description":"Train: the destination station."},` +
	`"via":{"type":"string","description":"Train: an optional station the train passes."},` +
	`"stop":{"type":"string","description":"Bus: the stop the user boards at."},` +
	`"line":{"type":"string","description":"Bus: the line number."},` +
	`"stations":{"type":"array","items":{"type":"string"},"maxItems":10,"description":"Disruptions: the stations to watch, such as a pair."},` +
	`"route":{"type":"string","description":"Route: the saved route's name."},` +
	`"name":{"type":"string","description":"Home: the name of the device or sensor as the user says it, such as Balcony door."},` +
	`"entity":{"type":"string","description":"Home: the entity id, only when the user or a tool result gave it."},` +
	`"condition":{"type":"object","description":"Home: what to wait for. Give exactly one of state, above, or below.","properties":{` +
	`"state":{"type":"array","items":{"type":"string"},"maxItems":10,"description":"The states that count, such as on, off, or idle; open and detected mean on for a binary sensor."},` +
	`"above":{"type":"number","description":"A numeric sensor going above this value."},` +
	`"below":{"type":"number","description":"A numeric sensor going below this value."},` +
	`"for_minutes":{"type":"integer","minimum":1,"maximum":240,"description":"How long the condition must hold before it counts."}},"additionalProperties":false},` +
	`"departure":{"type":"string","description":"Departure or route: the planned departure, local HH:MM."},` +
	`"minutes_before":{"type":"integer","minimum":10,"maximum":240,"description":"Departure or route: how long before the departure checking starts; 90 by default."},` +
	`"window_from":{"type":"string","description":"Disruptions or home: when checking starts, local HH:MM; a home watch defaults to 00:00."},` +
	`"window_until":{"type":"string","description":"Disruptions or home: when checking ends, local HH:MM; a home watch may use 24:00 for the end of the day and defaults to it."},` +
	`"days":{"type":"array","items":{"type":"string","enum":["mon","tue","wed","thu","fri","sat","sun"]},"description":"The weekdays the watch runs on."},` +
	`"date":{"type":"string","description":"Instead of days: the one date, YYYY-MM-DD, of a one-off watch."},` +
	`"triggers":{"type":"array","items":{"type":"string","enum":["cancelled","partly_cancelled","delay","platform","leg","arrival","new","changed","enter","clear"]},"description":"What to alert on; omit for all of the kind's triggers."},` +
	`"delay_minutes":{"type":"integer","minimum":1,"maximum":120,"description":"The delay that counts, in minutes; 5 by default."},` +
	`"timezone":{"type":"string","description":"IANA time zone such as Europe/Amsterdam; omit to use the user's own."},` +
	`"language":{"type":"string","description":"The language the watch's messages are written in, such as Russian: the one the user writes to you in. Omit for English."}`

var writeDefinitions = []tools.Definition{
	{
		Name:        toolPropose,
		Description: "Propose a watch that checks a departure, a saved route, NS disruptions, or a device or sensor at home during a daily window and messages the user only when something changes. The user must confirm.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"kind":{"type":"string","enum":["departure","disruptions","route","home"],"description":"What to watch."},` +
			specProperties + `},"required":["kind"],"additionalProperties":false}`),
	},
	{
		Name:        toolUpdate,
		Description: "Propose changing one of the user's watches; omitted fields keep their current values. The user must confirm.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"watch_id":{"type":"string","description":"The id from watch_list."},` +
			specProperties + `},"required":["watch_id"],"additionalProperties":false}`),
	},
	{
		Name:        toolRemove,
		Description: "Propose deleting one of the user's watches. The user must confirm.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"watch_id":{"type":"string","description":"The id from watch_list."}},` +
			`"required":["watch_id"],"additionalProperties":false}`),
	},
}

var _ tools.Provider = (*Service)(nil)
var _ approval.Executor = (*Service)(nil)

// Contribute offers nothing until an evaluator is registered, so watches are never promised before
// they can be checked; the write tools need the approval service as well.
func (s *Service) Contribute(context.Context, string) (tools.Contribution, error) {
	if !s.Available() {
		return tools.Contribution{}, nil
	}
	contribution := tools.Contribution{Guidance: listGuidance, Definitions: []tools.Definition{listDefinition}}
	if s.proposer != nil {
		contribution.WriteGuidance = proposeGuidance(s.zone, s.kinds())
		contribution.Definitions = append(contribution.Definitions, writeDefinitions...)
	}
	return contribution, nil
}

func (s *Service) kinds() []string {
	var kinds []string
	for _, kind := range []string{KindDeparture, KindRoute, KindDisruptions, KindHome} {
		if _, known := s.evaluators[kind]; known {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

func (s *Service) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case toolList:
		watches, err := s.repository.ListWatches(ctx, principalID)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Text: s.renderWatches(watches)}, nil
	case toolPropose:
		return s.proposeWatch(ctx, principalID, call)
	case toolUpdate:
		return s.proposeUpdate(ctx, principalID, call)
	case toolRemove:
		return s.proposeRemoval(ctx, principalID, call)
	default:
		return tools.Result{Text: "unknown watch tool", IsError: true}, nil
	}
}

// specArguments are the fields of a watch as the model gives them.
type specArguments struct {
	Mode          string     `json:"mode"`
	From          string     `json:"from"`
	To            string     `json:"to"`
	Via           string     `json:"via"`
	Stop          string     `json:"stop"`
	Line          string     `json:"line"`
	Stations      []string   `json:"stations"`
	Route         string     `json:"route"`
	Name          string     `json:"name"`
	Entity        string     `json:"entity"`
	Condition     *Condition `json:"condition"`
	Departure     string     `json:"departure"`
	MinutesBefore int        `json:"minutes_before"`
	WindowFrom    string     `json:"window_from"`
	WindowUntil   string     `json:"window_until"`
	Days          []string   `json:"days"`
	Date          string     `json:"date"`
	Triggers      []string   `json:"triggers"`
	DelayMinutes  int        `json:"delay_minutes"`
	Timezone      string     `json:"timezone"`
	Language      string     `json:"language"`
}

type proposeArguments struct {
	Kind string `json:"kind"`
	specArguments
}

type updateArguments struct {
	WatchID string `json:"watch_id"`
	specArguments
}

type removeArguments struct {
	WatchID string `json:"watch_id"`
}

// confirmedUpdate is the watch as it will be, with its last change as proposed, so a watch changed
// before the press is not overwritten.
type confirmedUpdate struct {
	WatchID   string    `json:"watch_id"`
	UpdatedAt time.Time `json:"updated_at"`
	Spec      Spec      `json:"spec"`
}

// prepare turns a kind and the model's arguments into a normalized spec, refusing a kind that
// cannot be checked yet and a target its evaluator cannot check for the principal.
func (s *Service) prepare(ctx context.Context, principalID, kind string, arguments specArguments) (Spec, error) {
	spec, err := s.specOf(kind, arguments)
	if err != nil {
		return Spec{}, err
	}
	if resolver, ok := s.evaluators[kind].(Resolver); ok {
		resolved, err := resolver.Resolve(ctx, principalID, spec)
		if err != nil {
			return Spec{}, err
		}
		if spec, err = normalize(resolved, s.now()); err != nil {
			return Spec{}, err
		}
	}
	if kind == KindHome && spec.Target.Entity == "" {
		return Spec{}, fmt.Errorf("the device could not be matched to one entity")
	}
	return spec, nil
}

func (s *Service) specOf(kind string, arguments specArguments) (Spec, error) {
	if _, known := s.evaluators[kind]; !known {
		if _, exists := kindTriggers[kind]; exists {
			return Spec{}, fmt.Errorf("%s watches are not available yet", kind)
		}
		return Spec{}, fmt.Errorf("the kind must be one of: %s", strings.Join(s.kinds(), ", "))
	}
	spec := Spec{
		Kind: kind,
		Target: Target{
			Mode: arguments.Mode, From: arguments.From, To: arguments.To, Via: arguments.Via,
			Stop: arguments.Stop, Line: arguments.Line, Stations: arguments.Stations, Route: arguments.Route,
			Entity: arguments.Entity, Name: arguments.Name,
		},
		Condition:    arguments.Condition,
		Triggers:     arguments.Triggers,
		DelayMinutes: arguments.DelayMinutes,
		Days:         arguments.Days,
		Date:         strings.TrimSpace(arguments.Date),
		Zone:         strings.TrimSpace(arguments.Timezone),
		Language:     arguments.Language,
	}
	if spec.Zone == "" {
		spec.Zone = s.zone
	}
	if kind == KindDisruptions || kind == KindHome {
		if arguments.Departure != "" || arguments.MinutesBefore != 0 {
			return Spec{}, fmt.Errorf("a %s watch takes window_from and window_until, not a departure", kind)
		}
		spec.Start, spec.End = strings.TrimSpace(arguments.WindowFrom), strings.TrimSpace(arguments.WindowUntil)
	} else {
		if arguments.WindowFrom != "" || arguments.WindowUntil != "" {
			return Spec{}, fmt.Errorf("a %s watch takes a departure and minutes_before, not a fixed window", kind)
		}
		departure, err := time.Parse(clockLayout, strings.TrimSpace(arguments.Departure))
		if err != nil {
			return Spec{}, fmt.Errorf("departure must be a local time, HH:MM, such as 07:42")
		}
		lead := DefaultLead
		if arguments.MinutesBefore != 0 {
			lead = time.Duration(arguments.MinutesBefore) * time.Minute
		}
		start := departure.Add(-lead)
		if start.Day() != departure.Day() {
			return Spec{}, fmt.Errorf("checking must start on the same day as the departure")
		}
		spec.Start, spec.End = start.Format(clockLayout), departure.Format(clockLayout)
	}
	return normalize(spec, s.now())
}

// argumentsOf is a stored spec in the form the model gives, for applying a change to it.
func argumentsOf(spec Spec) specArguments {
	arguments := specArguments{
		Mode: spec.Target.Mode, From: spec.Target.From, To: spec.Target.To, Via: spec.Target.Via,
		Stop: spec.Target.Stop, Line: spec.Target.Line, Stations: spec.Target.Stations, Route: spec.Target.Route,
		Name: spec.Target.Name, Entity: spec.Target.Entity, Condition: spec.Condition,
		Days: spec.Days, Date: spec.Date, Triggers: spec.Triggers, DelayMinutes: spec.DelayMinutes, Timezone: spec.Zone,
		Language: spec.Language,
	}
	if spec.Kind == KindDisruptions || spec.Kind == KindHome {
		arguments.WindowFrom, arguments.WindowUntil = spec.Start, spec.End
		return arguments
	}
	start, startErr := time.Parse(clockLayout, spec.Start)
	end, endErr := time.Parse(clockLayout, spec.End)
	if startErr == nil && endErr == nil {
		arguments.Departure, arguments.MinutesBefore = spec.End, int(end.Sub(start).Minutes())
	}
	return arguments
}

// overlay applies the fields the model gave to the current ones. Days and a date replace each
// other, a new mode drops the other mode's target fields, and a new device name or entity replaces
// both of the device's.
func overlay(current, change specArguments) specArguments {
	next := current
	if mode := strings.TrimSpace(change.Mode); mode != "" && !strings.EqualFold(mode, current.Mode) {
		next.Mode, next.From, next.To, next.Via, next.Stop, next.Line = mode, "", "", "", "", ""
	}
	set := func(field *string, value string) {
		if strings.TrimSpace(value) != "" {
			*field = value
		}
	}
	set(&next.From, change.From)
	set(&next.To, change.To)
	set(&next.Via, change.Via)
	set(&next.Stop, change.Stop)
	set(&next.Line, change.Line)
	set(&next.Route, change.Route)
	if strings.TrimSpace(change.Name) != "" || strings.TrimSpace(change.Entity) != "" {
		next.Name, next.Entity = change.Name, change.Entity
	}
	if change.Condition != nil {
		numeric := func(c *Condition) bool { return c != nil && (c.Above != nil || c.Below != nil) }
		if len(change.Triggers) == 0 && numeric(current.Condition) != numeric(change.Condition) {
			next.Triggers = nil
		}
		next.Condition = change.Condition
	}
	set(&next.Departure, change.Departure)
	set(&next.WindowFrom, change.WindowFrom)
	set(&next.WindowUntil, change.WindowUntil)
	set(&next.Timezone, change.Timezone)
	set(&next.Language, change.Language)
	if len(change.Stations) > 0 {
		next.Stations = change.Stations
	}
	if len(change.Triggers) > 0 {
		next.Triggers = change.Triggers
	}
	if change.MinutesBefore != 0 {
		next.MinutesBefore = change.MinutesBefore
	}
	if change.DelayMinutes != 0 {
		next.DelayMinutes = change.DelayMinutes
	}
	switch {
	case len(change.Days) > 0:
		next.Days, next.Date = change.Days, ""
	case strings.TrimSpace(change.Date) != "":
		next.Days, next.Date = nil, change.Date
	}
	return next
}

func (s *Service) proposeWatch(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "Watches cannot be set up here.", IsError: true}, nil
	}
	var arguments proposeArguments
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	spec, err := s.prepare(ctx, principalID, strings.TrimSpace(arguments.Kind), arguments.specArguments)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	existing, err := s.repository.ListWatches(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if len(existing) >= MaximumWatchesPerPrincipal {
		return tools.Result{Text: tooManyText(), IsError: true}, nil
	}
	canonical, err := json.Marshal(spec)
	if err != nil {
		return tools.Result{}, err
	}
	summary := "Watch " + spec.Label() + "\n" + spec.Describe() + "\nYou get a message only when something changes, and once more when it is back to normal."
	if spec.Kind == KindHome {
		summary = "Watch " + spec.Label() + "\n" + spec.Describe() + "\nYou get a message only when this changes."
	}
	return s.propose(ctx, principalID, call, approval.Proposal{Tool: toolPropose, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "watch", TargetKind: spec.Kind + " watch", TargetName: intentName(spec)}})
}

func (s *Service) proposeUpdate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "Watches cannot be changed here.", IsError: true}, nil
	}
	var arguments updateArguments
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	watch, found, err := s.repository.LoadWatch(ctx, principalID, strings.TrimSpace(arguments.WatchID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return tools.Result{Text: "No watch has that id. Use watch_list to see the user's watches.", IsError: true}, nil
	}
	spec, err := s.prepare(ctx, principalID, watch.Kind, overlay(argumentsOf(watch.Spec), arguments.specArguments))
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if sameSpec(spec, watch.Spec) {
		return tools.Result{Text: "That changes nothing; the watch already reads exactly like this.", IsError: true}, nil
	}
	canonical, err := json.Marshal(confirmedUpdate{WatchID: watch.ID, UpdatedAt: watch.UpdatedAt, Spec: spec})
	if err != nil {
		return tools.Result{}, err
	}
	summary := "Change the watch " + watch.Label() + " to:\n" + spec.Label() + "\n" + spec.Describe()
	if !watch.Enabled {
		summary += "\nIt stays paused until you resume it in /jobs."
	}
	return s.propose(ctx, principalID, call, approval.Proposal{Tool: toolUpdate, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "change", TargetKind: watch.Kind + " watch", TargetName: intentName(watch.Spec), NewName: intentName(spec)}})
}

func (s *Service) proposeRemoval(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "Watches cannot be removed here.", IsError: true}, nil
	}
	var arguments removeArguments
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	watch, found, err := s.repository.LoadWatch(ctx, principalID, strings.TrimSpace(arguments.WatchID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return tools.Result{Text: "No watch has that id. Use watch_list to see the user's watches.", IsError: true}, nil
	}
	canonical, err := json.Marshal(removeArguments{WatchID: watch.ID})
	if err != nil {
		return tools.Result{}, err
	}
	summary := "Stop watching " + watch.Label() + " (" + watch.DescribeDays() + ", " + watch.DescribeWindow() + ")."
	return s.propose(ctx, principalID, call, approval.Proposal{Tool: toolRemove, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "remove", TargetKind: watch.Kind + " watch", TargetName: watch.Label()}})
}

func (s *Service) propose(ctx context.Context, principalID string, call tools.Call, proposal approval.Proposal) (tools.Result, error) {
	if _, err := s.proposer.Propose(ctx, principalID, proposal); err != nil {
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		s.logger.WarnContext(ctx, "watch.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return tools.Result{Text: "the proposal could not be recorded", IsError: true}, nil
	}
	return tools.Result{Text: approval.ProposedText}, nil
}

func (s *Service) ActionTools() []string {
	return []string{toolPropose, toolUpdate, toolRemove}
}

// ExecuteApproved stores, changes, or deletes the confirmed watch. The spec is validated again,
// because a one-off window can pass and the principal's count can change before the press, and
// the approval's own ID becomes the watch ID so a second execution cannot create a second watch.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	switch record.Tool {
	case toolPropose:
		var confirmed Spec
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed watch could not be read; nothing was set up."}
		}
		spec, err := normalize(confirmed, s.now())
		if err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That watch is no longer valid (" + err.Error() + "); nothing was set up."}
		}
		now := s.now()
		watch := Watch{ID: record.ID, PrincipalID: record.PrincipalID, Spec: spec, Enabled: true, CreatedAt: now, UpdatedAt: now}
		created, err := s.repository.CreateWatch(ctx, watch, MaximumWatchesPerPrincipal)
		if err != nil {
			if errors.Is(err, ErrTooManyWatches) {
				return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: tooManyText() + " Nothing was set up."}
			}
			return approval.Outcome{}, err
		}
		if created {
			s.logger.InfoContext(ctx, "watch.created", "principal_id", record.PrincipalID, "watch_id", record.ID, "kind", spec.Kind)
		}
		return approval.Outcome{Text: "Watching. " + s.nextWindow(spec)}, nil
	case toolUpdate:
		return s.executeUpdate(ctx, record)
	case toolRemove:
		var confirmed removeArguments
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed action could not be read; nothing was deleted."}
		}
		deleted, err := s.Delete(ctx, record.PrincipalID, confirmed.WatchID)
		if err != nil {
			return approval.Outcome{}, err
		}
		if !deleted {
			return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That watch no longer exists."}
		}
		return approval.Outcome{Text: "Deleted."}, nil
	default:
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That action is no longer available."}
	}
}

func (s *Service) executeUpdate(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedUpdate
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed change could not be read; nothing was changed."}
	}
	current, found, err := s.repository.LoadWatch(ctx, record.PrincipalID, confirmed.WatchID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found || current.Kind != confirmed.Spec.Kind {
		return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That watch no longer exists; nothing was changed."}
	}
	stale := &approval.ActionError{Reason: "stale", Text: "That watch changed after this was proposed; nothing was changed. Ask again to change it."}
	if !current.UpdatedAt.Equal(confirmed.UpdatedAt) {
		return approval.Outcome{}, stale
	}
	spec, err := normalize(confirmed.Spec, s.now())
	if err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That watch is no longer valid (" + err.Error() + "); nothing was changed."}
	}
	next := current
	next.Spec, next.UpdatedAt = spec, s.now()
	updated, err := s.repository.UpdateWatch(ctx, next, confirmed.UpdatedAt)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, stale
	}
	s.logger.InfoContext(ctx, "watch.updated", "principal_id", record.PrincipalID, "watch_id", current.ID, "kind", spec.Kind)
	if !current.Enabled {
		return approval.Outcome{Text: "Changed. It stays paused until you resume it in /jobs."}, nil
	}
	return approval.Outcome{Text: "Changed. " + s.nextWindow(spec)}, nil
}

// nextWindow says when the watch next checks, within the coming week.
func (s *Service) nextWindow(spec Spec) string {
	now := s.now()
	for days := 0; days < 8; days++ {
		occurrence, active := spec.OccurrenceAt(now.AddDate(0, 0, days))
		if active && occurrence.End.After(now) {
			start := occurrence.Start
			if start.Before(now) {
				if spec.End == endOfDay {
					return "Checking now until midnight."
				}
				return "Checking now until " + occurrence.End.Format("15:04") + "."
			}
			return "First check: " + start.Format("Mon 2 Jan 15:04 MST") + "."
		}
	}
	return ""
}

// intentName is the watch as the intent check reads it: its label and, when it has one, its
// language, so a request to change only the language still reads as requested.
func intentName(spec Spec) string {
	if spec.Language == "" {
		return spec.Label()
	}
	return spec.Label() + ", messages in " + spec.Language
}

func sameSpec(a, b Spec) bool {
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(encodedA, encodedB)
}

func tooManyText() string {
	return fmt.Sprintf("You already have %d watches, which is the most allowed; delete one in /jobs first.", MaximumWatchesPerPrincipal)
}

// renderWatches is the principal's watches as the model reads them.
func (s *Service) renderWatches(watches []Watch) string {
	if len(watches) == 0 {
		return "The user has no watches."
	}
	now := s.now()
	rendered := make([]string, 0, len(watches))
	for _, watch := range watches {
		var states []string
		if !watch.Enabled {
			states = append(states, "paused")
		}
		if occurrence, active := watch.OccurrenceAt(now); active && watch.MutedOn == occurrence.Date {
			states = append(states, "muted today")
		}
		if _, known := s.evaluators[watch.Kind]; !known {
			states = append(states, "not checked: this kind is not available")
		}
		heading := fmt.Sprintf("id %s: %s watch, %s", watch.ID, watch.Kind, watch.Label())
		if len(states) > 0 {
			heading += " (" + strings.Join(states, ", ") + ")"
		}
		rendered = append(rendered, heading+"\n"+watch.Describe())
	}
	return strings.Join(rendered, "\n\n")
}

func decodeArguments(raw json.RawMessage, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("the arguments do not match the tool's schema")
	}
	return nil
}

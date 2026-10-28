package routes

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	toolList   = "route_list"
	toolGet    = "route_get"
	toolCheck  = "route_check"
	toolSave   = "route_save"
	toolUpdate = "route_update"
	toolRemove = "route_remove"

	legReplace = "replace"
	legInsert  = "insert"
	legRemove  = "remove"

	// checkTimeout covers several legs, each under its connector's own deadline.
	checkTimeout = 2 * time.Minute
	dateLayout   = "2006-01-02"
	// actTimeout lets a change made at once run as long as one made after Confirm.
	actTimeout = 45 * time.Second
)

const guidance = "Routes: the user's saved journeys, such as work, each with the weekdays it applies to, an optional arrival target, and legs: walk or bike minutes the user stated; a train with from, to, optional via, and fixed planned departures; a bus with stop, line, optional towards, fixed departures, and optional ride minutes. " +
	"route_list lists them and route_get shows one with numbered legs. " +
	"route_check checks a route for a day in one call: each train and bus leg's fixed departures, timed in order through the walk and bike legs with no buffer added, then whether the arrival target is still met and which legs failed to check; without a name it checks the route whose weekdays include that day. " +
	"For any question or report about a saved route, such as report my work route, call route_check once and report its legs in order, copying each status phrase word for word; never plan a leg it checked again with other tools, and say which legs failed to check instead of guessing. " +
	"When no saved train of a leg can be taken, route_check continues on an alternative the train service plans and still lists the saved departures with why each failed: report both, with the alternative's times. A saved departure that is not in the timetable comes with the nearest real one; when it never runs, offer to replace it in the route. " +
	"A scheduled report about a commute should only name the route to check, such as \"check my work route with route_check and report it\", or \"check today's route with route_check\" for one weekday job that serves routes differing by day, instead of spelling out legs and times in the prompt."

func writeGuidance() string {
	return "route_save saves a new route; route_update changes one, either named fields or one leg by its number from route_get, with leg_action replace (fields you leave out keep their values), insert, or remove; route_remove deletes one. " +
		"When the user asked for it in this conversation and the conversation has read no outside content, each is done at once and the user can undo it; tell the user it is done. After outside content, such as a web page or an email, it is only proposed with Confirm and Reject buttons, so never claim it is saved. " +
		"Never invent a leg value: ask the user for a walking or cycling time, a departure, a ride time, or a buffer they did not state, instead of choosing one. Stations and stops are looked up when a leg is proposed; when the lookup offers candidates, ask which one the user means. " +
		"Keep route details, such as legs, departures, and transfer times, in routes and not in memory; a memory entry may name a route instead. " +
		"When the user changes one of several per-weekday copies of a commute report, offer to save the route, or one route per variant with its weekdays, and to replace the copies with one job on those weekdays whose prompt checks the day's route. " +
		fmt.Sprintf("Each user may have %d routes.", MaximumRoutesPerPrincipal)
}

const legSchema = `{"type":"object","properties":{` +
	`"mode":{"type":"string","enum":["walk","bike","train","bus"]},` +
	`"minutes":{"type":"integer","minimum":1,"maximum":240,"description":"Walk or bike: the minutes the user stated. Bus: the ride time the user stated, optional."},` +
	`"from":{"type":"string","description":"Train: the departure station."},` +
	`"to":{"type":"string","description":"Train: the arrival station."},` +
	`"via":{"type":"string","description":"Train: an optional station to travel through; an empty string removes it."},` +
	`"stop":{"type":"string","description":"Bus: the stop the user boards at."},` +
	`"line":{"type":"string","description":"Bus: the line's public number."},` +
	`"towards":{"type":"string","description":"Bus: part of the line's destination, for a stop served both ways; an empty string removes it."},` +
	`"departures":{"type":"array","maxItems":6,"items":{"type":"string","pattern":"^[0-9]{2}:[0-9]{2}$"},"description":"Train or bus: the fixed planned departures the user takes, local HH:MM."}},` +
	`"additionalProperties":false}`

const daysSchema = `{"type":"array","minItems":1,"items":{"type":"string","enum":["mon","tue","wed","thu","fri","sat","sun"]},"description":"The weekdays the route applies to."}`

var readDefinitions = []tools.Definition{
	{
		Name:        toolList,
		Description: "List the user's saved routes with their weekdays, arrival targets, and legs.",
		Risk:        tools.RiskRead,
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
	{
		Name:        toolGet,
		Description: "Show one saved route with its legs numbered.",
		Risk:        tools.RiskRead,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"The route's name."}},` +
			`"required":["name"],"additionalProperties":false}`),
	},
	{
		Name:        toolCheck,
		Description: "Check a saved route for a day: the live status of each train and bus leg's fixed departures, timing through the walk and bike legs, whether the arrival target is met, and which legs failed to check.",
		Risk:        tools.RiskRead,
		Timeout:     checkTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"The route's name; omit for the route whose weekdays include the day."},` +
			`"date":{"type":"string","pattern":"^[0-9]{4}-[0-9]{2}-[0-9]{2}$","description":"The day, YYYY-MM-DD; default today."}},` +
			`"additionalProperties":false}`),
	},
}

var writeDefinitions = []tools.Definition{
	{
		Name:        toolSave,
		Description: "Save a new route with its weekdays, optional arrival target, and legs. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"A short name, such as work."},` +
			`"days":` + daysSchema + `,` +
			`"arrive_by":{"type":"string","pattern":"^[0-9]{2}:[0-9]{2}$","description":"The arrival target the user stated, local HH:MM; omit when they gave none."},` +
			`"legs":{"type":"array","minItems":1,"maxItems":8,"items":` + legSchema + `,"description":"The legs in travel order."}},` +
			`"required":["name","days","legs"],"additionalProperties":false}`),
	},
	{
		Name:        toolUpdate,
		Description: "Change a saved route: its name, weekdays, or arrival target, or one leg by its number. Omitted fields keep their values. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"The route's current name."},` +
			`"new_name":{"type":"string","description":"A new name."},` +
			`"days":` + daysSchema + `,` +
			`"arrive_by":{"type":"string","pattern":"^([0-9]{2}:[0-9]{2})?$","description":"A new arrival target, local HH:MM; an empty string removes it."},` +
			`"leg_index":{"type":"integer","minimum":1,"maximum":9,"description":"The number of the leg to change, from route_get; for insert, the position the new leg takes."},` +
			`"leg_action":{"type":"string","enum":["replace","insert","remove"],"description":"What to do at leg_index; replace by default."},` +
			`"leg":` + legSchema + `},` +
			`"required":["name"],"additionalProperties":false}`),
	},
	{
		Name:        toolRemove,
		Description: "Delete a saved route. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"The route's name."}},` +
			`"required":["name"],"additionalProperties":false}`),
	},
}

var _ tools.Provider = (*Service)(nil)
var _ approval.Executor = (*Service)(nil)
var _ approval.Undoer = (*Service)(nil)

// Contribute offers routes only where a train or bus leg can be checked; the write tools need the
// approval service as well.
func (s *Service) Contribute(context.Context, string) (tools.Contribution, error) {
	if s.trains == nil && s.buses == nil {
		return tools.Contribution{}, nil
	}
	contribution := tools.Contribution{Guidance: guidance, Definitions: slices.Clone(readDefinitions)}
	if s.proposer != nil {
		contribution.WriteGuidance = writeGuidance()
		contribution.Definitions = append(contribution.Definitions, writeDefinitions...)
	}
	return contribution, nil
}

func (s *Service) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case toolList:
		routes, err := s.repository.ListRoutes(ctx, principalID)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Text: renderRoutes(routes)}, nil
	case toolGet:
		var arguments nameArguments
		if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
			return errorResult(err.Error()), nil
		}
		route, refusal, err := s.named(ctx, principalID, arguments.Name)
		if err != nil {
			return tools.Result{}, err
		}
		if refusal != "" {
			return errorResult(refusal), nil
		}
		return tools.Result{Text: route.Describe()}, nil
	case toolCheck:
		return s.check(ctx, principalID, call)
	case toolSave:
		return s.proposeSave(ctx, principalID, call)
	case toolUpdate:
		return s.proposeUpdate(ctx, principalID, call)
	case toolRemove:
		return s.proposeRemoval(ctx, principalID, call)
	default:
		return errorResult("unknown route tool"), nil
	}
}

type nameArguments struct {
	Name string `json:"name"`
}

type checkArguments struct {
	Name string `json:"name"`
	Date string `json:"date"`
}

// legArguments are a leg as the model gives it. Pointers tell a field left out, which keeps its
// value in an update, from one set to empty.
type legArguments struct {
	Mode       *string  `json:"mode"`
	Minutes    *int     `json:"minutes"`
	From       *string  `json:"from"`
	To         *string  `json:"to"`
	Via        *string  `json:"via"`
	Stop       *string  `json:"stop"`
	Line       *string  `json:"line"`
	Towards    *string  `json:"towards"`
	Departures []string `json:"departures"`
}

// over applies the given fields to current; a different mode starts the leg afresh.
func (a legArguments) over(current Leg) Leg {
	next := current
	next.Departures = slices.Clone(current.Departures)
	if a.Mode != nil && !strings.EqualFold(strings.TrimSpace(*a.Mode), current.Mode) {
		next = Leg{Mode: *a.Mode}
	}
	set := func(field *string, value *string) {
		if value != nil {
			*field = *value
		}
	}
	set(&next.From, a.From)
	set(&next.To, a.To)
	set(&next.Via, a.Via)
	set(&next.Stop, a.Stop)
	set(&next.Line, a.Line)
	set(&next.Towards, a.Towards)
	if a.Minutes != nil {
		next.Minutes = *a.Minutes
	}
	if a.Departures != nil {
		next.Departures = a.Departures
	}
	return next
}

type saveArguments struct {
	Name     string         `json:"name"`
	Days     []string       `json:"days"`
	ArriveBy string         `json:"arrive_by"`
	Legs     []legArguments `json:"legs"`
}

type updateArguments struct {
	Name      string        `json:"name"`
	NewName   *string       `json:"new_name"`
	Days      []string      `json:"days"`
	ArriveBy  *string       `json:"arrive_by"`
	LegIndex  int           `json:"leg_index"`
	LegAction string        `json:"leg_action"`
	Leg       *legArguments `json:"leg"`
}

// confirmedUpdate is the route as it will be, with its last change as proposed, so a route changed
// before the press is not overwritten.
type confirmedUpdate struct {
	RouteID   string    `json:"route_id"`
	UpdatedAt time.Time `json:"updated_at"`
	Spec      Spec      `json:"spec"`
}

type confirmedRemoval struct {
	RouteID string `json:"route_id"`
}

func (s *Service) check(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments checkArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	var date time.Time
	if value := strings.TrimSpace(arguments.Date); value != "" {
		parsed, err := time.ParseInLocation(dateLayout, value, s.location)
		if err != nil {
			return errorResult("date must be YYYY-MM-DD"), nil
		}
		date = parsed
	}
	result, err := s.Check(ctx, principalID, arguments.Name, date)
	var selection *SelectionError
	if errors.As(err, &selection) {
		return errorResult(selection.Text), nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	s.logger.InfoContext(ctx, "route.checked", "principal_id", principalID, "route_id", result.Route.ID, "failed_legs", len(result.FailedLegs))
	return tools.Result{Text: s.Render(result)}, nil
}

// named finds the principal's route by name; a refusal is text for the model.
func (s *Service) named(ctx context.Context, principalID, name string) (Route, string, error) {
	routes, err := s.repository.ListRoutes(ctx, principalID)
	if err != nil {
		return Route{}, "", err
	}
	if route, found := byName(routes, name); found {
		return route, "", nil
	}
	if len(routes) == 0 {
		return Route{}, "The user has no saved routes.", nil
	}
	return Route{}, fmt.Sprintf("No route is called %q. The user's routes: %s.", strings.TrimSpace(name), listNames(routes)), nil
}

func (s *Service) proposeSave(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Routes cannot be saved here."), nil
	}
	var arguments saveArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	spec := Spec{Name: arguments.Name, Days: arguments.Days, ArriveBy: arguments.ArriveBy}
	for _, leg := range arguments.Legs {
		spec.Legs = append(spec.Legs, leg.over(Leg{}))
	}
	spec, err := normalize(spec)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	routes, err := s.repository.ListRoutes(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	if _, taken := byName(routes, spec.Name); taken {
		return errorResult(fmt.Sprintf("A route called %q already exists; change it with route_update or choose another name.", spec.Name)), nil
	}
	if len(routes) >= MaximumRoutesPerPrincipal {
		return errorResult(tooManyText()), nil
	}
	for index := range spec.Legs {
		resolved, refusal, err := s.resolveLeg(ctx, principalID, spec.Legs[index])
		if err != nil {
			return tools.Result{}, err
		}
		if refusal != "" {
			return errorResult(fmt.Sprintf("leg %d: %s", index+1, refusal)), nil
		}
		spec.Legs[index] = resolved
	}
	canonical, err := json.Marshal(spec)
	if err != nil {
		return tools.Result{}, err
	}
	summary := "Save the route " + spec.Describe()
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolSave, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "save", TargetKind: "route", TargetName: spec.Name}})
}

func (s *Service) proposeUpdate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Routes cannot be changed here."), nil
	}
	var arguments updateArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	route, refusal, err := s.named(ctx, principalID, arguments.Name)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	next := route.Spec
	next.Days = slices.Clone(route.Days)
	next.Legs = slices.Clone(route.Legs)
	if arguments.NewName != nil {
		next.Name = *arguments.NewName
	}
	if arguments.Days != nil {
		next.Days = arguments.Days
	}
	if arguments.ArriveBy != nil {
		next.ArriveBy = *arguments.ArriveBy
	}
	changedLeg := -1
	action := strings.TrimSpace(arguments.LegAction)
	if action == "" {
		action = legReplace
	}
	switch {
	case arguments.LegIndex == 0 && (arguments.Leg != nil || arguments.LegAction != ""):
		return errorResult("give leg_index, the leg's number from route_get"), nil
	case arguments.LegIndex == 0:
	case action == legReplace || action == legRemove:
		if arguments.LegIndex > len(next.Legs) {
			return errorResult(fmt.Sprintf("the route has %d legs", len(next.Legs))), nil
		}
		index := arguments.LegIndex - 1
		if action == legRemove {
			if arguments.Leg != nil {
				return errorResult("removing a leg takes no leg"), nil
			}
			next.Legs = slices.Delete(next.Legs, index, index+1)
			break
		}
		if arguments.Leg == nil {
			return errorResult("give leg with the fields that change"), nil
		}
		next.Legs[index] = arguments.Leg.over(next.Legs[index])
		changedLeg = index
	case action == legInsert:
		if arguments.LegIndex > len(next.Legs)+1 {
			return errorResult(fmt.Sprintf("the route has %d legs; a new leg goes at position 1 to %d", len(next.Legs), len(next.Legs)+1)), nil
		}
		if arguments.Leg == nil || arguments.Leg.Mode == nil {
			return errorResult("give the new leg with its mode"), nil
		}
		index := arguments.LegIndex - 1
		next.Legs = slices.Insert(next.Legs, index, arguments.Leg.over(Leg{}))
		changedLeg = index
	default:
		return errorResult("leg_action must be replace, insert, or remove"), nil
	}
	next, err = normalize(next)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	if !strings.EqualFold(next.Name, route.Name) {
		routes, err := s.repository.ListRoutes(ctx, principalID)
		if err != nil {
			return tools.Result{}, err
		}
		if _, taken := byName(routes, next.Name); taken {
			return errorResult(fmt.Sprintf("A route called %q already exists.", next.Name)), nil
		}
	}
	if changedLeg >= 0 {
		resolved, refusal, err := s.resolveLeg(ctx, principalID, next.Legs[changedLeg])
		if err != nil {
			return tools.Result{}, err
		}
		if refusal != "" {
			return errorResult(fmt.Sprintf("leg %d: %s", changedLeg+1, refusal)), nil
		}
		next.Legs[changedLeg] = resolved
	}
	changes := describeChanges(route.Spec, next, arguments.LegIndex-1, action)
	if len(changes) == 0 {
		return errorResult("That changes nothing; the route already reads exactly like this."), nil
	}
	canonical, err := json.Marshal(confirmedUpdate{RouteID: route.ID, UpdatedAt: route.UpdatedAt, Spec: next})
	if err != nil {
		return tools.Result{}, err
	}
	summary := "Change the route " + route.Name + ":\n" + strings.Join(changes, "\n")
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolUpdate, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "change", TargetKind: "route", TargetName: route.Name, NewName: next.Name}})
}

func (s *Service) proposeRemoval(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if s.proposer == nil {
		return errorResult("Routes cannot be removed here."), nil
	}
	var arguments nameArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	route, refusal, err := s.named(ctx, principalID, arguments.Name)
	if err != nil {
		return tools.Result{}, err
	}
	if refusal != "" {
		return errorResult(refusal), nil
	}
	canonical, err := json.Marshal(confirmedRemoval{RouteID: route.ID})
	if err != nil {
		return tools.Result{}, err
	}
	summary := "Delete the route " + route.Heading() + "\n" + route.Outline()
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolRemove, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "delete", TargetKind: "route", TargetName: route.Name}})
}

// act makes the change at once in a clean turn and proposes it otherwise.
func (s *Service) act(ctx context.Context, principalID string, call tools.Call, proposal approval.Proposal) (tools.Result, error) {
	text, err := s.proposer.Act(ctx, principalID, proposal)
	if err != nil {
		if approval.Refused(err) {
			return errorResult(err.Error()), nil
		}
		s.logger.WarnContext(ctx, "route.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return errorResult("the change could not be recorded"), nil
	}
	return tools.Result{Text: text}, nil
}

// resolveLeg looks a train leg's stations or a bus leg's stop up, so the route keeps the names the
// checks will find again. A refusal is text for the model.
func (s *Service) resolveLeg(ctx context.Context, principalID string, leg Leg) (Leg, string, error) {
	refused := func(err error) (string, error) {
		var refusal *transit.RefusalError
		if errors.As(err, &refusal) {
			return refusal.Text, nil
		}
		return "", err
	}
	switch leg.Mode {
	case ModeTrain:
		if s.trains == nil {
			return Leg{}, "Train legs cannot be saved: train information is not set up.", nil
		}
		for _, field := range []struct {
			role  string
			value *string
		}{{"from", &leg.From}, {"to", &leg.To}, {"via", &leg.Via}} {
			if *field.value == "" {
				continue
			}
			name, err := s.trains.ResolvePlace(ctx, principalID, field.role, *field.value)
			if err != nil {
				refusal, err := refused(err)
				return Leg{}, refusal, err
			}
			*field.value = name
		}
		if leg.From == leg.To {
			return Leg{}, fmt.Sprintf("from and to are both %s.", leg.From), nil
		}
	case ModeBus:
		if s.buses == nil {
			return Leg{}, "Bus legs cannot be saved: bus information is not set up.", nil
		}
		name, err := s.buses.ResolvePlace(ctx, principalID, "stop", leg.Stop)
		if err != nil {
			refusal, err := refused(err)
			return Leg{}, refusal, err
		}
		leg.Stop = name
	}
	return leg, "", nil
}

// describeChanges lists what an update changes, each as the old and the new value; legIndex and
// action name the one leg the update touches.
func describeChanges(before, after Spec, legIndex int, action string) []string {
	var changes []string
	if before.Name != after.Name {
		changes = append(changes, fmt.Sprintf("name: %s → %s", before.Name, after.Name))
	}
	if !slices.Equal(before.Days, after.Days) {
		changes = append(changes, fmt.Sprintf("days: %s → %s", before.DescribeDays(), after.DescribeDays()))
	}
	if before.ArriveBy != after.ArriveBy {
		changes = append(changes, fmt.Sprintf("arrival target: %s → %s", orNone(before.ArriveBy), orNone(after.ArriveBy)))
	}
	switch {
	case legIndex < 0:
	case action == legInsert:
		changes = append(changes, fmt.Sprintf("new leg %d: %s", legIndex+1, after.Legs[legIndex].Describe()))
	case action == legRemove:
		changes = append(changes, fmt.Sprintf("leg %d removed: %s", legIndex+1, before.Legs[legIndex].Describe()))
	case !sameLeg(before.Legs[legIndex], after.Legs[legIndex]):
		changes = append(changes, fmt.Sprintf("leg %d: %s → %s", legIndex+1, before.Legs[legIndex].Describe(), after.Legs[legIndex].Describe()))
	}
	return changes
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func sameLeg(a, b Leg) bool {
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(encodedA, encodedB)
}

func (s *Service) ActionTools() []string {
	return []string{toolSave, toolUpdate, toolRemove}
}

// ExecuteApproved stores, changes, or deletes the confirmed route. The spec is checked again, and
// the approval's own ID becomes the route ID so a second execution cannot create a second route.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	switch record.Tool {
	case toolSave:
		var confirmed Spec
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed route could not be read; nothing was saved."}
		}
		spec, err := normalize(confirmed)
		if err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That route is not valid (" + err.Error() + "); nothing was saved."}
		}
		now := s.now()
		created, err := s.repository.CreateRoute(ctx, Route{ID: record.ID, PrincipalID: record.PrincipalID, Spec: spec, CreatedAt: now, UpdatedAt: now}, MaximumRoutesPerPrincipal)
		switch {
		case errors.Is(err, ErrTooManyRoutes):
			return approval.Outcome{}, &approval.ActionError{Reason: "too_many", Text: tooManyText() + " Nothing was saved."}
		case errors.Is(err, ErrNameTaken):
			return approval.Outcome{}, &approval.ActionError{Reason: "name_taken", Text: "Another route is already called " + spec.Name + "; nothing was saved."}
		case err != nil:
			return approval.Outcome{}, err
		}
		if created {
			s.logger.InfoContext(ctx, "route.created", "principal_id", record.PrincipalID, "route_id", record.ID, "legs", len(spec.Legs))
		}
		undo, err := s.undoPayload(ctx, record.PrincipalID, record.ID, nil)
		if err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: "Saved.", Undo: undo}, nil
	case toolUpdate:
		return s.executeUpdate(ctx, record)
	case toolRemove:
		var confirmed confirmedRemoval
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed action could not be read; nothing was deleted."}
		}
		// The whole route is read before it goes, so an Undo can put it back.
		route, found, err := s.repository.LoadRoute(ctx, record.PrincipalID, confirmed.RouteID)
		if err != nil {
			return approval.Outcome{}, err
		}
		deleted := false
		if found {
			if deleted, err = s.Delete(ctx, record.PrincipalID, confirmed.RouteID); err != nil {
				return approval.Outcome{}, err
			}
		}
		if !deleted {
			return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That route no longer exists."}
		}
		undo, err := json.Marshal(routeUndo{RouteID: route.ID, Removed: &route})
		if err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: "Deleted.", Undo: undo}, nil
	default:
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That action is no longer available."}
	}
}

func (s *Service) executeUpdate(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedUpdate
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed change could not be read; nothing was changed."}
	}
	current, found, err := s.repository.LoadRoute(ctx, record.PrincipalID, confirmed.RouteID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That route no longer exists; nothing was changed."}
	}
	stale := &approval.ActionError{Reason: "stale", Text: "That route changed after this was proposed; nothing was changed. Ask again to change it."}
	if !current.UpdatedAt.Equal(confirmed.UpdatedAt) {
		return approval.Outcome{}, stale
	}
	spec, err := normalize(confirmed.Spec)
	if err != nil {
		return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "That route is not valid (" + err.Error() + "); nothing was changed."}
	}
	next := current
	next.Spec, next.UpdatedAt = spec, s.now()
	updated, err := s.repository.UpdateRoute(ctx, next, confirmed.UpdatedAt)
	if errors.Is(err, ErrNameTaken) {
		return approval.Outcome{}, &approval.ActionError{Reason: "name_taken", Text: "Another route is already called " + spec.Name + "; nothing was changed."}
	}
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, stale
	}
	s.logger.InfoContext(ctx, "route.updated", "principal_id", record.PrincipalID, "route_id", current.ID)
	undo, err := s.undoPayload(ctx, record.PrincipalID, current.ID, &current)
	if err != nil {
		return approval.Outcome{}, err
	}
	return approval.Outcome{Text: "Changed.", Undo: undo}, nil
}

func tooManyText() string {
	return fmt.Sprintf("You already have %d routes, which is the most allowed; delete one in /jobs first.", MaximumRoutesPerPrincipal)
}

// renderRoutes is the principal's routes as the model reads them.
func renderRoutes(routes []Route) string {
	if len(routes) == 0 {
		return "The user has no saved routes."
	}
	rendered := make([]string, 0, len(routes))
	for _, route := range routes {
		rendered = append(rendered, route.Heading()+"\n"+route.Outline())
	}
	return strings.Join(rendered, "\n\n")
}

// Render is a checked route as the model reads it: one block per leg, its status first, then the
// overall result.
func (s *Service) Render(result Result) string {
	clock := func(at time.Time) string { return at.In(s.location).Format("15:04") }
	var builder strings.Builder
	fmt.Fprintf(&builder, "Check of the route %s on %s (times in %s). Each leg starts with its status; copy it word for word:\n",
		result.Route.Heading(), result.Date.Format("Mon 2 Jan"), s.location)
	writeDepartures := func(departures []transit.Departure) {
		var lines strings.Builder
		transit.Write(&lines, s.location, departures)
		for _, line := range strings.Split(strings.TrimRight(lines.String(), "\n"), "\n") {
			builder.WriteString("   " + line + "\n")
		}
	}
	for index, item := range result.Legs {
		fmt.Fprintf(&builder, "Leg %d, %s: %s", index+1, item.Leg.Describe(), item.Status)
		taken := ""
		if item.Taken >= 0 {
			taken = "the " + clock(item.Departures[item.Taken].Planned)
			if item.Departures[item.Taken].Alternative {
				taken = "the alternative " + clock(item.Departures[item.Taken].Planned) + " instead of a saved departure"
			}
		}
		switch {
		case item.Taken >= 0 && !item.Start.IsZero() && !item.End.IsZero():
			fmt.Fprintf(&builder, ", taking %s, %s–%s", taken, clock(item.Start), clock(item.End))
		case item.Taken >= 0 && !item.Start.IsZero():
			fmt.Fprintf(&builder, ", taking %s", taken)
		case item.Taken >= 0:
			fmt.Fprintf(&builder, ", first usable: %s", taken)
		case !item.Start.IsZero():
			fmt.Fprintf(&builder, ", %s–%s", clock(item.Start), clock(item.End))
		}
		builder.WriteString("\n")
		if item.Note != "" {
			fmt.Fprintf(&builder, "   %s\n", item.Note)
		}
		saved := item.Saved()
		alternatives := item.Departures[len(saved):]
		if len(alternatives) > 0 {
			builder.WriteString("   Saved departures: none usable; ")
			if cmp.Or(item.SavedStatus, item.Status) == statusMissed {
				builder.WriteString("those that run leave before the route gets there:\n")
			} else {
				builder.WriteString("each is cancelled or does not run:\n")
			}
		}
		if len(saved) > 0 {
			writeDepartures(saved)
		}
		if len(alternatives) > 0 {
			builder.WriteString("   Alternatives the train service plans in their place, each with its own status:\n")
			writeDepartures(alternatives)
		}
		for _, replacement := range item.Replace {
			fmt.Fprintf(&builder, "   The saved %s is not in the timetable on this day; ", clock(replacement.Planned))
			switch {
			case replacement.Found:
				fmt.Fprintf(&builder, "the nearest real departure is the %s.", clock(replacement.Nearest.Planned))
			case replacement.Checked:
				builder.WriteString("no real departure near it was found.")
			default:
				builder.WriteString("no nearby departure was looked up on this check.")
			}
			builder.WriteString(" If it never runs, offer to replace it in the route.\n")
		}
	}
	builder.WriteString("Result: ")
	switch result.Verdict {
	case VerdictMet:
		spare := int(result.Target.Sub(result.Arrival).Minutes())
		fmt.Fprintf(&builder, "arrives %s; the arrival target %s is met with %d min to spare.", clock(result.Arrival), clock(result.Target), spare)
	case VerdictMissed:
		late := int(result.Arrival.Sub(result.Target).Minutes())
		fmt.Fprintf(&builder, "arrives %s; the arrival target %s is missed by %d min.", clock(result.Arrival), clock(result.Target), late)
	case VerdictUnknown:
		fmt.Fprintf(&builder, "the arrival cannot be worked out (%s), so whether the arrival target %s is met is unknown.", result.Reason, clock(result.Target))
	default:
		if result.Arrival.IsZero() {
			fmt.Fprintf(&builder, "the arrival cannot be worked out (%s).", result.Reason)
		} else {
			fmt.Fprintf(&builder, "arrives %s; no arrival target is saved.", clock(result.Arrival))
		}
	}
	if !result.Leave.IsZero() {
		fmt.Fprintf(&builder, " Leave by %s.", clock(result.Leave))
	}
	builder.WriteString("\nLegs that failed to check: ")
	if len(result.FailedLegs) == 0 {
		builder.WriteString("none.")
	} else {
		numbers := make([]string, 0, len(result.FailedLegs))
		for _, number := range result.FailedLegs {
			numbers = append(numbers, fmt.Sprint(number))
		}
		builder.WriteString(strings.Join(numbers, ", ") + ".")
	}
	return builder.String()
}

func errorResult(text string) tools.Result {
	return tools.Result{Text: text, IsError: true}
}

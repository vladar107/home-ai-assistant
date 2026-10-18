package routes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

// Repository stores routes, always scoped to their principal.
type Repository interface {
	// CreateRoute stores a new route unless one with its ID exists, refusing with ErrTooManyRoutes
	// at limit and ErrNameTaken for a name the principal already uses.
	CreateRoute(ctx context.Context, route Route, limit int) (bool, error)
	ListRoutes(ctx context.Context, principalID string) ([]Route, error)
	LoadRoute(ctx context.Context, principalID, routeID string) (Route, bool, error)
	// UpdateRoute replaces the route's spec only while it was last changed at expectedUpdatedAt,
	// refusing with ErrNameTaken for a name another of the principal's routes uses.
	UpdateRoute(ctx context.Context, route Route, expectedUpdatedAt time.Time) (bool, error)
	DeleteRoute(ctx context.Context, principalID, routeID string) (bool, error)
	// DeleteUnchangedRoute deletes the route only while it was last changed at updatedAt.
	DeleteUnchangedRoute(ctx context.Context, principalID, routeID string, updatedAt time.Time) (bool, error)
}

// Proposer is the core approval service as the routes service sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
	Act(context.Context, string, approval.Proposal) (string, error)
}

type Config struct {
	// Location is the household's time zone, for today and the arrival target.
	Location *time.Location
	// Trains and Buses check train and bus legs; nil leaves that kind of leg out.
	Trains transit.Departures
	Buses  transit.Departures
	Now    func() time.Time
}

type Service struct {
	repository Repository
	proposer   Proposer
	trains     transit.Departures
	buses      transit.Departures
	location   *time.Location
	now        func() time.Time
	logger     *slog.Logger
}

// NewService builds the routes service. A nil proposer leaves out the tools that save, change, and
// remove routes; /jobs still deletes them.
func NewService(cfg Config, repository Repository, proposer Proposer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("routes repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("routes logger is required")
	}
	if cfg.Location == nil {
		return nil, fmt.Errorf("routes location is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		repository: repository,
		proposer:   proposer,
		trains:     cfg.Trains,
		buses:      cfg.Buses,
		location:   cfg.Location,
		now:        now,
		logger:     logger,
	}, nil
}

// Routes lists the principal's routes, oldest first.
func (s *Service) Routes(ctx context.Context, principalID string) ([]Route, error) {
	return s.repository.ListRoutes(ctx, principalID)
}

// Delete removes one of the principal's routes; false means it no longer exists.
func (s *Service) Delete(ctx context.Context, principalID, routeID string) (bool, error) {
	deleted, err := s.repository.DeleteRoute(ctx, principalID, routeID)
	if err == nil && deleted {
		s.logger.InfoContext(ctx, "route.deleted", "principal_id", principalID, "route_id", routeID)
	}
	return deleted, err
}

// Find returns the principal's route called name, ignoring case; a name that matches none is a
// *SelectionError that lists the principal's routes.
func (s *Service) Find(ctx context.Context, principalID, name string) (Route, error) {
	route, refusal, err := s.named(ctx, principalID, name)
	if err != nil {
		return Route{}, err
	}
	if refusal != "" {
		return Route{}, &SelectionError{Text: refusal}
	}
	return route, nil
}

// SelectionError is a route that could not be chosen, worded for the model or the user.
type SelectionError struct {
	Text string
}

func (e *SelectionError) Error() string { return e.Text }

// Verdict is whether a checked route still meets its arrival target.
type Verdict int

const (
	VerdictNoTarget Verdict = iota
	VerdictMet
	VerdictMissed
	VerdictUnknown
)

// LegResult is one leg of a checked route. Start and End are when the leg begins and ends along
// the route, zero where they cannot be worked out.
type LegResult struct {
	Leg Leg
	// Departures are the leg's fixed departures as checked, for a train or bus leg.
	Departures []transit.Departure
	// Taken indexes the departure the route uses, or is -1.
	Taken      int
	Start, End time.Time
	// Failed reports that a departure of the leg could not be checked.
	Failed bool
	// Disabled reports that the leg's service refused because the owner switched it off, so none
	// of its departures was checked.
	Disabled bool
	// Status is the leg's normalized status phrase, which replies copy word for word.
	Status string
	Note   string
}

// Unusable reports that no departure of a train or bus leg can be taken: all are cancelled or
// gone, or the ones that run leave before the route gets there.
func (l LegResult) Unusable() bool { return l.Status == statusNoUsable || l.Status == statusMissed }

// Result is one route checked for one day.
type Result struct {
	Route Route
	// Date is local midnight of the day checked.
	Date time.Time
	Legs []LegResult
	// Leave is when to set off for the first leg, and Arrival when the last one ends; zero when
	// unknown.
	Leave, Arrival time.Time
	// Target is the arrival target on Date, zero without one.
	Target  time.Time
	Verdict Verdict
	// Reason says why the arrival could not be worked out.
	Reason string
	// FailedLegs are the 1-based numbers of the legs that failed to check.
	FailedLegs []int
}

const (
	statusAsStated   = "as stated"
	statusNotChecked = "not checked"
	statusFailed     = "check failed"
	statusNoUsable   = "no usable departure"
	statusMissed     = "connection missed"
)

// Check checks the principal's route called name, or with no name the one whose weekdays include
// date's, on the calendar day date has in its own zone; a zero date means today here. Train and
// bus legs are checked through their fixed departures and timed in order through the walk and bike
// legs, with no buffer added.
func (s *Service) Check(ctx context.Context, principalID, name string, date time.Time) (Result, error) {
	if date.IsZero() {
		date = s.now().In(s.location)
	}
	day := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, s.location)
	route, err := s.choose(ctx, principalID, name, day)
	if err != nil {
		return Result{}, err
	}
	result := Result{Route: route, Date: day}
	if route.ArriveBy != "" {
		if clock, err := transit.ParseClock(route.ArriveBy); err == nil {
			result.Target = clock.On(day, s.location)
		}
	}
	var ready time.Time
	leadMinutes := 0
	for index, leg := range route.Legs {
		item := LegResult{Leg: leg, Taken: -1}
		if leg.Mode == ModeWalk || leg.Mode == ModeBike {
			item.Status = statusAsStated
			if result.Reason == "" && !ready.IsZero() {
				item.Start, item.End = ready, ready.Add(time.Duration(leg.Minutes)*time.Minute)
				ready = item.End
			} else if result.Reason == "" {
				leadMinutes += leg.Minutes
			}
			result.Legs = append(result.Legs, item)
			continue
		}
		if item.Departures, item.Disabled, item.Note, err = s.checkLeg(ctx, principalID, leg, day); err != nil {
			return Result{}, err
		}
		for _, departure := range item.Departures {
			item.Failed = item.Failed || departure.Status == transit.StatusCheckFailed
		}
		if item.Failed {
			result.FailedLegs = append(result.FailedLegs, index+1)
		}
		broken := result.Reason != ""
		if broken {
			ready = time.Time{}
		}
		item.Taken, item.Status = take(item.Departures, ready)
		if len(item.Departures) == 0 {
			item.Status = statusNotChecked
		}
		if broken {
			result.Legs = append(result.Legs, item)
			continue
		}
		if item.Taken < 0 {
			result.Reason = fmt.Sprintf("leg %d: %s", index+1, item.Status)
			if len(item.Departures) == 0 {
				result.Reason = fmt.Sprintf("leg %d has no fixed departures saved", index+1)
			}
			result.Legs = append(result.Legs, item)
			continue
		}
		taken := item.Departures[item.Taken]
		item.Start = leaves(taken)
		switch {
		case leg.Mode == ModeTrain:
			item.End = taken.ActualArrival
			if item.End.IsZero() {
				item.End = taken.PlannedArrival
			}
		case leg.Minutes > 0:
			item.End = item.Start.Add(time.Duration(leg.Minutes) * time.Minute)
		}
		if item.End.IsZero() {
			result.Reason = fmt.Sprintf("leg %d has no arrival time", index+1)
			if leg.Mode == ModeBus {
				result.Reason = fmt.Sprintf("leg %d has no ride time saved", index+1)
			}
		}
		if result.Leave.IsZero() {
			result.Leave = item.Start.Add(-time.Duration(leadMinutes) * time.Minute)
			at := result.Leave
			for lead := range result.Legs {
				result.Legs[lead].Start = at
				at = at.Add(time.Duration(result.Legs[lead].Leg.Minutes) * time.Minute)
				result.Legs[lead].End = at
			}
		}
		ready = item.End
		result.Legs = append(result.Legs, item)
	}
	if result.Reason == "" {
		result.Arrival = ready
	}
	switch {
	case result.Target.IsZero():
		result.Verdict = VerdictNoTarget
	case result.Arrival.IsZero():
		result.Verdict = VerdictUnknown
	case result.Arrival.After(result.Target):
		result.Verdict = VerdictMissed
	default:
		result.Verdict = VerdictMet
	}
	return result, nil
}

func (s *Service) choose(ctx context.Context, principalID, name string, day time.Time) (Route, error) {
	routes, err := s.repository.ListRoutes(ctx, principalID)
	if err != nil {
		return Route{}, err
	}
	if len(routes) == 0 {
		return Route{}, &SelectionError{Text: "The user has no saved routes."}
	}
	if strings.TrimSpace(name) != "" {
		if route, found := byName(routes, name); found {
			return route, nil
		}
		return Route{}, &SelectionError{Text: fmt.Sprintf("No route is called %q. The user's routes: %s.", strings.TrimSpace(name), listNames(routes))}
	}
	var matching []Route
	for _, route := range routes {
		if route.AppliesOn(day.Weekday()) {
			matching = append(matching, route)
		}
	}
	switch len(matching) {
	case 1:
		return matching[0], nil
	case 0:
		return Route{}, &SelectionError{Text: fmt.Sprintf("No route applies on %s. The user's routes: %s. Name one.", day.Format("Monday"), listNames(routes))}
	default:
		return Route{}, &SelectionError{Text: fmt.Sprintf("Several routes apply on %s: %s. Name one.", day.Format("Monday"), listNames(matching))}
	}
}

func byName(routes []Route, name string) (Route, bool) {
	wanted := strings.Join(strings.Fields(name), " ")
	for _, route := range routes {
		if strings.EqualFold(route.Name, wanted) {
			return route, true
		}
	}
	return Route{}, false
}

func listNames(routes []Route) string {
	names := make([]string, 0, len(routes))
	for _, route := range routes {
		names = append(names, fmt.Sprintf("%q (%s)", route.Name, route.DescribeDays()))
	}
	return strings.Join(names, ", ")
}

// checkLeg checks a train or bus leg's fixed departures. A service that is not set up, turned
// off, or refusing the leg makes each departure a failed check, so the other legs still answer;
// disabled reports that the refusal was the owner's switch.
func (s *Service) checkLeg(ctx context.Context, principalID string, leg Leg, day time.Time) ([]transit.Departure, bool, string, error) {
	if len(leg.Departures) == 0 {
		return nil, false, "No fixed departures are saved for this leg.", nil
	}
	clocks := make([]transit.Clock, 0, len(leg.Departures))
	for _, value := range leg.Departures {
		clock, err := transit.ParseClock(value)
		if err != nil {
			return nil, false, "", err
		}
		clocks = append(clocks, clock)
	}
	var departures []transit.Departure
	var err error
	unavailable, disabled := "", false
	from, to := leg.From, leg.To
	switch {
	case leg.Mode == ModeTrain && s.trains != nil:
		departures, err = s.trains.CheckDepartures(ctx, principalID, transit.DepartureQuery{From: leg.From, To: leg.To, Via: leg.Via, Date: day, Times: clocks})
	case leg.Mode == ModeTrain:
		unavailable = "Train information is not set up."
	case s.buses != nil:
		from, to = leg.Stop, ""
		departures, err = s.buses.CheckDepartures(ctx, principalID, transit.DepartureQuery{From: leg.Stop, Line: leg.Line, Towards: leg.Towards, Date: day, Times: clocks})
	default:
		from, to = leg.Stop, ""
		unavailable = "Bus information is not set up."
	}
	var refusal *transit.RefusalError
	switch {
	case errors.As(err, &refusal):
		unavailable, disabled = refusal.Text, refusal.Disabled
	case err != nil:
		return nil, false, "", err
	}
	if unavailable == "" {
		return departures, false, "", nil
	}
	failed := make([]transit.Departure, 0, len(clocks))
	for _, clock := range clocks {
		failed = append(failed, transit.Departure{Planned: clock.On(day, s.location), From: from, To: to, Status: transit.StatusCheckFailed, Note: unavailable})
	}
	return failed, disabled, "", nil
}

// take picks the first departure that runs and leaves no earlier than ready, when ready is known,
// and names the leg's status: the taken departure's, or why none can be taken.
func take(departures []transit.Departure, ready time.Time) (int, string) {
	usable, failed := 0, 0
	for index, departure := range departures {
		switch departure.Status {
		case transit.StatusOnTime, transit.StatusDelayed, transit.StatusChangedPlatform:
			usable++
			if ready.IsZero() || !leaves(departure).Before(ready) {
				return index, departure.StatusText()
			}
		case transit.StatusCheckFailed:
			failed++
		}
	}
	switch {
	case len(departures) > 0 && failed == len(departures):
		return -1, statusFailed
	case usable > 0:
		return -1, statusMissed
	default:
		return -1, statusNoUsable
	}
}

func leaves(departure transit.Departure) time.Time {
	if !departure.ActualDeparture.IsZero() {
		return departure.ActualDeparture
	}
	return departure.Planned
}

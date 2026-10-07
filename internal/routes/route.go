// Package routes keeps each user's saved journeys, such as "work": the weekdays a route applies to,
// an optional arrival target, and its legs, and checks a route for a day in code. Leg values are
// only ever what the user stated; nothing here picks a walking time, a departure, or a buffer.
package routes

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	ModeWalk  = "walk"
	ModeBike  = "bike"
	ModeTrain = "train"
	ModeBus   = "bus"

	MaximumRoutesPerPrincipal = 20
	MaximumLegs               = 8
	// MaximumDepartures matches what one NS or bus departure check takes.
	MaximumDepartures = 6

	maximumNameRunes  = 60
	maximumPlaceRunes = 100
	maximumLineRunes  = 20
	maximumLegMinutes = 240
	clockLayout       = "15:04"
)

var (
	// ErrTooManyRoutes reports that the principal already holds MaximumRoutesPerPrincipal routes.
	ErrTooManyRoutes = errors.New("the principal already has the maximum number of routes")
	// ErrNameTaken reports that another of the principal's routes has the name.
	ErrNameTaken = errors.New("the principal already has a route with that name")
)

// weekdays are in the order they are shown, Monday first.
var weekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

func weekdayName(day time.Weekday) string { return weekdays[(int(day)+6)%7] }

// Leg is one part of a route. A walk or bike leg has the minutes the user stated. A train leg has
// From and To stations and an optional Via; a bus leg has a Stop, a Line, an optional Towards, and
// optional ride Minutes. Either may have fixed planned Departures, as local HH:MM.
type Leg struct {
	Mode       string   `json:"mode"`
	Minutes    int      `json:"minutes,omitempty"`
	From       string   `json:"from,omitempty"`
	To         string   `json:"to,omitempty"`
	Via        string   `json:"via,omitempty"`
	Stop       string   `json:"stop,omitempty"`
	Line       string   `json:"line,omitempty"`
	Towards    string   `json:"towards,omitempty"`
	Departures []string `json:"departures,omitempty"`
}

// Spec is a route as the user confirms it.
type Spec struct {
	Name string   `json:"name"`
	Days []string `json:"days"`
	// ArriveBy is the optional arrival target, local HH:MM.
	ArriveBy string `json:"arrive_by,omitempty"`
	Legs     []Leg  `json:"legs"`
}

// Route is one principal's stored route.
type Route struct {
	ID          string
	PrincipalID string
	Spec
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AppliesOn reports whether the route's weekdays include day's.
func (s Spec) AppliesOn(day time.Weekday) bool {
	return slices.Contains(s.Days, weekdayName(day))
}

// normalize checks a spec's shape and puts it in canonical form. It does not look station or stop
// names up; that happens when a leg is proposed.
func normalize(spec Spec) (Spec, error) {
	name, err := normalizeName(spec.Name)
	if err != nil {
		return Spec{}, err
	}
	days, err := normalizeDays(spec.Days)
	if err != nil {
		return Spec{}, err
	}
	arriveBy := ""
	if strings.TrimSpace(spec.ArriveBy) != "" {
		parsed, err := time.Parse(clockLayout, strings.TrimSpace(spec.ArriveBy))
		if err != nil {
			return Spec{}, fmt.Errorf("arrive_by must be a local time, HH:MM, such as 09:00")
		}
		arriveBy = parsed.Format(clockLayout)
	}
	if len(spec.Legs) == 0 || len(spec.Legs) > MaximumLegs {
		return Spec{}, fmt.Errorf("a route has 1 to %d legs", MaximumLegs)
	}
	legs := make([]Leg, 0, len(spec.Legs))
	timed := false
	for index, leg := range spec.Legs {
		normalized, err := normalizeLeg(leg)
		if err != nil {
			return Spec{}, fmt.Errorf("leg %d: %w", index+1, err)
		}
		timed = timed || normalized.Mode == ModeTrain || normalized.Mode == ModeBus
		legs = append(legs, normalized)
	}
	if !timed {
		return Spec{}, fmt.Errorf("a route needs at least one train or bus leg to be checked")
	}
	return Spec{Name: name, Days: days, ArriveBy: arriveBy, Legs: legs}, nil
}

func normalizeName(value string) (string, error) {
	name := strings.Join(strings.Fields(value), " ")
	if name == "" {
		return "", fmt.Errorf("a route needs a name, such as work")
	}
	if utf8.RuneCountInString(name) > maximumNameRunes {
		return "", fmt.Errorf("a route name has at most %d characters", maximumNameRunes)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("a route name must be plain text")
	}
	return name, nil
}

func normalizeDays(values []string) ([]string, error) {
	var days []string
	for _, value := range values {
		day := strings.ToLower(strings.TrimSpace(value))
		if !slices.Contains(weekdays, day) {
			return nil, fmt.Errorf("days must be among mon, tue, wed, thu, fri, sat, sun")
		}
		if !slices.Contains(days, day) {
			days = append(days, day)
		}
	}
	if len(days) == 0 {
		return nil, fmt.Errorf("a route needs the weekdays it applies to; ask the user")
	}
	slices.SortFunc(days, func(a, b string) int { return slices.Index(weekdays, a) - slices.Index(weekdays, b) })
	return days, nil
}

func normalizeLeg(leg Leg) (Leg, error) {
	mode := strings.ToLower(strings.TrimSpace(leg.Mode))
	clean := func(value string, limit int, field string) (string, error) {
		trimmed := strings.Join(strings.Fields(value), " ")
		if utf8.RuneCountInString(trimmed) > limit {
			return "", fmt.Errorf("%s has at most %d characters", field, limit)
		}
		return trimmed, nil
	}
	var err error
	result := Leg{Mode: mode}
	switch mode {
	case ModeWalk, ModeBike:
		if leg.From != "" || leg.To != "" || leg.Via != "" || leg.Stop != "" || leg.Line != "" || leg.Towards != "" || len(leg.Departures) > 0 {
			return Leg{}, fmt.Errorf("a %s leg takes only minutes", mode)
		}
		if leg.Minutes <= 0 {
			return Leg{}, fmt.Errorf("a %s leg needs the minutes the user stated; ask them instead of choosing a number", mode)
		}
		if leg.Minutes > maximumLegMinutes {
			return Leg{}, fmt.Errorf("a leg takes at most %d minutes", maximumLegMinutes)
		}
		result.Minutes = leg.Minutes
		return result, nil
	case ModeTrain:
		if leg.Stop != "" || leg.Line != "" || leg.Towards != "" || leg.Minutes != 0 {
			return Leg{}, fmt.Errorf("a train leg takes from, to, via, and departures")
		}
		if result.From, err = clean(leg.From, maximumPlaceRunes, "from"); err != nil {
			return Leg{}, err
		}
		if result.To, err = clean(leg.To, maximumPlaceRunes, "to"); err != nil {
			return Leg{}, err
		}
		if result.Via, err = clean(leg.Via, maximumPlaceRunes, "via"); err != nil {
			return Leg{}, err
		}
		if result.From == "" || result.To == "" {
			return Leg{}, fmt.Errorf("a train leg needs the from and to stations")
		}
	case ModeBus:
		if leg.From != "" || leg.To != "" || leg.Via != "" {
			return Leg{}, fmt.Errorf("a bus leg takes stop, line, towards, departures, and minutes")
		}
		if result.Stop, err = clean(leg.Stop, maximumPlaceRunes, "stop"); err != nil {
			return Leg{}, err
		}
		if result.Line, err = clean(leg.Line, maximumLineRunes, "line"); err != nil {
			return Leg{}, err
		}
		if result.Towards, err = clean(leg.Towards, maximumPlaceRunes, "towards"); err != nil {
			return Leg{}, err
		}
		if result.Stop == "" || result.Line == "" {
			return Leg{}, fmt.Errorf("a bus leg needs the stop and the line")
		}
		if leg.Minutes < 0 || leg.Minutes > maximumLegMinutes {
			return Leg{}, fmt.Errorf("a bus ride takes 1 to %d minutes", maximumLegMinutes)
		}
		result.Minutes = leg.Minutes
	default:
		return Leg{}, fmt.Errorf("mode must be walk, bike, train, or bus")
	}
	if len(leg.Departures) > MaximumDepartures {
		return Leg{}, fmt.Errorf("a leg has at most %d fixed departures", MaximumDepartures)
	}
	for _, value := range leg.Departures {
		clock, err := transit.ParseClock(value)
		if err != nil {
			return Leg{}, err
		}
		if formatted := clock.String(); !slices.Contains(result.Departures, formatted) {
			result.Departures = append(result.Departures, formatted)
		}
	}
	slices.Sort(result.Departures)
	return result, nil
}

// Describe is the leg in one line, as summaries and lists show it.
func (l Leg) Describe() string {
	switch l.Mode {
	case ModeWalk, ModeBike:
		return fmt.Sprintf("%s %d min", l.Mode, l.Minutes)
	case ModeTrain:
		text := fmt.Sprintf("train %s → %s", l.From, l.To)
		if l.Via != "" {
			text += " via " + l.Via
		}
		return text + departuresText(l.Departures)
	case ModeBus:
		text := fmt.Sprintf("bus %s from %s", l.Line, l.Stop)
		if l.Towards != "" {
			text += " towards " + l.Towards
		}
		text += departuresText(l.Departures)
		if l.Minutes > 0 {
			text += fmt.Sprintf(", ride %d min", l.Minutes)
		}
		return text
	default:
		return l.Mode
	}
}

func departuresText(departures []string) string {
	if len(departures) == 0 {
		return ", no fixed departures"
	}
	return ", departures " + strings.Join(departures, ", ")
}

// DescribeDays is the route's weekdays, such as "Mon–Fri" or "Mon, Wed".
func (s Spec) DescribeDays() string {
	switch {
	case slices.Equal(s.Days, weekdays):
		return "every day"
	case slices.Equal(s.Days, weekdays[:5]):
		return "Mon–Fri"
	case slices.Equal(s.Days, weekdays[5:]):
		return "Sat–Sun"
	}
	names := make([]string, 0, len(s.Days))
	for _, day := range s.Days {
		names = append(names, strings.ToUpper(day[:1])+day[1:])
	}
	return strings.Join(names, ", ")
}

// Heading is the route's name, weekdays, and arrival target in one line.
func (s Spec) Heading() string {
	heading := s.Name + ": " + s.DescribeDays()
	if s.ArriveBy != "" {
		heading += ", arrive by " + s.ArriveBy
	}
	return heading
}

// Describe is the route with one numbered line per leg.
func (s Spec) Describe() string {
	lines := []string{s.Heading()}
	for index, leg := range s.Legs {
		lines = append(lines, fmt.Sprintf("%d. %s", index+1, leg.Describe()))
	}
	return strings.Join(lines, "\n")
}

// Outline is the route's legs in one line, for lists.
func (s Spec) Outline() string {
	parts := make([]string, 0, len(s.Legs))
	for _, leg := range s.Legs {
		parts = append(parts, leg.Describe())
	}
	return strings.Join(parts, "; ")
}

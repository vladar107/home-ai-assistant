package schedule

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	maximumCronBytes = 100
	intervalSamples  = 50
	// maximumCatchUpSteps bounds the walk over occurrences missed during downtime; a job that
	// missed more than this many runs simply resumes from now.
	maximumCatchUpSteps = 10000
)

// Schedule is a standard five-field cron expression evaluated in one time zone.
type Schedule struct {
	Expression string
	Location   *time.Location
	spec       cron.Schedule
}

// ParseSchedule accepts only the five standard fields. Descriptors such as @every and @daily, a
// seconds field, and a CRON_TZ= prefix are refused: the zone is the job's own column, and one
// syntax is all the model and the user need to agree on.
func ParseSchedule(expression, zone string) (Schedule, error) {
	expression = strings.Join(strings.Fields(expression), " ")
	if expression == "" || len(expression) > maximumCronBytes {
		return Schedule{}, fmt.Errorf("the schedule must be a five-field cron expression such as 0 9 * * 1")
	}
	if strings.HasPrefix(expression, "@") || strings.Contains(expression, "TZ=") || len(strings.Fields(expression)) != 5 {
		return Schedule{}, fmt.Errorf("the schedule must be a five-field cron expression such as 0 9 * * 1")
	}
	spec, err := cron.ParseStandard(expression)
	if err != nil {
		return Schedule{}, fmt.Errorf("the schedule %q is not a valid cron expression", expression)
	}
	location, err := LoadZone(zone)
	if err != nil {
		return Schedule{}, err
	}
	return Schedule{Expression: expression, Location: location, spec: spec}, nil
}

// LoadZone resolves an IANA zone name; the binary embeds the zone database.
func LoadZone(zone string) (*time.Location, error) {
	if zone == "" || zone == "Local" {
		return nil, fmt.Errorf("the time zone must be an IANA name such as Europe/Amsterdam")
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("the time zone %q is not known", zone)
	}
	return location, nil
}

// Next returns the first occurrence strictly after the given instant, or the zero time when the
// expression never fires again.
//
// When clocks go back, the repeated local hour would otherwise fire a second time at the same
// wall-clock time, so an occurrence that repeats the previous one's local time is passed over.
// When clocks go forward, a time inside the missing hour does not exist and that day's occurrence
// is skipped, which is the parser's own behaviour.
func (s Schedule) Next(after time.Time) time.Time {
	local := after.In(s.Location)
	next := s.spec.Next(local)
	if !next.IsZero() && sameWallClock(next, local) {
		next = s.spec.Next(next)
	}
	return next
}

func sameWallClock(a, b time.Time) bool {
	return a.Format("2006-01-02 15:04") == b.Format("2006-01-02 15:04")
}

// Upcoming lists the next count occurrences after the given instant.
func (s Schedule) Upcoming(after time.Time, count int) []time.Time {
	occurrences := make([]time.Time, 0, count)
	for at := after; len(occurrences) < count; {
		at = s.Next(at)
		if at.IsZero() {
			break
		}
		occurrences = append(occurrences, at)
	}
	return occurrences
}

// CheckInterval refuses a schedule that never fires or that fires closer together than minimum
// anywhere in its next samples occurrences.
func (s Schedule) CheckInterval(now time.Time, minimum time.Duration) error {
	occurrences := s.Upcoming(now, intervalSamples)
	if len(occurrences) == 0 {
		return fmt.Errorf("the schedule %q never runs", s.Expression)
	}
	for index := 1; index < len(occurrences); index++ {
		if occurrences[index].Sub(occurrences[index-1]) < minimum {
			return fmt.Errorf("the schedule %q runs more often than once every %s", s.Expression, formatInterval(minimum))
		}
	}
	return nil
}

// Latest returns the last occurrence at or before now, walking forward from a known occurrence,
// and the first occurrence after now.
func (s Schedule) Latest(from, now time.Time) (time.Time, time.Time) {
	latest := time.Time{}
	at := from
	if !at.After(now) {
		latest = at
	}
	for steps := 0; steps < maximumCatchUpSteps; steps++ {
		next := s.Next(at)
		if next.IsZero() || next.After(now) {
			return latest, next
		}
		latest, at = next, next
	}
	return latest, s.Next(now)
}

func formatInterval(duration time.Duration) string {
	if duration%time.Hour == 0 {
		hours := int(duration / time.Hour)
		if hours == 1 {
			return "hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}
	return duration.String()
}

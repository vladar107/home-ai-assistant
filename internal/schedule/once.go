package schedule

import (
	"fmt"
	"strings"
	"time"
)

// maximumLeadTime is how far ahead a one-time job may be set.
const maximumLeadTime = 366 * 24 * time.Hour

var localTimeLayouts = []string{"2006-01-02 15:04", "2006-01-02T15:04"}

// OnceAt is the schedule of a one-time job already stored at the given instant.
func OnceAt(at time.Time, zone string) (Schedule, error) {
	location, err := LoadZone(zone)
	if err != nil {
		return Schedule{}, err
	}
	return Schedule{Once: at.UTC(), Location: location}, nil
}

// ParseOnce reads a local wall-clock time such as 2026-10-04 08:30 in the zone. A time that the
// spring-forward gap skips is refused rather than moved, because the user named a time that does
// not happen; a time the autumn change repeats is the earlier of the two.
func ParseOnce(value, zone string, now time.Time) (Schedule, error) {
	location, err := LoadZone(zone)
	if err != nil {
		return Schedule{}, err
	}
	value = strings.TrimSpace(value)
	var wall time.Time
	for _, layout := range localTimeLayouts {
		if wall, err = time.Parse(layout, value); err == nil {
			break
		}
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("the time must be a local date and time such as 2026-10-04 08:30")
	}
	at := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), 0, 0, location)
	if !sameWallClock(at, wall) {
		return Schedule{}, fmt.Errorf("%s does not exist in %s because the clocks change then; choose another time", value, zone)
	}
	if earlier := at.Add(-time.Hour); sameWallClock(earlier.In(location), wall) {
		at = earlier
	}
	if !at.After(now) {
		return Schedule{}, fmt.Errorf("%s in %s has already passed", value, zone)
	}
	if at.Sub(now) > maximumLeadTime {
		return Schedule{}, fmt.Errorf("a one-time job can be set at most a year ahead")
	}
	return Schedule{Once: at.UTC(), Location: location}, nil
}

// JobSchedule reads the stored schedule of a job, whichever kind it is.
func JobSchedule(job Job) (Schedule, error) {
	if job.OneTime() {
		return OnceAt(job.RunAt, job.Zone)
	}
	return ParseSchedule(job.Cron, job.Zone)
}

// Describe is the schedule as the user reads it: the cron expression and zone, or the one time.
func (s Schedule) Describe() string {
	if !s.Once.IsZero() {
		return "once, " + FormatOccurrence(s.Once, s.Location)
	}
	return s.Expression + " (" + s.Location.String() + ")"
}

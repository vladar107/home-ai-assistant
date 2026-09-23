package schedule

import (
	"strings"
	"testing"
	"time"
)

func amsterdam(t *testing.T, value string) time.Time {
	t.Helper()
	location, err := LoadZone("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.ParseInLocation("2006-01-02 15:04", value, location)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func upcoming(t *testing.T, expression, from string, count int) []string {
	t.Helper()
	schedule, err := ParseSchedule(expression, "Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	var formatted []string
	for _, at := range schedule.Upcoming(amsterdam(t, from), count) {
		formatted = append(formatted, at.UTC().Format("01-02 15:04Z"))
	}
	return formatted
}

// Daylight saving is where a naive cron misbehaves: the missing hour in spring and the repeated
// hour in autumn. These pin what a job actually does on both days.
func TestScheduleAcrossDaylightSaving(t *testing.T) {
	// Spring: 02:30 does not exist on 29 March, so that day has no run.
	if got := strings.Join(upcoming(t, "30 2 * * *", "2026-03-28 12:00", 2), " "); got != "03-30 00:30Z 03-31 00:30Z" {
		t.Fatalf("spring 02:30 = %s", got)
	}
	// Autumn: 02:30 happens twice on 25 October, and runs once.
	if got := strings.Join(upcoming(t, "30 2 * * *", "2026-10-24 12:00", 2), " "); got != "10-25 00:30Z 10-26 01:30Z" {
		t.Fatalf("autumn 02:30 = %s", got)
	}
	// A morning job keeps its local time on both sides of each change.
	if got := strings.Join(upcoming(t, "0 9 * * *", "2026-10-24 12:00", 2), " "); got != "10-25 08:00Z 10-26 08:00Z" {
		t.Fatalf("autumn 09:00 = %s", got)
	}
}

func TestScheduleIntervalAndSyntax(t *testing.T) {
	now := amsterdam(t, "2026-09-28 08:00")
	for _, expression := range []string{"0 * * * *", "0 9 * * 1", "30 7 * * 1-5"} {
		schedule, err := ParseSchedule(expression, "Europe/Amsterdam")
		if err != nil {
			t.Fatalf("%s: %v", expression, err)
		}
		if err := schedule.CheckInterval(now, time.Hour); err != nil {
			t.Fatalf("%s: %v", expression, err)
		}
	}
	for _, expression := range []string{"*/30 * * * *", "0,30 9 * * *"} {
		schedule, err := ParseSchedule(expression, "Europe/Amsterdam")
		if err != nil {
			t.Fatalf("%s: %v", expression, err)
		}
		if err := schedule.CheckInterval(now, time.Hour); err == nil {
			t.Fatalf("%s ran more than once an hour without being refused", expression)
		}
	}
	never, err := ParseSchedule("0 9 30 2 *", "Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	if err := never.CheckInterval(now, time.Hour); err == nil || !strings.Contains(err.Error(), "never runs") {
		t.Fatalf("30 February = %v", err)
	}
	for _, expression := range []string{"@every 1h", "@daily", "0 0 9 * * *", "CRON_TZ=UTC 0 9 * * *", "0 9 * *", "61 9 * * *", ""} {
		if _, err := ParseSchedule(expression, "Europe/Amsterdam"); err == nil {
			t.Fatalf("%q was accepted", expression)
		}
	}
	for _, zone := range []string{"", "Local", "Mars/Olympus"} {
		if _, err := ParseSchedule("0 9 * * *", zone); err == nil {
			t.Fatalf("zone %q was accepted", zone)
		}
	}
}

package schedule

import (
	"strings"
	"testing"
)

// The spring change skips an hour and the autumn change repeats one; a one-time job must refuse
// the first and take the earlier instant of the second.
func TestParseOnceAcrossDaylightSaving(t *testing.T) {
	now := amsterdam(t, "2026-03-01 12:00")

	if _, err := ParseOnce("2027-03-28 02:30", "Europe/Amsterdam", now); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a time in the spring gap = %v", err)
	}

	schedule, err := ParseOnce("2026-10-25 02:30", "Europe/Amsterdam", now)
	if err != nil {
		t.Fatal(err)
	}
	if got := schedule.Once.Format("01-02 15:04Z"); got != "10-25 00:30Z" {
		t.Fatalf("repeated autumn hour = %s, want the earlier instant 10-25 00:30Z", got)
	}

	schedule, err = ParseOnce("2026-10-25 03:30", "Europe/Amsterdam", now)
	if err != nil || schedule.Once.Format("01-02 15:04Z") != "10-25 02:30Z" {
		t.Fatalf("after the autumn change = %v %v", schedule.Once, err)
	}
}

func TestParseOnceRefusesPastAndTooFarAhead(t *testing.T) {
	now := amsterdam(t, "2026-10-03 12:00")

	for _, value := range []string{"2026-10-03 12:00", "2026-10-03 11:59", "2025-01-01 09:00"} {
		if _, err := ParseOnce(value, "Europe/Amsterdam", now); err == nil || !strings.Contains(err.Error(), "already passed") {
			t.Fatalf("%s = %v, want a refusal as past", value, err)
		}
	}
	if _, err := ParseOnce("2026-10-03 12:01", "Europe/Amsterdam", now); err != nil {
		t.Fatalf("a minute ahead = %v", err)
	}
	if _, err := ParseOnce("2027-10-03 09:00", "Europe/Amsterdam", now); err != nil {
		t.Fatalf("just under a year ahead = %v", err)
	}
	if _, err := ParseOnce("2027-11-03 09:00", "Europe/Amsterdam", now); err == nil || !strings.Contains(err.Error(), "a year ahead") {
		t.Fatalf("more than a year ahead = %v", err)
	}
}

func TestParseOnceAcceptsBothLayoutsAndRejectsOthers(t *testing.T) {
	now := amsterdam(t, "2026-10-03 12:00")

	spaced, err := ParseOnce("2026-10-04 08:30", "Europe/Amsterdam", now)
	if err != nil {
		t.Fatal(err)
	}
	separated, err := ParseOnce(" 2026-10-04T08:30 ", "Europe/Amsterdam", now)
	if err != nil {
		t.Fatal(err)
	}
	if !spaced.Once.Equal(separated.Once) || spaced.Once.Format("15:04Z") != "06:30Z" {
		t.Fatalf("layouts = %v and %v", spaced.Once, separated.Once)
	}
	if got := spaced.Describe(); got != "once, Sun 4 Oct 08:30 CEST" {
		t.Fatalf("describe = %q", got)
	}

	for _, value := range []string{"", "tomorrow", "2026-10-04", "2026-10-04 8:30 pm", "2026-10-04T08:30:00Z"} {
		if _, err := ParseOnce(value, "Europe/Amsterdam", now); err == nil {
			t.Fatalf("%q was accepted", value)
		}
	}
	if _, err := ParseOnce("2026-10-04 08:30", "Mars/Olympus", now); err == nil {
		t.Fatal("an unknown zone was accepted")
	}
}

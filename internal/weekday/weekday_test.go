package weekday

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNameStartsWeekOnMonday(t *testing.T) {
	if Name(time.Monday) != "mon" || Name(time.Sunday) != "sun" || Name(time.Saturday) != "sat" {
		t.Fatalf("Name: mon=%q sun=%q sat=%q", Name(time.Monday), Name(time.Sunday), Name(time.Saturday))
	}
}

func TestNormalize(t *testing.T) {
	days, err := Normalize([]string{" Fri", "mon", "MON", "wed "})
	if err != nil || !slices.Equal(days, []string{"mon", "wed", "fri"}) {
		t.Fatalf("Normalize = %v, %v", days, err)
	}
	if _, err := Normalize([]string{"mon", "monday"}); err == nil {
		t.Fatal("Normalize accepted a value that is not a day")
	}
	if days, err := Normalize(nil); err != nil || len(days) != 0 {
		t.Fatalf("Normalize(nil) = %v, %v", days, err)
	}
}

func TestDescribe(t *testing.T) {
	for days, want := range map[string]string{
		"mon,tue,wed,thu,fri,sat,sun": "every day",
		"mon,tue,wed,thu,fri":         "Mon–Fri",
		"sat,sun":                     "Sat–Sun",
		"mon,wed":                     "Mon, Wed",
	} {
		normalized, err := Normalize(strings.Split(days, ","))
		if err != nil {
			t.Fatal(err)
		}
		if got := Describe(normalized); got != want {
			t.Errorf("Describe(%s) = %q, want %q", days, got, want)
		}
	}
}

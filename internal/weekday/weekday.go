// Package weekday names the days of the week the way routes and watches store them: "mon" to
// "sun", in the order they are shown, Monday first.
package weekday

import (
	"errors"
	"slices"
	"strings"
	"time"
)

var names = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// All returns every day, Monday first.
func All() []string { return slices.Clone(names) }

// Name is how day is stored.
func Name(day time.Weekday) string { return names[(int(day)+6)%7] }

// Normalize trims, lower-cases, and de-duplicates days and orders them Monday first. It fails on a
// value that is not a day.
func Normalize(values []string) ([]string, error) {
	var days []string
	for _, value := range values {
		day := strings.ToLower(strings.TrimSpace(value))
		if !slices.Contains(names, day) {
			return nil, errors.New("days must be among mon, tue, wed, thu, fri, sat, sun")
		}
		if !slices.Contains(days, day) {
			days = append(days, day)
		}
	}
	slices.SortFunc(days, func(a, b string) int { return slices.Index(names, a) - slices.Index(names, b) })
	return days, nil
}

// Describe names normalized days for a reader, such as "Mon–Fri" or "Mon, Wed".
func Describe(days []string) string {
	switch {
	case slices.Equal(days, names):
		return "every day"
	case slices.Equal(days, names[:5]):
		return "Mon–Fri"
	case slices.Equal(days, names[5:]):
		return "Sat–Sun"
	}
	described := make([]string, 0, len(days))
	for _, day := range days {
		described = append(described, strings.ToUpper(day[:1])+day[1:])
	}
	return strings.Join(described, ", ")
}

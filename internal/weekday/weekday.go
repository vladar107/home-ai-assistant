// Package weekday names the days of the week the way routes and watches store them: "mon" to
// "sun", in the order they are shown, Monday first.
package weekday

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

var names = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

var (
	everyDay   = locale.T("every day", "каждый день")
	workdays   = locale.T("Mon–Fri", "пн–пт")
	weekend    = locale.T("Sat–Sun", "сб–вс")
	shortNames = [...]locale.Text{locale.T("Mon", "пн"), locale.T("Tue", "вт"), locale.T("Wed", "ср"), locale.T("Thu", "чт"), locale.T("Fri", "пт"), locale.T("Sat", "сб"), locale.T("Sun", "вс")}
)

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
func Describe(days []string) string { return DescribeFor(context.Background(), days) }

// DescribeFor is Describe in the context's language, for text the user reads; Describe stays
// English for the model.
func DescribeFor(ctx context.Context, days []string) string {
	switch {
	case slices.Equal(days, names):
		return everyDay.Format(ctx)
	case slices.Equal(days, names[:5]):
		return workdays.Format(ctx)
	case slices.Equal(days, names[5:]):
		return weekend.Format(ctx)
	}
	described := make([]string, 0, len(days))
	for _, day := range days {
		if index := slices.Index(names, day); index >= 0 {
			described = append(described, shortNames[index].Format(ctx))
		}
	}
	return strings.Join(described, ", ")
}

package locale

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestPluralForms(t *testing.T) {
	tasks := N("%d task", "%d tasks", "%d задача", "%d задачи", "%d задач")
	for _, tc := range []struct {
		n       int
		english string
		russian string
	}{
		{0, "0 tasks", "0 задач"},
		{1, "1 task", "1 задача"},
		{2, "2 tasks", "2 задачи"},
		{5, "5 tasks", "5 задач"},
		{11, "11 tasks", "11 задач"},
		{12, "12 tasks", "12 задач"},
		{14, "14 tasks", "14 задач"},
		{21, "21 tasks", "21 задача"},
		{22, "22 tasks", "22 задачи"},
		{25, "25 tasks", "25 задач"},
		{101, "101 tasks", "101 задача"},
		{111, "111 tasks", "111 задач"},
	} {
		if got := tasks.Format(context.Background(), tc.n); got != tc.english {
			t.Errorf("English %d = %q, want %q", tc.n, got, tc.english)
		}
		if got := tasks.Format(With(context.Background(), Russian), tc.n); got != tc.russian {
			t.Errorf("Russian %d = %q, want %q", tc.n, got, tc.russian)
		}
	}
}

func TestCountFormatPassesExtraArguments(t *testing.T) {
	left := N("%[1]d item left in %[2]s", "%[1]d items left in %[2]s", "Осталась %[1]d позиция в %[2]s", "Осталось %[1]d позиции в %[2]s", "Осталось %[1]d позиций в %[2]s")
	if got := left.Format(With(context.Background(), Russian), 3, "списке"); got != "Осталось 3 позиции в списке" {
		t.Fatalf("Format = %q", got)
	}
}

func TestContextDefaultsToEnglish(t *testing.T) {
	ctx := context.Background()
	if From(ctx) != English {
		t.Fatalf("From(empty) = %v, want English", From(ctx))
	}
	if From(With(ctx, Russian)) != Russian {
		t.Fatal("With did not carry Russian")
	}
	if From(With(With(ctx, Russian), English)) != English {
		t.Fatal("the inner language did not win")
	}
	hello := T("Hello, %s", "Привет, %s")
	if got := hello.Format(ctx, "Sam"); got != "Hello, Sam" {
		t.Fatalf("English = %q", got)
	}
	if got := hello.Format(With(ctx, Russian), "Sam"); got != "Привет, Sam" {
		t.Fatalf("Russian = %q", got)
	}
	if got := hello.FormatIn(Russian, "Sam"); got != "Привет, Sam" {
		t.Fatalf("FormatIn = %q", got)
	}
	tasks := N("%d task", "%d tasks", "%d задача", "%d задачи", "%d задач")
	if got := tasks.FormatIn(Russian, 3); got != "3 задачи" {
		t.Fatalf("Count.FormatIn = %q", got)
	}
	if Back.In(Russian) != "Назад" || Back.In(English) != "Back" {
		t.Fatal("a shared word is not in both languages")
	}
}

func TestParse(t *testing.T) {
	for code, want := range map[string]Language{"en": English, "ru": Russian} {
		if got, ok := Parse(code); !ok || got != want || got.String() != code {
			t.Errorf("Parse(%q) = %v, %v", code, got, ok)
		}
	}
	if _, ok := Parse("de"); ok {
		t.Fatal("Parse accepted a language that is not offered")
	}
}

func TestFormatPanicsOnBadArguments(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Format did not panic on a missing argument")
		}
	}()
	T("%s and %s", "%s и %s").Format(context.Background(), "a")
}

func TestFormatKeepsPercentSignsOfArguments(t *testing.T) {
	if got := T("Name: %s", "Имя: %s").Format(context.Background(), "100%!d"); got != "Name: 100%!d" {
		t.Fatalf("Format = %q", got)
	}
}

func TestDateStyles(t *testing.T) {
	at := time.Date(2026, time.January, 5, 9, 7, 0, 0, time.UTC)
	for _, tc := range []struct {
		style   DateStyle
		english string
		russian string
	}{
		{DayTime, "Mon 5 Jan 09:07", "пн 5 янв 09:07"},
		{Day, "Mon 5 Jan", "пн 5 янв"},
		{DayYear, "Mon 5 Jan 2026", "пн 5 янв 2026"},
		{DayYearTime, "Mon 5 Jan 2026 09:07", "пн 5 янв 2026 09:07"},
		{DateTime, "5 Jan 09:07", "5 янв 09:07"},
		{DateYear, "5 Jan 2026", "5 янв 2026"},
		{LongDate, "5 January 2026", "5 января 2026"},
		{Weekday, "Monday", "понедельник"},
		{WeekdayTime, "Mon 09:07", "пн 09:07"},
		{DayTimeZone, "Mon 5 Jan 09:07 UTC", "пн 5 янв 09:07 UTC"},
		{TimeOnLongDay, "09:07 on Monday 5 January 2026", "09:07, понедельник, 5 января 2026"},
	} {
		if got := Date(English, at, tc.style); got != tc.english {
			t.Errorf("English style %d = %q, want %q", tc.style, got, tc.english)
		}
		if got := Date(Russian, at, tc.style); got != tc.russian {
			t.Errorf("Russian style %d = %q, want %q", tc.style, got, tc.russian)
		}
	}
	var days, months []string
	for i := range 7 {
		days = append(days, Date(Russian, at.AddDate(0, 0, i-1), Day)[:len("вс")])
	}
	for i := range 12 {
		months = append(months, Date(Russian, time.Date(2026, time.Month(i+1), 15, 0, 0, 0, 0, time.UTC), Day)[len("пн 15 "):])
	}
	if !slices.Equal(days, []string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}) {
		t.Errorf("weekdays = %v", days)
	}
	if want := []string{"янв", "фев", "мар", "апр", "мая", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"}; !slices.Equal(months, want) {
		t.Errorf("months = %v", months)
	}
}

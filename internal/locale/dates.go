package locale

import (
	"strconv"
	"time"
)

// DateStyle is a named way to write a date the user reads.
type DateStyle uint8

const (
	// DayTime is "Mon 2 Jan 15:04", "пн 2 янв 15:04".
	DayTime DateStyle = iota
	// Day is "Mon 2 Jan", "пн 2 янв".
	Day
	// DayYear is "Mon 2 Jan 2026", "пн 2 янв 2026".
	DayYear
	// DayYearTime is "Mon 2 Jan 2026 15:04", "пн 2 янв 2026 15:04".
	DayYearTime
	// DateTime is "2 Jan 15:04", "2 янв 15:04".
	DateTime
	// DateYear is "2 Jan 2026", "2 янв 2026".
	DateYear
	// LongDate is "2 January 2026", "2 января 2026".
	LongDate
	// Weekday is "Monday", "понедельник".
	Weekday
	// WeekdayTime is "Mon 15:04", "пн 15:04".
	WeekdayTime
	// DayTimeZone is "Mon 2 Jan 15:04 MST", "пн 2 янв 15:04 MST".
	DayTimeZone
	// TimeOnLongDay is "15:04 on Monday 2 January 2026", "15:04, понедельник, 2 января 2026".
	TimeOnLongDay
)

var (
	englishLayouts = [...]string{
		DayTime:       "Mon 2 Jan 15:04",
		Day:           "Mon 2 Jan",
		DayYear:       "Mon 2 Jan 2006",
		DayYearTime:   "Mon 2 Jan 2006 15:04",
		DateTime:      "2 Jan 15:04",
		DateYear:      "2 Jan 2006",
		LongDate:      "2 January 2006",
		Weekday:       "Monday",
		WeekdayTime:   "Mon 15:04",
		DayTimeZone:   "Mon 2 Jan 15:04 MST",
		TimeOnLongDay: "15:04 on Monday 2 January 2006",
	}
	russianWeekdays     = [...]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}
	russianWeekdaysLong = [...]string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}
	russianMonths       = [...]string{"янв", "фев", "мар", "апр", "мая", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"}
	russianMonthsLong   = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}
)

// Date writes the time, in its own zone, in a style and language.
func Date(language Language, at time.Time, style DateStyle) string {
	if language != Russian {
		return at.Format(englishLayouts[style])
	}
	var (
		day      = strconv.Itoa(at.Day())
		year     = strconv.Itoa(at.Year())
		weekday  = russianWeekdays[at.Weekday()]
		month    = russianMonths[at.Month()-1]
		clock    = at.Format("15:04")
		longDate = day + " " + russianMonthsLong[at.Month()-1] + " " + year
	)
	switch style {
	case Day:
		return weekday + " " + day + " " + month
	case DayYear:
		return weekday + " " + day + " " + month + " " + year
	case DayYearTime:
		return weekday + " " + day + " " + month + " " + year + " " + clock
	case DateTime:
		return day + " " + month + " " + clock
	case DateYear:
		return day + " " + month + " " + year
	case LongDate:
		return longDate
	case Weekday:
		return russianWeekdaysLong[at.Weekday()]
	case WeekdayTime:
		return weekday + " " + clock
	case DayTimeZone:
		return weekday + " " + day + " " + month + " " + at.Format("15:04 MST")
	case TimeOnLongDay:
		return clock + ", " + russianWeekdaysLong[at.Weekday()] + ", " + longDate
	}
	return weekday + " " + day + " " + month + " " + clock
}

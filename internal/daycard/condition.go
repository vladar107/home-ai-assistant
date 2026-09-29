package daycard

import "fmt"

// Condition is the weather shown by one icon and, for the header, the background palette.
type Condition int

const (
	Clear Condition = iota
	PartlyCloudy
	Cloudy
	Fog
	Drizzle
	Rain
	HeavyRain
	Snow
	Thunderstorm
)

var conditionNames = [...]string{
	Clear:        "clear",
	PartlyCloudy: "partly cloudy",
	Cloudy:       "cloudy",
	Fog:          "fog",
	Drizzle:      "drizzle",
	Rain:         "rain",
	HeavyRain:    "heavy rain",
	Snow:         "snow",
	Thunderstorm: "thunderstorm",
}

func (c Condition) String() string {
	if !c.valid() {
		return fmt.Sprintf("Condition(%d)", int(c))
	}
	return conditionNames[c]
}

func (c Condition) valid() bool {
	return c >= Clear && c <= Thunderstorm
}

// FromWMO maps a WMO weather interpretation code, as Open-Meteo reports it, to a Condition.
// Open-Meteo uses only a subset of the 0–99 table; the other codes are mapped by their WMO 4677
// meaning so that any value still yields a sensible icon, and codes outside 0–99 read as cloudy.
func FromWMO(code int) Condition {
	switch {
	case code == 0 || code == 1:
		return Clear
	case code == 2:
		return PartlyCloudy
	case code == 3:
		return Cloudy
	case code >= 4 && code <= 12:
		return Fog
	case code == 13 || code == 17 || code == 29:
		return Thunderstorm
	case code == 14 || code == 15 || code == 16 || code == 21 || code == 25:
		return Rain
	case code == 20:
		return Drizzle
	case code == 22 || code == 23 || code == 24 || code == 26 || code == 27:
		return Snow
	case code == 18 || code == 19 || code == 28:
		return Cloudy
	case code >= 30 && code <= 35:
		return Fog
	case code >= 36 && code <= 39:
		return Snow
	case code >= 40 && code <= 49:
		return Fog
	case code >= 50 && code <= 59:
		return Drizzle
	case code == 65 || code == 67 || code == 82:
		return HeavyRain
	case code >= 60 && code <= 69:
		return Rain
	case code >= 70 && code <= 79:
		return Snow
	case code >= 80 && code <= 84:
		return Rain
	case code >= 85 && code <= 90:
		return Snow
	case code >= 91 && code <= 99:
		return Thunderstorm
	default:
		return Cloudy
	}
}

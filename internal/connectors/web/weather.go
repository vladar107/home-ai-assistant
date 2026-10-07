package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	defaultForecastDays = 3
	maximumForecastDays = 7
	maximumAPIBytes     = 1 << 20
	// maximumHourlyDaysAhead is how far past today Open-Meteo's hourly forecast reaches.
	maximumHourlyDaysAhead = 15
	// geocodingCandidates leaves room to pick the right country or region among same-named places.
	geocodingCandidates = 10

	currentFields = "temperature_2m,apparent_temperature,relative_humidity_2m,precipitation,weather_code,wind_speed_10m,wind_gusts_10m"
	dailyFields   = "weather_code,temperature_2m_max,temperature_2m_min,precipitation_sum,precipitation_probability_max,wind_speed_10m_max,sunrise,sunset"
	hourlyFields  = "temperature_2m,apparent_temperature,precipitation_probability,precipitation,weather_code,wind_speed_10m,wind_gusts_10m"

	weatherUnavailableText = "The weather service is unavailable right now."
	askForPlaceText        = "No place was given and no home place is configured; ask the user which place they want the weather for."
)

var errWeatherMalformed = errors.New("the weather service returned an unexpected response")

type weatherRequest struct {
	Location  string   `json:"location"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	Days      int      `json:"days"`
	Date      string   `json:"date"`
	FromHour  *int     `json:"from_hour"`
	ToHour    *int     `json:"to_hour"`
	WindUnit  string   `json:"wind_unit"`
}

type place struct {
	Name        string  `json:"name"`
	Timezone    string  `json:"timezone"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Admin1      string  `json:"admin1"`
}

type forecast struct {
	Timezone string `json:"timezone"`
	Current  *struct {
		Time        string   `json:"time"`
		Temperature *float64 `json:"temperature_2m"`
		Apparent    *float64 `json:"apparent_temperature"`
		Humidity    *float64 `json:"relative_humidity_2m"`
		Rain        *float64 `json:"precipitation"`
		Code        *float64 `json:"weather_code"`
		Wind        *float64 `json:"wind_speed_10m"`
		Gusts       *float64 `json:"wind_gusts_10m"`
	} `json:"current"`
	Daily *struct {
		Time        []string   `json:"time"`
		Code        []*float64 `json:"weather_code"`
		High        []*float64 `json:"temperature_2m_max"`
		Low         []*float64 `json:"temperature_2m_min"`
		Rain        []*float64 `json:"precipitation_sum"`
		Probability []*float64 `json:"precipitation_probability_max"`
		Wind        []*float64 `json:"wind_speed_10m_max"`
		Sunrise     []*string  `json:"sunrise"`
		Sunset      []*string  `json:"sunset"`
	} `json:"daily"`
	Hourly *struct {
		Time        []string   `json:"time"`
		Temperature []*float64 `json:"temperature_2m"`
		Apparent    []*float64 `json:"apparent_temperature"`
		Probability []*float64 `json:"precipitation_probability"`
		Rain        []*float64 `json:"precipitation"`
		Code        []*float64 `json:"weather_code"`
		Wind        []*float64 `json:"wind_speed_10m"`
		Gusts       []*float64 `json:"wind_gusts_10m"`
	} `json:"hourly"`
}

// hourWindow is the inclusive range of local hours an hourly forecast covers on one day.
type hourWindow struct {
	date     time.Time
	from, to int
}

func (s *Service) weatherForecast(ctx context.Context, principalID string, request weatherRequest) tools.Result {
	location := strings.TrimSpace(request.Location)
	byCoordinates := request.Latitude != nil || request.Longitude != nil
	switch {
	case byCoordinates && location != "":
		return errorResult("give either location or latitude and longitude, not both")
	case byCoordinates && (request.Latitude == nil || request.Longitude == nil):
		return errorResult("latitude and longitude must be given together")
	case byCoordinates && !validCoordinates(*request.Latitude, *request.Longitude):
		return errorResult("latitude must be between -90 and 90 and longitude between -180 and 180")
	case !byCoordinates && location == "":
		location = s.weatherHome
	}
	if !byCoordinates && location == "" {
		return errorResult(askForPlaceText)
	}
	if len([]rune(location)) > maximumPlaceRunes {
		return errorResult(fmt.Sprintf("location must be at most %d characters", maximumPlaceRunes))
	}
	days := request.Days
	if days == 0 {
		days = defaultForecastDays
	}
	if days < 1 || days > maximumForecastDays {
		return errorResult(fmt.Sprintf("days must be between 1 and %d", maximumForecastDays))
	}
	window, hourly, err := parseHourWindow(request)
	if err != nil {
		return errorResult(err.Error())
	}
	unit, err := parseWindUnit(request.WindUnit)
	if err != nil {
		return errorResult(err.Error())
	}

	var found place
	if byCoordinates {
		found = place{
			Name:      fmt.Sprintf("your location (%.4f, %.4f)", *request.Latitude, *request.Longitude),
			Latitude:  *request.Latitude,
			Longitude: *request.Longitude,
		}
	} else {
		var ok bool
		found, ok, err = s.geocode(ctx, location)
		if err != nil {
			return s.weatherFailure(ctx, principalID, err)
		}
		if !ok {
			return errorResult(fmt.Sprintf("No place called %q was found; ask the user for a nearby town or a more precise name.", location))
		}
	}
	query := placeQuery(found)
	if hourly {
		if err := s.checkHourlyDate(found, window.date); err != nil {
			return errorResult(err.Error())
		}
		query.Set("hourly", hourlyFields)
		query.Set("start_date", request.Date)
		query.Set("end_date", request.Date)
	} else {
		query.Set("forecast_days", strconv.Itoa(days))
		query.Set("current", currentFields)
		query.Set("daily", dailyFields)
	}
	var result forecast
	if err := s.getJSON(ctx, s.forecastURL, query, &result); err != nil {
		return s.weatherFailure(ctx, principalID, err)
	}
	var text string
	if hourly {
		text, err = renderHourly(found, result, window, unit)
	} else {
		text, err = renderForecast(found, result, unit)
	}
	if err != nil {
		return s.weatherFailure(ctx, principalID, err)
	}
	return tools.Result{Text: text}
}

// parseHourWindow validates the hourly inputs; it reports false when no date asks for them.
func parseHourWindow(request weatherRequest) (hourWindow, bool, error) {
	if request.Date == "" {
		if request.FromHour != nil || request.ToHour != nil {
			return hourWindow{}, false, errors.New("from_hour and to_hour need a date; give the day as YYYY-MM-DD")
		}
		return hourWindow{}, false, nil
	}
	date, err := time.Parse(time.DateOnly, request.Date)
	if err != nil {
		return hourWindow{}, false, fmt.Errorf("date must be a calendar day written as YYYY-MM-DD, such as 2026-09-28")
	}
	window := hourWindow{date: date, from: 0, to: 23}
	if request.FromHour != nil {
		window.from = *request.FromHour
	}
	if request.ToHour != nil {
		window.to = *request.ToHour
	}
	if window.from < 0 || window.from > 23 || window.to < 0 || window.to > 23 {
		return hourWindow{}, false, errors.New("from_hour and to_hour must be hours from 0 to 23")
	}
	if window.from > window.to {
		return hourWindow{}, false, errors.New("from_hour must not be later than to_hour; ask for each day separately when the hours cross midnight")
	}
	return window, true, nil
}

// checkHourlyDate bounds the date by today in the place's own calendar, falling back to UTC when
// the place's time zone is unknown.
func (s *Service) checkHourlyDate(found place, date time.Time) error {
	zone := time.UTC
	if found.Timezone != "" {
		if loaded, err := time.LoadLocation(found.Timezone); err == nil {
			zone = loaded
		}
	}
	year, month, day := s.now().In(zone).Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	last := today.AddDate(0, 0, maximumHourlyDaysAhead)
	if date.Before(today) || date.After(last) {
		return fmt.Errorf("date must be between %s and %s, today and %d days ahead in %s",
			today.Format(time.DateOnly), last.Format(time.DateOnly), maximumHourlyDaysAhead, placeName(found))
	}
	return nil
}

func validCoordinates(latitude, longitude float64) bool {
	return math.Abs(latitude) <= 90 && math.Abs(longitude) <= 180
}

func placeQuery(found place) url.Values {
	return url.Values{
		"latitude":  {strconv.FormatFloat(found.Latitude, 'f', 4, 64)},
		"longitude": {strconv.FormatFloat(found.Longitude, 'f', 4, 64)},
		"timezone":  {"auto"},
	}
}

// geocode finds a place by name. Open-Meteo matches the place name alone, so a qualifier after the
// first comma ("Zwolle, Netherlands", "Bergen, Limburg") is matched against the candidates' country
// and region instead of being sent along.
func (s *Service) geocode(ctx context.Context, query string) (place, bool, error) {
	name, qualifier, _ := strings.Cut(query, ",")
	name = strings.TrimSpace(name)
	candidates, err := s.geocodeName(ctx, name)
	if err == nil && len(candidates) == 0 {
		if base := baseName(name); base != "" && base != name {
			candidates, err = s.geocodeName(ctx, base)
		}
	}
	if err != nil || len(candidates) == 0 {
		return place{}, false, err
	}
	found := pickPlace(candidates, qualifier)
	if strings.TrimSpace(found.Name) == "" || math.Abs(found.Latitude) > 90 || math.Abs(found.Longitude) > 180 {
		return place{}, false, errWeatherMalformed
	}
	return found, true, nil
}

func (s *Service) geocodeName(ctx context.Context, name string) ([]place, error) {
	var response struct {
		Results []place `json:"results"`
	}
	query := url.Values{"name": {name}, "count": {strconv.Itoa(geocodingCandidates)}, "language": {"en"}, "format": {"json"}}
	if err := s.getJSON(ctx, s.geocodingURL, query, &response); err != nil {
		return nil, err
	}
	return response.Results, nil
}

// baseName drops a parenthesised or dashed suffix, as in "Zwolle (Overijssel)" or "Zwolle - Centrum".
func baseName(name string) string {
	if before, _, found := strings.Cut(name, "("); found {
		name = before
	}
	if before, _, found := strings.Cut(name, " - "); found {
		name = before
	}
	return strings.TrimSpace(name)
}

// pickPlace returns the first candidate whose country, country code, or region matches a part of
// the qualifier, and otherwise the first candidate, which is Open-Meteo's best match.
func pickPlace(candidates []place, qualifier string) place {
	for _, part := range strings.Split(qualifier, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		for _, candidate := range candidates {
			if strings.EqualFold(part, candidate.Country) || strings.EqualFold(part, candidate.CountryCode) ||
				strings.EqualFold(part, candidate.Admin1) {
				return candidate
			}
		}
	}
	return candidates[0]
}

func (s *Service) getJSON(ctx context.Context, endpoint *url.URL, query url.Values, into any) error {
	target := *endpoint
	target.RawQuery = query.Encode()
	resp, err := s.guard.get(ctx, &target, "application/json")
	if err != nil {
		return err
	}
	defer resp.body.Close()
	if resp.status < 200 || resp.status > 299 {
		return fmt.Errorf("%w: status %d", errWeatherMalformed, resp.status)
	}
	data, err := readLimited(resp.body, maximumAPIBytes)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, into); err != nil {
		return errWeatherMalformed
	}
	return nil
}

func (s *Service) weatherFailure(ctx context.Context, principalID string, err error) tools.Result {
	class := errorClass(err)
	if errors.Is(err, errWeatherMalformed) {
		class = "unexpected_response"
	}
	s.logger.InfoContext(ctx, "web.weather.failed", "principal_id", principalID, "error_class", class)
	if class == "timeout" {
		return errorResult("The weather service did not respond in time.")
	}
	return errorResult(weatherUnavailableText)
}

func renderForecast(found place, result forecast, unit windUnit) (string, error) {
	if result.Current == nil || result.Daily == nil || len(result.Daily.Time) == 0 {
		return "", errWeatherMalformed
	}
	daily := result.Daily
	count := len(daily.Time)
	for _, length := range []int{len(daily.Code), len(daily.High), len(daily.Low), len(daily.Rain), len(daily.Probability), len(daily.Wind), len(daily.Sunrise), len(daily.Sunset)} {
		if length != count {
			return "", errWeatherMalformed
		}
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "Weather for %s", placeName(found))
	if result.Timezone != "" {
		fmt.Fprintf(&builder, " (local time zone %s)", clip(result.Timezone, 64))
	}
	builder.WriteString("\n")

	current := result.Current
	fmt.Fprintf(&builder, "Now, at %s local time: %s", localTime(current.Time), describe(current.Code))
	if current.Temperature != nil {
		fmt.Fprintf(&builder, ", %.1f °C", *current.Temperature)
		if current.Apparent != nil {
			fmt.Fprintf(&builder, " (feels like %.1f °C)", *current.Apparent)
		}
	}
	if current.Humidity != nil {
		fmt.Fprintf(&builder, ", humidity %.0f%%", *current.Humidity)
	}
	if current.Rain != nil {
		fmt.Fprintf(&builder, ", precipitation %.1f mm", *current.Rain)
	}
	if current.Wind != nil {
		fmt.Fprintf(&builder, ", wind %s", unit.format(*current.Wind))
		if current.Gusts != nil {
			fmt.Fprintf(&builder, " gusting %s", unit.format(*current.Gusts))
		}
	}
	builder.WriteString("\n")

	fmt.Fprintf(&builder, "Forecast for %d days:\n", count)
	for index, day := range daily.Time {
		fmt.Fprintf(&builder, "- %s: %s", dayLabel(day), describe(daily.Code[index]))
		if low, high := daily.Low[index], daily.High[index]; low != nil && high != nil {
			fmt.Fprintf(&builder, ", %.0f to %.0f °C", *low, *high)
		}
		if probability := daily.Probability[index]; probability != nil {
			fmt.Fprintf(&builder, ", %.0f%% chance of precipitation", *probability)
		}
		if rain := daily.Rain[index]; rain != nil {
			fmt.Fprintf(&builder, ", %.1f mm", *rain)
		}
		if wind := daily.Wind[index]; wind != nil {
			fmt.Fprintf(&builder, ", wind up to %s", unit.format(*wind))
		}
		if sunrise, sunset := daily.Sunrise[index], daily.Sunset[index]; sunrise != nil && sunset != nil {
			fmt.Fprintf(&builder, ", sunrise %s, sunset %s", clockTime(*sunrise), clockTime(*sunset))
		}
		builder.WriteString("\n")
	}
	return strings.TrimRight(builder.String(), "\n"), nil
}

func renderHourly(found place, result forecast, window hourWindow, unit windUnit) (string, error) {
	if err := checkHourly(result); err != nil {
		return "", err
	}
	hourly := result.Hourly

	var builder strings.Builder
	fmt.Fprintf(&builder, "Weather for %s", placeName(found))
	if result.Timezone != "" {
		fmt.Fprintf(&builder, " (local time zone %s)", clip(result.Timezone, 64))
	}
	fmt.Fprintf(&builder, "\nHourly forecast for %s, %02d:00 to %02d:00 local time:\n", window.date.Format("Mon 2 Jan"), window.from, window.to)
	rendered := 0
	for index, value := range hourly.Time {
		at, err := time.Parse("2006-01-02T15:04", value)
		if err != nil {
			return "", errWeatherMalformed
		}
		if !sameDay(at, window.date) || at.Hour() < window.from || at.Hour() > window.to {
			continue
		}
		fmt.Fprintf(&builder, "- %s: %s", at.Format("15:04"), describe(hourly.Code[index]))
		if temperature := hourly.Temperature[index]; temperature != nil {
			fmt.Fprintf(&builder, ", %.1f °C", *temperature)
			if apparent := hourly.Apparent[index]; apparent != nil {
				fmt.Fprintf(&builder, " (feels like %.1f °C)", *apparent)
			}
		}
		if probability := hourly.Probability[index]; probability != nil {
			fmt.Fprintf(&builder, ", %.0f%% chance of precipitation", *probability)
		}
		if rain := hourly.Rain[index]; rain != nil {
			fmt.Fprintf(&builder, ", %.1f mm", *rain)
		}
		if wind := hourly.Wind[index]; wind != nil {
			fmt.Fprintf(&builder, ", wind %s", unit.format(*wind))
			if gusts := hourly.Gusts[index]; gusts != nil {
				fmt.Fprintf(&builder, " gusting %s", unit.format(*gusts))
			}
		}
		builder.WriteString("\n")
		rendered++
	}
	if rendered == 0 {
		return "", errWeatherMalformed
	}
	return strings.TrimRight(builder.String(), "\n"), nil
}

func checkHourly(result forecast) error {
	hourly := result.Hourly
	if hourly == nil || len(hourly.Time) == 0 {
		return errWeatherMalformed
	}
	count := len(hourly.Time)
	for _, length := range []int{len(hourly.Temperature), len(hourly.Apparent), len(hourly.Probability), len(hourly.Rain), len(hourly.Code), len(hourly.Wind), len(hourly.Gusts)} {
		if length != count {
			return errWeatherMalformed
		}
	}
	return nil
}

func sameDay(a, b time.Time) bool {
	yearA, monthA, dayA := a.Date()
	yearB, monthB, dayB := b.Date()
	return yearA == yearB && monthA == monthB && dayA == dayB
}

func placeName(found place) string {
	parts := []string{strings.TrimSpace(found.Name)}
	for _, part := range []string{found.Admin1, found.Country} {
		if part = strings.TrimSpace(part); part != "" && part != parts[len(parts)-1] {
			parts = append(parts, part)
		}
	}
	return clip(strings.Join(parts, ", "), 300)
}

func localTime(value string) string {
	parsed, err := time.Parse("2006-01-02T15:04", value)
	if err != nil {
		return "an unknown time"
	}
	return parsed.Format("Mon 2 Jan 15:04")
}

func dayLabel(value string) string {
	parsed, err := time.Parse(time.DateOnly, value)
	if err != nil {
		return clip(value, 20)
	}
	return parsed.Format("Mon 2 Jan")
}

func clockTime(value string) string {
	parsed, err := time.Parse("2006-01-02T15:04", value)
	if err != nil {
		return "unknown"
	}
	return parsed.Format("15:04")
}

// weatherCodes are the WMO weather interpretation codes Open-Meteo reports.
var weatherCodes = map[int]string{
	0: "clear sky", 1: "mainly clear", 2: "partly cloudy", 3: "overcast",
	45: "fog", 48: "freezing fog",
	51: "light drizzle", 53: "drizzle", 55: "heavy drizzle",
	56: "light freezing drizzle", 57: "heavy freezing drizzle",
	61: "light rain", 63: "rain", 65: "heavy rain",
	66: "light freezing rain", 67: "heavy freezing rain",
	71: "light snow", 73: "snow", 75: "heavy snow", 77: "snow grains",
	80: "light rain showers", 81: "rain showers", 82: "violent rain showers",
	85: "light snow showers", 86: "heavy snow showers",
	95: "thunderstorm", 96: "thunderstorm with light hail", 99: "thunderstorm with heavy hail",
}

func describe(code *float64) string {
	if code == nil {
		return "conditions unknown"
	}
	if description, found := weatherCodes[int(*code)]; found {
		return description
	}
	return fmt.Sprintf("weather code %.0f", *code)
}

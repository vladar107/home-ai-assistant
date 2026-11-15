// Package weathersim is a loopback fake of the Open-Meteo geocoding and forecast endpoints the web
// connector reads. Places and their forecasts come from the seed; a request with a parameter or a
// variable Open-Meteo does not take, or for coordinates nothing was seeded for, is refused as
// Open-Meteo refuses a bad request.
//
// The web connector only dials public addresses on standard ports, so the fake is mounted on a
// simkit.Router under a public-looking host name, and the connector is given the router's
// resolver and dialer; the connector's address guard stays whole.
//
// Example seed (fictional town):
//
//	{
//	  "places": [{"name": "Eastbrook", "latitude": 52.1, "longitude": 5.1, "country": "Netherlands",
//	    "country_code": "NL", "admin1": "Westland", "timezone": "Europe/Amsterdam", "population": 250000}],
//	  "forecasts": [{"latitude": 52.1, "longitude": 5.1, "timezone": "Europe/Amsterdam",
//	    "current": {"time": "2026-03-10T08:00", "temperature": 7.5, "apparent": 5, "humidity": 80,
//	      "precipitation": 0, "code": 3, "wind": 18, "gusts": 30},
//	    "days": [{"date": "2026-03-10", "code": 61, "high": 9, "low": 3, "rain": 4.2, "probability": 70,
//	      "wind": 25, "sunrise": "2026-03-10T06:45", "sunset": "2026-03-10T18:20"}],
//	    "hours": [{"time": "2026-03-10T08:00", "temperature": 7.5, "apparent": 5, "probability": 20,
//	      "rain": 0, "code": 3, "wind": 18, "gusts": 30}],
//	    "minutely": [{"time": "2026-03-10T08:00", "precipitation": 0.1}]}]
//	}
package weathersim

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

const (
	// Host is the name the fake answers under on a router.
	Host            = "weather.example"
	GeocodingPath   = "/v1/search"
	ForecastPath    = "/v1/forecast"
	coordinateSlack = 0.01
)

var (
	currentVariables = []string{"temperature_2m", "apparent_temperature", "relative_humidity_2m", "precipitation", "weather_code", "wind_speed_10m", "wind_gusts_10m"}
	dailyVariables   = []string{"weather_code", "temperature_2m_max", "temperature_2m_min", "precipitation_sum", "precipitation_probability_max", "wind_speed_10m_max", "sunrise", "sunset", "daylight_duration", "sunshine_duration"}
	hourlyVariables  = []string{"temperature_2m", "apparent_temperature", "precipitation_probability", "precipitation", "weather_code", "wind_speed_10m", "wind_gusts_10m"}
)

type Seed struct {
	Places    []Place    `json:"places"`
	Forecasts []Forecast `json:"forecasts"`
}

// Place is a geocoder answer.
type Place struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Admin1      string  `json:"admin1"`
	Timezone    string  `json:"timezone"`
	Population  int64   `json:"population"`
	FeatureCode string  `json:"feature_code"`
}

// Forecast is the weather at coordinates. Days and hours are the whole series; a request takes
// the part it asks for.
type Forecast struct {
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Timezone  string    `json:"timezone"`
	Current   *Current  `json:"current"`
	Days      []Day     `json:"days"`
	Hours     []Hour    `json:"hours"`
	Minutely  []Quarter `json:"minutely"`
}

type Current struct {
	Time          string   `json:"time"`
	Temperature   *float64 `json:"temperature"`
	Apparent      *float64 `json:"apparent"`
	Humidity      *float64 `json:"humidity"`
	Precipitation *float64 `json:"precipitation"`
	Code          *float64 `json:"code"`
	Wind          *float64 `json:"wind"`
	Gusts         *float64 `json:"gusts"`
}

type Day struct {
	Date        string   `json:"date"`
	Code        *float64 `json:"code"`
	High        *float64 `json:"high"`
	Low         *float64 `json:"low"`
	Rain        *float64 `json:"rain"`
	Probability *float64 `json:"probability"`
	Wind        *float64 `json:"wind"`
	Sunrise     *string  `json:"sunrise"`
	Sunset      *string  `json:"sunset"`
	Daylight    *float64 `json:"daylight_seconds"`
	Sunshine    *float64 `json:"sunshine_seconds"`
}

type Hour struct {
	Time        string   `json:"time"`
	Temperature *float64 `json:"temperature"`
	Apparent    *float64 `json:"apparent"`
	Probability *float64 `json:"probability"`
	Rain        *float64 `json:"rain"`
	Code        *float64 `json:"code"`
	Wind        *float64 `json:"wind"`
	Gusts       *float64 `json:"gusts"`
}

type Quarter struct {
	Time          string   `json:"time"`
	Precipitation *float64 `json:"precipitation"`
}

// Validate checks that seed has the shape the fake accepts.
func Validate(seed json.RawMessage) error {
	_, err := parse(seed)
	return err
}

func parse(raw json.RawMessage) (Seed, error) {
	var seed Seed
	if err := simkit.Decode(raw, &seed); err != nil {
		return seed, err
	}
	for index, place := range seed.Places {
		if place.Name == "" {
			return seed, fmt.Errorf("places[%d] needs a name", index)
		}
	}
	for index, forecast := range seed.Forecasts {
		for _, day := range forecast.Days {
			if len(day.Date) != len("2006-01-02") {
				return seed, fmt.Errorf("forecasts[%d] days need a date as YYYY-MM-DD", index)
			}
		}
		for _, hour := range forecast.Hours {
			if len(hour.Time) != len("2006-01-02T15:04") {
				return seed, fmt.Errorf("forecasts[%d] hours need a time as YYYY-MM-DDTHH:MM", index)
			}
		}
	}
	return seed, nil
}

// Server is the running fake.
type Server struct {
	*simkit.Server
	seed Seed
}

// Start runs the fake on a loopback port; the caller closes it.
func Start(raw json.RawMessage) (*Server, error) {
	seed, err := parse(raw)
	if err != nil {
		return nil, err
	}
	s := &Server{seed: seed}
	inner, err := simkit.Serve(http.HandlerFunc(s.serve))
	if err != nil {
		return nil, err
	}
	s.Server = inner
	return s, nil
}

// New runs the fake for the length of a test.
func New(t testing.TB, raw json.RawMessage) *Server {
	t.Helper()
	s, err := Start(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.Cleanup(t)
	return s
}

// Mount routes Host to the fake and returns the addresses for the web connector's GeocodingURL and
// ForecastURL.
func (s *Server) Mount(router *simkit.Router) (geocodingURL, forecastURL string) {
	router.Add(Host, strings.TrimPrefix(s.URL(), "http://"))
	return "http://" + Host + GeocodingPath, "http://" + Host + ForecastPath
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case GeocodingPath:
		s.geocode(w, r)
	case ForecastPath:
		s.forecast(w, r)
	default:
		http.NotFound(w, r)
	}
}

func refuse(w http.ResponseWriter, reason string) {
	simkit.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": true, "reason": reason})
}

func (s *Server) geocode(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if unknown := simkit.UnknownParameters(query, "name", "count", "language", "format"); len(unknown) > 0 {
		refuse(w, "Unknown parameter: "+strings.Join(unknown, ", "))
		return
	}
	count, err := strconv.Atoi(query.Get("count"))
	if err != nil || count < 1 {
		count = 10
	}
	name := strings.ToLower(strings.TrimSpace(query.Get("name")))
	var results []Place
	if len([]rune(name)) >= 2 {
		for _, place := range s.seed.Places {
			if strings.HasPrefix(strings.ToLower(place.Name), name) {
				results = append(results, place)
			}
		}
	}
	if len(results) == 0 {
		simkit.WriteJSON(w, http.StatusOK, map[string]any{})
		return
	}
	results = results[:min(len(results), count)]
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}

func variables(w http.ResponseWriter, query map[string][]string, name string, allowed []string) ([]string, bool) {
	value := ""
	if values := query[name]; len(values) > 0 {
		value = values[0]
	}
	if value == "" {
		return nil, true
	}
	list := strings.Split(value, ",")
	for _, variable := range list {
		if !slices.Contains(allowed, variable) {
			refuse(w, fmt.Sprintf("Data corrupted at path ''. Cannot initialize %s from invalid String value %s.", name, variable))
			return nil, false
		}
	}
	return list, true
}

func (s *Server) forecast(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if unknown := simkit.UnknownParameters(query, "latitude", "longitude", "timezone", "current", "daily", "hourly",
		"forecast_days", "start_date", "end_date", "minutely_15", "forecast_minutely_15"); len(unknown) > 0 {
		refuse(w, "Unknown parameter: "+strings.Join(unknown, ", "))
		return
	}
	latitude, latErr := strconv.ParseFloat(query.Get("latitude"), 64)
	longitude, lonErr := strconv.ParseFloat(query.Get("longitude"), 64)
	if latErr != nil || lonErr != nil {
		refuse(w, "Parameter latitude and longitude are required")
		return
	}
	var found *Forecast
	for index := range s.seed.Forecasts {
		candidate := &s.seed.Forecasts[index]
		if math.Abs(candidate.Latitude-latitude) <= coordinateSlack && math.Abs(candidate.Longitude-longitude) <= coordinateSlack {
			found = candidate
			break
		}
	}
	if found == nil {
		refuse(w, "No forecast is available for these coordinates")
		return
	}
	current, ok := variables(w, query, "current", currentVariables)
	if !ok {
		return
	}
	daily, ok := variables(w, query, "daily", dailyVariables)
	if !ok {
		return
	}
	hourly, ok := variables(w, query, "hourly", hourlyVariables)
	if !ok {
		return
	}
	answer := map[string]any{"latitude": found.Latitude, "longitude": found.Longitude, "timezone": found.Timezone}
	if len(current) > 0 && found.Current != nil {
		answer["current"] = currentAnswer(*found.Current, current)
	}
	if len(daily) > 0 {
		days := found.Days
		if value := query.Get("forecast_days"); value != "" {
			if count, err := strconv.Atoi(value); err == nil && count >= 0 {
				days = days[:min(len(days), count)]
			}
		}
		days = slices.DeleteFunc(slices.Clone(days), func(day Day) bool { return outside(query, day.Date) })
		answer["daily"] = dailyAnswer(days, daily)
	}
	if len(hourly) > 0 {
		hours := slices.DeleteFunc(slices.Clone(found.Hours), func(hour Hour) bool { return outside(query, hour.Time[:10]) })
		answer["hourly"] = hourlyAnswer(hours, hourly)
	}
	if query.Get("minutely_15") == "precipitation" {
		limit := len(found.Minutely)
		if count, err := strconv.Atoi(query.Get("forecast_minutely_15")); err == nil && count >= 0 {
			limit = min(limit, count)
		}
		times, values := []string{}, []*float64{}
		for _, slot := range found.Minutely[:limit] {
			times, values = append(times, slot.Time), append(values, slot.Precipitation)
		}
		answer["minutely_15"] = map[string]any{"time": times, "precipitation": values}
	}
	simkit.WriteJSON(w, http.StatusOK, answer)
}

// outside reports whether a date is outside the request's start_date to end_date.
func outside(query map[string][]string, date string) bool {
	if values := query["start_date"]; len(values) > 0 && date < values[0] {
		return true
	}
	if values := query["end_date"]; len(values) > 0 && date > values[0] {
		return true
	}
	return false
}

func currentAnswer(current Current, variables []string) map[string]any {
	values := map[string]*float64{
		"temperature_2m": current.Temperature, "apparent_temperature": current.Apparent,
		"relative_humidity_2m": current.Humidity, "precipitation": current.Precipitation,
		"weather_code": current.Code, "wind_speed_10m": current.Wind, "wind_gusts_10m": current.Gusts,
	}
	answer := map[string]any{"time": current.Time}
	for _, variable := range variables {
		answer[variable] = values[variable]
	}
	return answer
}

func dailyAnswer(days []Day, variables []string) map[string]any {
	answer := map[string]any{}
	times := []string{}
	for _, day := range days {
		times = append(times, day.Date)
	}
	answer["time"] = times
	for _, variable := range variables {
		numbers, texts := []*float64{}, []*string{}
		for _, day := range days {
			switch variable {
			case "weather_code":
				numbers = append(numbers, day.Code)
			case "temperature_2m_max":
				numbers = append(numbers, day.High)
			case "temperature_2m_min":
				numbers = append(numbers, day.Low)
			case "precipitation_sum":
				numbers = append(numbers, day.Rain)
			case "precipitation_probability_max":
				numbers = append(numbers, day.Probability)
			case "wind_speed_10m_max":
				numbers = append(numbers, day.Wind)
			case "daylight_duration":
				numbers = append(numbers, day.Daylight)
			case "sunshine_duration":
				numbers = append(numbers, day.Sunshine)
			case "sunrise":
				texts = append(texts, day.Sunrise)
			case "sunset":
				texts = append(texts, day.Sunset)
			}
		}
		if variable == "sunrise" || variable == "sunset" {
			answer[variable] = texts
		} else {
			answer[variable] = numbers
		}
	}
	return answer
}

func hourlyAnswer(hours []Hour, variables []string) map[string]any {
	answer := map[string]any{}
	times := []string{}
	for _, hour := range hours {
		times = append(times, hour.Time)
	}
	answer["time"] = times
	for _, variable := range variables {
		numbers := []*float64{}
		for _, hour := range hours {
			switch variable {
			case "temperature_2m":
				numbers = append(numbers, hour.Temperature)
			case "apparent_temperature":
				numbers = append(numbers, hour.Apparent)
			case "precipitation_probability":
				numbers = append(numbers, hour.Probability)
			case "precipitation":
				numbers = append(numbers, hour.Rain)
			case "weather_code":
				numbers = append(numbers, hour.Code)
			case "wind_speed_10m":
				numbers = append(numbers, hour.Wind)
			case "wind_gusts_10m":
				numbers = append(numbers, hour.Gusts)
			}
		}
		answer[variable] = numbers
	}
	return answer
}

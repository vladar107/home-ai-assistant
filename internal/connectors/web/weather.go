package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	defaultForecastDays = 3
	maximumForecastDays = 7
	maximumAPIBytes     = 1 << 20
	// maximumHourlyDaysAhead is how far past today Open-Meteo's hourly forecast reaches.
	maximumHourlyDaysAhead = 15
	// geocodingCandidates leaves room to pick among same-named places; qualifiedCandidates, the
	// most Open-Meteo answers, is asked for when a qualifier names the country or region, so a
	// smaller namesake there is among them.
	geocodingCandidates = 10
	qualifiedCandidates = 100
	// maximumGeocodeTries bounds the names one location is looked up by, such as the parts of a
	// street address; each name costs one request, or nine when the language retry runs.
	maximumGeocodeTries = 4
	// strongPopulation is the population from which a settlement of the very name asked for, or
	// the first of its answer, is taken as the place meant, with no retry in other languages.
	strongPopulation = 100_000
	// lookupTimeout bounds the name's language retry, and separately a qualifier's whole lookup,
	// English and retry together. In the usual case they fit inside the tool's timeout after the
	// English request; when they do not, the tool's deadline cuts them off and the call fails.
	lookupTimeout = 5 * time.Second
	// geocodeLifetime is how long a geocoding answer is reused, so a name's extra lookups are paid
	// once a day.
	geocodeLifetime = 24 * time.Hour
	// maximumGeocodeEntries bounds the remembered answers, since the names come from the model.
	maximumGeocodeEntries = 1000
	// maximumFoundInBytes bounds the list of namesakes a refusal names.
	maximumFoundInBytes = 300

	currentFields = "temperature_2m,apparent_temperature,relative_humidity_2m,precipitation,weather_code,wind_speed_10m,wind_gusts_10m"
	dailyFields   = "weather_code,temperature_2m_max,temperature_2m_min,precipitation_sum,precipitation_probability_max,wind_speed_10m_max,sunrise,sunset"
	hourlyFields  = "temperature_2m,apparent_temperature,precipitation_probability,precipitation,weather_code,wind_speed_10m,wind_gusts_10m"

	weatherUnavailableText = "The weather service is unavailable right now."
	askForPlaceText        = "No place was given and no home place is configured; ask the user which place they want the weather for."
)

var errWeatherMalformed = errors.New("the weather service returned an unexpected response")

// retryLanguages are the languages a name is looked up in again when English finds no strong
// match, such as a city written in its own language.
var retryLanguages = []string{"de", "fr", "es", "it", "pt", "nl", "pl", "ru"}

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
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Timezone    string  `json:"timezone"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Admin1      string  `json:"admin1"`
	Admin1ID    int64   `json:"admin1_id"`
	// Population is absent, and so zero, for many small places.
	Population  int64  `json:"population"`
	FeatureCode string `json:"feature_code"`
	// leading marks the first candidate of its answer.
	leading bool
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

	resolved, err := s.resolvePlace(ctx, principalID, placeRequest{
		name: location, latitude: request.Latitude, longitude: request.Longitude, offerCoordinates: true,
	})
	var refusal *placeRefusal
	if errors.As(err, &refusal) {
		return errorResult(sentence(refusal.text))
	}
	if err != nil {
		return s.weatherFailure(ctx, principalID, err)
	}
	found := resolved.place
	note := resolved.checkNote()
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
	if note != "" {
		text += "\n" + note
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

// checkHourlyDate bounds the date by today in the place's own calendar. A place without a known
// zone, such as one given by coordinates, is taken to be near home, so the household's calendar
// stands in for it.
func (s *Service) checkHourlyDate(found place, date time.Time) error {
	zone := s.location
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

// placeRequest names a weather place: coordinates with the label they go by, or else a name.
type placeRequest struct {
	name                string
	latitude, longitude *float64
	// label names coordinates; empty means the user's own location.
	label string
	// offerCoordinates lets a refusal suggest coordinates, for the tools that take them.
	offerCoordinates bool
}

// resolvedPlace is a weather place and the short name it goes by: the label given with its
// coordinates, the saved place's name, or the geocoded name. Its place.Name names it in full for a
// result's text. incomplete marks a geocoded place that is no strong match, chosen while a language
// of the retry failed, so another place may have been meant.
type resolvedPlace struct {
	place
	shown      string
	saved      bool
	incomplete bool
}

// checkNote asks the model to check a place an incomplete lookup chose, or is empty.
func (r resolvedPlace) checkNote() string {
	if !r.incomplete {
		return ""
	}
	return fmt.Sprintf("The place lookup was incomplete, so %s was used; check that it is the place meant.", placeName(r.place))
}

// placeRefusal is why a place could not be resolved as given, worded for the model.
type placeRefusal struct{ text string }

func (r *placeRefusal) Error() string { return r.text }

// resolvePlace finds a place the same way for every weather tool: coordinates as given, one of the
// principal's saved places, or else geocoded. A *placeRefusal is told to the model; any other
// error is the weather service failing.
func (s *Service) resolvePlace(ctx context.Context, principalID string, request placeRequest) (resolvedPlace, error) {
	if request.latitude != nil && request.longitude != nil {
		label := request.label
		if label == "" {
			label = "your location"
		}
		return resolvedPlace{
			place: place{
				Name:      fmt.Sprintf("%s (%.4f, %.4f)", label, *request.latitude, *request.longitude),
				Latitude:  *request.latitude,
				Longitude: *request.longitude,
			},
			shown: label,
		}, nil
	}
	saved, ok, err := s.savedPlace(ctx, principalID, request.name)
	if err != nil {
		s.logger.WarnContext(ctx, "web.weather.failed", "principal_id", principalID, "stage", "saved_place", "error", err)
		return resolvedPlace{}, &placeRefusal{"the user's saved places could not be read right now"}
	}
	if ok {
		return resolvedPlace{
			place: place{Name: saved.Name + " (saved place)", Latitude: saved.Latitude, Longitude: saved.Longitude},
			shown: saved.Name,
			saved: true,
		}, nil
	}
	found, ok, err := s.geocode(ctx, principalID, request.name)
	var refusal *placeRefusal
	if errors.As(err, &refusal) && request.offerCoordinates {
		return resolvedPlace{}, &placeRefusal{refusal.text + ", or its latitude and longitude"}
	}
	if err != nil {
		return resolvedPlace{}, err
	}
	if !ok {
		return resolvedPlace{}, &placeRefusal{fmt.Sprintf("no place called %q was found; try its English name with its country, such as Zwolle, Netherlands, and otherwise ask the user for a nearby town or a more precise name", request.name)}
	}
	return found, nil
}

// sentence makes a refusal's text a sentence of its own.
func sentence(text string) string {
	return strings.ToUpper(text[:1]) + text[1:] + "."
}

// geocode finds a place by name. Open-Meteo matches the place name alone, so a qualifier after the
// first comma ("Zwolle, Netherlands", "Bergen, Limburg") is matched against the candidates' country
// and region instead of being sent along. When the name before the first comma finds nothing, as with
// a street address, each later part is tried in turn with its postcode stripped. A qualifier that
// matches no candidate is a *placeRefusal.
func (s *Service) geocode(ctx context.Context, principalID, query string) (resolvedPlace, bool, error) {
	parts := strings.Split(query, ",")
	qualifier := parts[1:]
	answers, err := s.lookupName(ctx, principalID, strings.TrimSpace(parts[0]), candidateCount(qualifier))
	tries := 1
	for i := 1; err == nil && !anyFound(answers.lists) && i < len(parts) && tries < maximumGeocodeTries; i++ {
		town := stripPostcode(parts[i])
		if town == "" {
			continue
		}
		qualifier = parts[i+1:]
		answers, err = s.lookupName(ctx, principalID, town, candidateCount(qualifier))
		tries++
	}
	if err != nil || !anyFound(answers.lists) {
		return resolvedPlace{}, false, err
	}
	found, err := s.pickPlace(ctx, principalID, &answers, qualifier)
	if err != nil {
		return resolvedPlace{}, false, err
	}
	if strings.TrimSpace(found.Name) == "" || math.Abs(found.Latitude) > 90 || math.Abs(found.Longitude) > 180 {
		return resolvedPlace{}, false, errWeatherMalformed
	}
	return resolvedPlace{
		place:      found,
		shown:      strings.TrimSpace(found.Name),
		incomplete: answers.incomplete && !strongMatch(found, answers.name),
	}, true, nil
}

// candidateCount asks for more candidates when a qualifier will pick among them, so a namesake in
// the region named is among them; Open-Meteo answers at most 100.
func candidateCount(qualifier []string) int {
	if strings.TrimSpace(strings.Join(qualifier, "")) != "" {
		return qualifiedCandidates
	}
	return geocodingCandidates
}

// stripPostcode drops the leading tokens of an address part that contain a digit, and a short
// all-caps token right after one ("1098 NJ"), leaving the town name.
func stripPostcode(part string) string {
	fields := strings.Fields(part)
	for len(fields) > 0 && strings.ContainsFunc(fields[0], unicode.IsDigit) {
		fields = fields[1:]
		if len(fields) > 0 && len(fields[0]) <= 3 && strings.ToUpper(fields[0]) == fields[0] &&
			!strings.ContainsFunc(fields[0], func(r rune) bool { return !unicode.IsLetter(r) }) {
			fields = fields[1:]
		}
	}
	return strings.Join(fields, " ")
}

// nameAnswers are what the geocoder said for one name: the English answer first, then each retry
// language's candidates of that very name, since a looser match in another language, even a city
// it puts first, is mostly a different place. retried tells whether the retry ran, and incomplete
// whether one of its languages failed or ran out of time, with failure the last such error.
type nameAnswers struct {
	name       string
	count      int
	lists      [][]place
	retried    bool
	incomplete bool
	failure    error
}

// lookupName asks for a name in English, then for its base name when the name finds nothing.
// Unless English found a strong match or the name is a street address, it asks again in each of
// retryLanguages at once, since Open-Meteo matches a name only in the language it is asked in.
func (s *Service) lookupName(ctx context.Context, principalID, name string, count int) (nameAnswers, error) {
	english, err := s.geocodeName(ctx, name, "en", count)
	if err != nil {
		return nameAnswers{}, err
	}
	if base := baseName(name); len(english) == 0 && base != "" && base != name {
		name = base
		if english, err = s.geocodeName(ctx, name, "en", count); err != nil {
			return nameAnswers{}, err
		}
	}
	answers := nameAnswers{name: name, count: count, lists: [][]place{english}}
	if strings.ContainsFunc(name, unicode.IsDigit) || slices.ContainsFunc(english, func(candidate place) bool { return strongMatch(candidate, name) }) {
		return answers, nil
	}
	return answers, s.retryName(ctx, principalID, &answers)
}

// retryName adds the language retry's answers for the name, once.
func (s *Service) retryName(ctx context.Context, principalID string, answers *nameAnswers) error {
	if answers.retried {
		return nil
	}
	asked, err := s.askLanguages(ctx, answers.name, answers.count)
	if err != nil {
		return err
	}
	answers.retried = true
	for _, answer := range asked.answers {
		answers.lists = append(answers.lists, slices.DeleteFunc(slices.Clone(answer), func(candidate place) bool {
			return !sameName(candidate.Name, answers.name)
		}))
	}
	if asked.failed > 0 {
		s.logger.InfoContext(ctx, "web.weather.failed", "principal_id", principalID, "stage", "language_retry", "failed", asked.failed, "error_class", errorClass(asked.failure))
		answers.incomplete, answers.failure = true, asked.failure
	}
	return nil
}

// languageAnswers are the retry's answers in the order of retryLanguages, nil where a language
// failed, with how many failed and the last failure.
type languageAnswers struct {
	answers [][]place
	failed  int
	failure error
}

// askLanguages asks for a name in each of retryLanguages at once under lookupTimeout. Running out
// of that limit is a failure of the languages still waiting; ctx itself ending is an error, so a
// tool past its own deadline never takes a partial answer for a whole one.
func (s *Service) askLanguages(ctx context.Context, name string, count int) (languageAnswers, error) {
	retryCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	answers := make([][]place, len(retryLanguages))
	failures := make([]error, len(retryLanguages))
	var group sync.WaitGroup
	for index, language := range retryLanguages {
		group.Go(func() {
			answers[index], failures[index] = s.geocodeName(retryCtx, name, language, count)
		})
	}
	group.Wait()
	if err := ctx.Err(); err != nil {
		return languageAnswers{}, err
	}
	asked := languageAnswers{answers: answers}
	for _, err := range failures {
		if err != nil {
			asked.failed++
			asked.failure = err
		}
	}
	return asked, nil
}

// geocodeName asks Open-Meteo for a name in one language, or answers from what it said within the
// last geocodeLifetime. Answers are kept by language, candidate count, and name; a longer answer
// serves a shorter request, as its first candidates.
func (s *Service) geocodeName(ctx context.Context, name, language string, count int) ([]place, error) {
	key := func(count int) string {
		return language + "\x00" + strconv.Itoa(count) + "\x00" + strings.ToLower(name)
	}
	if results, found := s.cachedGeocode(key(count)); found {
		return results, nil
	}
	if count < qualifiedCandidates {
		if results, found := s.cachedGeocode(key(qualifiedCandidates)); found {
			return results[:min(count, len(results))], nil
		}
	}
	var response struct {
		Results []place `json:"results"`
	}
	query := url.Values{"name": {name}, "count": {strconv.Itoa(count)}, "language": {language}, "format": {"json"}}
	if err := s.getJSON(ctx, s.geocodingURL, query, &response); err != nil {
		return nil, err
	}
	if len(response.Results) > 0 {
		response.Results[0].leading = true
	}
	s.storeGeocode(key(count), response.Results)
	return response.Results, nil
}

type geocodeEntry struct {
	results []place
	expires time.Time
}

func (s *Service) cachedGeocode(key string) ([]place, bool) {
	s.geocodeMu.Lock()
	defer s.geocodeMu.Unlock()
	entry, found := s.geocoded[key]
	if !found || !s.now().Before(entry.expires) {
		return nil, false
	}
	return entry.results, true
}

// storeGeocode keeps an answer, making room by dropping expired answers and then the one that
// expires first, since the names come from the model.
func (s *Service) storeGeocode(key string, results []place) {
	s.geocodeMu.Lock()
	defer s.geocodeMu.Unlock()
	now := s.now()
	if len(s.geocoded) >= maximumGeocodeEntries {
		for existing, entry := range s.geocoded {
			if !now.Before(entry.expires) {
				delete(s.geocoded, existing)
			}
		}
	}
	if len(s.geocoded) >= maximumGeocodeEntries {
		oldest := ""
		for existing, entry := range s.geocoded {
			if oldest == "" || entry.expires.Before(s.geocoded[oldest].expires) {
				oldest = existing
			}
		}
		delete(s.geocoded, oldest)
	}
	s.geocoded[key] = geocodeEntry{results: results, expires: now.Add(geocodeLifetime)}
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

// sameName compares names without regard to case, surrounding space, or the suffix baseName drops;
// accents count.
func sameName(a, b string) bool {
	return strings.EqualFold(baseName(a), baseName(b))
}

// settlement reports whether a candidate is a populated place rather than a country, a region, a
// mountain, or another feature.
func (p place) settlement() bool {
	return strings.HasPrefix(p.FeatureCode, "PPL")
}

// strongMatch is a candidate that is plainly the place meant: a city of the very name asked for,
// or the city the geocoder puts first, which is how English answers a native name it knows by
// another, such as Praha with Prague. Retry answers keep only candidates of the very name, so a
// city another language puts first under another name never counts.
func strongMatch(candidate place, name string) bool {
	return candidate.settlement() && candidate.Population >= strongPopulation &&
		(candidate.leading || sameName(candidate.Name, name))
}

func anyFound(lists [][]place) bool {
	return slices.ContainsFunc(lists, func(list []place) bool { return len(list) > 0 })
}

// countryAliases are short forms of a country's name that the geocoder resolves as a country in no
// language, so a qualifier written with them would otherwise be refused.
var countryAliases = map[string]string{
	"UK": "GB", "U.K.": "GB", "GREAT BRITAIN": "GB", "BRITAIN": "GB",
	"USA": "US", "U.S.": "US", "U.S.A.": "US", "US": "US", "AMERICA": "US",
}

// area is a country, by its code, or a first-level region, by its GeoNames id, or by its name where
// the geocoder gave no id.
type area struct {
	countryCode string
	regionID    int64
	region      string
}

func (a area) holds(candidate place) bool {
	switch {
	case a.regionID != 0 && candidate.Admin1ID != 0:
		return candidate.Admin1ID == a.regionID
	case a.region != "":
		return strings.EqualFold(candidate.CountryCode, a.countryCode) && sameAreaName(candidate.Admin1, a.region)
	default:
		return strings.EqualFold(candidate.CountryCode, a.countryCode)
	}
}

// sameAreaName compares a country's or region's names without regard to case or a leading "The",
// so "Netherlands" names "The Netherlands".
func sameAreaName(a, b string) bool {
	trim := func(name string) string {
		name = strings.TrimSpace(name)
		if len(name) > 4 && strings.EqualFold(name[:4], "the ") {
			name = strings.TrimSpace(name[4:])
		}
		return name
	}
	return strings.EqualFold(trim(a), trim(b))
}

// areaNamed finds the area a qualifier part names from what is already known: a short form of a
// country, or the first candidate whose country code, country, or region the part names, in the
// language that candidate came in.
func areaNamed(part string, lists [][]place) (area, bool) {
	if code, found := countryAliases[strings.ToUpper(part)]; found {
		return area{countryCode: code}, true
	}
	for _, list := range lists {
		for _, candidate := range list {
			switch {
			case candidate.CountryCode != "" && (strings.EqualFold(part, candidate.CountryCode) || sameAreaName(part, candidate.Country)):
				return area{countryCode: candidate.CountryCode}, true
			case candidate.Admin1 != "" && sameAreaName(part, candidate.Admin1):
				return area{countryCode: candidate.CountryCode, regionID: candidate.Admin1ID, region: candidate.Admin1}, true
			}
		}
	}
	return area{}, false
}

// narrowTo keeps the candidates in every area in turn, or reports false when an area holds none.
func narrowTo(areas []area, lists [][]place) ([][]place, bool) {
	for _, within := range areas {
		lists = keepCandidates(lists, within.holds)
		if !anyFound(lists) {
			return nil, false
		}
	}
	return lists, true
}

// narrowByNames narrows to the areas the parts name among the candidates, or reports false when a
// part names none of them or names one that holds no candidate.
func narrowByNames(parts []string, lists [][]place) ([][]place, bool) {
	areas := make([]area, 0, len(parts))
	for _, part := range parts {
		named, ok := areaNamed(part, lists)
		if !ok {
			return nil, false
		}
		areas = append(areas, named)
	}
	return narrowTo(areas, lists)
}

// pickPlace chooses among a name's answers, narrowed to the qualifier's country or region. Each
// qualifier part is taken for a country code or a region id and narrows the candidates of every
// answer to those in it, in turn, so "Bergen, Limburg, Netherlands" keeps the Bergen in Limburg.
// Comparing codes and ids rather than names lets a qualifier in one language pick a candidate found
// in another. When a part names no area the candidates show, or the areas hold none of them, the
// retry languages are asked for the name, whose answers name regions in their own language; a
// part still unknown is then looked up as a country or a first-level region. Every part must name
// an area that holds the place, or the qualifier is refused rather than ignored.
func (s *Service) pickPlace(ctx context.Context, principalID string, answers *nameAnswers, qualifier []string) (place, error) {
	var parts []string
	for _, part := range qualifier {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return choosePlace(answers.name, answers.lists), nil
	}
	lists, ok := narrowByNames(parts, answers.lists)
	if !ok && !answers.retried && !strings.ContainsFunc(answers.name, unicode.IsDigit) {
		if err := s.retryName(ctx, principalID, answers); err != nil {
			return place{}, err
		}
		lists, ok = narrowByNames(parts, answers.lists)
	}
	if !ok {
		areas := make([]area, 0, len(parts))
		for _, part := range parts {
			named, found := areaNamed(part, answers.lists)
			if !found {
				var err error
				if named, found, err = s.lookupArea(ctx, principalID, part); err != nil {
					return place{}, err
				}
			}
			if !found {
				return place{}, answers.qualifierRefusal(parts)
			}
			areas = append(areas, named)
		}
		if lists, ok = narrowTo(areas, answers.lists); !ok {
			return place{}, answers.qualifierRefusal(parts)
		}
	}
	return choosePlace(answers.name, lists), nil
}

// qualifierRefusal refuses a qualifier that matches nothing, unless the retry that could have found
// the place there failed, which is the weather service failing instead.
func (a *nameAnswers) qualifierRefusal(parts []string) error {
	if a.incomplete {
		return a.failure
	}
	return &placeRefusal{fmt.Sprintf("no place called %q was found in %s, only in %s; give the place's English name with its country, such as Zwolle, Netherlands",
		a.name, clip.Bytes(strings.Join(parts, ", "), 100), foundIn(a.name, a.lists))}
}

// choosePlace picks among a name's answers, English first. A strong match in English stands; else
// the most populous strong match among the language retry's answers; else the English answer
// stands as the geocoder ranks it: a settlement it puts first stands, whatever its name; after a
// country or a region, its first settlement; after another feature, its first settlement of the
// name asked for, else that feature, so an island asked by its own name is itself. When English
// found nothing, the most populous of the retry's candidates of the name stands in for it,
// settlements first.
func choosePlace(name string, lists [][]place) place {
	english, retried := lists[0], lists[1:]
	for _, candidate := range english {
		if strongMatch(candidate, name) {
			return candidate
		}
	}
	if best, found := mostPopulous(retried, func(candidate place) bool { return strongMatch(candidate, name) }); found {
		return best
	}
	if len(english) > 0 {
		first := english[0]
		prefer := func(candidate place) bool { return candidate.settlement() && sameName(candidate.Name, name) }
		switch {
		case first.settlement():
			return first
		case first.country() || strings.HasPrefix(first.FeatureCode, "ADM"):
			prefer = place.settlement
		}
		if index := slices.IndexFunc(english, prefer); index >= 0 {
			return english[index]
		}
		return first
	}
	if best, found := mostPopulous(retried, func(candidate place) bool { return candidate.settlement() }); found {
		return best
	}
	best, _ := mostPopulous(retried, func(place) bool { return true })
	return best
}

func mostPopulous(lists [][]place, eligible func(place) bool) (place, bool) {
	var best place
	found := false
	for _, list := range lists {
		for _, candidate := range list {
			if eligible(candidate) && (!found || candidate.Population > best.Population) {
				best, found = candidate, true
			}
		}
	}
	return best, found
}

func keepCandidates(lists [][]place, keep func(place) bool) [][]place {
	kept := make([][]place, len(lists))
	for index, list := range lists {
		for _, candidate := range list {
			if keep(candidate) {
				kept[index] = append(kept[index], candidate)
			}
		}
	}
	return kept
}

// lookupArea is the country or first-level region a qualifier names: the first answer for it in
// English when that is one of that very name, else the first such answer among the retry
// languages, which know names such as "Deutschland" that English does not. Only the first answer
// is read, so few are asked for, and the retry shares the English request's five seconds. A
// qualifier that names a town, or an area of another name such as Romania for Roma, is no area; a
// lookup that failed is an error, not a refusal.
func (s *Service) lookupArea(ctx context.Context, principalID, qualifier string) (area, bool, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	names := func(answer []place) bool {
		return len(answer) > 0 && answer[0].area() && sameAreaName(answer[0].Name, qualifier)
	}
	english, err := s.geocodeName(lookupCtx, qualifier, "en", geocodingCandidates)
	if err != nil {
		s.logger.InfoContext(ctx, "web.weather.failed", "principal_id", principalID, "stage", "qualifier", "error_class", errorClass(err))
		return area{}, false, err
	}
	if names(english) {
		return english[0].asArea(), true, nil
	}
	asked, err := s.askLanguages(lookupCtx, qualifier, geocodingCandidates)
	if err != nil {
		s.logger.InfoContext(ctx, "web.weather.failed", "principal_id", principalID, "stage", "qualifier_retry", "error_class", errorClass(err))
		return area{}, false, err
	}
	for _, answer := range asked.answers {
		if names(answer) {
			return answer[0].asArea(), true, nil
		}
	}
	if asked.failed > 0 {
		s.logger.InfoContext(ctx, "web.weather.failed", "principal_id", principalID, "stage", "qualifier_retry", "failed", asked.failed, "error_class", errorClass(asked.failure))
		return area{}, false, asked.failure
	}
	return area{}, false, nil
}

// area reports whether a geocoding answer is a country or a first-level region.
func (p place) area() bool {
	return p.CountryCode != "" && (p.country() || p.FeatureCode == "ADM1")
}

func (p place) asArea() area {
	if p.country() {
		return area{countryCode: p.CountryCode}
	}
	return area{countryCode: p.CountryCode, regionID: p.ID, region: p.Name}
}

// country reports whether a geocoding answer is a country, which GeoNames codes PCL and PCLI,
// PCLD, and the like.
func (p place) country() bool {
	return strings.HasPrefix(p.FeatureCode, "PCL")
}

// foundIn names where a name's namesakes lie, as region and country, once each, so the model can
// name the one it means; without a namesake of the very name, it names where the candidates lie.
func foundIn(name string, lists [][]place) string {
	namesakes := keepCandidates(lists, func(candidate place) bool { return sameName(candidate.Name, name) })
	if !anyFound(namesakes) {
		namesakes = lists
	}
	var where []string
	seen := map[string]bool{}
	for _, list := range namesakes {
		for _, candidate := range list {
			country := strings.TrimSpace(candidate.Country)
			if country == "" {
				continue
			}
			key := strings.ToUpper(candidate.CountryCode) + "\x00" + strconv.FormatInt(candidate.Admin1ID, 10)
			if candidate.Admin1ID == 0 {
				key += "\x00" + candidate.Admin1
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			if admin1 := strings.TrimSpace(candidate.Admin1); admin1 != "" {
				country = admin1 + ", " + country
			}
			where = append(where, country)
		}
	}
	if len(where) == 0 {
		return "places without a country"
	}
	listed, length := 0, 0
	for listed < len(where) && (listed == 0 || length+len(where[listed]) <= maximumFoundInBytes) {
		length += len(where[listed]) + 2
		listed++
	}
	text := strings.Join(where[:listed], "; ")
	if listed < len(where) {
		text += fmt.Sprintf("; and %d more", len(where)-listed)
	}
	return clip.Bytes(text, maximumFoundInBytes+100)
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
		fmt.Fprintf(&builder, " (local time zone %s)", clip.Bytes(result.Timezone, 64))
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
		fmt.Fprintf(&builder, " (local time zone %s)", clip.Bytes(result.Timezone, 64))
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
	return clip.Bytes(strings.Join(parts, ", "), 300)
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
		return clip.Bytes(value, 20)
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

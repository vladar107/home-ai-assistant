package web

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/daycard"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	maximumTitleRunes   = 100
	maximumSummaryRunes = 160
	maximumLabelRunes   = 40

	daylightFields = "sunrise,sunset"
	// notableGust is the gust speed, in km/h, from which a leg's tile mentions the wind.
	notableGust = 40

	noTurnText = "A card can only be attached during a conversation turn."
)

type cardRequest struct {
	Title    string           `json:"title"`
	Summary  string           `json:"summary"`
	Language string           `json:"language"`
	Legs     []cardLegRequest `json:"legs"`
}

type cardLegRequest struct {
	Label    string `json:"label"`
	Place    string `json:"place"`
	Date     string `json:"date"`
	FromHour *int   `json:"from_hour"`
	ToHour   *int   `json:"to_hour"`
}

// cardWords are the fixed words on a card; everything else on it is a number or the model's text.
type cardWords struct {
	feelsLike, millimetres, gusts, dry string
}

var cardLanguages = map[string]cardWords{
	"en": {feelsLike: "feels like", millimetres: "mm", gusts: "gusts", dry: "dry"},
	"ru": {feelsLike: "ощущается", millimetres: "мм", gusts: "порывы", dry: "сухо"},
}

// severity ranks conditions by how much they matter to someone out and about; a leg shows the
// worst hour, since one hour of heavy rain is what decides whether to take an umbrella.
var severity = map[daycard.Condition]int{
	daycard.Clear:        0,
	daycard.PartlyCloudy: 1,
	daycard.Cloudy:       2,
	daycard.Fog:          3,
	daycard.Drizzle:      4,
	daycard.Rain:         5,
	daycard.HeavyRain:    6,
	daycard.Snow:         7,
	daycard.Thunderstorm: 8,
}

type cardLeg struct {
	label  string
	window hourWindow
	place  string
}

// legReading is one leg's hours reduced to what its tile shows.
type legReading struct {
	condition     daycard.Condition
	night         bool
	temperature   float64
	precipitation float64
	gust          float64
	hasGust       bool
	hours         int

	first struct {
		hour        int
		condition   daycard.Condition
		night       bool
		temperature float64
		apparent    float64
	}
}

func (s *Service) weatherCard(ctx context.Context, principalID string, request cardRequest) (tools.Result, error) {
	turn := tools.TurnFrom(ctx)
	if turn == "" {
		return errorResult(noTurnText), nil
	}
	language := request.Language
	if language == "" {
		language = "en"
	}
	words, known := cardLanguages[language]
	if !known {
		return errorResult(`language must be "en" or "ru"`), nil
	}
	title, summary := strings.TrimSpace(request.Title), strings.TrimSpace(request.Summary)
	if title == "" || summary == "" {
		return errorResult("A card needs a title and a one-line summary."), nil
	}
	if len([]rune(title)) > maximumTitleRunes {
		return errorResult(fmt.Sprintf("title must be at most %d characters", maximumTitleRunes)), nil
	}
	if len([]rune(summary)) > maximumSummaryRunes {
		return errorResult(fmt.Sprintf("summary must be at most %d characters", maximumSummaryRunes)), nil
	}
	if len(request.Legs) == 0 || len(request.Legs) > daycard.MaximumLegs {
		return errorResult(fmt.Sprintf("A card needs 1 to %d legs.", daycard.MaximumLegs)), nil
	}
	legs := make([]cardLeg, 0, len(request.Legs))
	for index, requested := range request.Legs {
		leg, err := s.parseCardLeg(requested)
		if err != nil {
			return errorResult(fmt.Sprintf("Leg %d: %s", index+1, err)), nil
		}
		legs = append(legs, leg)
	}

	places := map[string]place{}
	forecasts := map[string]forecast{}
	card := daycard.Card{Title: title, Summary: summary, FeelsLikeLabel: words.feelsLike}
	var report strings.Builder
	for index, leg := range legs {
		key := strings.ToLower(leg.place)
		found, cached := places[key]
		if !cached {
			var ok bool
			var err error
			found, ok, err = s.geocode(ctx, leg.place)
			if err != nil {
				return s.weatherFailure(ctx, principalID, err), nil
			}
			if !ok {
				return errorResult(fmt.Sprintf("Leg %d: no place called %q was found; ask the user for a nearby town or a more precise name.", index+1, leg.place)), nil
			}
			places[key] = found
		}
		if err := s.checkHourlyDate(found, leg.window.date); err != nil {
			return errorResult(fmt.Sprintf("Leg %d: %s", index+1, err)), nil
		}
		date := leg.window.date.Format(time.DateOnly)
		forecastKey := key + "\x00" + date
		result, cached := forecasts[forecastKey]
		if !cached {
			query := placeQuery(found)
			query.Set("hourly", hourlyFields)
			query.Set("daily", daylightFields)
			query.Set("start_date", date)
			query.Set("end_date", date)
			if err := s.getJSON(ctx, s.forecastURL, query, &result); err != nil {
				return s.weatherFailure(ctx, principalID, err), nil
			}
			forecasts[forecastKey] = result
		}
		reading, err := readLeg(result, leg.window)
		if err != nil {
			return s.weatherFailure(ctx, principalID, err), nil
		}

		label := leg.label
		if label == "" {
			label = fmt.Sprintf("%s · %02d–%02d", strings.TrimSpace(found.Name), leg.window.from, leg.window.to)
		}
		detail := reading.detail(words)
		card.Legs = append(card.Legs, daycard.Leg{
			Label:       label,
			Icon:        reading.condition,
			Night:       reading.night,
			Temperature: reading.temperature,
			Detail:      detail,
		})
		if index == 0 {
			card.HeaderLabel = fmt.Sprintf("%s · %02d:00", strings.TrimSpace(found.Name), reading.first.hour)
			card.Icon = reading.first.condition
			card.Night = reading.first.night
			card.Temperature = reading.first.temperature
			card.FeelsLike = reading.first.apparent
			fmt.Fprintf(&report, "Header: %s, %s, %.1f °C (feels like %.1f °C).\nLegs:\n",
				card.HeaderLabel, reading.first.condition, reading.first.temperature, reading.first.apparent)
		}
		fmt.Fprintf(&report, "- %q, %s, %02d:00 to %02d:00 in %s: %s, average %.1f °C, %.1f mm precipitation",
			label, leg.window.date.Format("Mon 2 Jan"), leg.window.from, leg.window.to, placeName(found),
			reading.condition, reading.temperature, reading.precipitation)
		if reading.hasGust {
			fmt.Fprintf(&report, ", gusts up to %.0f km/h", reading.gust)
		}
		fmt.Fprintf(&report, "; the tile reads %q.\n", detail)
	}

	png, err := daycard.Render(card)
	if err != nil {
		return tools.Result{}, fmt.Errorf("render day card: %w", err)
	}
	if err := s.cards.Stage(principalID, turn, png); err != nil {
		return tools.Result{}, fmt.Errorf("stage day card: %w", err)
	}
	s.logger.InfoContext(ctx, "web.card.staged", "principal_id", principalID, "legs", len(card.Legs), "bytes", len(png))
	return tools.Result{Text: "The day card is ready and goes out as a photo with your reply, which becomes its caption; a later card in this turn would replace it.\n" +
		report.String() +
		"The card already shows these numbers: keep the reply to about 800 characters and do not repeat them."}, nil
}

func (s *Service) parseCardLeg(requested cardLegRequest) (cardLeg, error) {
	leg := cardLeg{label: strings.TrimSpace(requested.Label), place: strings.TrimSpace(requested.Place)}
	if len([]rune(leg.label)) > maximumLabelRunes {
		return cardLeg{}, fmt.Errorf("label must be at most %d characters", maximumLabelRunes)
	}
	if leg.place == "" {
		leg.place = s.weatherHome
	}
	if leg.place == "" {
		return cardLeg{}, errors.New("no place was given and no home place is configured; ask the user where they will be")
	}
	if len([]rune(leg.place)) > maximumPlaceRunes {
		return cardLeg{}, fmt.Errorf("place must be at most %d characters", maximumPlaceRunes)
	}
	if requested.Date == "" || requested.FromHour == nil || requested.ToHour == nil {
		return cardLeg{}, errors.New("give the day as date, YYYY-MM-DD, and the hours as from_hour and to_hour")
	}
	window, _, err := parseHourWindow(weatherRequest{Date: requested.Date, FromHour: requested.FromHour, ToHour: requested.ToHour})
	if err != nil {
		return cardLeg{}, err
	}
	leg.window = window
	return leg, nil
}

// readLeg reduces the leg's hours to the worst condition, the average temperature, the total
// precipitation, and the strongest gust. An hour counts as night when its middle falls outside
// the day's sunrise and sunset, and the leg is night when most of its hours are.
func readLeg(result forecast, window hourWindow) (legReading, error) {
	if err := checkHourly(result); err != nil {
		return legReading{}, err
	}
	sunrise, sunset, daylight := daylightOn(result, window.date)
	hourly := result.Hourly
	var reading legReading
	var temperatureSum float64
	temperatures, conditions, nights := 0, 0, 0
	for index, value := range hourly.Time {
		at, err := time.Parse("2006-01-02T15:04", value)
		if err != nil {
			return legReading{}, errWeatherMalformed
		}
		if !sameDay(at, window.date) || at.Hour() < window.from || at.Hour() > window.to {
			continue
		}
		middle := at.Add(30 * time.Minute)
		night := daylight && (middle.Before(sunrise) || !middle.Before(sunset))
		if night {
			nights++
		}
		condition := daycard.Cloudy
		if code := hourly.Code[index]; code != nil {
			condition = daycard.FromWMO(int(*code))
			if conditions == 0 || severity[condition] > severity[reading.condition] {
				reading.condition = condition
			}
			conditions++
		}
		temperature := hourly.Temperature[index]
		if temperature == nil {
			return legReading{}, errWeatherMalformed
		}
		if reading.hours == 0 {
			apparent := hourly.Apparent[index]
			if apparent == nil {
				return legReading{}, errWeatherMalformed
			}
			reading.first.hour = at.Hour()
			reading.first.condition = condition
			reading.first.night = night
			reading.first.temperature = *temperature
			reading.first.apparent = *apparent
		}
		temperatureSum += *temperature
		temperatures++
		if rain := hourly.Rain[index]; rain != nil {
			reading.precipitation += *rain
		}
		if gust := hourly.Gusts[index]; gust != nil && (!reading.hasGust || *gust > reading.gust) {
			reading.gust, reading.hasGust = *gust, true
		}
		reading.hours++
	}
	if reading.hours == 0 || conditions == 0 {
		return legReading{}, errWeatherMalformed
	}
	reading.temperature = temperatureSum / float64(temperatures)
	reading.night = nights*2 > reading.hours
	return reading, nil
}

// daylightOn finds the date's sunrise and sunset; without them every hour counts as day.
func daylightOn(result forecast, date time.Time) (time.Time, time.Time, bool) {
	daily := result.Daily
	if daily == nil || len(daily.Sunrise) != len(daily.Time) || len(daily.Sunset) != len(daily.Time) {
		return time.Time{}, time.Time{}, false
	}
	for index, day := range daily.Time {
		if day != date.Format(time.DateOnly) || daily.Sunrise[index] == nil || daily.Sunset[index] == nil {
			continue
		}
		sunrise, errRise := time.Parse("2006-01-02T15:04", *daily.Sunrise[index])
		sunset, errSet := time.Parse("2006-01-02T15:04", *daily.Sunset[index])
		if errRise == nil && errSet == nil {
			return sunrise, sunset, true
		}
	}
	return time.Time{}, time.Time{}, false
}

func (r legReading) detail(words cardWords) string {
	rain := math.Round(r.precipitation*10) / 10
	text := words.dry
	if rain > 0 {
		text = fmt.Sprintf("%.1f %s", rain, words.millimetres)
	}
	if r.hasGust && math.Round(r.gust) >= notableGust {
		text += fmt.Sprintf(" · %s %.0f", words.gusts, r.gust)
	}
	return text
}

package web

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/daycard"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	maximumTitleRunes   = 100
	maximumSummaryRunes = 160
	maximumLabelRunes   = 40

	maximumTrains          = 3
	maximumTrainLabelRunes = 20
	// trainTimeout bounds the train statuses well inside cardTimeout, so a slow NS turns into a
	// refusal the model can act on rather than the broker's timeout.
	trainTimeout = 20 * time.Second

	daylightFields = "sunrise,sunset"
	// notableGust is the gust speed, in km/h, from which a leg's tile mentions the wind.
	notableGust = 40

	noTurnText = "A card can only be attached during a conversation turn."

	trainsUnavailableText = "Train status is not available for this card; call weather_card again without trains and describe the trains in the reply."
)

type cardRequest struct {
	Title    string             `json:"title"`
	Summary  string             `json:"summary"`
	Language string             `json:"language"`
	WindUnit string             `json:"wind_unit"`
	Legs     []cardLegRequest   `json:"legs"`
	Trains   []cardTrainRequest `json:"trains"`
}

type cardTrainRequest struct {
	Label    string `json:"label"`
	From     string `json:"from"`
	To       string `json:"to"`
	DateTime string `json:"date_time"`
	ArriveBy bool   `json:"arrive_by"`
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
	wind                               map[windUnit]string
	// minutesLate is a format for a train's minutes late, such as "+%d min". disruption is the
	// milder word for a slower train, disrupted the one for a red row.
	onTime, minutesLate, cancelled, bus, noConnection, disruption, disrupted, noTrain string
}

var cardLanguages = map[string]cardWords{
	"en": {feelsLike: "feels like", millimetres: "mm", gusts: "gusts", dry: "dry", wind: windSymbols,
		onTime: "on time", minutesLate: "+%d min", cancelled: "cancelled", bus: "bus", noConnection: "no connection",
		disruption: "disruption", disrupted: "disrupted", noTrain: "no train"},
	"ru": {feelsLike: "ощущается", millimetres: "мм", gusts: "порывы", dry: "сухо",
		wind:   map[windUnit]string{windKilometres: "км/ч", windMetres: "м/с", windKnots: "уз", windBeaufort: "Бфт"},
		onTime: "вовремя", minutesLate: "+%d мин", cancelled: "отменён", bus: "автобус", noConnection: "нет пересадки",
		disruption: "помехи", disrupted: "сбой", noTrain: "нет поезда"},
}

var trainLevels = map[daycard.TrainStatus]string{
	daycard.TrainOnTime:    "green",
	daycard.TrainSlower:    "yellow",
	daycard.TrainDisrupted: "red",
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
	unit, err := parseWindUnit(request.WindUnit)
	if err != nil {
		return errorResult(err.Error()), nil
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
	if len(request.Trains) > 0 && s.trains == nil {
		return errorResult(trainsUnavailableText), nil
	}
	if len(request.Trains) > maximumTrains {
		return errorResult(fmt.Sprintf("A card shows at most %d trains.", maximumTrains)), nil
	}
	queries := make([]TrainQuery, 0, len(request.Trains))
	for index, requested := range request.Trains {
		query, err := parseCardTrain(requested)
		if err != nil {
			return errorResult(fmt.Sprintf("Train %d: %s", index+1, err)), nil
		}
		queries = append(queries, query)
	}

	var trains <-chan trainOutcome
	if len(queries) > 0 {
		trainCtx, cancel := context.WithTimeout(ctx, trainTimeout)
		defer cancel()
		outcome := make(chan trainOutcome, 1)
		go func() { outcome <- s.trainReports(trainCtx, principalID, queries) }()
		trains = outcome
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
		detail := reading.detail(words, unit)
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
			fmt.Fprintf(&report, ", gusts up to %s", unit.format(reading.gust))
		}
		fmt.Fprintf(&report, "; the tile reads %q.\n", detail)
	}

	var reports []TrainReport
	if trains != nil {
		outcome := <-trains
		if outcome.err != nil {
			var refusal *TrainRefusal
			if !errors.As(outcome.err, &refusal) {
				return tools.Result{}, outcome.err
			}
			text := fmt.Sprintf("Train %d: %s", outcome.failed+1, refusal.Text)
			if refusal.Unavailable {
				text = strings.TrimSuffix(text, ".") + "; call weather_card again without trains."
			}
			return errorResult(text), nil
		}
		reports = outcome.reports
		report.WriteString("Trains:\n")
	}
	for index, train := range reports {
		row := daycard.Train{
			Label:  strings.TrimSpace(request.Trains[index].Label),
			Status: train.Level,
			Badge:  trainBadge(train, words),
			Times:  trainTimes(train),
		}
		card.Trains = append(card.Trains, row)
		fmt.Fprintf(&report, "- %q, %s → %s: %s; the row reads %q", row.Label, train.From, train.To, trainLevels[row.Status], row.Badge)
		if row.Times != "" {
			fmt.Fprintf(&report, " and %q", row.Times)
		}
		switch {
		case !train.Found:
		case train.Transfers == 0:
			report.WriteString(", direct")
		case train.Transfers == 1:
			report.WriteString(", 1 transfer")
		default:
			fmt.Fprintf(&report, ", %d transfers", train.Transfers)
		}
		fmt.Fprintf(&report, "; %s.\n", train.Reason)
	}

	renderStarted := time.Now()
	png, err := daycard.Render(card)
	if s.telemetry != nil {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		s.telemetry.RecordDayCardRender(ctx, outcome, time.Since(renderStarted))
	}
	if err != nil {
		return tools.Result{}, fmt.Errorf("render day card: %w", err)
	}
	if err := s.cards.Stage(principalID, turn, png); err != nil {
		return tools.Result{}, fmt.Errorf("stage day card: %w", err)
	}
	s.logger.InfoContext(ctx, "web.card.staged", "principal_id", principalID, "legs", len(card.Legs), "trains", len(card.Trains), "bytes", len(png))
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

func (r legReading) detail(words cardWords, unit windUnit) string {
	rain := math.Round(r.precipitation*10) / 10
	text := words.dry
	if rain > 0 {
		text = fmt.Sprintf("%.1f %s", rain, words.millimetres)
	}
	if r.hasGust && math.Round(r.gust) >= notableGust {
		text += fmt.Sprintf(" · %s %s", words.gusts, unit.formatWith(r.gust, words.wind))
	}
	return text
}

func parseCardTrain(requested cardTrainRequest) (TrainQuery, error) {
	label := strings.TrimSpace(requested.Label)
	if label == "" {
		return TrainQuery{}, errors.New("give the row a label, such as There or Back")
	}
	if len([]rune(label)) > maximumTrainLabelRunes {
		return TrainQuery{}, fmt.Errorf("label must be at most %d characters", maximumTrainLabelRunes)
	}
	query := TrainQuery{From: strings.TrimSpace(requested.From), To: strings.TrimSpace(requested.To), ArriveBy: requested.ArriveBy}
	if query.From == "" || query.To == "" {
		return TrainQuery{}, errors.New("give the from and to stations")
	}
	at, err := time.Parse("2006-01-02T15:04", strings.TrimSpace(requested.DateTime))
	if err != nil {
		return TrainQuery{}, errors.New("date_time must be Dutch local time written as YYYY-MM-DDTHH:MM, such as 2026-09-29T08:10")
	}
	query.At = at
	return query, nil
}

// trainOutcome is every train's report, or the first failure in the card's order with its index.
type trainOutcome struct {
	reports []TrainReport
	failed  int
	err     error
}

// trainReports asks for every train at once, so the slowest one bounds the wait.
func (s *Service) trainReports(ctx context.Context, principalID string, queries []TrainQuery) trainOutcome {
	reports := make([]TrainReport, len(queries))
	failures := make([]error, len(queries))
	var group sync.WaitGroup
	for index, query := range queries {
		group.Go(func() {
			reports[index], failures[index] = s.trains.TripStatus(ctx, principalID, query)
		})
	}
	group.Wait()
	for index, err := range failures {
		if err != nil {
			return trainOutcome{failed: index, err: err}
		}
	}
	return trainOutcome{reports: reports}
}

// trainBadge names the dominant cause, so a row that is red for a cancelled leg never reads as a
// few minutes late.
func trainBadge(report TrainReport, words cardWords) string {
	switch report.Cause {
	case TrainCauseNoTrip:
		return words.noTrain
	case TrainCauseCancelled:
		return words.cancelled
	case TrainCauseAlternative:
		return words.bus
	case TrainCauseMissedConnection:
		return words.noConnection
	case TrainCauseDelay, TrainCauseLonger:
		if minutes := max(report.DelayMinutes, report.ExtraMinutes); minutes > 0 {
			return fmt.Sprintf(words.minutesLate, minutes)
		}
	}
	switch report.Level {
	case daycard.TrainOnTime:
		return words.onTime
	case daycard.TrainSlower:
		return words.disruption
	default:
		return words.disrupted
	}
}

// trainTimes is the live departure → arrival where NS has live times, else the planned ones; the
// pill beside it already says how late that is, and a card row has no room for both spans. A
// cancelled train shows its plan.
func trainTimes(report TrainReport) string {
	if !report.Found || report.PlannedDeparture.IsZero() || report.PlannedArrival.IsZero() {
		return ""
	}
	span := func(departure, arrival time.Time) string {
		return departure.Format("15:04") + " → " + arrival.Format("15:04")
	}
	planned := span(report.PlannedDeparture, report.PlannedArrival)
	if report.Cancelled {
		return planned
	}
	departure, arrival := report.PlannedDeparture, report.PlannedArrival
	if !report.ActualDeparture.IsZero() {
		departure = report.ActualDeparture
	}
	if !report.ActualArrival.IsZero() {
		arrival = report.ActualArrival
	}
	return span(departure, arrival)
}

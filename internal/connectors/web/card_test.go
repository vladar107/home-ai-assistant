package web_test

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
	"github.com/vladar107/home-ai-assistant/internal/daycard"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const turnID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

const geocodingZwolle = `{"results":[{"id":2743477,"name":"Zwolle","latitude":52.5125,"longitude":6.09444,` +
	`"timezone":"Europe/Amsterdam","country":"Netherlands","admin1":"Overijssel"}]}`

// hourlyZwolle is a cold, overcast 2026-09-28 with a wet evening: light rain from 17:00 to
// 20:00, a thunderstorm at 18:00 and snow at 19:00.
func hourlyZwolle() string {
	var times, temperatures, rain, codes, gusts []string
	for hour := range 24 {
		times = append(times, fmt.Sprintf(`"2026-09-28T%02d:00"`, hour))
		temperatures = append(temperatures, "5.0")
		code, amount := 3, 0.0
		switch {
		case hour == 18:
			code, amount = 95, 0.5
		case hour == 19:
			code, amount = 71, 0.5
		case hour == 17 || hour == 20:
			code, amount = 61, 0.5
		}
		rain = append(rain, fmt.Sprintf("%.1f", amount))
		codes = append(codes, fmt.Sprint(code))
		gusts = append(gusts, "30")
	}
	join := func(values []string) string { return "[" + strings.Join(values, ",") + "]" }
	return `{"timezone":"Europe/Amsterdam","hourly":{"time":` + join(times) +
		`,"temperature_2m":` + join(temperatures) + `,"apparent_temperature":` + join(temperatures) +
		`,"precipitation_probability":` + join(rain) + `,"precipitation":` + join(rain) +
		`,"weather_code":` + join(codes) + `,"wind_speed_10m":` + join(gusts) + `,"wind_gusts_10m":` + join(gusts) + `},` +
		`"daily":{"time":["2026-09-28"],"sunrise":["2026-09-28T07:32"],"sunset":["2026-09-28T19:14"]}}`
}

type cardSimulator struct {
	mu        sync.Mutex
	geocoded  []string
	forecasts int
}

// openMeteoDay answers the card's requests: hourly fields plus the day's sunrise and sunset, for
// Amsterdam and Zwolle on 2026-09-28.
func (f *fixture) openMeteoDay() *cardSimulator {
	simulator := &cardSimulator{}
	f.route("geocoding.test/v1/search", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		simulator.mu.Lock()
		simulator.geocoded = append(simulator.geocoded, name)
		simulator.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch name {
		case "Amsterdam":
			_, _ = io.WriteString(w, geocodingAmsterdam)
		case "Zwolle":
			_, _ = io.WriteString(w, geocodingZwolle)
		default:
			_, _ = io.WriteString(w, `{"generationtime_ms":0.2}`)
		}
	})
	f.route("forecast.test/v1/forecast", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("hourly") != "temperature_2m,apparent_temperature,precipitation_probability,precipitation,weather_code,wind_speed_10m,wind_gusts_10m" ||
			query.Get("daily") != "sunrise,sunset" || query.Get("timezone") != "auto" ||
			query.Get("start_date") != "2026-09-28" || query.Get("end_date") != "2026-09-28" || query.Has("current") {
			http.Error(w, "unexpected query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		simulator.mu.Lock()
		simulator.forecasts++
		simulator.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch query.Get("latitude") {
		case "52.3740":
			body := hourlyAmsterdam()
			_, _ = io.WriteString(w, strings.TrimSuffix(body, "}")+
				`,"daily":{"time":["2026-09-28"],"sunrise":["2026-09-28T07:33"],"sunset":["2026-09-28T19:15"]}}`)
		case "52.5125":
			_, _ = io.WriteString(w, hourlyZwolle())
		default:
			http.Error(w, "unknown place", http.StatusBadRequest)
		}
	})
	return simulator
}

func newCardFixture(t *testing.T) (*fixture, *outbox.Box) {
	t.Helper()
	box, err := outbox.New(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	return newFixtureWith(t, web.Config{Cards: box}), box
}

// callInTurn runs the card tool through a broker turn that carries the turn key, as the
// conversation and the scheduler set it.
func (f *fixture) callInTurn(turn string, arguments any) agentruntime.ToolResult {
	f.t.Helper()
	set, err := f.broker.ToolsFor(tools.WithTurn(f.ctx, turn), "alice")
	if err != nil {
		f.t.Fatal(err)
	}
	return f.callWith(set, "weather_card", arguments)
}

func stagedCard(t *testing.T, box *outbox.Box, turn string) []byte {
	t.Helper()
	card, found, err := box.Card("alice", turn)
	if err != nil || !found || len(card.PNG) == 0 {
		t.Fatalf("staged card = %v %v %v", len(card.PNG), found, err)
	}
	decoded, err := png.Decode(bytes.NewReader(card.PNG))
	if err != nil {
		t.Fatal(err)
	}
	if width := decoded.Bounds().Dx(); width != daycard.Width {
		t.Fatalf("card is %d px wide, want %d", width, daycard.Width)
	}
	return card.PNG
}

func TestWeatherCardStagesTheDayForTheTurn(t *testing.T) {
	f, box := newCardFixture(t)
	simulator := f.openMeteoDay()
	turn := outbox.ChatTurn(turnID)

	set := f.toolSet("alice")
	if got := toolNames(set); !slices.Equal(got, []string{"fetch_page", "weather_forecast", "weather_card"}) {
		t.Fatalf("tools = %v", got)
	}
	for _, fragment := range []string{"weather_card", "about 800 characters", "never repeat the per-leg numbers"} {
		if !strings.Contains(set.Guidance, fragment) {
			t.Fatalf("guidance lacks %q: %s", fragment, set.Guidance)
		}
	}
	contribution, err := f.service.Contribute(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range contribution.Definitions {
		if definition.Name == "weather_card" && (definition.Risk != tools.RiskRead || definition.Timeout != 45*time.Second) {
			t.Fatalf("weather_card definition = %+v", definition)
		}
	}

	result := f.callInTurn(turn, map[string]any{
		"title":   "Mon 28.09 · Amsterdam → Zwolle",
		"summary": "Dry morning, a stormy evening.",
		"legs": []map[string]any{
			{"place": "Amsterdam", "date": "2026-09-28", "from_hour": 6, "to_hour": 9},
			{"label": "Evening", "place": "Zwolle", "date": "2026-09-28", "from_hour": 17, "to_hour": 21},
		},
	})
	wantText(t, result,
		"goes out as a photo with your reply",
		"Header: Amsterdam · 06:00, partly cloudy, 11.0 °C (feels like 9.0 °C).",
		`- "Amsterdam · 06–09", Mon 28 Sep, 06:00 to 09:00 in Amsterdam, North Holland, Netherlands: partly cloudy, average 11.5 °C, 0.0 mm precipitation, gusts up to 38 km/h; the tile reads "dry".`,
		`- "Evening", Mon 28 Sep, 17:00 to 21:00 in Zwolle, Overijssel, Netherlands: thunderstorm, average 5.0 °C, 2.0 mm precipitation, gusts up to 30 km/h; the tile reads "2.0 mm".`,
		"do not repeat them",
	)
	stagedCard(t, box, turn)
	if !slices.Equal(simulator.geocoded, []string{"Amsterdam", "Zwolle"}) || simulator.forecasts != 2 {
		t.Fatalf("geocoded %v with %d forecasts", simulator.geocoded, simulator.forecasts)
	}
	if !strings.Contains(f.logs.String(), "web.card.staged") {
		t.Fatalf("logs lack the staging event:\n%s", f.logs.String())
	}
}

func TestWeatherCardAggregatesLegsInRussian(t *testing.T) {
	f, box := newCardFixture(t)
	simulator := f.openMeteoDay()
	turn := outbox.ScheduledTurn(turnID)

	result := f.callInTurn(turn, map[string]any{
		"title":    "Пн 28.09 · Амстердам",
		"summary":  "Днём дождь, вечером ветрено.",
		"language": "ru",
		"legs": []map[string]any{
			{"label": "Утро", "place": "Amsterdam", "date": "2026-09-28", "from_hour": 6, "to_hour": 9},
			{"label": "День", "place": "amsterdam ", "date": "2026-09-28", "from_hour": 10, "to_hour": 16},
		},
	})
	wantText(t, result,
		`- "Утро", Mon 28 Sep, 06:00 to 09:00 in Amsterdam, North Holland, Netherlands: partly cloudy, average 11.5 °C, 0.0 mm precipitation, gusts up to 38 km/h; the tile reads "сухо".`,
		`- "День", Mon 28 Sep, 10:00 to 16:00 in Amsterdam, North Holland, Netherlands: rain, average 13.3 °C, 6.0 mm precipitation, gusts up to 52 km/h; the tile reads "6.0 мм · порывы 52 км/ч".`,
	)
	stagedCard(t, box, turn)
	if len(simulator.geocoded) != 1 || simulator.forecasts != 1 {
		t.Fatalf("one place and day were fetched as %v with %d forecasts", simulator.geocoded, simulator.forecasts)
	}
}

func TestWeatherCardRefusals(t *testing.T) {
	f, box := newCardFixture(t)
	f.openMeteoDay()
	turn := outbox.ChatTurn(turnID)
	leg := map[string]any{"place": "Amsterdam", "date": "2026-09-28", "from_hour": 8, "to_hour": 9}
	card := func(legs ...map[string]any) map[string]any {
		return map[string]any{"title": "Today", "summary": "Mild.", "legs": legs}
	}
	with := func(key string, value any) map[string]any {
		changed := map[string]any{}
		for k, v := range leg {
			changed[k] = v
		}
		changed[key] = value
		return changed
	}

	for _, tc := range []struct {
		name      string
		arguments map[string]any
		want      string
	}{
		{"no legs", card(), "1 to 4 legs"},
		{"five legs", card(leg, leg, leg, leg, leg), "1 to 4 legs"},
		{"hours reversed", card(leg, with("from_hour", 10)), "Leg 2: from_hour must not be later than to_hour"},
		{"hour out of range", card(with("to_hour", 24)), "Leg 1: from_hour and to_hour must be hours from 0 to 23"},
		{"no hours", card(map[string]any{"place": "Amsterdam", "date": "2026-09-28"}), "Leg 1: give the day as date"},
		{"bad date", card(with("date", "28-09-2026")), "date must be a calendar day written as YYYY-MM-DD"},
		{"date too far", card(with("date", "2026-10-13")), "Leg 1: date must be between 2026-09-27 and 2026-10-12"},
		{"no place", card(with("place", "")), "no home place is configured"},
		{"unknown place", card(with("place", "Atlantis")), `no place called "Atlantis" was found`},
		{"no summary", map[string]any{"title": "Today", "legs": []any{leg}}, "a title and a one-line summary"},
		{"unknown language", map[string]any{"title": "Today", "summary": "Mild.", "language": "nl", "legs": []any{leg}}, `language must be "en" or "ru"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, f.callInTurn(turn, tc.arguments), tc.want)
		})
	}
	wantError(t, f.call("alice", "weather_card", card(leg)), "A card can only be attached during a conversation turn.")
	if _, found, err := box.Card("alice", turn); err != nil || found {
		t.Fatalf("a refused call staged a card: %v %v", found, err)
	}
}

func TestWeatherCardFollowsTheWebSwitch(t *testing.T) {
	f, _ := newCardFixture(t)
	f.setWeb("alice", false)
	if set := f.toolSet("alice"); !set.Empty() {
		t.Fatalf("tools with web access off = %v", toolNames(set))
	}
	if got := toolNames(f.toolSet("bob")); !slices.Contains(got, "weather_card") {
		t.Fatalf("bob's tools = %v", got)
	}
}

// fakeTrains stands in for the NS connector at the web connector's Trains boundary: it answers
// each direction, keyed by its from station, and records what it was asked.
type fakeTrains struct {
	mu       sync.Mutex
	reports  map[string]transit.TripStatus
	failures map[string]error
	off      map[string]bool
	asked    []string
}

func (f *fakeTrains) Enabled(_ context.Context, principalID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.off[principalID], nil
}

func (f *fakeTrains) TripStatus(_ context.Context, principalID string, query transit.TripQuery) (transit.TripStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, fmt.Sprintf("%s %s→%s %s arrive_by=%t", principalID, query.From, query.To, query.At.Format("2006-01-02 15:04"), query.ArriveBy))
	if err := f.failures[query.From]; err != nil {
		return transit.TripStatus{}, err
	}
	return f.reports[query.From], nil
}

func newTrainsFixture(t *testing.T, trains *fakeTrains) (*fixture, *outbox.Box) {
	t.Helper()
	box, err := outbox.New(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	return newFixtureWith(t, web.Config{Cards: box, Trains: trains}), box
}

func dutch(t *testing.T, clock string) time.Time {
	t.Helper()
	location, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04", "2026-09-28 "+clock, location)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func trainReport(t *testing.T, level transit.Level, planned, actual [2]string) transit.TripStatus {
	t.Helper()
	report := transit.TripStatus{
		Level: level, From: "Den Haag", To: "Zwolle", Found: true, Transfers: 1,
		PlannedDeparture: dutch(t, planned[0]), PlannedArrival: dutch(t, planned[1]),
	}
	if actual[0] != "" {
		report.ActualDeparture, report.ActualArrival = dutch(t, actual[0]), dutch(t, actual[1])
	}
	return report
}

func trainsCard(language string, trains ...map[string]any) map[string]any {
	return map[string]any{
		"title":    "Mon 28.09 · Amsterdam",
		"summary":  "Dry morning.",
		"language": language,
		"legs":     []map[string]any{{"place": "Amsterdam", "date": "2026-09-28", "from_hour": 6, "to_hour": 9}},
		"trains":   trains,
	}
}

func TestWeatherCardShowsTrainStatuses(t *testing.T) {
	onTime := trainReport(t, transit.LevelOnTime, [2]string{"08:10", "09:02"}, [2]string{"08:10", "09:02"})
	onTime.Reason = "on time"
	late := trainReport(t, transit.LevelSlower, [2]string{"08:10", "09:02"}, [2]string{"08:22", "09:14"})
	late.From = "Dordrecht"
	late.Cause, late.DelayMinutes, late.Reason = transit.CauseDelay, 12, "12 min late; Disruption: fewer trains between Dordrecht and Utrecht"
	cancelled := trainReport(t, transit.LevelDisrupted, [2]string{"17:31", "18:20"}, [2]string{"17:35", "18:24"})
	cancelled.From, cancelled.To = "Zwolle", "Den Haag"
	cancelled.Cause, cancelled.Cancelled, cancelled.DelayMinutes, cancelled.Transfers, cancelled.Reason = transit.CauseCancelled, true, 4, 0, "IC 3027 cancelled; 4 min late"
	longer := trainReport(t, transit.LevelSlower, [2]string{"08:10", "09:02"}, [2]string{"08:10", "09:08"})
	longer.Cause, longer.ExtraMinutes, longer.Transfers, longer.Reason = transit.CauseLonger, 6, 2, "6 min longer than planned"
	bus := transit.TripStatus{Level: transit.LevelDisrupted, Cause: transit.CauseAlternative, From: "Dordrecht", To: "Utrecht Centraal", Found: true,
		PlannedDeparture: dutch(t, "07:40"), PlannedArrival: dutch(t, "07:47"), DelayMinutes: 3, Reason: "alternative transport; 3 min late"}
	missing := transit.TripStatus{Level: transit.LevelDisrupted, Cause: transit.CauseNoTrip, From: "Zwolle", To: "Den Haag", Reason: "no trip found"}
	unreachable := trainReport(t, transit.LevelDisrupted, [2]string{"08:10", "09:02"}, [2]string{"08:13", "09:05"})
	unreachable.Cause, unreachable.DelayMinutes, unreachable.Reason = transit.CauseMissedConnection, 3, "a connection cannot be made; 3 min late"
	troubled := trainReport(t, transit.LevelSlower, [2]string{"08:10", "09:02"}, [2]string{"08:10", "09:02"})
	troubled.From, troubled.Cause, troubled.Reason = "Dordrecht", transit.CauseDisruption, "Disruption: fewer trains"
	delayed := trainReport(t, transit.LevelDisrupted, [2]string{"17:31", "18:20"}, [2]string{"17:31", "18:40"})
	delayed.From, delayed.To = "Zwolle", "Den Haag"
	delayed.Cause, delayed.DelayMinutes, delayed.Reason = transit.CauseDelay, 20, "20 min late"

	for _, test := range []struct {
		name     string
		language string
		reports  map[string]transit.TripStatus
		want     []string
	}{
		{
			name:     "english",
			language: "en",
			reports:  map[string]transit.TripStatus{"Den Haag": onTime, "Dordrecht": late, "Zwolle": cancelled},
			want: []string{
				"Trains:\n",
				`- "There", Den Haag → Zwolle: green; the row reads "on time" and "08:10 → 09:02", 1 transfer; on time.`,
				`- "Via Dordrecht", Dordrecht → Zwolle: yellow; the row reads "+12 min" and "08:22 → 09:14", 1 transfer; 12 min late; Disruption: fewer trains between Dordrecht and Utrecht.`,
				`- "Back", Zwolle → Den Haag: red; the row reads "cancelled" and "17:31 → 18:20", direct; IC 3027 cancelled; 4 min late.`,
			},
		},
		{
			name:     "russian",
			language: "ru",
			reports:  map[string]transit.TripStatus{"Den Haag": longer, "Dordrecht": bus, "Zwolle": missing},
			want: []string{
				`- "There", Den Haag → Zwolle: yellow; the row reads "+6 мин" and "08:10 → 09:08", 2 transfers; 6 min longer than planned.`,
				`- "Via Dordrecht", Dordrecht → Utrecht Centraal: red; the row reads "автобус" and "07:40 → 07:47", direct; alternative transport; 3 min late.`,
				`- "Back", Zwolle → Den Haag: red; the row reads "нет поезда"; no trip found.`,
			},
		},
		{
			name:     "connection, disruption, and delay",
			language: "en",
			reports:  map[string]transit.TripStatus{"Den Haag": unreachable, "Dordrecht": troubled, "Zwolle": delayed},
			want: []string{
				`- "There", Den Haag → Zwolle: red; the row reads "no connection" and "08:13 → 09:05", 1 transfer; a connection cannot be made; 3 min late.`,
				`- "Via Dordrecht", Dordrecht → Zwolle: yellow; the row reads "disruption" and "08:10 → 09:02", 1 transfer; Disruption: fewer trains.`,
				`- "Back", Zwolle → Den Haag: red; the row reads "+20 min" and "17:31 → 18:40", 1 transfer; 20 min late.`,
			},
		},
		{
			name:     "russian disruption",
			language: "ru",
			reports:  map[string]transit.TripStatus{"Den Haag": onTime, "Dordrecht": troubled, "Zwolle": cancelled},
			want: []string{
				`- "Via Dordrecht", Dordrecht → Zwolle: yellow; the row reads "помехи" and "08:10 → 09:02", 1 transfer; Disruption: fewer trains.`,
				`- "Back", Zwolle → Den Haag: red; the row reads "отменён" and "17:31 → 18:20", direct; IC 3027 cancelled; 4 min late.`,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			trains := &fakeTrains{reports: test.reports}
			f, box := newTrainsFixture(t, trains)
			f.openMeteoDay()
			turn := outbox.ChatTurn(turnID)

			set, err := f.broker.ToolsFor(tools.WithTurn(f.ctx, turn), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(set.Guidance, "trains entry for each travel direction") {
				t.Fatalf("guidance lacks trains: %s", set.Guidance)
			}
			result := f.callWith(set, "weather_card", trainsCard(test.language,
				map[string]any{"label": "There", "from": "Den Haag", "to": "Zwolle", "date_time": "2026-09-28T08:10"},
				map[string]any{"label": "Via Dordrecht", "from": "Dordrecht", "to": "Zwolle", "date_time": "2026-09-28T08:05"},
				map[string]any{"label": "Back", "from": "Zwolle", "to": "Den Haag", "date_time": "2026-09-28T18:30", "arrive_by": true},
			))
			wantText(t, result, test.want...)
			stagedCard(t, box, turn)
			slices.Sort(trains.asked)
			if want := []string{
				"alice Den Haag→Zwolle 2026-09-28 08:10 arrive_by=false",
				"alice Dordrecht→Zwolle 2026-09-28 08:05 arrive_by=false",
				"alice Zwolle→Den Haag 2026-09-28 18:30 arrive_by=true",
			}; !slices.Equal(trains.asked, want) {
				t.Fatalf("asked %v", trains.asked)
			}
			if !strings.Contains(f.logs.String(), "trains=3") {
				t.Fatalf("logs lack the train count:\n%s", f.logs.String())
			}
		})
	}
}

func TestWeatherCardOffersTrainsOnlyWithTrainInformationOn(t *testing.T) {
	f, _ := newTrainsFixture(t, &fakeTrains{off: map[string]bool{"alice": true}})
	offersTrains := func(principalID string) bool {
		t.Helper()
		contribution, err := f.service.Contribute(f.ctx, principalID)
		if err != nil {
			t.Fatal(err)
		}
		schema := false
		for _, definition := range contribution.Definitions {
			if definition.Name == "weather_card" {
				schema = strings.Contains(string(definition.InputSchema), `"trains"`)
			}
		}
		if guidance := strings.Contains(contribution.Guidance, "trains entry"); guidance != schema {
			t.Fatalf("%s: trains in guidance %t but in schema %t", principalID, guidance, schema)
		}
		return schema
	}
	if offersTrains("alice") {
		t.Fatal("weather_card offers trains to alice with her train information off")
	}
	if !offersTrains("bob") {
		t.Fatal("weather_card does not offer trains to bob")
	}
}

func TestWeatherCardTrainRefusals(t *testing.T) {
	train := map[string]any{"label": "There", "from": "Den Haag", "to": "Zwolle", "date_time": "2026-09-28T08:10"}
	with := func(key string, value any) map[string]any {
		changed := maps.Clone(train)
		changed[key] = value
		return changed
	}
	turn := outbox.ChatTurn(turnID)

	withoutTrains, _ := newCardFixture(t)
	withoutTrains.openMeteoDay()
	contribution, err := withoutTrains.service.Contribute(withoutTrains.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range contribution.Definitions {
		if definition.Name == "weather_card" && strings.Contains(string(definition.InputSchema), "trains") {
			t.Fatalf("weather_card offers trains without a train source: %s", definition.InputSchema)
		}
	}
	if strings.Contains(contribution.Guidance, "trains entry") {
		t.Fatalf("guidance offers trains without a train source: %s", contribution.Guidance)
	}
	wantError(t, withoutTrains.callInTurn(turn, trainsCard("en", train)), "Train status is not available")

	trains := &fakeTrains{}
	f, box := newTrainsFixture(t, trains)
	f.openMeteoDay()
	for _, tc := range []struct {
		name   string
		trains []map[string]any
		want   string
	}{
		{"four trains", []map[string]any{train, train, train, train}, "at most 3 trains"},
		{"no label", []map[string]any{with("label", " ")}, "Train 1: give the row a label"},
		{"long label", []map[string]any{train, with("label", strings.Repeat("я", 21))}, "Train 2: label must be at most 20 characters"},
		{"no station", []map[string]any{with("to", "")}, "Train 1: give the from and to stations"},
		{"bad time", []map[string]any{with("date_time", "tomorrow 8")}, "Train 1: date_time must be Dutch local time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, f.callInTurn(turn, trainsCard("en", tc.trains...)), tc.want)
		})
	}
	if len(trains.asked) != 0 {
		t.Fatalf("invalid trains were asked about: %v", trains.asked)
	}

	back := map[string]any{"label": "Back", "from": "Zwolle", "to": "Den Haag", "date_time": "2026-09-28T18:30"}
	trains.failures = map[string]error{"Zwolle": &transit.RefusalError{Text: `No NS station matches to "Zwolle".`}}
	wantError(t, f.callInTurn(turn, trainsCard("en", train, back)), `Train 2: No NS station matches to "Zwolle".`)
	trains.failures = map[string]error{"Den Haag": &transit.RefusalError{Text: "Train information is off for this user; they can turn it on in /settings.", Unavailable: true}}
	wantError(t, f.callInTurn(turn, trainsCard("en", back, train)),
		"Train 2: Train information is off for this user; they can turn it on in /settings; call weather_card again without trains.")
	if _, found, err := box.Card("alice", turn); err != nil || found {
		t.Fatalf("a refused call staged a card: %v %v", found, err)
	}
}

func TestWeatherCardPrintsWindInTheAskedUnit(t *testing.T) {
	f, box := newCardFixture(t)
	f.openMeteoDay()
	turn := outbox.ScheduledTurn(turnID)

	result := f.callInTurn(turn, map[string]any{
		"title":     "Mon 28.09 · Amsterdam",
		"summary":   "Rain and gusts in the afternoon.",
		"wind_unit": "kn",
		"legs":      []map[string]any{{"label": "Day", "place": "Amsterdam", "date": "2026-09-28", "from_hour": 10, "to_hour": 16}},
	})
	wantText(t, result, `6.0 mm precipitation, gusts up to 28 kn; the tile reads "6.0 mm · gusts 28 kn".`)
	stagedCard(t, box, turn)
}

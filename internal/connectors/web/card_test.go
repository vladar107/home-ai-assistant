package web_test

import (
	"bytes"
	"fmt"
	"image/png"
	"io"
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
)

const turnID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

const geocodingUtrecht = `{"results":[{"id":2745912,"name":"Utrecht","latitude":52.09083,"longitude":5.12222,` +
	`"timezone":"Europe/Amsterdam","country":"Netherlands","admin1":"Utrecht"}]}`

// hourlyUtrecht is a cold, overcast 2026-09-28 with a wet evening: light rain from 17:00 to
// 20:00, a thunderstorm at 18:00 and snow at 19:00.
func hourlyUtrecht() string {
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
// Amsterdam and Utrecht on 2026-09-28.
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
		case "Utrecht":
			_, _ = io.WriteString(w, geocodingUtrecht)
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
		case "52.0908":
			_, _ = io.WriteString(w, hourlyUtrecht())
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
		"title":   "Mon 28.09 · Amsterdam → Utrecht",
		"summary": "Dry morning, a stormy evening.",
		"legs": []map[string]any{
			{"place": "Amsterdam", "date": "2026-09-28", "from_hour": 6, "to_hour": 9},
			{"label": "Evening", "place": "Utrecht", "date": "2026-09-28", "from_hour": 17, "to_hour": 21},
		},
	})
	wantText(t, result,
		"goes out as a photo with your reply",
		"Header: Amsterdam · 06:00, partly cloudy, 11.0 °C (feels like 9.0 °C).",
		`- "Amsterdam · 06–09", Mon 28 Sep, 06:00 to 09:00 in Amsterdam, North Holland, Netherlands: partly cloudy, average 11.5 °C, 0.0 mm precipitation, gusts up to 38 km/h; the tile reads "dry".`,
		`- "Evening", Mon 28 Sep, 17:00 to 21:00 in Utrecht, Netherlands: thunderstorm, average 5.0 °C, 2.0 mm precipitation, gusts up to 30 km/h; the tile reads "2.0 mm".`,
		"do not repeat them",
	)
	stagedCard(t, box, turn)
	if !slices.Equal(simulator.geocoded, []string{"Amsterdam", "Utrecht"}) || simulator.forecasts != 2 {
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
		`- "День", Mon 28 Sep, 10:00 to 16:00 in Amsterdam, North Holland, Netherlands: rain, average 13.3 °C, 6.0 mm precipitation, gusts up to 52 km/h; the tile reads "6.0 мм · порывы 52".`,
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
	if status, err := f.service.SetEnabled(f.ctx, "alice", false); err != nil || status.Enabled {
		t.Fatalf("disable = %+v %v", status, err)
	}
	if set := f.toolSet("alice"); !set.Empty() {
		t.Fatalf("tools with web access off = %v", toolNames(set))
	}
	if got := toolNames(f.toolSet("bob")); !slices.Contains(got, "weather_card") {
		t.Fatalf("bob's tools = %v", got)
	}
}

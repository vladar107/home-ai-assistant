package weathersim_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
	"github.com/vladar107/home-ai-assistant/internal/testutil/weathersim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const seed = `{
  "places": [{"name": "Eastbrook", "latitude": 52.1, "longitude": 5.1, "country": "Netherlands",
    "country_code": "NL", "admin1": "Westland", "timezone": "Europe/Amsterdam", "population": 250000}],
  "forecasts": [{"latitude": 52.1, "longitude": 5.1, "timezone": "Europe/Amsterdam",
    "current": {"time": "2026-03-10T08:00", "temperature": 7.5, "apparent": 5, "humidity": 80,
      "precipitation": 0, "code": 3, "wind": 18, "gusts": 30},
    "days": [{"date": "2026-03-10", "code": 61, "high": 9, "low": 3, "rain": 4.2, "probability": 70,
      "wind": 25, "sunrise": "2026-03-10T06:45", "sunset": "2026-03-10T18:20"},
      {"date": "2026-03-11", "code": 1, "high": 11, "low": 4, "rain": 0, "probability": 5,
      "wind": 12, "sunrise": "2026-03-11T06:43", "sunset": "2026-03-11T18:22"}],
    "hours": [{"time": "2026-03-10T08:00", "temperature": 7.5, "apparent": 5, "probability": 20,
      "rain": 0, "code": 3, "wind": 18, "gusts": 30},
      {"time": "2026-03-10T09:00", "temperature": 8.5, "apparent": 6, "probability": 80,
      "rain": 1.5, "code": 61, "wind": 20, "gusts": 33}]}]
}`

func service(t *testing.T) *web.Service {
	t.Helper()
	router := simkit.NewRouter()
	geocoding, forecast := weathersim.New(t, json.RawMessage(seed)).Mount(router)
	service, err := web.NewService(web.Config{
		GeocodingURL: geocoding,
		ForecastURL:  forecast,
		Resolver:     router,
		Dial:         router.DialContext,
		Now:          func() time.Time { return time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC) },
	}, simkit.AlwaysOn{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func call(t *testing.T, service *web.Service, arguments string) string {
	t.Helper()
	result, err := service.Execute(t.Context(), "alice", tools.Call{Name: "weather_forecast", Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("weather_forecast failed: %s", result.Text)
	}
	return result.Text
}

func TestDailyForecastComesFromSeed(t *testing.T) {
	text := call(t, service(t), `{"location":"Eastbrook","days":2}`)
	for _, want := range []string{"Eastbrook", "7.5", "70"} {
		if !strings.Contains(text, want) {
			t.Errorf("forecast lacks %q:\n%s", want, text)
		}
	}
}

func TestHourlyForecastComesFromSeed(t *testing.T) {
	text := call(t, service(t), `{"location":"Eastbrook","date":"2026-03-10","from_hour":8,"to_hour":9}`)
	if !strings.Contains(text, "8.5") {
		t.Errorf("hourly forecast lacks the seeded 09:00 temperature:\n%s", text)
	}
}

func TestUnknownVariableIsRefused(t *testing.T) {
	fake := weathersim.New(t, json.RawMessage(seed))
	response, err := http.Get(fake.URL() + weathersim.ForecastPath + "?latitude=52.1&longitude=5.1&daily=bogus")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", response.StatusCode)
	}
}

func TestValidateRejectsUnknownFields(t *testing.T) {
	if err := weathersim.Validate(json.RawMessage(seed)); err != nil {
		t.Fatal(err)
	}
	if err := weathersim.Validate(json.RawMessage(`{"places":[],"nonsense":1}`)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

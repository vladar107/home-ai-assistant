package daycard_test

import (
	"bytes"
	"errors"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/daycard"
)

// Set DAYCARD_SAMPLES to a directory to keep the rendered samples for a visual review.
func TestRenderSamples(t *testing.T) {
	long := strings.Repeat("Sint-Oedenrode-aan-de-Dommel ", 4)
	samples := map[string]daycard.Card{
		"ru-4-legs": {
			Title:          "Пн 12.10 · Amersfoort Schothorst → Utrecht",
			HeaderLabel:    "Amersfoort Schothorst · 07:00",
			Icon:           daycard.Rain,
			Temperature:    10.2,
			FeelsLike:      5.4,
			FeelsLikeLabel: "ощущается",
			Summary:        "Утром дождь, вечером морось.",
			Legs: []daycard.Leg{
				{Label: "Amersfoort Schothorst · 07–09", Icon: daycard.Rain, Temperature: 10, Detail: "3.4 мм · порывы 47"},
				{Label: "Utrecht · 10–15", Icon: daycard.PartlyCloudy, Temperature: 13, Detail: "1.2 мм"},
				{Label: "Utrecht · 16–17", Icon: daycard.Cloudy, Temperature: 12, Detail: "сухо"},
				{Label: "Amersfoort Schothorst · 17–18", Icon: daycard.Drizzle, Temperature: 11, Detail: "0.8 мм"},
			},
		},
		"en-1-leg": {
			Title:          "Sat 17.10 · Amsterdam",
			HeaderLabel:    "Amsterdam · 10:00",
			Icon:           daycard.Clear,
			Temperature:    18,
			FeelsLike:      17,
			FeelsLikeLabel: "feels like",
			Summary:        "Sunny all day.",
			Legs: []daycard.Leg{
				{Label: "Amsterdam · 10–18", Icon: daycard.Clear, Temperature: 18, Detail: "dry · gusts 22"},
			},
		},
		"en-long-night": {
			Title:          "Wednesday 23.12 · " + long,
			HeaderLabel:    long,
			Icon:           daycard.Snow,
			Night:          true,
			Temperature:    -12.6,
			FeelsLike:      -19,
			FeelsLikeLabel: "feels like",
			Summary:        "Heavy snow through the evening with strong gusts, icy roads, and more snow expected after midnight across the whole region.",
			Legs: []daycard.Leg{
				{Label: long, Icon: daycard.Snow, Night: true, Temperature: -12, Detail: long},
				{Label: "Utrecht · 18–20", Icon: daycard.Fog, Night: true, Temperature: -3, Detail: "12.4 mm · gusts 88"},
				{Label: "Utrecht · 20–22", Icon: daycard.Thunderstorm, Temperature: 0, Detail: "dry"},
				{Label: "Home · 22–23", Icon: daycard.PartlyCloudy, Night: true, Temperature: -0.4, Detail: "0.1 mm"},
			},
		},
		"ru-long-storm": {
			Title:          "Воскресенье 25.10 · Зволле → Амерсфорт → Утрехт → Зволле",
			HeaderLabel:    "Зволле · 07:00",
			Icon:           daycard.Thunderstorm,
			Temperature:    21,
			FeelsLike:      24,
			FeelsLikeLabel: "ощущается",
			Summary:        "Днём грозы с ливнями и шквалистым ветром, к вечеру прояснение и заметно тише, но дороги ещё долго будут мокрыми.",
			Legs: []daycard.Leg{
				{Label: "Амерсфорт-Схотхорст · 08–12", Icon: daycard.HeavyRain, Temperature: 19, Detail: "14.2 мм · порывы 76 км/ч"},
				{Label: "Утрехт · 12–16", Icon: daycard.Thunderstorm, Temperature: 22, Detail: "8.0 мм · порывы 64"},
			},
		},
		"ru-3-legs-trains": {
			Title:          "Вт 29.09 · Amersfoort → Utrecht → Gouda → Amersfoort",
			HeaderLabel:    "Amersfoort · 07:00",
			Icon:           daycard.Cloudy,
			Temperature:    17,
			FeelsLike:      17,
			FeelsLikeLabel: "ощущается",
			Summary:        "Утром поездка в Utrecht, днём — в Gouda; вечером возвращение. Весь день сухо и тепло.",
			Legs: []daycard.Leg{
				{Label: "Amersfoort → Utrecht Centraal · 07–09", Icon: daycard.Cloudy, Temperature: 17, Detail: "сухо"},
				{Label: "Utrecht → Gouda · 16:30", Icon: daycard.PartlyCloudy, Temperature: 26, Detail: "0.4 мм · порывы 32"},
				{Label: "Gouda → Amersfoort · 18–19", Icon: daycard.PartlyCloudy, Night: true, Temperature: 22, Detail: "сухо"},
			},
			Trains: []daycard.Train{
				{Label: "Туда", Status: daycard.TrainSlower, Badge: "+12 мин", Times: "08:10 → 09:02"},
				{Label: "Utrecht → Gouda", Status: daycard.TrainOnTime, Badge: "вовремя", Times: "16:31 → 16:52"},
				{Label: "Обратно", Status: daycard.TrainDisrupted, Badge: "отменён", Times: "18:05 → 18:47"},
			},
		},
		"en-long-trains": {
			Title:          "Fri 16.10 · Amsterdam Centraal → Maastricht",
			HeaderLabel:    "Amsterdam Centraal · 06:30",
			Icon:           daycard.Drizzle,
			Temperature:    8,
			FeelsLike:      4,
			FeelsLikeLabel: "feels like",
			Summary:        "Drizzle in the morning, dry later.",
			Legs: []daycard.Leg{
				{Label: "Amsterdam Centraal · 06–08", Icon: daycard.Drizzle, Temperature: 8, Detail: "0.6 mm · gusts 30"},
				{Label: "Maastricht · 10–18", Icon: daycard.Cloudy, Temperature: 12, Detail: "dry"},
			},
			Trains: []daycard.Train{
				{Label: "Outbound via Eindhoven Centraal and Sittard", Status: daycard.TrainSlower, Badge: "+7 min", Times: "06:44 → 09:12 (09:19)"},
				{Label: "Return", Status: daycard.TrainDisrupted, Badge: "part-cancelled, bus replacement", Times: "18:15 → 20:40"},
			},
		},
	}

	directory := os.Getenv("DAYCARD_SAMPLES")
	for name, card := range samples {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := renderPNG(t, card)
			if directory != "" {
				if err := os.WriteFile(filepath.Join(directory, name+".png"), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestHeightFollowsContent(t *testing.T) {
	base := daycard.Card{Title: "Amersfoort Schothorst", Legs: []daycard.Leg{{Label: "Amersfoort Schothorst", Icon: daycard.Rain}}}
	withSummary := base
	withSummary.Summary = strings.Repeat("Rain in the morning and drizzle in the evening. ", 3)
	withTrains := base
	withTrains.Trains = []daycard.Train{{Label: "Out", Badge: "on time"}, {Label: "Back", Badge: "on time"}}

	baseHeight := height(t, base)
	if got := height(t, withSummary); got <= baseHeight {
		t.Errorf("a summary left the height at %d, want more than %d", got, baseHeight)
	}
	if got := height(t, withTrains); got <= baseHeight {
		t.Errorf("trains left the height at %d, want more than %d", got, baseHeight)
	}
}

func height(t *testing.T, card daycard.Card) int {
	t.Helper()
	decoded, err := png.Decode(bytes.NewReader(renderPNG(t, card)))
	if err != nil {
		t.Fatal(err)
	}
	return decoded.Bounds().Dy()
}

func TestEveryConditionHasAnIcon(t *testing.T) {
	for condition := daycard.Clear; condition <= daycard.Thunderstorm; condition++ {
		t.Run(condition.String(), func(t *testing.T) {
			t.Parallel()
			for _, night := range []bool{false, true} {
				renderPNG(t, daycard.Card{
					Title: condition.String(),
					Icon:  condition,
					Night: night,
					Legs:  []daycard.Leg{{Label: condition.String(), Icon: condition, Night: night}},
				})
			}
		})
	}
}

func TestRenderRefusesInvalidCards(t *testing.T) {
	leg := daycard.Leg{Label: "Amersfoort Schothorst", Icon: daycard.Rain}
	for _, count := range []int{0, daycard.MaximumLegs + 1} {
		legs := make([]daycard.Leg, count)
		for index := range legs {
			legs[index] = leg
		}
		if _, err := daycard.Render(daycard.Card{Legs: legs}); !errors.Is(err, daycard.ErrLegCount) {
			t.Errorf("%d legs: err = %v, want ErrLegCount", count, err)
		}
	}
	trains := make([]daycard.Train, daycard.MaximumTrains+1)
	if _, err := daycard.Render(daycard.Card{Legs: []daycard.Leg{leg}, Trains: trains}); !errors.Is(err, daycard.ErrTrainCount) {
		t.Errorf("%d trains: err = %v, want ErrTrainCount", len(trains), err)
	}
	if _, err := daycard.Render(daycard.Card{Legs: []daycard.Leg{leg}, Trains: []daycard.Train{{Status: daycard.TrainStatus(7)}}}); err == nil {
		t.Error("an unknown train status was accepted")
	}
	if _, err := daycard.Render(daycard.Card{Icon: daycard.Condition(42), Legs: []daycard.Leg{leg}}); err == nil {
		t.Error("an unknown condition was accepted")
	}
	if _, err := daycard.Render(daycard.Card{Temperature: math.NaN(), Legs: []daycard.Leg{leg}}); err == nil {
		t.Error("a NaN temperature was accepted")
	}
}

func TestFromWMO(t *testing.T) {
	cases := map[int]daycard.Condition{
		0: daycard.Clear, 1: daycard.Clear, 2: daycard.PartlyCloudy, 3: daycard.Cloudy,
		45: daycard.Fog, 48: daycard.Fog,
		51: daycard.Drizzle, 53: daycard.Drizzle, 55: daycard.Drizzle, 56: daycard.Drizzle, 57: daycard.Drizzle,
		61: daycard.Rain, 63: daycard.Rain, 65: daycard.HeavyRain, 66: daycard.Rain, 67: daycard.HeavyRain,
		71: daycard.Snow, 73: daycard.Snow, 75: daycard.Snow, 77: daycard.Snow,
		80: daycard.Rain, 81: daycard.Rain, 82: daycard.HeavyRain, 85: daycard.Snow, 86: daycard.Snow,
		95: daycard.Thunderstorm, 96: daycard.Thunderstorm, 99: daycard.Thunderstorm,
		-1: daycard.Cloudy, 100: daycard.Cloudy,
	}
	for code, want := range cases {
		if got := daycard.FromWMO(code); got != want {
			t.Errorf("FromWMO(%d) = %v, want %v", code, got, want)
		}
	}
}

func renderPNG(t *testing.T, card daycard.Card) []byte {
	t.Helper()
	data, err := daycard.Render(card)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if width := decoded.Bounds().Dx(); width != daycard.Width {
		t.Fatalf("width = %d, want %d", width, daycard.Width)
	}
	return data
}

package web_test

import (
	"strings"
	"testing"

	"golang.org/x/image/font/sfnt"

	"github.com/vladar107/home-ai-assistant/internal/fonts"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
)

func TestWeatherCardUsesTheUsersLanguageWhenTheModelGivesNone(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		language locale.Language
		given    string
		tile     string
	}{
		{"russian user", locale.Russian, "", `the tile reads "6.0 мм · порывы 52 км/ч"`},
		{"english user", locale.English, "", `the tile reads "6.0 mm · gusts 52 km/h"`},
		{"model asks for english", locale.Russian, "en", `the tile reads "6.0 mm · gusts 52 km/h"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, box := newCardFixture(t)
			f.openMeteoDay()
			f.ctx = locale.With(f.ctx, test.language)
			turn := outbox.ScheduledTurn(turnID)
			arguments := map[string]any{
				"title":   "Mon 28.09",
				"summary": "Rain.",
				"legs":    []map[string]any{{"label": "Day", "place": "Amsterdam", "date": "2026-09-28", "from_hour": 10, "to_hour": 16}},
			}
			if test.given != "" {
				arguments["language"] = test.given
			}
			result := f.callInTurn(turn, arguments)
			wantText(t, result, test.tile, "Mon 28 Sep, 10:00 to 16:00")
			stagedCard(t, box, turn)
		})
	}
}

func TestEmbeddedFontsDrawEveryRussianCardWord(t *testing.T) {
	t.Parallel()
	set, err := fonts.Inter()
	if err != nil {
		t.Fatal(err)
	}
	words := "ощущается мм порывы сухо км/ч м/с уз Бфт вовремя +0 мин отменён автобус нет пересадки помехи сбой нет поезда"
	var buffer sfnt.Buffer
	for name, face := range map[string]*sfnt.Font{"regular": set.Regular, "semibold": set.SemiBold} {
		for _, character := range strings.ReplaceAll(words, " ", "") {
			index, err := face.GlyphIndex(&buffer, character)
			if err != nil || index == 0 {
				t.Errorf("%s font has no glyph for %q", name, character)
			}
		}
	}
}

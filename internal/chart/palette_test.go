package chart

import (
	"image/color"
	"math"
	"testing"
)

// TestPaletteContrast checks that every series colour stays legible as a line and as a marker's
// value against the background, by the WCAG contrast ratio, and that no two series colours are
// close, so the series stay apart.
func TestPaletteContrast(t *testing.T) {
	for index, colour := range palette {
		if ratio := contrast(colour, background); ratio < 4.5 {
			t.Errorf("series colour %d has contrast %.2f against the background, want at least 4.5", index, ratio)
		}
	}
	for _, text := range []color.NRGBA{textPrimary, textSecondary} {
		if ratio := contrast(text, background); ratio < 4.5 {
			t.Errorf("text colour %v has contrast %.2f, want at least 4.5", text, ratio)
		}
	}
	for first := range palette {
		for second := first + 1; second < len(palette); second++ {
			if distance(palette[first], palette[second]) < 100 {
				t.Errorf("series colours %d and %d are too alike", first, second)
			}
		}
	}
}

func luminance(c color.NRGBA) float64 {
	channel := func(value uint8) float64 {
		v := float64(value) / 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(c.R) + 0.7152*channel(c.G) + 0.0722*channel(c.B)
}

func contrast(a, b color.NRGBA) float64 {
	la, lb := luminance(a), luminance(b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

func distance(a, b color.NRGBA) float64 {
	return math.Sqrt(math.Pow(float64(a.R)-float64(b.R), 2) + math.Pow(float64(a.G)-float64(b.G), 2) + math.Pow(float64(a.B)-float64(b.B), 2))
}

package weekday_test

import (
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/weekday"
)

func TestDescribeForReadsInTheContextsLanguage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		days    []string
		english string
		russian string
	}{
		{weekday.All(), "every day", "каждый день"},
		{weekday.All()[:5], "Mon–Fri", "пн–пт"},
		{weekday.All()[5:], "Sat–Sun", "сб–вс"},
		{[]string{"mon", "wed", "sun"}, "Mon, Wed, Sun", "пн, ср, вс"},
	} {
		if got := weekday.Describe(test.days); got != test.english {
			t.Errorf("Describe(%v) = %q, want %q", test.days, got, test.english)
		}
		if got := weekday.DescribeFor(t.Context(), test.days); got != test.english {
			t.Errorf("DescribeFor(%v) in English = %q, want %q", test.days, got, test.english)
		}
		if got := weekday.DescribeFor(locale.With(t.Context(), locale.Russian), test.days); got != test.russian {
			t.Errorf("DescribeFor(%v) in Russian = %q, want %q", test.days, got, test.russian)
		}
	}
}

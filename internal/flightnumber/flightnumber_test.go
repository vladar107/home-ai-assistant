package flightnumber_test

import (
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/flightnumber"
)

func TestFlightNumbersNormaliseToOneForm(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"ex123": "EX123", " EX 0123 ": "EX123", "EX0123": "EX123", "EX0641": "EX641", "Z0 4567": "Z04567", "Z0123": "Z0123",
		"0Z123": "0Z123", "0Z0012": "0Z12", "EXA0042": "EXA42", "EX123A": "EX123A",
		"EX0": "", "12123": "", "EX": "", "EX12345": "", "EX-123": "", "EX123 and OP456": "", "": "",
	}
	for given, want := range cases {
		got, ok := flightnumber.Normalize(given)
		if got != want || ok != (want != "") {
			t.Errorf("Normalize(%q) = %q, %v; want %q", given, got, ok, want)
		}
	}
}

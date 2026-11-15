package eval

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestZeroMinuteTransferCaseResolvesItsDestination(t *testing.T) {
	cases, err := LoadCases(filepath.Join("..", "..", "evals", "cases"))
	if err != nil {
		t.Fatal(err)
	}
	var seeded Case
	for _, c := range cases {
		if c.ID == "f2-zero-minute-transfer" {
			seeded = c
		}
	}
	if seeded.ID == "" {
		t.Fatal("case f2-zero-minute-transfer not found")
	}
	w := newCaseWorld(t, seeded, "")

	record := w.play(Step{Say: strings.Join([]string{
		call("bus_trip", `{"from":"Eastbrook, Market Square","to":"Westfield Ice Arena","date_time":"2026-03-10T17:00"}`),
		call("ns_trips", `{"from":"Eastbrook Central","to":"Westfield","date_time":"2026-03-10T17:00"}`),
	}, "\n")})

	for tool, wants := range map[string][]string{
		"bus_trip": {"18:05", "Westfield Ice Arena"},
		"ns_trips": {"18:05", "IC 1100"},
	} {
		result := w.toolResult(record, tool)
		if result.IsError {
			t.Fatalf("%s failed: %q", tool, result.Result)
		}
		for _, want := range wants {
			if !strings.Contains(result.Result, want) {
				t.Errorf("%s: want %q in %q", tool, want, result.Result)
			}
		}
	}
}

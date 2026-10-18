package prompt

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// The golden files pin the exact bytes the model reads, so any change to the wording is a visible
// diff in review. Run go test ./internal/prompt -update after an intended change.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".txt")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v; run with -update to create it", path, err)
	}
	if got != string(want) {
		t.Errorf("%s changed; run with -update if the change is intended\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

const (
	guidance = "  Weather: weather_get answers forecasts.\nCalendar: calendar_list shows events.  "
	memory   = "Memory:\n- Prefers short answers."
	jobs     = "Scheduled jobs:\n- Morning summary, every day at 07:00."
)

func TestSystemPrompt(t *testing.T) {
	cases := map[string]string{
		"system-no-tools":          System(false, "", false),
		"system-web-search":        System(false, "", true),
		"system-tools":             System(true, guidance, false),
		"system-tools-web-search":  System(true, guidance, true),
		"system-tools-no-guidance": System(true, "", false),
		"system-tools-sections":    WithSections(System(true, guidance, true), memory, " ", jobs),
		"system-no-tools-sections": WithSections(System(false, "", false), memory),
	}
	for name, got := range cases {
		checkGolden(t, name, got)
		// Per-turn lines belong in the message; one in the system prompt would break its cache.
		if strings.Contains(got, Marker) {
			t.Errorf("%s carries a %s line", name, Marker)
		}
	}
	if System(false, guidance, false) != System(false, "", false) {
		t.Error("a turn without tools took the guidance of tools it does not have")
	}
}

func TestTurnLines(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	message := Prepend(Forwarded, "Can you look at this?")
	for _, line := range []string{
		ToolHint("weather_get"),
		Notice("The user confirmed your schedule_create proposal and it was executed."),
		Place("User's location: near the station."),
		User("Alex"),
		AboutJob(`Morning "summary"`, "j1"),
		Time(now, zone),
	} {
		message = Prepend(line, message)
	}
	checkGolden(t, "turn-lines", message)
	checkGolden(t, "turn-about-watch", AboutWatch("", "w1"))
	checkGolden(t, "turn-about-trip", AboutTrip("Conference", "t1"))
	checkGolden(t, "turn-continue", Continue)
	checkGolden(t, "turn-reconsider", ReconsiderAt(now.In(zone), false))
	checkGolden(t, "turn-reconsider-learn", ReconsiderAt(now.In(zone), true))

	for _, empty := range []string{ToolHint(""), Notice(""), Place(""), User("")} {
		if empty != "" {
			t.Errorf("an empty value gave the line %q", empty)
		}
	}
	if got := Prepend("", "hi"); got != "hi" {
		t.Errorf("an empty line changed the message to %q", got)
	}
}

func TestUnattendedTurns(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	slot := time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC)
	checkGolden(t, "scheduled-job", ScheduledJob(`Morning "summary"`, slot, zone, "Summarise today's calendar."))
	checkGolden(t, "scheduled-scan", ScheduledScan("Trip scan", slot, zone, "Search the mail for bookings."))
	checkGolden(t, "watch-check", WatchCheck("Price", slot, time.UTC, "Check: the page.\nTell the user when: below 10."))
	checkGolden(t, "background-task", BackgroundTask(`Read "mail"`, 100, "1h", "Go through last week's mail."))
	checkGolden(t, "background-task-limit", BackgroundTaskLimit("time limit"))
	checkGolden(t, "background-task-resume", BackgroundTaskResume)
}

func TestToolResults(t *testing.T) {
	checkGolden(t, "tool-results", strings.Join([]string{
		ToolResultUntrusted + ToolResultScreened + "result text",
		ToolUnavailable,
		ToolUnavailableNow,
		ToolTimedOut,
		ToolFailed,
		ToolOversized,
		ToolBudgetExhausted(""),
		ToolBudgetExhausted("task_start"),
	}, "\n---\n"))
	checkGolden(t, "act-results", strings.Join([]string{
		Proposed,
		ActDone,
		ActUndoable,
		ActNotUndoable,
		ActUndone,
		ActRepeated,
		ActRetried,
		ActExecuting,
	}, "\n---\n"))
}

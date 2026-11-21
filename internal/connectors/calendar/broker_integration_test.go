package calendar

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/calendarsim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// Through the broker, a listing and a read event carry the locations they show as declared
// values, while the text still shows them; an event without a location declares nothing.
func TestBrokerCarriesEventLocationsAsDeclaredValues(t *testing.T) {
	h := newHarness(t, work)
	h.sim.AddCalendar(work, calendarsim.Calendar{ID: work, Summary: "Ann Work", Primary: true})
	const street = "12 Example Street, Springfield"
	h.sim.AddEvents(work, work,
		calendarsim.Event{ID: "review", Summary: "Review", Start: utc(5, 7, 0), End: utc(5, 8, 0), Location: "  " + street + "  "},
		calendarsim.Event{ID: "standup", Summary: "Standup", Start: utc(5, 9, 0), End: utc(5, 9, 15), Location: "Room 4"},
		calendarsim.Event{ID: "follow-up", Summary: "Follow-up", Start: utc(5, 10, 0), End: utc(5, 11, 0), Location: street},
		calendarsim.Event{ID: "focus", Summary: "Focus time", Start: utc(5, 12, 0), End: utc(5, 13, 0)},
	)
	broker, err := tools.NewBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(h.service); err != nil {
		t.Fatal(err)
	}
	set, err := broker.ToolsFor(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(name string, arguments map[string]any) agentruntime.ToolResult {
		t.Helper()
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		result, err := set.Executor.Execute(context.Background(), agentruntime.ToolCall{Name: name, Arguments: raw})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s failed: %s", name, result.Text)
		}
		return result
	}

	listing := execute(toolEvents, map[string]any{"start": "2026-10-05", "end": "2026-10-05"})
	if !strings.Contains(listing.Text, "Location: "+street+"\n") || !strings.Contains(listing.Text, "Location: Room 4\n") {
		t.Fatalf("listing text:\n%s", listing.Text)
	}
	if want := []string{street, "Room 4"}; !slices.Equal(listing.Sensitive, want) {
		t.Fatalf("listing declared %q, want %q", listing.Sensitive, want)
	}

	read := execute(toolRead, map[string]any{"account": work, "calendar_id": work, "event_id": "review"})
	if !strings.Contains(read.Text, "Location: "+street) {
		t.Fatalf("read text:\n%s", read.Text)
	}
	if want := []string{street}; !slices.Equal(read.Sensitive, want) {
		t.Fatalf("read declared %q, want %q", read.Sensitive, want)
	}

	if bare := execute(toolRead, map[string]any{"account": work, "calendar_id": work, "event_id": "focus"}); bare.Sensitive != nil {
		t.Fatalf("an event without a location declared %q", bare.Sensitive)
	}
}

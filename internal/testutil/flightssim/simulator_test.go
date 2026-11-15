package flightssim_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/flights"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/flightssim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const seed = `{
  "flights": [{"number": "EX123", "date": "2026-03-10", "status": "Delayed", "airline": "Example Air",
    "departure": {"iata": "EBK", "name": "Eastbrook Airport", "city": "Eastbrook", "timezone": "Europe/Amsterdam",
      "scheduled": "2026-03-10T10:00:00Z", "revised": "2026-03-10T10:50:00Z", "terminal": "1", "gate": "B4"},
    "arrival": {"iata": "WFD", "name": "Westfield Airport", "city": "Westfield", "timezone": "Europe/Lisbon",
      "scheduled": "2026-03-10T13:10:00Z"},
    "live": true}]
}`

func call(t *testing.T, arguments string) tools.Result {
	t.Helper()
	httpretry.WithoutWaits(t)
	fake := flightssim.New(t, json.RawMessage(seed))
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC)
	service, err := flights.NewService(flights.Config{
		APIKey: "test-key", MonthlyUnits: 100, QuotaResetDay: 1, Zone: time.UTC, BaseURL: fake.URL(),
		Now:   func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error { return nil },
	}, store, simkit.AlwaysOn{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(t.Context(), "alice", tools.Call{Name: "flight_status", Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFlightStatusComesFromSeed(t *testing.T) {
	result := call(t, `{"flight_number":"EX 123","date":"2026-03-10"}`)
	for _, want := range []string{"EX123", "delayed", "Eastbrook", "Westfield"} {
		if result.IsError || !strings.Contains(result.Text, want) {
			t.Errorf("lacks %q: %+v", want, result)
		}
	}
}

func TestUnseededFlightIsNotFound(t *testing.T) {
	result := call(t, `{"flight_number":"EX999","date":"2026-03-10"}`)
	if result.IsError || !strings.Contains(result.Text, "was not found") {
		t.Fatalf("got %+v", result)
	}
}

func TestUnknownQueryParameterIsRefused(t *testing.T) {
	fake := flightssim.New(t, json.RawMessage(seed))
	request, _ := http.NewRequest(http.MethodGet, fake.URL()+"/flights/number/EX123/2026-03-10?bogus=1", nil)
	request.Header.Set("X-RapidAPI-Key", "k")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", response.StatusCode)
	}
}

func TestValidateRejectsUnknownFields(t *testing.T) {
	if err := flightssim.Validate(json.RawMessage(seed)); err != nil {
		t.Fatal(err)
	}
	if err := flightssim.Validate(json.RawMessage(`{"flights":[],"nonsense":1}`)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

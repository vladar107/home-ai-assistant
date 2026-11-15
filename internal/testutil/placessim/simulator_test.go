package placessim_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/connectors/places"
	"github.com/vladar107/home-ai-assistant/internal/testutil/placessim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const seed = `{
  "elements": [
    {"lat": 52.1010, "lon": 5.1020, "tags": {"amenity": "pharmacy", "name": "Eastbrook Pharmacy",
      "addr:street": "Market Street", "addr:housenumber": "4", "opening_hours": "Mo-Fr 09:00-18:00"}},
    {"lat": 52.1012, "lon": 5.1021, "tags": {"amenity": "cafe", "name": "Corner Cafe"}},
    {"lat": 52.5, "lon": 5.9, "tags": {"amenity": "pharmacy", "name": "Faraway Pharmacy"}}
  ],
  "reverse": [{"lat": 52.1, "lon": 5.1, "type": "ADDRESS", "street": "Market Street", "house_number": "1",
    "areas": [{"name": "Eastbrook", "admin_level": 8, "default": true}]}]
}`

func service(t *testing.T) *places.Service {
	t.Helper()
	fake := placessim.New(t, json.RawMessage(seed))
	service, err := places.NewService(places.Config{
		OverpassURL:   fake.OverpassURL(),
		TransitousURL: fake.TransitousURL(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestNearbyPlacesComeFromSeed(t *testing.T) {
	result, err := service(t).Execute(t.Context(), "alice", tools.Call{
		Name:      places.ToolNearbyPlaces,
		Arguments: json.RawMessage(`{"latitude":52.1,"longitude":5.1,"kind":"pharmacy","radius":1000}`),
	})
	if err != nil || result.IsError {
		t.Fatalf("got %+v, %v", result, err)
	}
	if !strings.Contains(result.Text, "Eastbrook Pharmacy") || strings.Contains(result.Text, "Corner Cafe") || strings.Contains(result.Text, "Faraway") {
		t.Fatalf("unexpected places:\n%s", result.Text)
	}
}

func TestDescribeUsesSeededReverseGeocode(t *testing.T) {
	text, err := service(t).Describe(t.Context(), 52.1, 5.1)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Market Street 1, Eastbrook" {
		t.Fatalf("got %q", text)
	}
}

func TestUnknownFormFieldIsRefused(t *testing.T) {
	fake := placessim.New(t, json.RawMessage(seed))
	response, err := http.PostForm(fake.OverpassURL(), url.Values{"data": {"x"}, "bogus": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", response.StatusCode)
	}
}

func TestValidateRejectsUnknownFields(t *testing.T) {
	if err := placessim.Validate(json.RawMessage(seed)); err != nil {
		t.Fatal(err)
	}
	if err := placessim.Validate(json.RawMessage(`{"elements":[],"nonsense":1}`)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

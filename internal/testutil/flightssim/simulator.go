// Package flightssim is a loopback fake of the AeroDataBox flight status endpoint the flights
// connector reads. A flight is seeded by number and departure day; a lookup for one that was not
// seeded answers 204, as AeroDataBox does for an unknown flight, and a request without the
// RapidAPI key, or with a query parameter the endpoint does not take, is refused.
//
// Example seed (fictional airports and airline):
//
//	{
//	  "flights": [{"number": "EX123", "date": "2026-03-10", "status": "Delayed", "airline": "Example Air",
//	    "departure": {"iata": "EBK", "name": "Eastbrook Airport", "city": "Eastbrook", "timezone": "Europe/Amsterdam",
//	      "scheduled": "2026-03-10T10:00:00Z", "revised": "2026-03-10T10:50:00Z", "terminal": "1", "gate": "B4"},
//	    "arrival": {"iata": "WFD", "name": "Westfield Airport", "city": "Westfield", "timezone": "Europe/Lisbon",
//	      "scheduled": "2026-03-10T13:10:00Z"},
//	    "live": true}]
//	}
package flightssim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

const timeLayout = "2006-01-02 15:04Z07:00"

type Seed struct {
	Flights []Flight `json:"flights"`
}

// Flight is one flight on one departure day. Status is one of AeroDataBox's status words, such as
// Expected, Departed, Delayed, Arrived or Canceled. Codeshare is IsOperator or IsCodeshared.
type Flight struct {
	Number    string   `json:"number"`
	Date      string   `json:"date"`
	Status    string   `json:"status"`
	Codeshare string   `json:"codeshare"`
	Airline   string   `json:"airline"`
	Live      bool     `json:"live"`
	Departure Movement `json:"departure"`
	Arrival   Movement `json:"arrival"`
}

// Movement is one end of a flight; its times are RFC 3339 and may be empty except for the
// departure's scheduled time.
type Movement struct {
	IATA      string `json:"iata"`
	ICAO      string `json:"icao"`
	Name      string `json:"name"`
	City      string `json:"city"`
	Timezone  string `json:"timezone"`
	Scheduled string `json:"scheduled"`
	Revised   string `json:"revised"`
	Predicted string `json:"predicted"`
	Runway    string `json:"runway"`
	Terminal  string `json:"terminal"`
	Gate      string `json:"gate"`
}

// Validate checks that seed has the shape the fake accepts.
func Validate(seed json.RawMessage) error {
	_, err := parse(seed)
	return err
}

func parse(raw json.RawMessage) (Seed, error) {
	var seed Seed
	if err := simkit.Decode(raw, &seed); err != nil {
		return seed, err
	}
	for index, flight := range seed.Flights {
		if flight.Number == "" {
			return seed, fmt.Errorf("flights[%d] needs a number", index)
		}
		if _, err := time.Parse(time.DateOnly, flight.Date); err != nil {
			return seed, fmt.Errorf("flights[%d] date must be YYYY-MM-DD", index)
		}
		for _, end := range []Movement{flight.Departure, flight.Arrival} {
			for _, moment := range []string{end.Scheduled, end.Revised, end.Predicted, end.Runway} {
				if moment == "" {
					continue
				}
				if _, err := time.Parse(time.RFC3339, moment); err != nil {
					return seed, fmt.Errorf("flights[%d] times must be RFC 3339", index)
				}
			}
		}
		if flight.Departure.Scheduled == "" {
			return seed, fmt.Errorf("flights[%d] needs a scheduled departure", index)
		}
	}
	return seed, nil
}

// Server is the running fake.
type Server struct {
	*simkit.Server
	seed Seed
}

// Start runs the fake on a loopback port; the caller closes it.
func Start(raw json.RawMessage) (*Server, error) {
	seed, err := parse(raw)
	if err != nil {
		return nil, err
	}
	s := &Server{seed: seed}
	inner, err := simkit.Serve(http.HandlerFunc(s.serve))
	if err != nil {
		return nil, err
	}
	s.Server = inner
	return s, nil
}

// New runs the fake for the length of a test.
func New(t testing.TB, raw json.RawMessage) *Server {
	t.Helper()
	s, err := Start(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.Cleanup(t)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	rest, found := strings.CutPrefix(r.URL.Path, "/flights/number/")
	number, day, ok := strings.Cut(rest, "/")
	if !found || !ok {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("X-RapidAPI-Key") == "" {
		simkit.WriteJSON(w, http.StatusUnauthorized, map[string]string{"message": "Invalid API key"})
		return
	}
	if unknown := simkit.UnknownParameters(r.URL.Query(), "withAircraftImage", "withLocation", "withFlightPlan", "dateLocalRole"); len(unknown) > 0 {
		simkit.WriteJSON(w, http.StatusBadRequest, map[string]string{"message": "Unknown parameter: " + strings.Join(unknown, ", ")})
		return
	}
	answer := []map[string]any{}
	for _, flight := range s.seed.Flights {
		if strings.EqualFold(compact(flight.Number), compact(number)) && flight.Date == day {
			answer = append(answer, flight.encode())
		}
	}
	if len(answer) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	simkit.WriteJSON(w, http.StatusOK, answer)
}

func compact(number string) string { return strings.ReplaceAll(number, " ", "") }

func (f Flight) encode() map[string]any {
	quality := []string{"Basic"}
	if f.Live {
		quality = append(quality, "Live")
	}
	entry := map[string]any{
		"number":          f.Number,
		"status":          f.Status,
		"codeshareStatus": f.Codeshare,
		"airline":         map[string]string{"name": f.Airline},
		"departure":       f.Departure.encode(quality),
		"arrival":         f.Arrival.encode(quality),
	}
	return entry
}

func (m Movement) encode(quality []string) map[string]any {
	entry := map[string]any{
		"airport": map[string]string{
			"iata": m.IATA, "icao": m.ICAO, "name": m.Name, "municipalityName": m.City, "timeZone": m.Timezone,
		},
		"terminal": m.Terminal,
		"gate":     m.Gate,
		"quality":  quality,
	}
	for key, moment := range map[string]string{"scheduledTime": m.Scheduled, "revisedTime": m.Revised, "predictedTime": m.Predicted, "runwayTime": m.Runway} {
		if parsed, err := time.Parse(time.RFC3339, moment); err == nil {
			entry[key] = map[string]string{"utc": parsed.UTC().Format(timeLayout)}
		}
	}
	return entry
}

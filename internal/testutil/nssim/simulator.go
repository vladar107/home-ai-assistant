// Package nssim is a loopback fake of the NS travel information and places endpoints the ns
// connector reads: the station list, trips, departures, disruptions and OV-fiets points. Trips,
// departures, disruptions and rental points are given as the objects the NS API itself returns,
// so a case controls delays, cancellations, transfers and platforms. A query parameter the real
// API does not take is refused with a 400 error body, as NS does.
//
// Example seed (fictional stations and trains):
//
//	{
//	  "stations": [
//	    {"code": "EBK", "name": "Eastbrook Centraal"},
//	    {"code": "WFD", "name": "Westfield"}
//	  ],
//	  "trips": [{"from": "EBK", "to": "WFD", "trip": {
//	    "status": "NORMAL", "transfers": 0, "plannedDurationInMinutes": 24,
//	    "legs": [{"name": "IC 1100", "travelType": "PUBLIC_TRANSIT", "direction": "Westfield",
//	      "product": {"categoryCode": "IC", "number": "1100", "longCategoryName": "Intercity"},
//	      "origin": {"name": "Eastbrook Centraal", "stationCode": "EBK", "plannedDateTime": "2026-03-10T08:10:00+0100", "plannedTrack": "4"},
//	      "destination": {"name": "Westfield", "stationCode": "WFD", "plannedDateTime": "2026-03-10T08:34:00+0100"}}]}}],
//	  "departures": [{"station": "EBK", "departure": {"direction": "Westfield", "name": "NS 1100",
//	    "plannedDateTime": "2026-03-10T08:10:00+0100", "plannedTrack": "4", "cancelled": false,
//	    "product": {"categoryCode": "IC", "number": "1100"}}}],
//	  "disruptions": [{"type": "DISRUPTION", "id": "d1", "title": "Eastbrook - Westfield", "isActive": true,
//	    "publicationSections": [{"section": {"stations": [{"name": "Westfield", "stationCode": "WFD"}]}}]}],
//	  "ovfiets": [{"station": "EBK", "location": {"name": "Eastbrook Centraal", "stationCode": "EBK",
//	    "description": "OV-fiets Eastbrook", "extra": {"rentalBikes": "7"}}}]
//	}
package nssim

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

const (
	// APIPrefix is the path under the server's URL the connector's BaseURL takes.
	APIPrefix = "/reisinformatie-api/api"
	// PlacesPrefix is the path under the server's URL the connector's PlacesBaseURL takes.
	PlacesPrefix = "/places-api"

	// NS answers an arrive-by search with trips around the requested time, later arrivals included.
	arrivalWindowBefore = 90 * time.Minute
	arrivalWindowAfter  = 60 * time.Minute
)

type Seed struct {
	Stations    []Station         `json:"stations"`
	Trips       []Trip            `json:"trips"`
	Departures  []Departure       `json:"departures"`
	Disruptions []json.RawMessage `json:"disruptions"`
	OVFiets     []OVFiets         `json:"ovfiets"`
}

type Station struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// UIC defaults to the code.
	UIC string `json:"uic"`
	// Country defaults to NL.
	Country  string   `json:"country"`
	Synonyms []string `json:"synonyms"`
}

// Trip is one planned trip between two station codes, as the NS trip object.
type Trip struct {
	From string          `json:"from"`
	To   string          `json:"to"`
	Trip json.RawMessage `json:"trip"`
}

// Departure is one row of a station's departure board, as the NS departure object.
type Departure struct {
	Station   string          `json:"station"`
	Departure json.RawMessage `json:"departure"`
}

// OVFiets is one rental point at a station, as the NS places location object.
type OVFiets struct {
	Station  string          `json:"station"`
	Location json.RawMessage `json:"location"`
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
	for index, station := range seed.Stations {
		if station.Code == "" || station.Name == "" {
			return seed, fmt.Errorf("stations[%d] needs a code and a name", index)
		}
	}
	for index, item := range seed.Trips {
		if item.From == "" || item.To == "" {
			return seed, fmt.Errorf("trips[%d] needs from and to", index)
		}
		if err := simkit.Object(fmt.Sprintf("trips[%d].trip", index), item.Trip); err != nil {
			return seed, err
		}
	}
	for index, item := range seed.Departures {
		if item.Station == "" {
			return seed, fmt.Errorf("departures[%d] needs a station", index)
		}
		if err := simkit.Object(fmt.Sprintf("departures[%d].departure", index), item.Departure); err != nil {
			return seed, err
		}
	}
	for index, item := range seed.Disruptions {
		if err := simkit.Object(fmt.Sprintf("disruptions[%d]", index), item); err != nil {
			return seed, err
		}
	}
	for index, item := range seed.OVFiets {
		if item.Station == "" {
			return seed, fmt.Errorf("ovfiets[%d] needs a station", index)
		}
		if err := simkit.Object(fmt.Sprintf("ovfiets[%d].location", index), item.Location); err != nil {
			return seed, err
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

// BaseURL is the address for the connector's BaseURL.
func (s *Server) BaseURL() string { return s.URL() + APIPrefix }

// PlacesBaseURL is the address for the connector's PlacesBaseURL.
func (s *Server) PlacesBaseURL() string { return s.URL() + PlacesPrefix }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var allowed []string
	var handler func(http.ResponseWriter, *http.Request)
	switch path := r.URL.Path; {
	case path == APIPrefix+"/v2/stations":
		handler = s.stations
	case path == APIPrefix+"/v3/trips":
		allowed = []string{"fromStation", "toStation", "dateTime", "lang", "viaStation", "searchForArrival", "addChangeTime"}
		handler = s.trips
	case path == APIPrefix+"/v2/departures":
		allowed = []string{"station", "maxJourneys", "lang", "dateTime"}
		handler = s.departures
	case path == APIPrefix+"/v3/disruptions":
		allowed = []string{"lang", "isActive"}
		handler = s.disruptions
	case strings.HasPrefix(path, APIPrefix+"/v3/disruptions/station/"):
		allowed = []string{"lang"}
		handler = s.disruptions
	case path == PlacesPrefix+"/v2/ovfiets":
		allowed = []string{"station_code"}
		handler = s.ovfiets
	default:
		http.NotFound(w, r)
		return
	}
	if unknown := simkit.UnknownParameters(query, allowed...); len(unknown) > 0 {
		refuse(w, "UNKNOWN_PARAMETER", "Unknown query parameter: "+strings.Join(unknown, ", "))
		return
	}
	handler(w, r)
}

func refuse(w http.ResponseWriter, kind, message string) {
	simkit.WriteJSON(w, http.StatusBadRequest, map[string]any{
		"errors": []map[string]string{{"type": kind, "message": message}},
	})
}

func (s *Server) stations(w http.ResponseWriter, _ *http.Request) {
	records := make([]map[string]any, 0, len(s.seed.Stations))
	for _, station := range s.seed.Stations {
		records = append(records, map[string]any{
			"UICCode":    cmp.Or(station.UIC, station.Code),
			"code":       station.Code,
			"land":       cmp.Or(station.Country, "NL"),
			"namen":      map[string]string{"lang": station.Name, "middel": station.Name, "kort": station.Name},
			"synoniemen": station.Synonyms,
		})
	}
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"payload": records})
}

func (s *Server) knows(code string) bool {
	return slices.ContainsFunc(s.seed.Stations, func(station Station) bool { return strings.EqualFold(station.Code, code) })
}

func parseTime(value string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

type tripTimes struct {
	Legs []struct {
		Origin      struct{ PlannedDateTime string } `json:"origin"`
		Destination struct{ PlannedDateTime string } `json:"destination"`
	} `json:"legs"`
}

func (s *Server) trips(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	from, to := query.Get("fromStation"), query.Get("toStation")
	at, valid := parseTime(query.Get("dateTime"))
	if !valid {
		refuse(w, "INVALID_PARAMETER", "dateTime must be an ISO 8601 date and time with an offset")
		return
	}
	for name, code := range map[string]string{"fromStation": from, "toStation": to} {
		if !s.knows(code) {
			refuse(w, "INVALID_PARAMETER", name+" is not a known station code")
			return
		}
	}
	arrival, _ := strconv.ParseBool(query.Get("searchForArrival"))
	type timed struct {
		at   time.Time
		trip json.RawMessage
	}
	var found []timed
	for _, item := range s.seed.Trips {
		if !strings.EqualFold(item.From, from) || !strings.EqualFold(item.To, to) {
			continue
		}
		var times tripTimes
		_ = json.Unmarshal(item.Trip, &times)
		if len(times.Legs) == 0 {
			continue
		}
		departs, _ := parseTime(times.Legs[0].Origin.PlannedDateTime)
		arrives, _ := parseTime(times.Legs[len(times.Legs)-1].Destination.PlannedDateTime)
		switch {
		case arrival && !arrives.Before(at.Add(-arrivalWindowBefore)) && !arrives.After(at.Add(arrivalWindowAfter)):
			found = append(found, timed{arrives, item.Trip})
		case !arrival && !departs.Before(at):
			found = append(found, timed{departs, item.Trip})
		}
	}
	slices.SortStableFunc(found, func(a, b timed) int { return a.at.Compare(b.at) })
	trips := make([]json.RawMessage, 0, len(found))
	for _, item := range found {
		trips = append(trips, item.trip)
	}
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"trips": trips})
}

func (s *Server) departures(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	station := query.Get("station")
	if !s.knows(station) {
		refuse(w, "INVALID_PARAMETER", "station is not a known station code")
		return
	}
	limit, err := strconv.Atoi(query.Get("maxJourneys"))
	if err != nil || limit < 1 {
		limit = 40
	}
	var after time.Time
	if value := query.Get("dateTime"); value != "" {
		parsed, valid := parseTime(value)
		if !valid {
			refuse(w, "INVALID_PARAMETER", "dateTime must be an ISO 8601 date and time with an offset")
			return
		}
		after = parsed
	}
	rows := []json.RawMessage{}
	for _, item := range s.seed.Departures {
		if !strings.EqualFold(item.Station, station) {
			continue
		}
		var planned struct {
			PlannedDateTime string `json:"plannedDateTime"`
		}
		_ = json.Unmarshal(item.Departure, &planned)
		if at, ok := parseTime(planned.PlannedDateTime); ok && at.Before(after) {
			continue
		}
		rows = append(rows, item.Departure)
	}
	rows = rows[:min(len(rows), limit)]
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"payload": map[string]any{"departures": rows}})
}

func (s *Server) disruptions(w http.ResponseWriter, r *http.Request) {
	const stationPath = APIPrefix + "/v3/disruptions/station/"
	station, byStation := strings.CutPrefix(r.URL.Path, stationPath)
	if byStation && !s.knows(station) {
		simkit.WriteJSON(w, http.StatusNotFound, map[string]any{})
		return
	}
	activeOnly := r.URL.Query().Get("isActive") == "true"
	result := []json.RawMessage{}
	for _, item := range s.seed.Disruptions {
		var entry struct {
			IsActive            *bool `json:"isActive"`
			PublicationSections []struct {
				Section struct {
					Stations []struct {
						StationCode string `json:"stationCode"`
					} `json:"stations"`
				} `json:"section"`
			} `json:"publicationSections"`
		}
		_ = json.Unmarshal(item, &entry)
		if activeOnly && entry.IsActive != nil && !*entry.IsActive {
			continue
		}
		if byStation && !touches(entry.PublicationSections, station) {
			continue
		}
		result = append(result, item)
	}
	simkit.WriteJSON(w, http.StatusOK, result)
}

func touches(sections []struct {
	Section struct {
		Stations []struct {
			StationCode string `json:"stationCode"`
		} `json:"stations"`
	} `json:"section"`
}, code string) bool {
	for _, section := range sections {
		for _, station := range section.Section.Stations {
			if strings.EqualFold(station.StationCode, code) {
				return true
			}
		}
	}
	return false
}

func (s *Server) ovfiets(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("station_code")
	locations := []json.RawMessage{}
	for _, item := range s.seed.OVFiets {
		if strings.EqualFold(item.Station, code) {
			locations = append(locations, item.Location)
		}
	}
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"payload": []map[string]any{{"locations": locations}}})
}

// Package bussim is a loopback fake of the three services the bus connector reads: OVapi's live
// timing-point calls, the national GTFS archive with its stop and route tables and its
// GTFS-realtime alert feed, and Transitous trip planning, stop times and geocoding. Stops, routes
// and alerts become a real zip archive and a real protobuf feed, so the connector's range reads
// and decoders run as in production.
//
// Example seed (fictional town, line 340):
//
//	{
//	  "stops": [
//	    {"id": "100", "code": "30000001", "name": "Eastbrook, Market Square", "lat": 52.1, "lon": 5.1, "platform": "A"},
//	    {"id": "101", "code": "30000002", "name": "Westfield, Station", "lat": 52.2, "lon": 5.2}
//	  ],
//	  "routes": [{"id": "r340", "agency": "UOV", "short": "340", "long": "Eastbrook - Westfield"}],
//	  "timing_points": [{"code": "30000001", "passes": {"J1": {
//	    "LinePublicNumber": "340", "JourneyNumber": 1, "UserStopOrderNumber": 1,
//	    "DestinationName50": "Westfield", "TransportType": "BUS", "TripStopStatus": "PLANNED",
//	    "JourneyStopType": "FIRST", "TargetDepartureTime": "2026-03-10T08:15:00",
//	    "ExpectedDepartureTime": "2026-03-10T08:17:00"}}}],
//	  "alerts": [{"effect": 4, "header": "Line 340 detour", "from": "2026-03-10T00:00:00Z",
//	    "informed": [{"route_id": "r340"}]}],
//	  "places": [{"type": "STOP", "name": "Westfield Central", "lat": 52.2, "lon": 5.2, "country": "NL"}]
//	}
//
// Transitous plans and stop times are given as the objects Transitous returns ("plans" and
// "stop_times"); a plan answers a request whose endpoints lie within about 200 m of its own.
package bussim

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

const (
	OVapiPrefix      = "/ovapi"
	GTFSPrefix       = "/gtfs"
	TransitousPrefix = "/transitous"

	openOVPrefix = "nl-OpenOV_"
	placeRadius  = 0.002
)

type Seed struct {
	Stops        []Stop        `json:"stops"`
	Routes       []Route       `json:"routes"`
	TimingPoints []TimingPoint `json:"timing_points"`
	Alerts       []Alert       `json:"alerts"`
	Plans        []Plan        `json:"plans"`
	StopTimes    []StopTime    `json:"stop_times"`
	Places       []Place       `json:"places"`
}

// Stop is one GTFS platform. Code is its OVapi timing point code and may be empty.
type Stop struct {
	ID       string  `json:"id"`
	Code     string  `json:"code"`
	Name     string  `json:"name"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Platform string  `json:"platform"`
}

type Route struct {
	ID     string `json:"id"`
	Agency string `json:"agency"`
	Short  string `json:"short"`
	Long   string `json:"long"`
}

// TimingPoint is what OVapi reports at one timing point: its passes by journey key.
type TimingPoint struct {
	Code   string          `json:"code"`
	Passes map[string]Pass `json:"passes"`
}

// Pass is one vehicle's call, with OVapi's own field names.
type Pass struct {
	OperatorCode          string `json:"OperatorCode"`
	OperationDate         string `json:"OperationDate"`
	LinePlanningNumber    string `json:"LinePlanningNumber"`
	LinePublicNumber      string `json:"LinePublicNumber"`
	JourneyNumber         int    `json:"JourneyNumber"`
	FortifyOrderNumber    int    `json:"FortifyOrderNumber"`
	UserStopOrderNumber   int    `json:"UserStopOrderNumber"`
	DestinationName50     string `json:"DestinationName50"`
	TargetArrivalTime     string `json:"TargetArrivalTime"`
	ExpectedArrivalTime   string `json:"ExpectedArrivalTime"`
	TargetDepartureTime   string `json:"TargetDepartureTime"`
	ExpectedDepartureTime string `json:"ExpectedDepartureTime"`
	TripStopStatus        string `json:"TripStopStatus"`
	TransportType         string `json:"TransportType"`
	TimingPointCode       string `json:"TimingPointCode"`
	JourneyStopType       string `json:"JourneyStopType"`
}

// Alert is one service alert. Effect is the GTFS-realtime effect number; From and To are RFC 3339
// and either may be empty.
type Alert struct {
	Effect      uint64     `json:"effect"`
	Header      string     `json:"header"`
	Description string     `json:"description"`
	From        string     `json:"from"`
	To          string     `json:"to"`
	Informed    []Informed `json:"informed"`
}

type Informed struct {
	StopID  string `json:"stop_id"`
	RouteID string `json:"route_id"`
}

type Point struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Plan is an itinerary Transitous returns for a trip from one point to another.
type Plan struct {
	From      Point           `json:"from"`
	To        Point           `json:"to"`
	Itinerary json.RawMessage `json:"itinerary"`
}

// StopTime is a departure Transitous lists at a stop, by the stop's GTFS ID.
type StopTime struct {
	StopID   string          `json:"stop_id"`
	StopTime json.RawMessage `json:"stop_time"`
}

// Place is a geocoder match.
type Place struct {
	Type    string  `json:"type"`
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Country string  `json:"country"`
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
	for index, stop := range seed.Stops {
		if stop.ID == "" || stop.Name == "" {
			return seed, fmt.Errorf("stops[%d] needs an id and a name", index)
		}
	}
	for index, route := range seed.Routes {
		if route.ID == "" {
			return seed, fmt.Errorf("routes[%d] needs an id", index)
		}
	}
	for index, point := range seed.TimingPoints {
		if point.Code == "" {
			return seed, fmt.Errorf("timing_points[%d] needs a code", index)
		}
	}
	for index, alert := range seed.Alerts {
		for _, moment := range []string{alert.From, alert.To} {
			if moment == "" {
				continue
			}
			if _, err := time.Parse(time.RFC3339, moment); err != nil {
				return seed, fmt.Errorf("alerts[%d] times must be RFC 3339", index)
			}
		}
	}
	for index, plan := range seed.Plans {
		if err := simkit.Object(fmt.Sprintf("plans[%d].itinerary", index), plan.Itinerary); err != nil {
			return seed, err
		}
	}
	for index, item := range seed.StopTimes {
		if item.StopID == "" {
			return seed, fmt.Errorf("stop_times[%d] needs a stop_id", index)
		}
		if err := simkit.Object(fmt.Sprintf("stop_times[%d].stop_time", index), item.StopTime); err != nil {
			return seed, err
		}
	}
	return seed, nil
}

// Server is the running fake.
type Server struct {
	*simkit.Server
	seed    Seed
	archive []byte
	alerts  []byte
}

// Start runs the fake on a loopback port; the caller closes it.
func Start(raw json.RawMessage) (*Server, error) {
	seed, err := parse(raw)
	if err != nil {
		return nil, err
	}
	archive, err := buildArchive(seed)
	if err != nil {
		return nil, err
	}
	s := &Server{seed: seed, archive: archive, alerts: buildAlerts(seed)}
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

func (s *Server) OVapiURL() string      { return s.URL() + OVapiPrefix }
func (s *Server) GTFSURL() string       { return s.URL() + GTFSPrefix }
func (s *Server) TransitousURL() string { return s.URL() + TransitousPrefix }

func buildArchive(seed Seed) ([]byte, error) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	stops := [][]string{{"stop_id", "stop_code", "stop_name", "stop_lat", "stop_lon", "location_type", "platform_code"}}
	for _, stop := range seed.Stops {
		stops = append(stops, []string{stop.ID, stop.Code, stop.Name,
			strconv.FormatFloat(stop.Lat, 'f', 6, 64), strconv.FormatFloat(stop.Lon, 'f', 6, 64), "0", stop.Platform})
	}
	routes := [][]string{{"route_id", "agency_id", "route_short_name", "route_long_name"}}
	for _, route := range seed.Routes {
		routes = append(routes, []string{route.ID, route.Agency, route.Short, route.Long})
	}
	for _, table := range []struct {
		name    string
		records [][]string
	}{{"routes.txt", routes}, {"stops.txt", stops}} {
		writer, err := archive.CreateHeader(&zip.FileHeader{Name: table.name, Method: zip.Deflate})
		if err != nil {
			return nil, err
		}
		if err := csv.NewWriter(writer).WriteAll(table.records); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func buildAlerts(seed Seed) []byte {
	translated := func(text string) []byte {
		var translation []byte
		translation = protowire.AppendTag(translation, 1, protowire.BytesType)
		translation = protowire.AppendString(translation, text)
		translation = protowire.AppendTag(translation, 2, protowire.BytesType)
		translation = protowire.AppendString(translation, "en")
		var message []byte
		message = protowire.AppendTag(message, 1, protowire.BytesType)
		return protowire.AppendBytes(message, translation)
	}
	var feed []byte
	feed = protowire.AppendTag(feed, 1, protowire.BytesType)
	feed = protowire.AppendBytes(feed, []byte{})
	for index, item := range seed.Alerts {
		var message []byte
		if item.From != "" || item.To != "" {
			var period []byte
			if from, err := time.Parse(time.RFC3339, item.From); err == nil {
				period = protowire.AppendTag(period, 1, protowire.VarintType)
				period = protowire.AppendVarint(period, uint64(from.Unix()))
			}
			if to, err := time.Parse(time.RFC3339, item.To); err == nil {
				period = protowire.AppendTag(period, 2, protowire.VarintType)
				period = protowire.AppendVarint(period, uint64(to.Unix()))
			}
			message = protowire.AppendTag(message, 1, protowire.BytesType)
			message = protowire.AppendBytes(message, period)
		}
		for _, informed := range item.Informed {
			var selector []byte
			if informed.RouteID != "" {
				selector = protowire.AppendTag(selector, 2, protowire.BytesType)
				selector = protowire.AppendString(selector, informed.RouteID)
			}
			if informed.StopID != "" {
				selector = protowire.AppendTag(selector, 5, protowire.BytesType)
				selector = protowire.AppendString(selector, informed.StopID)
			}
			message = protowire.AppendTag(message, 5, protowire.BytesType)
			message = protowire.AppendBytes(message, selector)
		}
		message = protowire.AppendTag(message, 7, protowire.VarintType)
		message = protowire.AppendVarint(message, item.Effect)
		message = protowire.AppendTag(message, 10, protowire.BytesType)
		message = protowire.AppendBytes(message, translated(item.Header))
		if item.Description != "" {
			message = protowire.AppendTag(message, 11, protowire.BytesType)
			message = protowire.AppendBytes(message, translated(item.Description))
		}
		var entity []byte
		entity = protowire.AppendTag(entity, 1, protowire.BytesType)
		entity = protowire.AppendString(entity, "alert-"+strconv.Itoa(index))
		entity = protowire.AppendTag(entity, 5, protowire.BytesType)
		entity = protowire.AppendBytes(entity, message)
		feed = protowire.AppendTag(feed, 2, protowire.BytesType)
		feed = protowire.AppendBytes(feed, entity)
	}
	return feed
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	refuse := func(allowed ...string) bool {
		if unknown := simkit.UnknownParameters(query, allowed...); len(unknown) > 0 {
			http.Error(w, "unknown query parameter: "+strings.Join(unknown, ", "), http.StatusBadRequest)
			return true
		}
		return false
	}
	switch path := r.URL.Path; {
	case strings.HasPrefix(path, OVapiPrefix+"/tpc/"):
		if !refuse() {
			s.timingPoints(w, strings.TrimPrefix(path, OVapiPrefix+"/tpc/"))
		}
	case path == GTFSPrefix+"/gtfs-nl.zip":
		http.ServeContent(w, r, "gtfs-nl.zip", time.Time{}, bytes.NewReader(s.archive))
	case path == GTFSPrefix+"/alerts.pb":
		_, _ = w.Write(s.alerts)
	case path == TransitousPrefix+"/v5/plan":
		if !refuse("fromPlace", "toPlace", "time", "numItineraries", "arriveBy") {
			s.plan(w, r)
		}
	case path == TransitousPrefix+"/v5/stoptimes":
		if !refuse("stopId", "time", "n") {
			s.stopTimes(w, r)
		}
	case path == TransitousPrefix+"/v1/geocode":
		if !refuse("text", "language") {
			s.geocode(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) timingPoints(w http.ResponseWriter, codes string) {
	answer := map[string]map[string]map[string]Pass{}
	for requested := range strings.SplitSeq(codes, ",") {
		for _, point := range s.seed.TimingPoints {
			if point.Code == requested {
				answer[requested] = map[string]map[string]Pass{"Passes": point.Passes}
			}
		}
	}
	simkit.WriteJSON(w, http.StatusOK, answer)
}

func near(a, b Point) bool {
	return math.Abs(a.Lat-b.Lat) <= placeRadius && math.Abs(a.Lon-b.Lon) <= placeRadius
}

func parseCoordinates(value string) (Point, bool) {
	latText, lonText, found := strings.Cut(value, ",")
	lat, latErr := strconv.ParseFloat(latText, 64)
	lon, lonErr := strconv.ParseFloat(lonText, 64)
	return Point{lat, lon}, found && latErr == nil && lonErr == nil
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	from, fromOK := parseCoordinates(query.Get("fromPlace"))
	to, toOK := parseCoordinates(query.Get("toPlace"))
	at, err := time.Parse(time.RFC3339, query.Get("time"))
	if !fromOK || !toOK || err != nil {
		http.Error(w, "fromPlace and toPlace must be lat,lon and time RFC 3339", http.StatusBadRequest)
		return
	}
	limit, err := strconv.Atoi(query.Get("numItineraries"))
	if err != nil || limit < 1 {
		limit = 5
	}
	arriveBy := query.Get("arriveBy") == "true"
	itineraries := []json.RawMessage{}
	for _, plan := range s.seed.Plans {
		if !near(plan.From, from) || !near(plan.To, to) {
			continue
		}
		var times struct {
			StartTime string `json:"startTime"`
			EndTime   string `json:"endTime"`
		}
		_ = json.Unmarshal(plan.Itinerary, &times)
		start, _ := time.Parse(time.RFC3339, times.StartTime)
		end, _ := time.Parse(time.RFC3339, times.EndTime)
		if (arriveBy && end.After(at)) || (!arriveBy && start.Before(at)) {
			continue
		}
		itineraries = append(itineraries, plan.Itinerary)
	}
	itineraries = itineraries[:min(len(itineraries), limit)]
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"itineraries": itineraries})
}

func (s *Server) stopTimes(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	stopID, prefixed := strings.CutPrefix(query.Get("stopId"), openOVPrefix)
	at, err := time.Parse(time.RFC3339, query.Get("time"))
	if !prefixed || err != nil {
		http.Error(w, "stopId must be a known stop and time RFC 3339", http.StatusBadRequest)
		return
	}
	limit, err := strconv.Atoi(query.Get("n"))
	if err != nil || limit < 1 {
		limit = 10
	}
	group := s.platformsOf(stopID)
	rows := []json.RawMessage{}
	for _, item := range s.seed.StopTimes {
		if !slices.Contains(group, item.StopID) {
			continue
		}
		var place struct {
			Place struct {
				Departure          string `json:"departure"`
				ScheduledDeparture string `json:"scheduledDeparture"`
			} `json:"place"`
		}
		_ = json.Unmarshal(item.StopTime, &place)
		departs, _ := time.Parse(time.RFC3339, place.Place.Departure)
		if departs.IsZero() {
			departs, _ = time.Parse(time.RFC3339, place.Place.ScheduledDeparture)
		}
		if departs.Before(at) {
			continue
		}
		rows = append(rows, item.StopTime)
	}
	rows = rows[:min(len(rows), limit)]
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"stopTimes": rows})
}

// platformsOf lists the IDs of every platform that shares a name with the given one, since
// Transitous answers for the whole stop.
func (s *Server) platformsOf(id string) []string {
	ids := []string{id}
	index := slices.IndexFunc(s.seed.Stops, func(stop Stop) bool { return stop.ID == id })
	if index < 0 {
		return ids
	}
	for _, stop := range s.seed.Stops {
		if stop.Name == s.seed.Stops[index].Name && stop.ID != id {
			ids = append(ids, stop.ID)
		}
	}
	return ids
}

func (s *Server) geocode(w http.ResponseWriter, r *http.Request) {
	text := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("text")))
	matches := []Place{}
	for _, place := range s.seed.Places {
		name := strings.ToLower(place.Name)
		if text != "" && (strings.Contains(name, text) || strings.Contains(text, name)) {
			matches = append(matches, place)
		}
	}
	simkit.WriteJSON(w, http.StatusOK, matches)
}

// Package placessim is a loopback fake of the two services the places connector reads: an
// Overpass interpreter that answers the connector's own query from seeded map elements, and the
// Transitous reverse geocoder that names a point.
//
// The Overpass fake reads the query the connector sends: each "nwr" clause's tag selectors and its
// "around" radius and centre. An element answers a clause when it carries every selector's tag and
// lies within the radius, so a case controls what exists near a point by placing elements and
// tags. A form field other than "data", and a query the fake cannot read, are refused with a 400.
//
// Example seed (fictional town and street):
//
//	{
//	  "elements": [
//	    {"lat": 52.1010, "lon": 5.1020, "tags": {"amenity": "pharmacy", "name": "Eastbrook Pharmacy",
//	      "addr:street": "Market Street", "addr:housenumber": "4", "opening_hours": "Mo-Fr 09:00-18:00"}}
//	  ],
//	  "reverse": [{"lat": 52.1, "lon": 5.1, "type": "ADDRESS", "street": "Market Street", "house_number": "1",
//	    "areas": [{"name": "Eastbrook", "admin_level": 8, "default": true}]}]
//	}
package placessim

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

const (
	// OverpassPath is the interpreter's path under the server's URL.
	OverpassPath = "/api/interpreter"
	// TransitousPrefix is the path under the server's URL the connector's TransitousURL takes.
	TransitousPrefix = "/transitous"

	reverseSlackMeters = 100
	earthRadiusMeters  = 6371000
)

type Seed struct {
	Elements []Element `json:"elements"`
	Reverse  []Reverse `json:"reverse"`
}

// Element is one map object: a node at Lat and Lon, with its OpenStreetMap tags.
type Element struct {
	Lat  float64           `json:"lat"`
	Lon  float64           `json:"lon"`
	Tags map[string]string `json:"tags"`
}

// Reverse is what the reverse geocoder says about the point nearest it, within 100 m.
type Reverse struct {
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Type        string  `json:"type"`
	Name        string  `json:"name"`
	Street      string  `json:"street"`
	HouseNumber string  `json:"house_number"`
	Areas       []Area  `json:"areas"`
}

type Area struct {
	Name       string  `json:"name"`
	AdminLevel float64 `json:"admin_level"`
	Default    bool    `json:"default"`
	Zip        bool    `json:"zip"`
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
	for index, element := range seed.Elements {
		if len(element.Tags) == 0 {
			return seed, fmt.Errorf("elements[%d] needs tags", index)
		}
	}
	for index, item := range seed.Reverse {
		switch item.Type {
		case "ADDRESS", "PLACE", "STOP":
		default:
			return seed, fmt.Errorf("reverse[%d] type must be ADDRESS, PLACE or STOP", index)
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

// OverpassURL is the address for the connector's OverpassURL.
func (s *Server) OverpassURL() string { return s.URL() + OverpassPath }

// TransitousURL is the address for the connector's TransitousURL.
func (s *Server) TransitousURL() string { return s.URL() + TransitousPrefix }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == OverpassPath && r.Method == http.MethodPost:
		s.overpass(w, r)
	case r.URL.Path == TransitousPrefix+"/v1/reverse-geocode" && r.Method == http.MethodGet:
		s.reverse(w, r)
	default:
		http.NotFound(w, r)
	}
}

var (
	clausePattern   = regexp.MustCompile(`nwr((?:\["[^"]*"="[^"]*"\])*)\(around:(\d+),(-?[\d.]+),(-?[\d.]+)\);`)
	selectorPattern = regexp.MustCompile(`\["([^"]*)"="([^"]*)"\]`)
)

func (s *Server) overpass(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "unreadable form", http.StatusBadRequest)
		return
	}
	if unknown := simkit.UnknownParameters(r.PostForm, "data"); len(unknown) > 0 {
		http.Error(w, "unknown form field: "+strings.Join(unknown, ", "), http.StatusBadRequest)
		return
	}
	clauses := clausePattern.FindAllStringSubmatch(r.PostForm.Get("data"), -1)
	if len(clauses) == 0 {
		http.Error(w, "the query has no nwr clause this server can read", http.StatusBadRequest)
		return
	}
	elements := []map[string]any{}
	for _, element := range s.seed.Elements {
		for _, clause := range clauses {
			radius, _ := strconv.ParseFloat(clause[2], 64)
			lat, _ := strconv.ParseFloat(clause[3], 64)
			lon, _ := strconv.ParseFloat(clause[4], 64)
			if matches(element, clause[1]) && distance(lat, lon, element.Lat, element.Lon) <= radius {
				elements = append(elements, map[string]any{"type": "node", "lat": element.Lat, "lon": element.Lon, "tags": element.Tags})
				break
			}
		}
	}
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"version": 0.6, "elements": elements})
}

func matches(element Element, selectors string) bool {
	for _, selector := range selectorPattern.FindAllStringSubmatch(selectors, -1) {
		if element.Tags[selector[1]] != selector[2] {
			return false
		}
	}
	return true
}

func distance(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dLat, dLon := p2-p1, (lon2-lon1)*math.Pi/180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusMeters * math.Asin(math.Min(1, math.Sqrt(a)))
}

func (s *Server) reverse(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if unknown := simkit.UnknownParameters(query, "place"); len(unknown) > 0 {
		http.Error(w, "unknown query parameter: "+strings.Join(unknown, ", "), http.StatusBadRequest)
		return
	}
	latText, lonText, _ := strings.Cut(query.Get("place"), ",")
	lat, latErr := strconv.ParseFloat(latText, 64)
	lon, lonErr := strconv.ParseFloat(lonText, 64)
	if latErr != nil || lonErr != nil {
		http.Error(w, "place must be lat,lon", http.StatusBadRequest)
		return
	}
	matches := []map[string]any{}
	for _, item := range s.seed.Reverse {
		if distance(lat, lon, item.Lat, item.Lon) > reverseSlackMeters {
			continue
		}
		areas := []map[string]any{}
		for _, area := range item.Areas {
			areas = append(areas, map[string]any{"name": area.Name, "adminLevel": area.AdminLevel, "default": area.Default, "zip": area.Zip})
		}
		matches = append(matches, map[string]any{
			"type": item.Type, "name": item.Name, "street": item.Street, "houseNumber": item.HouseNumber, "areas": areas,
		})
	}
	simkit.WriteJSON(w, http.StatusOK, matches)
}

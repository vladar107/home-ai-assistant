package bus

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	// Transitous identifies Dutch GTFS objects as the feed's own identifiers behind this prefix,
	// which is how its stops and lines meet the alert feed.
	openOVPrefix = "nl-OpenOV_"

	geocodeTypeStop = "STOP"
	homeCountry     = "NL"
	modeWalk        = "WALK"
)

type place struct {
	name     string
	lat, lon float64
	// group is the directory stop the place was resolved to, if it was one.
	group *stopGroup
}

func (p place) coordinates() string {
	return strconv.FormatFloat(p.lat, 'f', 6, 64) + "," + strconv.FormatFloat(p.lon, 'f', 6, 64)
}

type planStop struct {
	Name               string  `json:"name"`
	StopID             string  `json:"stopId"`
	Lat                float64 `json:"lat"`
	Lon                float64 `json:"lon"`
	Departure          string  `json:"departure"`
	ScheduledDeparture string  `json:"scheduledDeparture"`
	Arrival            string  `json:"arrival"`
	ScheduledArrival   string  `json:"scheduledArrival"`
	Track              string  `json:"track"`
	ScheduledTrack     string  `json:"scheduledTrack"`
	Cancelled          bool    `json:"cancelled"`
}

type planLeg struct {
	Mode               string   `json:"mode"`
	From               planStop `json:"from"`
	To                 planStop `json:"to"`
	Duration           int      `json:"duration"`
	StartTime          string   `json:"startTime"`
	EndTime            string   `json:"endTime"`
	ScheduledStartTime string   `json:"scheduledStartTime"`
	ScheduledEndTime   string   `json:"scheduledEndTime"`
	RealTime           bool     `json:"realTime"`
	Headsign           string   `json:"headsign"`
	RouteID            string   `json:"routeId"`
	RouteShortName     string   `json:"routeShortName"`
	DisplayName        string   `json:"displayName"`
	AgencyName         string   `json:"agencyName"`
	Cancelled          bool     `json:"cancelled"`
}

type itinerary struct {
	Duration  int       `json:"duration"`
	StartTime string    `json:"startTime"`
	EndTime   string    `json:"endTime"`
	Transfers int       `json:"transfers"`
	Legs      []planLeg `json:"legs"`
}

type stopTime struct {
	Place          planStop `json:"place"`
	Mode           string   `json:"mode"`
	RealTime       bool     `json:"realTime"`
	Headsign       string   `json:"headsign"`
	RouteID        string   `json:"routeId"`
	RouteShortName string   `json:"routeShortName"`
	DisplayName    string   `json:"displayName"`
	AgencyName     string   `json:"agencyName"`
	Cancelled      bool     `json:"cancelled"`
	TripCancelled  bool     `json:"tripCancelled"`
}

type stopTimesResponse struct {
	StopTimes []stopTime `json:"stopTimes"`
}

type planResponse struct {
	Itineraries []itinerary `json:"itineraries"`
}

type geocodeMatch struct {
	Type    string  `json:"type"`
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Country string  `json:"country"`
}

// plan asks Transitous for itineraries between two places, cached briefly by request.
func (s *Service) plan(ctx context.Context, from, to place, at time.Time, arriveBy bool, limit int) (planResponse, error) {
	query := url.Values{
		"fromPlace":      {from.coordinates()},
		"toPlace":        {to.coordinates()},
		"time":           {at.UTC().Format(time.RFC3339)},
		"numItineraries": {strconv.Itoa(limit)},
	}
	if arriveBy {
		query.Set("arriveBy", "true")
	}
	key := "plan|" + query.Encode()
	if cached, found := s.cached(key); found {
		return cached.(planResponse), nil
	}
	var response planResponse
	if err := s.transitous.getJSON(ctx, "v5/plan", query, &response); err != nil {
		return planResponse{}, err
	}
	s.store(key, response, planLifetime)
	return response, nil
}

// stopTimes asks Transitous for the next departures at a stop, which it reads for all platforms
// of the stop the given one belongs to.
func (s *Service) stopTimes(ctx context.Context, group *stopGroup, at time.Time, limit int) (stopTimesResponse, error) {
	query := url.Values{
		"stopId": {openOVPrefix + group.stopIDs[0]},
		"time":   {at.UTC().Format(time.RFC3339)},
		"n":      {strconv.Itoa(limit)},
	}
	key := "stoptimes|" + query.Encode()
	if cached, found := s.cached(key); found {
		return cached.(stopTimesResponse), nil
	}
	var response stopTimesResponse
	if err := s.transitous.getJSON(ctx, "v5/stoptimes", query, &response); err != nil {
		return stopTimesResponse{}, err
	}
	s.store(key, response, tpcLifetime)
	return response, nil
}

// geocode finds a place the stop directory does not know, such as a railway station or an address,
// preferring a Dutch stop.
func (s *Service) geocode(ctx context.Context, text string) (place, bool, error) {
	key := "geocode|" + transit.Normalize(text)
	if cached, found := s.cached(key); found {
		match := cached.(place)
		return match, match.name != "", nil
	}
	var matches []geocodeMatch
	if err := s.transitous.getJSON(ctx, "v1/geocode", url.Values{"text": {text}, "language": {"en"}}, &matches); err != nil {
		return place{}, false, err
	}
	var chosen place
	for _, preferred := range []func(geocodeMatch) bool{
		func(m geocodeMatch) bool { return m.Type == geocodeTypeStop && m.Country == homeCountry },
		func(m geocodeMatch) bool { return m.Country == homeCountry },
		func(geocodeMatch) bool { return true },
	} {
		for _, match := range matches {
			if preferred(match) && strings.TrimSpace(match.Name) != "" {
				chosen = place{name: clip.Runes(strings.TrimSpace(match.Name), maximumStopRunes), lat: match.Lat, lon: match.Lon}
				break
			}
		}
		if chosen.name != "" {
			break
		}
	}
	s.store(key, chosen, geocodeLifetime)
	return chosen, chosen.name != "", nil
}

func parseUTC(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

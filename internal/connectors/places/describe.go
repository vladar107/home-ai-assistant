package places

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
)

const (
	describeTimeout  = 5 * time.Second
	describeLifetime = 24 * time.Hour
	describeGridM    = 50

	describeFailedKey       = "describe|failed"
	describeFailureLifetime = time.Minute

	municipalityLevel = 8
	maximumNameRunes  = 80
)

type geocodeArea struct {
	Name       string  `json:"name"`
	AdminLevel float64 `json:"adminLevel"`
	Default    bool    `json:"default"`
	Zip        bool    `json:"zip"`
}

type geocodeMatch struct {
	Type        string        `json:"type"`
	Name        string        `json:"name"`
	Street      string        `json:"street"`
	HouseNumber string        `json:"houseNumber"`
	Areas       []geocodeArea `json:"areas"`
}

var matchRank = map[string]int{"ADDRESS": 0, "PLACE": 1, "STOP": 2}

// Describe names the point as a short place such as "Stationsplein 1, Leiden", or "" when
// Transitous knows nothing there. It never logs the point or the name.
func (s *Service) Describe(ctx context.Context, lat, lon float64) (string, error) {
	if !validPoint(lat, lon) {
		return "", fmt.Errorf("coordinates are out of range")
	}
	key := "describe|" + snapKey(lat, lon, describeGridM)
	if cached, found := s.cached(key); found {
		return cached.(string), nil
	}
	if failed, found := s.cached(describeFailedKey); found {
		return "", failed.(error)
	}
	requestCtx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	var matches []geocodeMatch
	query := url.Values{"place": {coordinates(lat, lon)}}
	if err := s.getJSON(requestCtx, s.transitous, "v1/reverse-geocode", query, &matches); err != nil {
		s.logger.InfoContext(ctx, "places.describe.failed", "error_class", errorClass(err))
		// Every turn names the user's position before the model starts, so while the service is
		// down a failure is remembered for every point, and turns do not each wait out the timeout.
		if ctx.Err() == nil {
			s.store(describeFailedKey, err, describeFailureLifetime)
		}
		return "", err
	}
	text := describeMatches(matches)
	s.store(key, text, describeLifetime)
	return text, nil
}

func describeMatches(matches []geocodeMatch) string {
	var best *geocodeMatch
	for i := range matches {
		rank, known := matchRank[matches[i].Type]
		if !known || label(matches[i]) == "" {
			continue
		}
		if best == nil || rank < matchRank[best.Type] {
			best = &matches[i]
		}
	}
	if best == nil {
		return ""
	}
	text := label(*best)
	if place := locality(best.Areas); place != "" && !strings.EqualFold(place, text) {
		text += ", " + place
	}
	return text
}

func label(match geocodeMatch) string {
	street := clean(match.Street)
	if street == "" {
		return clean(match.Name)
	}
	return strings.TrimSpace(street + " " + clean(match.HouseNumber))
}

// locality prefers the municipality, which is the town a person would name, then the area
// Transitous marks as the default, then any named area.
func locality(areas []geocodeArea) string {
	var fallback string
	for _, area := range areas {
		name := clean(area.Name)
		if name == "" || area.Zip {
			continue
		}
		if area.AdminLevel == municipalityLevel {
			return name
		}
		if area.Default && fallback == "" {
			fallback = name
		}
	}
	if fallback != "" {
		return fallback
	}
	for _, area := range areas {
		if name := clean(area.Name); name != "" && !area.Zip {
			return name
		}
	}
	return ""
}

func clean(value string) string {
	return clip.Runes(strings.Join(strings.Fields(value), " "), maximumNameRunes)
}

func validPoint(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

func coordinates(lat, lon float64) string {
	return strconv.FormatFloat(lat, 'f', 6, 64) + "," + strconv.FormatFloat(lon, 'f', 6, 64)
}

// snapKey rounds a point to a grid of about the given size in metres, widening the longitude step
// toward the poles so cells stay roughly square.
func snapKey(lat, lon float64, metres float64) string {
	const metresPerDegree = 111_320
	latStep := metres / metresPerDegree
	snappedLat := math.Round(lat/latStep) * latStep
	lonStep := latStep / math.Max(math.Cos(snappedLat*math.Pi/180), 0.01)
	snappedLon := math.Round(lon/lonStep) * lonStep
	return strconv.FormatFloat(snappedLat, 'f', 5, 64) + "," + strconv.FormatFloat(snappedLon, 'f', 5, 64)
}

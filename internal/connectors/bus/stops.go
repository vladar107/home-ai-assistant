package bus

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	directoryLifetime = 24 * time.Hour
	// directoryRetryDelay keeps a failing refresh from costing a download on every call while the
	// stale directory still answers.
	directoryRetryDelay = 15 * time.Minute

	gtfsArchive = "gtfs-nl.zip"
	// stops.txt was 1.5 MB compressed and 5.6 MB unzipped on 2026-10-02.
	maximumEntryBytes = 8 << 20
	// rangeBudget bounds what one directory refresh may download: the archive's directory, both
	// entries, and the blocks they straddle.
	rangeBudget = 24 << 20

	maximumCandidates = 8
	maximumStopRunes  = 100
	// samePlaceMeters is how close exact matches must lie to count as one place, such as the
	// railway platforms and the bus stop of one station, which the feed names differently.
	samePlaceMeters = 500
)

// stopGroup is one named stop: every platform the GTFS feed lists under the same name, which is
// how travellers think of a stop, even where the feed splits it across stop areas.
type stopGroup struct {
	name string
	// codes are the timing point codes OVapi knows the platforms by, and platforms their letters.
	codes     []string
	platforms map[string]string
	// stopIDs are the GTFS stop identifiers alerts and Transitous use for the same platforms; a
	// group always has at least one.
	stopIDs  []string
	lat, lon float64
	forms    []string
}

type route struct {
	id     string
	agency string
	short  string
	long   string
}

// stopIndex is the national stop and route directory built from the GTFS feed.
type stopIndex struct {
	groups   []*stopGroup
	byCode   map[string]*stopGroup
	byStopID map[string]*stopGroup
	routes   map[string]route
}

// fetchStops builds the stop index for the directory.
func (s *Service) fetchStops(ctx context.Context) (*stopIndex, error) {
	tables, err := s.gtfs.directoryTables(ctx)
	if err != nil {
		return nil, err
	}
	return buildIndex(tables)
}

type gtfsTables struct {
	stops  [][]string
	routes [][]string
}

// directoryTables reads stops.txt and routes.txt out of the national GTFS archive by range
// requests, without downloading the rest of it.
func (c *client) directoryTables(ctx context.Context) (gtfsTables, error) {
	reader, err := c.openRange(ctx, gtfsArchive, rangeBudget)
	if err != nil {
		return gtfsTables{}, err
	}
	archive, err := zip.NewReader(reader, reader.Size())
	if err != nil {
		return gtfsTables{}, archiveError(err)
	}
	var tables gtfsTables
	if tables.stops, err = readTable(archive, "stops.txt"); err != nil {
		return gtfsTables{}, err
	}
	if tables.routes, err = readTable(archive, "routes.txt"); err != nil {
		return gtfsTables{}, err
	}
	return tables, nil
}

func readTable(archive *zip.Reader, name string) ([][]string, error) {
	for _, file := range archive.File {
		if file.Name != name {
			continue
		}
		if file.UncompressedSize64 > maximumEntryBytes {
			return nil, errMalformed
		}
		opened, err := file.Open()
		if err != nil {
			return nil, archiveError(err)
		}
		defer opened.Close()
		reader := csv.NewReader(io.LimitReader(opened, maximumEntryBytes))
		reader.FieldsPerRecord = -1
		reader.LazyQuotes = true
		reader.ReuseRecord = false
		records, err := reader.ReadAll()
		if err != nil {
			return nil, archiveError(err)
		}
		return records, nil
	}
	return nil, errMalformed
}

// archiveError keeps a request failure's class and reduces anything else the zip or CSV reader
// reports to a malformed archive.
func archiveError(err error) error {
	var failure *apiError
	if errors.As(err, &failure) {
		return failure
	}
	return errMalformed
}

func buildIndex(tables gtfsTables) (*stopIndex, error) {
	stops, err := columns(tables.stops, "stop_id", "stop_code", "stop_name", "stop_lat", "stop_lon", "location_type")
	if err != nil {
		return nil, err
	}
	routes, err := columns(tables.routes, "route_id", "agency_id", "route_short_name", "route_long_name")
	if err != nil {
		return nil, err
	}

	index := &stopIndex{
		byCode:   make(map[string]*stopGroup),
		byStopID: make(map[string]*stopGroup),
		routes:   make(map[string]route),
	}
	byName := make(map[string]*stopGroup)
	counts := make(map[*stopGroup]float64)
	for _, record := range tables.stops[1:] {
		if kind := stops.get(record, "location_type"); kind != "" && kind != "0" {
			continue
		}
		name := strings.TrimSpace(stops.get(record, "stop_name"))
		id := stops.get(record, "stop_id")
		if name == "" || id == "" {
			continue
		}
		group := byName[name]
		if group == nil {
			group = &stopGroup{name: name, forms: nameForms(name), platforms: make(map[string]string)}
			byName[name] = group
		}
		group.stopIDs = append(group.stopIDs, id)
		if code := stops.get(record, "stop_code"); digitsOnly(code) {
			group.codes = append(group.codes, code)
			if platform := stops.get(record, "platform_code"); platform != "" {
				group.platforms[code] = clip(platform, 10)
			}
		}
		lat, latErr := strconv.ParseFloat(stops.get(record, "stop_lat"), 64)
		lon, lonErr := strconv.ParseFloat(stops.get(record, "stop_lon"), 64)
		if latErr == nil && lonErr == nil {
			counts[group]++
			group.lat += (lat - group.lat) / counts[group]
			group.lon += (lon - group.lon) / counts[group]
		}
	}
	// A stop without timing point codes has no live OVapi data,
	// but Transitous still answers for it.
	for _, group := range byName {
		if counts[group] == 0 {
			continue
		}
		index.groups = append(index.groups, group)
		for _, code := range group.codes {
			index.byCode[code] = group
		}
		for _, id := range group.stopIDs {
			index.byStopID[id] = group
		}
	}
	if len(index.groups) == 0 {
		return nil, errMalformed
	}
	sort.Slice(index.groups, func(i, j int) bool { return index.groups[i].name < index.groups[j].name })

	for _, record := range tables.routes[1:] {
		entry := route{
			id:     routes.get(record, "route_id"),
			agency: routes.get(record, "agency_id"),
			short:  strings.TrimSpace(routes.get(record, "route_short_name")),
			long:   strings.TrimSpace(routes.get(record, "route_long_name")),
		}
		if entry.id != "" {
			index.routes[entry.id] = entry
		}
	}
	return index, nil
}

type columnIndex map[string]int

func columns(table [][]string, required ...string) (columnIndex, error) {
	if len(table) == 0 {
		return nil, errMalformed
	}
	found := make(columnIndex)
	for position, name := range table[0] {
		found[strings.TrimPrefix(strings.TrimSpace(name), "\ufeff")] = position
	}
	for _, name := range required {
		if _, ok := found[name]; !ok {
			return nil, errMalformed
		}
	}
	return found, nil
}

func (c columnIndex) get(record []string, name string) string {
	position, ok := c[name]
	if !ok || position >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[position])
}

// nameForms are the ways a stop can be asked for: GTFS writes "Town, Stop name",
// people say "Town Stop name", or just "Stop name" when the town is obvious.
func nameForms(name string) []string {
	forms := []string{transit.Normalize(name)}
	if town, rest, found := strings.Cut(name, ","); found {
		town, rest = transit.Normalize(town), transit.Normalize(rest)
		if rest != "" {
			forms = append(forms, rest)
			if !strings.HasPrefix(rest, town+" ") && rest != town {
				forms = append(forms, town+" "+rest)
			}
		}
	}
	return forms
}

// resolution is a stop name matched against the directory: one stop, or the candidates the model
// has to choose from.
type resolution struct {
	group      *stopGroup
	candidates []*stopGroup
}

func (i *stopIndex) resolve(query string) resolution {
	trimmed := strings.TrimSpace(query)
	if group, found := i.byCode[trimmed]; found {
		return resolution{group: group}
	}
	normalized := transit.Normalize(trimmed)
	if normalized == "" {
		return resolution{}
	}
	matchers := []func(string) bool{
		func(form string) bool { return form == normalized },
		func(form string) bool { return strings.HasPrefix(form, normalized+" ") },
		func(form string) bool { return strings.HasPrefix(form, normalized) },
		func(form string) bool { return strings.Contains(form, normalized) },
	}
	for tier, matches := range matchers {
		exact := tier == 0
		var hits []*stopGroup
		for _, group := range i.groups {
			for _, form := range group.forms {
				if matches(form) {
					hits = append(hits, group)
					break
				}
			}
		}
		if len(hits) == 0 {
			continue
		}
		if len(hits) == 1 {
			return resolution{group: hits[0]}
		}
		if exact && samePlace(hits) {
			return resolution{group: busiest(hits)}
		}
		if len(hits) > maximumCandidates {
			hits = hits[:maximumCandidates]
		}
		return resolution{candidates: hits}
	}
	return resolution{}
}

func samePlace(groups []*stopGroup) bool {
	for _, a := range groups {
		for _, b := range groups {
			if distanceMeters(a.lat, a.lon, b.lat, b.lon) > samePlaceMeters {
				return false
			}
		}
	}
	return true
}

// busiest prefers the stop OVapi knows most platforms of, then the one with most platforms.
func busiest(groups []*stopGroup) *stopGroup {
	best := groups[0]
	for _, group := range groups[1:] {
		if len(group.codes) > len(best.codes) || (len(group.codes) == len(best.codes) && len(group.stopIDs) > len(best.stopIDs)) {
			best = group
		}
	}
	return best
}

func distanceMeters(latA, lonA, latB, lonB float64) float64 {
	const earthRadius = 6371000
	toRadians := math.Pi / 180
	dLat, dLon := (latB-latA)*toRadians, (lonB-lonA)*toRadians
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(latA*toRadians)*math.Cos(latB*toRadians)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadius * math.Asin(math.Sqrt(h))
}

var coordinatesPattern = regexp.MustCompile(`^([+-]?[0-9]+(?:\.[0-9]+)?)\s*,\s*([+-]?[0-9]+(?:\.[0-9]+)?)$`)

// parseCoordinates reads a "lat,lon" query in decimal degrees. matched tells the query has that
// shape, in which case err says whether the numbers are a point on Earth.
func parseCoordinates(query string) (lat, lon float64, matched bool, err error) {
	parts := coordinatesPattern.FindStringSubmatch(strings.TrimSpace(query))
	if parts == nil {
		return 0, 0, false, nil
	}
	lat, latErr := strconv.ParseFloat(parts[1], 64)
	lon, lonErr := strconv.ParseFloat(parts[2], 64)
	if latErr != nil || lonErr != nil || math.IsInf(lat, 0) || math.IsInf(lon, 0) || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		return 0, 0, true, errors.New("coordinates must be decimal degrees as lat,lon, with latitude within 90 and longitude within 180")
	}
	return lat, lon, true, nil
}

// nearbyStop is a stop group with its distance from a point.
type nearbyStop struct {
	group  *stopGroup
	meters float64
}

// nearest lists the stop groups within a radius of a point, closest first.
func (i *stopIndex) nearest(lat, lon, radius float64) []nearbyStop {
	var found []nearbyStop
	for _, group := range i.groups {
		if meters := distanceMeters(lat, lon, group.lat, group.lon); meters <= radius {
			found = append(found, nearbyStop{group: group, meters: meters})
		}
	}
	sort.SliceStable(found, func(a, b int) bool { return found[a].meters < found[b].meters })
	return found
}

// resolutionError is the sentence the model sees when a name does not pick out one stop.
func resolutionError(role, query string, match resolution) string {
	if len(match.candidates) == 0 {
		return fmt.Sprintf("No bus stop matches %s %q; ask the user for the stop's name, written as \"Town, Stop\".", role, clip(query, maximumStopRunes))
	}
	names := make([]string, 0, len(match.candidates))
	for _, candidate := range match.candidates {
		names = append(names, candidate.name)
	}
	return fmt.Sprintf("%s %q matches several bus stops: %s. Call again with the full name of the one the user means, or ask them.",
		role, clip(query, maximumStopRunes), strings.Join(names, "; "))
}

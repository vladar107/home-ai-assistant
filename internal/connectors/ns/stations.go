package ns

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/transit"
)

const (
	stationListLifetime = 24 * time.Hour
	// stationRetryDelay keeps a failing refresh from costing a request on every call while the
	// stale list still answers.
	stationRetryDelay     = 15 * time.Minute
	maximumCandidates     = 8
	maximumStationRunes   = 100
	homeCountry           = "NL"
	centralStationSuffix  = " centraal"
	minimumNamedFormBytes = 3
)

type station struct {
	code  string
	uic   string
	name  string
	land  string
	forms []string
}

// fetchStations reads the NS station list for the directory.
func (s *Service) fetchStations(ctx context.Context) ([]station, error) {
	records, err := s.client.stations(ctx)
	if err != nil {
		return nil, err
	}
	if list := stationsFrom(records); len(list) > 0 {
		return list, nil
	}
	return nil, errMalformed
}

func stationsFrom(records []stationRecord) []station {
	list := make([]station, 0, len(records))
	for _, record := range records {
		code := strings.TrimSpace(record.Code)
		name := strings.TrimSpace(record.Namen.Lang)
		if code == "" || name == "" {
			continue
		}
		entry := station{code: code, uic: record.UICCode, name: name, land: record.Land}
		for _, form := range append([]string{record.Namen.Lang, record.Namen.Middel, record.Namen.Kort}, record.Synoniemen...) {
			if normalized := transit.Normalize(form); normalized != "" {
				entry.forms = append(entry.forms, normalized)
			}
		}
		list = append(list, entry)
	}
	return list
}

// resolution is a station name matched against the directory: one station, or the candidates the
// model has to choose from.
type resolution struct {
	station    station
	found      bool
	candidates []station
	// named lists the stations whose names occur inside a query that matched none, such as the
	// town in a bus stop's name.
	named []station
}

func resolveStation(list []station, query string) resolution {
	trimmed := strings.TrimSpace(query)
	for _, entry := range list {
		if strings.EqualFold(entry.code, trimmed) {
			return resolution{station: entry, found: true}
		}
	}
	normalized := transit.Normalize(trimmed)
	if normalized == "" {
		return resolution{}
	}
	matchers := []func(string) bool{
		func(form string) bool { return form == normalized },
		// A bare city name means its central station, as travellers use it.
		func(form string) bool { return form == normalized+centralStationSuffix },
		func(form string) bool { return strings.HasPrefix(form, normalized) },
		func(form string) bool { return strings.Contains(form, normalized) },
	}
	for _, matches := range matchers {
		var hits []station
		for _, entry := range list {
			for _, form := range entry.forms {
				if matches(form) {
					hits = append(hits, entry)
					break
				}
			}
		}
		if len(hits) == 0 {
			continue
		}
		if domestic := inCountry(hits, homeCountry); len(domestic) > 0 {
			hits = domestic
		}
		if len(hits) == 1 {
			return resolution{station: hits[0], found: true}
		}
		if len(hits) > maximumCandidates {
			hits = hits[:maximumCandidates]
		}
		return resolution{candidates: hits}
	}
	return resolution{named: namedStations(list, normalized)}
}

// namedStations finds the stations one of whose forms occurs as whole words in a normalized query.
// A station whose form lies inside another hit's longer form is dropped, so "amsterdam science
// park" names Amsterdam Science Park and not every station called Amsterdam.
func namedStations(list []station, normalized string) []station {
	padded := " " + normalized + " "
	type hit struct {
		entry station
		form  string
	}
	var found []hit
	for _, entry := range list {
		longest := ""
		for _, form := range entry.forms {
			// Two-letter abbreviations would match ordinary words in a stop's name.
			if len(form) >= minimumNamedFormBytes && len(form) > len(longest) && strings.Contains(padded, " "+form+" ") {
				longest = form
			}
		}
		if longest != "" {
			found = append(found, hit{entry, longest})
		}
	}
	if domestic := slices.DeleteFunc(slices.Clone(found), func(h hit) bool { return h.entry.land != homeCountry }); len(domestic) > 0 {
		found = domestic
	}
	var hits []station
	for _, candidate := range found {
		covered := false
		for _, other := range found {
			if len(other.form) > len(candidate.form) && strings.Contains(" "+other.form+" ", " "+candidate.form+" ") {
				covered = true
				break
			}
		}
		if !covered {
			hits = append(hits, candidate.entry)
		}
	}
	if len(hits) > maximumCandidates {
		hits = hits[:maximumCandidates]
	}
	return hits
}

func inCountry(list []station, land string) []station {
	var kept []station
	for _, entry := range list {
		if entry.land == land {
			kept = append(kept, entry)
		}
	}
	return kept
}

// resolutionError is the sentence the model sees when a name does not pick out one station.
func resolutionError(role, query string, match resolution) string {
	if len(match.candidates) == 0 {
		text := fmt.Sprintf("No NS station matches %s %q; NS tools only know railway stations, so plan a leg to or from a bus, tram, or metro stop separately.",
			role, clip.Runes(query, maximumStationRunes))
		switch len(match.named) {
		case 0:
			return text + " Ask the user for the railway station's name."
		case 1:
			return text + fmt.Sprintf(" The railway station named in it is %s; call again with it if the user takes the train there, or ask them.", stationNames(match.named))
		default:
			return text + fmt.Sprintf(" Railway stations named in it: %s; call again with the one the user takes the train from or to, or ask them.", stationNames(match.named))
		}
	}
	return fmt.Sprintf("%s %q matches several NS stations: %s. Call again with the name or code of the one the user means, or ask them.",
		role, clip.Runes(query, maximumStationRunes), stationNames(match.candidates))
}

func stationNames(list []station) string {
	names := make([]string, 0, len(list))
	for _, entry := range list {
		names = append(names, fmt.Sprintf("%s (%s)", entry.name, entry.code))
	}
	return strings.Join(names, ", ")
}

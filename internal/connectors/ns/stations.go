package ns

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	stationListLifetime = 24 * time.Hour
	// stationRetryDelay keeps a failing refresh from costing a request on every call while the
	// stale list still answers.
	stationRetryDelay    = 15 * time.Minute
	maximumCandidates    = 8
	maximumStationRunes  = 100
	homeCountry          = "NL"
	centralStationSuffix = " centraal"
)

type station struct {
	code  string
	uic   string
	name  string
	land  string
	forms []string
}

// directory is the NS station list, fetched on first use and at most once a day after that.
type directory struct {
	fetch func(context.Context) ([]stationRecord, error)
	now   func() time.Time

	mu         sync.Mutex
	list       []station
	fetchedAt  time.Time
	retryAfter time.Time
	refresh    *refresh
}

// refresh is one fetch of the station list that concurrent callers wait on instead of repeating.
type refresh struct {
	done chan struct{}
	list []station
	err  error
	// abandoned marks a fetch that failed because its caller gave up, which says nothing about NS,
	// so waiters try again themselves.
	abandoned bool
}

// load returns the station list. A refresh that fails while an older list is at hand returns that
// list together with the refresh error, so the caller can log it and carry on.
func (d *directory) load(ctx context.Context) ([]station, error) {
	for {
		d.mu.Lock()
		now := d.now()
		if d.list != nil && (now.Sub(d.fetchedAt) < stationListLifetime || now.Before(d.retryAfter)) {
			list := d.list
			d.mu.Unlock()
			return list, nil
		}
		if running := d.refresh; running != nil {
			stale := d.list
			d.mu.Unlock()
			select {
			case <-running.done:
				if running.abandoned {
					continue
				}
				return running.list, running.err
			case <-ctx.Done():
				return stale, transportError(ctx.Err())
			}
		}
		running := &refresh{done: make(chan struct{})}
		d.refresh = running
		d.mu.Unlock()

		records, err := d.fetch(ctx)
		var list []station
		if err == nil {
			if list = stationsFrom(records); len(list) == 0 {
				err = errMalformed
			}
		}

		d.mu.Lock()
		switch {
		case err == nil:
			d.list, d.fetchedAt, d.retryAfter = list, now, time.Time{}
		case ctx.Err() != nil:
			running.abandoned = true
		case d.list != nil:
			d.retryAfter = now.Add(stationRetryDelay)
		}
		running.list, running.err = d.list, err
		d.refresh = nil
		d.mu.Unlock()
		close(running.done)
		return running.list, running.err
	}
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
			if normalized := normalize(form); normalized != "" {
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
}

func resolveStation(list []station, query string) resolution {
	trimmed := strings.TrimSpace(query)
	for _, entry := range list {
		if strings.EqualFold(entry.code, trimmed) {
			return resolution{station: entry, found: true}
		}
	}
	normalized := normalize(trimmed)
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
	return resolution{}
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
		return fmt.Sprintf("No NS station matches %s %q; ask the user for the station's name.", role, clip(query, maximumStationRunes))
	}
	names := make([]string, 0, len(match.candidates))
	for _, candidate := range match.candidates {
		names = append(names, fmt.Sprintf("%s (%s)", candidate.name, candidate.code))
	}
	return fmt.Sprintf("%s %q matches several NS stations: %s. Call again with the name or code of the one the user means, or ask them.",
		role, clip(query, maximumStationRunes), strings.Join(names, ", "))
}

// normalize folds a station name for matching: lower case, no accents, and punctuation as spaces,
// so "'s-Hertogenbosch" and "s Hertogenbosch" meet.
func normalize(name string) string {
	var builder strings.Builder
	space := false
	for _, r := range strings.ToLower(name) {
		if folded, found := accentFolds[r]; found {
			if space && builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			builder.WriteString(folded)
			space = false
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			builder.WriteRune(r)
			space = false
			continue
		}
		space = true
	}
	return builder.String()
}

var accentFolds = func() map[rune]string {
	folds := map[rune]string{'æ': "ae", 'œ': "oe", 'ß': "ss", 'ø': "o", 'ł': "l", 'đ': "d"}
	for base, accented := range map[string]string{
		"a": "àáâãäåāăą",
		"c": "çćĉċč",
		"e": "èéêëēĕėęě",
		"g": "ĝğġģ",
		"i": "ìíîïĩīĭįı",
		"n": "ñńņňŉ",
		"o": "òóôõöōŏő",
		"r": "ŕŗř",
		"s": "śŝşš",
		"t": "ţťŧ",
		"u": "ùúûüũūŭůűų",
		"y": "ýÿŷ",
		"z": "źżž",
	} {
		for _, r := range accented {
			folds[r] = base
		}
	}
	return folds
}()

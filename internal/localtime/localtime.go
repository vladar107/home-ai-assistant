// Package localtime loads IANA time zones and writes the local time the way model turns read it.
package localtime

import (
	"fmt"
	"time"
)

const layout = "Monday 2 January 2006, 15:04 MST"

// LoadZone resolves an IANA zone name; the binary embeds the zone database.
func LoadZone(zone string) (*time.Location, error) {
	if zone == "" || zone == "Local" {
		return nil, fmt.Errorf("the time zone must be an IANA name such as Europe/Amsterdam")
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("the time zone %q is not known", zone)
	}
	return location, nil
}

// SavedZone resolves a zone that was validated by LoadZone when its record was saved. Should it
// ever stop loading, it is UTC, so times written with the location's own name stay true.
func SavedZone(zone string) *time.Location {
	location, err := LoadZone(zone)
	if err != nil {
		return time.UTC
	}
	return location
}

// Format writes an instant in a zone with its weekday, date, and both the zone's abbreviation and
// its IANA name, such as "Saturday 3 October 2026, 08:00 CEST (Europe/Amsterdam)".
func Format(at time.Time, location *time.Location) string {
	return at.In(location).Format(layout) + " (" + location.String() + ")"
}

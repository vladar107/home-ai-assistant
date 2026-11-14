// Package flightnumber is the one form of a flight's number, which trips store and the flight
// status connector looks up; it imports nothing of the application.
package flightnumber

import (
	"regexp"
	"strings"
	"unicode"
)

var designator = regexp.MustCompile(`^([A-Z0-9]{2}|[A-Z]{3})([0-9]{1,4})([A-Z]?)$`)

// Normalize returns the flight's designator in its one form: uppercase, without spaces, an airline
// code of two letters or digits (not both digits, as in EX, Z0, or 0Z) or of three letters, then
// the number without its leading zeros and an optional letter. False means the text is no flight
// number.
func Normalize(text string) (string, bool) {
	text = strings.ToUpper(strings.Join(strings.Fields(text), ""))
	parts := designator.FindStringSubmatch(text)
	if parts == nil {
		return "", false
	}
	airline, number := parts[1], strings.TrimLeft(parts[2], "0")
	if number == "" || len(airline) == 2 && !strings.ContainsFunc(airline, unicode.IsLetter) {
		return "", false
	}
	return airline + number + parts[3], true
}

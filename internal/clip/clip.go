// Package clip shortens text in the two ways the application needs: to a size in bytes, and to a
// length in characters for reading.
package clip

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const ellipsis = "…"

// Bytes cuts text to at most limit bytes, on a rune boundary and without a marker, for a cap on
// how much is stored, sent, or read.
func Bytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if limit <= 0 {
		return ""
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// Runes cuts text meant to be read to at most limit runes. When it cuts, the last rune is an
// ellipsis, so the reader can tell, and whitespace before the ellipsis is dropped.
func Runes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	return strings.TrimRightFunc(string(runes[:limit-1]), unicode.IsSpace) + ellipsis
}

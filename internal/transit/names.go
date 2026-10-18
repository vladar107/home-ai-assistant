package transit

import (
	"strings"
	"unicode"
)

// Normalize folds a station or stop name for matching: lower case, no accents, and punctuation as
// spaces, so "'s-Hertogenbosch" and "s Hertogenbosch" meet.
func Normalize(name string) string {
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

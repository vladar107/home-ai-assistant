// Package localetest helps tests check texts written in Russian.
package localetest

import (
	"strings"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

// Latin returns the words of a text written only in Latin letters, which a Russian text should
// not hold except for the allowed ones (commands, product names, values from the data), compared
// without case. Single letters are ignored.
func Latin(text string, allowed ...string) []string {
	skip := make(map[string]bool, len(allowed))
	for _, word := range allowed {
		skip[strings.ToLower(word)] = true
	}
	var found []string
	for _, word := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) }) {
		if len([]rune(word)) < 2 || skip[strings.ToLower(word)] {
			continue
		}
		onlyLatin := true
		for _, r := range word {
			if !unicode.Is(unicode.Latin, r) {
				onlyLatin = false
				break
			}
		}
		if onlyLatin {
			found = append(found, word)
		}
	}
	return found
}

// English reads a message in English, for tests that compare what the assistant wrote with a
// reply the code exports.
func English(text locale.Text) string { return text.In(locale.English) }

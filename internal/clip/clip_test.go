package clip

import (
	"testing"
	"unicode/utf8"
)

func TestBytes(t *testing.T) {
	for _, test := range []struct {
		text  string
		limit int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"abcdef", 3, "abc"},
		{"añb", 2, "a"},
		{"añb", 3, "añ"},
		{"日本", 4, "日"},
		{"text", 0, ""},
	} {
		got := Bytes(test.text, test.limit)
		if got != test.want || len(got) > max(test.limit, 0) || !utf8.ValidString(got) {
			t.Errorf("Bytes(%q, %d) = %q, want %q", test.text, test.limit, got, test.want)
		}
	}
}

func TestRunes(t *testing.T) {
	for _, test := range []struct {
		text  string
		limit int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"abcdef", 4, "abc…"},
		{"привет мир", 7, "привет…"},
		{"привет мир", 8, "привет…"},
		{"abc", 1, "…"},
		{"abc", 0, ""},
	} {
		got := Runes(test.text, test.limit)
		if got != test.want || utf8.RuneCountInString(got) > max(test.limit, 0) {
			t.Errorf("Runes(%q, %d) = %q, want %q", test.text, test.limit, got, test.want)
		}
	}
}

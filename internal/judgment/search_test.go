package judgment

import (
	"strings"
	"testing"
)

func TestCarriesData(t *testing.T) {
	plain := []string{
		"what happened in the news today",
		"weather in Den Haag tomorrow",
		"Champions League results 2026",
		"best train from Utrecht to Zwolle at 22",
		"recipe for pancakes in 20 minutes",
		"news on 2026-10-08",
		"AC/DC tour dates",
		"is it 24/7 or open late",
		"e.g. a plain question about Python 3.12 release notes",
	}
	for _, query := range plain {
		if CarriesData(query) {
			t.Errorf("CarriesData(%q) = true, want false", query)
		}
	}

	carrying := map[string]string{
		"url":              "https://example.com/page?id=1",
		"scheme-less url":  "www.example.org/a",
		"bare host":        "example.com",
		"host in words":    "reviews of foo.example.co.uk shop",
		"site operator":    "site:example.com report",
		"inurl operator":   "inurl:admin login",
		"filetype":         "filetype:pdf budget",
		"email":            "contact jan@example.com",
		"phone":            "call +31 6 1234 5678",
		"iban":             "NL91 ABNA 0417 1643 00",
		"card":             "4111 1111 1111 1111",
		"ipv4":             "192.168.1.10 status",
		"ipv6":             "2001:db8::1 status",
		"jwt":              "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abc_DEF-123",
		"key prefix":       "sk-ant-abcdefghijklmnop1234",
		"long hex":         "0123456789abcdef0123456789abcdef",
		"long base64":      "Zm9vYmFyQmF6MTIzNDU2Nzg5MGFiY2RlZkdISUo",
		"unix path":        "open /home/user/notes.txt",
		"windows path":     `C:\Users\someone\notes.txt`,
		"assignment":       "token=abc123xyz",
		"too long":         strings.Repeat("word ", 101),
		"invalid encoding": "caf\xe9 news",
	}
	for name, query := range carrying {
		if !CarriesData(query) {
			t.Errorf("%s: CarriesData(%q) = false, want true", name, query)
		}
	}
}

package judgment

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRedact(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"url with path and query", "see https://example.com/a?b=c now", "see example.com now"},
		{"url with credentials and port", "http://user:pw@host.example:8080/x", "host.example"},
		{"url trailing punctuation", "open (https://example.com/a).", "open (example.com)."},
		{"bare www", "go to www.example.org/page", "go to www.example.org"},
		{"email", "mail jan.de-vries+x@example.co.uk please", "mail [email] please"},
		{"jwt", "t eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abc_DEF-123 end", "t [token] end"},
		{"bearer", "Authorization: Bearer abcd1234efgh5678", "Authorization: Bearer [secret]"},
		{"bearer word", "the bearer of news", "the bearer of news"},
		{"assignment equals", "API_KEY=abc123xyz", "API_KEY=[secret]"},
		{"assignment colon", "password: hunter2 ok", "password: [secret]"},
		{"assignment quoted", `token="a b c"`, `token=[secret]`},
		{"prefixed key", "use sk-ant-abcdefghijklmnop1234 now", "use [secret] now"},
		{"github token", "ghp_abcdefghijklmnopqrstuvwxyz0123", "[secret]"},
		{"aws key", "AKIAIOSFODNN7EXAMPLE", "[secret]"},
		{"long hex", "hash 0123456789abcdef0123456789abcdef", "hash [secret]"},
		{"long base64", "blob Zm9vYmFyQmF6MTIzNDU2Nzg5MGFiY2RlZkdISUo done", "blob [secret] done"},
		{"iban", "pay NL91 ABNA 0417 1643 00 today", "pay [iban] today"},
		{"card", "card 4111 1111 1111 1111 exp", "card [card] exp"},
		{"card dashes", "4111-1111-1111-1111", "[card]"},
		{"ipv4", "server 192.168.1.10 down", "server [ip] down"},
		{"ipv6", "at 2001:db8::1 up", "at [ip] up"},
		{"time not ipv6", "at 10:30:45 sharp", "at 10:30:45 sharp"},
		{"phone international", "call +31 6 1234 5678 now", "call [phone] now"},
		{"phone local", "call (020) 123-4567.", "call [phone]."},
		{"absolute path", "read /home/jan/tax/2024.pdf now", "read 2024.pdf now"},
		{"home path", "open ~/Documents/notes.md", "open notes.md"},
		{"windows path", `open C:\Users\jan\report.docx`, "open report.docx"},
		{"relative path", "file Documents/tax/2024.pdf", "file 2024.pdf"},
		{"relative root path", "Root/sub/file.pdf", "file.pdf"},
		{"quoted path", `"Root/sub/file.pdf"`, `"file.pdf"`},
		{"scheme-less url query", "example.com/reset?code=abc123&state=xyz", "example.com"},
		{"scheme-less url path", "see example.com/private-doc now", "see example.com now"},
		{"two-segment root", "Root/Taxes", "Taxes"},
		{"two-segment photos", "Photos/Jan", "Jan"},
		{"two-segment private", "Private/Medical", "Medical"},
		{"labelled relative", "path=Root/Taxes", "path=Taxes"},
		{"windows backslash relative", `Private\Medical\x.pdf`, "x.pdf"},
		{"either/or", "either/or", "either/or"},
		{"m/s", "10 m/s", "10 m/s"},
		{"24/7", "open 24/7", "open 24/7"},
		{"path with spaces", "/Volume1/Photos/My Family/holiday 2024.jpg", "holiday 2024.jpg"},
		{"windows path with spaces", `C:\Users\jan\My Documents\file.docx`, "file.docx"},
		{"path with person name", "/volume1/Photos/Jan Smith/x.jpg", "x.jpg"},
		{"windows forward slashes", "C:/Users/jan/file.docx", "file.docx"},
		{"unc path", `\\nas\share\private\file.pdf`, "file.pdf"},
		{"home user path", "~jan/private", "private"},
		{"two paths", "cp /a/b/one.txt /c/d/two.txt", "cp one.txt two.txt"},
		{"authorization basic", "Authorization: Basic dXNlcjpwYXNzd29yZA==", "Authorization: Basic [secret]"},
		{"authorization token", "Authorization: Token abc123def", "Authorization: Token [secret]"},
		{"authorization bearer no digits", "Authorization: Bearer abcdefghijklmnop", "Authorization: Bearer [secret]"},
		{"long bearer no digits", "use Bearer abcdefghijklmnopqrst", "use Bearer [secret]"},
		{"gitlab token", "glpat-abcdefghijklmnopqrst", "[secret]"},
		{"encoded assignment", "access_token%3Dabc123", "access_token%3D[secret]"},
		{"prose password english", "my password is hunter2", "my password is [secret]"},
		{"prose password dutch", "het wachtwoord is Geheim123!", "het wachtwoord is [secret]"},
		{"prose password russian colon", "пароль: qwerty123", "пароль: [secret]"},
		{"prose password russian", "мой пароль qwerty123", "мой пароль [secret]"},
		{"prose password multiword", "wifi password: Correct Horse Battery", "wifi password: [secret]"},
		{"prose pin with digits", "pin 4821", "pin [secret]"},
		{"pin without digits", "the pin of the map", "the pin of the map"},
		{"prose secret needs separator", "secret santa gift", "secret santa gift"},
		{"prose api key", "api key is abcd", "api key is [secret]"},
		{"lowercase iban", "nl91abna0417164300", "[iban]"},
		{"postcode spaced", "1012 AB Amsterdam", "[postcode] Amsterdam"},
		{"postcode compact", "1012AB", "[postcode]"},
		{"postcode excluded letters", "1012 SS", "1012 SS"},
		{"mac", "mac aa:bb:cc:dd:ee:ff up", "mac [mac] up"},
		{"parenthesised phone", "call (+31) 20-1234567 now", "call [phone] now"},
		{"phone in parentheses", "(call 06 1234 5678)", "(call [phone])"},
		{"ipv6 zone", "fe80::1%en0", "[ip]"},
		{"date short", "on 29/09 at noon", "on 29/09 at noon"},
		{"date long", "due 29/09/2026", "due 29/09/2026"},
		{"iso date", "due 2026-09-29", "due 2026-09-29"},
		{"iso datetime", "at 2026-09-29 12:00", "at 2026-09-29 12:00"},
		{"dotted date", "due 29.09.2026", "due 29.09.2026"},
		{"year range", "in 2024-2025", "in 2024-2025"},
		{"fraction", "add 1/2 cup", "add 1/2 cup"},
		{"cron step", "*/5 * * * *", "*/5 * * * *"},
		{"cron weekdays", "0 7 * * 1-5", "0 7 * * 1-5"},
		{"relative spaced folders", "Photos/Family 2024/Beach Day/IMG_0001.jpg", "IMG_0001.jpg"},
		{"relative long folder in prose", "save into Root/My Family Photos Summer Holiday/img.jpg", "save into img.jpg"},
		{"relative backslash spaced", `Docs\Tax Returns\final.pdf`, "final.pdf"},
		{"relative folder file", "docs/secret-plan.md", "secret-plan.md"},
		{"pass with separator", "Wi-Fi: HomeNet / pass: tulip2024", "Wi-Fi: HomeNet / pass: [secret]"},
		{"pass without separator", "pass the salt", "pass the salt"},
		{"code with digits", "code 4832 for the door", "code [secret]"},
		{"code colon", "door code: 4832", "door code: [secret]"},
		{"code without digits", "the code is in the repo", "the code is in the repo"},
		{"secret hyphenated name", "secret-plan is ready", "secret-plan is ready"},
		{"and/or", "tea and/or coffee", "tea and/or coffee"},
		{"km/h", "60 km/h", "60 km/h"},
		{"slash command", "/help me", "/help me"},
		{"file name", "invoice-2024.pdf", "invoice-2024.pdf"},
		{"long slug name", "a-very-long-descriptive-file-name-for-the-tax-return.pdf", "a-very-long-descriptive-file-name-for-the-tax-return.pdf"},
		{"plain text", "Move the meeting to Friday at 15:00.", "Move the meeting to Friday at 15:00."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.in).String(); got != tc.want {
				t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactHandlesInvalidUTF8AndLength(t *testing.T) {
	if got := Redact("a\xffb").String(); got != "ab" || !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 result = %q", got)
	}
	long := strings.Repeat("word ", maximumRedactedRunes)
	if got := utf8.RuneCountInString(Redact(long).String()); got != maximumRedactedRunes {
		t.Errorf("rune count = %d, want %d", got, maximumRedactedRunes)
	}
	if !Redact("").Empty() {
		t.Error("empty input must give an empty Redacted")
	}
}

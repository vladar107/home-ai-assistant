package placeholder

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var emailPattern = regexp.MustCompile(`[A-Za-z0-9_%+-]+(?:\.[A-Za-z0-9_%+-]+)*@(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}`)

func findEmails(text string) []span {
	var spans []span
	for _, match := range emailPattern.FindAllStringIndex(text, -1) {
		if after, _ := utf8.DecodeRuneInString(text[match[1]:]); match[1] < len(text) && isAlnum(after) {
			continue
		}
		value := text[match[0]:match[1]]
		if strings.IndexByte(value, '@') > 64 || len(value) > 254 {
			continue
		}
		spans = append(spans, span{match[0], match[1]})
	}
	return spans
}

// Digit runs that look like a phone number but are not one. A date with a two-digit year is
// excluded only when it stands alone, so dotted numbers such as 06.12.34.56.78 are still found.
var (
	tokenPattern  = regexp.MustCompile(`⟦[^⟦⟧]*⟧`)
	ibanPattern   = regexp.MustCompile(`(?i)\b[A-Z]{2}[0-9]{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,3})?\b`)
	datesTimes    = regexp.MustCompile(`\b(?:\d{4}-\d{1,2}-\d{1,2}|\d{1,2}[./-]\d{1,2}[./-]\d{4}|\d{1,2}:\d{2}(?::\d{2})?)\b`)
	shortDate     = regexp.MustCompile(`\b\d{1,2}\.\d{1,2}\.\d{2}\b`)
	numberJoiners = ".-:,/"
)

// A number right after one of these words is an identifier, not a phone number.
var (
	idWords = map[string]bool{
		"order": true, "ordernummer": true, "klantnummer": true, "customer": true, "account": true,
		"invoice": true, "factuur": true, "build": true, "tracking": true, "track": true, "parcel": true,
		"pakket": true, "ref": true, "reference": true, "nr": true, "no": true, "id": true,
		"gls": true, "dhl": true, "dpd": true, "ups": true, "postnl": true, "fedex": true,
	}
	phoneWords = map[string]bool{
		"tel": true, "phone": true, "telephone": true, "telefoon": true, "telefon": true,
		"mobile": true, "mobiel": true, "mob": true, "gsm": true, "fax": true,
	}
)

func blockedSpans(text string) []span {
	var spans []span
	for _, match := range tokenPattern.FindAllStringIndex(text, -1) {
		spans = append(spans, span{match[0], match[1]})
	}
	for _, match := range ibanPattern.FindAllStringIndex(text, -1) {
		spans = append(spans, span{match[0], match[1]})
	}
	for _, match := range datesTimes.FindAllStringIndex(text, -1) {
		spans = append(spans, span{match[0], match[1]})
	}
	for _, match := range shortDate.FindAllStringIndex(text, -1) {
		if standsAlone(text, match[0], match[1]) {
			spans = append(spans, span{match[0], match[1]})
		}
	}
	slices.SortFunc(spans, func(a, b span) int { return cmp.Compare(a.start, b.start) })
	return spans
}

func standsAlone(text string, start, end int) bool {
	if start >= 2 && strings.IndexByte(numberJoiners, text[start-1]) >= 0 && isDigit(text[start-2]) {
		return false
	}
	return end+1 >= len(text) || strings.IndexByte(numberJoiners, text[end]) < 0 || !isDigit(text[end+1])
}

func findPhones(text string) []span {
	blocked := blockedSpans(text)
	var spans []span
	next, joined := 0, -1
	for i := 0; i < len(text); {
		for next < len(blocked) && blocked[next].end <= i {
			next++
		}
		limit := len(text)
		if next < len(blocked) {
			if blocked[next].start <= i {
				i = blocked[next].end
				continue
			}
			limit = blocked[next].start
		}
		if !phoneStart(text, i, i == joined) {
			_, size := utf8.DecodeRuneInString(text[i:])
			i += size
			continue
		}
		end, ok := phoneAt(text, i, limit)
		if ok && !afterIDWord(text, i) {
			spans = append(spans, span{i, end})
			if end < len(text) && text[end] == '/' {
				joined = end + 1
			}
		}
		i = max(end, i+1)
	}
	return spans
}

// WithoutPhones removes from text the phone numbers Hide would replace, leaving identifiers such as
// an order number in place.
func WithoutPhones(text string) string {
	var kept strings.Builder
	last := 0
	for _, phone := range findPhones(text) {
		kept.WriteString(text[last:phone.start])
		last = phone.end
	}
	kept.WriteString(text[last:])
	return kept.String()
}

// phoneStart reports whether a phone number can start at i. joined is true right after a phone
// number and a slash, as in 0612345678/0687654321.
func phoneStart(text string, i int, joined bool) bool {
	switch c := text[i]; {
	case c == '+' || c == '0':
	case c == '(' && i+1 < len(text) && (text[i+1] == '+' || text[i+1] == '0'):
	default:
		return false
	}
	if i == 0 || joined {
		return true
	}
	before, size := utf8.DecodeLastRuneInString(text[:i])
	if before == '.' || before == ':' {
		prior, _ := utf8.DecodeLastRuneInString(text[:i-size])
		return i-size > 0 && unicode.IsLetter(prior)
	}
	return !isAlnum(before) && !strings.ContainsRune("-_/#@%&=+", before)
}

// phoneAt reads a number from start: an optional leading +, digit groups joined by single spaces,
// dashes, dots or one slash, and at most one bracketed group. Every group boundary with 9 to 15
// digits that the following text allows is a candidate, and the first with 10 or more digits wins
// over a 9-digit one, so a run does not swallow a number written after the phone number. It
// returns the candidate's end, or, when there is none, where the run ends.
func phoneAt(text string, start, limit int) (int, bool) {
	const (
		begin = iota
		plus
		digit
		separator
		opened
		closed
	)
	prev, digits, inside, runEnd, nine := begin, 0, 0, start, -1
	bracketed, slashed := false, false
scan:
	for j := start; j < limit && digits <= 15; {
		r, size := utf8.DecodeRuneInString(text[j:])
		switch {
		case r >= '0' && r <= '9':
			digits++
			prev = digit
			if inside > 0 {
				inside++
			}
		case r == '+':
			if j != start && (j != start+1 || text[start] != '(') {
				break scan
			}
			prev = plus
		case r == ' ' || r == '-' || r == '.' || r == '/' || r == ' ' || r == ' ':
			if prev != digit && prev != closed || inside > 0 || r == '/' && slashed {
				break scan
			}
			slashed = slashed || r == '/'
			prev = separator
		case r == '(':
			if bracketed || prev == plus {
				break scan
			}
			bracketed, inside, prev = true, 1, opened
		case r == ')':
			if inside < 2 || inside > 6 || prev != digit {
				break scan
			}
			inside, prev = 0, closed
		default:
			break scan
		}
		j += size
		if !(prev == digit && inside == 0 || prev == closed) || j < limit && isDigit(text[j]) {
			continue
		}
		runEnd = j
		if digits < 9 || digits > 15 || !numberEnds(text, j) || otherNumber(text[start:j]) {
			continue
		}
		if digits >= 10 {
			return j, true
		}
		nine = j
	}
	if nine >= 0 {
		return nine, true
	}
	return runEnd, false
}

func numberEnds(text string, end int) bool {
	if end == len(text) {
		return true
	}
	after, size := utf8.DecodeRuneInString(text[end:])
	next, _ := utf8.DecodeRuneInString(text[end+size:])
	switch {
	case isAlnum(after) || strings.ContainsRune("_#@%&=", after):
		return false
	case after == '/':
		return !isAlnum(next) || next == '0'
	case strings.ContainsRune(".-:,", after):
		return !unicode.IsDigit(next)
	}
	return true
}

// otherNumber reports whether a phone-shaped number is a decimal, a coordinate such as
// +52.3702163, or a range of zero-padded numbers such as 000123-000456.
func otherNumber(number string) bool {
	if strings.HasPrefix(number, "0.") {
		return true
	}
	if number[0] == '+' && strings.Count(number, ".") == 1 && !strings.ContainsAny(number, " -/()  ") {
		return true
	}
	low, high, ok := strings.Cut(number, "-")
	return ok && low[0] == '0' && high[0] == '0' && len(low) == len(high) && allDigits(low) && allDigits(high)
}

func allDigits(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
}

// afterIDWord reports whether the number at start follows an identifier label such as "Order" or
// "Ref.", within a few separator characters. "Phone no." is still a phone label.
func afterIDWord(text string, start int) bool {
	word, rest := lastWord(text[:start])
	if !idWords[strings.ToLower(word)] {
		return false
	}
	label, _ := lastWord(strings.TrimRight(rest, "-"))
	return !phoneWords[strings.ToLower(label)]
}

func lastWord(text string) (string, string) {
	trimmed := strings.TrimRight(text, " :#.  ")
	if utf8.RuneCountInString(text[len(trimmed):]) > 3 {
		return "", trimmed
	}
	at := strings.LastIndexFunc(trimmed, func(r rune) bool { return !unicode.IsLetter(r) }) + 1
	if at > 0 {
		_, size := utf8.DecodeRuneInString(trimmed[at-1:])
		at += size - 1
	}
	return trimmed[at:], trimmed[:at]
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

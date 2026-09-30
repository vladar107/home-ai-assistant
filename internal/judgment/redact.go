package judgment

import (
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maximumRedactedRunes = 16384
	// pathWords is one path segment; it may span up to four space-separated
	// words so folder names like "My Family" are consumed with the path.
	pathWords = `[^\s/\\]+(?: [^\s/\\]+){0,3}`
	// relativeWords is a folder segment of a relative path, which is only redacted
	// when it ends in a file name, so it can safely span more words.
	relativeWords = `[^\s/\\]+(?: [^\s/\\]+){0,9}`
)

// Redacted is text that has passed through Redact. The unexported field means
// no other package can construct one, so the client cannot be handed raw text.
type Redacted struct{ text string }

func (r Redacted) String() string { return r.text }

func (r Redacted) Empty() bool { return r.text == "" }

var (
	urlPattern       = regexp.MustCompile(`(?i)\b(?:(?:https?|ftp)://|www\.)[^\s<>"']+`)
	emailPattern     = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+`)
	jwtPattern       = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	authPattern      = regexp.MustCompile(`(?i)\b(Authorization[ \t]*[:=][ \t]*(?:(?:Bearer|Basic|Token|Digest)[ \t]+)?)[^\r\n]+`)
	bearerPattern    = regexp.MustCompile(`(?i)\b(Bearer\s+)([A-Za-z0-9._~+/=-]{8,})`)
	assignPattern    = regexp.MustCompile(`(?i)\b((?:[A-Za-z0-9]+[_-])*(?:api[_-]?key|key|token|secret|password|passwd|pwd)[ \t]*(?:[=:]|%3D)[ \t]*)("[^"]*"|'[^']*'|[^\s"',;&]+)`)
	passwordProse    = regexp.MustCompile(`(?i)(^|[^\pL\pN_])((?:password|passwd|passphrase|pwd|wachtwoord|пароль)[ \t]*(?:(?:is|are)[ \t]+|[:=][ \t]*|[ \t]+[—–-][ \t]+|[ \t]+))([^\r\n]+)`)
	codeProse        = regexp.MustCompile(`(?i)(^|[^\pL\pN_])(code[ \t]*(?:is[ \t]+|:[ \t]*|[ \t]+))([^\r\n]+)`)
	passProse        = regexp.MustCompile(`(?i)(^|[^\pL\pN_])(pass[ \t]*[:=][ \t]*)([^\r\n]+)`)
	pinProse         = regexp.MustCompile(`(?i)(^|[^\pL\pN_])((?:pincode|pin)[ \t]*(?:is[ \t]+|[:=][ \t]*|[ \t]+[—–-][ \t]+|[ \t]+))([^\r\n]+)`)
	secretProse      = regexp.MustCompile(`(?i)(^|[^\pL\pN_])((?:secret|api[ _-]?key|apikey|token)[ \t]*(?:is[ \t]+|[:=][ \t]*|[ \t]+[—–-][ \t]+))([^\r\n]+)`)
	prefixPattern    = regexp.MustCompile(`\b(?:sk-|ghp_|gho_|github_pat_|glpat-|xox[bap]-|AKIA|ntn_|secret_)[A-Za-z0-9_-]{16,}`)
	hexPattern       = regexp.MustCompile(`\b[0-9a-fA-F]{24,}\b`)
	longRunPattern   = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)
	ibanPattern      = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]){11,30}\b`)
	ibanLowerPattern = regexp.MustCompile(`(?i)\b[a-z]{2}\d{2}[a-z0-9]{11,30}\b`)
	postcodePattern  = regexp.MustCompile(`\b[1-9]\d{3} ?(?:[A-RT-Z][A-Z]|S[B-CE-RT-Z])\b`)
	macPattern       = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}\b`)
	cardPattern      = regexp.MustCompile(`\d(?:[ -]?\d){12,18}`)
	ipv4Pattern      = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ipv6Pattern      = regexp.MustCompile(`[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*(?:%[A-Za-z0-9_.-]+)?`)
	phonePattern     = regexp.MustCompile(`\(?\+?\(?\d[\d ().-]{5,}\d\)?`)
	datePattern      = regexp.MustCompile(`\b(?:\d{4}[-.]\d{1,2}[-.]\d{1,2}|\d{1,2}[-.]\d{1,2}[-.]\d{4})\b`)
	yearRangePattern = regexp.MustCompile(`^\d{4}-\d{4}$`)
	tokenPattern     = regexp.MustCompile(`\S+`)
	hostPattern      = regexp.MustCompile(`^((?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,})(?::\d+)?[/?#]`)
	wholePathPattern = regexp.MustCompile(`\\\\[^\s/\\]+(?:\\` + pathWords + `)+|[A-Za-z]:[/\\](?:` + pathWords + `[/\\])*` + pathWords + `|~[\w.-]*(?:/` + pathWords + `)+|(?:/` + pathWords + `){2,}|[\pL\pN_.~-][^\s/\\]*(?:[/\\]` + relativeWords + `)*[/\\][^\s/\\]*\.[A-Za-z][A-Za-z0-9]{1,7}`)
	labelPattern     = regexp.MustCompile(`^[A-Za-z_][\w-]*[=:]`)
)

// Redact removes what must not reach TypeSafe: URLs shrink to their host, paths
// to their last segment, and identifiers and secrets to typed placeholders.
// File and folder names, dates, fractions, and cron expressions survive.
func Redact(text string) Redacted {
	text = strings.ToValidUTF8(text, "")
	text = urlPattern.ReplaceAllStringFunc(text, urlHost)
	text = emailPattern.ReplaceAllString(text, "[email]")
	text = jwtPattern.ReplaceAllString(text, "[token]")
	text = authPattern.ReplaceAllString(text, "${1}[secret]")
	text = bearerPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := bearerPattern.FindStringSubmatch(match)
		if len(parts[2]) < 16 && !strings.ContainsAny(parts[2], "0123456789") {
			return match
		}
		return parts[1] + "[secret]"
	})
	text = assignPattern.ReplaceAllString(text, "${1}[secret]")
	text = redactProse(passwordProse, text, false)
	text = redactProse(passProse, text, false)
	text = redactProse(secretProse, text, false)
	text = redactProse(pinProse, text, true)
	text = redactProse(codeProse, text, true)
	text = prefixPattern.ReplaceAllString(text, "[secret]")
	text = hexPattern.ReplaceAllString(text, "[secret]")
	text = longRunPattern.ReplaceAllStringFunc(text, func(match string) string {
		if looksLikeBase64(match) {
			return "[secret]"
		}
		return match
	})
	text = replaceBounded(wholePathPattern, text, lastPathSegment)
	text = tokenPattern.ReplaceAllStringFunc(text, redactPathToken)
	text = ibanPattern.ReplaceAllString(text, "[iban]")
	text = ibanLowerPattern.ReplaceAllString(text, "[iban]")
	text = postcodePattern.ReplaceAllString(text, "[postcode]")
	text = macPattern.ReplaceAllString(text, "[mac]")
	text = replaceBounded(cardPattern, text, func(match string) string {
		if luhnValid(match) {
			return "[card]"
		}
		return match
	})
	text = replaceBounded(ipv4Pattern, text, func(match string) string {
		if validIPv4(match) {
			return "[ip]"
		}
		return match
	})
	text = replaceBounded(ipv6Pattern, text, redactIPv6)
	text = replaceBounded(phonePattern, text, redactPhone)

	if utf8.RuneCountInString(text) > maximumRedactedRunes {
		text = string([]rune(text)[:maximumRedactedRunes])
	}
	return Redacted{text: text}
}

func urlHost(match string) string {
	trimmed := strings.TrimRight(match, ".,;:!?)]}")
	suffix := match[len(trimmed):]
	candidate := trimmed
	if !strings.Contains(candidate, "://") {
		candidate = "http://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Hostname() == "" {
		return "[url]" + suffix
	}
	return parsed.Hostname() + suffix
}

func looksLikeBase64(run string) bool {
	var upper, lower, digit bool
	for _, r := range run {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return upper && lower && digit
}

// replaceBounded replaces matches that are not glued to a surrounding letter,
// digit, or underscore, since regexp has no lookaround.
func replaceBounded(pattern *regexp.Regexp, text string, replace func(string) string) string {
	var out strings.Builder
	last := 0
	for _, loc := range pattern.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if start > 0 {
			if before, _ := utf8.DecodeLastRuneInString(text[:start]); isWordRune(before) {
				continue
			}
		}
		if end < len(text) {
			if after, _ := utf8.DecodeRuneInString(text[end:]); isWordRune(after) {
				continue
			}
		}
		out.WriteString(text[last:start])
		out.WriteString(replace(text[start:end]))
		last = end
	}
	out.WriteString(text[last:])
	return out.String()
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func luhnValid(candidate string) bool {
	var digits []int
	for _, r := range candidate {
		if r >= '0' && r <= '9' {
			digits = append(digits, int(r-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	for i := range digits {
		digit := digits[len(digits)-1-i]
		if i%2 == 1 {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
	}
	return sum%10 == 0
}

func validIPv4(candidate string) bool {
	_, err := netip.ParseAddr(candidate)
	return err == nil
}

func redactIPv6(match string) string {
	trimmed := strings.TrimRight(match, ".:")
	address, err := netip.ParseAddr(trimmed)
	if err != nil || !address.Is6() {
		return match
	}
	return "[ip]" + match[len(trimmed):]
}

func redactPhone(match string) string {
	digits := 0
	for _, r := range match {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if digits < 8 || digits > 15 {
		return match
	}
	if datePattern.MatchString(match) || yearRangePattern.MatchString(match) {
		return match
	}
	if !strings.HasPrefix(match, "+") && !strings.ContainsAny(match, " -()") {
		if strings.Contains(match, ".") {
			return match
		}
	}
	if strings.HasSuffix(match, ")") && strings.Count(match, ")") > strings.Count(match, "(") {
		return "[phone])"
	}
	return "[phone]"
}

func redactProse(pattern *regexp.Regexp, text string, needDigit bool) string {
	return pattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := pattern.FindStringSubmatch(match)
		if needDigit && !strings.ContainsAny(parts[3], "0123456789") {
			return match
		}
		return parts[1] + parts[2] + "[secret]"
	})
}

func lastPathSegment(match string) string {
	return match[strings.LastIndexAny(match, `/\`)+1:]
}

// Unit and word pairs that look like relative paths but are ordinary prose.
var slashPairs = map[string]bool{
	"and/or": true, "either/or": true, "he/she": true, "his/her": true, "him/her": true,
	"yes/no": true, "on/off": true, "true/false": true, "s/he": true,
}

// redactPathToken handles what the whole-path pass leaves: scheme-less URLs
// (a dotted host followed by a path or query) reduce to the host, and any
// relative token with a letter and two or more segments reduces to its last
// segment. Tokens whose segments are all at most three characters (km/h, m/s,
// 24/7, TCP/IP) and the few prose pairs in slashPairs survive, as do dates,
// fractions, and cron expressions, which contain no letters.
func redactPathToken(token string) string {
	if !strings.ContainsAny(token, `/\?#`) {
		return token
	}
	core := strings.TrimLeft(token, `("'[<`)
	prefix := token[:len(token)-len(core)]
	trimmed := strings.TrimRight(core, `.,;:!?)"'>]}`)
	suffix := core[len(trimmed):]
	core = trimmed
	if label := labelPattern.FindString(core); label != "" {
		prefix += label
		core = core[len(label):]
	}

	if match := hostPattern.FindStringSubmatch(core); match != nil {
		return prefix + match[1] + suffix
	}
	segments := strings.FieldsFunc(core, func(r rune) bool { return r == '/' || r == '\\' })
	if len(segments) < 2 || !strings.ContainsFunc(core, unicode.IsLetter) {
		return token
	}
	if slashPairs[strings.ToLower(core)] {
		return token
	}
	short := true
	for _, segment := range segments {
		if utf8.RuneCountInString(segment) > 3 {
			short = false
		}
	}
	if short {
		return token
	}
	return prefix + segments[len(segments)-1] + suffix
}

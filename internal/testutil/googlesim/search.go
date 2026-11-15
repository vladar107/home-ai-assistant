package googlesim

import (
	"strconv"
	"strings"
	"time"
	"unicode"
)

// SetClock replaces the clock that newer_than, older_than and the date operators of a search are
// read against; the default is time.Now. The clock's zone is the zone of after: and before: dates.
func (s *Simulator) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

func (s *Simulator) clock() time.Time {
	s.mu.Lock()
	now := s.now
	s.mu.Unlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

// matchesQuery reports whether the message answers a Gmail search. It understands what the assistant
// sends: plain words and quoted phrases matched against subject, sender and snippet, the operators
// from:, to:, subject:, label:, in:, is:unread, is:read, newer_than:, older_than:, after: and
// before:, negation with -, grouping with parentheses, and OR (also as braces). Terms of a search
// are joined by AND, as in Gmail.
func matchesQuery(message Message, query string, now time.Time) bool {
	parser := &searchParser{tokens: tokenize(query)}
	node := parser.parseAll()
	return node(message, now)
}

type matcher func(Message, time.Time) bool

type searchParser struct {
	tokens []string
	pos    int
}

// tokenize splits a search into words, quoted phrases (kept with their quotes, and an operator's
// quoted value kept with its operator), parentheses and braces.
func tokenize(query string) []string {
	var tokens []string
	var current strings.Builder
	inQuote := false
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, r := range query {
		switch {
		case r == '"':
			current.WriteRune(r)
			inQuote = !inQuote
		case inQuote:
			current.WriteRune(r)
		case unicode.IsSpace(r):
			flush()
		case r == '(' || r == ')' || r == '{' || r == '}':
			flush()
			tokens = append(tokens, string(r))
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return tokens
}

func (p *searchParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *searchParser) done() bool { return p.pos >= len(p.tokens) }

func (p *searchParser) parseAll() matcher {
	var all []matcher
	for !p.done() {
		if p.peek() == ")" || p.peek() == "}" {
			p.pos++
			continue
		}
		all = append(all, p.parseOr())
	}
	return and(all)
}

func and(all []matcher) matcher {
	return func(m Message, now time.Time) bool {
		for _, one := range all {
			if !one(m, now) {
				return false
			}
		}
		return true
	}
}

func or(any []matcher) matcher {
	return func(m Message, now time.Time) bool {
		for _, one := range any {
			if one(m, now) {
				return true
			}
		}
		return false
	}
}

func (p *searchParser) parseOr() matcher {
	options := []matcher{p.parseUnary()}
	for p.peek() == "OR" && p.pos+1 < len(p.tokens) {
		p.pos++
		options = append(options, p.parseUnary())
	}
	if len(options) == 1 {
		return options[0]
	}
	return or(options)
}

func (p *searchParser) parseUnary() matcher {
	token := p.peek()
	p.pos++
	switch {
	case token == "(":
		var inner []matcher
		for !p.done() && p.peek() != ")" {
			inner = append(inner, p.parseOr())
		}
		p.pos++
		return and(inner)
	case token == "{":
		var inner []matcher
		for !p.done() && p.peek() != "}" {
			inner = append(inner, p.parseUnary())
		}
		p.pos++
		return or(inner)
	case strings.HasPrefix(token, "-") && len(token) > 1:
		p.pos--
		p.tokens[p.pos] = token[1:]
		not := p.parseUnary()
		return func(m Message, now time.Time) bool { return !not(m, now) }
	}
	return term(token)
}

func unquote(value string) string {
	return strings.Trim(value, `"`)
}

func term(token string) matcher {
	if name, value, found := strings.Cut(token, ":"); found && name != "" && !strings.HasPrefix(token, `"`) {
		if operator, ok := operatorMatcher(strings.ToLower(name), unquote(value)); ok {
			return operator
		}
	}
	text := strings.ToLower(unquote(token))
	return func(m Message, _ time.Time) bool {
		return strings.Contains(strings.ToLower(m.Subject+" "+m.From+" "+m.Snippet), text)
	}
}

func operatorMatcher(name, value string) (matcher, bool) {
	needle := strings.ToLower(value)
	switch name {
	case "from":
		return func(m Message, _ time.Time) bool { return strings.Contains(strings.ToLower(m.From), needle) }, true
	case "to":
		return func(m Message, _ time.Time) bool { return strings.Contains(strings.ToLower(m.To), needle) }, true
	case "subject":
		return func(m Message, _ time.Time) bool { return strings.Contains(strings.ToLower(m.Subject), needle) }, true
	case "label", "in":
		return labelMatcher(needle), true
	case "is":
		switch needle {
		case "unread":
			return func(m Message, _ time.Time) bool { return contains(m.Labels, "UNREAD") }, true
		case "read":
			return func(m Message, _ time.Time) bool { return !contains(m.Labels, "UNREAD") }, true
		}
	case "newer_than", "older_than":
		age, ok := parseAge(needle)
		if !ok {
			return nil, false
		}
		if name == "newer_than" {
			return func(m Message, now time.Time) bool { return m.Received.After(now.Add(-age)) }, true
		}
		return func(m Message, now time.Time) bool { return m.Received.Before(now.Add(-age)) }, true
	case "after", "before":
		return dateMatcher(name == "after", needle)
	}
	return nil, false
}

func labelMatcher(label string) matcher {
	if label == "anywhere" {
		return func(Message, time.Time) bool { return true }
	}
	want := strings.ToUpper(label)
	return func(m Message, _ time.Time) bool { return contains(m.Labels, want) }
}

// parseAge reads Gmail's 3d, 2m (months), 1y, and the hours the assistant also writes as 12h.
func parseAge(value string) (time.Duration, bool) {
	if len(value) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || n < 0 {
		return 0, false
	}
	day := 24 * time.Hour
	switch value[len(value)-1] {
	case 'h':
		return time.Duration(n) * time.Hour, true
	case 'd':
		return time.Duration(n) * day, true
	case 'm':
		return time.Duration(n) * 30 * day, true
	case 'y':
		return time.Duration(n) * 365 * day, true
	}
	return 0, false
}

// dateMatcher reads YYYY/MM/DD, YYYY-MM-DD or epoch seconds; a date is midnight in the clock's zone.
func dateMatcher(after bool, value string) (matcher, bool) {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && len(value) > 8 {
		at := time.Unix(seconds, 0)
		return compare(after, func(time.Time) time.Time { return at }), true
	}
	day, err := time.Parse("2006/01/02", strings.ReplaceAll(value, "-", "/"))
	if err != nil {
		return nil, false
	}
	return compare(after, func(now time.Time) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, now.Location())
	}), true
}

func compare(after bool, boundary func(time.Time) time.Time) matcher {
	return func(m Message, now time.Time) bool {
		at := boundary(now)
		if after {
			return !m.Received.Before(at)
		}
		return m.Received.Before(at)
	}
}

package gmail

import (
	"net/mail"
	"regexp"
	"strings"
)

var (
	dmarcFail = regexp.MustCompile(`(?i)\bdmarc\s*=\s*fail\b`)
	// dmarcClause reads a result clause, with its comments and quoted strings left out, that is a
	// DMARC result, and gmailDMARCPass one that is passing, with the domain it is for.
	dmarcClause    = regexp.MustCompile(`(?i)^dmarc\s*=`)
	gmailDMARCPass = regexp.MustCompile(`(?i)^dmarc\s*=\s*pass\s(?:.*\s)?header\.from\s*=(\S+)`)
)

// DMARCFailed reports whether an Authentication-Results value reports DMARC failing anywhere in
// it.
func DMARCFailed(value string) bool { return dmarcFail.MatchString(value) }

// DMARCPassed reports whether Gmail's own Authentication-Results shows DMARC passing for the
// domain. Gmail adds its result above the headers a message arrived with, where a sender can write
// one that claims to be Gmail's, so only the topmost counts, and a failure anywhere refuses.
func DMARCPassed(message Message, domain string) bool {
	results := message.HeaderValues("Authentication-Results")
	if len(results) == 0 {
		return false
	}
	for _, value := range results {
		if DMARCFailed(value) {
			return false
		}
	}
	clauses, ok := resultClauses(results[0])
	if !ok || !strings.EqualFold(strings.TrimSpace(clauses[0]), "mx.google.com") {
		return false
	}
	dmarcResults, passed := 0, false
	for _, clause := range clauses[1:] {
		clause = strings.TrimSpace(clause)
		if !dmarcClause.MatchString(clause) {
			continue
		}
		dmarcResults++
		result := gmailDMARCPass.FindStringSubmatch(clause)
		passed = result != nil && strings.EqualFold(strings.TrimSuffix(result[1], "."), domain)
	}
	return dmarcResults == 1 && passed
}

// resultClauses splits an Authentication-Results value into its clauses at the semicolons outside
// comments and quoted strings, where a sender can write text of its own, such as its envelope
// address. Each comment and quoted string reads as one space, and an unterminated one fails the
// value.
func resultClauses(value string) ([]string, bool) {
	var clauses []string
	var clause strings.Builder
	depth, quoted, escaped := 0, false, false
	for _, r := range value {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && (quoted || depth > 0):
			escaped = true
		case quoted:
			if r == '"' {
				quoted = false
				clause.WriteByte(' ')
			}
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
			if depth == 0 {
				clause.WriteByte(' ')
			}
		case depth > 0:
		case r == '"':
			quoted = true
		case r == ';':
			clauses = append(clauses, clause.String())
			clause.Reset()
		default:
			clause.WriteRune(r)
		}
	}
	if quoted || depth > 0 {
		return nil, false
	}
	return append(clauses, clause.String()), true
}

// soleSender is the message's one sender address, lowercase, or "" when it has no single readable
// one.
func soleSender(message Message) string {
	values := message.HeaderValues("From")
	if len(values) != 1 {
		return ""
	}
	list, err := mail.ParseAddressList(values[0])
	if err != nil || len(list) != 1 || strings.Count(list[0].Address, "@") != 1 {
		return ""
	}
	return strings.ToLower(list[0].Address)
}

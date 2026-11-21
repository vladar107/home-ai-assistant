// Package placeholder replaces email addresses, phone numbers and declared street addresses in
// model-bound text with per-principal tokens such as ⟦email-k3f9qa example.com⟧, and resolves
// tokens the model sends back. Tokens live in a vault, so the same value always gets the same
// token, and only the principal's own vault can resolve them.
package placeholder

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind is the kind of value a token stands for.
type Kind string

const (
	Email   Kind = "email"
	Phone   Kind = "phone"
	Address Kind = "address"
)

// Entry is one value the vault holds, with its token suffix.
type Entry struct {
	Kind   Kind
	Value  string
	Suffix string
}

// Vault keeps one principal's values and their token suffixes. Token allocates a suffix for a new
// value and returns the existing one for a known value. A suffix is six characters of lowercase
// base32 (a-z, 2-7).
type Vault interface {
	Token(ctx context.Context, principalID string, kind Kind, value string) (string, error)
	Value(ctx context.Context, principalID string, kind Kind, suffix string) (string, bool, error)
	Entries(ctx context.Context, principalID string, kind Kind) ([]Entry, error)
}

// Mapper hides and resolves values for one principal. It holds no state of its own, so it is safe
// for concurrent use when its vault is.
type Mapper struct {
	vault       Vault
	principalID string
}

// New returns the mapper for principalID over vault.
func New(vault Vault, principalID string) *Mapper {
	return &Mapper{vault: vault, principalID: principalID}
}

var neutralizer = strings.NewReplacer("⟦", "[", "⟧", "]")

// Hide returns text with every email address and phone number found by pattern, and every known
// or declared street address, replaced by its token. Literal ⟦ and ⟧ in text become ordinary
// brackets first, so text cannot carry a forged token. Declared values that pass IsAddress are
// added to the vault as addresses, and so is the part of one before its first comma when it holds
// a number, since other text may quote it alone; other declared values are ignored. Every known address replaced is
// touched in the vault, so an address that keeps appearing does not expire.
func (m *Mapper) Hide(ctx context.Context, text string, declared []string) (string, error) {
	text = neutralizer.Replace(text)
	suffixes := map[string]string{}
	for _, value := range declared {
		value = neutralizer.Replace(strings.TrimSpace(value))
		if !IsAddress(value) {
			continue
		}
		if err := m.addAddress(ctx, suffixes, value); err != nil {
			return "", err
		}
		if first, _, ok := strings.Cut(value, ","); ok && streetShaped(strings.TrimSpace(first)) {
			if err := m.addAddress(ctx, suffixes, strings.TrimSpace(first)); err != nil {
				return "", err
			}
		}
	}
	entries, err := m.vault.Entries(ctx, m.principalID, Address)
	if err != nil {
		return "", fmt.Errorf("placeholder: list addresses: %w", err)
	}
	known := map[string]bool{}
	for _, entry := range entries {
		if _, ok := suffixes[entry.Value]; !ok && entry.Value != "" {
			suffixes[entry.Value] = entry.Suffix
			known[entry.Value] = true
		}
	}
	text, replaced := replaceAddresses(text, suffixes)
	for _, value := range replaced {
		if known[value] {
			if _, err := m.vault.Token(ctx, m.principalID, Address, value); err != nil {
				return "", fmt.Errorf("placeholder: touch address: %w", err)
			}
		}
	}
	if text, err = m.replaceSpans(ctx, text, Email, findEmails(text)); err != nil {
		return "", err
	}
	return m.replaceSpans(ctx, text, Phone, findPhones(text))
}

func (m *Mapper) addAddress(ctx context.Context, suffixes map[string]string, value string) error {
	if _, ok := suffixes[value]; ok {
		return nil
	}
	suffix, err := m.vault.Token(ctx, m.principalID, Address, value)
	if err != nil {
		return fmt.Errorf("placeholder: store address: %w", err)
	}
	suffixes[value] = suffix
	return nil
}

func (m *Mapper) replaceSpans(ctx context.Context, text string, kind Kind, spans []span) (string, error) {
	if len(spans) == 0 {
		return text, nil
	}
	suffixes := map[string]string{}
	var out strings.Builder
	last := 0
	for _, s := range spans {
		value := text[s.start:s.end]
		suffix, ok := suffixes[value]
		if !ok {
			var err error
			if suffix, err = m.vault.Token(ctx, m.principalID, kind, value); err != nil {
				return "", fmt.Errorf("placeholder: store %s: %w", kind, err)
			}
			suffixes[value] = suffix
		}
		out.WriteString(text[last:s.start])
		out.WriteString(format(kind, suffix, value))
		last = s.end
	}
	out.WriteString(text[last:])
	return out.String(), nil
}

func format(kind Kind, suffix, value string) string {
	if kind == Email {
		return "⟦email-" + suffix + " " + emailDomain(value) + "⟧"
	}
	return "⟦" + string(kind) + "-" + suffix + "⟧"
}

func emailDomain(value string) string {
	return strings.ToLower(value[strings.LastIndexByte(value, '@')+1:])
}

type span struct{ start, end int }

// replaceAddresses returns text with each value replaced by its token, and the values it replaced.
func replaceAddresses(text string, suffixes map[string]string) (string, []string) {
	values := make([]string, 0, len(suffixes))
	for value := range suffixes {
		values = append(values, value)
	}
	slices.SortFunc(values, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	var claimed []span
	var replaced []string
	tokens := map[span]string{}
	for _, value := range values {
		for from := 0; from < len(text); {
			at := strings.Index(text[from:], value)
			if at < 0 {
				break
			}
			s := span{from + at, from + at + len(value)}
			if onWordBoundaries(text, s, value) && !overlaps(claimed, s) {
				if !slices.Contains(replaced, value) {
					replaced = append(replaced, value)
				}
				claimed = append(claimed, s)
				tokens[s] = format(Address, suffixes[value], value)
				from = s.end
				continue
			}
			_, size := utf8.DecodeRuneInString(text[s.start:])
			from = s.start + size
		}
	}
	if len(claimed) == 0 {
		return text, nil
	}
	slices.SortFunc(claimed, func(a, b span) int { return cmp.Compare(a.start, b.start) })
	var out strings.Builder
	last := 0
	for _, s := range claimed {
		out.WriteString(text[last:s.start])
		out.WriteString(tokens[s])
		last = s.end
	}
	out.WriteString(text[last:])
	return out.String(), replaced
}

func onWordBoundaries(text string, s span, value string) bool {
	first, _ := utf8.DecodeRuneInString(value)
	if before, _ := utf8.DecodeLastRuneInString(text[:s.start]); s.start > 0 && isAlnum(first) && isAlnum(before) {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(value)
	after, _ := utf8.DecodeRuneInString(text[s.end:])
	return s.end == len(text) || !isAlnum(last) || !isAlnum(after)
}

func overlaps(spans []span, s span) bool {
	return slices.ContainsFunc(spans, func(o span) bool { return s.start < o.end && o.start < s.end })
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

var (
	urlScheme = regexp.MustCompile(`(?i)(^|\s)(https?://|www\.|mailto:|tel:|sips?:)`)
	bareURL   = regexp.MustCompile(`(?i)(^|\s)[a-z0-9-]+(\.[a-z0-9-]+)+/`)
	months    = `(jan|january|feb|february|mar|march|apr|april|may|jun|june|jul|july|aug|august|sep|sept|september|oct|october|nov|november|dec|december)`
	dateTime  = regexp.MustCompile(`(?i)\d{4}-\d{1,2}-\d{1,2}|\b\d{1,2}[./-]\d{1,2}[./-]\d{2,4}\b|\b\d{1,2}:\d{2}\b|\b\d{1,2}\s?(am|pm)\b|` +
		`\b\d{1,2}(st|nd|rd|th)?\s+` + months + `\b|\b` + months + `\s+\d{1,2}(st|nd|rd|th)?\b`)
)

// IsAddress reports whether a declared value looks like a street address: street-shaped, and
// either with a comma or with at least two words besides its numbers. "Home", "Zoom", meeting
// links, dates, and a number after one word, such as "Platform 12" or "Terminal 2", are not.
func IsAddress(value string) bool {
	value = strings.TrimSpace(value)
	return streetShaped(value) && (strings.Contains(value, ",") || words(value) >= 2)
}

// streetShaped reports whether a value has at least eight characters, a digit, a letter and a
// space, and is neither a URL nor a date or time.
func streetShaped(value string) bool {
	return utf8.RuneCountInString(value) >= 8 &&
		strings.ContainsFunc(value, unicode.IsDigit) &&
		strings.ContainsFunc(value, unicode.IsLetter) &&
		strings.ContainsFunc(value, unicode.IsSpace) &&
		!strings.Contains(value, "://") &&
		!urlScheme.MatchString(value) &&
		!bareURL.MatchString(value) &&
		!dateTime.MatchString(value)
}

// words counts the value's words that hold no digit.
func words(value string) int {
	count := 0
	for _, field := range strings.Fields(value) {
		if strings.ContainsFunc(field, unicode.IsLetter) && !strings.ContainsFunc(field, unicode.IsDigit) {
			count++
		}
	}
	return count
}

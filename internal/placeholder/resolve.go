package placeholder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Reason is why a tool call's arguments were refused.
type Reason string

const (
	UnknownToken   Reason = "unknown"
	MalformedToken Reason = "malformed"
	BareToken      Reason = "bare"
	DomainMismatch Reason = "domain"
	TokenInKey     Reason = "key"
	RebuiltToken   Reason = "rebuilt"
)

// ResolveError refuses a tool call whose arguments hold a token that cannot be resolved. Its
// message tells the model what to do instead.
type ResolveError struct {
	Reason Reason
	Text   string
}

func (e *ResolveError) Error() string {
	switch e.Reason {
	case UnknownToken:
		return fmt.Sprintf("placeholder %s is not known; use only placeholders exactly as they appeared in this conversation", e.Text)
	case BareToken:
		return fmt.Sprintf("%q looks like a placeholder without its brackets; write placeholders exactly as they appeared, including ⟦ and ⟧", e.Text)
	case TokenInKey:
		return fmt.Sprintf("placeholder %s is used as a key; placeholders can only be values", e.Text)
	case RebuiltToken:
		return fmt.Sprintf("%q is a placeholder rewritten as an address, which reaches nobody; pass the placeholder itself exactly as it appeared, such as ⟦email-abc234 example.com⟧", e.Text)
	case DomainMismatch:
		return fmt.Sprintf("placeholder %s does not match the domain of the address it stands for; copy placeholders exactly as they appeared", e.Text)
	default:
		return fmt.Sprintf("%q is not a valid placeholder; copy placeholders exactly as they appeared, such as ⟦email-abc234 example.com⟧ or ⟦phone-abc234⟧", e.Text)
	}
}

const maxDomain = 253

// maxToken is the length in characters of the longest well-formed token, an email token with the
// longest domain.
var maxToken = utf8.RuneCountInString("⟦email-abcdef ⟧") + maxDomain

var (
	wellFormed  = regexp.MustCompile(`⟦(email|phone|address)-([a-z2-7]{6})(?: ([A-Za-z0-9.-]{1,` + strconv.Itoa(maxDomain) + `}))?⟧`)
	malformed   = regexp.MustCompile(`(?i)⟦\s*(email|phone|address)-[^⟧]{0,40}⟧?`)
	bareBody    = regexp.MustCompile(`\b(email|phone|address)-([a-z2-7]{6})\b`)
	rebuilt     = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9._%+-])([a-z2-7]{6})@([A-Za-z0-9.-]{1,` + strconv.Itoa(maxDomain) + `})`)
	hiddenLabel = map[Kind]string{Email: "(hidden address)", Phone: "(hidden number)", Address: "(hidden place)"}
)

// ResolveArguments returns a tool call's JSON arguments with every token in a string value
// replaced by its real value. It refuses, with a *ResolveError, arguments that hold an unknown or
// malformed token, a token body without brackets, an email token whose domain differs from its
// value's, or a token in an object key. Arguments without tokens are returned as given.
func (m *Mapper) ResolveArguments(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("placeholder: decode arguments: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("placeholder: decode arguments: data after the JSON value")
	}
	resolved, changed, err := m.resolveValue(ctx, value)
	if err != nil {
		return nil, err
	}
	if !changed {
		return raw, nil
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(resolved); err != nil {
		return nil, fmt.Errorf("placeholder: encode arguments: %w", err)
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

func (m *Mapper) resolveValue(ctx context.Context, value any) (any, bool, error) {
	switch v := value.(type) {
	case string:
		resolved, err := m.resolveStrict(ctx, v)
		return resolved, err == nil && resolved != v, err
	case []any:
		changed := false
		for i, item := range v {
			resolved, itemChanged, err := m.resolveValue(ctx, item)
			if err != nil {
				return nil, false, err
			}
			v[i], changed = resolved, changed || itemChanged
		}
		return v, changed, nil
	case map[string]any:
		changed := false
		for key, item := range v {
			if token := wellFormed.FindString(key); token != "" {
				return nil, false, &ResolveError{Reason: TokenInKey, Text: token}
			}
			if err := m.checkOutside(ctx, key); err != nil {
				return nil, false, err
			}
			resolved, itemChanged, err := m.resolveValue(ctx, item)
			if err != nil {
				return nil, false, err
			}
			v[key], changed = resolved, changed || itemChanged
		}
		return v, changed, nil
	default:
		return value, false, nil
	}
}

func (m *Mapper) resolveStrict(ctx context.Context, text string) (string, error) {
	var out strings.Builder
	last := 0
	for _, match := range wellFormed.FindAllStringSubmatchIndex(text, -1) {
		if err := m.checkOutside(ctx, text[last:match[0]]); err != nil {
			return "", err
		}
		token := text[match[0]:match[1]]
		value, err := m.lookup(ctx, text, match)
		if err != nil {
			return "", err
		}
		switch {
		case value.problem != "":
			return "", &ResolveError{Reason: value.problem, Text: token}
		case !value.found:
			return "", &ResolveError{Reason: UnknownToken, Text: token}
		}
		out.WriteString(text[last:match[0]])
		out.WriteString(value.value)
		last = match[1]
	}
	if err := m.checkOutside(ctx, text[last:]); err != nil {
		return "", err
	}
	if last == 0 {
		return text, nil
	}
	out.WriteString(text[last:])
	return out.String(), nil
}

func (m *Mapper) checkOutside(ctx context.Context, text string) error {
	if found := malformed.FindString(text); found != "" {
		return &ResolveError{Reason: MalformedToken, Text: found}
	}
	for _, match := range bareBody.FindAllStringSubmatch(text, -1) {
		if !strings.ContainsAny(match[2], "234567") {
			// An all-letter body such as "phone-number" is ordinary text unless the vault knows it.
			if _, found, err := m.vault.Value(ctx, m.principalID, Kind(match[1]), match[2]); err != nil {
				return fmt.Errorf("placeholder: resolve %s: %w", match[1], err)
			} else if !found {
				continue
			}
		}
		return &ResolveError{Reason: BareToken, Text: match[0]}
	}
	// A model that drops a token's brackets can join its suffix and domain into an address that
	// looks real but reaches nobody, or a stranger at the same domain.
	for _, match := range rebuilt.FindAllStringSubmatch(text, -1) {
		suffix, domain := strings.ToLower(match[1]), strings.TrimRight(match[2], ".")
		value, found, err := m.vault.Value(ctx, m.principalID, Email, suffix)
		if err != nil {
			return fmt.Errorf("placeholder: resolve %s: %w", Email, err)
		}
		if found && strings.EqualFold(emailDomain(value), domain) && !strings.EqualFold(value, suffix+"@"+domain) {
			return &ResolveError{Reason: RebuiltToken, Text: suffix + "@" + domain}
		}
	}
	return nil
}

type tokenValue struct {
	value   string
	found   bool
	problem Reason
}

func (m *Mapper) lookup(ctx context.Context, text string, match []int) (tokenValue, error) {
	kind, suffix := Kind(text[match[2]:match[3]]), text[match[4]:match[5]]
	domain := ""
	if match[6] >= 0 {
		domain = text[match[6]:match[7]]
	}
	if (kind == Email) != (domain != "") {
		return tokenValue{problem: MalformedToken}, nil
	}
	value, found, err := m.vault.Value(ctx, m.principalID, kind, suffix)
	if err != nil {
		return tokenValue{}, fmt.Errorf("placeholder: resolve %s: %w", kind, err)
	}
	if found && kind == Email && !strings.EqualFold(emailDomain(value), domain) {
		return tokenValue{problem: DomainMismatch}, nil
	}
	return tokenValue{value: value, found: found}, nil
}

// HoldsPlaceholder reports whether text holds a token, a malformed one, or a token body without
// brackets, any of which would carry a hidden value's stand-in to where it cannot be resolved.
func (m *Mapper) HoldsPlaceholder(ctx context.Context, text string) (bool, error) {
	if wellFormed.MatchString(text) {
		return true, nil
	}
	var refused *ResolveError
	switch err := m.checkOutside(ctx, text); {
	case errors.As(err, &refused):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

// ResolveText returns text for a user, with every known token replaced by its real value. A token
// the vault does not know, or an email token whose domain differs from its value's, becomes
// "(hidden address)", "(hidden number)" or "(hidden place)".
func (m *Mapper) ResolveText(ctx context.Context, text string) (string, error) {
	matches := wellFormed.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil
	}
	var out strings.Builder
	last := 0
	for _, match := range matches {
		value, err := m.lookup(ctx, text, match)
		if err != nil {
			return "", err
		}
		out.WriteString(text[last:match[0]])
		switch {
		case value.problem == MalformedToken:
			out.WriteString(text[match[0]:match[1]])
		case value.found && value.problem == "":
			out.WriteString(value.value)
		default:
			out.WriteString(hiddenLabel[Kind(text[match[2]:match[3]])])
		}
		last = match[1]
	}
	out.WriteString(text[last:])
	return out.String(), nil
}

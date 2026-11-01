package tools

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/clip"
)

// MaximumLabelRunes bounds a call's source label.
const MaximumLabelRunes = 120

// SourceLabel names what one call looked at, such as a page's address or a mail's sender and
// subject, from the call's arguments and the result its provider returned. It returns "" when it
// has nothing to say. A label is kept in its owner's database for explaining a reply and never
// logged or exported, but it may be private, so it holds no more than a reader needs to recognise
// the source.
type SourceLabel func(arguments json.RawMessage, result Result) string

// CleanLabel keeps a label to one line of printable text of at most MaximumLabelRunes.
func CleanLabel(label string) string {
	label = strings.Join(strings.FieldsFunc(label, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	return clip.Runes(label, MaximumLabelRunes)
}

// ArgumentLabel labels a call with the values of the named arguments, joined by ", ", skipping the
// ones the call left out. Strings and numbers are used as they are; other values are skipped.
func ArgumentLabel(keys ...string) SourceLabel {
	return joinedArguments(", ", keys)
}

// RouteLabel labels a call with the values of the named arguments joined by an arrow, such as a
// journey's from and to.
func RouteLabel(keys ...string) SourceLabel {
	return joinedArguments(" → ", keys)
}

func joinedArguments(separator string, keys []string) SourceLabel {
	return func(arguments json.RawMessage, _ Result) string {
		values := argumentValues(arguments)
		var parts []string
		for _, key := range keys {
			if value := values[key]; value != "" {
				parts = append(parts, value)
			}
		}
		return strings.Join(parts, separator)
	}
}

// URLLabel labels a call with the host and path of the address in the named argument, without its
// query, which can carry tokens.
func URLLabel(key string) SourceLabel {
	return func(arguments json.RawMessage, _ Result) string {
		address := argumentValues(arguments)[key]
		parsed, err := url.Parse(strings.TrimSpace(address))
		if err != nil || parsed.Host == "" {
			return ""
		}
		return parsed.Host + strings.TrimSuffix(parsed.EscapedPath(), "/")
	}
}

// ResultLineLabel labels a call with the values of the result's lines that start with the given
// prefixes, such as "From: " and "Subject: ", in the order of the prefixes and joined by " — ".
// Only the first line for each prefix counts.
func ResultLineLabel(prefixes ...string) SourceLabel {
	return func(_ json.RawMessage, result Result) string {
		lines := strings.Split(result.Text, "\n")
		var parts []string
		for _, prefix := range prefixes {
			for _, line := range lines {
				if value, found := strings.CutPrefix(strings.TrimSpace(line), prefix); found {
					if value = strings.TrimSpace(value); value != "" {
						parts = append(parts, value)
					}
					break
				}
			}
		}
		return strings.Join(parts, " — ")
	}
}

// FirstLabel returns the first label of the given ones that says something.
func FirstLabel(labels ...SourceLabel) SourceLabel {
	return func(arguments json.RawMessage, result Result) string {
		for _, label := range labels {
			if text := label(arguments, result); text != "" {
				return text
			}
		}
		return ""
	}
}

// argumentValues reads a call's top-level string and number arguments; anything else, or
// arguments that are not an object, give nothing.
func argumentValues(arguments json.RawMessage) map[string]string {
	var decoded map[string]any
	if err := json.Unmarshal(arguments, &decoded); err != nil {
		return nil
	}
	values := make(map[string]string, len(decoded))
	for key, value := range decoded {
		switch typed := value.(type) {
		case string:
			values[key] = strings.TrimSpace(typed)
		case float64:
			values[key] = strconv.FormatFloat(typed, 'f', -1, 64)
		}
	}
	return values
}

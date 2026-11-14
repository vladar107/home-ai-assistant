// Package locale holds the texts the assistant writes for its users in English and Russian. A
// message is a Go value that carries both languages where it is used, so there are no string keys
// and the compiler finds every use. The language of the moment travels in the context.
package locale

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Language is a language a user can choose. English is the zero value.
type Language uint8

const (
	English Language = iota
	Russian
)

// String returns the stored code of the language: "en" or "ru".
func (l Language) String() string {
	if l == Russian {
		return "ru"
	}
	return "en"
}

// Parse reads a stored language code.
func Parse(code string) (Language, bool) {
	switch code {
	case "en":
		return English, true
	case "ru":
		return Russian, true
	}
	return English, false
}

type languageKey struct{}

// With returns a context that carries the language.
func With(ctx context.Context, language Language) context.Context {
	return context.WithValue(ctx, languageKey{}, language)
}

// From returns the context's language, English when it carries none.
func From(ctx context.Context) Language {
	language, _ := ctx.Value(languageKey{}).(Language)
	return language
}

// Text is one message in both languages.
type Text struct {
	en, ru string
}

// T writes a message in English and Russian. Both are fmt formats; Russian word order uses
// explicit argument indexes such as %[2]s. The catalog test requires literal, non-empty arguments
// whose verbs match.
func T(en, ru string) Text { return Text{en: en, ru: ru} }

// In returns the message in a language.
func (t Text) In(language Language) string {
	if language == Russian {
		return t.ru
	}
	return t.en
}

// Format formats the message in the context's language.
func (t Text) Format(ctx context.Context, args ...any) string {
	return t.FormatIn(From(ctx), args...)
}

// FormatIn formats the message in a language, for code that has no context.
func (t Text) FormatIn(language Language, args ...any) string {
	return sprintf(t.In(language), args)
}

// Count is a message that depends on a whole number.
type Count struct {
	enOne, enOther       string
	ruOne, ruFew, ruMany string
}

// N writes a message that depends on a number: English has one and other, Russian one, few and
// many by the CLDR integer rule. Every form is a fmt format that takes the number as %[1]d.
func N(enOne, enOther, ruOne, ruFew, ruMany string) Count {
	return Count{enOne: enOne, enOther: enOther, ruOne: ruOne, ruFew: ruFew, ruMany: ruMany}
}

// Form returns the form of the message for n in a language.
func (c Count) Form(language Language, n int) string {
	if n < 0 {
		n = -n
	}
	if language == Russian {
		switch {
		case n%10 == 1 && n%100 != 11:
			return c.ruOne
		case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
			return c.ruFew
		}
		return c.ruMany
	}
	if n == 1 {
		return c.enOne
	}
	return c.enOther
}

// Format picks the form for n in the context's language and formats it with n as the first
// argument, followed by args.
func (c Count) Format(ctx context.Context, n int, args ...any) string {
	return c.FormatIn(From(ctx), n, args...)
}

// FormatIn is Format for a language, for code that has no context.
func (c Count) FormatIn(language Language, n int, args ...any) string {
	return sprintf(c.Form(language, n), append([]any{n}, args...))
}

var warned sync.Map

// sprintf formats a message. A format error is a catalog bug the catalog test should have caught:
// tests panic, production logs it once per message and shows the text as formatted.
func sprintf(format string, args []any) string {
	out := fmt.Sprintf(format, args...)
	// An argument that itself holds "%!" is the caller's text, not an error of the format.
	if strings.Count(out, "%!") <= strings.Count(fmt.Sprint(args...), "%!") {
		return out
	}
	if testing.Testing() {
		panic(fmt.Sprintf("locale: format %q with %d arguments wrote %q", format, len(args), out))
	}
	if _, seen := warned.LoadOrStore(format, true); !seen {
		slog.Warn("locale format error", "format", format, "arguments", len(args))
	}
	return out
}

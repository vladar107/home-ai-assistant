// Package dutchphrase gives a user who practises Dutch one natural, everyday phrase a day at the
// end of their morning Heads-up. A tool-less turn on the user's own subscription proposes ranked
// candidates; code takes the first one never sent to that user and keeps it for good, so no phrase
// repeats however long ago it was sent.
package dutchphrase

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/integration"
)

// Switch is each principal's choice to get the phrase with their Heads-up. It is off until the
// principal turns it on.
var Switch = integration.Switch{Integration: "dutch-phrase", Name: "Dutch phrase"}

const (
	MaximumPhraseRunes = 120
	MaximumTextRunes   = 200

	// DateLayout is how a phrase's local date is stored.
	DateLayout = "2006-01-02"
)

// Phrase is the phrase of one local date of a principal's, as stored.
type Phrase struct {
	PrincipalID      string
	LocalDate        string
	Phrase           string
	Normalized       string
	Usage            string
	Translation      string
	UsageTranslation string
	Note             string
	CreatedAt        time.Time
}

// Repository keeps every phrase sent; it is the SQLite store.
type Repository interface {
	LoadDutchPhrase(ctx context.Context, principalID, localDate string) (Phrase, bool, error)
	// RecentDutchPhrases returns the newest limit phrases, newest first.
	RecentDutchPhrases(ctx context.Context, principalID string, limit int) ([]string, error)
	// SentDutchPhrases reports which of the normalized phrases the principal was ever sent.
	SentDutchPhrases(ctx context.Context, principalID string, normalized []string) (map[string]bool, error)
	// InsertDutchPhrase stores the phrase unless the principal already holds one for its date or
	// its normalized form; inserted is false then.
	InsertDutchPhrase(ctx context.Context, phrase Phrase) (inserted bool, err error)
}

// Normalize is the form two phrases share when they differ only in case, punctuation, or spacing.
func Normalize(phrase string) string {
	stripped := strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, phrase)
	return strings.Join(strings.Fields(stripped), " ")
}

// validText reports whether a field fits its bound and can stand in the block's markup: it holds no
// line break, backtick, link bracket, bold marker, spoiler marker, or "://", which Telegram would
// turn into a link, does not open a quote, and
// starts and ends with neither a space, which a spoiler needs, nor a | or *, which would close a
// spoiler or bold early. An empty field is valid only when optional.
func validText(text string, maximum int, optional bool) bool {
	if text == "" {
		return optional
	}
	return utf8.ValidString(text) && utf8.RuneCountInString(text) <= maximum && strings.TrimSpace(text) == text &&
		!strings.ContainsAny(text, "\n\r\u2028\u2029`[") && !strings.Contains(text, "**") && !strings.Contains(text, "||") && !strings.Contains(text, "://") &&
		!strings.HasPrefix(text, ">") && strings.Trim(text, "|*") == text
}

// Valid reports whether the phrase's texts are all usable, with a normalized form that matches.
func (p Phrase) Valid() bool {
	if _, err := time.Parse(DateLayout, p.LocalDate); err != nil {
		return false
	}
	return validText(p.Phrase, MaximumPhraseRunes, false) && p.Normalized != "" && p.Normalized == Normalize(p.Phrase) &&
		validText(p.Usage, MaximumTextRunes, false) && validText(p.Translation, MaximumTextRunes, false) &&
		validText(p.UsageTranslation, MaximumTextRunes, false) && validText(p.Note, MaximumTextRunes, true)
}

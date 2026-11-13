package trip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	rewriteTimeout = 30 * time.Second
	// maximumMessageUnits is what one Telegram message holds, in UTF-16 code units.
	maximumMessageUnits = 4096
	// rewriteSlackRunes lets a short message grow past twice its length, as one line of a few
	// words can in another language.
	rewriteSlackRunes = 40
)

// Destinations says which tab a trip's messages go to, zero for the main view; the Telegram
// notifier that sends them implements it.
type Destinations interface {
	TripTab(ctx context.Context, principalID, tripID string) (int64, error)
}

// Messages reads what the user wrote; the conversation history implements it.
type Messages interface {
	// LatestMessage returns the last thing the principal wrote themselves in the tab, or with
	// anywhere in whichever tab they wrote last; false when they wrote nothing.
	LatestMessage(ctx context.Context, principalID string, threadID int64, anywhere bool) (string, bool, error)
}

// Rewriter rewrites a message the code wrote into the language of a sample, with the owner's own
// model subscription; the turn engine implements it.
type Rewriter interface {
	RewriteAlertLike(ctx context.Context, principalID, sample, text string) (string, error)
}

// LanguageSamples gives the sample that names the language of a trip's messages; it is *Language.
type LanguageSamples interface {
	Sample(ctx context.Context, principalID, tripID string) string
}

// Language puts a trip's messages in the language the user writes in. Nothing names that language:
// the user's latest message in the tab the trip's messages go to, or in any tab when they never
// wrote there, is the sample a rewrite is told to follow. A user who never wrote gets the English
// text the code wrote.
type Language struct {
	destinations Destinations
	messages     Messages
	rewriter     Rewriter
	trips        Repository
	logger       *slog.Logger
}

var _ LanguageSamples = (*Language)(nil)

// NewLanguage needs every part but the messages: without them there is no sample, and every
// message stays in English.
func NewLanguage(destinations Destinations, messages Messages, rewriter Rewriter, trips Repository, logger *slog.Logger) (*Language, error) {
	switch {
	case destinations == nil:
		return nil, fmt.Errorf("trip language destinations are required")
	case rewriter == nil:
		return nil, fmt.Errorf("trip language rewriter is required")
	case trips == nil:
		return nil, fmt.Errorf("trip language repository is required")
	case logger == nil:
		return nil, fmt.Errorf("trip language logger is required")
	}
	return &Language{destinations: destinations, messages: messages, rewriter: rewriter, trips: trips, logger: logger}, nil
}

// Sample returns the user's latest message where the trip's messages go, or anywhere when they
// never wrote there, and "" when they never wrote or it cannot be read.
func (l *Language) Sample(ctx context.Context, principalID, tripID string) string {
	if l.messages == nil {
		return ""
	}
	threadID, err := l.destinations.TripTab(ctx, principalID, tripID)
	if err != nil {
		l.failed(ctx, principalID, tripID, "tab", err)
		return ""
	}
	for _, anywhere := range []bool{false, true} {
		sample, found, err := l.messages.LatestMessage(ctx, principalID, threadID, anywhere)
		if err != nil {
			l.failed(ctx, principalID, tripID, "sample", err)
			return ""
		}
		if found && strings.TrimSpace(sample) != "" {
			return sample
		}
	}
	return ""
}

func (l *Language) failed(ctx context.Context, principalID, tripID, stage string, err error) {
	if ctx.Err() == nil {
		l.logger.WarnContext(ctx, "trip.language.failed", "principal_id", principalID, "trip_id", tripID, "stage", stage, "error", err)
	}
}

// Sender returns a sender that rewrites each message into the user's language before next sends
// it. Callers keep the English text they passed, which is what they store.
func (l *Language) Sender(next Sender) Sender {
	return localizedSender{language: l, next: next}
}

type localizedSender struct {
	language *Language
	next     Sender
}

func (s localizedSender) Send(ctx context.Context, principalID, tripID, text string) error {
	return s.next.Send(ctx, principalID, tripID, s.language.localize(ctx, principalID, tripID, text))
}

// localize returns the text in the user's language, or as it is when there is no sample or the
// rewrite fails, is late, or is unfaithful, so a message is never lost or held
// back by its translation.
func (l *Language) localize(ctx context.Context, principalID, tripID, text string) string {
	sample := l.Sample(ctx, principalID, tripID)
	if sample == "" {
		return text
	}
	found, _, err := l.trips.LoadTrip(ctx, principalID, tripID)
	if err != nil {
		l.failed(ctx, principalID, tripID, "trip", err)
		return text
	}
	var references []string
	for _, booking := range found.Bookings {
		if booking.Reference != "" {
			references = append(references, booking.Reference)
		}
	}
	rewriteCtx, cancel := context.WithTimeout(ctx, rewriteTimeout)
	rewritten, err := l.rewriter.RewriteAlertLike(rewriteCtx, principalID, sample, text)
	late := errors.Is(rewriteCtx.Err(), context.DeadlineExceeded)
	cancel()
	rewritten = strings.TrimSpace(rewritten)
	outcome, reason := "rewritten", ""
	switch {
	case ctx.Err() != nil:
		return text
	case late:
		outcome = "timeout"
	case err != nil:
		outcome = "failed"
	default:
		if reason = unfaithful(text, rewritten, references); reason != "" {
			outcome = "rejected"
		}
	}
	attributes := []any{"principal_id", principalID, "trip_id", tripID, "outcome", outcome}
	if outcome != "rewritten" {
		if err != nil {
			attributes = append(attributes, "error", err)
		}
		if reason != "" {
			attributes = append(attributes, "reason", reason)
		}
		l.logger.WarnContext(ctx, "trip.language.rewrite", attributes...)
		return text
	}
	l.logger.InfoContext(ctx, "trip.language.rewrite", attributes...)
	return rewritten
}

var (
	numberPattern = regexp.MustCompile(`\d+`)
	quotedPattern = regexp.MustCompile(`"[^"\n]+"`)
)

// unfaithful says why a rewrite cannot stand in for the original, as one fixed word, or "" when it
// can. The original holds text from outside, a title or a mail or web item, and the sample is read
// by the same model, so the rewrite is held both to what it keeps and to what it adds: it has the
// original's lines, each with the original's numbers in their order, which carry the dates and
// the old and new times; every text the original quotes and every booking reference of the trip
// that the original holds, written as they were; no address, handle, or command the original
// lacks, which Telegram would make tappable; and it fits one message and is at most about twice
// as long, so nothing was appended.
func unfaithful(original, rewritten string, references []string) string {
	switch {
	case rewritten == "":
		return "empty"
	case utf16Units(rewritten) > maximumMessageUnits || utf8.RuneCountInString(rewritten) > 2*utf8.RuneCountInString(original)+rewriteSlackRunes:
		return "length"
	}
	lines, written := strings.Split(original, "\n"), strings.Split(rewritten, "\n")
	if len(lines) != len(written) {
		return "lines"
	}
	for index, line := range lines {
		if !slices.Equal(numberPattern.FindAllString(line, -1), numberPattern.FindAllString(asciiDigits(written[index]), -1)) {
			return "numbers"
		}
	}
	for _, quoted := range quotedPattern.FindAllString(original, -1) {
		if !strings.Contains(rewritten, strings.Trim(quoted, `"`)) {
			return "quoted"
		}
	}
	for _, reference := range references {
		if strings.Contains(original, reference) && !strings.Contains(rewritten, reference) {
			return "reference"
		}
	}
	known := tappable(original)
	for _, word := range tappable(rewritten) {
		if !slices.Contains(known, word) {
			return "address"
		}
	}
	return ""
}

// asciiDigits writes every decimal digit of another script as its ASCII digit, so a language that
// has digits of its own keeps the numbers by writing them its way.
func asciiDigits(text string) string {
	return strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf || !unicode.IsDigit(r) {
			return r
		}
		for _, block := range unicode.Nd.R16 {
			if rune(block.Lo) <= r && r <= rune(block.Hi) {
				return '0' + (r-rune(block.Lo))%10
			}
		}
		for _, block := range unicode.Nd.R32 {
			if rune(block.Lo) <= r && r <= rune(block.Hi) {
				return '0' + (r-rune(block.Lo))%10
			}
		}
		return r
	}, text)
}

// tappable returns the words of text Telegram would make tappable: a web address, an @ handle or
// mail address, and a / command.
func tappable(text string) []string {
	var found []string
	for _, word := range strings.Fields(plain(text)) {
		bare := strings.TrimLeft(word, `("'\u00ab\u201c\u2018[`)
		if strings.Contains(word, "@") || strings.HasPrefix(bare, "/") || WithoutAddresses(word) == "" {
			found = append(found, word)
		}
	}
	return found
}

// Package intake keeps what the model read from incoming content, such as a letter, an invoice, or
// a parcel notice, as a record of its facts, and runs the opt-in inbox check that finds such mail
// on a schedule and offers its follow-ups on one card. A record holds the facts the model
// extracted, never the content itself, and every record is outside content, since it was read
// from someone else's text.
package intake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// Sources of a record.
const (
	SourcePhoto   = "photo"
	SourceFile    = "file"
	SourceForward = "forward"
	SourceMail    = "mail"
)

const (
	MaximumDates      = 10
	MaximumAmounts    = 5
	MaximumReferences = 5

	maximumKindRunes      = 40
	maximumTitleRunes     = 120
	maximumSenderRunes    = 120
	maximumLabelRunes     = 60
	maximumValueRunes     = 80
	maximumAmountRunes    = 32
	maximumActionRunes    = 300
	maximumSourceRefRunes = 300
)

// Record is one item of content as the model read it.
type Record struct {
	ID          string
	PrincipalID string
	Source      string
	// SourceRef names the item: an attachment ID, a mail account and message ID joined by a
	// slash, or the record's own ID for a forwarded message.
	SourceRef      string
	Kind           string
	Title          string
	Sender         string
	Facts          Facts
	ActionRequired string
	// Pending are the follow-ups an inbox check proposed that have reached no final outcome.
	Pending   []Action
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Facts are the dated facts, amounts, and references of a record.
type Facts struct {
	Dates      []Date      `json:"dates"`
	Amounts    []Amount    `json:"amounts"`
	References []Reference `json:"references"`
}

// Date is a labelled ISO date, YYYY-MM-DD, or local date and time, YYYY-MM-DDTHH:MM.
type Date struct {
	Label string `json:"label"`
	Date  string `json:"date"`
}

// Amount is a labelled amount as found, with its ISO currency code when the content gave one.
type Amount struct {
	Label    string `json:"label"`
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

// Reference is a labelled reference, such as an invoice or tracking number.
type Reference struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Action types an inbox check proposes.
const (
	ActionReminder = "reminder"
	ActionWatch    = "watch"
)

// Action is one follow-up an inbox check proposed: a one-time reminder of Text at the local time
// At, or an agent watch named Name that checks Instruction for Condition until Until. ApprovalID is
// the proposal it was last offered as, or "" while it waits to be offered. Offering marks an action
// being offered whose approval is not recorded yet, and Expiries counts the cards it expired on.
type Action struct {
	Type            string `json:"type"`
	Text            string `json:"text,omitempty"`
	At              string `json:"at,omitempty"`
	Name            string `json:"name,omitempty"`
	Instruction     string `json:"instruction,omitempty"`
	Condition       string `json:"condition,omitempty"`
	IntervalMinutes int    `json:"interval_minutes,omitempty"`
	Until           string `json:"until,omitempty"`
	ApprovalID      string `json:"approval_id,omitempty"`
	Offering        bool   `json:"offering,omitempty"`
	Expiries        int    `json:"expiries,omitempty"`
}

// Repository stores the records, always scoped to their principal.
type Repository interface {
	// CreateIntakeRecord stores the record unless the principal already has one from the same
	// source and source reference, reporting whether it did.
	CreateIntakeRecord(ctx context.Context, record Record) (bool, error)
	// LoadIntakeRecordBySource returns the principal's record from the source and reference.
	LoadIntakeRecordBySource(ctx context.Context, principalID, source, sourceRef string) (Record, bool, error)
	// UpdateIntakeRecord replaces the record's kind, title, sender, facts, and action only while it
	// was last changed at expectedUpdatedAt.
	UpdateIntakeRecord(ctx context.Context, record Record, expectedUpdatedAt time.Time) (bool, error)
	// ListIntakeRecords returns the principal's newest records, of the kind when it is not empty,
	// ignoring case.
	ListIntakeRecords(ctx context.Context, principalID, kind string, limit int) ([]Record, error)
	LoadIntakeRecord(ctx context.Context, principalID, id string) (Record, bool, error)
	DeleteIntakeRecord(ctx context.Context, principalID, id string) (bool, error)
	// PendingIntakeRecords returns the principal's records that hold pending actions, oldest
	// first.
	PendingIntakeRecords(ctx context.Context, principalID string) ([]Record, error)
	// SetIntakePendingActions replaces the record's pending actions.
	SetIntakePendingActions(ctx context.Context, principalID, id string, actions []Action) error
	// InboxReadUntil returns the instant the principal's last inbox check read mail up to, and how
	// many checks in a row made no progress past it.
	InboxReadUntil(ctx context.Context, principalID string) (time.Time, int, bool, error)
	RecordInboxReadUntil(ctx context.Context, principalID string, until time.Time, stalls int) error
	// ResetInbox forgets where the inbox check read to and every pending action of the principal.
	ResetInbox(ctx context.Context, principalID string) error
}

// Draft is a record as the model gives it, before validation.
type Draft struct {
	Source         string      `json:"source"`
	SourceRef      string      `json:"source_ref"`
	Kind           string      `json:"kind"`
	Title          string      `json:"title"`
	Sender         string      `json:"sender"`
	Dates          []Date      `json:"dates"`
	Amounts        []Amount    `json:"amounts"`
	References     []Reference `json:"references"`
	ActionRequired string      `json:"action_required"`
}

var (
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	// dateLayouts are read in order; a time with seconds keeps its minute.
	dateLayouts = []struct{ read, keep string }{
		{"2006-01-02", "2006-01-02"},
		{"2006-01-02T15:04", "2006-01-02T15:04"},
		{"2006-01-02 15:04", "2006-01-02T15:04"},
		{"2006-01-02T15:04:05", "2006-01-02T15:04"},
		{"2006-01-02 15:04:05", "2006-01-02T15:04"},
	}
)

// validate turns a draft into a record's fields, or says why it cannot be kept. Text that reads as
// a link or a web address is left out, as it is from a trip found in mail. Strict refuses the
// whole draft for one bad field, so the model can correct it; otherwise, as for mail the inbox
// check read, a field too long is cut, a bad fact and the facts past the caps are left out, and
// dropped counts what was left out.
func validate(draft Draft, strict bool) (record Record, dropped int, reason string) {
	record = Record{Source: strings.TrimSpace(draft.Source)}
	switch record.Source {
	case SourcePhoto, SourceFile, SourceForward, SourceMail:
	default:
		return Record{}, 0, "source must be photo, file, forward, or mail."
	}
	field := func(name, value string, limit int, required, addresses bool) (string, bool) {
		cleaned, problem := line(name, value, limit, required, addresses)
		if problem == "" {
			return cleaned, true
		}
		if strict || !strings.Contains(problem, "at most") {
			if reason == "" {
				reason = problem
			}
			return "", false
		}
		dropped++
		cleaned, _ = line(name, value, 1<<20, required, addresses)
		return clip.Runes(cleaned, limit), true
	}
	if record.SourceRef, _ = field("source_ref", draft.SourceRef, maximumSourceRefRunes, false, false); reason != "" {
		return Record{}, 0, reason
	}
	if record.SourceRef == "" && record.Source != SourceForward {
		return Record{}, 0, "source_ref is required: the attachment id, or the mail account and message id as account/id."
	}
	record.Kind, _ = field("kind", draft.Kind, maximumKindRunes, true, true)
	record.Title, _ = field("title", draft.Title, maximumTitleRunes, true, true)
	record.Sender, _ = field("sender", draft.Sender, maximumSenderRunes, false, true)
	record.ActionRequired, _ = field("action_required", draft.ActionRequired, maximumActionRunes, false, true)
	if reason != "" {
		return Record{}, 0, reason
	}
	if strict && (len(draft.Dates) > MaximumDates || len(draft.Amounts) > MaximumAmounts || len(draft.References) > MaximumReferences) {
		return Record{}, 0, fmt.Sprintf("a record keeps at most %d dates, %d amounts, and %d references.", MaximumDates, MaximumAmounts, MaximumReferences)
	}
	// fact is one fact's fields, or false for a fact that is left out, or that refuses the draft
	// when strict.
	fact := func(problem string) bool {
		if strict {
			reason = problem
		} else {
			dropped++
		}
		return false
	}
	record.Facts = Facts{Dates: []Date{}, Amounts: []Amount{}, References: []Reference{}}
	for _, date := range draft.Dates {
		label, ok := factLine("a date's label", date.Label, maximumLabelRunes, true, strict, &dropped)
		if !ok {
			fact("a date needs a label of at most 60 characters.")
		} else if value, valid := normalDate(date.Date); !valid {
			fact("a date must be YYYY-MM-DD or YYYY-MM-DDTHH:MM, such as 2026-11-01 or 2026-11-01T09:30.")
		} else if len(record.Facts.Dates) == MaximumDates {
			dropped++
		} else {
			record.Facts.Dates = append(record.Facts.Dates, Date{Label: label, Date: value})
		}
		if reason != "" {
			return Record{}, 0, reason
		}
	}
	for _, amount := range draft.Amounts {
		label, labelOK := factLine("an amount's label", amount.Label, maximumLabelRunes, true, strict, &dropped)
		value, valueProblem := line("an amount's value", amount.Value, maximumAmountRunes, true, false)
		currency := strings.ToUpper(strings.TrimSpace(amount.Currency))
		switch {
		case !labelOK:
			fact("an amount needs a label of at most 60 characters.")
		case valueProblem != "":
			fact(valueProblem)
		case currency != "" && !currencyPattern.MatchString(currency):
			fact("a currency must be a three-letter ISO code such as EUR, or empty.")
		case len(record.Facts.Amounts) == MaximumAmounts:
			dropped++
		default:
			record.Facts.Amounts = append(record.Facts.Amounts, Amount{Label: label, Value: value, Currency: currency})
		}
		if reason != "" {
			return Record{}, 0, reason
		}
	}
	for _, reference := range draft.References {
		label, labelOK := factLine("a reference's label", reference.Label, maximumLabelRunes, true, strict, &dropped)
		value, valueProblem := line("a reference's value", reference.Value, maximumValueRunes, true, true)
		switch {
		case !labelOK:
			fact("a reference needs a label of at most 60 characters.")
		case valueProblem != "":
			fact(valueProblem)
		case len(record.Facts.References) == MaximumReferences:
			dropped++
		default:
			record.Facts.References = append(record.Facts.References, Reference{Label: label, Value: value})
		}
		if reason != "" {
			return Record{}, 0, reason
		}
	}
	return record, dropped, ""
}

// factLine is a fact's label: cut to limit when not strict, counting the cut in dropped.
func factLine(name, value string, limit int, required, strict bool, dropped *int) (string, bool) {
	cleaned, problem := line(name, value, limit, required, true)
	if problem == "" {
		return cleaned, true
	}
	if strict || !strings.Contains(problem, "at most") {
		return "", false
	}
	*dropped++
	cleaned, _ = line(name, value, 1<<20, required, true)
	return clip.Runes(cleaned, limit), true
}

// line makes a value one line of plain text, refusing one over limit runes. With addresses set,
// the words that read as a link or a web address are left out.
func line(field, value string, limit int, required, addresses bool) (string, string) {
	if !utf8.ValidString(value) {
		return "", field + " must be valid UTF-8."
	}
	value = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, value)), " ")
	if addresses {
		value = trip.WithoutAddresses(value)
	}
	switch {
	case required && value == "":
		return "", field + " is required."
	case utf8.RuneCountInString(value) > limit:
		return "", fmt.Sprintf("%s must be at most %d characters.", field, limit)
	}
	return value, ""
}

// normalDate reads a date or a local date and time, keeping the minute of one with seconds.
func normalDate(value string) (string, bool) {
	value = strings.TrimSpace(value)
	for _, layout := range dateLayouts {
		if at, err := time.Parse(layout.read, value); err == nil {
			return at.Format(layout.keep), true
		}
	}
	return "", false
}

// linkFree reports whether text has no word that reads as a link or a web address.
func linkFree(text string) bool {
	return strings.Join(strings.Fields(text), " ") == trip.WithoutAddresses(text)
}

func newID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate intake record identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

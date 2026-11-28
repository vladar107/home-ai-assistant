// Package looseend tracks the user's loose ends: deadlines nobody asked them about, such as a
// free-cancellation window, a return window, a renewal, a trial's end, or an expiry, found in mail
// and calendar events, and the promises the user made in chat or in sent mail. An item is tracked
// silently until the morning it comes due for a nudge, and it stays a tombstone once it is done,
// dropped, or closed, so a later scan never brings it back.
package looseend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/placeholder"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

// Integration names the Loose ends switch.
const Integration = "loose-ends"

// Switch is each principal's Loose ends switch, off until the principal turns it on.
var Switch = integration.Switch{Integration: Integration, Name: "Loose ends"}

// Kind is what an item is.
type Kind string

const (
	KindCancelWindow Kind = "cancel_window"
	KindReturnWindow Kind = "return_window"
	KindRenewal      Kind = "renewal"
	KindTrialEnd     Kind = "trial_end"
	KindExpiry       Kind = "expiry"
	KindPromise      Kind = "promise"
)

// Kinds are every kind, in the order they are offered.
var Kinds = []Kind{KindCancelWindow, KindReturnWindow, KindRenewal, KindTrialEnd, KindExpiry, KindPromise}

func (k Kind) Valid() bool {
	switch k {
	case KindCancelWindow, KindReturnWindow, KindRenewal, KindTrialEnd, KindExpiry, KindPromise:
		return true
	}
	return false
}

// leadDays is how many days before its due date an item of the kind is first nudged.
func (k Kind) leadDays() int {
	switch k {
	case KindCancelWindow:
		return 2
	case KindReturnWindow, KindRenewal, KindTrialEnd:
		return 3
	case KindExpiry:
		return 30
	}
	return 0
}

// expirySecondLeadDays is how many days before its due date an expiry is nudged a second time.
const expirySecondLeadDays = 7

// Source says where an item was found.
type Source string

const (
	SourceMail     Source = "mail"
	SourceSent     Source = "sent"
	SourceCalendar Source = "calendar"
	SourceChat     Source = "chat"
)

func (s Source) Valid() bool {
	switch s {
	case SourceMail, SourceSent, SourceCalendar, SourceChat:
		return true
	}
	return false
}

// Status is where an item stands. Every status but open is final; done, dropped, and closed items
// are tombstones that keep the same deadline from being tracked again.
type Status string

const (
	StatusOpen    Status = "open"
	StatusDone    Status = "done"
	StatusDropped Status = "dropped"
	StatusExpired Status = "expired"
	StatusClosed  Status = "closed"
)

func (s Status) Valid() bool {
	switch s {
	case StatusOpen, StatusDone, StatusDropped, StatusExpired, StatusClosed:
		return true
	}
	return false
}

// Tombstone reports whether the status keeps the item's deadline from being tracked again.
func (s Status) Tombstone() bool {
	return s == StatusDone || s == StatusDropped || s == StatusClosed
}

// Caps and lifetimes.
const (
	MaximumTitleRunes       = 120
	MaximumCounterpartRunes = 120
	// MaximumNewPerScan is how many new items one scan may add.
	MaximumNewPerScan = 10
	// MaximumNewPerSender is how many new items one sender domain may add in one scan.
	MaximumNewPerSender = 2
	// MaximumOpen is how many open items a principal may have.
	MaximumOpen = 100
	// MaximumAhead is how far ahead a due date may lie.
	MaximumAhead = 400 * 24 * time.Hour
	// Retention is how long an item no longer open is kept after its due date.
	Retention = 90 * 24 * time.Hour
	// BackfillWindow is how far back the scan reads once the switch is turned on.
	BackfillWindow = 30 * 24 * time.Hour

	maximumAccountRunes = 320
	maximumRefRunes     = 300
)

// DateLayout is the layout of a local date, as nudge_on and nudged_on hold it.
const DateLayout = "2006-01-02"

// Item is one tracked loose end. Title and Counterpart may come from outside text and are shown as
// plain text only.
type Item struct {
	ID          string
	PrincipalID string
	Kind        Kind
	Title       string
	// Counterpart is who the item is with: a sender's or organiser's domain, or a name the user
	// gave.
	Counterpart string
	DueAt       time.Time
	// DueAllDay marks a due date given without a time, which DueAt holds as the end of that local
	// day.
	DueAllDay bool
	// NudgeOn is the local date the item is next nudged on, and NudgedOn the one it was last nudged
	// on, empty before its first nudge.
	NudgeOn  string
	NudgedOn string
	Source   Source
	// Account is the Google account a mail, sent, or calendar item was found in.
	Account string
	// SourceRef is the message or event ID, or the item's own ID for a chat item.
	SourceRef string
	// ThreadRef is a mail or sent item's Gmail thread.
	ThreadRef string
	// Outside marks an item that holds text someone else wrote.
	Outside   bool
	Status    Status
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Draft is an item as a tool call or a scan gives it, before validation.
type Draft struct {
	Kind        Kind
	Title       string
	Counterpart string
	// Due is YYYY-MM-DD, a local date and time, or a date and time with its offset.
	Due       string
	Source    Source
	Account   string
	SourceRef string
	ThreadRef string
	Outside   bool
	// Domain is the sender's registrable domain, which the per-sender cap of a batch counts by; it
	// is not stored. An empty one is not capped per sender.
	Domain string
}

// Outcome is what tracking one draft did.
type Outcome string

const (
	// OutcomeCreated stored a new item.
	OutcomeCreated Outcome = "created"
	// OutcomeDuplicate found the same message, event, or chat item already tracked.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeMerged found an open item of the same kind, thread, and local due date.
	OutcomeMerged Outcome = "merged"
	// OutcomeTombstoned found the same deadline already done, dropped, or closed.
	OutcomeTombstoned Outcome = "tombstoned"
	// OutcomeInvalid refused the draft; Tracked.Reason says why.
	OutcomeInvalid Outcome = "invalid"
	// OutcomeOpenCapped left the item out because the principal has the most open items there may
	// be.
	OutcomeOpenCapped Outcome = "open_capped"
	// OutcomeScanCapped left the item out because the batch already added the most new items.
	OutcomeScanCapped Outcome = "scan_capped"
	// OutcomeSenderCapped left the item out because the batch already added the most new items of
	// its sender domain.
	OutcomeSenderCapped Outcome = "sender_capped"
)

// Insert is an item to store, and what may keep it out: an item with the same key, an open or
// tombstoned item of the same kind, account, and thread whose due date falls in [DayStart, DayEnd),
// the item's local due day, or MaximumOpen open items already.
type Insert struct {
	Item        Item
	DayStart    time.Time
	DayEnd      time.Time
	MaximumOpen int
}

// StatusChange moves an item from one of From to To at At, only while it was last changed at
// Expected when Expected is set.
type StatusChange struct {
	PrincipalID string
	ID          string
	From        []Status
	To          Status
	Expected    time.Time
	At          time.Time
}

// ScanState is how far a principal's scan has read. BackfillUntil and BackfillBefore bound the
// backfill window still to read, back to BackfillUntil from BackfillBefore; both are zero once it
// is drained. Stalls and BackfillStalls count the scans in a row that made no progress on new mail
// and on the backfill. CalendarCursor is set while a changed calendar's events are offered over
// several scans: the key of the last one offered, with CalendarDigest still the one before the
// change. CalendarDeclined holds a key for each event in the window the scan offered and the model
// reported nothing for, made of its account, ID, and update time, so an event is offered again only
// once it changes.
type ScanState struct {
	ReadUntil        time.Time
	BackfillUntil    time.Time
	BackfillBefore   time.Time
	Stalls           int
	BackfillStalls   int
	CalendarDigest   string
	CalendarCursor   string
	CalendarDeclined []string
}

// Ref names where an item was found: its account and its message or event ID.
type Ref struct {
	Account   string
	SourceRef string
}

// Repository stores the items and scan states, always scoped to their principal.
type Repository interface {
	// CreateLooseEnd stores the insert's item unless something keeps it out, and returns what
	// happened with the ID of the item that kept it out.
	CreateLooseEnd(ctx context.Context, insert Insert) (Outcome, string, error)
	// FindLooseEnd returns the outcome and ID of the item that would keep the insert's item out by
	// its key or the thread merge, or an empty outcome when nothing would.
	FindLooseEnd(ctx context.Context, insert Insert) (Outcome, string, error)
	LoadLooseEnd(ctx context.Context, principalID, id string) (Item, bool, error)
	// ListLooseEnds returns the principal's items of the status, and of the kind when it is not
	// empty, soonest due first.
	ListLooseEnds(ctx context.Context, principalID string, status Status, kind Kind) ([]Item, error)
	// LooseEndRefs returns where the principal's items from the source were found, in any status.
	LooseEndRefs(ctx context.Context, principalID string, source Source) ([]Ref, error)
	// DueLooseEnds returns the principal's open items due after now whose nudge_on is today or
	// earlier and whose nudged_on is before it, soonest due first.
	DueLooseEnds(ctx context.Context, principalID, today string, now time.Time) ([]Item, error)
	// ChangeLooseEndStatus applies the change, reporting whether the item was in a status it
	// allows.
	ChangeLooseEndStatus(ctx context.Context, change StatusChange) (bool, error)
	// SnoozeLooseEnd moves an open item's nudge day.
	SnoozeLooseEnd(ctx context.Context, principalID, id, nudgeOn string, at time.Time) (bool, error)
	// RecordLooseEndNudge records that an open item was nudged and sets its next nudge day, only
	// while its nudge day is still expectedNudgeOn, so a Tomorrow pressed meanwhile stands. It
	// leaves updated_at alone, which times the user's own changes.
	RecordLooseEndNudge(ctx context.Context, principalID, id, expectedNudgeOn, nudgedOn, nudgeOn string) (bool, error)
	// DeleteLooseEnd deletes the item while it was last changed at expected.
	DeleteLooseEnd(ctx context.Context, principalID, id string, expected time.Time) (bool, error)
	// ExpireLooseEnds marks the principal's open items due at or before now expired.
	ExpireLooseEnds(ctx context.Context, principalID string, now time.Time) (int, error)
	// PruneLooseEnds deletes every principal's items no longer open that were due before before.
	PruneLooseEnds(ctx context.Context, before time.Time) (int, error)
	LoadLooseEndScan(ctx context.Context, principalID string) (ScanState, bool, error)
	// CreateLooseEndScan stores the principal's first scan state unless one exists.
	CreateLooseEndScan(ctx context.Context, principalID string, state ScanState) error
	// SaveLooseEndScan replaces the principal's scan state only while one exists, so a scan that
	// ends after the switch was turned off does not bring it back.
	SaveLooseEndScan(ctx context.Context, principalID string, state ScanState) error
	// ResetLooseEndScan forgets how far the principal's scan has read.
	ResetLooseEndScan(ctx context.Context, principalID string) error
}

// Scanner runs one principal's daily scan of mail and calendar, after the nudges.
type Scanner interface {
	Scan(ctx context.Context, principalID string, now time.Time) error
}

// DueItem is an item due for a nudge, with what the run learned of it since it was tracked.
type DueItem struct {
	Item
	// WroteAgain is when the user last sent a message in a sent promise's thread after the promise,
	// zero when they did not or the thread could not be read.
	WroteAgain time.Time
}

// Nudger sends one principal one message listing the items due for a nudge, with their buttons, in
// the context's tab, and returns its text and the message it went out as.
type Nudger interface {
	Nudge(ctx context.Context, principalID string, items []DueItem) (string, schedule.Sent, error)
}

// ParseDue reads a due date. A date alone is the end of that local day in location, with allDay
// set; a date and time with an offset is that instant; one without an offset is read in location.
func ParseDue(value string, location *time.Location) (at time.Time, allDay bool, ok bool) {
	value = strings.TrimSpace(value)
	if day, err := time.ParseInLocation(DateLayout, value, location); err == nil {
		return endOfDay(day, location), true, true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02 15:04Z07:00", "2006-01-02 15:04:05Z07:00"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, false, true
		}
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, location); err == nil {
			return parsed, false, true
		}
	}
	return time.Time{}, false, false
}

// endOfDay is the last second of the local day that day falls on.
func endOfDay(day time.Time, location *time.Location) time.Time {
	local := day.In(location)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, location).Add(-time.Second)
}

// civil is the local date of at in location, as midnight UTC, so whole days can be added without
// a daylight-saving change in the way.
func civil(at time.Time, location *time.Location) time.Time {
	local := at.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

// LocalDate is the local date of at in location, as YYYY-MM-DD.
func LocalDate(at time.Time, location *time.Location) string {
	return civil(at, location).Format(DateLayout)
}

// dayBounds is the local day that at falls on, as the instants it starts and ends.
func dayBounds(at time.Time, location *time.Location) (time.Time, time.Time) {
	local := at.In(location)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	return start, time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, location)
}

// runOn is the instant the daily job runs on the local date day, a civil date, at the local time
// of day runTime.
func runOn(day time.Time, location *time.Location, runTime time.Duration) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), 0, int(runTime/time.Minute), 0, 0, location)
}

// firstNudge is the local date an item of the kind due at due is first nudged on: its lead time
// before the local due date, the day before when that day's run, at runTime, would come at or after
// the due time, or today when it was found inside its lead time.
func firstNudge(kind Kind, due, now time.Time, location *time.Location, runTime time.Duration) string {
	day := civil(due, location).AddDate(0, 0, -kind.leadDays())
	if !runOn(day, location, runTime).Before(due) {
		day = day.AddDate(0, 0, -1)
	}
	if today := civil(now, location); day.Before(today) {
		day = today
	}
	return day.Format(DateLayout)
}

// nextNudge is the nudge day an item takes once it was nudged on nudgedOn: an expiry's second
// nudge while that is still ahead, and otherwise the day it already has, which a nudge on it
// leaves behind.
func nextNudge(item Item, nudgedOn string, location *time.Location) string {
	if item.Kind == KindExpiry {
		second := civil(item.DueAt, location).AddDate(0, 0, -expirySecondLeadDays).Format(DateLayout)
		if second > nudgedOn {
			return second
		}
	}
	return item.NudgeOn
}

// validate turns a draft into a new item due after now, or says why it cannot be tracked. A title
// that holds a link is refused; a bare web address and a phone number are stripped from it, phone
// numbers and links from the counterpart, and both are cut to their caps. Every item found outside
// chat holds someone else's text, so it is outside whatever the draft says. runTime is the daily
// job's local time of day, which the first nudge must come before the due time at.
func validate(draft Draft, now time.Time, location *time.Location, runTime time.Duration) (Item, string) {
	item := Item{Kind: draft.Kind, Source: draft.Source, Outside: draft.Outside || draft.Source != SourceChat, Status: StatusOpen}
	if !item.Kind.Valid() {
		return Item{}, "kind must be cancel_window, return_window, renewal, trial_end, expiry, or promise."
	}
	if !item.Source.Valid() {
		return Item{}, "source must be mail, sent, calendar, or chat."
	}
	var reason string
	if item.Title, reason = cleanTitle(draft.Title); reason != "" {
		return Item{}, reason
	}
	item.Counterpart = cleanCounterpart(draft.Counterpart)
	item.Account = clip.Runes(oneLine(draft.Account), maximumAccountRunes)
	item.SourceRef = clip.Runes(oneLine(draft.SourceRef), maximumRefRunes)
	item.ThreadRef = clip.Runes(oneLine(draft.ThreadRef), maximumRefRunes)
	if item.Source != SourceChat && item.SourceRef == "" {
		return Item{}, "a mail, sent, or calendar item needs its message or event id."
	}
	due, allDay, ok := ParseDue(draft.Due, location)
	switch {
	case !ok:
		return Item{}, "due must be YYYY-MM-DD, or a date and time such as 2026-11-01T09:30."
	case !due.After(now):
		return Item{}, "due must be in the future."
	case due.Sub(now) > MaximumAhead:
		return Item{}, "due must be at most 400 days away."
	}
	item.DueAt, item.DueAllDay = due.UTC().Truncate(time.Millisecond), allDay
	item.NudgeOn = firstNudge(item.Kind, item.DueAt, now, location, runTime)
	return item, ""
}

// cleanTitle makes a title one line of plain text without web addresses or phone numbers, cut to
// its cap. A word shaped like a domain, as in "Booking.com reservation", is stripped; a link, with
// a scheme, www., or a path after a domain, refuses the title, as does one left empty.
func cleanTitle(value string) (string, string) {
	value = oneLine(value)
	for _, word := range strings.Fields(value) {
		if isLink(word) {
			return "", "title must not contain a link."
		}
	}
	value = oneLine(placeholder.WithoutPhones(trip.WithoutAddresses(value)))
	if value == "" {
		return "", "title is required, without links, web addresses, or phone numbers."
	}
	return clip.Runes(value, MaximumTitleRunes), ""
}

func isLink(word string) bool {
	lower := strings.ToLower(word)
	if strings.Contains(lower, "://") {
		return true
	}
	return trip.WithoutAddresses(word) == "" && (strings.Contains(lower, "/") || strings.Contains(lower, "www"))
}

// cleanCounterpart makes a counterpart one line of plain text without phone numbers or links, cut
// to its cap. A bare domain stays, since a sender's domain is what it often is.
func cleanCounterpart(value string) string {
	var kept []string
	for _, word := range strings.Fields(oneLine(placeholder.WithoutPhones(value))) {
		if !strings.Contains(word, "/") && !strings.HasPrefix(strings.ToLower(word), "www.") {
			kept = append(kept, word)
		}
	}
	return clip.Runes(strings.Join(kept, " "), MaximumCounterpartRunes)
}

// oneLine makes a value one line without control or invisible formatting characters.
func oneLine(value string) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "")
	}
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, value)), " ")
}

func newID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate loose end identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

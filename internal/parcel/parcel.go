// Package parcel keeps the parcels a user is waiting for: their model, the carriers that deliver
// them, and the interfaces the Home Assistant source, the mail source, the Telegram cards, and the
// tracker loop meet at.
package parcel

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/integration"
)

// Integration names the Parcels switch.
const Integration = "parcels"

// Switch is each principal's Parcels switch, off until the principal turns it on.
var Switch = integration.Switch{Integration: Integration, Name: "Parcels"}

// ToolTrack is the write tool that starts following a tracking number.
const ToolTrack = "parcel_track"

// TrackArguments are the arguments of ToolTrack. Carrier is optional and names a carrier's ID.
type TrackArguments struct {
	Number  string `json:"number"`
	Carrier string `json:"carrier,omitempty"`
}

// Source says where a parcel was found.
type Source string

const (
	// SourceHomeAssistant is a carrier integration in Home Assistant.
	SourceHomeAssistant Source = "ha"
	// SourceMail is shipping mail.
	SourceMail Source = "mail"
)

// CardKind is the kind of Telegram message a parcel's card is.
type CardKind string

const (
	CardWidget CardKind = "widget"
	CardText   CardKind = "text"
)

// Retention and tombstone lifetimes.
const (
	// Retention is how long a parcel is kept after it was delivered or stopped.
	Retention = 14 * 24 * time.Hour
	// TombstoneLifetime is how long a tombstone is kept after a source last listed its parcel.
	TombstoneLifetime = 30 * 24 * time.Hour
	// IdleLifetime is how long a parcel not yet delivered is followed after a source last listed it.
	IdleLifetime = 30 * 24 * time.Hour
)

// Parcel is one parcel a user is waiting for. Title, Sender, and PickupPoint come from outside
// text and are shown as plain text only.
type Parcel struct {
	ID          string
	PrincipalID string
	// Carrier is a carrier's ID in the list, or another lower-case word for one the list lacks.
	Carrier  string
	Number   string
	OrderRef string
	Title    string
	Sender   string
	Source   Source
	// SourceKey is the source's own reference, such as a Home Assistant package key.
	SourceKey   string
	Status      Status
	WindowFrom  time.Time
	WindowTo    time.Time
	DeliveredAt time.Time
	PickupPoint string
	Link        string
	CardMessage int64
	CardKind    CardKind
	CreatedAt   time.Time
	UpdatedAt   time.Time
	StoppedAt   time.Time
}

var (
	carrierPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	linkPattern    = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)
	// domainPattern is a bare address such as shop.example/track, which outside text never needs.
	domainPattern = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}\b(?:/\S*)?`)

	knownCarriers = DefaultCarriers()
)

// PlainText is outside text, such as a title or a sender from a carrier or a shop, as one line
// without links, bare domains, or control and format characters, cut to the limit.
func PlainText(text string, limit int) string {
	return oneLine(domainPattern.ReplaceAllString(linkPattern.ReplaceAllString(text, ""), ""), limit)
}

// oneLine is text as one line without control or format characters, cut to the limit.
func oneLine(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, text)
	return clip.Runes(strings.Join(strings.Fields(text), " "), limit)
}

// Keys are every identity the parcel is known by: its carrier and number, under each carrier that
// shares the number's pattern with its own, its source's key, and its sender and order reference,
// whichever it has. A tombstone is left under each, so a source that knows the parcel by only one
// of them still finds it.
func (p Parcel) Keys() []string {
	var keys []string
	if p.Number != "" {
		for _, carrier := range knownCarriers.Twins(p.Carrier, p.Number) {
			keys = append(keys, carrier+"|"+p.Number)
		}
	}
	if p.SourceKey != "" {
		keys = append(keys, "source|"+string(p.Source)+"|"+p.SourceKey)
	}
	if p.OrderRef != "" && p.Sender != "" {
		keys = append(keys, "order|"+p.Sender+"|"+p.OrderRef)
	}
	return keys
}

// Stopped reports whether the user stopped tracking the parcel.
func (p Parcel) Stopped() bool { return !p.StoppedAt.IsZero() }

// Validate checks what the store holds to its limits: a parcel is found by number, source key, or
// sender and order reference, and its texts are short.
func (p Parcel) Validate() error {
	switch {
	case !carrierPattern.MatchString(p.Carrier):
		return errors.New("the carrier is not a short lower-case word")
	case p.Source != SourceHomeAssistant && p.Source != SourceMail:
		return errors.New("the source is not known")
	case !p.Status.Valid():
		return errors.New("the status is not known")
	case p.CardKind != "" && p.CardKind != CardWidget && p.CardKind != CardText:
		return errors.New("the card kind is not known")
	case p.Number == "" && p.OrderRef == "" && p.SourceKey == "":
		return errors.New("the parcel has no number, order reference, or source key")
	case p.Number == "" && p.OrderRef != "" && p.SourceKey == "" && p.Sender == "":
		return errors.New("an order reference needs its sender")
	}
	if p.Number != "" {
		if normalised, ok := NormalizeNumber(p.Number); !ok || normalised != p.Number {
			return errors.New("the number is not normalised")
		}
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"order reference", p.OrderRef, 64}, {"title", p.Title, 200}, {"sender", p.Sender, 100},
		{"source key", p.SourceKey, 200}, {"pickup point", p.PickupPoint, 200}, {"link", p.Link, 500},
	} {
		if utf8.RuneCountInString(field.value) > field.limit {
			return fmt.Errorf("the %s is too long", field.name)
		}
	}
	if p.Link != "" {
		link, err := url.Parse(p.Link)
		if err != nil || link.Scheme != "https" || link.Hostname() == "" {
			return errors.New("the link is not an https address")
		}
	}
	return nil
}

// AddedNumber is a number a principal added to a carrier integration through the assistant.
type AddedNumber struct {
	PrincipalID string
	Carrier     string
	Number      string
	// Domain and EntryID name the Home Assistant integration and config entry that tracks it.
	Domain  string
	EntryID string
	AddedAt time.Time
}

// Outcome is what became of a proposal to add a number.
type Outcome string

const (
	// OutcomeProposed is a number offered and still undecided, expired, or replaced.
	OutcomeProposed Outcome = "proposed"
	OutcomeAdded    Outcome = "added"
	// OutcomeDeclined is a number the user explicitly rejected.
	OutcomeDeclined Outcome = "declined"
)

// ProposedNumber is a number the assistant proposed to add, with its outcome. ApprovalID is the
// approval the latest offer waits on while the outcome is proposed, so a proposal still pending can
// be told from one that expired or was replaced; it is empty once decided, and never restored.
// Offers counts how often the number was offered.
type ProposedNumber struct {
	PrincipalID string
	Number      string
	Carrier     string
	Outcome     Outcome
	ApprovalID  string
	Offers      int
	ProposedAt  time.Time
	UpdatedAt   time.Time
}

// Tombstone remembers a deleted or stopped parcel by its Key, so a source that still lists it never
// brings it back.
type Tombstone struct {
	PrincipalID string
	Key         string
	CreatedAt   time.Time
	LastSeenAt  time.Time
}

// Settings are one user's parcel settings.
type Settings struct {
	PrincipalID string
	// Postcode is added to the integrations that take one when a number is added.
	Postcode string
}

// MailCursor is how far the mail job has read: the epoch second of the newest message it read and
// the IDs of those it already read at or after that second.
type MailCursor struct {
	AfterEpoch int64
	Processed  []string
}

// Repository stores the parcel domain. Every operation is scoped to one principal, except where
// noted.
type Repository interface {
	// UpsertParcel merges the parcel into the principal's by carrier and number; a parcel without
	// a number is found by sender and order reference, or by source key, whether or not the stored
	// one has gained a number since, and one with a number also takes over the number-less parcel
	// of its order reference. It returns the stored parcel and whether it is new. A stopped parcel
	// is returned unchanged; a status only moves forward, except to or from a problem.
	UpsertParcel(ctx context.Context, parcel Parcel) (Parcel, bool, error)
	LoadParcel(ctx context.Context, principalID, id string) (Parcel, bool, error)
	// ListParcels returns the principal's parcels, oldest first.
	ListParcels(ctx context.Context, principalID string) ([]Parcel, error)
	// SaveCard records the message that shows the parcel, or clears it when messageID is zero.
	SaveCard(ctx context.Context, principalID, id string, messageID int64, kind CardKind) error
	// StopParcel marks the parcel stopped and leaves its tombstone.
	StopParcel(ctx context.Context, principalID, id string, at time.Time) (Parcel, bool, error)
	// DeleteExpired deletes, for every principal, the parcels delivered or stopped before the
	// instant, leaving a tombstone for each, and returns how many.
	DeleteExpired(ctx context.Context, before, now time.Time) (int, error)
	// DeleteIdle deletes the principal's parcels, not stopped and not yet delivered, that no source
	// has updated since the instant, and returns them. It leaves no tombstone: the user did not stop
	// them, so news of one, or the user's own request, follows it afresh.
	DeleteIdle(ctx context.Context, principalID string, before time.Time) ([]Parcel, error)
	// TombstoneSeenAny reports whether any of the keys, which are a parcel's Keys, is tombstoned,
	// and notes that a source listed each one that is.
	TombstoneSeenAny(ctx context.Context, principalID string, keys []string, now time.Time) (bool, error)
	// PruneTombstones drops the tombstones no source has listed since the instant, for every
	// principal.
	PruneTombstones(ctx context.Context, before time.Time) (int, error)

	SaveAddedNumber(ctx context.Context, number AddedNumber) error
	LoadAddedNumber(ctx context.Context, principalID, carrier, number string) (AddedNumber, bool, error)
	ListAddedNumbers(ctx context.Context, principalID string) ([]AddedNumber, error)
	DeleteAddedNumber(ctx context.Context, principalID, carrier, number string) error
	// AddedNumberOwners names every principal that added the number to the carrier, across
	// principals: it decides whose parcel a Home Assistant read is.
	AddedNumberOwners(ctx context.Context, carrier, number string) ([]string, error)

	// RecordProposal notes that a number was offered under the proposal's ApprovalID and counts the
	// offer; it leaves an added or declined number as it is.
	RecordProposal(ctx context.Context, proposal ProposedNumber) error
	// SetProposalOutcome records what became of a proposed number.
	SetProposalOutcome(ctx context.Context, principalID, number string, outcome Outcome, at time.Time) error
	LoadProposal(ctx context.Context, principalID, number string) (ProposedNumber, bool, error)

	// RecordNotice records that the status's notice was sent for the parcel and reports whether it
	// was new, so only the first caller sends it.
	RecordNotice(ctx context.Context, principalID, parcelID string, status Status, at time.Time) (bool, error)

	LoadMailCursor(ctx context.Context, principalID string) (MailCursor, bool, error)
	SaveMailCursor(ctx context.Context, principalID string, cursor MailCursor, at time.Time) error

	LoadSettings(ctx context.Context, principalID string) (Settings, error)
	SaveSettings(ctx context.Context, settings Settings) error
}

// SourceState is how a Home Assistant source last read.
type SourceState string

const (
	SourceOK SourceState = "ok"
	// SourceUnknown means nothing has been read yet.
	SourceUnknown SourceState = "unknown"
	// SourceStale means the last read failed and the parcels shown are from an earlier one.
	SourceStale SourceState = "stale"
	// SourceMissing means the assigned config entry is not in Home Assistant.
	SourceMissing SourceState = "missing"
)

// SourceInfo is one Home Assistant config entry assigned to a principal and how it reads.
type SourceInfo struct {
	EntryID string
	// Domain is the integration's Home Assistant domain, which is on the closed list.
	Domain string
	Title  string
	State  SourceState
	ReadAt time.Time
}

// Reading is what the assigned sources listed for one principal.
type Reading struct {
	Parcels []Parcel
	// Stale is set when a source failed to read and its last parcels stand in.
	Stale bool
	// Unread is set while an assigned source that Home Assistant knows has never read well, so the
	// parcels it will list are still unknown.
	Unread bool
}

// Sources is the Home Assistant side: the principal's assigned sources and their parcels.
type Sources interface {
	// Assigned returns the config entry IDs assigned to the principal in configuration.
	Assigned(principalID string) []string
	// List returns the principal's assigned sources with their state.
	List(ctx context.Context, principalID string) ([]SourceInfo, error)
	// Read returns the principal's parcels from their assigned sources and added numbers.
	Read(ctx context.Context, principalID string) (Reading, error)
}

// CardMode is the state a card is edited to.
type CardMode string

const (
	// CardCurrent shows the parcel as stored.
	CardCurrent CardMode = "current"
	// CardStopped shows the final "Stopped" state.
	CardStopped CardMode = "stopped"
	// CardTrackingOff shows that Parcels was turned off.
	CardTrackingOff CardMode = "tracking_off"
	// CardIdle shows the final state of a parcel no source updated for IdleLifetime, which is no
	// longer followed until there is news of it.
	CardIdle CardMode = "idle"
)

// ErrCardGone is returned by EditCard when the card's message or its tab no longer exists.
var ErrCardGone = errors.New("the parcel card is gone")

// Cards is the Telegram side: one card per parcel, edited in place, and short notices.
type Cards interface {
	// SendCard sends the parcel's card, silently when asked, and returns its message and kind.
	SendCard(ctx context.Context, parcel Parcel, silent bool) (int64, CardKind, error)
	// EditCard edits the parcel's stored card, returning ErrCardGone when it cannot be.
	EditCard(ctx context.Context, parcel Parcel, mode CardMode) error
	// SendNotice sends the status notice as a reply to the card, without sound when asked.
	SendNotice(ctx context.Context, parcel Parcel, silent bool) error
	// SendSummary posts the one line that follows a silent baseline, such as "Tracking 3 parcels".
	SendSummary(ctx context.Context, principalID string, count int) error
}

// Tracker is the loop that keeps cards current, which the tools and the Stop control call.
type Tracker interface {
	// Stop stops tracking one of the principal's parcels: it is tombstoned, a number the user added
	// is removed from Home Assistant on a best-effort basis, and its card shows Stopped. Stop owns all
	// of that; the Telegram tap only authorizes it. It reports false for an unknown parcel.
	Stop(ctx context.Context, principalID, parcelID string) (Parcel, bool, error)
}

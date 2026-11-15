package parcel

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

// ErrNotProposed means a number is not offered for adding to Home Assistant, and wraps the reason.
var ErrNotProposed = errors.New("the number is not proposed")

// ErrNoIntegration is the reason, wrapped in ErrNotProposed, when the carrier has no Home Assistant
// integration that tracks numbers: a parcel mail follows is all it can be.
var ErrNoIntegration = errors.New("no Home Assistant integration tracks numbers of the carrier")

var (
	errNotNumber    = errors.New("the number is not a tracking number")
	errAlreadyAdded = errors.New("the number is already tracked in Home Assistant")
	errTombstoned   = errors.New("the user stopped tracking the number")
	errPending      = errors.New("a proposal for the number is waiting")
	errDeclined     = errors.New("the user declined the number")
	errUnanswered   = errors.New("the number went unanswered on every card it was offered on")
)

// MaximumOffers is how many cards a number may be offered on before it is left alone.
const MaximumOffers = 3

// numberTrackingCarriers are the carriers whose Home Assistant integration tracks a number the
// user adds.
var numberTrackingCarriers = []string{"dhl", "gls"}

// TracksNumbers reports whether the carrier's Home Assistant integration tracks numbers.
func TracksNumbers(carrier string) bool { return slices.Contains(numberTrackingCarriers, carrier) }

// ApprovalLoader reads an approval, which the SQLite store implements.
type ApprovalLoader interface {
	LoadApproval(ctx context.Context, id string) (approval.Record, bool, error)
}

var trackSummary = locale.T("Track parcel %s with %s in Home Assistant.", "Отслеживать посылку %s (%s) в Home Assistant.")

// Proposals decides which numbers are worth proposing for adding to Home Assistant and keeps the
// record of what was proposed. The mail job, the inbox check, and the tool share it.
type Proposals struct {
	Repository Repository
	// Approvals tells whether a proposal still waits; without it a proposal that was offered counts
	// as waiting.
	Approvals ApprovalLoader
	Carriers  Carriers
	Now       func() time.Time
}

// Proposal returns the proposal that adds the number to Home Assistant, or an error wrapping
// ErrNotProposed when it should not be offered: the carrier has no integration that tracks numbers,
// the number is already tracked there, tombstoned, waiting on a card, or declined. A number is
// tracked when the user added it or Home Assistant lists it; a parcel only mail follows is exactly
// what a proposal is for. The carrier may be empty when the number's pattern names one.
func (p Proposals) Proposal(ctx context.Context, principalID, number, carrier string) (approval.Proposal, error) {
	return p.proposal(ctx, principalID, number, carrier, false)
}

// Requested is Proposal for a number the user asked to follow in their own message. Neither an
// earlier decline nor offers that went unanswered stand in the way of the user's own request, so a
// declined number is set back to proposed and may be added; a number they stopped following is
// still refused.
func (p Proposals) Requested(ctx context.Context, principalID, number, carrier string) (approval.Proposal, error) {
	proposal, err := p.proposal(ctx, principalID, number, carrier, true)
	if err != nil {
		return proposal, err
	}
	normalised, _ := NormalizeNumber(number)
	if proposed, found, err := p.Repository.LoadProposal(ctx, principalID, normalised); err != nil {
		return approval.Proposal{}, err
	} else if found && proposed.Outcome == OutcomeDeclined {
		if err := p.Repository.SetProposalOutcome(ctx, principalID, normalised, OutcomeProposed, p.now()); err != nil {
			return approval.Proposal{}, err
		}
	}
	return proposal, nil
}

func (p Proposals) proposal(ctx context.Context, principalID, number, carrier string, requested bool) (approval.Proposal, error) {
	normalised, ok := NormalizeNumber(number)
	if !ok {
		return approval.Proposal{}, errors.Join(ErrNotProposed, errNotNumber)
	}
	number = normalised
	if carrier == "" {
		if found := p.Carriers.Detect(number); len(found) == 1 {
			carrier = found[0].ID
		}
	}
	if !TracksNumbers(carrier) {
		return approval.Proposal{}, errors.Join(ErrNotProposed, ErrNoIntegration)
	}
	if _, added, err := p.Repository.LoadAddedNumber(ctx, principalID, carrier, number); err != nil {
		return approval.Proposal{}, err
	} else if added {
		return approval.Proposal{}, errors.Join(ErrNotProposed, errAlreadyAdded)
	}
	parcels, err := p.Repository.ListParcels(ctx, principalID)
	if err != nil {
		return approval.Proposal{}, err
	}
	if slices.ContainsFunc(parcels, func(held Parcel) bool { return held.Number == number && held.Source == SourceHomeAssistant }) {
		return approval.Proposal{}, errors.Join(ErrNotProposed, errAlreadyAdded)
	}
	if tombstoned, err := p.Repository.TombstoneSeenAny(ctx, principalID, Parcel{Carrier: carrier, Number: number}.Keys(), p.now()); err != nil {
		return approval.Proposal{}, err
	} else if tombstoned {
		return approval.Proposal{}, errors.Join(ErrNotProposed, errTombstoned)
	}
	if err := p.checkProposed(ctx, principalID, number, requested); err != nil {
		return approval.Proposal{}, err
	}
	arguments, err := json.Marshal(TrackArguments{Number: number, Carrier: carrier})
	if err != nil {
		return approval.Proposal{}, err
	}
	name := carrier
	if found, ok := p.Carriers.ByID(carrier); ok {
		name = found.Name
	}
	return approval.Proposal{Tool: ToolTrack, Arguments: arguments, Summary: trackSummary.Format(ctx, number, name)}, nil
}

func (p Proposals) checkProposed(ctx context.Context, principalID, number string, requested bool) error {
	proposed, found, err := p.Repository.LoadProposal(ctx, principalID, number)
	if err != nil || !found {
		return err
	}
	switch {
	case proposed.Outcome == OutcomeAdded:
		return errors.Join(ErrNotProposed, errAlreadyAdded)
	case proposed.Outcome == OutcomeDeclined && !requested:
		return errors.Join(ErrNotProposed, errDeclined)
	case proposed.Offers >= MaximumOffers && proposed.ApprovalID != "" && !requested:
		return errors.Join(ErrNotProposed, errUnanswered)
	case proposed.ApprovalID == "":
		return nil
	}
	if p.Approvals == nil {
		return errors.Join(ErrNotProposed, errPending)
	}
	record, found, err := p.Approvals.LoadApproval(ctx, proposed.ApprovalID)
	if err != nil {
		return err
	}
	if found && record.PrincipalID == principalID && record.Status == approval.StatusPending && p.now().Before(record.ExpiresAt) {
		return errors.Join(ErrNotProposed, errPending)
	}
	return nil
}

// Offered records that the number was offered on the approval, so it is not offered again while
// that card waits.
func (p Proposals) Offered(ctx context.Context, principalID, number, carrier, approvalID string) error {
	normalised, ok := NormalizeNumber(number)
	if !ok {
		return errors.Join(ErrNotProposed, errNotNumber)
	}
	now := p.now()
	return p.Repository.RecordProposal(ctx, ProposedNumber{
		PrincipalID: principalID, Number: normalised, Carrier: carrier, Outcome: OutcomeProposed, ApprovalID: approvalID, ProposedAt: now, UpdatedAt: now,
	})
}

func (p Proposals) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// ValidCarrier reports whether the word can stand as a carrier: a short lower-case word.
func ValidCarrier(carrier string) bool { return carrierPattern.MatchString(carrier) }

// FollowByMail starts following a number through mail alone: a local parcel that touches nothing in
// Home Assistant and needs no approval. The carrier is the number's own when its pattern names one,
// else the given word, else "other", except that a number several carriers share always stands
// under one of them (Settle). A number already followed, or tombstoned, is left alone.
func (p Proposals) FollowByMail(ctx context.Context, principalID, number, carrier, title, sender string) error {
	number, ok := NormalizeNumber(number)
	if !ok {
		return errNotNumber
	}
	word := strings.ToLower(strings.TrimSpace(carrier))
	switch found := p.Carriers.Detect(number); {
	case len(found) == 1:
		carrier = found[0].ID
	case ValidCarrier(word):
		carrier = word
	default:
		carrier = "other"
	}
	carrier = p.Carriers.Settle(carrier, number)
	parcels, err := p.Repository.ListParcels(ctx, principalID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(parcels, func(held Parcel) bool { return held.Number == number }) {
		return nil
	}
	now := p.now()
	followed := Parcel{
		PrincipalID: principalID, Carrier: carrier, Number: number, Source: SourceMail, Status: StatusUnknown,
		Title: PlainText(title, maximumTitleRunes), Sender: PlainText(sender, maximumSenderRunes), CreatedAt: now, UpdatedAt: now,
	}
	if known, ok := p.Carriers.ByID(carrier); ok {
		followed.Link = known.Link(number)
	}
	if tombstoned, err := p.Repository.TombstoneSeenAny(ctx, principalID, followed.Keys(), now); err != nil || tombstoned {
		return err
	}
	_, _, err = p.Repository.UpsertParcel(ctx, followed)
	return err
}

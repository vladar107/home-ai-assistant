package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// Offer sends proposals that no chat turn made, such as a scheduled job's, to the principal's chat
// at once, as one bundle shown under heading in the given tab. Like a turn's first proposal it
// replaces the tab's pending bundle, and it stays confirmable for lifetime. Nobody asked for these
// proposals, so the intent check is skipped, and confirming them continues no conversation and
// leaves no notice for the tab's next turn. A delivery that fails is retried by the sweep until the
// bundle expires; the records are returned either way.
func (s *Service) Offer(ctx context.Context, principalID string, chatID, threadID int64, heading string, lifetime time.Duration, proposals []Proposal) ([]Record, error) {
	return s.offer(ctx, principalID, chatID, threadID, heading, lifetime, proposals, false)
}

// OfferIfIdle is Offer that leaves a tab alone while a bundle there still waits for the user, a
// chat turn's or one offered before, and then returns ErrBundleWaiting. The check and the offer
// hold the proposal lock together, so a chat turn proposing in the same tab is never replaced.
func (s *Service) OfferIfIdle(ctx context.Context, principalID string, chatID, threadID int64, heading string, lifetime time.Duration, proposals []Proposal) ([]Record, error) {
	return s.offer(ctx, principalID, chatID, threadID, heading, lifetime, proposals, true)
}

func (s *Service) offer(ctx context.Context, principalID string, chatID, threadID int64, heading string, lifetime time.Duration, proposals []Proposal, ifIdle bool) ([]Record, error) {
	if principalID == "" || chatID == 0 || strings.TrimSpace(heading) == "" || lifetime <= 0 || len(proposals) == 0 {
		return nil, fmt.Errorf("approval offer is incomplete")
	}
	if len(proposals) > maximumBundleItems {
		return nil, ErrBundleFull
	}
	ctx = thread.With(ctx, threadID)
	now := s.now()
	records := make([]Record, 0, len(proposals))
	for position, proposal := range proposals {
		if proposal.Tool == "" || strings.TrimSpace(proposal.Summary) == "" {
			return nil, fmt.Errorf("approval proposal is incomplete")
		}
		s.mu.Lock()
		_, executable := s.executors[proposal.Tool]
		s.mu.Unlock()
		if !executable {
			return nil, fmt.Errorf("approval proposal names a tool without an executor")
		}
		summary := strings.TrimSpace(proposal.Summary)
		if !SummaryFits(summary) {
			return nil, ErrSummaryTooLong
		}
		canonical, err := canonicalJSON(proposal.Arguments)
		if err != nil {
			return nil, fmt.Errorf("canonicalize approval arguments: %w", err)
		}
		id, err := newID()
		if err != nil {
			return nil, err
		}
		bundleID := id
		if position > 0 {
			bundleID = records[0].ID
		}
		digest := sha256.Sum256(canonical)
		records = append(records, Record{
			ID: id, BundleID: bundleID, Position: position, PrincipalID: principalID, Tool: proposal.Tool,
			Arguments: canonical, Hash: hex.EncodeToString(digest[:]), Summary: summary, Status: StatusPending,
			ThreadID: thread.From(ctx), CreatedAt: now, ExpiresAt: now.Add(lifetime), Heading: heading,
		})
	}

	// The bundle is stored whole or not at all, and a turn proposing in the same tab never joins it,
	// since joinable refuses a bundle with a heading. The lock is held until the message is recorded,
	// as afterDelivery holds it.
	proposing := s.proposalLock(principalID)
	proposing.Lock()
	if ifIdle {
		pending, err := s.repository.PendingApprovals(ctx, principalID, thread.From(ctx))
		if err != nil {
			proposing.Unlock()
			return nil, fmt.Errorf("load pending approvals: %w", err)
		}
		// A record past its expiry that the sweep has not reached yet waits for nobody.
		if slices.ContainsFunc(pending, func(record Record) bool { return now.Before(record.ExpiresAt) }) {
			proposing.Unlock()
			return nil, ErrBundleWaiting
		}
	}
	replaced, err := s.repository.CreateApprovals(ctx, records)
	if err != nil {
		proposing.Unlock()
		return nil, fmt.Errorf("store approval: %w", err)
	}
	for _, record := range records {
		s.logger.InfoContext(ctx, "approval.proposed", "principal_id", principalID, "approval_id", record.ID, "bundle_id", record.BundleID, "tool", record.Tool, "replaced", len(replaced) > 0)
		s.recordStatus(ctx, record, "proposed")
	}
	err = s.send(ctx, records, chatID)
	proposing.Unlock()
	s.report(ctx, replaced)
	return records, err
}

// offered reports whether a bundle was offered outside a chat turn.
func offered(bundle []Record) bool {
	return len(bundle) > 0 && bundle[0].Heading != ""
}

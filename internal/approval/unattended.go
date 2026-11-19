package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
)

// UnattendedTool is the one tool ActUnattended makes: the assistant mailbox's routine reply.
const UnattendedTool = "assistant_mail_reply_routine"

// UnattendedRequest starts the request ID of an unattended action, which no chat request has; a
// repository leaves such records out of the "Done" messages, since the caller reports them itself.
const UnattendedRequest = "unattended:"

// ActUnattended makes at once, without asking, the action an unattended run decided on, and records
// it as a direct action that cannot be undone. It is a deliberate, narrow exception to the rule that
// unattended or externally exposed runs only propose: it takes only UnattendedTool, whose own code
// enforces whom it may answer and how often, and runs it through the executor the caller passes,
// never one registered for confirmations. The record keeps the whole action in its arguments, so
// every autonomous email can be audited. It returns the finished record, done or failed.
func (s *Service) ActUnattended(ctx context.Context, principalID string, proposal Proposal, executor Executor) (Record, error) {
	if proposal.Tool != UnattendedTool || executor == nil || !slices.Contains(executor.ActionTools(), proposal.Tool) {
		return Record{}, fmt.Errorf("approval: %q cannot act without asking", proposal.Tool)
	}
	summary := strings.TrimSpace(proposal.Summary)
	if principalID == "" || summary == "" {
		return Record{}, fmt.Errorf("approval proposal is incomplete")
	}
	if utf16Units(summary) > maximumSummaryRunes {
		return Record{}, ErrSummaryTooLong
	}
	canonical, err := canonicalJSON(proposal.Arguments)
	if err != nil {
		return Record{}, fmt.Errorf("canonicalize approval arguments: %w", err)
	}
	digest := sha256.Sum256(canonical)
	id, err := newID()
	if err != nil {
		return Record{}, err
	}
	now := s.now()
	record := Record{
		ID:          id,
		BundleID:    id,
		PrincipalID: principalID,
		Tool:        proposal.Tool,
		Arguments:   canonical,
		Hash:        hex.EncodeToString(digest[:]),
		Summary:     summary,
		Status:      StatusConfirmed,
		CreatedAt:   now,
		// It is never shown in a "Done" message, which is what a direct record's expiry bounds.
		ExpiresAt: now.Add(time.Millisecond),
		StartedAt: now,
		Mode:      ModeDirect,
		RequestID: UnattendedRequest + id,
	}
	if err := s.repository.CreateDirectApproval(ctx, record); err != nil {
		return Record{}, fmt.Errorf("store unattended approval: %w", err)
	}
	succeeded, reason, outcome, userText, _ := s.runExecutor(ctx, executor, record)
	status := StatusFailed
	if succeeded {
		status, reason = StatusDone, ""
	}
	outcome, userText = clip.Runes(outcome, outcomeRunes), clip.Runes(userText, outcomeRunes)
	if _, err := s.repository.FinishDirectApproval(context.WithoutCancel(ctx), id, status, reason, outcome, userText, nil, time.Time{}); err != nil {
		s.logger.ErrorContext(ctx, "approval.finish_failed", "principal_id", principalID, "approval_id", id, "status", status)
	}
	record.Status, record.Reason, record.Outcome, record.UserOutcome, record.FinishedAt = status, reason, outcome, userText, s.now()
	s.recordStatus(ctx, record, status)
	s.logger.InfoContext(ctx, "approval.resolved", "principal_id", principalID, "approval_id", id, "tool", proposal.Tool, "mode", ModeDirect, "status", status, "reason", reason, "unattended", true)
	return record, nil
}

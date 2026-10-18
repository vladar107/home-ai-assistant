package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	outcomeRunes = 1000
	// directOutcomeRunes cuts an outcome in a "Done" message, so the longest summary Act accepts
	// still fits one Telegram message with it.
	directOutcomeRunes = 500
	// directItemsUnits is the item budget of one "Done" message; it leaves room in Telegram's
	// 4096-unit message for the header and the Undo line.
	directItemsUnits = 4000
	// directMessageItems is Telegram's limit on the buttons of one message.
	directMessageItems = 100
)

// Act makes the proposed action at once, without asking, when the call belongs to a chat turn that
// answers the user's own message and has read no outside content, nor any third-party content for a
// Strict proposal. Anything else, or anything it cannot tell, proposes instead. It returns the text
// for the model; a direct action that failed returns a *FailedError, which Refused reports.
func (s *Service) Act(ctx context.Context, principalID string, proposal Proposal) (string, error) {
	s.mu.Lock()
	exposure := s.exposure
	executor, executable := s.executors[proposal.Tool]
	s.mu.Unlock()
	if exposure == nil {
		return s.proposeText(ctx, principalID, proposal)
	}
	requestID, userChat := exposure.UserChatRequest(ctx)
	if !userChat || requestID == "" {
		return s.proposeText(ctx, principalID, proposal)
	}
	if principalID == "" || proposal.Tool == "" || strings.TrimSpace(proposal.Summary) == "" {
		return "", fmt.Errorf("approval proposal is incomplete")
	}
	if !executable {
		return "", fmt.Errorf("approval proposal names a tool without an executor")
	}
	summary := strings.TrimSpace(proposal.Summary)
	if utf16Units(summary)+utf16Units(DirectIntentWarning)+2 > bundleSummaryUnits {
		return "", ErrSummaryTooLong
	}
	canonical, err := canonicalJSON(proposal.Arguments)
	if err != nil {
		return "", fmt.Errorf("canonicalize approval arguments: %w", err)
	}
	digest := sha256.Sum256(canonical)
	hash := hex.EncodeToString(digest[:])
	id, err := newID()
	if err != nil {
		return "", err
	}

	// The exposure is read under the lock that orders the principal's proposals, so a call that
	// marked the run external just before is seen.
	proposing := s.proposalLock(principalID)
	proposing.Lock()
	if exposure.External(ctx, principalID) || proposal.Strict && exposure.Exposed(ctx, principalID) {
		proposing.Unlock()
		return s.proposeText(ctx, principalID, proposal)
	}
	existing, found, err := s.repository.FindDirectApproval(ctx, principalID, proposal.Tool, hash, requestID)
	if err != nil {
		proposing.Unlock()
		return "", fmt.Errorf("find direct approval: %w", err)
	}
	if found {
		proposing.Unlock()
		return s.repeatedText(existing)
	}
	now := s.now()
	record := Record{
		ID:          id,
		BundleID:    id,
		PrincipalID: principalID,
		Tool:        proposal.Tool,
		Arguments:   canonical,
		Hash:        hash,
		Summary:     summary,
		Status:      StatusConfirmed,
		CreatedAt:   now,
		ExpiresAt:   now.Add(UndoWindow),
		StartedAt:   now,
		Mode:        ModeDirect,
		RequestID:   requestID,
		ThreadID:    thread.From(ctx),
	}
	err = s.repository.CreateDirectApproval(ctx, record)
	proposing.Unlock()
	if err != nil {
		return "", fmt.Errorf("store direct approval: %w", err)
	}
	if proposal.Intent != (Intent{}) {
		s.mu.Lock()
		s.intents[id] = proposal.Intent
		s.mu.Unlock()
	}

	succeeded, reason, outcome, undo := s.runExecutor(ctx, executor, record)
	status := StatusFailed
	if succeeded {
		status, reason = StatusDone, ""
		if _, undoable := executor.(Undoer); !undoable {
			undo = nil
		}
		if len(undo) > 0 {
			record.Undo, record.UndoUntil = undo, now.Add(UndoWindow)
		}
	}
	outcome = clip.Runes(outcome, outcomeRunes)
	if _, err := s.repository.FinishDirectApproval(context.WithoutCancel(ctx), id, status, reason, outcome, record.Undo, record.UndoUntil); err != nil {
		s.logger.ErrorContext(ctx, "approval.finish_failed", "principal_id", principalID, "approval_id", id, "status", status)
	}
	record.Status, record.Reason, record.Outcome = status, reason, outcome
	if !succeeded {
		s.dropIntents([]Record{record})
	}
	s.recordStatus(ctx, record, status)
	s.logger.InfoContext(ctx, "approval.resolved", "principal_id", principalID, "approval_id", id, "tool", proposal.Tool, "mode", ModeDirect, "status", status, "reason", reason)
	return s.repeatedOrDone(record, false)
}

func (s *Service) proposeText(ctx context.Context, principalID string, proposal Proposal) (string, error) {
	if _, err := s.Propose(ctx, principalID, proposal); err != nil {
		return "", err
	}
	return ProposedText, nil
}

func (s *Service) repeatedText(record Record) (string, error) {
	if record.Status == StatusConfirmed {
		return prompt.ActExecuting, nil
	}
	return s.repeatedOrDone(record, true)
}

// repeatedOrDone tells the model what became of a direct record.
func (s *Service) repeatedOrDone(record Record, repeated bool) (string, error) {
	prefix := ""
	if repeated {
		prefix = prompt.ActRepeated + " "
		if record.Status != StatusDone && record.Status != StatusUndone {
			prefix = prompt.ActRetried + " "
		}
	}
	switch record.Status {
	case StatusDone, StatusUndone:
		text := prompt.ActNotUndoable
		if len(record.Undo) > 0 {
			text = prompt.ActUndoable
		}
		if record.Status == StatusUndone {
			text = prompt.ActUndone
		}
		return strings.Join(strings.Fields(prefix+prompt.ActDone+" "+record.Outcome+" "+text), " "), nil
	default:
		return "", &FailedError{Text: prefix + "The action failed: " + statusLine(record, outcomeRunes)}
	}
}

// sendDirect posts the "Done" messages of the principal's direct actions that no message shows yet:
// this chat request's, and those of an earlier turn whose reply never went out, while they can
// still be undone. Delivery is best-effort: an action stands even when the message is lost, and the
// user's lists still show it.
func (s *Service) sendDirect(ctx context.Context, principalID string, chatID int64) {
	unsent, err := s.repository.UnsentDirectApprovals(ctx, principalID, thread.From(ctx), s.now())
	if err != nil {
		s.logger.WarnContext(ctx, "approval.direct.unlisted", "principal_id", principalID, "error", err)
		return
	}
	if len(unsent) == 0 {
		return
	}
	s.checkIntents(ctx, unsent)
	// Another delivery of the principal could pick up the same records; the lock lets only one send
	// them.
	sending := s.proposalLock(principalID)
	sending.Lock()
	defer sending.Unlock()
	if unsent, err = s.repository.UnsentDirectApprovals(ctx, principalID, thread.From(ctx), s.now()); err != nil {
		return
	}
	for _, group := range s.directMessages(unsent) {
		messageID, err := s.deliverer.SendDirectActions(ctx, chatID, s.directActions(group))
		if err != nil {
			s.logger.WarnContext(ctx, "approval.direct.undeliverable", "principal_id", principalID, "error", err)
			return
		}
		if err := s.repository.SetApprovalMessage(ctx, recordIDs(group), chatID, messageID); err != nil {
			s.logger.WarnContext(ctx, "approval.direct.unrecorded", "principal_id", principalID, "error", err)
		}
	}
}

// directMessages splits records into the "Done" messages that carry them: runs that fit one
// message's text and buttons, each record whole in one message.
func (s *Service) directMessages(records []Record) [][]Record {
	var groups [][]Record
	from, units := 0, 0
	for index, record := range records {
		// The cost covers the item's separators and its number, up to "100. ".
		cost := utf16Units(record.Summary) + 8
		if line := s.directStatus(record); line != "" {
			cost += utf16Units(line) + 1
		}
		if index > from && (units+cost > directItemsUnits || index-from == directMessageItems) {
			groups = append(groups, records[from:index])
			from, units = index, 0
		}
		units += cost
	}
	return append(groups, records[from:])
}

// directActions renders a "Done" message: each action's summary with where it stands, and an Undo
// button for each action that can still be undone.
func (s *Service) directActions(records []Record) DirectActions {
	var builder strings.Builder
	if len(records) == 1 {
		builder.WriteString("Done:")
	} else {
		fmt.Fprintf(&builder, "Done, %d changes:", len(records))
	}
	var actions DirectActions
	for index, record := range records {
		builder.WriteString("\n\n")
		label := "Undo"
		if len(records) > 1 {
			fmt.Fprintf(&builder, "%d. ", index+1)
			label = fmt.Sprintf("Undo %d", index+1)
		}
		builder.WriteString(record.Summary)
		if line := s.directStatus(record); line != "" {
			builder.WriteString("\n" + line)
		}
		if s.undoable(record) {
			actions.Undo = append(actions.Undo, UndoButton{Label: label, Data: UndoCallback(record.ID)})
		}
	}
	if len(actions.Undo) > 0 {
		builder.WriteString("\n\nUndo works for 24 hours.")
	}
	actions.Text = builder.String()
	return actions
}

func (s *Service) undoable(record Record) bool {
	if record.Status != StatusDone || len(record.Undo) == 0 || !s.now().Before(record.UndoUntil) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.executors[record.Tool].(Undoer)
	return ok
}

func (s *Service) directStatus(record Record) string {
	switch record.Status {
	case StatusDone:
		expired := len(record.Undo) > 0 && !s.now().Before(record.UndoUntil)
		switch {
		case record.Reason == ReasonUndoRefused:
			return "Not undone: " + clip.Runes(record.Outcome, directOutcomeRunes)
		case record.Reason == ReasonUndoInterrupted && expired:
			return "A restart interrupted its Undo, and it can no longer be undone; check whether the change still stands."
		case record.Reason == ReasonUndoInterrupted:
			return "A restart interrupted its Undo; check whether the change still stands, or press Undo again."
		case expired:
			return "It can no longer be undone."
		}
		return ""
	case StatusUndone:
		return "Undone."
	default:
		return ""
	}
}

// Undid is what an Undo press did.
type Undid struct {
	// Owned is false for a press on another principal's action, which is ignored.
	Owned bool
	// Answer is the short reply to the press.
	Answer string
}

// Undo reverses the principal's done direct action once, before its deadline, through its
// executor, and updates the "Done" message that shows it. The executor decides whether the object
// changed since.
func (s *Service) Undo(ctx context.Context, principalID, recordID string) (Undid, error) {
	record, found, err := s.repository.LoadApproval(ctx, recordID)
	if err != nil {
		return Undid{}, err
	}
	if !found || record.PrincipalID != principalID {
		return Undid{}, nil
	}
	if answer := s.undoRefusal(record); answer != "" {
		return Undid{Owned: true, Answer: answer}, nil
	}
	claimed, err := s.repository.ClaimUndo(ctx, record.ID, principalID, s.now())
	if err != nil {
		return Undid{}, err
	}
	if !claimed {
		current, _, err := s.repository.LoadApproval(ctx, record.ID)
		if err != nil {
			return Undid{}, err
		}
		answer := s.undoRefusal(current)
		if answer == "" {
			answer = "This can no longer be undone."
		}
		return Undid{Owned: true, Answer: answer}, nil
	}
	s.mu.Lock()
	undoer, _ := s.executors[record.Tool].(Undoer)
	s.mu.Unlock()
	undoCtx, cancel := context.WithTimeout(ctx, executionTimeout)
	result, err := undoer.Undo(undoCtx, record, record.Undo)
	cancel()
	storeCtx := context.WithoutCancel(ctx)
	var refusal *UndoRefusal
	var answer string
	switch {
	case err == nil:
		record.Status, record.Reason, record.Outcome = StatusUndone, "", clip.Runes(result.Text, outcomeRunes)
		if err := s.repository.CompleteUndo(storeCtx, record.ID, record.Outcome); err != nil {
			return Undid{}, err
		}
		answer = "Undone."
		s.recordStatus(ctx, record, StatusUndone)
	case errors.As(err, &refusal):
		reason := clip.Runes(refusal.Reason, outcomeRunes)
		if err := s.repository.ReleaseUndo(storeCtx, record.ID, ReasonUndoRefused, reason, true); err != nil {
			return Undid{}, err
		}
		record.Reason, record.Outcome, record.Undo, record.UndoUntil = ReasonUndoRefused, reason, nil, time.Time{}
		answer = "Not undone: " + reason
	default:
		if err := s.repository.ReleaseUndo(storeCtx, record.ID, "", "", false); err != nil {
			return Undid{}, err
		}
		s.logger.WarnContext(ctx, "approval.undo.failed", "principal_id", principalID, "approval_id", record.ID, "tool", record.Tool)
		return Undid{Owned: true, Answer: "Undo failed; try again in a moment."}, nil
	}
	s.logger.InfoContext(ctx, "approval.undo", "principal_id", principalID, "approval_id", record.ID, "tool", record.Tool, "status", record.Status, "reason", record.Reason)
	s.addNotice(record)
	s.showDirect(ctx, record)
	return Undid{Owned: true, Answer: answer}, nil
}

// undoRefusal says why a record cannot be undone now, or nothing when it can.
func (s *Service) undoRefusal(record Record) string {
	switch {
	case record.Mode != ModeDirect:
		return "This was not made without asking, so it has no Undo."
	case record.Status == StatusUndone:
		return "This was already undone."
	case record.Status != StatusDone:
		return "This did not complete, so there is nothing to undo."
	case record.Reason == ReasonUndoRefused:
		return "Not undone: " + record.Outcome
	case record.Reason == ReasonUndoing:
		return "This is being undone."
	case len(record.Undo) > 0 && !s.now().Before(record.UndoUntil):
		return "The 24 hours to undo this have passed."
	case !s.undoable(record):
		return "This cannot be undone."
	default:
		return ""
	}
}

// showDirect updates the "Done" message that shows the record.
func (s *Service) showDirect(ctx context.Context, record Record) {
	if record.ChatID == 0 || record.MessageID == 0 {
		return
	}
	shown, err := s.repository.ShownDirectApprovals(ctx, record.PrincipalID, record.ChatID, record.MessageID)
	if err != nil || len(shown) == 0 {
		return
	}
	if err := s.deliverer.EditDirectActions(ctx, record.ChatID, record.MessageID, s.directActions(shown)); err != nil {
		s.logger.WarnContext(ctx, "approval.direct.unshown", "principal_id", record.PrincipalID, "approval_id", record.ID, "error", err)
	}
}

// addNotice adds an Undo press to what the principal's next turn learns, keeping earlier ones, since
// each press is about a different action.
func (s *Service) addNotice(record Record) {
	text := noticeText(record)
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	executor, ok := s.executors[record.Tool].(PrivateExecutor)
	private := record.Outcome != "" && ok && executor.PrivateOutcomes()
	key := lane{record.PrincipalID, record.ThreadID}
	if earlier := s.notices[key]; earlier.text != "" {
		text, private = earlier.text+" "+text, private || earlier.private
	}
	s.notices[key] = notice{text: text, private: private}
}

func directNoticeText(record Record) string {
	switch record.Status {
	case StatusUndone:
		return strings.TrimSpace("The user pressed Undo on your " + record.Tool + " action; it was undone and the change no longer stands. " + record.Outcome)
	case StatusDone:
		switch record.Reason {
		case ReasonUndoRefused:
			return "The user pressed Undo on your " + record.Tool + " action, but it was not undone: " + record.Outcome
		case ReasonUndoInterrupted:
			return "The user pressed Undo on your " + record.Tool + " action, but a restart interrupted it; it may not have been undone, so check whether the change still stands."
		}
		return strings.TrimSpace("Your " + record.Tool + " action was done. " + record.Outcome)
	case StatusFailed:
		if record.Reason == ReasonInterrupted {
			return "Your " + record.Tool + " action was interrupted by a restart; check whether it took effect before trying again."
		}
		return strings.TrimSpace("Your " + record.Tool + " action failed. " + record.Outcome)
	default:
		return ""
	}
}

// UndoCallback is the callback data of a direct action's Undo button.
func UndoCallback(id string) string {
	return "undo:" + id
}

// ParseUndoCallback returns the record ID of Undo button data.
func ParseUndoCallback(data string) (string, bool) {
	id, found := strings.CutPrefix(data, "undo:")
	if !found || len(id) != idBytes*2 {
		return "", false
	}
	return id, true
}

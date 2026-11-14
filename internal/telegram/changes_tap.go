package telegram

import (
	"context"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

const (
	approvalCallbackPrefix = "approval:"
	// changesKeepPrefix starts the tap of a Confirm in a changes widget, which names the changes
	// kept as one 0 or 1 per change: approval:keep:<id>:<flags>.
	changesKeepPrefix = "approval:keep:"
	// changesReadPrefix starts the tap that reads one change in full: approval:read:<id>:<number>.
	changesReadPrefix = "approval:read:"
	// longReadMemory is how long the whole text of a change already sent as a reply is remembered:
	// twice the longest a proposal waits, which is 24 hours for the weekly review and the inbox.
	longReadMemory = 48 * time.Hour
)

// parseChangesID reports whether text is the form of an approval ID.
func parseChangesID(text string) bool {
	if len(text) != approval.IDLength {
		return false
	}
	_, err := hex.DecodeString(text)
	return err == nil
}

// parseKeepTap splits a changes widget's Confirm tap into the proposal's ID and which changes it
// keeps, by position.
func parseKeepTap(data string) (approvalID string, keep []bool, ok bool) {
	rest, found := strings.CutPrefix(data, changesKeepPrefix)
	if !found {
		return "", nil, false
	}
	approvalID, flags, found := strings.Cut(rest, ":")
	if !found || !parseChangesID(approvalID) || len(flags) == 0 || len(flags) > approval.MaximumBundleItems {
		return "", nil, false
	}
	keep = make([]bool, len(flags))
	for index, flag := range flags {
		switch flag {
		case '1':
			keep[index] = true
		case '0':
		default:
			return "", nil, false
		}
	}
	return approvalID, keep, true
}

// parseReadTap splits the tap that reads one change into the proposal's ID and the change's number.
func parseReadTap(data string) (approvalID string, number int, ok bool) {
	rest, found := strings.CutPrefix(data, changesReadPrefix)
	if !found {
		return "", 0, false
	}
	approvalID, digits, found := strings.Cut(rest, ":")
	if !found || !parseChangesID(approvalID) {
		return "", 0, false
	}
	number, err := strconv.Atoi(digits)
	if err != nil || number < 1 || number > approval.MaximumBundleItems || strconv.Itoa(number) != digits {
		return "", 0, false
	}
	return approvalID, number, true
}

// processChangesWidgetTap handles the taps only a changes widget sends: a Confirm that names which
// changes are kept, and the reading of one change. Its Reject and Preview taps are the proposal
// widget's. Like those it acts only from the message that shows the proposal.
func (s *Service) processChangesWidgetTap(ctx context.Context, bot Bot, updateID int64, tap press, principalID, data string) (bool, processingResult, string, error) {
	if s.approvals == nil {
		return false, processingResult{}, "", nil
	}
	if approvalID, keep, ok := parseKeepTap(data); ok {
		result, failure, err := s.processKeepTap(ctx, bot, updateID, tap, principalID, approvalID, keep)
		return true, result, failure, err
	}
	if approvalID, number, ok := parseReadTap(data); ok {
		result, failure, err := s.processReadTap(ctx, bot, tap, principalID, approvalID, number)
		return true, result, failure, err
	}
	return false, processingResult{}, "", nil
}

// processKeepTap confirms the kept changes of a proposal and leaves the others out. The tap is
// answered before the changes run, as changed when the choice no longer matches the proposal, and
// the message is always edited to the proposal as stored, with its controls while anything waits.
func (s *Service) processKeepTap(ctx context.Context, bot Bot, updateID int64, tap press, principalID, approvalID string, keep []bool) (processingResult, string, error) {
	ctx, ignored, result, failure, err := s.placeWidgetTap(ctx, bot, tap, principalID, approvalID)
	if ignored {
		return result, failure, err
	}
	result = processingResult{event: "telegram.callback.handled", principalID: principalID, command: "approval.keep"}
	current, err := s.approvals.KeepIsCurrent(ctx, principalID, approvalID, keep)
	if err != nil {
		return processingResult{}, "approval_press", err
	}
	answer := WidgetWorkingAnswer.Format(ctx)
	if !current {
		answer = approval.ProposalChangedText
	}
	if err := tap.answer(ctx, answer); err != nil && !isPermanentAPIError(err) {
		return processingResult{}, tap.failure, err
	}
	pressed, err := s.approvals.PressKept(ctx, principalID, approvalID, keep)
	if err != nil {
		return processingResult{}, "approval_press", err
	}
	if !pressed.Owned {
		result.event, result.reason = "telegram.update.ignored", "foreign_approval"
		return result, "", nil
	}
	if pressed.Ran {
		s.continueConversation(ctx, bot, updateID, tap, principalID)
	}
	shown := pressed.Shown
	if shown.Widget == approval.NotWidget {
		shown.Widget = approval.ProposalWidget
	}
	if pressed.Buttons.Confirm != "" {
		err = s.client.EditProposal(ctx, tap.chatID, tap.messageID, shown, pressed.Buttons)
	} else {
		err = s.client.EditText(ctx, tap.chatID, tap.messageID, shown)
	}
	if err != nil && isPermanentAPIError(err) {
		err = s.sendReplyChunks(ctx, tap.chatID, 0, shown.Text)
	}
	if err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

// processReadTap answers a tap that reads one change with its summary as plain text, whatever
// became of the change. A summary longer than an answer holds is answered by its beginning and then
// sent whole as a reply to the widget, once.
func (s *Service) processReadTap(ctx context.Context, bot Bot, tap press, principalID, approvalID string, number int) (processingResult, string, error) {
	ctx, ignored, result, failure, err := s.placeWidgetTap(ctx, bot, tap, principalID, approvalID)
	if ignored {
		return result, failure, err
	}
	summary, found, err := s.approvals.ChangeSummary(ctx, principalID, approvalID, number)
	if err != nil {
		return processingResult{}, "approval_change", err
	}
	if !found {
		return s.ignoreWidgetTap(ctx, tap, principalID, "unknown_change")
	}
	plain := plainWidgetText(summary)
	if err := tap.answer(ctx, plain); err != nil && !isPermanentAPIError(err) {
		return processingResult{}, tap.failure, err
	}
	result = processingResult{event: "telegram.callback.handled", principalID: principalID, command: "approval.read"}
	key := approvalID + ":" + strconv.Itoa(number)
	if len(plain) <= widgetDataLimit || s.longReadSent(key) {
		return result, "", nil
	}
	if err := s.sendReplyChunks(ctx, tap.chatID, tap.messageID, summary); err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	s.markLongRead(key)
	return result, "", nil
}

// longReadSent reports whether the whole text of the change was already sent as a reply.
func (s *Service) longReadSent(key string) bool {
	_, sent := s.longReads.Load(key)
	return sent
}

// markLongRead remembers that the change's whole text was sent, and forgets those sent longer ago
// than their proposals could be waiting.
func (s *Service) markLongRead(key string) {
	now := time.Now()
	s.longReads.Range(func(key, sentAt any) bool {
		if now.Sub(sentAt.(time.Time)) > longReadMemory {
			s.longReads.Delete(key)
		}
		return true
	})
	s.longReads.Store(key, now)
}

// sendReplyChunks sends text as a reply to a message, in as many messages as it needs.
func (s *Service) sendReplyChunks(ctx context.Context, chatID, replyTo int64, text string) error {
	for _, chunk := range conversation.SplitReply(text) {
		if err := s.client.SendReply(ctx, chatID, replyTo, chunk); err != nil {
			return err
		}
	}
	return nil
}

package approval

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

const (
	previewBusyText        = "A preview of this batch is already running; its report will follow here."
	previewNothingText     = "Nothing in this batch can be previewed."
	previewResolvedText    = "This batch is no longer waiting for confirmation, so there is nothing to preview."
	previewUnavailableText = "Previews are not available right now."
	previewFooter          = "Preview only: nothing is scheduled or changed until you confirm."
)

// PreviewPressed is what a Preview press did.
type PreviewPressed struct {
	// Text is the short answer shown to the pressing user.
	Text string
	// Owned is false for a press on another principal's proposal, which is ignored.
	Owned bool
}

// buttons is the callback data of the bundle's buttons; Preview is offered while a pending item can
// be previewed.
func (s *Service) buttons(ctx context.Context, bundle []Record) Buttons {
	id := bundle[0].BundleID
	buttons := Buttons{Confirm: callbackData(ActionConfirm, id), Reject: callbackData(ActionReject, id)}
	if len(s.previewable(ctx, bundle)) > 0 {
		buttons.Preview = callbackData(ActionPreview, id)
	}
	return buttons
}

// previewable returns the bundle's pending records whose executor can preview them, in order.
func (s *Service) previewable(ctx context.Context, bundle []Record) []Record {
	var records []Record
	for _, record := range bundle {
		if record.Status != StatusPending {
			continue
		}
		if previewer, ok := s.previewer(record.Tool); ok && previewer.Previewable(ctx, record) {
			records = append(records, record)
		}
	}
	return records
}

func (s *Service) previewer(tool string) (Previewer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previewer, ok := s.executors[tool].(Previewer)
	return previewer, ok
}

// Preview handles a Preview press by the pressing principal: it starts previews of the bundle's
// previewable items, one after another, each posted as a reply to the proposal message. One preview
// runs per bundle at a time, and the trigger names the press, so a press Telegram delivers again
// gets the first answer and starts nothing.
func (s *Service) Preview(ctx context.Context, principalID, approvalID, trigger string, chatID, messageID int64) (PreviewPressed, error) {
	s.mu.Lock()
	answer, seen := s.pressed[trigger]
	s.mu.Unlock()
	if seen {
		return PreviewPressed{Text: answer.text, Owned: true}, nil
	}
	record, found, err := s.repository.LoadApproval(ctx, approvalID)
	if err != nil {
		return PreviewPressed{}, err
	}
	if !found || record.PrincipalID != principalID {
		return PreviewPressed{}, nil
	}
	bundle, err := s.repository.LoadBundle(ctx, record.BundleID)
	if err != nil {
		return PreviewPressed{}, err
	}
	pending := slices.IndexFunc(bundle, func(item Record) bool { return item.Status == StatusPending })
	if pending < 0 || !s.now().Before(bundle[pending].ExpiresAt) {
		return s.answerPreview(trigger, previewResolvedText), nil
	}
	items := s.previewable(ctx, bundle)
	if len(items) == 0 {
		return s.answerPreview(trigger, previewNothingText), nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if answer, seen := s.pressed[trigger]; seen {
		return PreviewPressed{Text: answer.text, Owned: true}, nil
	}
	text := ""
	switch {
	case s.runCtx == nil:
		text = previewUnavailableText
	case s.previewing[record.BundleID]:
		text = previewBusyText
	case len(items) == 1:
		text = "Running a preview; its report will follow here. Nothing is saved."
	default:
		text = fmt.Sprintf("Running previews of %d items one after another; each report will follow here. Nothing is saved.", len(items))
	}
	s.rememberPress(trigger, text)
	if s.runCtx == nil || s.previewing[record.BundleID] {
		return PreviewPressed{Text: text, Owned: true}, nil
	}
	s.previewing[record.BundleID] = true
	s.previews.Add(1)
	go s.runPreviews(s.runCtx, record.BundleID, items, chatID, messageID)
	s.logger.InfoContext(ctx, "approval.preview.started", "principal_id", principalID, "bundle_id", record.BundleID, "items", len(items))
	return PreviewPressed{Text: text, Owned: true}, nil
}

func (s *Service) answerPreview(trigger, text string) PreviewPressed {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rememberPress(trigger, text)
	return PreviewPressed{Text: text, Owned: true}
}

// rememberPress keeps a press's answer for as long as its bundle could be waiting. The caller holds
// s.mu.
func (s *Service) rememberPress(trigger, text string) {
	now := s.now()
	for key, answer := range s.pressed {
		if now.Sub(answer.at) > 2*s.lifetime {
			delete(s.pressed, key)
		}
	}
	s.pressed[trigger] = previewAnswer{text: text, at: now}
}

// runPreviews previews each item in order, stopping once the bundle is no longer waiting.
func (s *Service) runPreviews(ctx context.Context, bundleID string, items []Record, chatID, messageID int64) {
	defer func() {
		s.mu.Lock()
		delete(s.previewing, bundleID)
		s.mu.Unlock()
		s.previews.Done()
	}()
	for index, item := range items {
		current, found, err := s.repository.LoadApproval(ctx, item.ID)
		if err != nil || !found || current.Status != StatusPending || !s.now().Before(current.ExpiresAt) {
			return
		}
		previewer, ok := s.previewer(current.Tool)
		if !ok {
			return
		}
		preview, err := previewer.Preview(ctx, current)
		if err != nil {
			if ctx.Err() == nil {
				s.logger.WarnContext(ctx, "approval.preview.failed", "principal_id", current.PrincipalID, "approval_id", current.ID, "error", err)
			}
			return
		}
		if err := s.deliverer.SendPreview(ctx, chatID, messageID, previewText(preview, index, len(items))); err != nil {
			s.logger.WarnContext(ctx, "approval.preview.undeliverable", "principal_id", current.PrincipalID, "approval_id", current.ID, "error", err)
			return
		}
		s.logger.InfoContext(ctx, "approval.preview.delivered", "principal_id", current.PrincipalID, "approval_id", current.ID, "bundle_id", bundleID)
	}
}

func previewText(preview Preview, index, count int) string {
	heading := "Preview: " + preview.Name
	if count > 1 {
		heading = fmt.Sprintf("Preview %d of %d: %s", index+1, count, preview.Name)
	}
	return "**" + heading + "**\n\n" + strings.TrimSpace(preview.Text) + "\n\n" + previewFooter
}

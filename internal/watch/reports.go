package watch

import (
	"context"
	"fmt"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/localtime"
)

const (
	// alertMessagesKept matches the job reports kept per principal.
	alertMessagesKept   = 200
	maximumQuotedRunes  = 2000
	maximumLabelRunes   = 100
	quotedAlertTimeForm = "Mon 2 Jan 15:04 MST"
)

var _ conversation.Reports = (*Service)(nil)

// keepMessage records a delivered alert's message, so a reply to it finds its watch and what it
// said. A failure only costs that reply its quote, so it is logged.
func (s *Service) keepMessage(ctx context.Context, alert Alert, sent string, messageID, threadID int64) {
	if messageID <= 0 {
		return
	}
	label := cut(strings.TrimSpace(alert.Label), maximumLabelRunes)
	if label == "" {
		label = "Watch"
	}
	message := AlertMessage{
		PrincipalID: alert.PrincipalID,
		MessageID:   messageID,
		ThreadID:    threadID,
		WatchID:     alert.WatchID,
		WatchLabel:  label,
		Text:        cut(strings.TrimSpace(sent), maximumQuotedRunes),
		Private:     alert.Private,
		External:    alert.Kind == KindAgent,
	}
	if message.Text == "" {
		return
	}
	if err := s.repository.SaveWatchAlertMessage(ctx, message, alertMessagesKept); err != nil && ctx.Err() == nil {
		s.logger.WarnContext(ctx, "watch.alert.save_failed", "principal_id", alert.PrincipalID, "watch_id", alert.WatchID, "error", err)
	}
}

// TurnReports quotes the alert the turn's message replied to, and drops it from the notices so the
// turn is told of it once. The quote is outside content when a model wrote the alert from what it
// read, and private when the check read private content.
func (s *Service) TurnReports(ctx context.Context, principalID string, turn conversation.ReportTurn) (string, bool, bool) {
	if turn.ReplyToMessageID <= 0 {
		return "", false, false
	}
	message, found, err := s.repository.WatchAlertByMessage(ctx, principalID, turn.ReplyToMessageID)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "watch.alert.read_failed", "principal_id", principalID, "error", err)
		}
		return "", false, false
	}
	if !found {
		return "", false, false
	}
	s.forgetNotice(principalID, message.MessageID)
	location := localtime.SavedZone(s.zone)
	text := fmt.Sprintf("The user is replying to this message from their watch %q (watch id %s), sent %s:\n%s",
		message.WatchLabel, message.WatchID, message.SentAt.In(location).Format(quotedAlertTimeForm), message.Text)
	return text, message.External, message.Private
}

func (s *Service) forgetNotice(principalID string, messageID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.notices[principalID][:0]
	for _, pending := range s.notices[principalID] {
		if pending.messageID != messageID {
			kept = append(kept, pending)
		}
	}
	if len(kept) == 0 {
		delete(s.notices, principalID)
		return
	}
	s.notices[principalID] = kept
}

// cut keeps text within limit runes, ending a shortened one with "…".
func cut(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}

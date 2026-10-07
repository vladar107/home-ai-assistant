package telegram

import (
	"context"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const (
	WatchGoneReply   = "This watch no longer exists."
	WatchMutedReply  = "Muted for today; it checks again on its next day."
	WatchPausedReply = "Paused. Resume it in /jobs."

	watchPrefix      = "watch"
	watchMuteAction  = "mute"
	watchPauseAction = "pause"
)

// WatchController is implemented by the watch service; nil leaves watches out of /jobs and
// ignores alert buttons.
type WatchController interface {
	Watches(context.Context, string) ([]watch.Watch, error)
	SetEnabled(context.Context, string, string, bool) (bool, error)
	Delete(context.Context, string, string) (bool, error)
	Mute(context.Context, string, string) (bool, error)
	DescribeState(watch.Watch) string
}

// SetWatches adds watches to /jobs and handles the buttons of their alerts; call it before Start.
func (s *Service) SetWatches(watches WatchController) {
	s.watches = watches
}

// WatchAlertSender delivers watch alerts with their Mute today and Pause buttons.
type WatchAlertSender struct {
	Client *Client
}

func (w WatchAlertSender) SendAlert(ctx context.Context, chatID int64, watchID, text string) error {
	return w.Client.SendKeyboard(ctx, chatID, 0, text, InlineKeyboard{{
		{Text: "Mute today", CallbackData: watchPrefix + ":" + watchMuteAction + ":" + watchID},
		{Text: "Pause", CallbackData: watchPrefix + ":" + watchPauseAction + ":" + watchID},
	}})
}

// parseWatchCallback accepts only the fixed alert button vocabulary.
func parseWatchCallback(data string) (string, string, bool) {
	if len(data) > maximumCallbackDataBytes {
		return "", "", false
	}
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != watchPrefix || parts[2] == "" {
		return "", "", false
	}
	if parts[1] != watchMuteAction && parts[1] != watchPauseAction {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// processWatchPress mutes or pauses the principal's own watch and replaces the alert's buttons with
// the outcome. Both actions only set state, so a retried press changes nothing more.
func (s *Service) processWatchPress(ctx context.Context, callback *CallbackQuery, principalID, action, watchID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "watch." + action}
	var changed bool
	var err error
	outcome := WatchMutedReply
	if action == watchPauseAction {
		changed, err = s.watches.SetEnabled(ctx, principalID, watchID, false)
		outcome = WatchPausedReply
	} else {
		changed, err = s.watches.Mute(ctx, principalID, watchID)
	}
	if err != nil {
		return processingResult{}, "watch_action", err
	}
	if !changed {
		outcome = WatchGoneReply
	}
	text := strings.TrimSpace(callback.Message.Text) + "\n\n" + outcome
	if err := s.client.EditKeyboard(ctx, callback.Message.Chat.ID, callback.Message.MessageID, text, nil); err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

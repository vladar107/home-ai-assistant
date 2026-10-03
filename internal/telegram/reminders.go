package telegram

import (
	"context"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

const (
	ReminderGoneReply = "This reminder was cancelled or is already set again."

	reminderPrefix = "remind"
)

// snoozeButtons are the fixed snooze choices, in the order they are shown.
var snoozeButtons = []struct {
	label  string
	snooze schedule.Snooze
}{
	{"Snooze 10 min", schedule.SnoozeTenMinutes},
	{"Snooze 1 hour", schedule.SnoozeHour},
	{"Tomorrow", schedule.SnoozeTomorrow},
}

// ReminderSender delivers the scheduler's reminders with their snooze buttons.
type ReminderSender struct {
	Client *Client
}

func (r ReminderSender) SendReminder(ctx context.Context, chatID int64, jobID, text string) error {
	row := make([]InlineButton, 0, len(snoozeButtons))
	for _, button := range snoozeButtons {
		row = append(row, InlineButton{Text: button.label, CallbackData: reminderPrefix + ":" + string(button.snooze) + ":" + jobID})
	}
	return r.Client.SendKeyboard(ctx, chatID, 0, text, InlineKeyboard{row})
}

// parseReminderCallback accepts only the fixed snooze vocabulary.
func parseReminderCallback(data string) (schedule.Snooze, string, bool) {
	if len(data) > maximumCallbackDataBytes {
		return "", "", false
	}
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != reminderPrefix || parts[2] == "" {
		return "", "", false
	}
	for _, button := range snoozeButtons {
		if string(button.snooze) == parts[1] {
			return button.snooze, parts[2], true
		}
	}
	return "", "", false
}

// processSnoozePress moves the principal's own delivered reminder and replaces the buttons with
// the outcome, so a second press on the same message has nothing left to press.
func (s *Service) processSnoozePress(ctx context.Context, callback *CallbackQuery, principalID string, snooze schedule.Snooze, jobID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "reminder.snooze"}
	job, snoozed, err := s.schedules.Snooze(ctx, principalID, jobID, snooze)
	if err != nil {
		return processingResult{}, "snooze_reminder", err
	}
	outcome := ReminderGoneReply
	if snoozed {
		location, err := schedule.LoadZone(job.Zone)
		if err != nil {
			location, _ = schedule.LoadZone(s.schedules.DefaultZone())
		}
		outcome = "Snoozed until " + schedule.FormatOccurrence(job.RunAt, location) + "."
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
